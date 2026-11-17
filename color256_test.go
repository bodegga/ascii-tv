package main

import "testing"

func TestTo256(t *testing.T) {
	in := []string{"\x1b[38;2;255;0;0m▀\x1b[48;2;0;0;0mX\x1b[0m", "plain", "\x1b[38;5;9mok"}
	got := to256(in)
	want := []string{"\x1b[38;5;196m▀\x1b[48;5;16mX\x1b[0m", "plain", "\x1b[38;5;9mok"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
	if n := xterm256(128, 128, 128); n != 244 {
		t.Errorf("grey 128 -> %d, want 244", n)
	}
}
