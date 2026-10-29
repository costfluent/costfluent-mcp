package server

import (
	"encoding/json"
	"fmt"
	"maps"
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
	costfluent.CapabilityViewSavingsItems,
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
		"/v1/annotations":                        costfluent.CapabilityViewCostReports,
		"/v1/budgets":                            costfluent.CapabilityViewBudgets,
		"/v1/anomalies":                          costfluent.CapabilityViewAnomalies,
		"/v1/savings-items":                      costfluent.CapabilityViewSavingsItems,
		"/v1/commitments":                        costfluent.CapabilityViewCosts,
		"/v1/commitments/coverage":               costfluent.CapabilityViewCosts,
		"/v1/kubernetes/efficiency":              costfluent.CapabilityViewCosts,
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
	q := r.URL.Query()
	for param, values := range enums[r.URL.Path] {
		if v := q.Get(param); v != "" && !slices.Contains(values, v) {
			problem(w, http.StatusBadRequest, "validation_failed", "Validation Error",
				fmt.Sprintf("%s must be one of %s", param, strings.Join(values, ", ")))
			return
		}
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
	case "/v1/annotations":
		reply(w, map[string]any{"links": twoPages(r), "data": []map[string]any{
			{"id": "ann_note", "date": "2026-10-20", "source": "manual", "text": "note in " + ws,
				"costReportIds": []string{reportID}, "createdByUserId": "usr_1", "createdAt": "2026-10-21T08:00:00Z"},
			{"id": "ann_firing", "date": "2026-10-22", "source": "budgetAlert", "costReportIds": []string{reportID},
				"budgetId": "bud_1", "sourceName": "budget in " + ws, "thresholdPercent": 80, "thresholdBasis": "forecast",
				"createdAt": "2026-10-22T00:05:00Z"}}})
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
	case "/v1/savings-items":
		reply(w, map[string]any{"links": twoPages(r), "data": []map[string]any{{
			"id": "sav_1", "title": "finding in " + ws, "evidence": "CPU under 5% for 30 days", "estimatedMonthlySaving": "120.50000000",
			"currency": "USD", "costBasis": "onDemand", "owner": "platform", "confidence": "high", "risk": "low", "effort": "low",
			"status": "open", "source": "awsCostOptimizationHub", "kind": "rightsizing", "resource": "i-0abc",
			"createdAt": "2026-10-01T00:00:00Z"}}})
	case "/v1/commitments":
		reply(w, map[string]any{"links": twoPages(r), "data": []map[string]any{{
			"id": "ri-0abc", "providerId": "prv_1", "providerName": "AWS production", "provider": "aws", "type": "ReservedInstance",
			"description": "commitment in " + ws, "account": "111111111111", "region": "eu-central-1", "service": "Amazon EC2",
			"status": "expiring", "providerStatus": "Active", "endsAt": "2026-11-15T00:00:00Z", "daysUntilExpiry": 21, "termYears": 1,
			"paymentOption": "No Upfront", "hourlyCommitment": "0.25000000", "utilizationPercentage": 87.5, "unusedCost": "22.50000000",
			"currency": "USD", "utilizationFrom": "2026-09-25", "utilizationTo": "2026-10-24", "lastReadAt": "2026-10-25T00:00:00Z"}}})
	case "/v1/commitments/coverage":
		figures := map[string]any{"provider": "aws-" + ws, "coveredOnDemandCost": "80.00000000", "eligibleOnDemandCost": "100.00000000",
			"actualCost": "60.00000000", "usedCommitment": "60.00000000", "unusedCommitment": "5.00000000",
			"coveragePercent": 80, "effectiveSavingsRatePercent": 40, "utilizationPercent": 92.3}
		bin := map[string]any{"date": "2026-10-01"}
		maps.Copy(bin, figures)
		reply(w, map[string]any{"currency": "USD", "granularity": q.Get("granularity"), "data": []map[string]any{bin},
			"totals": []map[string]any{figures}, "historyFloor": "2026-04-06",
			"unconverted": []map[string]any{{"currency": "EUR", "amount": "12.34567890"}}})
	case "/v1/kubernetes/efficiency":
		reply(w, map[string]any{"currency": "USD", "level": q.Get("level"), "truncated": true, "unconverted": []any{},
			"rows": []map[string]any{{"cluster": "cluster-" + ws, "cost": "300.00000000", "idleCost": "75.00000000",
				"cpu":    map[string]any{"requested": 720, "used": 180, "peak": 400, "usedPercent": 25, "peakPercent": 55.6},
				"memory": map[string]any{"requested": 2880, "used": 1440, "peak": 2000, "usedPercent": 50, "peakPercent": 69.4}}}})
	default:
		panic(fmt.Sprintf("unrouted %s", r.URL.Path))
	}
}

// enums are the values the API takes for each enum parameter of a route. It refuses any other with a
// 400 naming them, as the Public API's enum validation does; coverage refuses cumulative.
var enums = map[string]map[string][]string{
	"/v1/savings-items": {
		"tab":    {"toReview", "inProgress", "verified", "closed"},
		"source": {"operator", "awsCostOptimizationHub", "azureAdvisor", "gcpRecommender", "curated", "billingAnalysis", "kubernetesAnalysis"},
		"kind":   {"rightsizing", "idle", "generational", "storage", "commitmentPurchase", "extendedSupport", "other"},
		"sort":   {"newest", "largestSaving"},
	},
	"/v1/commitments":           {"status": {"active", "expiring", "expired", "unknown"}},
	"/v1/commitments/coverage":  {"granularity": {"day", "week", "month", "quarter"}},
	"/v1/kubernetes/efficiency": {"level": {"cluster", "namespace", "workload"}},
}

// twoPages are the links of a listing two pages long: page 1 names the next, page 2 none.
func twoPages(r *http.Request) map[string]any {
	links := map[string]any{"self": r.URL.Path}
	if r.URL.Query().Get("page") == "1" {
		links["next"] = r.URL.Path + "?page=2"
	}
	return links
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
