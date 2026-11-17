package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, keys ...string) (*server, *httptest.Server) {
	t.Helper()
	pub := writeTemp(t, "pub.txt", "1\nhello\n1\nworld\n")
	priv := writeTemp(t, "priv.txt", "1\nsecret\n")
	cat := newCatalog()
	for _, m := range []struct {
		slug, path string
		private    bool
	}{{"demo", pub, false}, {"hidden", priv, true}} {
		r, _, err := OpenRendition(m.path, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.Add(&Movie{Slug: m.slug, Title: m.slug, Private: m.private, Renditions: []*Rendition{r}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := parseConfig(nil)
	cfg.keys = keys
	s := newServer(cat, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.tick = time.Millisecond // play fast
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); cat.Close() })
	return s, ts
}

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestCurlGetsMovie(t *testing.T) {
	_, ts := testServer(t)
	res, body := get(t, ts.URL+"/", map[string]string{"User-Agent": "curl/8.9", "Accept": "*/*"})
	if res.StatusCode != 200 || !strings.Contains(body, "hello") || !strings.Contains(body, "world") {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if !strings.Contains(body, escHide) || !strings.HasSuffix(body, escShow+"\n") {
		t.Fatal("cursor should be hidden during playback and restored at the end")
	}
	if res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("missing X-Accel-Buffering: no")
	}
}

func TestBrowserGetsWebPage(t *testing.T) {
	_, ts := testServer(t)
	res, body := get(t, ts.URL+"/", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if !strings.Contains(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "<!doctype html>") {
		t.Fatalf("got %s", res.Header.Get("Content-Type"))
	}
	if strings.Contains(body, "{{HOST}}") {
		t.Fatal("host placeholder not replaced")
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP")
	}
}

func TestPlainFormatForWebPlayer(t *testing.T) {
	_, ts := testServer(t)
	_, body := get(t, ts.URL+"/demo?format=plain", nil)
	if strings.Contains(body, "\x1b") {
		t.Fatal("plain format must not contain escape codes")
	}
	frames := strings.Split(strings.TrimSuffix(body, "\f"), "\f")
	if len(frames) != 2 || !strings.HasPrefix(frames[1], "@66 demo\nworld") {
		t.Fatalf("frames %q", frames)
	}
}

func TestPrivateMovieNeedsKey(t *testing.T) {
	_, ts := testServer(t, "s3cret")
	res, _ := get(t, ts.URL+"/hidden", nil)
	if res.StatusCode != 404 {
		t.Fatalf("no key: %d", res.StatusCode)
	}
	_, menu := get(t, ts.URL+"/ls", nil)
	if strings.Contains(menu, "hidden") {
		t.Fatal("private title leaked in the menu")
	}
	res, body := get(t, ts.URL+"/hidden", map[string]string{"Authorization": "Bearer s3cret"})
	if res.StatusCode != 200 || !strings.Contains(body, "secret") {
		t.Fatalf("with key: %d", res.StatusCode)
	}
	res, _ = get(t, ts.URL+"/hidden?key=wrong", nil)
	if res.StatusCode != 404 {
		t.Fatal("wrong key accepted")
	}
}

func TestSeekStartsLater(t *testing.T) {
	_, ts := testServer(t)
	_, body := get(t, ts.URL+"/demo?format=plain&t=0:00:00", nil)
	if !strings.Contains(body, "hello") {
		t.Fatal("t=0 should include the first frame")
	}
	if d, ok := parseStart("1:02:03"); !ok || d != time.Hour+2*time.Minute+3*time.Second {
		t.Fatal("parseStart h:m:s")
	}
	if d, ok := parseStart("1h2m"); !ok || d != 62*time.Minute {
		t.Fatal("parseStart Go duration")
	}
	if _, ok := parseStart("soon"); ok {
		t.Fatal("parseStart accepted garbage")
	}
}

func TestPerIPLimit(t *testing.T) {
	s, _ := testServer(t)
	s.cfg.maxPerIP = 1
	if !s.acquire("1.2.3.4") {
		t.Fatal("first acquire failed")
	}
	if s.acquire("1.2.3.4") {
		t.Fatal("second stream from the same IP should be refused")
	}
	if !s.acquire("5.6.7.8") {
		t.Fatal("other IPs are unaffected")
	}
	s.release("1.2.3.4")
	if !s.acquire("1.2.3.4") {
		t.Fatal("slot not released")
	}
}

func TestClientDisconnectStopsStream(t *testing.T) {
	// Problem 4: abandoned viewers used to keep a goroutine sleeping for the
	// rest of the movie.
	s, ts := testServer(t)
	s.tick = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/demo", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	res.Body.Read(buf)
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.perIP)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stream still running after the client went away")
}

func TestLiveScheduleIsSharedClock(t *testing.T) {
	s, _ := testServer(t)
	s.now = func() time.Time { return s.cfg.liveEpoch.Add(Tick) } // one tick in
	req := httptest.NewRequest("GET", "/live", nil)
	items := s.liveSchedule(req)
	if len(items) < 1 || items[0].movie.Slug != "demo" || items[0].startTick != 1 {
		t.Fatalf("items %+v", items)
	}
}

func TestAnonIP(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.77":           "203.0.113.0/24",
		"2001:db8:abcd:1234::99": "2001:db8:abcd::/48",
		"::ffff:198.51.100.9":    "198.51.100.0/24",
		"nonsense":               "unknown",
	} {
		if got := anonIP(in); got != want {
			t.Errorf("anonIP(%q)=%q want %q", in, got, want)
		}
	}
}

func TestUnknownMovieShowsMenu(t *testing.T) {
	_, ts := testServer(t)
	res, body := get(t, ts.URL+"/nope", nil)
	if res.StatusCode != 404 || !strings.Contains(body, "NOW SHOWING") {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
}
