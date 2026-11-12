package server

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

var update = flag.Bool("update", false, "rewrite testdata/tools-list.json")

// The tools/list answer a token with every capability gets, in an organization that lets AI agents
// write, is the whole surface an assistant reads: each tool's name, description and input schema.
// It is pinned whole, so a change to what an assistant is told shows in review, and the agent
// skills' evals answer from a copy of it (evals/mocks/costfluent/_tools.json in the agent skills
// tree), which their check holds equal.
func TestToolsListIsTheGolden(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {capabilities: costfluent.Capabilities()}})
	api.setAgentWrites(true)
	cs := session(t, srv.URL, tokenSecret)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(map[string]any{"tools": res.Tools}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "tools-list.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; go test ./internal/server -run TestToolsListIsTheGolden -update writes it", err)
	}
	if string(got) != string(want) {
		t.Errorf("tools/list differs from %s; review the change, then rerun with -update:\n%s", path, got)
	}
}
