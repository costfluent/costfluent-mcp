package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

// session starts the server for secret against baseURL and connects a client to it in memory.
func session(t *testing.T, baseURL, secret string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	client := costfluent.NewClient(costfluent.WithAPIKey(secret), costfluent.WithBaseURL(baseURL))
	srv, _, err := New(ctx, client, "test")
	if err != nil {
		t.Fatal(err)
	}
	return connectTo(t, srv)
}

// connectTo connects a client to srv in memory.
func connectTo(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func listed(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// refusal decodes a failed call's ApiError.
func refusal(t *testing.T, res *mcp.CallToolResult) apiError {
	t.Helper()
	if !res.IsError {
		t.Fatalf("call succeeded, want a refusal: %s", text(res))
	}
	var ae apiError
	if err := json.Unmarshal([]byte(text(res)), &ae); err != nil {
		t.Fatalf("the error is not the ApiError shape: %s", text(res))
	}
	return ae
}

// toolCase is one tool: the capability it needs, arguments for its allowed call and what that
// call's result must contain, and arguments naming another workspace.
type toolCase struct {
	name       string
	capability costfluent.Capability
	args       map[string]any
	wantInOK   string
	crossArgs  map[string]any // nil when the tool takes no workspace and the API narrows instead
}

var toolCases = []toolCase{
	{"list_workspaces", costfluent.CapabilityViewWorkspace, map[string]any{}, `"id":"ws_own"`, nil},
	{"query_costs", costfluent.CapabilityViewCosts, map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31"}, `"workspace":"ws_own"`,
		map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "workspace_id": otherWS}},
	{"list_cost_reports", costfluent.CapabilityViewCostReports, map[string]any{}, `"title":"report in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"get_cost_report_data", costfluent.CapabilityViewCosts, map[string]any{"cost_report_id": reportID}, `"keys":["ws_own"]`,
		map[string]any{"cost_report_id": reportID, "workspace_id": otherWS}},
	{"list_annotations", costfluent.CapabilityViewCostReports, map[string]any{}, `"text":"note in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"list_budgets", costfluent.CapabilityViewBudgets, map[string]any{}, `"name":"budget in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"list_anomalies", costfluent.CapabilityViewAnomalies, map[string]any{}, `"workspaceId":"ws_own"`, nil},
	{"list_savings_items", costfluent.CapabilityViewSavingsItems, map[string]any{}, `"title":"finding in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"list_commitments", costfluent.CapabilityViewCosts, map[string]any{}, `"description":"commitment in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"get_commitment_coverage", costfluent.CapabilityViewCosts, map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31"}, `"provider":"aws-ws_own"`,
		map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "workspace_id": otherWS}},
	{"get_kubernetes_efficiency", costfluent.CapabilityViewCosts, map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "cluster"}, `"cluster":"cluster-ws_own"`,
		map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "cluster", "workspace_id": otherWS}},
}

func TestEveryToolIsCovered(t *testing.T) {
	all := ToolNames(&costfluent.Me{Capabilities: allCapabilities})
	var covered []string
	for _, tc := range toolCases {
		covered = append(covered, tc.name)
	}
	if !slices.Equal(all, covered) {
		t.Fatalf("tools %v, cases %v: every tool needs an allowed, a denied and a cross-workspace case", all, covered)
	}
}

func TestEachToolAllowedDeniedAndCrossWorkspace(t *testing.T) {
	for _, tc := range toolCases {
		t.Run(tc.name, func(t *testing.T) {
			const allowed, denied = "cfk_ws_allowed", "cfk_ws_denied"
			var without []costfluent.Capability
			for _, c := range allCapabilities {
				if c != tc.capability {
					without = append(without, c)
				}
			}
			api, srv := newFakeAPI(t, map[string]token{
				allowed: {workspace: ownWS, capabilities: []costfluent.Capability{tc.capability}},
				denied:  {workspace: ownWS, capabilities: without},
			})

			t.Run("allowed", func(t *testing.T) {
				cs := session(t, srv.URL, allowed)
				if !slices.Contains(listed(t, cs), tc.name) {
					t.Fatalf("%s is not listed for a token carrying %s", tc.name, tc.capability)
				}
				res := call(t, cs, tc.name, tc.args)
				if res.IsError {
					t.Fatalf("refused: %s", text(res))
				}
				body, _ := json.Marshal(res.StructuredContent)
				if !strings.Contains(string(body), tc.wantInOK) {
					t.Fatalf("result %s does not hold %s", body, tc.wantInOK)
				}
				if strings.Contains(string(body), otherWS) {
					t.Fatalf("a workspace token's result holds another workspace: %s", body)
				}
			})

			t.Run("denied", func(t *testing.T) {
				cs := session(t, srv.URL, denied)
				if slices.Contains(listed(t, cs), tc.name) {
					t.Fatalf("%s is listed for a token without %s", tc.name, tc.capability)
				}
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
				if err == nil && !res.IsError {
					t.Fatalf("an unlisted tool answered: %s", text(res))
				}
			})

			t.Run("cross-workspace", func(t *testing.T) {
				cs := session(t, srv.URL, allowed)
				if tc.crossArgs == nil {
					// The tool takes no workspace: the API narrows a workspace token to its own,
					// which the allowed case holds to; here the request must not widen it.
					call(t, cs, tc.name, tc.args)
					return
				}
				ae := refusal(t, call(t, cs, tc.name, tc.crossArgs))
				if ae.Status != http.StatusForbidden || ae.Code != "forbidden" || !strings.Contains(ae.Detail, "other workspaces") {
					t.Fatalf("refusal %+v, want the API's 403 forbidden", ae)
				}
			})
			if n := api.requestCount("/v1/me"); n == 0 {
				t.Error("the server never asked GET /v1/me for its token's capabilities")
			}
		})
	}
}

func TestAnOrganizationTokenNamesTheWorkspace(t *testing.T) {
	const org = "cfk_org_token"
	api, srv := newFakeAPI(t, map[string]token{org: {capabilities: allCapabilities}})
	cs := session(t, srv.URL, org)

	for name, args := range map[string]map[string]any{
		"list_cost_reports": {}, "list_budgets": {}, "get_cost_report_data": {"cost_report_id": reportID}, "list_annotations": {},
		"list_savings_items": {}, "list_commitments": {}, "get_commitment_coverage": {"start_date": "2026-10-01", "end_date": "2026-10-31"},
		"get_kubernetes_efficiency": {"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "cluster"},
	} {
		res := call(t, cs, name, args)
		if !res.IsError || !strings.Contains(text(res), "workspace_id is required") {
			t.Errorf("%s without a workspace: %s, want a request for workspace_id", name, text(res))
		}
	}
	for _, path := range []string{"/v1/budgets", "/v1/annotations", "/v1/savings-items", "/v1/commitments", "/v1/commitments/coverage", "/v1/kubernetes/efficiency"} {
		if n := api.requestCount(path); n != 0 {
			t.Errorf("called %s %d times without a workspace", path, n)
		}
	}

	res := call(t, cs, "list_budgets", map[string]any{"workspace_id": otherWS})
	if res.IsError {
		t.Fatalf("an organization token was refused workspace %s: %s", otherWS, text(res))
	}
	if q := api.lastQuery("/v1/budgets"); q.Get("workspaceId") != otherWS {
		t.Errorf("read workspace %q, want %q", q.Get("workspaceId"), otherWS)
	}

	res = call(t, cs, "list_workspaces", nil)
	body, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(body), ownWS) || !strings.Contains(string(body), otherWS) {
		t.Errorf("an organization token's workspaces: %s", body)
	}
}

func TestQueryCostsPassesItsQueryThrough(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "query_costs", map[string]any{
		"start_date": "2026-09-01", "end_date": "2026-09-30", "granularity": "month",
		"group_by": []string{"ServiceName", "Tag:team"}, "filter": "costs.provider = 'aws'", "measure": "usage",
		"amortize": true, "page": 2, "limit": 99999,
	})
	if res.IsError {
		t.Fatal(text(res))
	}
	q := api.lastQuery("/v1/costs")
	for key, want := range map[string]string{
		"startDate": "2026-09-01", "endDate": "2026-09-30", "granularity": "month", "filter": "costs.provider = 'aws'",
		"measure": "usage", "amortize": "true", "page": "2", "limit": "5000",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := q["groupBy"]; !slices.Equal(got, []string{"ServiceName", "Tag:team"}) {
		t.Errorf("groupBy = %v", got)
	}
	var out CostsResult
	body, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(body, &out); err != nil || out.NextPage != 3 || len(out.Rows) != 1 || out.Meta.TotalCost != "25.00000000" {
		t.Errorf("result %s (%v)", body, err)
	}

	call(t, cs, "query_costs", map[string]any{"start_date": "2026-09-01", "end_date": "2026-09-30"})
	if got := api.lastQuery("/v1/costs").Get("limit"); got != "500" {
		t.Errorf("default limit %q, want 500", got)
	}
}

func TestEveryCallCarriesTheStartupToken(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	for _, tc := range toolCases {
		call(t, cs, tc.name, tc.args)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.auth) < len(toolCases)+1 {
		t.Fatalf("the API saw %d requests", len(api.auth))
	}
	for _, a := range api.auth {
		if a != "Bearer "+tokenSecret {
			t.Fatalf("a request carried %q", a)
		}
	}
}

func TestTheTokenIsNeverEchoed(t *testing.T) {
	t.Parallel()
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	var seen []string
	record := func(v any) {
		b, _ := json.Marshal(v)
		seen = append(seen, string(b))
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	record(tools)
	record(cs.InitializeResult())
	for _, tc := range toolCases {
		record(call(t, cs, tc.name, tc.args))
		if tc.crossArgs != nil {
			record(call(t, cs, tc.name, tc.crossArgs))
		}
	}
	// Every failure path: the API refusing, and the API unreachable.
	api.override = func(w http.ResponseWriter, _ *http.Request) bool {
		problem(w, http.StatusUnauthorized, "unauthorized", "Authentication Error", "The API token is invalid or expired")
		return true
	}
	for _, tc := range toolCases {
		record(call(t, cs, tc.name, tc.args))
	}
	// One call: the SDK retries an unreachable API with backoff, and the error text is the same
	// transport error for every tool.
	srv.Close()
	record(call(t, cs, "list_budgets", nil))

	for _, s := range seen {
		if strings.Contains(s, tokenSecret) || strings.Contains(s, "Bearer") {
			t.Fatalf("a response discloses the token: %s", s)
		}
	}
}

func TestARateLimitedCallReturnsTheAPIsAnswer(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	api.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "120")
		problem(w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", "Rate limit exceeded")
		return true
	}

	ae := refusal(t, call(t, cs, "list_budgets", nil))
	if ae.Status != http.StatusTooManyRequests || ae.Code != "rate_limited" || !ae.Retryable {
		t.Fatalf("refusal %+v, want the API's retryable 429", ae)
	}
	if ae.RetryAfterSeconds != 120 {
		t.Errorf("retryAfterSeconds %d, want the 120 the API asked for", ae.RetryAfterSeconds)
	}
	if n := api.requestCount("/v1/budgets"); n != 1 {
		t.Errorf("sent %d requests; a reset beyond the SDK's wait is returned at once", n)
	}
}

func TestARefusalSaysHowLongTheAPIAskedToWaitAndOnlyThen(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		header map[string]string
		want   int
	}{
		"RateLimit-Reset alone": {http.StatusTooManyRequests, map[string]string{"RateLimit-Reset": "90"}, 90},
		"a 429 naming no wait":  {http.StatusTooManyRequests, nil, 0},
		"a 404":                 {http.StatusNotFound, map[string]string{"Retry-After": "30"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
			cs := session(t, srv.URL, tokenSecret)
			api.override = func(w http.ResponseWriter, _ *http.Request) bool {
				for k, v := range c.header {
					w.Header().Set(k, v)
				}
				problem(w, c.status, "refused", http.StatusText(c.status), "refused")
				return true
			}
			res := call(t, cs, "list_budgets", nil)
			if ae := refusal(t, res); ae.Status != c.status || ae.RetryAfterSeconds != c.want {
				t.Errorf("refusal %+v, want status %d and retryAfterSeconds %d", ae, c.status, c.want)
			}
			if c.want == 0 && strings.Contains(text(res), "retryAfterSeconds") {
				t.Errorf("a refusal naming no wait carries the field: %s", text(res))
			}
		})
	}
}

// failed asserts res is a failed call carrying no structuredContent: a client reading the typed
// result of a refused call must not see the zero output, which reads as an empty success.
func failed(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("call succeeded, want a failure: %s", text(res))
	}
	if res.StructuredContent != nil {
		body, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("a failed call carries structuredContent %s", body)
	}
}

func TestAFailedCallCarriesOnlyTheError(t *testing.T) {
	refusals := []struct {
		status         int
		code, detail   string
		retryable      bool
		retryAfterSecs string
	}{
		{http.StatusNotFound, "not_found", "The cost report was not found", false, ""},
		{http.StatusForbidden, "forbidden", "This API token cannot read other workspaces", false, ""},
		// Past the SDK's longest wait, so the 429 is returned at once rather than retried.
		{http.StatusTooManyRequests, "rate_limited", "Rate limit exceeded", true, "120"},
	}
	for _, tc := range toolCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
			cs := session(t, srv.URL, tokenSecret)

			ok := call(t, cs, tc.name, tc.args)
			if ok.IsError || ok.StructuredContent == nil {
				t.Fatalf("a successful call: isError=%v structuredContent=%v", ok.IsError, ok.StructuredContent)
			}

			for _, r := range refusals {
				api.mu.Lock()
				api.override = func(w http.ResponseWriter, _ *http.Request) bool {
					if r.retryAfterSecs != "" {
						w.Header().Set("Retry-After", r.retryAfterSecs)
					}
					problem(w, r.status, r.code, http.StatusText(r.status), r.detail)
					return true
				}
				api.mu.Unlock()

				res := call(t, cs, tc.name, tc.args)
				failed(t, res)
				ae := refusal(t, res)
				if ae.Status != r.status || ae.Code != r.code || ae.Detail != r.detail || ae.Retryable != r.retryable || ae.TraceID != "trace-1" {
					t.Errorf("%d: refusal %+v, want the API's ApiError", r.status, ae)
				}
			}

			// The SDK retries an unreachable API with backoff before it gives up.
			srv.Close()
			res := call(t, cs, tc.name, tc.args)
			failed(t, res)
			if !strings.Contains(text(res), "connect") {
				t.Errorf("transport error text %q does not name the failed connection", text(res))
			}
		})
	}

	t.Run("a call refused before the API", func(t *testing.T) {
		t.Parallel()
		_, srv := newFakeAPI(t, map[string]token{tokenSecret: {capabilities: allCapabilities}})
		cs := session(t, srv.URL, tokenSecret)
		res := call(t, cs, "list_budgets", nil)
		failed(t, res)
		if !strings.Contains(text(res), "workspace_id is required") {
			t.Errorf("error text %q, want a request for workspace_id", text(res))
		}
	})
}

func TestNewFailsOnATokenTheAPIRefuses(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{})
	client := costfluent.NewClient(costfluent.WithAPIKey(tokenSecret), costfluent.WithBaseURL(srv.URL))

	_, _, err := New(context.Background(), client, "test")
	if !costfluent.IsUnauthorized(err) {
		t.Fatalf("err = %v, want the API's 401", err)
	}
	if strings.Contains(err.Error(), tokenSecret) {
		t.Fatalf("the error discloses the token: %v", err)
	}
}

// A token holding none of the capabilities the tools need would serve an assistant nothing, and an
// assistant shows no log, so the server refuses to start and names every capability it looks for.
func TestNewFailsOnATokenWithNoReadCapability(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: []costfluent.Capability{costfluent.CapabilityViewProviders}}})
	client := costfluent.NewClient(costfluent.WithAPIKey(tokenSecret), costfluent.WithBaseURL(srv.URL))

	_, _, err := New(context.Background(), client, "test")
	if err == nil {
		t.Fatal("started with a token no tool can use")
	}
	for _, id := range allCapabilities {
		if !strings.Contains(err.Error(), string(id)) {
			t.Errorf("the error does not name %s: %v", id, err)
		}
	}
	if !strings.Contains(err.Error(), "Settings, API tokens") || !strings.Contains(err.Error(), "Read only") {
		t.Errorf("the error does not say where to create a token: %v", err)
	}
	if strings.Contains(err.Error(), tokenSecret) {
		t.Fatalf("the error discloses the token: %v", err)
	}
}

func TestToolsAreReadOnly(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	res, err := session(t, srv.URL, tokenSecret).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", tool.Name)
		}
		if !strings.Contains(tool.Description, "capability") {
			t.Errorf("%s does not name the capability it needs", tool.Name)
		}
	}
}

// A budget's spend and state leave out spend in other currencies and, when the plan's history
// starts inside the period, the days before it. The assistant must see both to know the budget
// does not cover its whole scope.
func TestListBudgetsNamesUnconvertedSpendAndHistoryFloor(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "list_budgets", nil)
	if res.IsError {
		t.Fatalf("list_budgets failed: %s", text(res))
	}
	// Both outputs are the tool's result as a map, so keys come sorted rather than in the SDK's order.
	structured, _ := json.Marshal(res.StructuredContent)
	for _, out := range []string{text(res), string(structured)} {
		var page struct {
			Items []struct {
				Unconverted  []map[string]string `json:"unconverted"`
				HistoryFloor string              `json:"historyFloor"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &page); err != nil || len(page.Items) != 1 {
			t.Fatalf("output %s: %v", out, err)
		}
		got := page.Items[0]
		if len(got.Unconverted) != 1 || got.Unconverted[0]["currency"] != "EUR" || got.Unconverted[0]["amount"] != "12.34567890" ||
			got.HistoryFloor != "2026-04-06" {
			t.Errorf("output lacks the unconverted spend or the history floor: %s", out)
		}
	}
}

// An unmeasured budget's figures arrive as null. The assistant must not read them as a measured
// 0 percent or an empty amount, which say the budget is on track.
func TestListBudgetsLeavesUnmeasuredFiguresUnset(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	api.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/budgets" {
			return false
		}
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/budgets"},
			"data": []map[string]any{{"id": "bud_1", "name": "unmeasured", "amount": "100.00000000", "currency": "USD",
				"startDate": "2026-01-01", "intervalCount": 1, "intervalUnit": "month", "status": "active", "state": "unknown",
				"spendAvailability": "unavailable", "currentSpend": nil, "budgetedToDate": nil, "projectedSpend": nil, "percentUsed": nil}}})
		return true
	}

	res := call(t, cs, "list_budgets", nil)
	if res.IsError {
		t.Fatalf("list_budgets failed: %s", text(res))
	}
	structured, _ := json.Marshal(res.StructuredContent)
	for _, out := range []string{text(res), string(structured)} {
		if !strings.Contains(out, `"name":"unmeasured"`) {
			t.Fatalf("output lacks the budget: %s", out)
		}
		for _, measured := range []string{`"percentUsed":0`, `"currentSpend":""`, `"budgetedToDate":""`, `"projectedSpend":""`} {
			if strings.Contains(out, measured) {
				t.Errorf("an unmeasured budget reads %s: %s", measured, out)
			}
		}
	}
}

// An assistant reading an anomaly gets what drove it: the region, the resource and the SKU holding
// most of the increase, and the three resources and SKUs that added most.
func TestListAnomaliesReturnsTheAttribution(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	api.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/anomalies" {
			return false
		}
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/anomalies"}, "data": []map[string]any{{
			"id": "anm_1", "workspaceId": ownWS, "providerId": "prv_1", "anomalyDate": "2026-10-10",
			"serviceName": "Amazon EC2", "region": "eu-central-1", "anomalyType": "spike", "severity": "high",
			"expectedCost": "10.00000000", "actualCost": "422.00000000", "deviationPercent": 4120, "currency": "EUR",
			"status": "open", "attributedResourceId": "i-0abc",
			"contributors": []map[string]any{
				{"accountId": "111111111111", "resourceId": "i-0abc", "increase": "412.00000000"},
				{"accountId": nil, "resourceId": "bucket-logs", "increase": "0.25000000"},
			},
			"attributedSku":   "EUC1-BoxUsage:m7i.4xlarge",
			"skuContributors": []map[string]any{{"sku": "EUC1-BoxUsage:m7i.4xlarge", "increase": "412.00000000"}},
		}}})
		return true
	}

	res := call(t, cs, "list_anomalies", map[string]any{})
	if res.IsError {
		t.Fatalf("list_anomalies failed: %s", text(res))
	}
	var page struct {
		Items []costfluent.Anomaly `json:"items"`
	}
	if err := json.Unmarshal([]byte(text(res)), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("list_anomalies returned %s: %v", text(res), err)
	}
	a := page.Items[0]
	if a.Region == nil || *a.Region != "eu-central-1" || a.AttributedResourceID == nil || *a.AttributedResourceID != "i-0abc" ||
		a.AttributedSku == nil || *a.AttributedSku != "EUC1-BoxUsage:m7i.4xlarge" {
		t.Errorf("list_anomalies returned the attribution %s", text(res))
	}
	if len(a.Contributors) != 2 || a.Contributors[0].AccountID == nil || *a.Contributors[0].AccountID != "111111111111" ||
		a.Contributors[0].Increase != "412.00000000" || a.Contributors[1].AccountID != nil || a.Contributors[1].ResourceID != "bucket-logs" {
		t.Errorf("list_anomalies returned the contributors %+v", a.Contributors)
	}
	if len(a.SkuContributors) != 1 || a.SkuContributors[0].Sku != "EUC1-BoxUsage:m7i.4xlarge" || a.SkuContributors[0].Increase != "412.00000000" {
		t.Errorf("list_anomalies returned the SKU contributors %+v", a.SkuContributors)
	}
}

// The API matches a severity case-insensitively and answers one it does not know with an empty
// page, which an assistant reports as "no anomalies". The tool names the four severities and
// refuses any other before it calls the API.
func TestListAnomaliesRefusesASeverityTheAPIDoesNotKnow(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	api.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/anomalies" {
			return false
		}
		data := []map[string]any{}
		if s := r.URL.Query().Get("severity"); s == "" || strings.ToLower(s) == "high" {
			data = append(data, map[string]any{"id": "anm_1", "workspaceId": ownWS, "providerId": "prv_1",
				"anomalyDate": "2026-10-10", "serviceName": "AmazonEC2", "anomalyType": "spike", "severity": "high",
				"expectedCost": "10.00000000", "actualCost": "30.00000000", "deviationPercent": 200, "currency": "USD", "status": "open"})
		}
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/anomalies"}, "data": data})
		return true
	}
	const severities = "low, medium, high, critical"

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var described string
	for _, tool := range tools.Tools {
		if tool.Name != "list_anomalies" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("list_anomalies input schema %s: %v", raw, err)
		}
		described = schema.Properties["severity"].Description
	}
	if !strings.Contains(described, severities) {
		t.Errorf("the severity argument reads %q, want it to name %s", described, severities)
	}

	for _, severity := range []string{"low", "medium", "High", "critical", "LOW", ""} {
		t.Run("accepts "+severity, func(t *testing.T) {
			before := api.requestCount("/v1/anomalies")
			res := call(t, cs, "list_anomalies", map[string]any{"severity": severity})
			if res.IsError {
				t.Fatalf("severity %q refused: %s", severity, text(res))
			}
			if n := api.requestCount("/v1/anomalies") - before; n != 1 {
				t.Fatalf("severity %q sent %d requests, want 1", severity, n)
			}
			if got := api.lastQuery("/v1/anomalies").Get("severity"); got != severity {
				t.Errorf("sent severity %q, want %q", got, severity)
			}
		})
	}
	for _, severity := range []string{"severe", "major", "7", "none", "highest", " high", "high "} {
		t.Run("refuses "+severity, func(t *testing.T) {
			before := api.requestCount("/v1/anomalies")
			res := call(t, cs, "list_anomalies", map[string]any{"severity": severity})
			failed(t, res)
			if !strings.Contains(text(res), severities) {
				t.Errorf("severity %q refused with %q, want it to name %s", severity, text(res), severities)
			}
			if n := api.requestCount("/v1/anomalies") - before; n != 0 {
				t.Errorf("severity %q sent %d requests, want none", severity, n)
			}
		})
	}
}

// decode reads a successful call's structured result into v.
func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("refused: %s", text(res))
	}
	body, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("result %s: %v", body, err)
	}
}

// sent asserts a request's query is exactly want: every parameter the tool sends, and no other.
func sent(t *testing.T, q url.Values, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for key := range q {
		got[key] = q.Get(key)
	}
	if !maps.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

// theAPIRefuses calls name with arg set to value, which the API does not take for param, and
// asserts the value reached the API as given and its 400 came back as the tool's error rather
// than as an empty result.
func theAPIRefuses(t *testing.T, api *fakeAPI, cs *mcp.ClientSession, name, path string, args map[string]any, arg, param, value string) {
	t.Helper()
	with := maps.Clone(args)
	with[arg] = value
	before := api.requestCount(path)
	res := call(t, cs, name, with)
	if n := api.requestCount(path) - before; n != 1 {
		t.Fatalf("%s %q sent %d requests, want 1", arg, value, n)
	}
	if got := api.lastQuery(path).Get(param); got != value {
		t.Errorf("sent %s %q, want %q as given", param, got, value)
	}
	failed(t, res)
	if ae := refusal(t, res); ae.Status != http.StatusBadRequest || ae.Code != "validation_failed" || !strings.Contains(ae.Detail, param+" must be one of") {
		t.Errorf("%s %q: refusal %+v, want the API's 400 naming the values it takes", arg, value, ae)
	}
}

func TestListSavingsItemsSendsItsFiltersAndPages(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "list_savings_items", map[string]any{
		"tab": "toReview", "source": "awsCostOptimizationHub", "kind": "rightsizing", "cloud_account_id": "prv_1",
		"minimum_monthly_saving": "100.50", "sort": "largestSaving",
	})
	sent(t, api.lastQuery("/v1/savings-items"), map[string]string{
		"workspaceId": ownWS, "tab": "toReview", "source": "awsCostOptimizationHub", "kind": "rightsizing",
		"cloudAccountId": "prv_1", "minimumMonthlySaving": "100.50", "sort": "largestSaving", "page": "1", "limit": "100",
	})
	var page Page[costfluent.SavingsItem]
	decode(t, res, &page)
	if page.NextPage != 2 || len(page.Items) != 1 {
		t.Fatalf("first page: next_page %d, %d items", page.NextPage, len(page.Items))
	}
	if item := page.Items[0]; item.ID != "sav_1" || item.Evidence != "CPU under 5% for 30 days" || item.EstimatedMonthlySaving != "120.50000000" ||
		item.Currency != "USD" || item.Status != "open" || item.Source != "awsCostOptimizationHub" || item.Kind == nil || *item.Kind != "rightsizing" {
		t.Errorf("finding %+v", item)
	}

	// Without filters the tool sends none, so the API's defaults apply: every status, newest first.
	res = call(t, cs, "list_savings_items", map[string]any{"page": 2})
	sent(t, api.lastQuery("/v1/savings-items"), map[string]string{"workspaceId": ownWS, "page": "2", "limit": "100"})
	page = Page[costfluent.SavingsItem]{}
	decode(t, res, &page)
	if page.NextPage != 0 || len(page.Items) != 1 {
		t.Errorf("last page: next_page %d, %d items", page.NextPage, len(page.Items))
	}
}

// The savings queue has a capability of its own: a token holding every other read, as one made
// before it existed does, is not served list_savings_items, and one holding only it is served that
// tool alone.
func TestListSavingsItemsNeedsViewSavingsItems(t *testing.T) {
	const reads, savings = "cfk_ws_reads", "cfk_ws_savings"
	others := slices.DeleteFunc(slices.Clone(allCapabilities), func(c costfluent.Capability) bool {
		return c == costfluent.CapabilityViewSavingsItems
	})
	api, srv := newFakeAPI(t, map[string]token{
		reads:   {workspace: ownWS, capabilities: others},
		savings: {workspace: ownWS, capabilities: []costfluent.Capability{costfluent.CapabilityViewSavingsItems}},
	})

	cs := session(t, srv.URL, reads)
	if names := listed(t, cs); slices.Contains(names, "list_savings_items") || !slices.Contains(names, "list_commitments") {
		t.Errorf("a token without viewSavingsItems is served %v", names)
	}
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_savings_items", Arguments: map[string]any{}}); err == nil && !res.IsError {
		t.Errorf("an unlisted list_savings_items answered: %s", text(res))
	}
	if n := api.requestCount("/v1/savings-items"); n != 0 {
		t.Errorf("a token without viewSavingsItems sent %d requests for the queue", n)
	}

	if names := listed(t, session(t, srv.URL, savings)); !slices.Equal(names, []string{"list_savings_items"}) {
		t.Errorf("a token with only viewSavingsItems is served %v", names)
	}
}

func TestListSavingsItemsReturnsTheAPIsRefusalOfAnUnknownValue(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	for _, arg := range []string{"tab", "source", "kind", "sort"} {
		t.Run(arg, func(t *testing.T) {
			theAPIRefuses(t, api, cs, "list_savings_items", "/v1/savings-items", map[string]any{}, arg, arg, "notAValue")
		})
	}
}

func TestListAnnotationsSendsItsFiltersAndPages(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "list_annotations", map[string]any{
		"cost_report_id": reportID, "start_date": "2026-10-01", "end_date": "2026-10-31", "include_resolved": true,
	})
	sent(t, api.lastQuery("/v1/annotations"), map[string]string{
		"workspaceId": ownWS, "costReportId": reportID, "startDate": "2026-10-01", "endDate": "2026-10-31",
		"includeResolved": "true", "page": "1", "limit": "100",
	})
	var page Page[costfluent.Annotation]
	decode(t, res, &page)
	if page.NextPage != 2 || len(page.Items) != 2 {
		t.Fatalf("first page: next_page %d, %d items", page.NextPage, len(page.Items))
	}
	if note := page.Items[0]; note.Source != "manual" || note.Text == nil || *note.Text != "note in "+ownWS ||
		!slices.Equal(note.CostReportIDs, []string{reportID}) {
		t.Errorf("note %+v", note)
	}
	if firing := page.Items[1]; firing.Source != "budgetAlert" || firing.Text != nil || firing.BudgetID == nil ||
		firing.ThresholdPercent == nil || *firing.ThresholdPercent != 80 || firing.ThresholdBasis == nil || *firing.ThresholdBasis != "forecast" {
		t.Errorf("budget firing %+v", firing)
	}

	// Without filters the tool sends none, so the API's defaults apply: open annotations on every report.
	res = call(t, cs, "list_annotations", map[string]any{"page": 2})
	sent(t, api.lastQuery("/v1/annotations"), map[string]string{"workspaceId": ownWS, "page": "2", "limit": "100"})
	page = Page[costfluent.Annotation]{}
	decode(t, res, &page)
	if page.NextPage != 0 {
		t.Errorf("last page: next_page %d", page.NextPage)
	}
}

func TestListCommitmentsSendsItsFiltersAndPages(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "list_commitments", map[string]any{
		"provider": "aws", "status": "expiring", "type": "ReservedInstance", "service": "Amazon EC2", "account": "111111111111",
		"region": "eu-central-1", "term_years": 1, "payment_option": "No Upfront",
	})
	sent(t, api.lastQuery("/v1/commitments"), map[string]string{
		"workspaceId": ownWS, "provider": "aws", "status": "expiring", "type": "ReservedInstance", "service": "Amazon EC2",
		"account": "111111111111", "region": "eu-central-1", "termYears": "1", "paymentOption": "No Upfront", "page": "1", "limit": "100",
	})
	var page Page[costfluent.Commitment]
	decode(t, res, &page)
	if page.NextPage != 2 || len(page.Items) != 1 {
		t.Fatalf("first page: next_page %d, %d items", page.NextPage, len(page.Items))
	}
	if c := page.Items[0]; c.ID != "ri-0abc" || c.Status != "expiring" || c.DaysUntilExpiry == nil || *c.DaysUntilExpiry != 21 ||
		c.UnusedCost == nil || *c.UnusedCost != "22.50000000" || c.UtilizationPercentage == nil || *c.UtilizationPercentage != 87.5 {
		t.Errorf("commitment %+v", c)
	}

	res = call(t, cs, "list_commitments", map[string]any{"page": 2})
	sent(t, api.lastQuery("/v1/commitments"), map[string]string{"workspaceId": ownWS, "page": "2", "limit": "100"})
	page = Page[costfluent.Commitment]{}
	decode(t, res, &page)
	if page.NextPage != 0 || len(page.Items) != 1 {
		t.Errorf("last page: next_page %d, %d items", page.NextPage, len(page.Items))
	}
}

func TestListCommitmentsReturnsTheAPIsRefusalOfAnUnknownStatus(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	theAPIRefuses(t, api, cs, "list_commitments", "/v1/commitments", map[string]any{}, "status", "status", "retired")
}

// The API bins coverage by day unless asked otherwise, and a daily series over a long range
// outgrows an assistant's context, so the tool asks monthly bins unless the caller names one.
func TestGetCommitmentCoverageAsksMonthlyBinsUnlessTold(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "get_commitment_coverage", map[string]any{"start_date": "2026-01-01", "end_date": "2026-10-31"})
	sent(t, api.lastQuery("/v1/commitments/coverage"), map[string]string{
		"workspaceId": ownWS, "startDate": "2026-01-01", "endDate": "2026-10-31", "granularity": "month",
	})
	var coverage costfluent.CommitmentCoverage
	decode(t, res, &coverage)
	if coverage.Granularity != "month" || len(coverage.Data) != 1 || coverage.Data[0].Date == nil || *coverage.Data[0].Date != "2026-10-01" ||
		coverage.Data[0].CoveragePercent == nil || *coverage.Data[0].CoveragePercent != 80 ||
		len(coverage.Totals) != 1 || coverage.Totals[0].Date != nil || coverage.Totals[0].UnusedCommitment == nil || *coverage.Totals[0].UnusedCommitment != "5.00000000" {
		t.Errorf("coverage %+v", coverage)
	}
	// The figures leave out spend with no rate and the days before the plan's history; the
	// assistant must see both to know what the coverage does not count.
	if len(coverage.Unconverted) != 1 || coverage.Unconverted[0].Currency != "EUR" || coverage.Unconverted[0].Amount != "12.34567890" ||
		coverage.HistoryFloor == nil || *coverage.HistoryFloor != "2026-04-06" {
		t.Errorf("coverage lacks the unconverted spend or the history floor: %+v", coverage)
	}

	for _, bin := range []string{"day", "week", "month", "quarter"} {
		call(t, cs, "get_commitment_coverage", map[string]any{
			"start_date": "2026-10-01", "end_date": "2026-10-31", "granularity": bin, "filter": "costs.provider = 'aws'",
		})
		sent(t, api.lastQuery("/v1/commitments/coverage"), map[string]string{
			"workspaceId": ownWS, "startDate": "2026-10-01", "endDate": "2026-10-31", "granularity": bin, "filter": "costs.provider = 'aws'",
		})
	}
}

func TestGetCommitmentCoverageReturnsTheAPIsRefusalOfAnUnknownGranularity(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	window := map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31"}
	// cumulative is a bin of /v1/costs that coverage refuses.
	for _, bin := range []string{"hour", "cumulative"} {
		t.Run(bin, func(t *testing.T) {
			theAPIRefuses(t, api, cs, "get_commitment_coverage", "/v1/commitments/coverage", window, "granularity", "granularity", bin)
		})
	}
}

func TestGetKubernetesEfficiencySendsItsWindowAndLevel(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)

	res := call(t, cs, "get_kubernetes_efficiency", map[string]any{
		"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "namespace", "filter": "costs.cluster = 'prod'",
	})
	sent(t, api.lastQuery("/v1/kubernetes/efficiency"), map[string]string{
		"workspaceId": ownWS, "startDate": "2026-10-01", "endDate": "2026-10-31", "level": "namespace", "filter": "costs.cluster = 'prod'",
	})
	var efficiency costfluent.KubernetesEfficiency
	decode(t, res, &efficiency)
	if efficiency.Level != "namespace" || len(efficiency.Rows) != 1 || efficiency.Rows[0].Cluster != "cluster-ws_own" ||
		efficiency.Rows[0].IdleCost != "75.00000000" || efficiency.Rows[0].CPU.UsedPercent == nil || *efficiency.Rows[0].CPU.UsedPercent != 25 {
		t.Errorf("efficiency %+v", efficiency)
	}
}

// The API returns at most 500 rows, largest cost first, and says when it left smaller ones out.
// The assistant must see that to know a total over the rows is not the clusters' total.
func TestGetKubernetesEfficiencySaysWhenRowsWereLeftOut(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	args := map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "workload"}

	for _, truncated := range []bool{true, false} {
		api.mu.Lock()
		api.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/v1/kubernetes/efficiency" {
				return false
			}
			reply(w, map[string]any{"currency": "USD", "level": "workload", "truncated": truncated, "unconverted": []any{},
				"rows": []map[string]any{{"cluster": "prod", "namespace": "payments", "workload": "Deployment/api", "cost": "10.00000000",
					"idleCost": "0.00000000", "cpu": map[string]any{"requested": 1, "used": 1, "peak": 1}, "memory": map[string]any{"requested": 1, "used": 1, "peak": 1}}}})
			return true
		}
		api.mu.Unlock()

		res := call(t, cs, "get_kubernetes_efficiency", args)
		if res.IsError {
			t.Fatalf("refused: %s", text(res))
		}
		structured, _ := json.Marshal(res.StructuredContent)
		for _, out := range []string{text(res), string(structured)} {
			if want := fmt.Sprintf(`"truncated":%t`, truncated); !strings.Contains(out, want) {
				t.Errorf("output %s lacks %s", out, want)
			}
		}
	}
}

func TestGetKubernetesEfficiencyReturnsTheAPIsRefusalOfAnUnknownLevel(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	theAPIRefuses(t, api, cs, "get_kubernetes_efficiency", "/v1/kubernetes/efficiency",
		map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31"}, "level", "level", "pod")
}

// The API needs a window and a level, so the tool's schema does too: an assistant that leaves one
// out is told by the tool which, without a call the API would refuse.
func TestGetKubernetesEfficiencyNeedsAWindowAndALevel(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	cs := session(t, srv.URL, tokenSecret)
	for _, missing := range []string{"start_date", "end_date", "level"} {
		args := map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-31", "level": "cluster"}
		delete(args, missing)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_kubernetes_efficiency", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("called without %s: %s", missing, text(res))
		}
		if err == nil && !strings.Contains(text(res), missing) {
			t.Errorf("the refusal %q does not name %s", text(res), missing)
		}
	}
	if n := api.requestCount("/v1/kubernetes/efficiency"); n != 0 {
		t.Errorf("sent %d requests without a window or a level", n)
	}
}

func TestBuildEndsAToolCallPastItsDeadlineAsAToolError(t *testing.T) {
	api, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	client := costfluent.NewClient(costfluent.WithAPIKey(tokenSecret), costfluent.WithBaseURL(srv.URL))
	me, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build(client, me, Options{Version: "test", CallTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cs := connectTo(t, built)
	if res := call(t, cs, "list_workspaces", nil); res.IsError {
		t.Fatalf("a call inside the deadline failed: %s", text(res))
	}

	api.override = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/budgets" {
			return false
		}
		<-r.Context().Done()
		return true
	}
	res := call(t, cs, "list_budgets", nil)
	if !res.IsError || !strings.Contains(text(res), "did not finish within 200ms") {
		t.Fatalf("IsError %v, %q; want the deadline as a tool error", res.IsError, text(res))
	}
	if len(listed(t, cs)) == 0 {
		t.Error("the deadline reached a method other than tools/call")
	}
}

func TestBuildRefusesATokenNoToolCanUse(t *testing.T) {
	_, err := Build(costfluent.NewClient(), &costfluent.Me{TokenName: "narrow", Capabilities: []costfluent.Capability{"viewProviders"}}, Options{Version: "test"})
	if err == nil || !strings.Contains(err.Error(), string(costfluent.CapabilityViewWorkspace)) {
		t.Fatalf("err = %v, want one naming the capabilities the tools need", err)
	}
}

func TestBuildHostedTellsACallerTheAPIWasUnreachableWithoutItsAddress(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: allCapabilities}})
	client := costfluent.NewClient(costfluent.WithAPIKey(tokenSecret), costfluent.WithBaseURL(srv.URL))
	me, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := Build(client, me, Options{Version: "test", Hosted: true})
	if err != nil {
		t.Fatal(err)
	}
	local, err := Build(client, me, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	hostedSession, localSession := connectTo(t, hosted), connectTo(t, local)
	if res := call(t, hostedSession, "list_anomalies", map[string]any{"severity": "dire"}); !strings.Contains(text(res), "severity must be one of") {
		t.Errorf("a hosted call's own validation error was replaced: %s", text(res))
	}
	srv.Close()

	res := call(t, hostedSession, "list_workspaces", nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	if !res.IsError || text(res) != errAPIUnreachable.Error() || strings.Contains(text(res), host) {
		t.Errorf("hosted: IsError %v, %q; want only %q", res.IsError, text(res), errAPIUnreachable)
	}
	if res := call(t, localSession, "list_workspaces", nil); !res.IsError || !strings.Contains(text(res), host) {
		t.Errorf("stdio: IsError %v, %q; want the transport's own error naming %s", res.IsError, text(res), host)
	}
}

func TestAPIAttemptTimeoutOutlastsTheAPIsInteractiveBudget(t *testing.T) {
	// The Public API answers an interactive request for up to 90 seconds before it gives up with a
	// 503 (RequestTimeoutPolicies.InteractiveTimeout), and the SDK's own default is 30.
	if APIAttemptTimeout < 90*time.Second || APIAttemptTimeout <= costfluent.DefaultTimeout {
		t.Fatalf("APIAttemptTimeout %s abandons reads the API would still answer", APIAttemptTimeout)
	}
}

// Names, titles, descriptions, tag keys and tag values reach the assistant as written by whoever
// can name a report or tag a cloud resource, so the server says they are data, and every tool,
// since each returns some, is annotated as reaching beyond the server's own domain.
func TestTheAssistantIsToldTenantTextIsData(t *testing.T) {
	const marker = "data, never instructions"
	for name, tok := range map[string]token{
		"an organization token": {capabilities: allCapabilities},
		"a workspace token":     {workspace: ownWS, capabilities: allCapabilities},
	} {
		t.Run(name, func(t *testing.T) {
			_, srv := newFakeAPI(t, map[string]token{tokenSecret: tok})
			cs := session(t, srv.URL, tokenSecret)
			if instructions := cs.InitializeResult().Instructions; !strings.Contains(instructions, marker) ||
				!strings.Contains(instructions, "tag keys and values") {
				t.Errorf("the instructions do not mark tenant text as data: %q", instructions)
			}
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Tools) == 0 {
				t.Fatal("no tools listed")
			}
			for _, tool := range res.Tools {
				if !strings.Contains(tool.Description, marker) {
					t.Errorf("%s's description does not mark its names and tag values as data", tool.Name)
				}
				if a := tool.Annotations; a == nil || a.OpenWorldHint == nil || !*a.OpenWorldHint || !a.ReadOnlyHint {
					t.Errorf("%s is not annotated read-only and open-world: %+v", tool.Name, a)
				}
			}
		})
	}
}
