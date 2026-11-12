package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// The smoke test reads the stdio server's smoke variables, so one Read only token smokes both, and
// runs only when it is given one, so `go test ./...` never reaches a deployed API.
const (
	smokeTokenEnv   = "COSTFLUENT_MCP_SMOKE_API_TOKEN"
	smokeBaseURLEnv = "COSTFLUENT_MCP_SMOKE_BASE_URL"
	smokeBaseURL    = "https://api.costfluent.dev"
)

// TestCLISmokeAgainstADeployedAPI runs the CLI smoke against a deployed Public API.
func TestCLISmokeAgainstADeployedAPI(t *testing.T) {
	token := os.Getenv(smokeTokenEnv)
	if token == "" {
		t.Skip(smokeTokenEnv + " is not set")
	}
	baseURL := os.Getenv(smokeBaseURLEnv)
	if baseURL == "" {
		baseURL = smokeBaseURL
	}
	smokeCLI(t, token, baseURL)
}

// TestCLISmokeAgainstTheFakeAPI runs the CLI smoke with a Read only token against the fake API, so a
// command the smoke cannot call with that token fails here, not first against a deployed API.
func TestCLISmokeAgainstTheFakeAPI(t *testing.T) {
	t.Setenv("COSTFLUENT_MCP_SMOKE_DOCS", "skip")
	for name, agentWrites := range map[string]bool{"agent writes off": false, "agent writes on": true} {
		t.Run(name, func(t *testing.T) {
			var reads []costfluent.Capability
			for _, e := range server.Catalog() {
				if !e.Writes && !slices.Contains(reads, e.Capability) {
					reads = append(reads, e.Capability)
				}
			}
			_, srv := newFakeAPI(t, map[string]token{tokenSecret: {name: "read-only", capabilities: reads, agentWrites: agentWrites}})
			smokeCLI(t, tokenSecret, srv.URL)
		})
	}
}

// smokeCLI runs the CLI as a process against the Public API at baseURL: whoami, then every read
// command once, each of which must exit 0 with JSON on stdout. The token is a Read only token, so
// the write commands are left out. It fails when list-cost-reports names no report, which would
// leave get-cost-report-data uncalled.
func smokeCLI(t *testing.T, token, baseURL string) {
	t.Helper()
	env := append(withoutCostfluent(os.Environ()), mainEnv+"=1", "COSTFLUENT_API_TOKEN="+token, "COSTFLUENT_BASE_URL="+baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cliRun := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("costfluent %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), token) {
			t.Fatalf("costfluent %s printed the token", strings.Join(args, " "))
		}
		if !json.Valid(stdout.Bytes()) {
			t.Fatalf("costfluent %s printed no JSON:\n%s", strings.Join(args, " "), stdout.String())
		}
		t.Logf("costfluent %s: ok", args[0])
		return stdout.String()
	}

	var me struct {
		WorkspaceID *string `json:"workspaceId"`
	}
	if err := json.Unmarshal([]byte(cliRun("whoami")), &me); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(cliRun("list-workspaces")), &listed); err != nil || len(listed.Items) == 0 {
		t.Fatalf("list-workspaces named no workspace: %v", err)
	}
	workspace := listed.Items[0].ID
	if me.WorkspaceID != nil {
		workspace = *me.WorkspaceID
	}

	commands, err := loadCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC()
	window := []string{"--start-date", end.AddDate(0, 0, -7).Format(time.DateOnly), "--end-date", end.Format(time.DateOnly)}
	reportID := ""
	for _, cmd := range commands {
		if cmd.writes {
			continue // a Read only token is refused every write; the server's write smoke makes them
		}
		args := []string{cmd.name}
		switch cmd.name {
		case "list-workspaces", "get-cost-report-data", "search-docs", "get-doc-page":
			continue // called above, and below with a report list-cost-reports names, and with the docs
		case "query-costs", "get-commitment-coverage":
			args = append(args, window...)
		case "get-kubernetes-efficiency":
			args = append(append(args, window...), "--level", "cluster")
		}
		if takesFlag(cmd, "workspace-id") {
			args = append(args, "--workspace-id", workspace)
		}
		out := cliRun(args...)
		if cmd.name == "list-cost-reports" {
			var reports struct {
				Reports []struct {
					ID string `json:"id"`
				} `json:"reports"`
			}
			if json.Unmarshal([]byte(out), &reports) == nil && len(reports.Reports) > 0 {
				reportID = reports.Reports[0].ID
			}
		}
	}
	if reportID == "" {
		t.Fatal("list-cost-reports named no report, so get-cost-report-data was never called; smoke with a token whose first workspace holds a saved report")
	}
	cliRun("get-cost-report-data", "--cost-report-id", reportID, "--workspace-id", workspace)

	// The docs commands need no token; COSTFLUENT_MCP_SMOKE_DOCS=skip leaves them out, as stack CI does.
	if os.Getenv("COSTFLUENT_MCP_SMOKE_DOCS") == "skip" {
		return
	}
	var found struct {
		Results []struct {
			URL string `json:"url"`
		} `json:"results"`
	}
	if out := cliRun("search-docs", "--query", "how do I manage budgets with Terraform?"); json.Unmarshal([]byte(out), &found) != nil || len(found.Results) == 0 {
		t.Fatalf("search-docs found nothing: %s", out)
	}
	if out := cliRun("get-doc-page", "--url", found.Results[0].URL); !strings.Contains(out, `"markdown"`) {
		t.Fatalf("get-doc-page read nothing at %s: %.200s", found.Results[0].URL, out)
	}
}

func takesFlag(cmd *command, flag string) bool {
	for _, p := range cmd.params {
		if p.flag == flag {
			return true
		}
	}
	return false
}
