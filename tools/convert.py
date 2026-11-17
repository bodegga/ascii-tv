#!/usr/bin/env python3
"""Convert any video into an ascii-tv movie file.

    python tools/convert.py input.mkv movies/mymovie.txt --width 80 --subs auto

The output is the classic ASCIImation format the server plays: each frame is
one line holding a duration (in 1/15 s) followed by HEIGHT lines of artwork,
LF line endings, identical consecutive frames merged into one longer frame.

Pipeline: ffmpeg decodes, crops letterbox bars, scales and converts to
grayscale, then pipes raw pixels here. numpy stretches contrast (smoothed over
time so it doesn't flicker), maps brightness to characters, and burns optional
subtitles into the bottom rows, because ascii-tv has no audio.

Requires ffmpeg/ffprobe on PATH and numpy (pip install numpy).
"""
from __future__ import annotations

import argparse
import os
import html
import json
import re
import subprocess
import sys
import textwrap
import time
import unicodedata
import zlib
from dataclasses import dataclass
from pathlib import Path

try:
    import numpy as np
except ImportError:  # pragma: no cover
    sys.exit("convert.py needs numpy:  python -m pip install numpy")

TICKS_PER_SECOND = 15  # fixed by the movie format

RAMPS = {
    # 10 levels: bold, readable at small sizes (the handoff's suggestion).
    "classic": " .:-=+*#%@",
    # 16 levels tuned for film: more midtones, still pure ASCII.
    "film": " .`',:;~-=+*o#%@",
    # Paul Bourke's 70-level ramp: most detail, busier look.
    "fine": " .'`^\",:;Il!i><~+_-?][}{1)(|\\/tfjrxnuvczXYUJCLQ0OZmwqpdbkhao*#MW&8%B@$",
}

# Typographic characters subtitles commonly contain, mapped to ASCII so every
# terminal (including legacy Windows consoles) shows them correctly.
ASCII_MAP = str.maketrans({
    "‘": "'", "’": "'", "“": '"', "”": '"', "–": "-", "—": "--",
    "…": "...", "♪": "~", "♫": "~", " ": " ", "•": "*",
})


# ---- subtitles -----------------------------------------------------------------

@dataclass
class Cue:
    start: float
    end: float
    text: str


_TS = re.compile(r"(\d+):(\d+):(\d+)[,.](\d+)")


def _secs(ts: str) -> float:
    m = _TS.search(ts)
    if not m:
        raise ValueError(f"bad SRT timestamp {ts!r}")
    h, mi, s, ms = m.groups()
    return int(h) * 3600 + int(mi) * 60 + int(s) + int(ms.ljust(3, "0")[:3]) / 1000


def to_ascii(text: str) -> str:
    text = text.translate(ASCII_MAP)
    text = unicodedata.normalize("NFKD", text)
    return text.encode("ascii", "ignore").decode("ascii")


def clean_cue_text(raw: str) -> str:
    text = re.sub(r"\{\\[^}]*\}", "", raw)          # ASS override tags {\an8}
    text = re.sub(r"</?[a-zA-Z][^>]*>", "", text)    # <i>, <font ...>
    text = html.unescape(text)
    text = " ".join(line.strip() for line in text.splitlines() if line.strip())
    return re.sub(r"\s+", " ", to_ascii(text)).strip()


def parse_srt(data: str) -> list[Cue]:
    cues = []
    for block in re.split(r"\r?\n\s*\r?\n", data.replace("﻿", "")):
        lines = [l for l in block.strip().splitlines()]
        idx = next((i for i, l in enumerate(lines) if "-->" in l), None)
        if idx is None:
            continue
        a, b = lines[idx].split("-->", 1)
        text = clean_cue_text("\n".join(lines[idx + 1:]))
        if text:
            cues.append(Cue(_secs(a), _secs(b), text))
    cues.sort(key=lambda c: c.start)
    return cues


def extract_subs(video: str) -> list[Cue]:
    """Pull the first text subtitle track out of the video, if it has one."""
    probe = run_json(["ffprobe", "-v", "error", "-select_streams", "s",
                      "-show_entries", "stream=index,codec_name:stream_tags=language",
                      "-of", "json", video])
    text_codecs = {"subrip", "ass", "ssa", "mov_text", "webvtt", "text"}
    streams = [s for s in probe.get("streams", []) if s.get("codec_name") in text_codecs]
    if not streams:
        return []
    eng = [s for s in streams if s.get("tags", {}).get("language", "").startswith("en")]
    pick = (eng or streams)[0]["index"]
    out = subprocess.run(["ffmpeg", "-v", "error", "-i", video, "-map", f"0:{pick}", "-f", "srt", "-"],
                         capture_output=True, check=True)
    return parse_srt(out.stdout.decode("utf-8", "replace"))


class SubtitleTrack:
    def __init__(self, cues: list[Cue], width: int, lines: int):
        self.cues, self.width, self.lines = cues, width, lines
        self.i = 0
        self._cache: dict[int, list[str]] = {}

    def at(self, t: float) -> list[str]:
        while self.i < len(self.cues) and self.cues[self.i].end <= t:
            self.i += 1
        if self.i < len(self.cues) and self.cues[self.i].start <= t:
            return self._render(self.i)
        return [" " * self.width] * self.lines

    def _render(self, i: int) -> list[str]:
        if i not in self._cache:
            wrapped = textwrap.wrap(self.cues[i].text, self.width - 2) or [""]
            if len(wrapped) > self.lines:  # too long: keep what fits, mark the cut
                wrapped = wrapped[: self.lines]
                wrapped[-1] = wrapped[-1][: self.width - 5].rstrip() + "..."
            wrapped = [""] * (self.lines - len(wrapped)) + wrapped  # bottom-align
            self._cache[i] = [w.center(self.width) for w in wrapped]
        return self._cache[i]


# ---- video ---------------------------------------------------------------------

def run_json(cmd: list[str]) -> dict:
    return json.loads(subprocess.run(cmd, capture_output=True, check=True, text=True).stdout or "{}")


def probe(video: str) -> tuple[int, int, float]:
    info = run_json(["ffprobe", "-v", "error", "-select_streams", "v:0",
                     "-show_entries", "stream=width,height:stream_side_data=rotation:format=duration",
                     "-of", "json", video])
    s = info["streams"][0]
    return int(s["width"]), int(s["height"]), float(info.get("format", {}).get("duration", 0) or 0)


def detect_crop(video: str, duration: float) -> str | None:
    """Find letterbox bars by sampling a few points in the film."""
    found = []
    for frac in (0.2, 0.4, 0.6, 0.8):
        ss = max(0.0, duration * frac) if duration else 0
        r = subprocess.run(["ffmpeg", "-v", "info", "-ss", f"{ss:.2f}", "-i", video, "-t", "4",
                            "-vf", "cropdetect=limit=24:round=2:reset=0", "-an", "-sn", "-f", "null", "-"],
                           capture_output=True, text=True)
        m = re.findall(r"crop=(\d+):(\d+):(\d+):(\d+)", r.stderr)
        if m:
            found.append(tuple(map(int, m[-1])))
    if not found:
        return None
    # Use the largest box seen, so a dark scene never crops real picture.
    w = max(f[0] for f in found)
    h = max(f[1] for f in found)
    x = min(f[2] for f in found)
    y = min(f[3] for f in found)
    return f"{w}:{h}:{x}:{y}"


def frames(video: str, vf: str, w: int, h: int, start: float | None, duration: float | None, rgb: bool = False):
    cmd = ["ffmpeg", "-v", "error", "-nostdin"]
    if start:
        cmd += ["-ss", str(start)]
    cmd += ["-i", video]
    if duration:
        cmd += ["-t", str(duration)]
    cmd += ["-map", "0:v:0", "-an", "-sn", "-vf", vf, "-pix_fmt", "rgb24" if rgb else "gray", "-f", "rawvideo", "-"]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE)
    size = w * h * (3 if rgb else 1)
    try:
        while True:
            buf = proc.stdout.read(size)
            if len(buf) < size:
                break
            a = np.frombuffer(buf, dtype=np.uint8)
            yield a.reshape(h, w, 3) if rgb else a.reshape(h, w)
    finally:
        proc.stdout.close()
        proc.wait()
        if proc.returncode not in (0, None, -15) and proc.returncode != 255:
            raise SystemExit(f"ffmpeg failed with exit code {proc.returncode}")


class ToneMapper:
    """Per-frame contrast stretch with exponential smoothing so brightness
    doesn't pump from frame to frame."""

    def __init__(self, ramp: str, invert: bool, gamma: float, auto: bool):
        self.lut_chars = np.frombuffer(ramp.encode("ascii"), dtype=np.uint8)
        self.levels = len(ramp)
        self.invert, self.gamma, self.auto = invert, gamma, auto
        self.lo, self.hi = 0.0, 255.0
        self.first = True

    def __call__(self, px: np.ndarray) -> list[bytes]:
        return [row.tobytes() for row in self.chars(px)]

    def chars(self, px: np.ndarray) -> np.ndarray:
        f = px.astype(np.float32)
        if self.auto:
            lo, hi = np.percentile(f, (1.5, 99.5))
            hi = max(hi, lo + 48)  # don't blow up noise in near-black scenes
            a = 1.0 if self.first else 0.15
            self.lo += (lo - self.lo) * a
            self.hi += (hi - self.hi) * a
            self.first = False
            f = (f - self.lo) / max(self.hi - self.lo, 1)
        else:
            f /= 255.0
        f = np.clip(f, 0, 1) ** self.gamma
        if self.invert:
            f = 1 - f
        idx = np.minimum((f * self.levels).astype(np.int32), self.levels - 1)
        return self.lut_chars[idx]



# ---- colour ------------------------------------------------------------------
#
# 256-colour ANSI (xterm palette): every modern terminal supports it, it keeps
# files small, and the web player can render it with plain CSS classes.

_CUBE = np.array([0, 95, 135, 175, 215, 255], dtype=np.int32)


def xterm256(rgb: np.ndarray) -> np.ndarray:
    """Map an (..., 3) uint8 RGB array to the nearest xterm-256 colour index."""
    c = rgb.astype(np.int32)
    lv = np.where(c < 48, 0, np.where(c < 115, 1, (c - 35) // 40)).clip(0, 5)
    cube = 16 + 36 * lv[..., 0] + 6 * lv[..., 1] + lv[..., 2]
    cube_err = ((c - _CUBE[lv]) ** 2).sum(-1)
    avg = c.sum(-1) // 3
    gi = ((avg - 8) // 10).clip(0, 23)
    gv = (8 + 10 * gi)[..., None]
    gray_err = ((c - gv) ** 2).sum(-1)
    return np.where(gray_err < cube_err, 232 + gi, cube).astype(np.int32)


def _esc_table(fmt: str) -> np.ndarray:
    t = np.empty(256, dtype=object)
    for n in range(256):
        t[n] = fmt.format(n).encode()
    return t


FG = _esc_table("\x1b[38;5;{}m")
BG = _esc_table("\x1b[48;5;{}m")
FGBG = np.empty(256 * 256, dtype=object)
for _f in range(256):
    for _b in range(256):
        FGBG[_f * 256 + _b] = f"\x1b[38;5;{_f};48;5;{_b}m".encode()
CHARS = np.empty(256, dtype=object)
for _i in range(256):
    CHARS[_i] = bytes([_i])
UPPER_HALF = "▀".encode()  # top half block: fg = upper pixel, bg = lower
RESET = b"\x1b[0m"


def _changes(codes: np.ndarray) -> np.ndarray:
    ch = np.ones(codes.shape, dtype=bool)
    ch[:, 1:] = codes[:, 1:] != codes[:, :-1]
    return ch


class Stretch:
    """Contrast stretch on luminance, smoothed over time, applied to RGB."""

    def __init__(self, auto: bool, gamma: float):
        self.auto, self.gamma = auto, gamma
        self.lo, self.hi, self.first = 0.0, 255.0, True

    def __call__(self, rgb: np.ndarray) -> np.ndarray:
        f = rgb.astype(np.float32)
        if self.auto:
            lum = f @ np.array([0.299, 0.587, 0.114], dtype=np.float32)
            lo, hi = np.percentile(lum, (1.0, 99.5))
            hi = max(hi, lo + 48)
            a = 1.0 if self.first else 0.15
            self.lo += (lo - self.lo) * a
            self.hi += (hi - self.hi) * a
            self.first = False
            f = (f - self.lo) * (255.0 / max(self.hi - self.lo, 1))
        f = np.clip(f / 255.0, 0, 1) ** self.gamma * 255.0
        return f.astype(np.uint8)


def _prefixes(mask_a, table_a, mask_b=None, table_b=None, both=None):
    out = np.empty(mask_a.shape, dtype=object)
    out.fill(b"")
    if mask_b is not None:
        out[mask_b] = table_b[mask_b]
    out[mask_a] = table_a[mask_a]
    if both is not None:
        m, t = both
        out[m] = t[m]
    return out


def render_hd(rgb: np.ndarray, stretch: Stretch) -> list[bytes]:
    """Two pixels per character cell using the upper-half block."""
    codes = xterm256(stretch(rgb))
    fg, bg = codes[0::2], codes[1::2]
    fgc, bgc = _changes(fg), _changes(bg)
    pre = _prefixes(fgc, FG[fg], bgc, BG[bg], (fgc & bgc, FGBG[fg * 256 + bg]))
    return [b"".join(p + UPPER_HALF for p in row) + RESET for row in pre]


def render_color_ascii(rgb: np.ndarray, tone, stretch: Stretch) -> list[bytes]:
    """Characters by brightness, coloured by the pixel's hue."""
    lum = (rgb.astype(np.float32) @ np.array([0.299, 0.587, 0.114], dtype=np.float32)).astype(np.uint8)
    chars = tone.chars(lum)
    px = stretch(rgb).astype(np.float32)
    # Brightness is already carried by the glyph; push colour toward full
    # value so dim glyphs stay readable on a black terminal.
    peak = px.max(-1, keepdims=True)
    px = np.clip(px * (255.0 / np.maximum(peak, 1)) * 0.92, 0, 255)
    codes = xterm256(px.astype(np.uint8))
    # Spaces need no colour: carry the previous code over them so an escape is
    # only emitted when a visible glyph changes colour.
    blank = chars == 32
    idx = np.where(blank, 0, np.arange(codes.shape[1])[None, :])
    idx = np.maximum.accumulate(idx, axis=1)
    codes = np.take_along_axis(codes, idx, axis=1)
    pre = _prefixes(_changes(codes) & ~blank, FG[codes])
    rows = []
    for r in range(chars.shape[0]):
        cells = CHARS[chars[r]]
        rows.append(b"".join(p + c for p, c in zip(pre[r], cells)).rstrip(b" ") + RESET)
    return rows


# ---- 24-bit colour --------------------------------------------------------------
#
# True colour looks far better than the 256-colour cube (skin tones, skies and
# shadows don't band or collapse to grey). Channels are kept to 5 bits so
# neighbouring cells share escapes and the packs compress well; the server can
# still convert to 256 colours for old terminals (?color=256).

def _lvl(q: int) -> int:
    return q * 8 + (q >> 2)  # 0..31 -> 0..255


def _tc_table(fmt: str) -> np.ndarray:
    t = np.empty(32768, dtype=object)
    for k in range(32768):
        t[k] = fmt.format(_lvl(k >> 10), _lvl((k >> 5) & 31), _lvl(k & 31)).encode()
    return t


TC_FG = _tc_table("[38;2;{};{};{}m")
TC_BG = _tc_table("[48;2;{};{};{}m")
EMPTY = np.empty(1, dtype=object)
EMPTY[0] = b""


COLOR_BITS = 5  # per channel; overridable with --color-bits


def _key15(px: np.ndarray, bits: int | None = None) -> np.ndarray:
    drop = 8 - (bits or COLOR_BITS)
    c = (np.clip(px, 0, 255).astype(np.int32) >> drop << drop) >> 3
    return (c[..., 0] << 10) | (c[..., 1] << 5) | c[..., 2]


def _tc_prefix(fg: np.ndarray, bg: np.ndarray, skip_fg: np.ndarray | None = None) -> np.ndarray:
    """Escape prefix per cell: only the colours that changed since the cell before."""
    fgc, bgc = _changes(fg), _changes(bg)
    if skip_fg is not None:
        fgc &= ~skip_fg
    return np.where(fgc, TC_FG[fg], EMPTY[0]) + np.where(bgc, TC_BG[bg], EMPTY[0])


def render_hd_tc(rgb: np.ndarray, stretch: Stretch) -> list[bytes]:
    """Half-block pixels in 24-bit colour."""
    key = _key15(stretch(rgb))
    pre = _tc_prefix(key[0::2], key[1::2])
    return [b"".join(p + UPPER_HALF for p in row) + RESET for row in pre]


TEXT_BG = float(os.environ.get("ATV_TEXT_BG", "0.8"))       # cell tint, fraction of the colour
TEXT_FG = tuple(float(v) for v in os.environ.get("ATV_TEXT_FG", "0.9,90").split(","))  # glyph gain, lift
TEXT_RAMP = np.frombuffer(b" .:-=+*#%@", dtype=np.uint8)


def render_text_tc(rgb: np.ndarray, stretch: Stretch) -> list[bytes]:
    """Colour text: each cell is tinted with the picture's colour (background)
    and carries a brighter character chosen by brightness, so the image reads
    as a picture from a distance and as ASCII up close."""
    px = stretch(rgb).astype(np.float32)
    lum = px @ np.array([0.299, 0.587, 0.114], dtype=np.float32)
    idx = np.minimum((lum * (len(TEXT_RAMP) / 256.0)).astype(np.int32), len(TEXT_RAMP) - 1)
    chars = TEXT_RAMP[idx]
    # Coarser colour steps for the glyphs: they're small, and fewer distinct
    # colours means far fewer escape codes.
    fg = _key15(np.clip(px * TEXT_FG[0] + TEXT_FG[1], 0, 255), min(COLOR_BITS, 4))
    bg = _key15(px * TEXT_BG)
    blank = chars == 32
    # A space shows no glyph: carry the previous glyph colour over it.
    pos = np.where(blank, 0, np.arange(fg.shape[1])[None, :])
    fg = np.take_along_axis(fg, np.maximum.accumulate(pos, axis=1), axis=1)
    pre = _tc_prefix(fg, bg)
    rows = []
    for r in range(chars.shape[0]):
        rows.append(b"".join(p + c for p, c in zip(pre[r], CHARS[chars[r]])) + RESET)
    return rows


class PackWriter:
    """Writes the .atv format the server reads (see pack.go)."""

    def __init__(self, f, width: int, height: int, style: str, color: bool):
        self.f = f
        f.write(b"ATV1\n")
        f.write(json.dumps({"width": width, "height": height, "style": style, "color": color}).encode() + b"\n")

    @staticmethod
    def _uvarint(n: int) -> bytes:
        out = bytearray()
        while True:
            b = n & 0x7F
            n >>= 7
            if n:
                out.append(b | 0x80)
            else:
                out.append(b)
                return bytes(out)

    def frame(self, art: bytes, ticks: int):
        c = zlib.compressobj(6, zlib.DEFLATED, -15)
        z = c.compress(art) + c.flush()
        self.f.write(self._uvarint(ticks) + self._uvarint(len(z)) + z)


# ---- main ----------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Convert a video into an ascii-tv movie file.")
    p.add_argument("input", help="video file (anything ffmpeg can read)")
    p.add_argument("output", help="movie file to write, e.g. movies/mymovie.txt")
    p.add_argument("--width", type=int, default=80, help="columns (default 80)")
    p.add_argument("--height", type=int, help="picture rows (default: from the aspect ratio)")
    p.add_argument("--max-rows", type=int, default=0,
                   help="cap picture + subtitle rows; extra picture is cropped top and bottom")
    p.add_argument("--fps", type=int, default=15, choices=[1, 3, 5, 15], help="frames per second (default 15)")
    p.add_argument("--cell-aspect", type=float, default=0.5,
                   help="character width / height in your terminal font (default 0.5)")
    p.add_argument("--ramp", default="film", help=f"one of {', '.join(RAMPS)} or a literal string, dark to bright")
    p.add_argument("--invert", action="store_true", help="for light-background terminals")
    p.add_argument("--gamma", type=float, default=0.9, help="<1 lifts shadows, >1 deepens them (default 0.9)")
    p.add_argument("--no-auto-contrast", action="store_true", help="disable per-scene contrast stretch")
    p.add_argument("--crop", default="auto", help="'auto' (remove letterbox bars), 'none', or ffmpeg w:h:x:y")
    p.add_argument("--subs", default="none", help="'auto' (from the video), 'none', or an .srt file")
    p.add_argument("--sub-lines", type=int, default=2, help="rows reserved for subtitles (default 2)")
    p.add_argument("--start", type=float, help="start at this many seconds (for clips and tests)")
    p.add_argument("--duration", type=float, help="convert only this many seconds")
    p.add_argument("--style", choices=["ascii", "hd"], default="ascii",
                   help="ascii: characters (default). hd: colour half-block pixels, twice the vertical detail")
    p.add_argument("--color", action="store_true", help="colour the ascii style (hd is always colour)")
    p.add_argument("--palette", choices=["24bit", "256"], default="24bit",
                   help="colour depth (default 24bit; the server can down-convert to 256 per viewer)")
    p.add_argument("--pack", action="store_true",
                   help="write a compressed .atv pack (implied by a .atv output name); use it for colour")
    p.add_argument("--color-bits", type=int, default=5, choices=[3, 4, 5],
                   help="24bit only: bits per colour channel (default 5; fewer = smaller files)")
    p.add_argument("--quiet", action="store_true")
    a = p.parse_args(argv)

    global COLOR_BITS
    COLOR_BITS = a.color_bits
    if a.output.endswith(".atv"):
        a.pack = True
    color = a.color or a.style == "hd"
    ramp = RAMPS.get(a.ramp, a.ramp)
    if len(ramp) < 2 or not ramp.isascii():
        p.error("--ramp needs at least two ASCII characters")
    if a.width < 8:
        p.error("--width must be at least 8")

    src_w, src_h, src_dur = probe(a.input)
    crop = None
    if a.crop == "auto":
        crop = detect_crop(a.input, src_dur)
    elif a.crop != "none":
        crop = a.crop
    eff_w, eff_h = src_w, src_h
    if crop:
        cw, ch = map(int, crop.split(":")[:2])
        if cw >= src_w * 0.98 and ch >= src_h * 0.98:
            crop = None  # nothing worth removing
        else:
            eff_w, eff_h = cw, ch

    cues: list[Cue] = []
    if a.subs == "auto":
        cues = extract_subs(a.input)
    elif a.subs != "none":
        cues = parse_srt(Path(a.subs).read_text(encoding="utf-8", errors="replace"))
    sub_lines = a.sub_lines if cues else 0

    natural = max(1, round(a.width * eff_h / eff_w * a.cell_aspect))
    rows = a.height or natural
    if a.max_rows and rows + sub_lines > a.max_rows:
        rows = a.max_rows - sub_lines
    if rows < 1:
        p.error("not enough rows left for the picture")
    # Scale to cover width x rows without distorting, then centre-crop.
    if natural >= rows:
        scaled_w, scaled_h = a.width, natural
    else:
        scaled_w, scaled_h = max(a.width, round(rows * eff_w / eff_h / a.cell_aspect)), rows

    vf = []
    if crop:
        vf.append(f"crop={crop}")
    vf.append(f"fps={a.fps}")
    py = 2 if a.style == "hd" else 1  # hd: two pixel rows per text row
    vf.append(f"scale={scaled_w}:{scaled_h * py}:flags=area")
    if (scaled_w, scaled_h) != (a.width, rows):
        vf.append(f"crop={a.width}:{rows * py}")
    vf = ",".join(vf)

    tone = ToneMapper(ramp, a.invert, a.gamma, not a.no_auto_contrast)
    stretch = Stretch(not a.no_auto_contrast, a.gamma)
    subs = SubtitleTrack(cues, a.width, sub_lines) if sub_lines else None
    ticks_per_frame = TICKS_PER_SECOND // a.fps
    t0 = a.start or 0.0
    total_est = (a.duration or max(src_dur - t0, 0)) * a.fps

    out_path = Path(a.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    tmp = out_path.with_suffix(out_path.suffix + ".part")
    written = n = 0
    began = time.monotonic()
    prev: bytes | None = None
    prev_ticks = 0
    with open(tmp, "wb") as out:
        packer = PackWriter(out, a.width, rows + sub_lines, a.style, color) if a.pack else None

        def emit(art: bytes, ticks: int):
            nonlocal written
            if packer:
                packer.frame(art, ticks)
                written += 1
                return
            # Very long holds are split so the duration field stays small.
            while ticks > 0:
                step = min(ticks, 9999)
                out.write(b"%d\n" % step)
                out.write(art)
                written += 1
                ticks -= step

        for px in frames(a.input, vf, a.width, rows * py, a.start, a.duration, rgb=color):
            if a.style == "hd":
                lines = render_hd_tc(px, stretch) if a.palette == "24bit" else render_hd(px, stretch)
            elif color:
                lines = render_text_tc(px, stretch) if a.palette == "24bit" else render_color_ascii(px, tone, stretch)
            else:
                lines = tone(px)
            if subs:
                lines += [s.encode("ascii") for s in subs.at(t0 + n / a.fps)]
            art = b"\n".join(lines if color else [l.rstrip() for l in lines]) + b"\n"
            if art == prev:
                prev_ticks += ticks_per_frame
            else:
                if prev is not None:
                    emit(prev, prev_ticks)
                prev, prev_ticks = art, ticks_per_frame
            n += 1
            if not a.quiet and n % 1500 == 0:
                el = time.monotonic() - began
                pct = f"{100 * n / total_est:4.1f}%" if total_est else ""
                print(f"\r  {pct} {n} frames, {n / el:.0f} fps", end="", file=sys.stderr, flush=True)
        if prev is not None:
            emit(prev, prev_ticks)
    if n == 0:
        tmp.unlink(missing_ok=True)
        print("error: ffmpeg produced no frames", file=sys.stderr)
        return 1
    tmp.replace(out_path)

    height = rows + sub_lines
    entry = {"file": out_path.name, "height": height}
    if not a.quiet:
        el = time.monotonic() - began
        print(f"\r  wrote {out_path}: {written} frames ({n} before merging), "
              f"{a.width}x{height}, {n / a.fps / 60:.1f} min, {out_path.stat().st_size / 1e6:.1f} MB "
              f"in {el:.0f}s", file=sys.stderr)
        print("  catalog rendition: " + json.dumps(entry)
              + (f'   (and "captionLines": {sub_lines} on the movie)' if sub_lines else ""), file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
