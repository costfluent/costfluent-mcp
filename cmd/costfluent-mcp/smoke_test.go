package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The smoke test runs only when it is given a real token, so `go test ./...` never reaches a
// deployed API.
const (
	smokeTokenEnv   = "COSTFLUENT_MCP_SMOKE_API_TOKEN"
	smokeBaseURLEnv = "COSTFLUENT_MCP_SMOKE_BASE_URL"
	smokeBaseURL    = "https://api.dev.costfluent.com"
)

// TestSmokeAgainstADeployedAPI starts the server over stdio against a deployed Public API and
// calls every tool its token is served once, failing on any refusal.
func TestSmokeAgainstADeployedAPI(t *testing.T) {
	token := os.Getenv(smokeTokenEnv)
	if token == "" {
		t.Skip(smokeTokenEnv + " is not set")
	}
	baseURL := os.Getenv(smokeBaseURLEnv)
	if baseURL == "" {
		baseURL = smokeBaseURL
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), serveEnv+"=1", "COSTFLUENT_API_TOKEN="+token, "COSTFLUENT_BASE_URL="+baseURL)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "test"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting: %v\n%s", err, stderr)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatalf("the token is served no tools; give it read capabilities\n%s", stderr)
	}
	end := time.Now().UTC()
	window := map[string]any{"start_date": end.AddDate(0, 0, -7).Format(time.DateOnly), "end_date": end.Format(time.DateOnly)}
	var reportID string
	for _, tool := range tools.Tools {
		args := map[string]any{}
		switch tool.Name {
		case "query_costs":
			args = window
		case "get_cost_report_data":
			continue // called below with a report list_cost_reports named
		}
		res := smokeCall(ctx, t, cs, tool.Name, args)
		if tool.Name == "list_cost_reports" {
			var out struct {
				Reports []struct {
					ID string `json:"id"`
				} `json:"reports"`
			}
			body, _ := json.Marshal(res.StructuredContent)
			if json.Unmarshal(body, &out) == nil && len(out.Reports) > 0 {
				reportID = out.Reports[0].ID
			}
		}
	}
	if reportID != "" {
		smokeCall(ctx, t, cs, "get_cost_report_data", map[string]any{"cost_report_id": reportID})
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the server logged its token")
	}
}

func smokeCall(ctx context.Context, t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Errorf("%s refused: %v", name, res.Content)
	} else {
		t.Logf("%s: ok", name)
	}
	return res
}
