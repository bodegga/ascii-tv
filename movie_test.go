package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenRenditionCRLF(t *testing.T) {
	// Problem 1 from the handoff: Git for Windows checks files out with CRLF,
	// and "2\r" used to fail to parse, so the movie flashed past instantly.
	p := writeTemp(t, "m.txt", "2\r\nab\r\ncd\r\n3\r\nef\r\ngh\r\n")
	r, warn, err := OpenRendition(p, 2)
	if err != nil || warn != "" {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	defer r.Close()
	if r.Frames() != 2 || r.total != 5 {
		t.Fatalf("frames=%d total=%d", r.Frames(), r.total)
	}
	lines, d, err := r.ReadFrame(1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "ef|gh" || d != 3*Tick {
		t.Fatalf("got %q %v", lines, d)
	}
}

func TestOpenRenditionPartialFrame(t *testing.T) {
	// Problem 3: a trailing partial frame used to panic with a slice out of range.
	p := writeTemp(t, "m.txt", "1\na\nb\n1\nc\n")
	r, warn, err := OpenRendition(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Frames() != 1 || warn == "" {
		t.Fatalf("frames=%d warn=%q", r.Frames(), warn)
	}
}

func TestOpenRenditionErrors(t *testing.T) {
	if _, _, err := OpenRendition(filepath.Join(t.TempDir(), "missing.txt"), 13); err == nil {
		t.Fatal("missing file should be an error (problem 2)")
	}
	p := writeTemp(t, "bad.txt", "x\na\n")
	if _, _, err := OpenRendition(p, 1); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Fatalf("bad duration should report line 1, got %v", err)
	}
	p = writeTemp(t, "empty.txt", "\n")
	if _, _, err := OpenRendition(p, 1); !errors.Is(err, ErrNoFrames) {
		t.Fatalf("want ErrNoFrames, got %v", err)
	}
}

func TestWidthIgnoresANSI(t *testing.T) {
	p := writeTemp(t, "c.txt", "1\n\x1b[38;5;196mred\x1b[0m\n")
	r, _, err := OpenRendition(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Width != 3 {
		t.Fatalf("width=%d", r.Width)
	}
}

func TestFrameAt(t *testing.T) {
	p := writeTemp(t, "m.txt", "2\na\n3\nb\n1\nc\n")
	r, _, _ := OpenRendition(p, 1)
	defer r.Close()
	for tick, want := range map[int64]int{0: 0, 1: 0, 2: 1, 4: 1, 5: 2, 99: 2} {
		if got := r.FrameAt(tick); got != want {
			t.Errorf("FrameAt(%d)=%d want %d", tick, got, want)
		}
	}
}

func TestRealStarWarsFile(t *testing.T) {
	r, _, err := OpenRendition("resources/sw1.txt", 13)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Frames() < 3400 || r.Width > 67 {
		t.Fatalf("frames=%d width=%d", r.Frames(), r.Width)
	}
	d := time.Duration(r.total) * Tick
	if d < 15*time.Minute || d > 20*time.Minute {
		t.Fatalf("runtime %v", d)
	}
}

func TestPickRendition(t *testing.T) {
	small := &Rendition{Width: 80, Height: 20}
	big := &Rendition{Width: 160, Height: 40}
	m := &Movie{Renditions: []*Rendition{small, big}}
	cases := []struct {
		cols, rows int
		want       *Rendition
	}{{0, 0, small}, {100, 0, small}, {200, 0, big}, {200, 30, small}, {200, 50, big}}
	for _, c := range cases {
		if got := m.Pick(c.cols, c.rows, 1, ""); got != c.want {
			t.Errorf("Pick(%d,%d) got width %d", c.cols, c.rows, got.Width)
		}
	}
}

func TestCatalogRejectsReservedSlug(t *testing.T) {
	c := newCatalog()
	if err := c.Add(&Movie{Slug: "live", Renditions: []*Rendition{{}}}); err == nil {
		t.Fatal("reserved slug accepted")
	}
}
