package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

const (
	orgID       = "org_aaaaaaaaaaaaaaaaaaaaaa"
	ownWS       = "ws_own"
	otherWS     = "ws_other"
	reportID    = "rpt_monthly"
	tokenSecret = "cfk_ws_s3cr3t-value-never-echoed"
)

var allCapabilities = []costfluent.Capability{
	costfluent.CapabilityViewWorkspace, costfluent.CapabilityViewCosts, costfluent.CapabilityViewCostReports, costfluent.CapabilityViewBudgets, costfluent.CapabilityViewAnomalies,
}

// token is what the fake API knows about one API token, as GET /v1/me reports it.
type token struct {
	workspace    string // empty for an organization token
	capabilities []costfluent.Capability
}

// fakeAPI answers the Public API operations the tools call, enforcing a token's capabilities and
// workspace the way the Costfluent API does: a missing capability or another workspace
// is 403 forbidden in the ApiError shape, and a listing without a workspace is narrowed to a
// workspace token's own.
type fakeAPI struct {
	t      *testing.T
	tokens map[string]token

	mu       sync.Mutex
	auth     []string
	requests []*http.Request

	// override answers every request but /v1/me when set.
	override func(http.ResponseWriter, *http.Request) bool
}

func newFakeAPI(t *testing.T, tokens map[string]token) (*fakeAPI, *httptest.Server) {
	api := &fakeAPI{t: t, tokens: tokens}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv
}

func problem(w http.ResponseWriter, status int, code, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status, "code": code, "detail": detail,
		"retryable": false, "traceId": "trace-1",
	})
}

func reply(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (api *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.auth = append(api.auth, r.Header.Get("Authorization"))
	api.requests = append(api.requests, r)
	api.mu.Unlock()

	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, known := api.tokens[secret]
	if !ok || !known {
		problem(w, http.StatusUnauthorized, "unauthorized", "Authentication Error", "The API token is invalid or expired")
		return
	}
	if r.URL.Path == "/v1/me" {
		me := map[string]any{"organizationId": orgID, "workspaceId": nil, "tokenType": "organization",
			"tokenName": "assistant", "capabilities": tok.capabilities}
		if tok.workspace != "" {
			me["workspaceId"], me["tokenType"] = tok.workspace, "workspace"
		}
		reply(w, me)
		return
	}
	if api.override != nil && api.override(w, r) {
		return
	}

	need := map[string]costfluent.Capability{
		"/v1/workspaces":                         costfluent.CapabilityViewWorkspace,
		"/v1/costs":                              costfluent.CapabilityViewCosts,
		"/v1/cost-reports":                       costfluent.CapabilityViewCostReports,
		"/v1/cost-reports/" + reportID + "/data": costfluent.CapabilityViewCosts,
		"/v1/budgets":                            costfluent.CapabilityViewBudgets,
		"/v1/anomalies":                          costfluent.CapabilityViewAnomalies,
	}[r.URL.Path]
	if need == "" {
		problem(w, http.StatusNotFound, "not_found", "Not Found", "no such route in the fake API: "+r.URL.Path)
		return
	}
	if !slices.Contains(tok.capabilities, need) {
		problem(w, http.StatusForbidden, "forbidden", "Authentication Error", "API token lacks capability: "+string(need))
		return
	}
	ws := r.URL.Query().Get("workspaceId")
	if tok.workspace != "" {
		if ws != "" && ws != tok.workspace {
			problem(w, http.StatusForbidden, "forbidden", "Authentication Error", "Workspace token cannot access other workspaces")
			return
		}
		ws = tok.workspace
	}

	switch r.URL.Path {
	case "/v1/workspaces":
		visible := []string{ownWS, otherWS}
		if tok.workspace != "" {
			visible = []string{tok.workspace}
		}
		var data []map[string]any
		for _, id := range visible {
			data = append(data, map[string]any{"id": id, "name": id, "currency": "USD", "timeZone": "UTC", "createdAt": "2026-01-01T00:00:00Z"})
		}
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/workspaces"}, "data": data})
	case "/v1/costs":
		dims := map[string]string{"workspace": ws}
		for _, g := range r.URL.Query()["groupBy"] {
			dims[g] = "value-of-" + g
		}
		reply(w, map[string]any{
			"links": map[string]any{"self": "/v1/costs", "next": "/v1/costs?page=2"},
			"data":  []map[string]any{{"date": "2026-10-01", "cost": "12.50000000", "currency": "USD", "dimensions": dims}},
			"meta": map[string]any{"startDate": r.URL.Query().Get("startDate"), "endDate": r.URL.Query().Get("endDate"),
				"granularity": "day", "totalRecords": 2, "totalCost": "25.00000000", "currency": "USD"},
		})
	case "/v1/cost-reports":
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/cost-reports"},
			"data": []map[string]any{{"id": reportID, "title": "report in " + ws, "chartType": "bar", "dateBin": "day",
				"dateInterval": "thisMonth", "groupBy": []any{}, "settings": map[string]any{}}}})
	case "/v1/cost-reports/" + reportID + "/data":
		reply(w, map[string]any{"summary": map[string]any{"totalCost": "99.00000000", "currency": "USD", "measure": "cost"},
			"series": []any{}, "breakdown": []map[string]any{{"keys": []string{ws}, "cost": "99.00000000", "percent": 100}}})
	case "/v1/budgets":
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/budgets"},
			"data": []map[string]any{{"id": "bud_1", "name": "budget in " + ws, "amount": "100.00000000", "currency": "USD",
				"startDate": "2026-01-01", "intervalCount": 1, "intervalUnit": "month", "status": "active", "state": "onTrack",
				"currentSpend": "40.00000000", "budgetedToDate": "45.00000000",
				"unconverted": []map[string]any{{"currency": "EUR", "amount": "12.34567890"}}, "historyFloor": "2026-04-06"}}})
	case "/v1/anomalies":
		var data []map[string]any
		for _, id := range []string{ownWS, otherWS} {
			if ws == "" || id == ws {
				data = append(data, map[string]any{"id": "anm_" + id, "workspaceId": id, "providerId": "prv_1",
					"anomalyDate": "2026-10-10", "serviceName": "AmazonEC2", "anomalyType": "spike", "severity": "high",
					"expectedCost": "10.00000000", "actualCost": "30.00000000", "deviationPercent": 200, "currency": "USD", "status": "open"})
			}
		}
		reply(w, map[string]any{"links": map[string]any{"self": "/v1/anomalies"}, "data": data})
	default:
		panic(fmt.Sprintf("unrouted %s", r.URL.Path))
	}
}

// lastQuery is the query of the last request the fake API received for path.
func (api *fakeAPI) lastQuery(path string) url.Values {
	api.mu.Lock()
	defer api.mu.Unlock()
	for i := len(api.requests) - 1; i >= 0; i-- {
		if api.requests[i].URL.Path == path {
			return api.requests[i].URL.Query()
		}
	}
	return nil
}

func (api *fakeAPI) requestCount(path string) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	n := 0
	for _, r := range api.requests {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}
