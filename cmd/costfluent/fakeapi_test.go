package main

import (
	"encoding/json"
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
	ownWS       = "ws_own"
	reportID    = "rpt_monthly"
	tokenSecret = "cfk_ws_cli-s3cr3t-never-printed"
)

// token is what the fake API knows about one API token, as GET /v1/me reports it.
type token struct {
	name         string
	org          bool // an organization token, which names no workspace of its own
	capabilities []costfluent.Capability
	// agentWrites is its organization's setting letting AI agents write: GET /v1/me reports it, and
	// a marked write while it is off is refused with 403 agent_writes_disabled, as the Public API does.
	agentWrites bool
}

// fakeAPI answers GET /v1/me and the reads each tool makes, refusing an unknown token with 401 and
// a missing capability with 403 in the ApiError shape, as the Public API does. It is the server
// package's fake reduced to what a command needs, which a test file there cannot export.
type fakeAPI struct {
	tokens map[string]token

	mu       sync.Mutex
	requests []*http.Request

	// override answers every request but /v1/me when it returns true.
	override func(http.ResponseWriter, *http.Request) bool
}

func newFakeAPI(t *testing.T, tokens map[string]token) (*fakeAPI, *httptest.Server) {
	api := &fakeAPI{tokens: tokens}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv
}

func problem(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body["status"] = status
	_ = json.NewEncoder(w).Encode(body)
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// routes are the reads the tools make: the capability each needs and its answer. Amounts carry more
// digits than a float64 keeps, and names carry control characters, as customer-written names can.
type route struct {
	need   costfluent.Capability
	body   string
	status int // 200 when zero
}

var routes = map[string]route{
	"/v1/workspaces": {costfluent.CapabilityViewWorkspace,
		`{"links":{"self":"/v1/workspaces"},"data":[{"id":"ws_own","name":"Prod\u001b[31m","currency":"USD","timeZone":"UTC","createdAt":"2026-01-01T00:00:00Z"}]}`, 0},
	"/v1/costs": {costfluent.CapabilityViewCosts,
		`{"links":{"self":"/v1/costs","next":"/v1/costs?page=2"},"data":[{"date":"2026-10-01","cost":"12345678901234.12345678","currency":"USD","dimensions":{"ServiceName":"AmazonEC2"}}],"meta":{"startDate":"2026-10-01","endDate":"2026-10-31","granularity":"day","totalRecords":2,"totalCost":"25.00000000","currency":"USD"}}`, 0},
	"/v1/cost-reports": {costfluent.CapabilityViewCostReports,
		`{"links":{"self":"/v1/cost-reports"},"data":[{"id":"rpt_monthly","title":"Monthly","chartType":"bar","dateBin":"day","dateInterval":"thisMonth","groupBy":[],"settings":{},"isDefault":true}]}`, 0},
	"/v1/cost-reports/" + reportID + "/data": {costfluent.CapabilityViewCosts,
		`{"summary":{"totalCost":"99.00000000","currency":"USD","measure":"cost"},"series":[],"breakdown":[{"keys":["ws_own"],"cost":"99.00000000","percent":100}]}`, 0},
	"/v1/annotations": {costfluent.CapabilityViewCostReports,
		`{"links":{"self":"/v1/annotations"},"data":[{"id":"ann_note","date":"2026-10-20","source":"manual","text":"deploy","costReportIds":["rpt_monthly"],"createdAt":"2026-10-21T08:00:00Z"}]}`, 0},
	"/v1/budgets": {costfluent.CapabilityViewBudgets,
		`{"links":{"self":"/v1/budgets"},"data":[{"id":"bud_1","name":"Production","amount":"100.00000000","currency":"USD","startDate":"2026-01-01","intervalCount":1,"intervalUnit":"month","status":"active","state":"onTrack","currentSpend":"40.00000000"}]}`, 0},
	"/v1/anomalies": {costfluent.CapabilityViewAnomalies,
		`{"links":{"self":"/v1/anomalies"},"data":[{"id":"anm_1","workspaceId":"ws_own","providerId":"prv_1","anomalyDate":"2026-10-10","serviceName":"AmazonEC2","anomalyType":"spike","severity":"high","expectedCost":"10.00000000","actualCost":"30.00000000","deviationPercent":200,"currency":"USD","status":"open"}]}`, 0},
	"/v1/savings-items": {costfluent.CapabilityViewSavingsItems,
		`{"links":{"self":"/v1/savings-items"},"data":[{"id":"sav_1","title":"Idle volume","estimatedMonthlySaving":"120.50000000","currency":"USD","confidence":"high","risk":"low","effort":"low","status":"open","source":"curated","kind":"idle","createdAt":"2026-10-01T00:00:00Z"}]}`, 0},
	"/v1/commitments": {costfluent.CapabilityViewCosts,
		`{"links":{"self":"/v1/commitments"},"data":[{"id":"ri-0abc","providerId":"prv_1","provider":"aws","type":"ReservedInstance","status":"active","termYears":1,"currency":"USD"}]}`, 0},
	"/v1/commitments/coverage": {costfluent.CapabilityViewCosts,
		`{"currency":"USD","granularity":"month","data":[],"totals":[{"provider":"aws","coveragePercent":80}]}`, 0},
	"/v1/kubernetes/efficiency": {costfluent.CapabilityViewCosts,
		`{"currency":"USD","level":"cluster","truncated":false,"unconverted":[],"rows":[{"cluster":"prod","cost":"300.00000000","idleCost":"75.00000000"}]}`, 0},
}

func (api *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.requests = append(api.requests, r)
	api.mu.Unlock()

	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, known := api.tokens[secret]
	if !ok || !known {
		problem(w, http.StatusUnauthorized, map[string]any{"code": "unauthorized", "title": "Authentication Error",
			"detail": "The API token is invalid or expired", "retryable": false, "traceId": "trace-401"})
		return
	}
	if r.URL.Path == "/v1/me" {
		me := map[string]any{"organizationId": "org_1", "workspaceId": ownWS, "tokenType": "workspace",
			"tokenName": tok.name, "capabilities": tok.capabilities, "agentWritesAllowed": tok.agentWrites}
		if tok.org {
			me["workspaceId"], me["tokenType"] = nil, "organization"
		}
		body, _ := json.Marshal(me)
		reply(w, string(body))
		return
	}
	if api.override != nil && api.override(w, r) {
		return
	}
	if _, marked := r.Header[http.CanonicalHeaderKey(costfluent.AgentHeader)]; marked && !tok.agentWrites && r.Method != http.MethodGet {
		problem(w, http.StatusForbidden, map[string]any{"code": "agent_writes_disabled", "title": "Forbidden",
			"detail": "This organization does not let AI agents create and change objects", "retryable": false, "traceId": "trace-403"})
		return
	}
	route, ok := routes[r.URL.Path]
	if write, isWrite := writeRoutes[r.Method+" "+r.URL.Path]; isWrite {
		route, ok = write, true
	} else if r.Method != http.MethodGet {
		ok = false
	}
	if !ok {
		problem(w, http.StatusNotFound, map[string]any{"code": "not_found", "detail": "no such route: " + r.URL.Path})
		return
	}
	if !slices.Contains(tok.capabilities, route.need) {
		problem(w, http.StatusForbidden, map[string]any{"code": "forbidden", "detail": "API token lacks capability: " + string(route.need)})
		return
	}
	if route.status != 0 {
		if route.body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(route.status)
		_, _ = w.Write([]byte(route.body))
		return
	}
	reply(w, route.body)
}

// writeRoutes are the writes the write tools make and the reads by ID around them, by method and path.
var writeRoutes = map[string]route{
	"POST /v1/annotations": {costfluent.CapabilityUpdateCostReport,
		`{"id":"ann_new","date":"2026-11-02","source":"manual","text":"deploy","costReportIds":["rpt_monthly"],"createdAt":"2026-11-02T08:00:00Z"}`, http.StatusCreated},
	"POST /v1/cost-reports": {costfluent.CapabilityCreateCostReport, `{"id":"rpt_new","title":"EC2"}`, http.StatusCreated},
	"GET /v1/cost-reports/rpt_new": {costfluent.CapabilityViewCostReports,
		`{"id":"rpt_new","title":"EC2","chartType":"bar","dateBin":"day","dateInterval":"thisMonth","groupBy":[],"settings":{}}`, 0},
	"PUT /v1/cost-reports/rpt_monthly": {costfluent.CapabilityUpdateCostReport,
		`{"id":"rpt_monthly","title":"Renamed","chartType":"bar","dateBin":"day","dateInterval":"thisMonth","groupBy":[],"settings":{}}`, 0},
	"POST /v1/budgets": {costfluent.CapabilityCreateBudget,
		`{"id":"bud_new","name":"Platform","amount":"5000.00000000","currency":"USD","startDate":"2026-11-01","intervalCount":1,"intervalUnit":"month","status":"active","state":"onTrack"}`, http.StatusCreated},
	"PUT /v1/budgets/bud_1": {costfluent.CapabilityUpdateBudget,
		`{"id":"bud_1","name":"Renamed","amount":"100.00000000","currency":"USD","startDate":"2026-01-01","intervalCount":1,"intervalUnit":"month","status":"active","state":"onTrack"}`, 0},
	"POST /v1/cost-alerts": {costfluent.CapabilityManageAlerts, `{"id":"cal_new","name":"EC2"}`, http.StatusCreated},
	"GET /v1/cost-alerts/cal_new": {costfluent.CapabilityViewCosts,
		`{"id":"cal_new","name":"EC2","thresholdType":"absolute","thresholdValue":"1000.00000000","appIds":[],"evaluationFrequencyMinutes":60,"status":"active","createdAt":"2026-11-02T08:00:00Z"}`, 0},
	"PUT /v1/cost-alerts/cal_1": {costfluent.CapabilityManageAlerts,
		`{"id":"cal_1","name":"EC2","thresholdType":"absolute","thresholdValue":"2000.00000000","appIds":[],"evaluationFrequencyMinutes":60,"status":"active","createdAt":"2026-11-02T08:00:00Z"}`, 0},
	"POST /v1/cost-alerts/cal_1/pause":  {costfluent.CapabilityManageAlerts, "", http.StatusNoContent},
	"POST /v1/cost-alerts/cal_1/resume": {costfluent.CapabilityManageAlerts, "", http.StatusNoContent},
	"POST /v1/dashboards":               {costfluent.CapabilityCreateDashboard, `{"id":"dsh_new","title":"Platform"}`, http.StatusCreated},
	"GET /v1/dashboards/dsh_new": {costfluent.CapabilityViewDashboards,
		`{"id":"dsh_new","title":"Platform","isDefault":false,"dateInterval":"last30Days","dateBin":"day","widgets":[],"createdAt":"2026-11-02T08:00:00Z"}`, 0},
	"GET /v1/dashboards/dsh_1": {costfluent.CapabilityViewDashboards,
		`{"id":"dsh_1","title":"Platform","isDefault":false,"dateInterval":"last30Days","dateBin":"day","widgets":[],"createdAt":"2026-11-02T08:00:00Z"}`, 0},
	"POST /v1/dashboards/dsh_1/widgets": {costfluent.CapabilityUpdateDashboard, `{"id":"wdg_new","title":"Monthly"}`, http.StatusCreated},
	"GET /v1/dashboards/dsh_1/widgets/wdg_new": {costfluent.CapabilityViewDashboards,
		`{"id":"wdg_new","type":"costReport","costReportId":"rpt_monthly","display":"chart","title":"Monthly","position":{"x":0,"y":0,"w":6,"h":4}}`, 0},
	"POST /v1/anomalies/anm_1/acknowledge": {costfluent.CapabilityAcknowledgeAnomaly, "", http.StatusNoContent},
	"POST /v1/savings-items/sav_1/reject":  {costfluent.CapabilityRespondToSavingsItem, "", http.StatusNoContent},
	"PUT /v1/savings-items/sav_1/owner": {costfluent.CapabilityManageSavingsItems,
		`{"id":"sav_1","title":"Idle volume","evidence":"unattached for 30 days","estimatedMonthlySaving":"12.00000000","currency":"USD","costBasis":"onDemand","owner":"Platform team","confidence":"high","risk":"low","effort":"low","status":"accepted","source":"operator","createdAt":"2026-10-01T00:00:00Z"}`, 0},
}

// last is the last request the fake API received for path, or nil.
func (api *fakeAPI) last(path string) *http.Request {
	api.mu.Lock()
	defer api.mu.Unlock()
	for i := len(api.requests) - 1; i >= 0; i-- {
		if api.requests[i].URL.Path == path {
			return api.requests[i]
		}
	}
	return nil
}

func (api *fakeAPI) count() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.requests)
}

func (api *fakeAPI) query(t *testing.T, path string) url.Values {
	t.Helper()
	r := api.last(path)
	if r == nil {
		t.Fatalf("the API received no request for %s", path)
	}
	return r.URL.Query()
}
