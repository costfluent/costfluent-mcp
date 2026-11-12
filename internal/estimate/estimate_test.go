package estimate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The arguments the Action's test produces its golden outputs with.
var goldenArgs = Args{
	WorkspaceID:   "wsp_8f3k2m4n5p6q7r8s9t0v1w",
	Repository:    "acme/infra",
	PullRequest:   "42",
	Commit:        "9fceb02d0ae598e95dc970b74767f19372d61af8",
	URL:           "https://github.com/acme/infra/pull/42",
	SourceVersion: "0.10.41",
}

func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return doc
}

// sameJSON compares two JSON texts as values, so key order and spacing do not matter.
func sameJSON(t *testing.T, name string, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: the output is not JSON: %v", name, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: the golden file is not JSON: %v", name, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s under gojq differs from its golden output:\n got %s\nwant %s", name, got, want)
	}
}

// TestEveryFixtureGivesItsGoldenOutput runs both programs on every fixture as the Action's own
// tests run them under jq, and holds gojq's output to the same golden files: an estimate per
// Infracost fixture, one with its Terraform plan, and the apply per plan with its state. A refused fixture's golden is {"error": <message>}.
func TestEveryFixtureGivesItsGoldenOutput(t *testing.T) {
	var checked []string
	golden := func(name string, got []byte) {
		t.Helper()
		checked = append(checked, name)
		want, err := os.ReadFile(filepath.Join("testdata", "expected", name))
		if err != nil {
			t.Errorf("%s has no golden output: %v", name, err)
			return
		}
		sameJSON(t, name, got, want)
	}
	estimate := func(name, input, plan string) {
		t.Helper()
		var planDoc map[string]any
		if plan != "" {
			planDoc = fixture(t, plan)
		}
		out, err := Estimate(fixture(t, input), planDoc, goldenArgs)
		if err != nil {
			out, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		golden(name, out)
	}

	inputs, err := filepath.Glob(filepath.Join("testdata", "infracost-*.json"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no Infracost fixtures: %v", err)
	}
	for _, input := range inputs {
		estimate(filepath.Base(input), filepath.Base(input), "")
	}
	estimate("infracost-v2-diff-usage+terraform-plan.json", "infracost-v2-diff-usage.json", "terraform-plan.json")

	plans, err := filepath.Glob(filepath.Join("testdata", "terraform*-plan.json"))
	if err != nil || len(plans) == 0 {
		t.Fatalf("no Terraform plan fixtures: %v", err)
	}
	for _, plan := range plans {
		name := filepath.Base(plan)
		applied, err := Apply(fixture(t, name), fixture(t, strings.TrimSuffix(name, "-plan.json")+"-state.json"),
			goldenArgs.WorkspaceID, goldenArgs.Repository, 42)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out, err := json.Marshal(applied)
		if err != nil {
			t.Fatal(err)
		}
		golden(name, out)
	}

	files, err := filepath.Glob(filepath.Join("testdata", "expected", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !slices.Contains(checked, filepath.Base(file)) {
			t.Errorf("the golden output %s has no input here", filepath.Base(file))
		}
	}
}

// infracost is a one-resource Infracost v0.10 document, with the given monthly cost after.
func infracost(cost any) map[string]any {
	return map[string]any{
		"version": "0.2", "currency": "USD",
		"projects": []any{map[string]any{
			"name":          "infra",
			"pastBreakdown": map[string]any{"resources": []any{}},
			"breakdown": map[string]any{"resources": []any{map[string]any{
				"name": "aws_instance.web", "resourceType": "aws_instance", "monthlyCost": cost,
			}}},
		}},
	}
}

func TestEstimateKeepsEveryDigitOfANumber(t *testing.T) {
	doc, err := Decode([]byte(`{"version":"0.2","currency":"USD","projects":[{"name":"infra","pastBreakdown":{"resources":[]},` +
		`"breakdown":{"resources":[{"name":"aws_instance.web","resourceType":"aws_instance","monthlyCost":12345678901234567.123456789}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Estimate(doc, nil, goldenArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"monthlyCostAfter":"12345678901234567.12345679"`) {
		t.Errorf("a cost written as a number lost digits or was not rounded to 8 places: %s", out)
	}
}

func TestEstimateRefusesWhatTheProgramRefuses(t *testing.T) {
	notAPlan := map[string]any{"format_version": "1.2"}
	cases := []struct {
		name string
		doc  map[string]any
		plan map[string]any
		args func(*Args)
		want string
	}{
		{"not Infracost JSON", map[string]any{"projects": "none"}, nil, nil,
			"infracost-to-estimate: the input is not Infracost JSON this reads"},
		{"a currency that is no ISO code", func() map[string]any { d := infracost("1"); d["currency"] = "usd"; return d }(), nil, nil,
			`infracost-to-estimate: the currency "usd" is not an ISO 4217 code`},
		{"a null currency", func() map[string]any { d := infracost("1"); d["currency"] = nil; return d }(), nil, nil,
			"infracost-to-estimate: the currency null is not an ISO 4217 code"},
		{"a false currency", func() map[string]any { d := infracost("1"); d["currency"] = false; return d }(), nil, nil,
			"infracost-to-estimate: the currency false is not an ISO 4217 code"},
		{"a cost that is no decimal", infracost("1,5"), nil, nil,
			`infracost-to-estimate: aws_instance.web: "1,5" is not a decimal number`},
		{"a plan that is no plan", infracost("1"), notAPlan, nil,
			"infracost-to-estimate: terraformPlan is not `terraform show -json` of a saved plan"},
		{"no workspace", infracost("1"), nil, func(a *Args) { a.WorkspaceID = "" },
			"infracost-to-estimate: workspaceId is required"},
		{"no repository", infracost("1"), nil, func(a *Args) { a.Repository = "" },
			"infracost-to-estimate: repository is required"},
		{"a pull request that is no number", infracost("1"), nil, func(a *Args) { a.PullRequest = "4x" },
			`infracost-to-estimate: pullRequest must be a pull request number, not "4x"`},
		{"a url that is not https", infracost("1"), nil, func(a *Args) { a.URL = "http://example.com" },
			"infracost-to-estimate: url must start with https://"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := goldenArgs
			if c.args != nil {
				c.args(&args)
			}
			out, err := Estimate(c.doc, c.plan, args)
			if err == nil {
				t.Fatalf("converted: %s", out)
			}
			if !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("error %q, want it to start %q", err, c.want)
			}
		})
	}
}

func TestEstimateLeavesAnEmptyArgumentOut(t *testing.T) {
	args := goldenArgs
	args.Commit, args.URL, args.SourceVersion = "", "", ""
	out, err := Estimate(infracost("10"), nil, args)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"commit", "url", "sourceVersion"} {
		if _, ok := body[key]; ok {
			t.Errorf("an empty %s is posted: %s", key, out)
		}
	}
	if body["pullRequest"] != float64(42) {
		t.Errorf("pullRequest is %v, want the number 42", body["pullRequest"])
	}
}

func TestApplyRefusesAPlanOrStateOfTheWrongKind(t *testing.T) {
	plan := fixture(t, "terraform-apply-plan.json")
	state := fixture(t, "terraform-apply-state.json")
	if _, err := Apply(state, state, "wsp_1", "acme/infra", 1); !errors.Is(err, ErrNotAPlan) {
		t.Errorf("a state given as the plan: %v, want %v", err, ErrNotAPlan)
	}
	if _, err := Apply(plan, plan, "wsp_1", "acme/infra", 1); !errors.Is(err, ErrNotAState) {
		t.Errorf("a plan given as the state: %v, want %v", err, ErrNotAState)
	}
	if _, err := Apply(plan, map[string]any{}, "wsp_1", "acme/infra", 1); !errors.Is(err, ErrNotAState) {
		t.Errorf("a state without format_version: %v, want %v", err, ErrNotAState)
	}
}

func TestApplyNamesTheAddressesTheStateHoldsNoIdFor(t *testing.T) {
	state := fixture(t, "terraform-apply-state.json")
	state["values"] = map[string]any{"root_module": map[string]any{}}
	applied, err := Apply(fixture(t, "terraform-apply-plan.json"), state, "wsp_1", "acme/infra", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aws_default_subnet.default", "aws_instance.ec2", "aws_volume_attachment.storage_attachment"}
	if !slices.Equal(applied.Missing, want) {
		t.Errorf("missing %v, want %v", applied.Missing, want)
	}
	var body struct {
		PullRequest int   `json:"pullRequest"`
		Resources   []any `json:"resources"`
	}
	if err := json.Unmarshal(applied.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest != 7 || len(body.Resources) != 0 {
		t.Errorf("body %s, want pull request 7 and no resources", applied.Body)
	}
}

func TestDecodeRefusesWhatIsNoJSONObject(t *testing.T) {
	for input, want := range map[string]string{
		``:          "not JSON",
		`{"a":`:     "not JSON",
		`[1]`:       "not a JSON object",
		`"text"`:    "not a JSON object",
		`{} {}`:     "not JSON: more follows the first value",
		`{"a":1} x`: "not JSON: more follows the first value",
	} {
		if _, err := Decode([]byte(input)); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Decode(%q): %v, want %q", input, err, want)
		}
	}
	if doc, err := Decode([]byte(" {\"a\": 1}\n")); err != nil || doc["a"] != json.Number("1") {
		t.Errorf("Decode of an object: %v, %v", doc, err)
	}
}

func TestFirstReportsAProgramThatGivesNoDocument(t *testing.T) {
	for source, want := range map[string]string{
		"empty":            "the program gave no output",
		`error({code: 1})`: `error: {"code":1}`,
		`error("refused")`: "refused",
		`{a: 1}, {a: 2}`:   "",
	} {
		code, err := compile("test.jq", source)
		if err != nil {
			t.Fatal(err)
		}
		out, err := first(code.Run(nil))
		switch {
		case want == "" && (err != nil || string(out) != `{"a":1}`):
			t.Errorf("%s: %s, %v; want the first output", source, out, err)
		case want != "" && (err == nil || err.Error() != want):
			t.Errorf("%s: %v, want %q", source, err, want)
		}
	}
}

func TestBothProgramsCompile(t *testing.T) {
	if _, err := compiled(); err != nil {
		t.Fatal(err)
	}
	if _, err := compile("broken.jq", "def x: ;", "$ARGS"); err == nil {
		t.Error("a program that does not parse compiled")
	}
	if _, err := compile("unbound.jq", "$nothing", "$ARGS"); err == nil {
		t.Error("a program reading an undefined variable compiled")
	}
}
