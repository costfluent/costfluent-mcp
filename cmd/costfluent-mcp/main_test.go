package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveEnv makes the test binary run main instead of the tests, so a test can start the server as
// a real process over stdio, the way an MCP client does.
const serveEnv = "COSTFLUENT_MCP_TEST_SERVE"

const secret = "cfk_ws_scenario-secret-never-logged"

func TestMain(m *testing.M) {
	if os.Getenv(serveEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// syncBuffer is a bytes.Buffer the child's stderr copier and the test read without a race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// narrowSecret is a token carrying no capability any tool needs.
const narrowSecret = "cfk_ws_narrow-secret-never-logged"

// fakeAPI serves GET /v1/me for a workspace token carrying viewBudgets, and its budgets, and for
// narrowSecret, a token carrying only viewProviders.
func fakeAPI(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer "+narrowSecret && r.URL.Path == "/v1/me" {
			_, _ = io.WriteString(w, `{"organizationId":"org_1","workspaceId":"ws_own","tokenType":"workspace","tokenName":"providers","capabilities":["viewProviders"]}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"title":"Authentication Error","status":401,"code":"unauthorized","retryable":false}`)
			return
		}
		switch r.URL.Path {
		case "/v1/me":
			_, _ = io.WriteString(w, `{"organizationId":"org_1","workspaceId":"ws_own","tokenType":"workspace","tokenName":"assistant","capabilities":["viewBudgets"]}`)
		case "/v1/budgets":
			_, _ = io.WriteString(w, `{"links":{"self":"/v1/budgets"},"data":[{"id":"bud_1","name":"Production","amount":"100.00000000","currency":"USD","state":"onTrack"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScenarioServesOverStdio(t *testing.T) {
	api := fakeAPI(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), serveEnv+"=1", "COSTFLUENT_API_TOKEN="+secret, "COSTFLUENT_BASE_URL="+api.URL)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "scenario", Version: "test"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting: %v\n%s", err, stderr)
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	// The docs tools, which need no capability, come with it.
	if !slices.Equal(names, []string{"get_doc_page", "list_budgets", "search_docs"}) {
		t.Errorf("tools %v, want list_budgets and the docs tools for a token carrying viewBudgets", names)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_budgets"})
	if err != nil || res.IsError {
		t.Fatalf("list_budgets: %v %+v", err, res)
	}
	body, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(body), `"name":"Production"`) {
		t.Errorf("list_budgets returned %s", body)
	}

	if err := cs.Close(); err != nil {
		t.Errorf("the server did not stop cleanly when its client closed: %v\n%s", err, stderr)
	}
	log := stderr.String()
	if !strings.Contains(log, "serving") || !strings.Contains(log, "tools=list_budgets") {
		t.Errorf("the server did not log what it serves:\n%s", log)
	}
	if strings.Contains(log, secret) {
		t.Errorf("the server logged its token:\n%s", log)
	}
}

func TestRunRefusesAMissingTokenOrABadAPI(t *testing.T) {
	api := fakeAPI(t)
	for name, env := range map[string]map[string]string{
		"no token":           {},
		"plain http":         {"COSTFLUENT_API_TOKEN": secret, "COSTFLUENT_BASE_URL": "http://api.costfluent.com"},
		"refused token":      {"COSTFLUENT_API_TOKEN": "cfk_ws_wrong", "COSTFLUENT_BASE_URL": api.URL},
		"no read capability": {"COSTFLUENT_API_TOKEN": narrowSecret, "COSTFLUENT_BASE_URL": api.URL},
	} {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&log, nil))
			err := run(context.Background(), func(k string) string { return env[k] }, logger, nil)
			if err == nil {
				t.Fatal("started")
			}
			if strings.Contains(err.Error()+log.String(), secret) || strings.Contains(err.Error(), "cfk_ws_wrong") ||
				strings.Contains(err.Error()+log.String(), narrowSecret) {
				t.Fatalf("the error discloses the token: %v", err)
			}
		})
	}
}

// slowAPI answers GET /v1/me at once and /v1/budgets after delay.
func slowAPI(t *testing.T, delay time.Duration) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me":
			_, _ = io.WriteString(w, `{"organizationId":"org_1","workspaceId":"ws_own","tokenType":"workspace","tokenName":"assistant","capabilities":["viewBudgets"]}`)
		case "/v1/budgets":
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"links":{"self":"/v1/budgets"},"data":[{"id":"bud_1","name":"Production","amount":"100.00000000","currency":"USD","state":"onTrack"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveRun serves run in this process over an in-memory transport and returns a client connected
// to it; both stop when the test ends.
func serveRun(t *testing.T, apiURL string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientT, serverT := mcp.NewInMemoryTransports()
	env := map[string]string{"COSTFLUENT_API_TOKEN": secret, "COSTFLUENT_BASE_URL": apiURL}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(k string) string { return env[k] }, slog.New(slog.DiscardHandler), serverT)
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); cancel(); <-done })
	return cs
}

// callThroughRun serves run in this process and calls list_budgets.
func callThroughRun(t *testing.T, apiURL string) (*mcp.CallToolResult, time.Duration) {
	t.Helper()
	cs := serveRun(t, apiURL)
	start := time.Now()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_budgets"})
	if err != nil {
		t.Fatal(err)
	}
	return res, time.Since(start)
}

func TestRunWaitsEachAPIAttemptForItsTimeoutAndNoLonger(t *testing.T) {
	saved := apiAttemptTimeout
	t.Cleanup(func() { apiAttemptTimeout = saved })

	// A read slower than the bound ends with the SDK's timeout, not the read's answer, and at the
	// bound: run passes the bound to the client rather than leaving the SDK's 30-second default.
	apiAttemptTimeout = 300 * time.Millisecond
	res, took := callThroughRun(t, slowAPI(t, 2*time.Second).URL)
	body, _ := json.Marshal(res.Content)
	if !res.IsError || !strings.Contains(string(body), "timed out after 300ms") || took > 1500*time.Millisecond {
		t.Errorf("a read past a 300ms bound: IsError %v after %s, %s; want the timeout at the bound", res.IsError, took, body)
	}

	// A read inside the bound is answered.
	apiAttemptTimeout = 5 * time.Second
	if res, _ := callThroughRun(t, slowAPI(t, 500*time.Millisecond).URL); res.IsError {
		body, _ := json.Marshal(res.Content)
		t.Errorf("a 500ms read under a 5s bound failed: %s", body)
	}
}

// answer is how a fake API answers one GET /v1/costs.
type answer func(w http.ResponseWriter, r *http.Request)

// costsAPI serves GET /v1/me for a workspace token carrying viewCosts and answers the nth GET
// /v1/costs with answers[n], the last answer standing for every request after them. requests counts
// the /v1/costs requests.
func costsAPI(t *testing.T, answers ...answer) (apiURL string, requests *atomic.Int32) {
	requests = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"organizationId":"org_1","workspaceId":"ws_own","tokenType":"workspace","tokenName":"assistant","capabilities":["viewCosts"]}`)
		case "/v1/costs":
			n := int(requests.Add(1))
			answers[min(n, len(answers))-1](w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

// after answers as a does once d has passed, the time the API works on a read, unless the client
// gave up first.
func after(d time.Duration, a answer) answer {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			a(w, r)
		case <-r.Context().Done():
		}
	}
}

// problem writes the API's own problem details, asking the client to wait retryAfter seconds.
func problem(w http.ResponseWriter, status int, retryAfter, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Retry-After", retryAfter)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// apiGaveUp is the API's answer to a read that ran past its budget, asking for the 30 seconds it does.
func apiGaveUp(w http.ResponseWriter, _ *http.Request) {
	problem(w, http.StatusServiceUnavailable, "30", `{"title":"Service Unavailable","status":503,"code":"service_unavailable",`+
		`"detail":"This request ran longer than the server allows; narrow the date range, filters or grouping, or try again later","retryable":true}`)
}

// rateLimited is the API's 429, asking the client to wait retryAfter seconds.
func rateLimited(retryAfter string) answer {
	return func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusTooManyRequests, retryAfter, `{"title":"Too Many Requests","status":429,"code":"rate_limit_exceeded","retryable":true}`)
	}
}

// proxy answers as the proxy in front of the API does: plain text, with no Retry-After.
func proxy(status int) answer {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, http.StatusText(status))
	}
}

func costs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"links":{"self":"/v1/costs"},"data":[{"date":"2026-10-01","cost":"12.50000000","currency":"USD","dimensions":{}}],`+
		`"meta":{"startDate":"2026-10-01","endDate":"2026-10-31","granularity":"day","totalRecords":1,"totalCost":"12.50000000","currency":"USD"}}`)
}

// toolText is the text a call's result carries: a refusal's ApiError, or the error's own words.
func toolText(res *mcp.CallToolResult) string {
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String()
}

// A read the API gives up on comes back after its budget as a 503 asking for 30 seconds, longer than
// the client waits out, so the call ends after the one attempt with the API's advice to narrow the
// read, not after four. The waits the client does take are a rate limit's, and a read after one is
// answered, or a longer limit goes back with its wait: neither is the advice to narrow. The API's
// 90-second budget is scaled down to one second and the attempt timeout to 1.3; Retry-After values
// stay real, because the client sleeps only the one-second ones.
func TestRunReturnsTheAPIsRequestTimeoutAfterOneAttempt(t *testing.T) {
	saved := apiAttemptTimeout
	t.Cleanup(func() { apiAttemptTimeout = saved })
	const budget, attempt = time.Second, 1300 * time.Millisecond
	apiAttemptTimeout = attempt
	const narrow = "narrow the date range"

	// A call per subtest: the client writes the request's metadata into the params it is given.
	query := func() *mcp.CallToolParams {
		return &mcp.CallToolParams{Name: "query_costs", Arguments: map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31"}}
	}
	for _, tc := range []struct {
		name         string
		answers      []answer
		wantRequests int32
		wantError    []string // each in the tool error; none for a call that is answered
	}{
		{"the API gives up on the read", []answer{after(budget, apiGaveUp)}, 1, []string{narrow}},
		// The proxies in front of the API give up after 100 seconds, as long as an attempt waits, and
		// the attempt's clock starts first; the fake gives up later still, so that order holds on a
		// loaded test run.
		{"the proxy gives up no sooner than the attempt", []answer{after(attempt+200*time.Millisecond, proxy(http.StatusGatewayTimeout))}, 1, []string{"timed out after 1.3s"}},
		{"the attempt times out on an API that never answers", []answer{after(time.Hour, costs)}, 1, []string{"timed out after 1.3s"}},
		{"a short rate limit is waited out", []answer{rateLimited("1"), costs}, 2, nil},
		{"a read after a rate-limit wait is answered", []answer{rateLimited("1"), after(700*time.Millisecond, costs)}, 2, nil},
		{"a longer rate limit goes back with its wait", []answer{rateLimited("40")}, 1, []string{`"status":429`, `"retryAfterSeconds":40`}},
		{"a transient proxy error is retried", []answer{proxy(http.StatusBadGateway), costs}, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apiURL, requests := costsAPI(t, tc.answers...)
			res, err := serveRun(t, apiURL).CallTool(context.Background(), query())
			if err != nil {
				t.Fatal(err)
			}
			text := toolText(res)
			if len(tc.wantError) == 0 && res.IsError {
				t.Errorf("the call failed: %s", text)
			}
			for _, want := range tc.wantError {
				if !res.IsError || !strings.Contains(text, want) {
					t.Errorf("IsError %v, %s; want a tool error with %s", res.IsError, text, want)
				}
			}
			if !slices.Contains(tc.wantError, narrow) && strings.Contains(text, narrow) {
				t.Errorf("%s: the assistant is told to narrow a read the API did not refuse", text)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("%d /v1/costs requests, want %d", got, tc.wantRequests)
			}
		})
	}

	t.Run("the assistant cancels the call", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		aborted := make(chan struct{}, 4)
		apiURL, requests := costsAPI(t, func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
			aborted <- struct{}{}
		})
		if _, err := serveRun(t, apiURL).CallTool(ctx, query()); err == nil {
			t.Fatal("the cancelled call was answered")
		}
		// Inside the attempt timeout, so only the cancellation can have ended the read.
		select {
		case <-aborted:
		case <-time.After(time.Second):
			t.Fatal("the read went on after the call was cancelled")
		}
		// Longer than the backoff a resent read would wait.
		time.Sleep(1500 * time.Millisecond)
		if got := requests.Load(); got != 1 {
			t.Errorf("%d /v1/costs requests, want 1: a cancelled call was sent again", got)
		}
	})
}
