#!/usr/bin/env bash
# Deploy ascii-tv to the bodegga-web Droplet (the same box that serves
# bodegga.net) at https://tv.bodegga.net.
#
#   ./scripts/deploy.sh            # binary + config (fast)
#   ./scripts/deploy.sh --movies   # also upload movies/ (private screenings, ~1.4 GB)
#
# Needs: the Bodegga deploy key (../MainWebsite/.secrets/bodegga_deploy_key or
# $DEPLOY_KEY), and either Go or Docker to cross-compile. DNS for
# tv.bodegga.net must already point at the Droplet (ZoneEdit).
# Safe to re-run: every step is idempotent.
set -euo pipefail
cd "$(dirname "$0")/.."

HOST="${DROPLET_IP:-147.182.175.148}"
KEY="${DEPLOY_KEY:-../MainWebsite/.secrets/bodegga_deploy_key}"
DOMAIN="tv.bodegga.net"
EMAIL="${CERT_EMAIL:-admin@bodegga.net}"
UPLOAD_MOVIES=0
[ "${1:-}" = "--movies" ] && UPLOAD_MOVIES=1

[ -f "$KEY" ] || { echo "Deploy key not found at $KEY (set DEPLOY_KEY)" >&2; exit 1; }
SSH=(ssh -i "$KEY" -o StrictHostKeyChecking=accept-new "root@$HOST")

echo "==> Building linux/amd64 binary"
mkdir -p dist
if command -v go >/dev/null && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/ascii-tv-linux-amd64 . 2>/dev/null; then
  :
else
  # Fallback for machines where the Go toolchain can't run (e.g. Windows
  # Application Control): build in the official Go image.
  MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W 2>/dev/null || pwd):/src" -v asciitv-gocache:/root/.cache -w /src \
    golang:1.25 sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/ascii-tv-linux-amd64 .'
fi

echo "==> Uploading binary, public movies and config"
tar czf - dist/ascii-tv-linux-amd64 resources/catalog.json resources/sw1.txt deploy |
  "${SSH[@]}" 'rm -rf /tmp/ascii-tv-up && mkdir -p /tmp/ascii-tv-up && tar xzf - -C /tmp/ascii-tv-up'

if [ "$UPLOAD_MOVIES" = 1 ]; then
  echo "==> Uploading private movies (compressed stream; this takes a few minutes)"
  [ -f movies/catalog.private.json ] || { echo "movies/catalog.private.json missing" >&2; exit 1; }
  # List files explicitly: --exclude='.*' would also exclude "." itself.
  (cd movies && tar czf - catalog.private.json $(ls *.txt)) |
    "${SSH[@]}" 'mkdir -p /var/lib/ascii-tv/movies.new && tar xzf - -C /var/lib/ascii-tv/movies.new &&
                 rm -rf /var/lib/ascii-tv/movies.old && { [ -d /var/lib/ascii-tv/movies ] && mv /var/lib/ascii-tv/movies /var/lib/ascii-tv/movies.old || true; } &&
                 mv /var/lib/ascii-tv/movies.new /var/lib/ascii-tv/movies && rm -rf /var/lib/ascii-tv/movies.old'
fi

KEYS=""
if [ -f movies/.screening-key ]; then
  KEYS="$(tr -d '\r\n' < movies/.screening-key)"
fi

echo "==> Installing on $HOST"
"${SSH[@]}" DOMAIN="$DOMAIN" EMAIL="$EMAIL" KEYS="$KEYS" 'bash -s' <<'REMOTE'
set -euo pipefail
U=/tmp/ascii-tv-up

install -d -m 755 /opt/ascii-tv/resources /var/lib/ascii-tv/movies /etc/ascii-tv
install -m 755 $U/dist/ascii-tv-linux-amd64 /opt/ascii-tv/ascii-tv.new
mv /opt/ascii-tv/ascii-tv.new /opt/ascii-tv/ascii-tv
install -m 644 $U/resources/catalog.json $U/resources/sw1.txt /opt/ascii-tv/resources/

# No private catalog yet? Ship an empty one so the service still starts.
[ -f /var/lib/ascii-tv/movies/catalog.private.json ] || echo '{"movies":[]}' > /var/lib/ascii-tv/movies/catalog.private.json
chmod -R a+rX /var/lib/ascii-tv

if [ -n "$KEYS" ]; then
  umask 077
  printf 'ASCII_TV_KEYS=%s\n' "$KEYS" > /etc/ascii-tv/env
  chmod 600 /etc/ascii-tv/env
fi

install -m 644 $U/deploy/ascii-tv.service /etc/systemd/system/ascii-tv.service
install -d /etc/systemd/journald.conf.d
install -m 644 $U/deploy/journald-retention.conf /etc/systemd/journald.conf.d/ascii-tv-retention.conf
systemctl restart systemd-journald
systemctl daemon-reload
systemctl enable ascii-tv >/dev/null 2>&1
systemctl restart ascii-tv

# nginx: install the site once; afterwards certbot owns the TLS lines, so
# only overwrite if there is no certificate yet.
if [ ! -f /etc/nginx/sites-available/tv.bodegga.net ] || ! grep -q ssl_certificate /etc/nginx/sites-available/tv.bodegga.net; then
  install -m 644 $U/deploy/nginx-tv.bodegga.net.conf /etc/nginx/sites-available/tv.bodegga.net
fi
ln -sf /etc/nginx/sites-available/tv.bodegga.net /etc/nginx/sites-enabled/tv.bodegga.net
nginx -t
systemctl reload nginx

if ! grep -q ssl_certificate /etc/nginx/sites-available/tv.bodegga.net; then
  # --no-redirect: plain-HTTP curl must keep working.
  certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos -m "$EMAIL" --no-redirect
fi

# Wait for the movie index to load (a few seconds for the big films).
for i in $(seq 1 60); do curl -fsS http://127.0.0.1:8090/healthz >/dev/null 2>&1 && break; sleep 1; done
systemctl --no-pager --lines=8 status ascii-tv | tail -8
rm -rf $U
REMOTE

echo "==> Verifying from here"
curl -fsS "http://$DOMAIN/healthz" && echo "  http ok"
curl -fsS "https://$DOMAIN/healthz" && echo "  https ok"
curl -s -m 4 "https://$DOMAIN/" | head -c 400 | grep -q 'Simon Jansen\|Star\|@' && echo "  stream ok" || echo "  (stream check inconclusive: run: curl $DOMAIN)"
echo "Done. Try:  curl $DOMAIN"
