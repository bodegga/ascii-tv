# Handoff: reviving ascii-tv

## Goal

Bring back a dead project that streams ASCII movies to a terminal over HTTP (`curl <url>` plays Star Wars Episode IV as ASCII animation). Then add the ability to serve our own movies converted from video files.

Done means:

1. The server runs locally on Windows and plays the movie at the right speed.
2. The code is modernized and the known bugs below are fixed.
3. It is deployed at a public URL where `curl <url>` plays the movie frame by frame.
4. A converter script turns any video file into a movie file the server can play.

## Background

- Original repo: https://github.com/martinraison/ascii-tv (MIT license, last touched about 8 years ago)
- The original hosted site, `asciitv.fr`, is dead. It returns Cloudflare error 1016 (origin DNS error), so the server behind it is gone. We don't own that domain and need our own URL.
- The animation was hand-drawn by Simon Jansen (asciimation.co.nz). Keep his credit, the Martin W. Kirst credit, and the original LICENSE copyright notice.

## What's in the repo

Verified by reading the source on 2026-11-17.

| File | Notes |
|---|---|
| `main.go` | The whole server, about 60 lines, standard library only |
| `resources/sw1.txt` | The movie, about 2 MB, 3,412 frames |
| `go.mod` | Declares `go 1.11`, no dependencies (`go.sum` is empty) |
| `Procfile` | `web: ascii-tv -addr :$PORT` (Heroku leftover) |
| `demo.svg` | Animated demo used in the README |

### How the server works

- Flags: `-movie` (default `resources/sw1.txt`) and `-addr` (default `:8080`). It does not read a `PORT` environment variable itself; the Procfile passes it in through `-addr`.
- It loads the movie file into memory at startup, then serves it on `/`.
- For each frame it writes `\033[2J\033[1;1H` (clear screen, cursor to top left), prints the frame's lines, flushes, and sleeps for `duration / 15` seconds.
- If the request's User-Agent doesn't contain `curl`, it returns a one-line message telling the visitor to use curl instead.

### Movie file format

- Each frame is exactly 14 lines: one line holding an integer (how long to show the frame, in 1/15ths of a second), then 13 lines of artwork.
- Artwork lines are up to 67 characters wide. The server doesn't enforce width, only the height of 13, which is hardcoded as `frameHeight`.
- Line endings must be LF. The total line count must be a multiple of 14.

## Setup on Windows

```powershell
winget install GoLang.Go
# reopen the terminal so go is on PATH
git clone https://github.com/<your-fork>/ascii-tv
cd ascii-tv
go run .
```

In a second terminal:

```powershell
curl.exe http://localhost:8080
```

Use `curl.exe`, not `curl`. In Windows PowerShell 5.1, `curl` is an alias for `Invoke-WebRequest` and won't stream.

## Known problems to fix

1. **CRLF breaks playback on Windows.** Git for Windows usually converts line endings to CRLF on checkout. The duration line then reads as `"2\r"`, `strconv.ParseInt` fails, the duration becomes 0, and the whole movie flashes past instantly. Fix both ways: add a `.gitattributes` with `resources/*.txt text eol=lf`, and make the parser strip `\r` from every line.
2. **Missing movie file doesn't stop the server.** The read error is printed and execution continues with zero frames. It should exit with a non-zero status.
3. **Malformed files can panic.** The frame slice `lines[i+1 : i+1+frameHeight]` goes out of range if the file ends in a partial frame. Validate the length and skip or reject the remainder.
4. **Disconnected clients aren't detected.** If the viewer hits Ctrl+C, the handler keeps sleeping through the rest of the movie (about 18 minutes of goroutine per abandoned viewer). Select on `r.Context().Done()` alongside the sleep.
5. **Hardcoded dead URL.** The non-curl message says `curl https://asciitv.fr`. Build it from the request's Host header or a flag.
6. **Deprecated API.** `ioutil.ReadFile` should be `os.ReadFile`. Bump the `go` directive in `go.mod` to a current version.
7. **Frame height is hardcoded.** Make it a flag (`-height`, default 13) so converted movies can use a larger frame.

## Tasks, in order

1. Fork the repo on GitHub and clone the fork.
2. Confirm it runs locally. If the movie plays instantly, that's problem 1.
3. Fix the seven problems above. Keep it standard library only.
4. Update the README: new URL, the Windows `curl.exe` note, how to run locally, how to add a movie. Keep the credits.
5. Pick a host and deploy (see below).
6. Build the video converter (see below).

## Deployment

Heroku, which the Procfile targets, no longer has a free tier. Any host that runs a Go binary or a container works. Fly.io, Render, and Railway are common choices; check current pricing before committing, since free tiers change.

Requirements for the host:

- **No response buffering.** The movie is one long streamed response. If a proxy buffers it, the viewer sees nothing and then the whole movie at once. Test with `curl.exe https://<url>` right after deploying.
- **Long request timeout.** The Star Wars movie runs about 18 minutes. A host that cuts requests at 30 or 60 seconds will truncate it.
- **Port binding.** Most hosts supply the port in a `PORT` variable. Either keep the `-addr :$PORT` start command or teach the server to fall back to `PORT` when `-addr` isn't set.

A small multi-stage Dockerfile (build with the official Go image, copy the binary and `resources/` into a minimal image) covers most hosts.

## Video converter

A script, `tools/convert.py`, that takes a video and writes a movie file in the format above.

1. Extract frames with ffmpeg at 15 fps, scaled to the target size and converted to grayscale. For the default frame: `ffmpeg -i input.mp4 -vf "fps=15,scale=67:13,format=gray" frames/%05d.png`
2. Map each pixel's brightness to a character using a ramp such as ` .:-=+*#%@`.
3. Write each frame as a duration line followed by the artwork lines, with LF endings.
4. Merge consecutive identical frames by raising the duration number instead of repeating the frame. This shrinks the file a lot.

Arguments to support: input path, output path, width, height, fps, and an invert switch for light terminals.

Things to know:

- Terminal characters are roughly twice as tall as they are wide, so scaling straight to 67x13 distorts the picture unless the aspect ratio is corrected. Compute the height from the source aspect ratio and halve it, then pad or crop to the frame height.
- 67x13 is very coarse. High-contrast footage with simple shapes works best. A larger frame such as 80x24 looks much better and needs the `-height` flag from problem 7.
- The duration unit is fixed at 1/15 of a second, so extract at 15 fps and write a duration of 1 per frame (before merging).
- Requires ffmpeg on PATH (`winget install Gyan.FFmpeg`) and Python with Pillow.

Serve the result with `go run . -movie resources/mymovie.txt`.

## Open decisions

- Which host, and whether to buy a short domain.
- Whether to serve more than one movie, for example by path (`curl <url>/starwars`). This would be a new feature; the current server serves a single file on `/`.
- Project name for the fork.
