# AGENTS.md

Guidance for AI agents and contributors working on ascii-tv (tv.bodegga.net).

## What this is

A Go server that streams ASCII-art movies to terminals over plain HTTP (`curl tv.bodegga.net`)
and to browsers through a small embedded web player. It is a revival of martinraison/ascii-tv
(MIT), run by Bodegga, a security company. The goal against competitors such as ascii.theater
(SSH-only) is to be **easier and more accessible**: one command on every OS, a browser player
that works on phones, subtitles, and screen-reader support.

## Layout

| Path | Purpose |
|---|---|
| `main.go` | flags, catalogue loading, HTTP server lifecycle |
| `movie.go` | movie file parser and on-disk frame index, renditions, catalogue |
| `pack.go` | `.atv` compressed movie packs (per-frame DEFLATE), colour stripping |
| `server.go` | routes, terminal/browser detection, playback loop, live channel, limits, keys |
| `web/` | embedded site: `index.html`, `styles.css`, `app.js`, `privacy.html`, `terms.html` |
| `resources/` | public movies (`sw1.txt`) and `catalog.json` |
| `movies/` | **git-ignored.** Private converted films, `catalog.private.json`, `.screening-key` |
| `tools/convert.py` | video to movie file converter (ffmpeg + numpy) |
| `deploy/` | systemd unit, nginx site, journald retention |
| `scripts/deploy.sh` | idempotent deploy to the bodegga-web Droplet |
| `work/` | **git-ignored.** Scratch: proxies, clips, screenshots |

## Hard rules

- **Standard library only** for the Go server. No modules in `go.sum`.
- **Strictly black and white** UI chrome (Bodegga house style). Movie pictures may be in colour: that's content, not chrome.
- **No third-party scripts, fonts or CDNs.** The CSP is `default-src 'self'`. No inline `<script>` or `style=` attributes.
- **Privacy claims must stay true.** `/privacy` promises truncated IPs in logs (`anonIP`), no query strings in nginx logs, 14-day retention, no cookies, and no analytics. If you change logging, update `web/privacy.html` in the same change.
- **Never commit movies you lack rights to publish, or secrets.** Private films and screening keys live in `movies/` (ignored). Private titles must 404 exactly like missing ones.
- **`curl tv.bodegga.net` must keep working** over plain HTTP with no flags. Never add an HTTP→HTTPS redirect for non-browser clients, and never buffer streams (`X-Accel-Buffering: no`, nginx `proxy_buffering off`).
- **Keep the credits** (Simon Jansen, Martin W. Kirst, Martin Raison) and the original LICENSE notice.

## Working on Windows

This workstation's Application Control policy blocks Go's toolchain and freshly built unsigned
`.exe` files. Build, test and run in Docker:

```sh
MSYS_NO_PATHCONV=1 docker run --rm -v "$(cygpath -w "$PWD"):/src" -v asciitv-gocache:/root/.cache \
  -w /src golang:1.25 sh -c 'gofmt -l . ; go vet ./... && go test -race ./...'
```

Run locally with the Linux build in a container (port 8099):

```sh
docker run -d --name asciitv-local -p 127.0.0.1:8099:8080 -e ASCII_TV_KEYS="$(cat movies/.screening-key)" \
  -v "$(cygpath -w "$PWD"):/app:ro" -w /app alpine:3.20 /app/dist/ascii-tv-linux-amd64 \
  -catalog resources/catalog.json -catalog movies/catalog.private.json
```

Use `curl.exe` in PowerShell. Plain `curl` is `Invoke-WebRequest` there.

## Definition of done

- `gofmt` clean, `go vet` clean, `go test -race ./...` passes.
- `curl` against a local server plays at the right speed. Ctrl+C ends the server-side stream.
- The web page has no console errors, works at 375 px wide, and is keyboard-operable.
- Privacy and terms pages still match actual behaviour.
- After a deploy, all of these work: `curl tv.bodegga.net`, `curl https://tv.bodegga.net/healthz`, and the browser page.

## Production facts

- Host: DigitalOcean Droplet `bodegga-web` (147.182.175.148), shared with bodegga.net. 512 MB RAM, so keep memory flat (movies are read from disk per frame, not loaded).
- Service: `ascii-tv.service` (DynamicUser, sandboxed) on `127.0.0.1:8090`. Keys in `/etc/ascii-tv/env` (0600). Private movies in `/var/lib/ascii-tv/movies/`.
- DNS: ZoneEdit, A record `tv` → Droplet. TLS: certbot `--nginx --no-redirect`.
- Logs: `journalctl -u ascii-tv` (14-day retention), `/var/log/nginx/tv.access.log` (anonymised).
