package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Tick is the movie file's time unit: every frame's duration line counts
// fifteenths of a second. This is fixed by the original ASCIImation format.
const Tick = time.Second / 15

// frameRef locates one frame's artwork inside a movie file. Frames are read
// from disk on demand, so a two-hour film costs a few MB of index instead of
// hundreds of MB of RAM.
type frameRef struct {
	off   int64 // byte offset of the first artwork line
	size  int32 // bytes of artwork, including line endings
	ticks int32 // how long to show it, in Ticks
}

// Rendition is one movie file at one frame size. A movie can ship several
// (for example 80 and 160 columns) and the server picks the biggest one that
// fits the viewer's terminal.
type Rendition struct {
	Path   string
	Width  int // widest artwork line, in terminal cells
	Height int // artwork lines per frame

	file   *os.File
	frames []frameRef
	start  []int64 // start[i] = tick at which frame i begins
	total  int64   // total length in ticks
}

// Movie is one catalog entry.
type Movie struct {
	Slug    string
	Title   string
	Year    string
	Credit  string
	Private bool
	// CaptionLines is how many rows at the bottom of each frame hold burned-in
	// subtitles. The web player mirrors them into a screen-reader live region.
	CaptionLines int
	Renditions   []*Rendition // sorted by Width, smallest first
}

// Duration is the running time of the movie.
func (m *Movie) Duration() time.Duration {
	return time.Duration(m.Renditions[0].total) * Tick
}

// Pick returns the largest rendition that fits in cols x rows (reserving
// `reserve` rows for chrome such as the status bar). Zero means unknown, and
// unknown dimensions fall back to the smallest rendition, which is the
// classic-terminal-safe choice.
func (m *Movie) Pick(cols, rows, reserve int) *Rendition {
	best := m.Renditions[0]
	if cols <= 0 {
		return best
	}
	for _, r := range m.Renditions {
		if r.Width <= cols && (rows <= 0 || r.Height+reserve <= rows) {
			best = r
		}
	}
	return best
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// visibleWidth counts terminal cells, ignoring ANSI colour escapes.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiRE.ReplaceAllString(s, ""))
}

// ErrNoFrames is returned for files that contain no complete frame.
var ErrNoFrames = errors.New("movie file contains no complete frames")

// OpenRendition indexes a movie file. Each frame is one duration line followed
// by `height` artwork lines. CRLF and LF endings are both accepted. A trailing
// partial frame is dropped (and reported via the returned warning) instead of
// crashing the server.
func OpenRendition(path string, height int) (r *Rendition, warning string, err error) {
	if height < 1 {
		return nil, "", fmt.Errorf("%s: frame height must be at least 1, got %d", path, height)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	r = &Rendition{Path: path, Height: height, file: f}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()

	br := bufio.NewReaderSize(f, 1<<16)
	var off int64
	lineNo := 0
	readLine := func() (string, int, error) {
		b, err := br.ReadBytes('\n')
		if len(b) == 0 && err != nil {
			return "", 0, err
		}
		lineNo++
		return strings.TrimRight(string(b), "\r\n"), len(b), nil
	}

	for {
		durLine, n, err := readLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		off += int64(n)
		durLine = strings.TrimSpace(durLine)
		if durLine == "" && isTrailingBlank(br) {
			break // tolerate a final empty line
		}
		ticks, perr := strconv.ParseInt(durLine, 10, 32)
		if perr != nil || ticks < 0 {
			return nil, "", fmt.Errorf("%s:%d: expected a frame duration (whole number of 1/15 s), got %q", path, lineNo, durLine)
		}
		fr := frameRef{off: off, ticks: int32(ticks)}
		got := 0
		for got < height {
			line, n, err := readLine()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, "", err
			}
			off += int64(n)
			fr.size += int32(n)
			// Trailing blanks don't count: sw1.txt has one line of 132 spaces.
			if w := visibleWidth(strings.TrimRight(line, " \t")); w > r.Width {
				r.Width = w
			}
			got++
		}
		if got < height {
			warning = fmt.Sprintf("%s: dropped a partial frame at line %d (%d of %d lines)", path, lineNo-got, got, height)
			break
		}
		r.start = append(r.start, r.total)
		r.total += ticks
		r.frames = append(r.frames, fr)
	}
	if len(r.frames) == 0 {
		return nil, "", fmt.Errorf("%s: %w", path, ErrNoFrames)
	}
	return r, warning, nil
}

// isTrailingBlank reports whether nothing but whitespace remains.
func isTrailingBlank(br *bufio.Reader) bool {
	rest, _ := io.ReadAll(br)
	return len(bytes.TrimSpace(rest)) == 0
}

// Frames is the number of frames in the rendition.
func (r *Rendition) Frames() int { return len(r.frames) }

// FrameAt returns the index of the frame showing at tick t.
func (r *Rendition) FrameAt(t int64) int {
	if t <= 0 {
		return 0
	}
	i := sort.Search(len(r.start), func(i int) bool { return r.start[i] > t }) - 1
	if i < 0 {
		i = 0
	}
	return i
}

// StartTick returns the tick at which frame i begins.
func (r *Rendition) StartTick(i int) int64 { return r.start[i] }

// ReadFrame returns frame i's artwork lines (without line endings) and how
// long to show it.
func (r *Rendition) ReadFrame(i int) ([]string, time.Duration, error) {
	fr := r.frames[i]
	buf := make([]byte, fr.size)
	if _, err := r.file.ReadAt(buf, fr.off); err != nil && err != io.EOF {
		return nil, 0, err
	}
	buf = bytes.ReplaceAll(buf, []byte("\r"), nil)
	lines := strings.Split(strings.TrimSuffix(string(buf), "\n"), "\n")
	return lines, time.Duration(fr.ticks) * Tick, nil
}

// Close releases the underlying file.
func (r *Rendition) Close() error { return r.file.Close() }

// ---- catalog ---------------------------------------------------------------

type catalogFile struct {
	Default string `json:"default"`
	Movies  []struct {
		Slug         string `json:"slug"`
		Title        string `json:"title"`
		Year         string `json:"year"`
		Credit       string `json:"credit"`
		Private      bool   `json:"private"`
		CaptionLines int    `json:"captionLines"`
		Renditions   []struct {
			File   string `json:"file"`
			Height int    `json:"height"`
		} `json:"renditions"`
	} `json:"movies"`
}

// Catalog is every movie the server knows about.
type Catalog struct {
	Default string
	Movies  []*Movie // in catalog order
	bySlug  map[string]*Movie
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// reservedSlugs are paths the server uses itself.
var reservedSlugs = map[string]bool{"ls": true, "help": true, "healthz": true, "api": true, "static": true, "favicon.ico": true, "robots.txt": true, "live": true, "privacy": true, "terms": true}

func newCatalog() *Catalog { return &Catalog{bySlug: map[string]*Movie{}} }

// Get looks a movie up by slug.
func (c *Catalog) Get(slug string) *Movie { return c.bySlug[slug] }

// Add registers a movie, rejecting duplicate or reserved slugs.
func (c *Catalog) Add(m *Movie) error {
	if !slugRE.MatchString(m.Slug) || reservedSlugs[m.Slug] {
		return fmt.Errorf("invalid or reserved slug %q", m.Slug)
	}
	if c.bySlug[m.Slug] != nil {
		return fmt.Errorf("duplicate slug %q", m.Slug)
	}
	if len(m.Renditions) == 0 {
		return fmt.Errorf("movie %q has no renditions", m.Slug)
	}
	sort.SliceStable(m.Renditions, func(i, j int) bool { return m.Renditions[i].Width < m.Renditions[j].Width })
	c.Movies = append(c.Movies, m)
	c.bySlug[m.Slug] = m
	return nil
}

// LoadCatalogFile adds every movie from a catalog JSON file. Rendition paths
// are relative to the catalog file's directory.
func (c *Catalog) LoadCatalogFile(path string, warn func(string)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cf catalogFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, e := range cf.Movies {
		m := &Movie{Slug: e.Slug, Title: e.Title, Year: e.Year, Credit: e.Credit, Private: e.Private, CaptionLines: e.CaptionLines}
		if m.Title == "" {
			m.Title = m.Slug
		}
		for _, rd := range e.Renditions {
			p := rd.File
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			r, w, err := OpenRendition(p, rd.Height)
			if err != nil {
				return fmt.Errorf("movie %q: %w", e.Slug, err)
			}
			if w != "" {
				warn(w)
			}
			m.Renditions = append(m.Renditions, r)
		}
		if err := c.Add(m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if cf.Default != "" && c.Default == "" {
		c.Default = cf.Default
	}
	return nil
}

// Validate checks catalog-wide invariants once everything is loaded.
func (c *Catalog) Validate() error {
	if len(c.Movies) == 0 {
		return errors.New("no movies loaded")
	}
	if c.Default == "" {
		for _, m := range c.Movies {
			if !m.Private {
				c.Default = m.Slug
				break
			}
		}
	}
	if c.Default != "" {
		m := c.Get(c.Default)
		if m == nil {
			return fmt.Errorf("default movie %q is not in the catalog", c.Default)
		}
		if m.Private {
			return fmt.Errorf("default movie %q must not be private", c.Default)
		}
	}
	return nil
}

// Close releases every rendition's file.
func (c *Catalog) Close() {
	for _, m := range c.Movies {
		for _, r := range m.Renditions {
			r.Close()
		}
	}
}
