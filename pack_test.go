package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePack(t *testing.T, style string, color bool, frames []string, ticks []int) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(packMagic)
	b.WriteString(`{"width":4,"height":2,"style":"` + style + `","color":` + map[bool]string{true: "true", false: "false"}[color] + "}\n")
	for i, f := range frames {
		var z bytes.Buffer
		w, _ := flate.NewWriter(&z, flate.BestCompression)
		w.Write([]byte(f))
		w.Close()
		b.Write(binary.AppendUvarint(nil, uint64(ticks[i])))
		b.Write(binary.AppendUvarint(nil, uint64(z.Len())))
		b.Write(z.Bytes())
	}
	p := filepath.Join(t.TempDir(), "m.atv")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPackRoundTrip(t *testing.T) {
	p := writePack(t, "hd", true, []string{"\x1b[38;5;196m▀▀\x1b[0m\nab\n", "cd\nef\n"}, []int{2, 3})
	r, warn, err := OpenRendition(p, 2)
	if err != nil || warn != "" {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	defer r.Close()
	if r.Frames() != 2 || r.total != 5 || r.Style != "hd" || !r.Color || r.Width != 4 {
		t.Fatalf("%+v", r)
	}
	lines, d, err := r.ReadFrame(1)
	if err != nil || strings.Join(lines, "|") != "cd|ef" || d != 3*Tick {
		t.Fatalf("%q %v %v", lines, d, err)
	}
	if got := stripANSI([]string{"\x1b[38;5;196mX\x1b[0m"}); got[0] != "X" {
		t.Fatalf("stripANSI %q", got)
	}
	if _, _, err := OpenRendition(p, 9); err == nil {
		t.Fatal("height mismatch should be an error")
	}
}

func TestPickPrefersStyle(t *testing.T) {
	a := &Rendition{Width: 80, Height: 20, Style: "ascii"}
	h := &Rendition{Width: 160, Height: 40, Style: "hd"}
	m := &Movie{Renditions: []*Rendition{a, h}}
	if m.Pick(0, 0, 0, "") != h {
		t.Fatal("default should be hd when available")
	}
	if m.Pick(0, 0, 0, "ascii") != a {
		t.Fatal("style=ascii ignored")
	}
}

func TestGzipStream(t *testing.T) {
	_, ts := testServer(t)
	res, body := get(t, ts.URL+"/demo?format=plain", map[string]string{"Accept-Encoding": "gzip"})
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip")
	}
	zr, err := gzip.NewReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !strings.Contains(string(plain), "world") {
		t.Fatalf("%q", plain)
	}
}
