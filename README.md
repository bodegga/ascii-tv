# ascii-tv

**Movies drawn in text, streamed to any terminal, or to your browser.**

```sh
curl tv.bodegga.net
```

That's it. No app, no account. `curl` ships with macOS, Linux, and Windows 10 and 11.
Open <https://tv.bodegga.net> in a browser for the web player and step-by-step help.

ascii-tv is Bodegga's revival of [martinraison/ascii-tv](https://github.com/martinraison/ascii-tv),
which streamed Simon Jansen's hand-drawn *Star Wars* ASCIImation at `asciitv.fr` until that
server disappeared around 2018.

## Watching

| Where | Command |
|---|---|
| macOS / Linux | `curl tv.bodegga.net` |
| Fit to your window | `curl -sN "tv.bodegga.net/starwars?cols=$(tput cols)&rows=$(tput lines)"` |
| Windows PowerShell | `curl.exe tv.bodegga.net` (with `.exe`: plain `curl` is an alias for `Invoke-WebRequest` in PowerShell 5.1 and won't stream) |
| Windows PowerShell, fitted | `curl.exe -sN "tv.bodegga.net/starwars?cols=$($Host.UI.RawUI.WindowSize.Width)&rows=$($Host.UI.RawUI.WindowSize.Height)"` |
| Windows Command Prompt | `curl tv.bodegga.net` |
| Android | Termux: `pkg install curl`, then as above |
| iPhone / iPad | a-Shell, then as above, or just use the web player |
| Any browser | <https://tv.bodegga.net> |

Paths and options:

| Path | Plays |
|---|---|
| `/` | the default movie (Star Wars) |
| `/starwars` | a movie by name |
| `/live` | a channel that loops the catalogue on a wall clock: every viewer sees the same frame |
| `/ls` or `/help` | what's showing, and these options |

| Option | Effect |
|---|---|
| `?t=12:30`, `?t=90`, `?t=1h2m` | start part-way through |
| `?cols=N&rows=N` | centre the picture, and pick the sharpest version of the movie that fits |
| `?bar=0` | hide the progress bar |
| `?loop` | play forever |
| `?format=plain` | no escape codes; frames separated by form feeds (`\f`). The web player uses this. |

Stop with <kbd>Ctrl</kbd>+<kbd>C</kbd>. If your cursor stays hidden afterwards, run `tput cnorm`.

## What changed since 2018

**Fixes to the original**

- CRLF line endings (Git for Windows) no longer make the movie flash past instantly. `\r` is stripped, and `.gitattributes` pins LF.
- A missing or malformed movie file stops the server with a clear error (file and line) instead of running with zero frames or panicking on a partial frame.
- A viewer pressing Ctrl+C ends their stream immediately. Before, a goroutine slept through the remaining 18 minutes.
- The dead `asciitv.fr` URL is gone. Help text uses the request's host or `-public-url`.
- Updated for current Go: `os.ReadFile`, `log/slog`, `net/http` pattern routing, `http.ResponseController`.
- Frame height is per movie (catalogue) or `-height` for single files.
- Playback timing is scheduled against the clock, so it doesn't drift over a two-hour film.

**New**

- **Web player.** Works on phones, with keyboard controls (Space, ←/→, F, C, +/−), seeking, text size, full screen, and `prefers-reduced-motion` support. It uses the same stream as curl.
- **Subtitles.** There's no audio, so the converter burns subtitles into the bottom rows. The web player mirrors them into a screen-reader live region and can toggle them.
- **More than one movie.** A JSON catalogue, plus private titles unlocked with a screening key (`Authorization: Bearer …`). Private titles are invisible without the key, and 404 exactly like missing ones.
- **Sizes that fit.** Each movie can have several renditions (for example 80 and 160 columns). The server picks the largest one that fits the `cols`/`rows` you send and centres it.
- **Live channel.** `/live` is broadcast TV: tune in mid-show.
- **Flicker-free rendering.** Cursor-home redraw plus synchronized output (`CSI ?2026`), hidden cursor during playback, and an ASCII-only progress bar so old Windows consoles render it.
- **Works with any HTTP client.** Browsers are detected by `Accept: text/html`, so wget, HTTPie and others get the stream too, not just clients that say "curl".
- **Built to run on a $4 server.** Movies are indexed on disk rather than loaded into RAM: about 16 bytes per frame, so a two-hour film costs a few MB. There are concurrent-stream limits (global and per IP), per-frame write deadlines for stuck clients, graceful shutdown, and a `/healthz` endpoint.
- **Privacy by design.** No cookies, no analytics, no third-party scripts, and a strict CSP. Logs hold only truncated client networks (`/24` or `/48`), never query strings. See [/privacy](https://tv.bodegga.net/privacy) and [/terms](https://tv.bodegga.net/terms).

## Running it yourself

Requires Go 1.24+ (standard library only, no dependencies).

```sh
go run .                       # http://localhost:8080, plays resources/sw1.txt
curl localhost:8080            # in another terminal (Windows: curl.exe)
go test ./...
```

Docker:

```sh
docker build -t ascii-tv . && docker run -p 8080:8080 ascii-tv
```

Flags:

| Flag | Default | |
|---|---|---|
| `-addr` | `:$PORT`, else `:8080` | listen address |
| `-catalog` | `resources/catalog.json` | catalogue file; repeat to merge several |
| `-movie` / `-height` / `-title` | | serve one movie file without a catalogue |
| `-public-url` | request host | host shown in help text |
| `-max-streams` / `-max-per-ip` | 200 / 4 | concurrency limits |
| `-trust-proxy` | off | trust `X-Real-IP` / `X-Forwarded-Proto` from a loopback proxy |
| `ASCII_TV_KEYS` env | | comma-separated private screening keys |

> **Windows note:** on PCs with Smart App Control or WDAC, Go's toolchain binaries can be blocked
> ("An Application Control policy has blocked this file"). Build and test in a container instead:
> `docker run --rm -v "%cd%:/src" -w /src golang:1.25 go test ./...`

## Adding a movie

You need ffmpeg and Python 3 with numpy (`winget install Gyan.FFmpeg`, `pip install numpy`).

```sh
python tools/convert.py input.mkv movies/mymovie-80.txt  --width 80  --max-rows 23 --subs auto
python tools/convert.py input.mkv movies/mymovie-160.txt --width 160 --max-rows 48 --subs auto
```

The converter crops letterbox bars, corrects for tall terminal characters, smooths contrast
over time, maps brightness to a 16-step ASCII ramp, burns in subtitles (`--subs auto` reads
the video's own text track; `--subs file.srt` also works), and merges identical frames. It
prints the catalogue entry to add. Useful flags: `--ramp classic|film|fine`, `--invert` for
light terminals, and `--start/--duration` to try a short clip first.

Then add it to a catalogue (paths are relative to the catalogue file):

```json
{
  "movies": [{
    "slug": "mymovie", "title": "My Movie", "year": "2024",
    "credit": "Who made it", "captionLines": 2, "private": true,
    "renditions": [
      { "file": "mymovie-80.txt",  "height": 23 },
      { "file": "mymovie-160.txt", "height": 48 }
    ]
  }]
}
```

```sh
go run . -catalog resources/catalog.json -catalog movies/catalog.private.json
```

**Only publish movies you have the rights to share.** Mark anything else `"private": true` so it
needs a screening key. `movies/` is git-ignored.

### Movie file format

Each frame is one line holding a whole number (how long to show the frame, in 1/15 s), followed
by exactly `height` lines of art. LF or CRLF endings both work. ANSI colour codes inside art lines
are passed through (and stripped for the web player). The original Star Wars file uses 13 lines
of up to 67 columns.

## Deploying (tv.bodegga.net)

Production runs on the `bodegga-web` Droplet that also serves bodegga.net: a sandboxed systemd
service on `127.0.0.1:8090`, behind nginx with buffering off and 4-hour timeouts, with TLS from
Let's Encrypt. Plain HTTP is kept on purpose so `curl tv.bodegga.net` works without `-L`, while
browsers get redirected to HTTPS by the app. DNS is a ZoneEdit A record.

```sh
./scripts/deploy.sh            # binary + config
./scripts/deploy.sh --movies   # also upload movies/ (private screenings)
```

See [deploy/](deploy/) for the unit file, nginx site and log-retention config.

## Credits

- **Simon Jansen** hand-drew the Star Wars ASCIImation: <https://www.asciimation.co.nz/>
- **Martin W. Kirst** made the [ascii-telnet-server](https://github.com/nitram509/ascii-telnet-server) that inspired the HTTP version.
- **Martin Raison** wrote the original [ascii-tv](https://github.com/martinraison/ascii-tv) (MIT).
- Revived and extended by [Bodegga](https://bodegga.net).

Not affiliated with Lucasfilm or any rights holder. MIT licensed. See [LICENSE](LICENSE).
