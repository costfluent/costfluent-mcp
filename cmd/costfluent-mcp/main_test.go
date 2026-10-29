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
	if !slices.Equal(names, []string{"list_budgets"}) {
		t.Errorf("tools %v, want only list_budgets for a token carrying viewBudgets", names)
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

// callThroughRun serves run in this process over an in-memory transport and calls list_budgets.
func callThroughRun(t *testing.T, apiURL string) (*mcp.CallToolResult, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	env := map[string]string{"COSTFLUENT_API_TOKEN": secret, "COSTFLUENT_BASE_URL": apiURL}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(k string) string { return env[k] }, slog.New(slog.DiscardHandler), serverT)
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); cancel(); <-done }()
	start := time.Now()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_budgets"})
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
