package docs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is the Source's injected time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// origin is a TLS docs origin whose answer to /llms-full.txt a test sets, recording each request.
type origin struct {
	*httptest.Server
	mu       sync.Mutex
	handler  http.HandlerFunc
	requests []*http.Request
	other    atomic.Int32 // requests to any other path
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	o := &origin{}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != indexPath {
			o.other.Add(1)
			http.NotFound(w, r)
			return
		}
		o.mu.Lock()
		o.requests = append(o.requests, r.Clone(context.Background()))
		h := o.handler
		o.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) answer(h http.HandlerFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.handler = h
}

func (o *origin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.requests)
}

func (o *origin) last() *http.Request {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests[len(o.requests)-1]
}

// serve answers with body as text/plain under the given validators.
func serve(body, etag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if etag != "" && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if etag != "" {
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", "Mon, 09 Nov 2026 07:30:41 GMT")
		}
		_, _ = w.Write([]byte(body))
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func newTestSource(o *origin, c *clock) *Source {
	return NewSource(Config{Origin: o.URL, Transport: o.Client().Transport, Now: c.Now})
}

var page = framed([3]string{"Terraform", "/developers/terraform", "# Terraform\n\nManage budgets.\n"})

func TestSourceFetchesOnceAndRevalidatesAfterAnHour(t *testing.T) {
	o := newOrigin(t)
	o.answer(serve(page, `W/"v1"`))
	c := &clock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	s := newTestSource(o, c)
	ctx := context.Background()

	first, err := s.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages() != 1 || o.count() != 1 {
		t.Fatalf("pages %d, requests %d", first.Pages(), o.count())
	}
	req := o.last()
	if req.Header.Get("If-None-Match") != "" || req.Header.Get("Accept") != "text/plain" || req.Header.Get("User-Agent") != "costfluent-mcp" {
		t.Errorf("first request headers %v", req.Header)
	}

	c.advance(59 * time.Minute)
	if ix, _ := s.Index(ctx); ix != first || o.count() != 1 {
		t.Fatalf("within the hour: requests %d", o.count())
	}

	c.advance(2 * time.Minute)
	if ix, err := s.Index(ctx); err != nil || ix != first || o.count() != 2 {
		t.Fatalf("after 304: %v, same index %v, requests %d", err, ix == first, o.count())
	}
	req = o.last()
	if req.Header.Get("If-None-Match") != `W/"v1"` || req.Header.Get("If-Modified-Since") != "Mon, 09 Nov 2026 07:30:41 GMT" {
		t.Errorf("revalidation headers %v", req.Header)
	}
	c.advance(30 * time.Minute)
	if _, _ = s.Index(ctx); o.count() != 2 {
		t.Errorf("a 304 did not keep the index fresh for another hour: requests %d", o.count())
	}

	o.answer(serve(framed([3]string{"One", "/one", "# One\n"}, [3]string{"Two", "/two", "# Two\n"}), `W/"v2"`))
	c.advance(31 * time.Minute)
	ix, err := s.Index(ctx)
	if err != nil || ix == first || ix.Pages() != 2 {
		t.Fatalf("a changed file: %v, pages %d", err, ix.Pages())
	}
}

func TestSourceRefusesWhatIsNotTheDocsFile(t *testing.T) {
	gzipped := func(n int) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte(page))
		_, _ = zw.Write(bytes.Repeat([]byte("x"), n))
		_ = zw.Close()
		return b.Bytes()
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"404", status(http.StatusNotFound), "answered 404 Not Found"},
		{"500", status(http.StatusInternalServerError), "answered 500 Internal Server Error"},
		{"304 with no index", status(http.StatusNotModified), "answered 304 Not Modified"},
		{"html", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(page))
		}, `answered "text/html; charset=utf-8", not text/plain`},
		{"malformed", serve("# Costfluent Docs\n\nNothing here.\n", ""), "is malformed"},
		{"oversized", serve(page+strings.Repeat("x", maxBytes), ""), "larger than 16 MiB"},
		{"gzip past the cap", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gzipped(maxBytes))
		}, "larger than 16 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrigin(t)
			o.answer(tc.handler)
			_, err := newTestSource(o, &clock{now: time.Now()}).Index(context.Background())
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), o.URL) {
				t.Fatalf("Index = %v, want an UnavailableError naming %s and %q", err, o.URL, tc.want)
			}
		})
	}
}

func TestSourceReadsAGzippedFileWithinTheCap(t *testing.T) {
	o := newOrigin(t)
	o.answer(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("Accept-Encoding %q", r.Header.Get("Accept-Encoding"))
		}
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte(page))
		_ = zw.Close()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(b.Bytes())
	})
	if ix, err := newTestSource(o, &clock{now: time.Now()}).Index(context.Background()); err != nil || ix.Pages() != 1 {
		t.Fatalf("Index = %v, %v", ix, err)
	}
}

func TestSourceFollowsNoRedirect(t *testing.T) {
	o := newOrigin(t)
	o.answer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn-cgi/access/login", http.StatusFound)
	})
	_, err := newTestSource(o, &clock{now: time.Now()}).Index(context.Background())
	if err == nil || !strings.Contains(err.Error(), "answered 302 Found") {
		t.Fatalf("Index = %v, want the redirect refused", err)
	}
	if n := o.other.Load(); n != 0 {
		t.Errorf("the redirect's target was requested %d times", n)
	}
}

func TestSourceTimesOutAndWrapsNoTransportError(t *testing.T) {
	o := newOrigin(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// The origin holds the request until cleanup, never until the request's context ends: the
	// client's timeout closes the TLS connection with a close_notify alert first, which ends that
	// context, and the empty 200 the handler would then answer can still reach the transport.
	o.answer(func(http.ResponseWriter, *http.Request) { <-release })
	s := newTestSource(o, &clock{now: time.Now()})
	s.client.Timeout = 50 * time.Millisecond
	_, err := s.Index(context.Background())
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("Index = %v, want a timeout", err)
	}
	// The hosted server reports any *url.Error as an unreachable Public API, so this must not be one.
	var transport *url.Error
	if errors.As(err, &transport) {
		t.Errorf("the docs error wraps %T", transport)
	}
}

func TestSourceKeepsAStaleIndexAndRetriesAfterAMinute(t *testing.T) {
	o := newOrigin(t)
	o.answer(serve(page, `W/"v1"`))
	c := &clock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	s := newTestSource(o, c)
	ctx := context.Background()
	first, err := s.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}

	o.answer(status(http.StatusBadGateway))
	c.advance(time.Hour)
	if ix, err := s.Index(ctx); err != nil || ix != first || o.count() != 2 {
		t.Fatalf("a failed refresh: %v, kept %v, requests %d", err, ix == first, o.count())
	}
	c.advance(59 * time.Second)
	if ix, err := s.Index(ctx); err != nil || ix != first || o.count() != 2 {
		t.Fatalf("within the minute: %v, requests %d", err, o.count())
	}
	o.answer(serve(page, `W/"v1"`))
	c.advance(2 * time.Second)
	if ix, err := s.Index(ctx); err != nil || ix != first || o.count() != 3 {
		t.Fatalf("after the minute: %v, requests %d", err, o.count())
	}
}

func TestSourceWithNoIndexRetriesOnlyAfterAMinute(t *testing.T) {
	o := newOrigin(t)
	o.answer(status(http.StatusServiceUnavailable))
	c := &clock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	s := newTestSource(o, c)
	ctx := context.Background()
	_, first := s.Index(ctx)
	c.advance(30 * time.Second)
	_, again := s.Index(ctx)
	if first == nil || again == nil || again.Error() != first.Error() || o.count() != 1 {
		t.Fatalf("errors %v, %v; requests %d", first, again, o.count())
	}
	o.answer(serve(page, ""))
	c.advance(31 * time.Second)
	if ix, err := s.Index(ctx); err != nil || ix.Pages() != 1 || o.count() != 2 {
		t.Fatalf("after the minute: %v, requests %d", err, o.count())
	}
}

func TestSourceDoesNotHoldACancelledCallAgainstTheOrigin(t *testing.T) {
	o := newOrigin(t)
	o.answer(serve(page, ""))
	s := newTestSource(o, &clock{now: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Index(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Index = %v, want the caller's cancellation", err)
	}
	if ix, err := s.Index(context.Background()); err != nil || ix.Pages() != 1 {
		t.Fatalf("the next call: %v", err)
	}
}

func TestSourceFetchesOnceForConcurrentFirstCalls(t *testing.T) {
	o := newOrigin(t)
	o.answer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		serve(page, "")(w, r)
	})
	s := newTestSource(o, &clock{now: time.Now()})
	var wg sync.WaitGroup
	start := make(chan struct{})
	indexes := make([]*Index, 10)
	for i := range indexes {
		wg.Go(func() {
			<-start
			ix, err := s.Index(context.Background())
			if err != nil {
				t.Error(err)
			}
			indexes[i] = ix
		})
	}
	close(start)
	wg.Wait()
	if o.count() != 1 {
		t.Errorf("ten first calls made %d requests", o.count())
	}
	for _, ix := range indexes {
		if ix != indexes[0] {
			t.Error("the calls returned different indexes")
		}
	}
}

func TestSourceSendsNoCredential(t *testing.T) {
	o := newOrigin(t)
	o.answer(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "CF_Authorization", Value: "x"})
		serve(page, `W/"v1"`)(w, r)
	})
	c := &clock{now: time.Now()}
	s := newTestSource(o, c)
	for range 2 {
		if _, err := s.Index(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.advance(2 * time.Hour)
	}
	for _, r := range o.requests {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("a request carried a credential: %v", r.Header)
		}
	}
	if o.count() != 2 {
		t.Errorf("requests %d", o.count())
	}
}

func TestSharedReadsThePublicOrigin(t *testing.T) {
	first, second := Shared(), Shared()
	if first != second || first.Origin() != "https://docs.costfluent.com" {
		t.Fatalf("Shared() is %p then %p, origin %s", first, second, first.Origin())
	}
}
