package server

import (
	"encoding/json"
	"fmt"
	"io"
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
	foreignWS   = "ws_foreign" // a workspace of another organization
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
	// exchanges are the requests as sent: their method, path, query, headers and JSON body.
	exchanges []exchange
	// agentWrites is the organization's setting letting AI agents write, which GET /v1/me reports
	// and which refuses a marked write while it is off, as the Public API does.
	agentWrites bool

	// override answers every request but /v1/me when set.
	override func(http.ResponseWriter, *http.Request) bool
}

// exchange is one request the fake API received.
type exchange struct {
	method, path string
	query        url.Values
	header       http.Header
	body         map[string]any
}

func (api *fakeAPI) setAgentWrites(on bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.agentWrites = on
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
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			api.t.Errorf("%s %s: the body is not a JSON object: %s", r.Method, r.URL.Path, raw)
		}
	}
	api.mu.Lock()
	api.auth = append(api.auth, r.Header.Get("Authorization"))
	api.requests = append(api.requests, r)
	api.exchanges = append(api.exchanges, exchange{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	agentWrites := api.agentWrites
	api.mu.Unlock()

	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, known := api.tokens[secret]
	if !ok || !known {
		problem(w, http.StatusUnauthorized, "unauthorized", "Authentication Error", "The API token is invalid or expired")
		return
	}
	if r.URL.Path == "/v1/me" {
		me := map[string]any{"organizationId": orgID, "workspaceId": nil, "tokenType": "organization",
			"tokenName": "assistant", "capabilities": tok.capabilities, "agentWritesAllowed": agentWrites}
		if tok.workspace != "" {
			me["workspaceId"], me["tokenType"] = tok.workspace, "workspace"
		}
		reply(w, me)
		return
	}
	if api.override != nil && api.override(w, r) {
		return
	}
	if _, marked := r.Header[http.CanonicalHeaderKey(costfluent.AgentHeader)]; marked && !agentWrites &&
		r.Method != http.MethodGet && r.Method != http.MethodHead {
		problem(w, http.StatusForbidden, agentWritesDisabled, "Forbidden",
			"This organization does not let AI agents create and change objects")
		return
	}
	if template, ids := templated(r.URL.Path); r.Method != http.MethodGet || len(ids) > 0 && template != "/v1/cost-reports/{id}/data" {
		api.serveObject(w, r, tok, template, ids, body)
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
		reply(w, map[string]any{"links": twoPages(r), "data": data})
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
		reply(w, map[string]any{"links": twoPages(r),
			"data": []map[string]any{{"id": reportID, "title": "report in " + ws, "chartType": "bar", "dateBin": "day",
				"dateInterval": "thisMonth", "groupBy": []any{}, "settings": map[string]any{}, "isDefault": true}}})
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
		reply(w, map[string]any{"links": twoPages(r),
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
		reply(w, map[string]any{"links": twoPages(r), "data": data})
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
		"kind":   {"rightsizing", "idle", "generational", "storage", "commitmentPurchase", "extendedSupport", "pricingTier", "other"},
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

// templated is path with each object ID, a segment holding an underscore, replaced by {id}, and
// those IDs in order.
func templated(path string) (string, []string) {
	segments := strings.Split(path, "/")
	var ids []string
	for i, s := range segments {
		if strings.Contains(s, "_") {
			ids = append(ids, s)
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/"), ids
}

// objects are the workspace each object the write routes know lives in. An ID ending _foreign is
// another organization's, which the API does not find.
var objects = map[string]string{
	reportID: ownWS, "rpt_other": otherWS, "rpt_new": ownWS,
	"bud_1": ownWS, "bud_other": otherWS,
	"cal_1": ownWS, "cal_other": otherWS, "cal_new": ownWS,
	"dsh_1": ownWS, "dsh_other": otherWS, "dsh_new": ownWS, "wdg_new": ownWS,
	"anm_" + ownWS: ownWS, "anm_" + otherWS: otherWS,
	"sav_1": ownWS, "sav_other": otherWS, closedFinding: ownWS,
}

// closedFinding is a savings finding already closed, whose status and owner every write is
// refused with the API's 409 conflict.
const closedFinding = "sav_closed"

// objectETag is the version every object the fake API reads by ID carries.
const objectETag = `"v1"`

// objectRoute is one operation on an object: the capabilities it needs and its answer.
type objectRoute struct {
	need   []costfluent.Capability
	status int
	answer func(ids []string, body map[string]any) any // nil answers no body
}

var (
	fakeReport = func(ids []string, _ map[string]any) any {
		return map[string]any{"id": ids[0], "title": "report " + ids[0], "chartType": "bar", "chartAxis": "date", "dateBin": "day",
			"dateInterval": "thisMonth", "groupBy": []any{}, "isDefault": false,
			"settings": map[string]any{"amortize": false, "unitCostMetricId": "bmt_requests", "unitCostLabelFilter": []map[string]any{
				{"key": "region", "values": []string{"eu"}}}, "comparisonStartDate": "2026-09-01", "comparisonEndDate": "2026-09-30"}}
	}
	fakeBudget = func(ids []string, _ map[string]any) any {
		id := "bud_new"
		if len(ids) > 0 {
			id = ids[0]
		}
		return map[string]any{"id": id, "name": "budget " + id, "amount": "100.00000000", "currency": "USD", "measure": "cost",
			"startDate": "2026-01-01", "intervalCount": 1, "intervalUnit": "month", "status": "active", "state": "onTrack",
			"spendAvailability": "computed", "createdAt": "2026-01-01T00:00:00Z",
			"alerts": []map[string]any{{"thresholdPercent": 80, "basis": "actual", "isTriggered": true}, {"thresholdPercent": 100, "basis": "forecast", "isTriggered": false}}}
	}
	fakeAlert = func(ids []string, _ map[string]any) any {
		return map[string]any{"id": ids[0], "name": "alert " + ids[0], "thresholdType": "absolute", "thresholdValue": "1000.00000000",
			"appIds": []string{"app_slack"}, "providerIds": []string{"prv_1"}, "evaluationFrequencyMinutes": 60, "status": "active",
			"createdAt": "2026-01-01T00:00:00Z"}
	}
	fakeDashboard = func(ids []string, _ map[string]any) any {
		return map[string]any{"id": ids[0], "title": "dashboard " + ids[0], "isDefault": false, "dateInterval": "last30Days", "dateBin": "day",
			"createdAt": "2026-01-01T00:00:00Z", "widgets": []map[string]any{
				{"id": "wdg_1", "type": "costReport", "costReportId": reportID, "display": "chart", "title": "top", "position": map[string]any{"x": 0, "y": 0, "w": 12, "h": 4}},
				{"id": "wdg_2", "type": "text", "content": "notes", "title": "notes", "position": map[string]any{"x": 6, "y": 4, "w": 6, "h": 3}}}}
	}
	created = func(id string) func([]string, map[string]any) any {
		return func([]string, map[string]any) any { return map[string]any{"id": id, "title": "created"} }
	}
)

// objectRoutes are the write routes the tools call and the reads by ID they make around them.
var objectRoutes = map[string]objectRoute{
	"POST /v1/annotations": {[]costfluent.Capability{costfluent.CapabilityUpdateCostReport}, http.StatusCreated, func(_ []string, b map[string]any) any {
		return map[string]any{"id": "ann_new", "date": b["date"], "source": "manual", "text": b["text"], "costReportIds": b["costReportIds"],
			"createdAt": "2026-11-02T08:00:00Z"}
	}},
	"POST /v1/cost-reports":            {[]costfluent.Capability{costfluent.CapabilityCreateCostReport}, http.StatusCreated, created("rpt_new")},
	"GET /v1/cost-reports/{id}":        {[]costfluent.Capability{costfluent.CapabilityViewCostReports}, http.StatusOK, fakeReport},
	"PUT /v1/cost-reports/{id}":        {[]costfluent.Capability{costfluent.CapabilityUpdateCostReport}, http.StatusOK, fakeReport},
	"POST /v1/budgets":                 {[]costfluent.Capability{costfluent.CapabilityCreateBudget}, http.StatusCreated, fakeBudget},
	"GET /v1/budgets/{id}":             {[]costfluent.Capability{costfluent.CapabilityViewBudgets}, http.StatusOK, fakeBudget},
	"PUT /v1/budgets/{id}":             {[]costfluent.Capability{costfluent.CapabilityUpdateBudget}, http.StatusOK, fakeBudget},
	"POST /v1/cost-alerts":             {[]costfluent.Capability{costfluent.CapabilityManageAlerts}, http.StatusCreated, created("cal_new")},
	"GET /v1/cost-alerts/{id}":         {[]costfluent.Capability{costfluent.CapabilityViewCosts}, http.StatusOK, fakeAlert},
	"PUT /v1/cost-alerts/{id}":         {[]costfluent.Capability{costfluent.CapabilityManageAlerts}, http.StatusOK, fakeAlert},
	"POST /v1/cost-alerts/{id}/pause":  {[]costfluent.Capability{costfluent.CapabilityManageAlerts}, http.StatusNoContent, nil},
	"POST /v1/cost-alerts/{id}/resume": {[]costfluent.Capability{costfluent.CapabilityManageAlerts}, http.StatusNoContent, nil},
	"POST /v1/dashboards":              {[]costfluent.Capability{costfluent.CapabilityCreateDashboard}, http.StatusCreated, created("dsh_new")},
	"GET /v1/dashboards/{id}":          {[]costfluent.Capability{costfluent.CapabilityViewDashboards}, http.StatusOK, fakeDashboard},
	"POST /v1/dashboards/{id}/widgets": {[]costfluent.Capability{costfluent.CapabilityViewCostReports, costfluent.CapabilityUpdateDashboard},
		http.StatusCreated, created("wdg_new")},
	"GET /v1/dashboards/{id}/widgets/{id}": {[]costfluent.Capability{costfluent.CapabilityViewDashboards}, http.StatusOK, func(ids []string, _ map[string]any) any {
		return map[string]any{"id": ids[1], "type": "costReport", "costReportId": reportID, "display": "chart", "title": "widget " + ids[1],
			"position": map[string]any{"x": 0, "y": 7, "w": 6, "h": 4}}
	}},
	"POST /v1/anomalies/{id}/acknowledge":   {[]costfluent.Capability{costfluent.CapabilityAcknowledgeAnomaly}, http.StatusNoContent, nil},
	"POST /v1/savings-items/{id}/accept":    {[]costfluent.Capability{costfluent.CapabilityRespondToSavingsItem}, http.StatusNoContent, nil},
	"POST /v1/savings-items/{id}/implement": {[]costfluent.Capability{costfluent.CapabilityRespondToSavingsItem}, http.StatusNoContent, nil},
	"POST /v1/savings-items/{id}/reject":    {[]costfluent.Capability{costfluent.CapabilityRespondToSavingsItem}, http.StatusNoContent, nil},
	"PUT /v1/savings-items/{id}/owner": {[]costfluent.Capability{costfluent.CapabilityManageSavingsItems}, http.StatusOK, func(ids []string, b map[string]any) any {
		return map[string]any{"id": ids[0], "title": "finding " + ids[0], "evidence": "CPU under 5% for 30 days", "estimatedMonthlySaving": "120.50000000",
			"currency": "USD", "costBasis": "onDemand", "owner": b["owner"], "confidence": "high", "risk": "low", "effort": "low",
			"status": "accepted", "source": "awsCostOptimizationHub", "createdAt": "2026-10-01T00:00:00Z"}
	}},
}

// serveObject answers a write, or a read of one object by ID, holding the token to the route's
// capabilities and to its own workspace as the Public API does: another workspace is 403 forbidden,
// and another organization's workspace or object is 404 not found.
func (api *fakeAPI) serveObject(w http.ResponseWriter, r *http.Request, tok token, template string, ids []string, body map[string]any) {
	route, ok := objectRoutes[r.Method+" "+template]
	if !ok {
		problem(w, http.StatusNotFound, "not_found", "Not Found", "no such route in the fake API: "+r.Method+" "+r.URL.Path)
		return
	}
	for _, need := range route.need {
		if !slices.Contains(tok.capabilities, need) {
			problem(w, http.StatusForbidden, "forbidden", "Authentication Error", "API token lacks capability: "+string(need))
			return
		}
	}
	ws := r.URL.Query().Get("workspaceId")
	if named, ok := body["workspaceId"].(string); ok && ws == "" {
		ws = named
	}
	if ws == foreignWS {
		problem(w, http.StatusNotFound, "not_found", "Not Found", "The workspace was not found")
		return
	}
	if tok.workspace != "" && ws != "" && ws != tok.workspace {
		problem(w, http.StatusForbidden, "forbidden", "Authentication Error", "Workspace token cannot access other workspaces")
		return
	}
	for _, id := range ids {
		owner, known := objects[id]
		if !known {
			problem(w, http.StatusNotFound, "not_found", "Not Found", "The object "+id+" was not found")
			return
		}
		if tok.workspace != "" && owner != tok.workspace {
			problem(w, http.StatusForbidden, "forbidden", "Authentication Error", "Workspace token cannot access other workspaces")
			return
		}
	}
	if r.Method != http.MethodGet && slices.Contains(ids, closedFinding) {
		problem(w, http.StatusConflict, "conflict", "Conflict", "The finding is closed, so its status and owner cannot change")
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("ETag", objectETag)
	}
	if route.answer == nil {
		w.WriteHeader(route.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_ = json.NewEncoder(w).Encode(route.answer(ids, body))
}

// received is every request the fake API received, in order.
func (api *fakeAPI) received() []exchange {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.exchanges)
}
