package docs

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

// Origin is where the docs are read from: the public portal, never a URL a caller passes. The dev
// portal sits behind a sign-in, so dev servers read this one too.
const Origin = "https://docs.costfluent.com"

// indexPath is the one file a Source requests: every listed page in both languages.
const indexPath = "/llms-full.txt"

// Fetch bounds and cache timings. The file is about 1.7 MB today; the cap leaves it room to grow
// tenfold while a misbehaving origin cannot fill the process.
const (
	fetchTimeout = 20 * time.Second
	maxBytes     = 16 << 20
	freshFor     = time.Hour
	retryAfter   = time.Minute
)

// Config builds a Source. Its zero value reads Origin with its own transport and the wall clock.
type Config struct {
	// Origin is the docs origin, https://host with no path; tests point it at their own server.
	Origin    string
	Transport http.RoundTripper
	Now       func() time.Time
	// UserAgent names the server that reads the docs.
	UserAgent string
}

// Source holds one process's index of the docs: fetched on the first call that needs it, fresh for
// an hour, then revalidated with the validators the origin sent. A failed refresh keeps the index
// it has and tries again a minute later; with no index yet the failure is the caller's error.
type Source struct {
	origin    string
	client    *http.Client
	now       func() time.Time
	userAgent string

	mu           sync.Mutex
	index        *Index
	etag         string
	lastModified string
	freshUntil   time.Time
	retryAt      time.Time
	lastErr      error
}

// NewSource returns a Source for c.
func NewSource(c Config) *Source {
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	return &Source{
		origin: cmp.Or(c.Origin, Origin),
		client: &http.Client{
			Transport: transport,
			Timeout:   fetchTimeout,
			// A redirect is an error, never followed: the dev portal's sign-in answers with one, and
			// its page would parse as no docs, or a redirect could point the fetch anywhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:       now,
		userAgent: cmp.Or(c.UserAgent, "costfluent-mcp"),
	}
}

// Shared is the process's Source for Origin, so every server a process builds, one per request in
// the hosted mode, reads one index.
var Shared = sync.OnceValue(func() *Source { return NewSource(Config{}) })

// Origin is the origin s reads.
func (s *Source) Origin() string { return s.origin }

// UnavailableError is a docs index that could not be read. It wraps no transport error, so a caller
// that reports a failure to reach the Public API by its type never takes it for one.
type UnavailableError struct{ msg string }

func (e *UnavailableError) Error() string { return e.msg }

// Index returns the docs index, fetching or revalidating it when it is due. Concurrent callers
// wait for one fetch.
func (s *Source) Index(ctx context.Context) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.index != nil && now.Before(s.freshUntil) {
		return s.index, nil
	}
	if now.Before(s.retryAt) {
		if s.index != nil {
			return s.index, nil
		}
		return nil, s.lastErr
	}
	err := s.fetch(ctx)
	if err == nil {
		s.freshUntil = now.Add(freshFor)
		return s.index, nil
	}
	if ctx.Err() != nil {
		// The caller gave up; the origin did not fail, so the next call tries at once.
		if s.index != nil {
			return s.index, nil
		}
		return nil, ctx.Err()
	}
	s.retryAt = now.Add(retryAfter)
	s.lastErr = &UnavailableError{msg: fmt.Sprintf("Costfluent's docs at %s could not be read (%v); try again in a minute", s.origin, err)}
	if s.index != nil {
		return s.index, nil
	}
	return nil, s.lastErr
}

// fetch reads the index file, conditionally when s holds one, and replaces s.index on a new one.
func (s *Source) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+indexPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", s.userAgent)
	if s.index != nil {
		if s.etag != "" {
			req.Header.Set("If-None-Match", s.etag)
		}
		if s.lastModified != "" {
			req.Header.Set("If-Modified-Since", s.lastModified)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// The request's own error names the URL, which is the public origin's file.
		return fmt.Errorf("%s", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified && s.index != nil:
		return nil
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s answered %s", indexPath, resp.Status)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/plain" {
		return fmt.Errorf("%s answered %q, not text/plain", indexPath, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("reading %s: %s", indexPath, err.Error())
	}
	if len(body) > maxBytes {
		return fmt.Errorf("%s is larger than %d MiB", indexPath, maxBytes>>20)
	}
	docs, err := Parse(string(body))
	if err != nil {
		return fmt.Errorf("%s is malformed: %w", indexPath, err)
	}
	s.index = NewIndex(s.origin, docs)
	s.etag = resp.Header.Get("ETag")
	s.lastModified = resp.Header.Get("Last-Modified")
	return nil
}
