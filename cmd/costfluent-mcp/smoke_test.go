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

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// The smoke test runs only when it is given a real token, so `go test ./...` never reaches a
// deployed API.
const (
	smokeTokenEnv      = "COSTFLUENT_MCP_SMOKE_API_TOKEN"
	smokeWriteTokenEnv = "COSTFLUENT_MCP_SMOKE_WRITE_API_TOKEN"
	smokeBaseURLEnv    = "COSTFLUENT_MCP_SMOKE_BASE_URL"
	smokeBaseURL       = "https://api.costfluent.dev"
	// smokeDocsEnv set to "skip" leaves the docs tools uncalled, so a run that must not depend on
	// the public docs portal, stack CI's, passes while the portal is down.
	smokeDocsEnv = "COSTFLUENT_MCP_SMOKE_DOCS"
	smokeDocsAsk = "how do I manage budgets with Terraform?"
)

// TestSmokeAgainstADeployedAPI starts the server over stdio against a deployed Public API and
// calls every tool its token is served once. It fails when the token is not served every read tool
// the server declares (a Read only token is meant to reach them all), when it is served a write
// tool (a Read only token carries no write capability), when a tool refuses, or when
// list_cost_reports names no report, which would leave get_cost_report_data uncalled.
func TestSmokeAgainstADeployedAPI(t *testing.T) {
	token := os.Getenv(smokeTokenEnv)
	if token == "" {
		t.Skip(smokeTokenEnv + " is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cs, stderr := smokeSession(ctx, t, token)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing := unserved(tools.Tools); len(missing) > 0 {
		t.Fatalf("the token is served %d of the tools the server declares, without %v; the Read only preset must grant every capability they need\n%s",
			len(tools.Tools), missing, stderr)
	}
	if writes := servedWrites(tools.Tools); len(writes) > 0 {
		t.Fatalf("a Read only token is served the write tools %v; smoke reads with a Read only token, writes with %s", writes, smokeWriteTokenEnv)
	}
	workspace := smokeWorkspace(ctx, t, cs, tools.Tools)
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
		case "get_cost_report_data", "search_docs", "get_doc_page":
			continue // called below with a report list_cost_reports names, and with the docs
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
	if reportID == "" {
		t.Fatal("list_cost_reports named no report, so get_cost_report_data was never called; smoke with a token whose workspaces hold saved reports")
	}
	args := map[string]any{"cost_report_id": reportID}
	if workspace != "" {
		args["workspace_id"] = workspace
	}
	smokeCall(ctx, t, cs, "get_cost_report_data", args)
	smokeDocs(t, func(name string, args map[string]any) string { return smokeCall(ctx, t, cs, name, args) })
	if strings.Contains(stderr.String(), token) {
		t.Error("the server logged its token")
	}
}

// smokeDocs asks search_docs what an engineer asks, then reads its first result with get_doc_page,
// unless smokeDocsEnv says to skip them.
func smokeDocs(t *testing.T, call func(name string, args map[string]any) string) {
	t.Helper()
	if os.Getenv(smokeDocsEnv) == "skip" {
		t.Logf("%s=skip: the docs tools are not called", smokeDocsEnv)
		return
	}
	var found struct {
		Results []struct {
			URL string `json:"url"`
		} `json:"results"`
	}
	text := call("search_docs", map[string]any{"query": smokeDocsAsk})
	if json.Unmarshal([]byte(text), &found) != nil || len(found.Results) == 0 {
		t.Fatalf("search_docs found nothing for %q: %s", smokeDocsAsk, text)
	}
	t.Logf("search_docs: %s first", found.Results[0].URL)
	var page struct {
		Markdown string `json:"markdown"`
	}
	text = call("get_doc_page", map[string]any{"url": found.Results[0].URL})
	if json.Unmarshal([]byte(text), &page) != nil || page.Markdown == "" {
		t.Fatalf("get_doc_page read nothing at %s: %.200s", found.Results[0].URL, text)
	}
}

// smokeSession starts the server over stdio with token against the deployed API the environment
// names, and returns the client session and the server's stderr.
func smokeSession(ctx context.Context, t *testing.T, token string) (*mcp.ClientSession, *syncBuffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), serveEnv+"=1", "COSTFLUENT_API_TOKEN="+token, "COSTFLUENT_BASE_URL="+smokeBase())
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "test"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting: %v\n%s", err, stderr)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, stderr
}

func smokeBase() string {
	if baseURL := os.Getenv(smokeBaseURLEnv); baseURL != "" {
		return baseURL
	}
	return smokeBaseURL
}

// smokeWorkspace names the workspace to act in: for an organization token the first that
// list_workspaces names, as an assistant learns it, and for a workspace token its own (empty).
func smokeWorkspace(ctx context.Context, t *testing.T, cs *mcp.ClientSession, tools []*mcp.Tool) string {
	t.Helper()
	if !slices.ContainsFunc(tools, func(tool *mcp.Tool) bool { return tool.Name == "list_workspaces" }) {
		return ""
	}
	text := smokeCall(ctx, t, cs, "list_workspaces", nil)
	var listed struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(text), &listed) != nil || len(listed.Items) == 0 {
		t.Fatalf("list_workspaces named no workspace: %s", text)
	}
	t.Logf("acting in workspace %s", listed.Items[0].ID)
	return listed.Items[0].ID
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

// unserved names each tool the server declares that the served list leaves out. The servers' docs
// and refusal tell a customer to give an assistant a Read only token, so every tool is meant for
// that preset, and a capability the preset stops granting takes its tools away from every customer.
func unserved(served []*mcp.Tool) []string {
	declared := server.ToolNames(&costfluent.Me{Capabilities: costfluent.Capabilities()})
	return slices.DeleteFunc(declared, func(name string) bool {
		return slices.ContainsFunc(served, func(tool *mcp.Tool) bool { return tool.Name == name })
	})
}

// writeTools names every write tool the server declares: those a token with every capability is
// served only once its organization lets AI agents create and change objects.
func writeTools() []string {
	reads := server.ToolNames(&costfluent.Me{Capabilities: costfluent.Capabilities()})
	all := server.ToolNames(&costfluent.Me{Capabilities: costfluent.Capabilities(), AgentWritesAllowed: true})
	return slices.DeleteFunc(all, func(name string) bool { return slices.Contains(reads, name) })
}

// servedWrites names each served tool that writes.
func servedWrites(served []*mcp.Tool) []string {
	writes := writeTools()
	var names []string
	for _, tool := range served {
		if slices.Contains(writes, tool.Name) {
			names = append(names, tool.Name)
		}
	}
	return names
}
