package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	cat   *Catalog
	cfg   *config
	log   *slog.Logger
	tick  time.Duration // real time per movie tick; tests shrink it
	now   func() time.Time
	slots chan struct{}

	mu    sync.Mutex
	perIP map[string]int
}

func newServer(cat *Catalog, cfg *config, log *slog.Logger) *server {
	return &server{
		cat:   cat,
		cfg:   cfg,
		log:   log,
		tick:  Tick,
		now:   time.Now,
		slots: make(chan struct{}, max(cfg.maxStreams, 1)),
		perIP: map[string]int{},
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16" rx="2"/><text x="8" y="12" font-family="monospace" font-size="10" fill="white" text-anchor="middle">&gt;_</text></svg>`)
	})
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "User-agent: *\nAllow: /$\nDisallow: /\n") })
	mux.HandleFunc("GET /api/movies", s.handleAPIMovies)
	mux.HandleFunc("GET /privacy", s.page("web/privacy.html"))
	mux.HandleFunc("GET /terms", s.page("web/terms.html"))
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /ls", s.handleMenu)
	mux.HandleFunc("GET /help", s.handleMenu)
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("GET /{slug}", s.handleMovie)
	return s.middleware(mux)
}

// middleware adds security headers and upgrades browsers (never curl) to HTTPS.
func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Plain-HTTP terminal viewers are a feature (`curl tv.bodegga.net`
		// must work without -L), so only browsers are redirected.
		if s.cfg.trustProxy && isLoopback(r.RemoteAddr) && r.Header.Get("X-Forwarded-Proto") == "http" && wantsHTML(r) {
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wantsHTML is true for web browsers. curl, wget, HTTPie and friends send
// Accept: */* (or nothing) and get the terminal experience.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// anonIP truncates an address (IPv4 to /24, IPv6 to /48) so logs never hold
// a full client IP. The full address only lives in memory, in the
// per-IP stream counter, for as long as the stream is open.
func anonIP(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "unknown"
	}
	bits := 24
	if a.Is6() && !a.Is4In6() {
		bits = 48
	}
	p, err := a.Unmap().Prefix(bits)
	if err != nil {
		return "unknown"
	}
	return p.String()
}

func (s *server) clientIP(r *http.Request) string {
	if s.cfg.trustProxy && isLoopback(r.RemoteAddr) {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authorized reports whether the request carries a valid private-screening key
// (?key=… or Authorization: Bearer …).
func (s *server) authorized(r *http.Request) bool {
	got := r.URL.Query().Get("key")
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		got = strings.TrimPrefix(a, "Bearer ")
	}
	if got == "" {
		return false
	}
	ok := false
	for _, k := range s.cfg.keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *server) visible(r *http.Request) []*Movie {
	auth := s.authorized(r)
	var out []*Movie
	for _, m := range s.cat.Movies {
		if !m.Private || auth {
			out = append(out, m)
		}
	}
	return out
}

func (s *server) host(r *http.Request) string {
	if s.cfg.publicURL != "" {
		return strings.TrimPrefix(strings.TrimPrefix(s.cfg.publicURL, "https://"), "http://")
	}
	return r.Host
}

// ---- handlers ---------------------------------------------------------------

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		s.serveWeb(w, r)
		return
	}
	if s.cat.Default == "" {
		s.handleMenu(w, r)
		return
	}
	s.stream(w, r, []playItem{{movie: s.cat.Get(s.cat.Default)}}, false)
}

func (s *server) handleMovie(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(r.PathValue("slug"))
	m := s.cat.Get(slug)
	if m == nil || (m.Private && !s.authorized(r)) {
		// Private titles 404 exactly like missing ones, so their names don't leak.
		if wantsHTML(r) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "\n  No movie called %q is showing here.\n", slug)
		s.writeMenu(w, r)
		return
	}
	if wantsHTML(r) {
		http.Redirect(w, r, "/?watch="+m.Slug, http.StatusFound)
		return
	}
	s.stream(w, r, []playItem{{movie: m}}, false)
}

func (s *server) handleLive(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		http.Redirect(w, r, "/?watch=live", http.StatusFound)
		return
	}
	items := s.liveSchedule(r)
	if len(items) == 0 {
		http.Error(w, "nothing is on right now\n", http.StatusServiceUnavailable)
		return
	}
	s.stream(w, r, items, true)
}

// liveSchedule works out what the live channel is showing right now: the
// visible movies play back to back, forever, on a wall clock shared by
// every viewer.
func (s *server) liveSchedule(r *http.Request) []playItem {
	movies := s.visible(r)
	var total int64
	for _, m := range movies {
		total += m.Renditions[0].total
	}
	if total == 0 {
		return nil
	}
	pos := int64(s.now().Sub(s.cfg.liveEpoch)/Tick) % total
	if pos < 0 {
		pos += total
	}
	var items []playItem
	for i := 0; i < len(movies); i++ {
		m := movies[i]
		if pos < m.Renditions[0].total {
			items = append(items, playItem{movie: m, startTick: pos, live: true})
			for j := 1; j <= len(movies); j++ {
				items = append(items, playItem{movie: movies[(i+j)%len(movies)], live: true})
			}
			return items
		}
		pos -= m.Renditions[0].total
	}
	return nil
}

func (s *server) handleMenu(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	s.writeMenu(w, r)
}

func (s *server) writeMenu(w http.ResponseWriter, r *http.Request) {
	h := s.host(r)
	var b strings.Builder
	fmt.Fprintf(&b, "\n   _   ___  ___ ___ ___   _______   __\n")
	fmt.Fprintf(&b, "  /_\\ / __|/ __|_ _|_ _| |_   _\\ \\ / /\n")
	fmt.Fprintf(&b, " / _ \\\\__ \\ (__ | | | |    | |  \\ V /\n")
	fmt.Fprintf(&b, "/_/ \\_\\___/\\___|___|___|   |_|   \\_/   %s\n\n", h)
	fmt.Fprintf(&b, "  NOW SHOWING\n\n")
	for _, m := range s.visible(r) {
		title := m.Title
		if m.Year != "" {
			title += " (" + m.Year + ")"
		}
		tag := ""
		if m.Private {
			tag = "  [private]"
		}
		fmt.Fprintf(&b, "    %-38s %8s   curl %s/%s%s\n", title, fmtClock(m.Duration()), h, m.Slug, tag)
	}
	fmt.Fprintf(&b, "\n    %-38s %8s   curl %s/live\n", "LIVE channel (everyone sees the same frame)", "24/7", h)
	fmt.Fprintf(&b, `
  OPTIONS  (add after the movie name, e.g.  curl "%[1]s/starwars?t=5m&bar=0")
    t=1h2m | t=12:30    start part-way through
    cols=N&rows=N       centre the picture and pick the sharpest size that fits
                          bash/zsh:   curl -s "%[1]s/starwars?cols=$COLUMNS&rows=$LINES"
    style=ascii         classic text characters instead of HD colour pixels
    color=0             black and white
    bar=0               hide the progress bar
    loop                play forever

  TIP  add --compressed for HD:  curl --compressed -sN %[1]s/starwars  (uses ~6x less data)

  Windows PowerShell: type curl.exe (not curl).   Stop with Ctrl+C.
  Cursor gone after stopping? Type  tput cnorm  (macOS/Linux) or open a new tab.
  In a browser? Open https://%[1]s for the web player.

`, h)
	fmt.Fprint(w, b.String())
}

type apiMovie struct {
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Year         string `json:"year,omitempty"`
	Credit       string `json:"credit,omitempty"`
	Private      bool   `json:"private,omitempty"`
	Seconds      int    `json:"seconds"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	CaptionLines int    `json:"captionLines,omitempty"`
}

func (s *server) handleAPIMovies(w http.ResponseWriter, r *http.Request) {
	var out []apiMovie
	for _, m := range s.visible(r) {
		big := m.Renditions[len(m.Renditions)-1]
		out = append(out, apiMovie{
			Slug: m.Slug, Title: m.Title, Year: m.Year, Credit: m.Credit, Private: m.Private,
			Seconds: int(m.Duration() / time.Second), Width: big.Width, Height: big.Height,
			CaptionLines: m.CaptionLines,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"default": s.cat.Default, "movies": out})
}

// page serves a static HTML document (privacy policy, terms) to browsers and
// as plain HTML to anything else, which is fine for curl too.
func (s *server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.serveHTML(w, r, name) }
}

func (s *server) serveWeb(w http.ResponseWriter, r *http.Request) {
	s.serveHTML(w, r, "web/index.html")
}

func (s *server) serveHTML(w http.ResponseWriter, r *http.Request, name string) {
	page, err := webFS.ReadFile(name)
	if err != nil {
		http.Error(w, "missing page", http.StatusInternalServerError)
		return
	}
	page = bytes.ReplaceAll(page, []byte("{{HOST}}"), []byte(s.host(r)))
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Cache-Control", "no-cache")
	w.Write(page)
}

// ---- playback ---------------------------------------------------------------

type playItem struct {
	movie     *Movie
	startTick int64
	live      bool
}

type playOpts struct {
	start int64 // ticks
	cols  int
	rows  int
	bar   bool
	loop  bool
	plain bool   // web player: frames separated by \f
	color bool   // keep ANSI colour (terminal default on; web asks with color=1)
	style string // "hd", "ascii" or "" (movie default)
}

func parseOpts(r *http.Request) playOpts {
	q := r.URL.Query()
	o := playOpts{bar: true}
	if t := q.Get("t"); t != "" {
		if d, ok := parseStart(t); ok {
			o.start = int64(d / Tick)
		}
	}
	o.cols = clampInt(q.Get("cols"), 0, 1000)
	o.rows = clampInt(q.Get("rows"), 0, 500)
	if v := q.Get("bar"); v == "0" || v == "false" || v == "off" {
		o.bar = false
	}
	_, o.loop = q["loop"]
	o.plain = q.Get("format") == "plain"
	switch q.Get("color") {
	case "0", "false", "off", "no":
		o.color = false
	case "1", "true", "on", "yes":
		o.color = true
	default:
		o.color = !o.plain
	}
	switch s := q.Get("style"); s {
	case "hd", "ascii":
		o.style = s
	}
	if !o.color && o.style == "" {
		o.style = "ascii" // half-blocks are meaningless without colour
	}
	return o
}

func clampInt(s string, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo {
		return lo
	}
	return min(n, hi)
}

// parseStart accepts "90", "90s", "1h2m3s", "12:30" and "1:02:03".
func parseStart(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, true
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total time.Duration
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + time.Duration(n)
	}
	return total * time.Second, true
}

func (s *server) acquire(ip string) bool {
	select {
	case s.slots <- struct{}{}:
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perIP[ip] >= s.cfg.maxPerIP {
		<-s.slots
		return false
	}
	s.perIP[ip]++
	return true
}

func (s *server) release(ip string) {
	s.mu.Lock()
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
	s.mu.Unlock()
	<-s.slots
}

const (
	escHide     = "\x1b[?25l"
	escShow     = "\x1b[?25h"
	escClear    = "\x1b[2J"
	escHome     = "\x1b[H"
	escEOL      = "\x1b[K"
	escEOS      = "\x1b[J"
	escReset    = "\x1b[0m"
	escSyncOn   = "\x1b[?2026h" // synchronized output: no tearing on modern terminals
	escSyncOff  = "\x1b[?2026l" // (older terminals ignore it)
	writeBufLen = 32 << 10
)

func (s *server) stream(w http.ResponseWriter, r *http.Request, items []playItem, live bool) {
	ip := s.clientIP(r)
	if !s.acquire(ip) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "\n  The theatre is full right now. Try again in a minute.\n", http.StatusTooManyRequests)
		return
	}
	defer s.release(ip)

	o := parseOpts(r)
	if !live && o.start > 0 {
		items[0].startTick = o.start
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer
	h.Add("Vary", "Accept-Encoding")
	// Browsers always accept gzip and `curl --compressed` does too. HD colour
	// frames shrink ~8x, which matters on phones. Plain curl gets raw bytes.
	var gz *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		gz, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
	}
	w.WriteHeader(http.StatusOK)

	started := time.Now()
	slug := items[0].movie.Slug
	if live {
		slug = "live"
	}
	s.log.Info("stream start", "movie", slug, "net", anonIP(ip), "plain", o.plain, "cols", o.cols, "rows", o.rows)
	err := s.play(r.Context(), w, gz, items, o)
	if gz != nil {
		gz.Close()
	}
	s.log.Info("stream end", "movie", slug, "net", anonIP(ip), "watched", time.Since(started).Round(time.Second), "err", err)
}

func (s *server) play(ctx context.Context, w http.ResponseWriter, gz *gzip.Writer, items []playItem, o playOpts) error {
	rc := http.NewResponseController(w)
	var dst io.Writer = w
	if gz != nil {
		dst = gz
	}
	bw := bufio.NewWriterSize(dst, writeBufLen)
	var frame bytes.Buffer
	next := time.Now()
	first := true

	flush := func() error {
		_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.frameWriteT))
		if err := bw.Flush(); err != nil {
			return err
		}
		if gz != nil {
			if err := gz.Flush(); err != nil {
				return err
			}
		}
		return rc.Flush()
	}
	defer func() {
		if !o.plain {
			bw.WriteString(escReset + escShow + "\n")
			flush()
		}
	}()

	for {
		for _, it := range items {
			reserve := 0
			if o.bar {
				reserve = 1
			}
			rend := it.movie.Pick(o.cols, o.rows, reserve, o.style)
			for i := rend.FrameAt(it.startTick); i < rend.Frames(); i++ {
				lines, dur, err := rend.ReadFrame(i)
				if err != nil {
					return err
				}
				if rend.Color && !o.color {
					lines = stripANSI(lines)
				}
				frame.Reset()
				elapsed := rend.StartTick(i)
				if o.plain {
					// Header: position in ms, movie, and the rendition actually
					// sent, so the web player knows how to draw it.
					fmt.Fprintf(&frame, "@%d %s %s %d %d\n", elapsed*int64(Tick/time.Millisecond), it.movie.Slug, rend.Style, rend.Width, rend.Height)
					for _, l := range lines {
						frame.WriteString(l)
						frame.WriteByte('\n')
					}
					frame.WriteByte('\f')
				} else {
					s.renderTerminal(&frame, it, rend, lines, elapsed, o, first)
				}
				first = false
				bw.Write(frame.Bytes())
				if err := flush(); err != nil {
					return err
				}
				next = next.Add(time.Duration(dur) * s.tick / Tick)
				if wait := time.Until(next); wait > 0 {
					t := time.NewTimer(wait)
					select {
					case <-ctx.Done():
						t.Stop()
						return ctx.Err()
					case <-t.C:
					}
				} else if ctx.Err() != nil {
					return ctx.Err()
				} else if wait < -2*time.Second {
					next = time.Now() // fell far behind (slow link): don't fast-forward
				}
			}
		}
		if !o.loop && !items[0].live {
			return nil
		}
		for i := range items {
			items[i].startTick = 0
		}
	}
}

func (s *server) renderTerminal(b *bytes.Buffer, it playItem, rend *Rendition, lines []string, elapsed int64, o playOpts, first bool) {
	width := max(rend.Width, 40)
	padL, padT := 0, 0
	used := rend.Height
	if o.bar {
		used++
	}
	if o.cols > width {
		padL = (o.cols - width) / 2
	}
	if o.rows > used {
		padT = (o.rows - used) / 2
	}
	pad := strings.Repeat(" ", padL)

	b.WriteString(escSyncOn)
	if first {
		b.WriteString(escHide + escClear)
	}
	b.WriteString(escHome)
	for i := 0; i < padT; i++ {
		b.WriteString(escEOL + "\n")
	}
	for i, l := range lines {
		b.WriteString(pad)
		b.WriteString(l)
		b.WriteString(escReset + escEOL)
		if i < len(lines)-1 || o.bar {
			b.WriteByte('\n')
		}
	}
	if o.bar {
		b.WriteString(pad)
		b.WriteString(statusBar(it, elapsed, rend.total, width))
		b.WriteString(escEOL)
	}
	b.WriteString(escEOS + escSyncOff)
}

// statusBar draws e.g. "LIVE  Star Wars  00:03:12 / 00:18:50 [####......]"
// in plain ASCII so it renders on every console, including old Windows ones.
func statusBar(it playItem, elapsed, total int64, width int) string {
	label := it.movie.Title
	if it.live {
		label = "LIVE  " + label
	}
	clock := fmt.Sprintf("  %s / %s ", fmtClock(time.Duration(elapsed)*Tick), fmtClock(time.Duration(total)*Tick))
	barW := width - len([]rune(label)) - len(clock) - 2
	if barW < 10 {
		maxLabel := max(width-len(clock)-12, 4)
		if r := []rune(label); len(r) > maxLabel {
			label = string(r[:maxLabel-1]) + "~"
		}
		barW = max(width-len([]rune(label))-len(clock)-2, 0)
	}
	fill := 0
	if total > 0 {
		fill = int(int64(barW) * elapsed / total)
	}
	return "\x1b[2m" + label + clock + "[" + strings.Repeat("#", fill) + strings.Repeat(".", barW-fill) + "]" + escReset
}

func fmtClock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}
