package main

// Packed movie files (.atv): the same frames as the text format, but each
// frame is compressed on its own so the server can still seek and read one
// frame at a time. Colour (ANSI) frames compress 5-10x, which is what makes
// HD colour films fit on a small server.
//
//	"ATV1\n"
//	{"width":160,"height":45,"style":"hd","color":true}\n
//	repeated: uvarint ticks, uvarint size, size bytes of raw DEFLATE
//
// The decompressed frame is exactly what a text movie file holds for that
// frame: `height` lines, each ending in "\n".

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const packMagic = "ATV1\n"

type packHeader struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Style  string `json:"style"` // "ascii" or "hd"
	Color  bool   `json:"color"`
}

// isPack reports whether f starts with the pack magic. It rewinds f.
func isPack(f *os.File) bool {
	buf := make([]byte, len(packMagic))
	n, _ := io.ReadFull(f, buf)
	f.Seek(0, io.SeekStart)
	return n == len(packMagic) && string(buf) == packMagic
}

type countingReader struct {
	r   *bufio.Reader
	off int64
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.off++
	}
	return b, err
}

func openPack(path string, f *os.File, height int) (*Rendition, string, error) {
	cr := &countingReader{r: bufio.NewReaderSize(f, 1<<16)}
	line, err := cr.r.ReadString('\n') // magic
	if err != nil {
		return nil, "", err
	}
	cr.off += int64(len(line))
	line, err = cr.r.ReadString('\n')
	if err != nil {
		return nil, "", fmt.Errorf("%s: missing pack header: %w", path, err)
	}
	cr.off += int64(len(line))
	var h packHeader
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return nil, "", fmt.Errorf("%s: bad pack header: %w", path, err)
	}
	if height > 0 && h.Height != height {
		return nil, "", fmt.Errorf("%s: catalog says height %d but the file has %d", path, height, h.Height)
	}
	if h.Style == "" {
		h.Style = "ascii"
	}
	r := &Rendition{Path: path, Width: h.Width, Height: h.Height, Style: h.Style, Color: h.Color, file: f, packed: true}
	warning := ""
	for {
		ticks, err := binary.ReadUvarint(cr)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		size, err := binary.ReadUvarint(cr)
		if err != nil {
			warning = fmt.Sprintf("%s: truncated after %d frames", path, len(r.frames))
			break
		}
		off := cr.off
		n, err := cr.r.Discard(int(size))
		cr.off += int64(n)
		if err != nil {
			warning = fmt.Sprintf("%s: dropped a partial frame after %d frames", path, len(r.frames))
			break
		}
		r.start = append(r.start, r.total)
		r.total += int64(ticks)
		r.frames = append(r.frames, frameRef{off: off, size: int32(size), ticks: int32(ticks)})
	}
	if len(r.frames) == 0 {
		return nil, "", fmt.Errorf("%s: %w", path, ErrNoFrames)
	}
	return r, warning, nil
}

func (r *Rendition) readPacked(fr frameRef) ([]byte, error) {
	buf := make([]byte, fr.size)
	if _, err := r.file.ReadAt(buf, fr.off); err != nil && err != io.EOF {
		return nil, err
	}
	zr := flate.NewReader(bytes.NewReader(buf))
	defer zr.Close()
	return io.ReadAll(zr)
}

// stripANSI removes colour escapes, for viewers who ask for ?color=0.
func stripANSI(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.IndexByte(l, 0x1b) >= 0 {
			l = ansiRE.ReplaceAllString(l, "")
		}
		out[i] = l
	}
	return out
}
