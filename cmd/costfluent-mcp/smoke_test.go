package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"slices"
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
	smokeBaseURL    = "https://api.costfluent.dev"
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
	// An organization token reads a workspace's objects only when told which workspace, as an
	// assistant learns from list_workspaces; a workspace token's own is the default.
	workspace := ""
	if slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == "list_workspaces" }) {
		text := smokeCall(ctx, t, cs, "list_workspaces", nil)
		var listed struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(text), &listed) != nil || len(listed.Items) == 0 {
			t.Fatalf("list_workspaces named no workspace: %s", text)
		}
		workspace = listed.Items[0].ID
		t.Logf("reading workspace %s", workspace)
	}
	end := time.Now().UTC()
	window := map[string]any{"start_date": end.AddDate(0, 0, -7).Format(time.DateOnly), "end_date": end.Format(time.DateOnly)}
	var reportID string
	for _, tool := range tools.Tools {
		args := map[string]any{}
		switch tool.Name {
		case "list_workspaces":
			continue // called above
		case "query_costs", "get_commitment_coverage":
			maps.Copy(args, window)
		case "get_kubernetes_efficiency":
			maps.Copy(args, window)
			args["level"] = "cluster"
		case "get_cost_report_data":
			continue // called below with a report list_cost_reports named
		}
		if workspace != "" && takes(tool, "workspace_id") {
			args["workspace_id"] = workspace
		}
		text := smokeCall(ctx, t, cs, tool.Name, args)
		if tool.Name == "list_cost_reports" {
			var out struct {
				Reports []struct {
					ID string `json:"id"`
				} `json:"reports"`
			}
			if json.Unmarshal([]byte(text), &out) == nil && len(out.Reports) > 0 {
				reportID = out.Reports[0].ID
			}
		}
	}
	if reportID != "" {
		args := map[string]any{"cost_report_id": reportID}
		if workspace != "" {
			args["workspace_id"] = workspace
		}
		smokeCall(ctx, t, cs, "get_cost_report_data", args)
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the server logged its token")
	}
}

// takes reports whether the tool's input schema has the argument.
func takes(tool *mcp.Tool, argument string) bool {
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	raw, _ := json.Marshal(tool.InputSchema)
	_ = json.Unmarshal(raw, &schema)
	_, ok := schema.Properties[argument]
	return ok
}

// smokeCall calls one tool and returns the text an assistant reads, failing the test with that
// text when the call is refused.
func smokeCall(ctx context.Context, t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Errorf("%s refused: %s", name, text.String())
	} else {
		t.Logf("%s: ok", name)
	}
	return text.String()
}
