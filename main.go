// Command ascii-tv streams ASCII-art movies to terminals over plain HTTP:
//
//	curl -N tv.bodegga.net
//
// and to browsers through a small built-in web player.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type config struct {
	addr        string
	catalogs    stringList
	movie       string
	height      int
	title       string
	publicURL   string
	maxStreams  int
	maxPerIP    int
	trustProxy  bool
	keys        []string
	liveEpoch   time.Time
	frameWriteT time.Duration
}

func parseConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("ascii-tv", flag.ContinueOnError)
	c := &config{}
	fs.StringVar(&c.addr, "addr", "", "TCP address to listen on (default :$PORT, else :8080)")
	fs.Var(&c.catalogs, "catalog", "catalog JSON file; repeat to merge several (default resources/catalog.json)")
	fs.StringVar(&c.movie, "movie", "", "serve a single movie file instead of a catalog")
	fs.IntVar(&c.height, "height", 13, "artwork lines per frame for -movie")
	fs.StringVar(&c.title, "title", "", "title for -movie (default: file name)")
	fs.StringVar(&c.publicURL, "public-url", "", "URL shown in help text (default: from the request's Host)")
	fs.IntVar(&c.maxStreams, "max-streams", 200, "maximum concurrent streams")
	fs.IntVar(&c.maxPerIP, "max-per-ip", 4, "maximum concurrent streams per client IP")
	fs.BoolVar(&c.trustProxy, "trust-proxy", false, "trust X-Real-IP / X-Forwarded-Proto from a loopback reverse proxy")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			c.addr = ":" + p
		} else {
			c.addr = ":8080"
		}
	}
	if c.movie == "" && len(c.catalogs) == 0 {
		c.catalogs = stringList{"resources/catalog.json"}
	}
	for _, k := range strings.Split(os.Getenv("ASCII_TV_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.keys = append(c.keys, k)
		}
	}
	// The live channel's schedule is anchored to a fixed instant so every
	// viewer (and every server restart) agrees on what is "on" right now.
	c.liveEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.frameWriteT = 30 * time.Second
	return c, nil
}

func loadCatalog(c *config, log *slog.Logger) (*Catalog, error) {
	cat := newCatalog()
	warn := func(s string) { log.Warn(s) }
	if c.movie != "" {
		r, w, err := OpenRendition(c.movie, c.height)
		if err != nil {
			return nil, err
		}
		if w != "" {
			warn(w)
		}
		slug := strings.ToLower(strings.TrimSuffix(filepath.Base(c.movie), filepath.Ext(c.movie)))
		slug = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
				return r
			}
			return '-'
		}, slug)
		title := c.title
		if title == "" {
			title = slug
		}
		if err := cat.Add(&Movie{Slug: slug, Title: title, Renditions: []*Rendition{r}}); err != nil {
			return nil, err
		}
	}
	for _, p := range c.catalogs {
		if err := cat.LoadCatalogFile(p, warn); err != nil {
			return nil, err
		}
	}
	return cat, cat.Validate()
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	cat, err := loadCatalog(cfg, log)
	if err != nil {
		log.Error("cannot load movies", "err", err)
		os.Exit(1)
	}
	defer cat.Close()
	for _, m := range cat.Movies {
		for _, r := range m.Renditions {
			log.Info("loaded", "movie", m.Slug, "private", m.Private, "frames", r.Frames(),
				"size", fmt.Sprintf("%dx%d", r.Width, r.Height), "runtime", m.Duration().Round(time.Second))
		}
	}

	// Cancelling baseCtx ends every running stream, so shutdown doesn't wait
	// for a two-hour movie to finish.
	baseCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	srv := newServer(cat, cfg, log)
	hs := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		// No WriteTimeout: streams are long. Each frame write gets its own
		// deadline instead (see play), which drops stuck clients.
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "movies", len(cat.Movies), "default", cat.Default)
		errc <- hs.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	case <-sigCtx.Done():
		log.Info("shutting down")
		cancelStreams()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}
}
