package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

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
	{"list_budgets", costfluent.CapabilityViewBudgets, map[string]any{}, `"name":"budget in ws_own"`,
		map[string]any{"workspace_id": otherWS}},
	{"list_anomalies", costfluent.CapabilityViewAnomalies, map[string]any{}, `"workspaceId":"ws_own"`, nil},
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
		"list_cost_reports": {}, "list_budgets": {}, "get_cost_report_data": {"cost_report_id": reportID},
	} {
		res := call(t, cs, name, args)
		if !res.IsError || !strings.Contains(text(res), "workspace_id is required") {
			t.Errorf("%s without a workspace: %s, want a request for workspace_id", name, text(res))
		}
	}
	if n := api.requestCount("/v1/budgets"); n != 0 {
		t.Errorf("called the API %d times without a workspace", n)
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
	if n := api.requestCount("/v1/budgets"); n != 1 {
		t.Errorf("sent %d requests; a reset beyond the SDK's wait is returned at once", n)
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
