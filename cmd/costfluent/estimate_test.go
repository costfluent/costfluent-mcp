package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

// estimateFixtures are the cost estimate Action's fixtures and golden outputs, as the estimate
// package's tests read them.
const estimateFixtures = "../../internal/estimate/testdata"

const (
	estimateWorkspace = "wsp_8f3k2m4n5p6q7r8s9t0v1w"
	estimateID        = "est_8f3k2m4n5p6q7r8s9t0v1w"
	agentHeader       = "Costfluent-Agent"
)

// openAPI is the part of the Public API contract the fake holds estimate requests and answers to.
type openAPI struct {
	schemas map[string]map[string]any
}

func loadOpenAPI(t *testing.T) *openAPI {
	t.Helper()
	raw, err := os.ReadFile("../../internal/costfluent/testdata/public.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return &openAPI{schemas: doc.Components.Schemas}
}

// check returns where body breaks the named component schema: its types, required and unknown
// members, patterns and enums.
func (o *openAPI) check(name string, body []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return []string{"not JSON: " + err.Error()}
	}
	var problems []string
	o.walk("$", o.schemas[name], value, &problems)
	return problems
}

func (o *openAPI) walk(at string, s map[string]any, v any, problems *[]string) {
	if s == nil {
		*problems = append(*problems, at+": no schema")
		return
	}
	if v == nil {
		if s["nullable"] != true {
			*problems = append(*problems, at+": null where the contract takes no null")
		}
		return
	}
	if ref, ok := s["$ref"].(string); ok {
		o.walk(at, o.schemas[strings.TrimPrefix(ref, "#/components/schemas/")], v, problems)
	}
	if oneOf, ok := s["oneOf"].([]any); ok {
		matched := false
		for _, option := range oneOf {
			var found []string
			o.walk(at, option.(map[string]any), v, &found)
			matched = matched || len(found) == 0
		}
		if !matched {
			*problems = append(*problems, at+": matches none of oneOf")
		}
	}
	switch s["type"] {
	case "object":
		object, ok := v.(map[string]any)
		if !ok {
			*problems = append(*problems, at+": not an object")
			return
		}
		properties, _ := s["properties"].(map[string]any)
		required, _ := s["required"].([]any)
		for _, key := range required {
			if _, ok := object[key.(string)]; !ok {
				*problems = append(*problems, fmt.Sprintf("%s: no required %s", at, key))
			}
		}
		for key, member := range object {
			property, ok := properties[key].(map[string]any)
			if !ok {
				*problems = append(*problems, fmt.Sprintf("%s: %s is no member of the contract's object", at, key))
				continue
			}
			o.walk(at+"."+key, property, member, problems)
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			*problems = append(*problems, at+": not an array")
			return
		}
		items, _ := s["items"].(map[string]any)
		for i, item := range list {
			o.walk(fmt.Sprintf("%s[%d]", at, i), items, item, problems)
		}
	case "string":
		text, ok := v.(string)
		if !ok {
			*problems = append(*problems, at+": not a string")
			return
		}
		if pattern, ok := s["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
			*problems = append(*problems, fmt.Sprintf("%s: %q does not match %s", at, text, pattern))
		}
		if values, ok := s["x-extensible-enum"].([]any); ok && !slices.Contains(values, any(text)) {
			*problems = append(*problems, fmt.Sprintf("%s: %q is no value the contract declares", at, text))
		}
	case "integer":
		if n, ok := v.(json.Number); !ok || strings.ContainsAny(string(n), ".eE") {
			*problems = append(*problems, at+": not an integer")
		}
	}
}

// answer is one reply of the fake estimate endpoints.
type answer struct {
	status int
	body   string
}

func receipt(state, planned string) answer {
	body := `{"id":"` + estimateID + `","state":"` + state + `"`
	if planned != "" {
		body += `,"planned":` + planned
	}
	return answer{status: http.StatusOK, body: body + "}"}
}

func planned(change string) string {
	return `{"monthlyCostBefore":"100.00000000","monthlyCostAfter":"600.00000000","monthlyChange":"` + change + `","unpricedResources":1}`
}

// estimateAPI is the fake API with the two estimate endpoints, answering each post with the next
// answer in turn. It holds every request body and every answer to the contract.
type estimateAPI struct {
	*fakeAPI
	env map[string]string

	mu      sync.Mutex
	answers []answer
	posted  map[string][][]byte
}

func newEstimateAPI(t *testing.T, answers ...answer) *estimateAPI {
	t.Helper()
	spec := loadOpenAPI(t)
	fake, srv := newFakeAPI(t, map[string]token{
		tokenSecret:   {name: "ci", capabilities: []costfluent.Capability{costfluent.CapabilitySubmitCostEstimates}},
		"cfk_ws_read": {name: "read", capabilities: catalogCapabilities()},
	})
	api := &estimateAPI{fakeAPI: fake, answers: answers, posted: map[string][][]byte{},
		env: map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": srv.URL}}
	requests := map[string]string{"/v1/cost-estimates": "SubmitCostEstimateRequest", "/v1/cost-estimates/apply": "ApplyCostEstimateRequest"}
	fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		schema, ok := requests[r.URL.Path]
		if !ok || r.Method != http.MethodPost {
			return false
		}
		secret, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !slices.Contains(fake.tokens[secret].capabilities, costfluent.CapabilitySubmitCostEstimates) {
			problem(w, http.StatusForbidden, map[string]any{"code": "forbidden", "detail": "API token lacks capability: submitCostEstimates"})
			return true
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, p := range spec.check(schema, body) {
			t.Errorf("POST %s breaks %s: %s", r.URL.Path, schema, p)
		}
		api.mu.Lock()
		api.posted[r.URL.Path] = append(api.posted[r.URL.Path], body)
		if len(api.answers) == 0 {
			api.mu.Unlock()
			t.Errorf("POST %s: no answer left", r.URL.Path)
			problem(w, http.StatusInternalServerError, map[string]any{"detail": "no answer"})
			return true
		}
		a := api.answers[0]
		api.answers = api.answers[1:]
		api.mu.Unlock()
		if a.status >= 400 {
			for _, p := range spec.check("ApiError", []byte(a.body)) {
				t.Errorf("the fake's %d answer breaks ApiError: %s", a.status, p)
			}
			w.Header().Set("Content-Type", "application/problem+json")
		} else {
			for _, p := range spec.check("CostEstimateReceiptResponse", []byte(a.body)) {
				t.Errorf("the fake's receipt breaks CostEstimateReceiptResponse: %s", p)
			}
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
		return true
	}
	return api
}

func (api *estimateAPI) bodies(path string) [][]byte {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.posted[path]
}

func fixturePath(name string) string { return filepath.Join(estimateFixtures, name) }

// submitArgs posts the Action's golden case: Infracost v2's diff with usage and its plan.
func submitArgs(extra ...string) []string {
	return append([]string{"estimate", "submit", "--workspace-id", estimateWorkspace, "--repository", "acme/infra",
		"--pull-request", "42", "--infracost-json", fixturePath("infracost-v2-diff-usage.json"),
		"--terraform-plan-json", fixturePath("terraform-plan.json"),
		"--commit", "9fceb02d0ae598e95dc970b74767f19372d61af8", "--url", "https://github.com/acme/infra/pull/42",
		"--infracost-version", "0.10.41"}, extra...)
}

func applyArgs(plan, state string, extra ...string) []string {
	return append([]string{"estimate", "apply", "--workspace-id", estimateWorkspace, "--repository", "acme/infra",
		"--pull-request", "42", "--terraform-plan-json", plan, "--terraform-state-json", state}, extra...)
}

// actionGolden is the Action's golden output as the SDK request posts it: the SDK leaves out a null
// cost and an empty id list the Action sends, which the contract reads alike.
func actionGolden[T any](t *testing.T, name string, member string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(estimateFixtures, "expected", name))
	if err != nil {
		t.Fatal(err)
	}
	if member != "" {
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			t.Fatal(err)
		}
		raw = outer[member]
	}
	sent, err := json.Marshal(decoded[T](t, raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded[T](t, sent)
}

func decoded[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

// writeFile writes content into the test's directory and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEstimateHelpNeedsNoToken(t *testing.T) {
	for _, args := range [][]string{{"help", "estimate"}, {"estimate", "--help"}, {"estimate", "help"},
		{"estimate", "submit", "--help"}, {"estimate", "apply", "-h"}} {
		res := invoke(t, map[string]string{}, false, args...)
		if res.code != exitOK || !strings.Contains(res.stdout, "costfluent estimate submit") || res.stderr != "" {
			t.Errorf("costfluent %v: exit %d, stdout %q, stderr %q", args, res.code, res.stdout, res.stderr)
		}
	}
	usage := invoke(t, nil, false, "--help")
	for _, want := range []string{"estimate submit", "estimate apply", "4 a\nthreshold you set was exceeded"} {
		if !strings.Contains(usage.stdout, want) {
			t.Errorf("costfluent --help does not say %q:\n%s", want, usage.stdout)
		}
	}
}

func TestEstimateRefusesAWrongCommandLineBeforeAnyRequest(t *testing.T) {
	notJSON := writeFile(t, "infracost.json", "Infracost failed")
	notObject := writeFile(t, "list.json", "[]")
	missing := filepath.Join(t.TempDir(), "absent.json")
	base := func(flags ...string) []string {
		return append([]string{"estimate", "submit", "--workspace-id", estimateWorkspace, "--repository", "acme/infra",
			"--pull-request", "42", "--infracost-json", fixturePath("infracost-v2-diff.json")}, flags...)
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"estimate"}, "estimate takes a subcommand, submit or apply"},
		{[]string{"estimate", "post"}, `estimate takes submit or apply, not "post"`},
		{[]string{"estimate", "submit"}, "missing --workspace-id, --repository, --pull-request, --infracost-json"},
		{[]string{"estimate", "apply", "--workspace-id", estimateWorkspace},
			"missing --repository, --pull-request, --terraform-plan-json, --terraform-state-json"},
		{base("--repository", " "), "missing --repository"},
		{base("--pull-request", "0"), `--pull-request takes a pull request number, not "0"`},
		{base("--pull-request", "4x"), `--pull-request takes a pull request number, not "4x"`},
		{base("--pull-request", "2147483648"), `--pull-request takes a pull request number, not "2147483648"`},
		{base("--url", "http://github.com/acme/infra/pull/42"), "--url must start with https://"},
		{base("--fail-above", "-1"), `--fail-above takes a non-negative amount such as 500 or 99.50, not "-1"`},
		{base("--fail-above", "1e3"), `not "1e3"`},
		{base("--fail-above", "5."), `not "5."`},
		{base("--terraform-state-json", "state.json"), "unknown flag --terraform-state-json"},
		{base("--token", "x"), "unknown flag --token"},
		{base("extra"), `unexpected argument "extra"`},
		{base("--output", "xml"), `--output takes json or table, not "xml"`},
		{applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json"), "--fail-above", "5"),
			"unknown flag --fail-above"},
		{base("--infracost-json", missing), "--infracost-json: open " + missing},
		{base("--infracost-json", notJSON), "--infracost-json (" + notJSON + ") is not JSON"},
		{base("--infracost-json", notObject), "--infracost-json (" + notObject + ") is not a JSON object"},
		{base("--terraform-plan-json", notJSON), "--terraform-plan-json (" + notJSON + ") is not JSON"},
		{applyArgs(fixturePath("terraform-apply-plan.json"), notObject), "--terraform-state-json (" + notObject + ") is not a JSON object"},
		{applyArgs(missing, fixturePath("terraform-apply-state.json")), "--terraform-plan-json: open " + missing},
	}
	api := newEstimateAPI(t)
	for _, c := range cases {
		res := invoke(t, api.env, false, c.args...)
		if res.code != exitUsage || !strings.Contains(res.stderr, c.want) {
			t.Errorf("costfluent %v: exit %d, stderr %q; want exit 2 naming %q", c.args, res.code, res.stderr, c.want)
		}
	}
	if n := api.count(); n != 0 {
		t.Errorf("a refused command line sent %d requests", n)
	}
}

func TestEstimateSubmitCreatesThenUpdatesThePullRequestsEstimate(t *testing.T) {
	created := receipt("proposed", planned("500.00000000"))
	created.status = http.StatusCreated
	api := newEstimateAPI(t, created, receipt("proposed", planned("450.12345678")))

	first := invoke(t, api.env, false, submitArgs()...)
	if first.code != exitOK || !strings.Contains(first.stderr, "Posted the cost estimate "+estimateID+" for acme/infra#42: 4 resources, created.") {
		t.Fatalf("first submit: exit %d, stderr %q", first.code, first.stderr)
	}
	got := decoded[costfluent.CostEstimateReceipt](t, []byte(first.stdout))
	if got.ID != estimateID || got.State != costfluent.CostEstimateStateProposed || got.Planned == nil || got.Planned.MonthlyChange != "500.00000000" {
		t.Errorf("first submit printed %s", first.stdout)
	}

	second := invoke(t, api.env, true, submitArgs()...)
	if second.code != exitOK || !strings.HasSuffix(second.stderr, "4 resources, updated.\n") {
		t.Fatalf("second submit: exit %d, stderr %q", second.code, second.stderr)
	}
	for _, want := range []string{"id:", estimateID, "planned.monthlyChange:", "450.12345678", "planned.unpricedResources:"} {
		if !strings.Contains(second.stdout, want) {
			t.Errorf("the table on a terminal lacks %q:\n%s", want, second.stdout)
		}
	}

	want := actionGolden[costfluent.SubmitCostEstimateInput](t, "infracost-v2-diff-usage+terraform-plan.json", "")
	bodies := api.bodies("/v1/cost-estimates")
	if len(bodies) != 2 {
		t.Fatalf("%d posts, want 2", len(bodies))
	}
	for _, body := range bodies {
		if posted := decoded[costfluent.SubmitCostEstimateInput](t, body); !reflect.DeepEqual(posted, want) {
			t.Errorf("posted %s\nwant the Action's golden estimate %+v", body, want)
		}
	}
}

func TestEstimateSubmitPostsNothingWhenNoPricedResourceChanges(t *testing.T) {
	api := newEstimateAPI(t)
	res := invoke(t, api.env, false, "estimate", "submit", "--workspace-id", estimateWorkspace, "--repository", "acme/infra",
		"--pull-request", "42", "--infracost-json", fixturePath("infracost-v0.10-unchanged.json"), "--fail-above", "0")
	if res.code != exitOK || res.stdout != "" || !strings.Contains(res.stderr, "nothing to post") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 0, nothing printed and the reason", res.code, res.stdout, res.stderr)
	}
	if n := api.count(); n != 0 {
		t.Errorf("an unchanged diff sent %d requests", n)
	}
}

func TestEstimateFailAboveComparesExactAmounts(t *testing.T) {
	cases := []struct {
		change, limit string
		want          int
	}{
		{"500.00000000", "500", exitOK},
		{"500.00000001", "500", exitThreshold},
		{"500", "499.99999999", exitThreshold},
		{"0.1", "0.10", exitOK},
		{"-1200.50000000", "0", exitOK},
		{"12345678901234567890.00000002", "12345678901234567890.00000001", exitThreshold},
		{"12345678901234567890.00000001", "12345678901234567890.00000002", exitOK},
	}
	for _, c := range cases {
		api := newEstimateAPI(t, receipt("proposed", planned(c.change)))
		res := invoke(t, api.env, false, submitArgs("--fail-above", c.limit)...)
		if res.code != c.want {
			t.Errorf("change %s, --fail-above %s: exit %d, want %d; stderr %q", c.change, c.limit, res.code, c.want, res.stderr)
		}
		if !strings.Contains(res.stdout, `"monthlyChange": "`+c.change+`"`) {
			t.Errorf("change %s, --fail-above %s: the receipt is not printed: %q", c.change, c.limit, res.stdout)
		}
		wantNote := "is not above --fail-above, " + c.limit + " USD."
		if c.want == exitThreshold {
			wantNote = "costfluent: the planned monthly change, " + c.change + " USD, is above --fail-above, " + c.limit + " USD"
		}
		if !strings.Contains(res.stderr, wantNote) {
			t.Errorf("change %s, --fail-above %s: stderr %q lacks %q", c.change, c.limit, res.stderr, wantNote)
		}
	}
}

func TestEstimateFailAboveIsNotAppliedWithoutPlannedTotals(t *testing.T) {
	api := newEstimateAPI(t, receipt("proposed", ""))
	res := invoke(t, api.env, false, submitArgs("--fail-above", "0")...)
	if res.code != exitOK || !strings.Contains(res.stderr, "warning: --fail-above is not applied") || !strings.Contains(res.stdout, estimateID) {
		t.Errorf("a receipt without planned: exit %d, stdout %q, stderr %q; want exit 0, the receipt and a warning", res.code, res.stdout, res.stderr)
	}
}

func TestEstimateApplyPostsTheIdsTheStateGives(t *testing.T) {
	api := newEstimateAPI(t, receipt("applied", planned("500.00000000")))
	res := invoke(t, api.env, false, applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json"))...)
	if res.code != exitOK || !strings.Contains(res.stderr, "Recorded the apply of the cost estimate "+estimateID+" for acme/infra#42: 3 created") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if got := decoded[costfluent.CostEstimateReceipt](t, []byte(res.stdout)); got.State != costfluent.CostEstimateStateApplied {
		t.Errorf("printed %s", res.stdout)
	}
	want := actionGolden[costfluent.ApplyCostEstimateInput](t, "terraform-apply-plan.json", "body")
	bodies := api.bodies("/v1/cost-estimates/apply")
	if len(bodies) != 1 {
		t.Fatalf("%d applies, want 1", len(bodies))
	}
	if posted := decoded[costfluent.ApplyCostEstimateInput](t, bodies[0]); !reflect.DeepEqual(posted, want) {
		t.Errorf("posted %s\nwant the Action's golden apply %+v", bodies[0], want)
	}
}

func TestEstimateApplyWarnsOfAddressesTheStateHoldsNoIdFor(t *testing.T) {
	api := newEstimateAPI(t, receipt("applied", ""))
	state := writeFile(t, "state.json", `{"format_version":"1.0","values":{"root_module":{}}}`)
	res := invoke(t, api.env, false, applyArgs(fixturePath("terraform-apply-plan.json"), state)...)
	if res.code != exitOK {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	for _, address := range []string{"aws_default_subnet.default", "aws_instance.ec2", "aws_volume_attachment.storage_attachment"} {
		if !strings.Contains(res.stderr, "warning: "+address+" is created or replaced by the plan, but the state holds no id for it; it is left out\n") {
			t.Errorf("no warning for %s: %q", address, res.stderr)
		}
	}
	if posted := decoded[costfluent.ApplyCostEstimateInput](t, api.bodies("/v1/cost-estimates/apply")[0]); len(posted.Resources) != 0 {
		t.Errorf("posted %d resources without ids", len(posted.Resources))
	}
}

func TestEstimateApplyWithNoEstimateHasNothingToApply(t *testing.T) {
	api := newEstimateAPI(t, answer{status: http.StatusNotFound,
		body: `{"type":"about:blank","status":404,"retryable":false,"code":"not_found","title":"Not Found","detail":"No cost estimate matches the reference"}`})
	res := invoke(t, api.env, false, applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json"))...)
	if res.code != exitOK || res.stdout != "" ||
		!strings.Contains(res.stderr, "No cost estimate for acme/infra#42 in this workspace: nothing to apply (No cost estimate matches the reference).") {
		t.Errorf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
}

// A 404 whose code is not not_found says the API serves no such route, not that no estimate exists,
// so the apply went unrecorded and the run fails.
func TestEstimateApplyOnARouteTheAPIDoesNotServeFails(t *testing.T) {
	api := newEstimateAPI(t, answer{status: http.StatusNotFound,
		body: `{"type":"about:blank","status":404,"retryable":false,"code":"operation_not_found","title":"Not Found","detail":"No operation matches the request"}`})
	res := invoke(t, api.env, false, applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json"))...)
	if res.code != exitFailed || res.stdout != "" || !strings.Contains(res.stderr, "operation_not_found") ||
		strings.Contains(res.stderr, "nothing to apply") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and the API's operation_not_found", res.code, res.stdout, res.stderr, exitFailed)
	}
	if n := len(api.bodies("/v1/cost-estimates/apply")); n != 1 {
		t.Errorf("%d applies, want 1", n)
	}
}

func TestEstimateApplyRefusesMoreResourcesThanOneApplyTakes(t *testing.T) {
	var changes, resources []string
	for i := range maxAppliedResources + 1 {
		address := fmt.Sprintf("aws_eip.ip[%d]", i)
		changes = append(changes, `{"address":"`+address+`","mode":"managed","change":{"actions":["create"]}}`)
		resources = append(resources, fmt.Sprintf(`{"address":"%s","mode":"managed","values":{"id":"eipalloc-%d"}}`, address, i))
	}
	plan := writeFile(t, "plan.json", `{"format_version":"1.2","resource_changes":[`+strings.Join(changes, ",")+`]}`)
	state := writeFile(t, "state.json", `{"format_version":"1.0","values":{"root_module":{"resources":[`+strings.Join(resources, ",")+`]}}}`)
	api := newEstimateAPI(t)
	res := invoke(t, api.env, false, applyArgs(plan, state)...)
	if res.code != exitFailed || !strings.Contains(res.stderr, "the plan creates or replaces 2001 resources with ids, more than the 2000 one apply takes") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
	if n := api.count(); n != 0 {
		t.Errorf("sent %d requests", n)
	}
}

func TestEstimateExitStatuses(t *testing.T) {
	conflict := answer{status: http.StatusConflict,
		body: `{"type":"about:blank","status":409,"retryable":false,"code":"conflict","title":"Conflict","detail":"The estimate was applied on 2026-11-01 and can no longer be replaced"}`}
	api := newEstimateAPI(t, conflict)
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want int
		says string
	}{
		{"no token", map[string]string{}, submitArgs(), exitAccess, "COSTFLUENT_API_TOKEN is not set"},
		{"a base URL in clear", map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": "http://api.example.com"},
			submitArgs(), exitAccess, "must use https"},
		{"a token the API refuses", map[string]string{"COSTFLUENT_API_TOKEN": "cfk_unknown", "COSTFLUENT_BASE_URL": api.env["COSTFLUENT_BASE_URL"]},
			submitArgs(), exitAccess, `"status":401`},
		{"a token without submitCostEstimates", map[string]string{"COSTFLUENT_API_TOKEN": "cfk_ws_read", "COSTFLUENT_BASE_URL": api.env["COSTFLUENT_BASE_URL"]},
			applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json")), exitAccess, "lacks capability: submitCostEstimates"},
		{"a refused post", api.env, submitArgs(), exitFailed, "can no longer be replaced"},
		{"an Infracost project that failed", api.env, []string{"estimate", "submit", "--workspace-id", estimateWorkspace,
			"--repository", "acme/infra", "--pull-request", "42", "--infracost-json", fixturePath("infracost-v0.10-project-error.json")},
			exitFailed, "failed in Infracost"},
		{"a state given as the plan", api.env, applyArgs(fixturePath("terraform-apply-state.json"), fixturePath("terraform-apply-state.json")),
			exitFailed, "is not `terraform show -json` of a saved plan"},
		{"a plan given as the state", api.env, applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-plan.json")),
			exitFailed, "is not `terraform show -json` of the state"},
	}
	for _, c := range cases {
		res := invoke(t, c.env, false, c.args...)
		if res.code != c.want || !strings.Contains(res.stderr, c.says) {
			t.Errorf("%s: exit %d, stderr %q; want exit %d saying %q", c.name, res.code, res.stderr, c.want, c.says)
		}
		if strings.Contains(res.stderr+res.stdout, tokenSecret) {
			t.Errorf("%s: the token was printed", c.name)
		}
	}
}

func TestEstimateRequestsCarryNoAgentMarker(t *testing.T) {
	api := newEstimateAPI(t, receipt("proposed", planned("1")), receipt("applied", planned("1")))
	if res := invoke(t, api.env, false, submitArgs()...); res.code != exitOK {
		t.Fatalf("submit: exit %d, %s", res.code, res.stderr)
	}
	if res := invoke(t, api.env, false, applyArgs(fixturePath("terraform-apply-plan.json"), fixturePath("terraform-apply-state.json"))...); res.code != exitOK {
		t.Fatalf("apply: exit %d, %s", res.code, res.stderr)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.requests) != 2 {
		t.Fatalf("%d requests, want the submit and the apply", len(api.requests))
	}
	for _, r := range api.requests {
		if _, ok := r.Header[http.CanonicalHeaderKey(agentHeader)]; ok {
			t.Errorf("%s %s carries %s: %q", r.Method, r.URL.Path, agentHeader, r.Header.Get(agentHeader))
		}
		if r.Header.Get("Authorization") != "Bearer "+tokenSecret {
			t.Errorf("%s %s is not sent with the environment's token", r.Method, r.URL.Path)
		}
	}
}

func TestOpenAPICheckRefusesABodyThatBreaksTheContract(t *testing.T) {
	spec := loadOpenAPI(t)
	for body, want := range map[string]string{
		`{"id":"est_1","state":"proposed"}`:                                               `"est_1" does not match`,
		`{"id":"` + estimateID + `"}`:                                                     "no required state",
		`{"id":"` + estimateID + `","state":"proposed","extra":1}`:                        "extra is no member",
		`{"id":"` + estimateID + `","state":"lost"}`:                                      `"lost" is no value`,
		`{"id":"` + estimateID + `","state":"proposed","planned":{"a":1}}`:                "matches none of oneOf",
		`{"id":null,"state":"proposed"}`:                                                  "null where the contract takes no null",
		`{"id":"` + estimateID + `","state":"proposed","planned":[]}`:                     "matches none of oneOf",
		`{"id":"` + estimateID + `","state":"proposed","planned":` + planned("1.5") + `}`: "",
	} {
		problems := strings.Join(spec.check("CostEstimateReceiptResponse", []byte(body)), "; ")
		if (want == "") != (problems == "") || !strings.Contains(problems, want) {
			t.Errorf("%s: %q, want %q", body, problems, want)
		}
	}
	if problems := spec.check("ApplyCostEstimateRequest", []byte(`{"workspaceId":"`+estimateWorkspace+`","pullRequest":4.5,"resources":{}}`)); len(problems) != 2 {
		t.Errorf("a fractional pull request and an object for a list: %q", problems)
	}
}
