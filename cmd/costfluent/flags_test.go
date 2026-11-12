package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testSchema has a property of every type a tool's input can take, as jsonschema-go infers them.
var testSchema = map[string]any{
	"type":     "object",
	"required": []any{"start_date"},
	"properties": map[string]any{
		"start_date":       map[string]any{"type": "string", "description": "the first day"},
		"page":             map[string]any{"type": "integer"},
		"minimum":          map[string]any{"type": "number"},
		"include_resolved": map[string]any{"type": "boolean"},
		"amortize":         map[string]any{"type": []any{"null", "boolean"}},
		"group_by":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

func testParams(t *testing.T) []param {
	t.Helper()
	params, err := paramsOf(testSchema)
	if err != nil {
		t.Fatal(err)
	}
	return params
}

func TestParamsOfEachSchemaType(t *testing.T) {
	params := testParams(t)
	var got []string
	kinds := map[string]kind{}
	for _, p := range params {
		got = append(got, p.flag)
		kinds[p.name] = p.kind
	}
	want := []string{"start-date", "amortize", "group-by", "include-resolved", "minimum", "page"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flags %v, want required first, then by name: %v", got, want)
	}
	wantKinds := map[string]kind{"start_date": kindString, "page": kindInteger, "minimum": kindNumber,
		"include_resolved": kindBool, "amortize": kindBool, "group_by": kindStrings}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("kinds %v, want %v", kinds, wantKinds)
	}
	if !params[0].required || params[0].description != "the first day" {
		t.Errorf("start_date %+v", params[0])
	}
}

func TestParamsOfRefusesWhatHasNoFlagForm(t *testing.T) {
	for name, prop := range map[string]any{
		"object":           map[string]any{"type": "object"},
		"array of numbers": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
		"two types":        map[string]any{"type": []any{"string", "integer"}},
		"no type":          map[string]any{},
	} {
		schema := map[string]any{"type": "object", "properties": map[string]any{"x": prop}}
		if _, err := paramsOf(schema); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	reserved := map[string]any{"type": "object", "properties": map[string]any{"output": map[string]any{"type": "string"}}}
	if _, err := paramsOf(reserved); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("a property named output: %v", err)
	}
}

func TestParseArgsSetsOnlyWhatIsGiven(t *testing.T) {
	args, output, err := parseArgs("test", testParams(t), []string{
		"--start-date", "2026-10-01", "--page=2", "--minimum", "12.50", "--include-resolved",
		"--amortize=false", "--group-by", "ServiceName", "--group-by=Tag:team",
	}, outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"start_date": "2026-10-01", "page": int64(2), "minimum": json.Number("12.50"),
		"include_resolved": true, "amortize": false, "group_by": []string{"ServiceName", "Tag:team"}}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args %#v, want %#v", args, want)
	}
	if output != outputJSON {
		t.Errorf("output %s", output)
	}

	args, output, err = parseArgs("test", testParams(t), []string{"--start-date", "2026-10-01", "--output", "table"}, outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, map[string]any{"start_date": "2026-10-01"}) || output != outputTable {
		t.Errorf("args %v, output %s: an omitted flag must stay out of the arguments", args, output)
	}
}

func TestParseArgsRefusesAWrongCommandLine(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		want string
	}{
		"unknown flag":     {[]string{"--start-date", "x", "--colour"}, "unknown flag --colour"},
		"bad integer":      {[]string{"--start-date", "x", "--page", "two"}, `--page takes a whole number, not "two"`},
		"bad number":       {[]string{"--start-date", "x", "--minimum", "lots"}, `--minimum takes a number, not "lots"`},
		"bad boolean":      {[]string{"--start-date", "x", "--amortize=perhaps"}, `--amortize takes true or false, not "perhaps"`},
		"missing value":    {[]string{"--start-date"}, "flag needs an argument: --start-date"},
		"missing required": {[]string{"--page", "1"}, "missing --start-date"},
		"positional":       {[]string{"--start-date", "x", "extra"}, `unexpected argument "extra"`},
		"bad output":       {[]string{"--start-date", "x", "--output", "csv"}, `--output takes json or table, not "csv"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseArgs("test", testParams(t), tc.argv, outputJSON)
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Fatalf("err %v, want a usage error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %q, want %q", err, tc.want)
			}
		})
	}
	if _, _, err := parseArgs("test", testParams(t), []string{"--help"}, outputJSON); !errors.Is(err, errHelp) {
		t.Errorf("--help: %v", err)
	}
}

func TestARequiredBooleanIsShownAsAFlagAlone(t *testing.T) {
	cmd := &command{name: "test", tool: &mcp.Tool{Description: "Test."}, params: []param{
		{name: "confirm", flag: "confirm", kind: kindBool, required: true, description: "confirm it"},
	}}
	help := commandHelp(cmd)
	if !strings.Contains(help, "Usage: costfluent test --confirm [flags]") || strings.Contains(help, "--confirm <") {
		t.Errorf("help:\n%s", help)
	}
	// The form the synopsis shows parses.
	if _, _, err := parseArgs("test", cmd.params, []string{"--confirm"}, outputJSON); err != nil {
		t.Error(err)
	}
}
