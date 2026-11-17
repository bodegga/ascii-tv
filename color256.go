package main

import (
	"strconv"
	"strings"
)

// Movies are converted with 24-bit colour (ESC[38;2;r;g;bm). Nearly every
// terminal in use today shows it, but some older ones (macOS Terminal before
// macOS 26, PuTTY with default settings, tmux without the Tc flag) only know
// the 256-colour palette. ?color=256 rewrites the escapes for them.

var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

func cubeIndex(c int) int {
	switch {
	case c < 48:
		return 0
	case c < 115:
		return 1
	default:
		return min((c-35)/40, 5)
	}
}

// xterm256 returns the nearest palette entry in the 6x6x6 cube or the grey
// ramp, the same mapping the converter uses.
func xterm256(r, g, b int) int {
	ri, gi, bi := cubeIndex(r), cubeIndex(g), cubeIndex(b)
	sq := func(x int) int { return x * x }
	cubeErr := sq(r-cubeLevels[ri]) + sq(g-cubeLevels[gi]) + sq(b-cubeLevels[bi])
	avg := (r + g + b) / 3
	gr := min(max((avg-8)/10, 0), 23)
	gv := 8 + 10*gr
	grayErr := sq(r-gv) + sq(g-gv) + sq(b-gv)
	if grayErr < cubeErr {
		return 232 + gr
	}
	return 16 + 36*ri + 6*gi + bi
}

// to256 rewrites 24-bit colour SGR sequences to their 256-colour equivalents.
func to256(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if !strings.Contains(l, ";2;") {
			out[i] = l
			continue
		}
		out[i] = rewriteLine(l)
	}
	return out
}

func rewriteLine(l string) string {
	var b strings.Builder
	b.Grow(len(l))
	for {
		j := strings.Index(l, "\x1b[")
		if j < 0 {
			b.WriteString(l)
			return b.String()
		}
		end := strings.IndexByte(l[j:], 'm')
		if end < 0 {
			b.WriteString(l)
			return b.String()
		}
		b.WriteString(l[:j])
		b.WriteString(rewriteSGR(l[j+2 : j+end]))
		l = l[j+end+1:]
	}
}

// rewriteSGR converts the parameters of one ESC[...m sequence.
func rewriteSGR(params string) string {
	p := strings.Split(params, ";")
	var o []string
	for i := 0; i < len(p); i++ {
		if (p[i] == "38" || p[i] == "48") && i+4 < len(p) && p[i+1] == "2" {
			r, e1 := strconv.Atoi(p[i+2])
			g, e2 := strconv.Atoi(p[i+3])
			bl, e3 := strconv.Atoi(p[i+4])
			if e1 == nil && e2 == nil && e3 == nil {
				o = append(o, p[i], "5", strconv.Itoa(xterm256(r, g, bl)))
				i += 4
				continue
			}
		}
		o = append(o, p[i])
	}
	return "\x1b[" + strings.Join(o, ";") + "m"
}
