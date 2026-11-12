package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// result is one run of the CLI.
type result struct {
	code           int
	stdout, stderr string
}

// invoke runs the CLI with args against env, its streams captured; tty sets both streams as a
// terminal or not.
func invoke(t *testing.T, env map[string]string, tty bool, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &cli{
		args: args, getenv: func(k string) string { return env[k] },
		stdout: &stdout, stderr: &stderr, stdoutTTY: tty, stderrTTY: tty,
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// catalogCapabilities is every capability a tool needs.
func catalogCapabilities() []costfluent.Capability {
	var caps []costfluent.Capability
	for _, e := range server.Catalog() {
		if !slices.Contains(caps, e.Capability) {
			caps = append(caps, e.Capability)
		}
	}
	return caps
}

// fullAPI is a fake API for a token carrying every capability the tools need, in an organization
// that lets AI agents write, and its environment.
func fullAPI(t *testing.T) (*fakeAPI, map[string]string) {
	// create-dashboard and add-dashboard-widget read the dashboard back, which needs viewDashboards,
	// a capability no tool is listed for.
	caps := append(catalogCapabilities(), costfluent.CapabilityViewDashboards)
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {name: "cli", capabilities: caps, agentWrites: true}})
	return api, map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": srv.URL}
}

func TestEveryToolIsACommand(t *testing.T) {
	commands, err := loadCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	usage := invoke(t, nil, false, "--help")
	if usage.code != exitOK {
		t.Fatalf("--help exited %d: %s", usage.code, usage.stderr)
	}
	for _, e := range server.Catalog() {
		name := strings.ReplaceAll(e.Name, "_", "-")
		cmd := find(commands, name)
		if cmd == nil {
			t.Errorf("the tool %s has no command %s", e.Name, name)
			continue
		}
		if cmd.capability != e.Capability {
			t.Errorf("%s needs %s, the catalog says %s", name, cmd.capability, e.Capability)
		}
		if !strings.Contains(usage.stdout, "  "+name+" ") {
			t.Errorf("costfluent --help does not list %s:\n%s", name, usage.stdout)
		}
		help := invoke(t, nil, false, name, "--help")
		if help.code != exitOK {
			t.Errorf("%s --help exited %d: %s", name, help.code, help.stderr)
		}
		raw, _ := json.Marshal(cmd.tool.InputSchema)
		var schema jsonSchema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for property := range schema.Properties {
			flagName := "--" + strings.ReplaceAll(property, "_", "-")
			if !strings.Contains(help.stdout, "  "+flagName+" ") {
				t.Errorf("%s --help lists no %s for the property %s:\n%s", name, flagName, property, help.stdout)
			}
		}
	}
	if len(commands) != len(server.Catalog()) {
		t.Errorf("%d commands for %d tools", len(commands), len(server.Catalog()))
	}
}

func TestHelpNeedsNoToken(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"help", "query-costs"}, {"query-costs", "--help"}, {"query-costs", "-h"}, {"--version"},
		{"help", "create-budget"}, {"create-budget", "--help"},
		{"help", "whoami"}, {"help", "mcp"}, {"help", "mcp", "install"}, {"whoami", "--help"}, {"mcp", "install", "--help"}} {
		res := invoke(t, map[string]string{}, false, args...)
		if res.code != exitOK || res.stdout == "" || res.stderr != "" {
			t.Errorf("costfluent %v: exit %d, stdout %q, stderr %q", args, res.code, res.stdout, res.stderr)
		}
	}
	if res := invoke(t, nil, false); res.code != exitUsage || !strings.Contains(res.stderr, "Commands:") {
		t.Errorf("no arguments: exit %d, stderr %q; want the usage and exit 2", res.code, res.stderr)
	}
	if res := invoke(t, nil, false, "--version"); strings.TrimSpace(res.stdout) != version {
		t.Errorf("--version printed %q, want %q", res.stdout, version)
	}
}

// calls is one call per tool: its command line, the path it reads and a value its JSON holds.
var calls = []struct {
	args []string
	path string
	want string
}{
	{[]string{"list-workspaces"}, "/v1/workspaces", `"id": "ws_own"`},
	{[]string{"query-costs", "--start-date", "2026-10-01", "--end-date=2026-10-31", "--group-by", "ServiceName", "--group-by", "Region", "--amortize"},
		"/v1/costs", `"cost": "12345678901234.12345678"`},
	{[]string{"list-cost-reports"}, "/v1/cost-reports", `"title": "Monthly"`},
	{[]string{"get-cost-report-data", "--cost-report-id", reportID}, "/v1/cost-reports/" + reportID + "/data", `"totalCost": "99.00000000"`},
	{[]string{"list-annotations", "--include-resolved"}, "/v1/annotations", `"text": "deploy"`},
	{[]string{"list-budgets"}, "/v1/budgets", `"name": "Production"`},
	{[]string{"list-anomalies", "--severity", "high"}, "/v1/anomalies", `"serviceName": "AmazonEC2"`},
	{[]string{"list-savings-items", "--sort", "largestSaving"}, "/v1/savings-items", `"title": "Idle volume"`},
	{[]string{"list-commitments", "--term-years", "1"}, "/v1/commitments", `"id": "ri-0abc"`},
	{[]string{"get-commitment-coverage", "--start-date", "2026-10-01", "--end-date", "2026-10-31"}, "/v1/commitments/coverage", `"coveragePercent": 80`},
	{[]string{"get-kubernetes-efficiency", "--start-date", "2026-10-01", "--end-date", "2026-10-31", "--level", "cluster"},
		"/v1/kubernetes/efficiency", `"cluster": "prod"`},
	{[]string{"create-annotation", "--date", "2026-11-02", "--text", "deploy", "--cost-report-ids", reportID}, "/v1/annotations", `"id": "ann_new"`},
	{[]string{"create-cost-report", "--title", "EC2", "--amortize"}, "/v1/cost-reports", `"id": "rpt_new"`},
	{[]string{"update-cost-report", "--cost-report-id", reportID, "--title", "Renamed"}, "/v1/cost-reports/" + reportID, `"title": "Renamed"`},
	{[]string{"create-budget", "--name", "Platform", "--currency", "USD", "--amount", "5000", "--alert-thresholds", "80"}, "/v1/budgets", `"id": "bud_new"`},
	{[]string{"update-budget", "--budget-id", "bud_1", "--name", "Renamed"}, "/v1/budgets/bud_1", `"name": "Renamed"`},
	{[]string{"create-cost-alert", "--name", "EC2", "--threshold-type", "absolute", "--threshold-value", "1000"}, "/v1/cost-alerts", `"id": "cal_new"`},
	{[]string{"update-cost-alert", "--cost-alert-id", "cal_1", "--threshold-value", "2000"}, "/v1/cost-alerts/cal_1", `"thresholdValue": "2000.00000000"`},
	{[]string{"pause-cost-alert", "--cost-alert-id", "cal_1"}, "/v1/cost-alerts/cal_1/pause", `"status": "paused"`},
	{[]string{"resume-cost-alert", "--cost-alert-id", "cal_1"}, "/v1/cost-alerts/cal_1/resume", `"status": "active"`},
	{[]string{"create-dashboard", "--title", "Platform"}, "/v1/dashboards", `"id": "dsh_new"`},
	{[]string{"add-dashboard-widget", "--dashboard-id", "dsh_1", "--title", "Monthly", "--cost-report-id", reportID}, "/v1/dashboards/dsh_1/widgets", `"id": "wdg_new"`},
	{[]string{"acknowledge-anomaly", "--anomaly-id", "anm_1"}, "/v1/anomalies/anm_1/acknowledge", `"status": "acknowledged"`},
	{[]string{"set-savings-item-status", "--savings-item-id", "sav_1", "--status", "rejected", "--reason", "Reserved capacity covers it"},
		"/v1/savings-items/sav_1/reject", `"status": "rejected"`},
	{[]string{"assign-savings-item-owner", "--savings-item-id", "sav_1", "--owner", "Platform team"}, "/v1/savings-items/sav_1/owner", `"owner": "Platform team"`},
}

func TestEachCommandCallsItsTool(t *testing.T) {
	var covered []string
	for _, c := range calls {
		covered = append(covered, c.args[0])
	}
	for _, e := range server.Catalog() {
		// The docs commands call no API; docs_test.go covers them.
		if e.Capability != server.NoCapability && !slices.Contains(covered, commandName(e.Name)) {
			t.Errorf("no call case for %s", commandName(e.Name))
		}
	}
	for _, c := range calls {
		t.Run(c.args[0], func(t *testing.T) {
			api, env := fullAPI(t)
			res := invoke(t, env, false, c.args...)
			if res.code != exitOK {
				t.Fatalf("exit %d: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, c.want) {
				t.Errorf("stdout lacks %s:\n%s", c.want, res.stdout)
			}
			if !json.Valid([]byte(res.stdout)) {
				t.Errorf("stdout off a terminal is not JSON:\n%s", res.stdout)
			}
			r := api.last(c.path)
			if r == nil {
				t.Fatalf("no request for %s", c.path)
			}
			if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "costfluent/"+version) {
				t.Errorf("User-Agent %q, want costfluent/%s", ua, version)
			}
			// A tool's request carries the agent marker, so the API refuses its writes while the
			// organization does not let AI agents write.
			if marker := r.Header.Get(costfluent.AgentHeader); marker != server.AgentName+"/"+version {
				t.Errorf("%s %q, want %s/%s", costfluent.AgentHeader, marker, server.AgentName, version)
			}
		})
	}
}

func TestFlagsReachTheAPI(t *testing.T) {
	api, env := fullAPI(t)
	res := invoke(t, env, false, calls[1].args...)
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	q := api.query(t, "/v1/costs")
	if got := q["groupBy"]; !slices.Equal(got, []string{"ServiceName", "Region"}) {
		t.Errorf("groupBy %v, want both, in order", got)
	}
	if q.Get("amortize") != "true" || q.Get("startDate") != "2026-10-01" || q.Get("endDate") != "2026-10-31" {
		t.Errorf("query %v", q)
	}

	// A nullable boolean left out is not sent, so the API applies its own default.
	if res := invoke(t, env, false, "query-costs", "--start-date", "2026-10-01", "--end-date", "2026-10-31"); res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if q := api.query(t, "/v1/costs"); q.Has("amortize") {
		t.Errorf("amortize sent unasked: %v", q)
	}
	if res := invoke(t, env, false, "query-costs", "--start-date", "2026-10-01", "--end-date", "2026-10-31", "--amortize=false"); res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if q := api.query(t, "/v1/costs"); q.Get("amortize") != "false" {
		t.Errorf("--amortize=false sent %v", q)
	}
}

func TestATableOnATerminal(t *testing.T) {
	_, env := fullAPI(t)
	res := invoke(t, env, true, calls[1].args...)
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	for _, want := range []string{"rows:", "dimensions.ServiceName", "12345678901234.12345678", "meta.totalCost:", "next page: 2"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, res.stdout)
		}
	}
	if json.Valid([]byte(res.stdout)) {
		t.Errorf("a terminal got JSON:\n%s", res.stdout)
	}

	// --output overrides the terminal both ways.
	if res := invoke(t, env, true, "list-budgets", "--output", "json"); !json.Valid([]byte(res.stdout)) {
		t.Errorf("--output json on a terminal printed:\n%s", res.stdout)
	}
	if res := invoke(t, env, false, "list-budgets", "--output=table"); !strings.Contains(res.stdout, "items:") {
		t.Errorf("--output table off a terminal printed:\n%s", res.stdout)
	}

	// A customer-written name reaches the terminal escaped.
	res = invoke(t, env, true, "list-workspaces")
	if strings.Contains(res.stdout, "\x1b") || !strings.Contains(res.stdout, `Prod\u001b[31m`) {
		t.Errorf("the control character was not escaped:\n%q", res.stdout)
	}
}

func TestUsageErrorsExit2BeforeAnyCall(t *testing.T) {
	api, env := fullAPI(t)
	for name, args := range map[string][]string{
		"unknown command":    {"list-everything"},
		"unknown flag":       {"list-budgets", "--color"},
		"bad integer":        {"list-budgets", "--page", "two"},
		"bad boolean":        {"list-annotations", "--include-resolved=maybe"},
		"missing required":   {"query-costs", "--start-date", "2026-10-01"},
		"positional":         {"list-budgets", "ws_own"},
		"bad output":         {"list-budgets", "--output", "yaml"},
		"help of no command": {"help", "nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			res := invoke(t, env, false, args...)
			if res.code != exitUsage {
				t.Errorf("exit %d, want 2; stderr %q", res.code, res.stderr)
			}
			if res.stdout != "" || !strings.HasPrefix(res.stderr, "costfluent: ") {
				t.Errorf("stdout %q, stderr %q", res.stdout, res.stderr)
			}
		})
	}
	if n := api.count(); n != 0 {
		t.Errorf("a wrong command line reached the API %d times", n)
	}
}

func TestAccessFailuresExit3(t *testing.T) {
	narrow := "cfk_ws_narrow-token-never-printed"
	api, srv := newFakeAPI(t, map[string]token{
		tokenSecret: {name: "cli", capabilities: catalogCapabilities()},
		narrow:      {name: "budgets only", capabilities: []costfluent.Capability{costfluent.CapabilityViewBudgets}},
	})
	full := map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": srv.URL}
	narrowEnv := map[string]string{"COSTFLUENT_API_TOKEN": narrow, "COSTFLUENT_BASE_URL": srv.URL}
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"no token":             {map[string]string{}, []string{"list-budgets"}, "COSTFLUENT_API_TOKEN is not set: create an API token under Settings, API tokens, and set it in the environment the MCP server or the costfluent command runs in"},
		"plain http":           {map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": "http://api.costfluent.com"}, []string{"list-budgets"}, "COSTFLUENT_BASE_URL"},
		"refused token":        {map[string]string{"COSTFLUENT_API_TOKEN": "cfk_ws_wrong", "COSTFLUENT_BASE_URL": srv.URL}, []string{"list-budgets"}, `"status":401`},
		"missing capability":   {map[string]string{"COSTFLUENT_API_TOKEN": narrow, "COSTFLUENT_BASE_URL": srv.URL}, []string{"list-anomalies"}, "viewAnomalies"},
		"forbidden by the API": {map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": srv.URL}, []string{"list-budgets", "--workspace-id", "ws_other"}, `"status":403`},
		"write capability missing": {narrowEnv, []string{"create-budget", "--name", "x", "--currency", "USD", "--amount", "1"},
			"lacks the createBudget capability create-budget needs; create one under Settings, API tokens carrying it"},
		// The token carries createBudget, but its organization does not let AI agents write.
		"writes not allowed": {full, []string{"create-budget", "--name", "x", "--currency", "USD", "--amount", "1"},
			"create-budget creates or changes objects, which this organization does not let AI agents do; an Owner can turn on " + server.AgentWritesSetting},
		// The setting went off after the command read /v1/me: the API refuses the marked write.
		"writes refused by the API": {full, []string{"create-budget", "--name", "x", "--currency", "USD", "--amount", "1"}, "Settings, Organization"},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "writes refused by the API" {
				api.override = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method == http.MethodGet {
						return false
					}
					problem(w, http.StatusForbidden, map[string]any{"code": "agent_writes_disabled", "title": "Forbidden",
						"detail": "refused", "retryable": false, "traceId": "trace-403"})
					return true
				}
				api.mu.Lock()
				tok := api.tokens[tokenSecret]
				tok.agentWrites = true
				api.tokens[tokenSecret] = tok
				api.mu.Unlock()
				defer func() {
					api.override = nil
					tok.agentWrites = false
					api.tokens[tokenSecret] = tok
				}()
			}
			if name == "forbidden by the API" {
				api.override = func(w http.ResponseWriter, r *http.Request) bool {
					problem(w, http.StatusForbidden, map[string]any{"code": "forbidden", "detail": "Workspace token cannot access other workspaces"})
					return true
				}
				defer func() { api.override = nil }()
			}
			before := api.count()
			res := invoke(t, tc.env, false, tc.args...)
			if res.code != exitAccess {
				t.Errorf("exit %d, want 3; stderr %q", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.want) || res.stdout != "" {
				t.Errorf("stderr %q lacks %q, or stdout %q", res.stderr, tc.want, res.stdout)
			}
			if name == "writes not allowed" || name == "write capability missing" {
				if api.count() != before+1 {
					t.Errorf("%d requests, want only GET /v1/me", api.count()-before)
				}
			}
			if name == "missing capability" {
				if r := api.last("/v1/anomalies"); r != nil {
					t.Error("a tool the token is not served was called")
				}
				if api.count() != before+1 {
					t.Errorf("%d requests, want only GET /v1/me", api.count()-before)
				}
			}
		})
	}
}

func TestAnAPIRefusalExits1WithTheAPIsError(t *testing.T) {
	api, env := fullAPI(t)
	api.override = func(w http.ResponseWriter, r *http.Request) bool {
		problem(w, http.StatusBadRequest, map[string]any{"code": "validation_failed", "title": "Validation Error",
			"detail": "groupBy names an unknown dimension", "retryable": false, "traceId": "trace-400",
			"errors": []map[string]any{{"name": "groupBy", "reason": "unknown dimension Nope"}}})
		return true
	}
	args := []string{"query-costs", "--start-date", "2026-10-01", "--end-date", "2026-10-31", "--group-by", "Nope"}

	res := invoke(t, env, false, args...)
	if res.code != exitFailed || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q", res.code, res.stdout)
	}
	var failure apiFailure
	if err := json.Unmarshal([]byte(res.stderr), &failure); err != nil {
		t.Fatalf("stderr off a terminal is not the ApiError JSON: %q", res.stderr)
	}
	if failure.Status != 400 || failure.Code != "validation_failed" || failure.TraceID != "trace-400" ||
		!strings.Contains(res.stderr, `"name":"groupBy"`) {
		t.Errorf("ApiError %+v", failure)
	}

	res = invoke(t, env, true, args...)
	want := "costfluent: 400 validation_failed: groupBy names an unknown dimension; groupBy: unknown dimension Nope (trace trace-400)\n"
	if res.code != exitFailed || res.stderr != want {
		t.Errorf("on a terminal: exit %d, stderr %q, want %q", res.code, res.stderr, want)
	}
}

func TestAToolErrorWithoutTheAPIExits1(t *testing.T) {
	// A workspace tool called by an organization token without a workspace is refused by the tool
	// itself, with no ApiError to pass on.
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {name: "cli", org: true, capabilities: catalogCapabilities()}})
	env := map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": srv.URL}
	res := invoke(t, env, false, "list-budgets")
	if res.code != exitFailed || !strings.Contains(res.stderr, "workspace_id is required") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	api, env := fullAPI(t)
	api.override = func(w http.ResponseWriter, r *http.Request) bool {
		problem(w, http.StatusBadRequest, map[string]any{"code": "validation_failed", "detail": "bad request"})
		return true
	}
	var runs []result
	for _, c := range calls {
		runs = append(runs, invoke(t, env, false, c.args...), invoke(t, env, true, c.args...))
	}
	runs = append(runs,
		invoke(t, map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": "http://example.com"}, true, "list-budgets"),
		invoke(t, map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret, "COSTFLUENT_BASE_URL": "http://127.0.0.1:1"}, false, "list-budgets"),
		invoke(t, env, false, "list-budgets", "--page", "x"),
	)
	api.override = nil
	for _, c := range calls {
		runs = append(runs, invoke(t, env, true, c.args...))
	}
	for _, r := range runs {
		if strings.Contains(r.stdout+r.stderr, tokenSecret) {
			t.Fatalf("the token was printed:\nstdout %s\nstderr %s", r.stdout, r.stderr)
		}
	}
}

func TestWhoami(t *testing.T) {
	_, env := fullAPI(t)
	res := invoke(t, env, false, "whoami")
	if res.code != exitOK || !strings.Contains(res.stdout, `"tokenName": "cli"`) || !strings.Contains(res.stdout, `"viewCosts"`) {
		t.Errorf("exit %d, stdout %s, stderr %s", res.code, res.stdout, res.stderr)
	}
	res = invoke(t, env, true, "whoami")
	if res.code != exitOK || !strings.Contains(res.stdout, "tokenName:") || !strings.Contains(res.stdout, "workspaceId:") {
		t.Errorf("on a terminal: exit %d, stdout %s", res.code, res.stdout)
	}
	if strings.Contains(res.stdout, tokenSecret) {
		t.Error("whoami printed the token")
	}
	refused := map[string]string{"COSTFLUENT_API_TOKEN": "cfk_ws_wrong", "COSTFLUENT_BASE_URL": env["COSTFLUENT_BASE_URL"]}
	if res := invoke(t, refused, false, "whoami"); res.code != exitAccess || !strings.Contains(res.stderr, `"status":401`) {
		t.Errorf("a refused token: exit %d, stderr %q", res.code, res.stderr)
	}
	if res := invoke(t, map[string]string{}, false, "whoami"); res.code != exitAccess {
		t.Errorf("no token: exit %d", res.code)
	}
	if res := invoke(t, env, false, "whoami", "--help"); res.code != exitOK || !strings.Contains(res.stdout, "GET /v1/me") {
		t.Errorf("--help: exit %d, %q", res.code, res.stdout)
	}
	if res := invoke(t, env, false, "whoami", "--all"); res.code != exitUsage {
		t.Errorf("an unknown flag: exit %d", res.code)
	}
}

func TestArgumentsTheSchemaRefusesExit2(t *testing.T) {
	_, env := fullAPI(t)
	commands, err := loadCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	c := &cli{getenv: func(k string) string { return env[k] }, stdout: &stdout, stderr: &stderr}
	code := c.call(context.Background(), find(commands, "list-budgets"), map[string]any{"page": "two"}, outputJSON)
	if code != exitUsage || !strings.Contains(stderr.String(), `validating "arguments"`) ||
		!strings.Contains(stderr.String(), "costfluent list-budgets --help") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}
