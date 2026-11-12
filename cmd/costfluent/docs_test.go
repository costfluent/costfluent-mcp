package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/costfluent/costfluent-mcp/internal/docs"
)

// docsPortal serves the docs package's cut of the published llms-full.txt, or status when it is not
// 200, and counts the requests it is sent.
func docsPortal(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	body, err := os.ReadFile("../../internal/docs/testdata/llms-full.txt")
	if err != nil {
		t.Fatal(err)
	}
	requests := &atomic.Int32{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if status != http.StatusOK || r.URL.Path != "/llms-full.txt" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

// invokeDocs runs the CLI as invoke does, its docs read from portal.
func invokeDocs(t *testing.T, portal *httptest.Server, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &cli{
		args: args, getenv: func(k string) string { return env[k] },
		stdout: &stdout, stderr: &stderr,
		docs: docs.NewSource(docs.Config{Origin: portal.URL, Transport: portal.Client().Transport}),
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestDocsCommandsNeedNoToken(t *testing.T) {
	portal, requests := docsPortal(t, http.StatusOK)
	res := invokeDocs(t, portal, map[string]string{}, "search-docs", "--query", "how do I manage budgets with Terraform?", "--limit", "3")
	if res.code != exitOK {
		t.Fatalf("search-docs without a token: exit %d, %s", res.code, res.stderr)
	}
	var found struct {
		Results []docs.Hit `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &found); err != nil {
		t.Fatalf("stdout is not the result: %v\n%s", err, res.stdout)
	}
	if len(found.Results) == 0 || len(found.Results) > 3 || found.Results[0].URL != portal.URL+"/developers/terraform#manage-objects" {
		t.Fatalf("results %+v", found.Results)
	}

	res = invokeDocs(t, portal, map[string]string{}, "get-doc-page", "--url", found.Results[0].URL)
	if res.code != exitOK || !strings.Contains(res.stdout, `"markdown": "## Manage objects`) {
		t.Fatalf("get-doc-page: exit %d, %.200s %s", res.code, res.stdout, res.stderr)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("two runs made %d requests to the portal, one each", n)
	}
}

func TestADocsFailureExits1AndAGatedCommandStillNeedsAToken(t *testing.T) {
	portal, _ := docsPortal(t, http.StatusServiceUnavailable)
	res := invokeDocs(t, portal, map[string]string{}, "search-docs", "--query", "terraform")
	if res.code != exitFailed || !strings.Contains(res.stderr, "could not be read") || !strings.Contains(res.stderr, portal.URL) {
		t.Errorf("a portal answering 503: exit %d, %q; want 1 naming the portal", res.code, res.stderr)
	}
	ok, requests := docsPortal(t, http.StatusOK)
	res = invokeDocs(t, ok, map[string]string{}, "get-doc-page", "--url", "https://evil.example/developers/terraform")
	if res.code != exitFailed || !strings.Contains(res.stderr, "reads only Costfluent's docs") || requests.Load() != 1 {
		t.Errorf("get-doc-page on another host: exit %d, %q, %d requests", res.code, res.stderr, requests.Load())
	}
	if res := invokeDocs(t, portal, map[string]string{}, "list-budgets"); res.code != exitAccess {
		t.Errorf("list-budgets without a token: exit %d, %q; want 3", res.code, res.stderr)
	}
}

// A docs command the user interrupts stops with exit 1 and says so, and reads nothing more.
func TestAnInterruptedDocsCommandExits1(t *testing.T) {
	portal, _ := docsPortal(t, http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, &cli{
		args: []string{"search-docs", "--query", "terraform"}, getenv: func(string) string { return "" },
		stdout: &stdout, stderr: &stderr,
		docs: docs.NewSource(docs.Config{Origin: portal.URL, Transport: portal.Client().Transport}),
	})
	if code != exitFailed || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and nothing printed", code, stdout.String(), stderr.String())
	}
}
