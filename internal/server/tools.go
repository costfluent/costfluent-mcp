package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/docs"
)

// pageSize is the page every list tool reads: the largest the API serves.
const pageSize = 100

// Cost rows a query_costs call returns by default and at most. The default keeps one answer
// within an assistant's context; the maximum is the API's page.
const (
	defaultCostRows = 500
	maxCostRows     = costfluent.CostDataPageSize
)

type handlers struct {
	client *costfluent.Client
	me     *costfluent.Me
	docs   *docs.Source // the process's shared one when nil
}

func (h *handlers) tools() []toolDef {
	return []toolDef{
		{"list_workspaces", costfluent.CapabilityViewWorkspace, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_workspaces",
				Description: tenantText("List the workspaces this API token can read, with their currency and time zone. A workspace token sees only its own. Needs the viewWorkspace capability (“View workspaces”)."),
				Annotations: readOnly("List workspaces"),
			}, handle(h.listWorkspaces))
		}, reads},
		{"query_costs", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "query_costs",
				Description: tenantText("Query cost, usage or resource count over a date range, per period and optionally grouped, narrowed by a Costfluent cost filter. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Query costs"),
			}, handle(h.queryCosts))
		}, reads},
		{"list_cost_reports", costfluent.CapabilityViewCostReports, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_cost_reports",
				Description: tenantText("List a workspace's saved cost reports: their window, grouping, filter and settings. Needs the viewCostReports capability (“View cost reports”)."),
				Annotations: readOnly("List cost reports"),
			}, handle(h.listCostReports))
		}, reads},
		{"get_cost_report_data", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_cost_report_data",
				Description: tenantText("Compute the numbers a saved cost report shows now: its total, series per period and breakdown by group. A report too large for one answer keeps only its most recent periods, at least its last: series_omitted_periods counts the dates left out and series_from is the first date its series, comparison, unit cost and forecast keep, while the summary, breakdown and names always cover the whole window; query_costs over the report's startDate to endDate with a coarser granularity reads the earlier periods. Needs the viewCosts capability (“View costs”), and viewCostReports (“View cost reports”) too for a report that charts a business metric."),
				Annotations: readOnly("Get cost report data"),
			}, handle(h.getCostReportData))
		}, reads},
		{"list_annotations", costfluent.CapabilityViewCostReports, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_annotations",
				Description: tenantText("List a workspace's annotations by date: the notes members pin to a day on its cost reports and the markers a budget or cost alert leaves as it fires, each with its source, text or the budget or alert that fired, and the cost reports it stands on. Narrowed to one cost report or a date range; resolved annotations are left out unless include_resolved is true. Needs the viewCostReports capability (“View cost reports”)."),
				Annotations: readOnly("List annotations"),
			}, handle(h.listAnnotations))
		}, reads},
		{"list_budgets", costfluent.CapabilityViewBudgets, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_budgets",
				Description: tenantText("List a workspace's budgets with their measure, amount, current spend, amount budgeted to date and state (onTrack, atRisk, over), the spend in other currencies it does not count (unconverted), and the date its spend counts from when the plan's history starts inside the period (historyFloor). The current spend and state leave the unconverted spend out, so a budget can read onTrack while its scope is over. A budget whose measure is usage counts a quantity in its usageUnit, not money in its currency. Needs the viewBudgets capability (“View budgets”)."),
				Annotations: readOnly("List budgets"),
			}, handle(h.listBudgets))
		}, reads},
		{"list_anomalies", costfluent.CapabilityViewAnomalies, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_anomalies",
				Description: tenantText("List detected cost anomalies, newest first, with expected and actual cost. A workspace token sees only its workspace's. Needs the viewAnomalies capability (“View anomalies”)."),
				Annotations: readOnly("List anomalies"),
			}, handle(h.listAnomalies))
		}, reads},
		{"list_savings_items", costfluent.CapabilityViewSavingsItems, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_savings_items",
				Description: tenantText("List a workspace's savings queue: each finding's title, evidence, estimated monthly saving in its currency, confidence, risk, effort, status and source, newest first or largest saving first, narrowed by tab, source, kind, connection, the resources of a cost estimate or a minimum monthly saving. Needs the viewSavingsItems capability (“View savings”)."),
				Annotations: readOnly("List savings items"),
			}, handle(h.listSavingsItems))
		}, reads},
		{"list_commitments", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_commitments",
				Description: tenantText("List the reservations, savings plans and committed use discounts held through a workspace's connections, earliest end first, with their term, payment option, hourly commitment, utilization, unused cost and days until expiry. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("List commitments"),
			}, handle(h.listCommitments))
		}, reads},
		{"get_commitment_coverage", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_commitment_coverage",
				Description: tenantText("Read how much of a workspace's eligible usage its commitments covered over a date range, with the effective savings rate, utilization and unused commitment, per period and provider and in total, at amortized cost in the presentation currency. The figures leave out the spend listed as unconverted and, when the plan's history starts inside the range, the days before historyFloor. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Get commitment coverage"),
			}, handle(h.getCommitmentCoverage))
		}, reads},
		{"get_kubernetes_efficiency", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_kubernetes_efficiency",
				Description: tenantText("Read the cost, idle cost and CPU and memory used and peak against requested of a workspace's Kubernetes clusters over a date range, per cluster, namespace or workload, largest cost first. It returns at most 500 rows: truncated is true when more matched and the smallest were left out. The figures leave out the spend listed as unconverted and the days before historyFloor. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Get Kubernetes efficiency"),
			}, handle(h.getKubernetesEfficiency))
		}, reads},
		{"create_annotation", costfluent.CapabilityUpdateCostReport, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "create_annotation",
				Description: writeText("Pin a dated note to one to ten of a workspace's saved cost reports, as a member does from a report's chart, and return it as stored. Needs the updateCostReport capability (“Update cost reports”)."),
				Annotations: changes("Create annotation", false, false),
			}, handle(h.createAnnotation))
		}, writes},
		{"create_cost_report", costfluent.CapabilityCreateCostReport, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "create_cost_report",
				Description: writeText("Save a new cost report in a workspace, with its title, window, grouping, filter, chart and settings, and return it as stored. Needs the createCostReport capability (“Create cost reports”); reading the new report back needs viewCostReports (“View cost reports”)."),
				Annotations: changes("Create cost report", false, false),
			}, handle(h.createCostReport))
		}, writes},
		{"update_cost_report", costfluent.CapabilityUpdateCostReport, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "update_cost_report",
				Description: writeText("Change a saved cost report: only the fields given change, and every other field keeps its value. Changing a setting reads the report first so its unit cost metric and comparison window stay, which needs viewCostReports (“View cost reports”) too. Needs the updateCostReport capability (“Update cost reports”)."),
				Annotations: changes("Update cost report", true, true),
			}, handle(h.updateCostReport))
		}, writes},
		{"create_budget", costfluent.CapabilityCreateBudget, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "create_budget",
				Description: writeText("Create a budget in a workspace: one amount for each period of a cadence from a start date, over the whole workspace, a saved cost report or an allocation segment, with alert thresholds that notify the given channels. Needs the createBudget capability (“Create budgets”)."),
				Annotations: changes("Create budget", false, false),
			}, handle(h.createBudget))
		}, writes},
		{"update_budget", costfluent.CapabilityUpdateBudget, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "update_budget",
				Description: writeText("Change a budget: its name, every period's amount, the saved cost report or segment it measures, or more alert thresholds. Only the fields given change; adding thresholds keeps every threshold the budget has, which reads the budget first and needs viewBudgets (“View budgets”) too. Needs the updateBudget capability (“Update budgets”)."),
				Annotations: changes("Update budget", true, true),
			}, handle(h.updateBudget))
		}, writes},
		{"create_cost_alert", costfluent.CapabilityManageAlerts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "create_cost_alert",
				Description: writeText("Create a cost alert in a workspace: it watches cost, narrowed by connections and a filter, against a threshold and notifies its channels when the cost crosses it. Needs the manageAlerts capability (“Manage cost alerts”); reading the new alert back needs viewCosts (“View costs”)."),
				Annotations: changes("Create cost alert", false, false),
			}, handle(h.createCostAlert))
		}, writes},
		{"update_cost_alert", costfluent.CapabilityManageAlerts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "update_cost_alert",
				Description: writeText("Change a cost alert: only the fields given change. provider_ids replaces the connections it watches and names at least one; add_app_ids adds notification channels and keeps the ones it has, which reads the alert first and needs viewCosts (“View costs”) too. Needs the manageAlerts capability (“Manage cost alerts”)."),
				Annotations: changes("Update cost alert", true, true),
			}, handle(h.updateCostAlert))
		}, writes},
		{"pause_cost_alert", costfluent.CapabilityManageAlerts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "pause_cost_alert",
				Description: writeText("Stop evaluating a cost alert, so it notifies nothing until resume_cost_alert resumes it. Needs the manageAlerts capability (“Manage cost alerts”)."),
				Annotations: changes("Pause cost alert", true, true),
			}, handle(h.pauseCostAlert))
		}, writes},
		{"resume_cost_alert", costfluent.CapabilityManageAlerts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "resume_cost_alert",
				Description: writeText("Resume evaluating a cost alert pause_cost_alert paused. Needs the manageAlerts capability (“Manage cost alerts”)."),
				Annotations: changes("Resume cost alert", true, true),
			}, handle(h.resumeCostAlert))
		}, writes},
		{"create_dashboard", costfluent.CapabilityCreateDashboard, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "create_dashboard",
				Description: writeText("Create an empty dashboard in a workspace with its date range and bin, which add_dashboard_widget fills; it does not become the workspace's default. Needs the createDashboard capability (“Create dashboards”); reading it back needs viewDashboards (“View dashboards”)."),
				Annotations: changes("Create dashboard", false, false),
			}, handle(h.createDashboard))
		}, writes},
		{"add_dashboard_widget", costfluent.CapabilityUpdateDashboard, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "add_dashboard_widget",
				Description: writeText("Add a widget to a dashboard: a saved cost report drawn as a chart, a table or a KPI, or a text widget. Without a position it goes below the widgets the dashboard has, half its width, which reads the dashboard first and needs viewDashboards (“View dashboards”) too. Needs the updateDashboard capability (“Update dashboards”), and viewCostReports (“View cost reports”), which the API checks."),
				Annotations: changes("Add dashboard widget", false, false),
			}, handle(h.addDashboardWidget))
		}, writes},
		{"acknowledge_anomaly", costfluent.CapabilityAcknowledgeAnomaly, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "acknowledge_anomaly",
				Description: writeText("Mark a cost anomaly, as list_anomalies names it, as acknowledged, so it no longer counts as open. Needs the acknowledgeAnomaly capability (“Acknowledge anomalies”)."),
				Annotations: changes("Acknowledge anomaly", true, true),
			}, handle(h.acknowledgeAnomaly))
		}, writes},
		{"set_savings_item_status", costfluent.CapabilityRespondToSavingsItem, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "set_savings_item_status",
				Description: writeText("Answer a savings finding, as list_savings_items names it: accepted records that the workspace will act on an open finding, implemented that the change an accepted finding recommends was made, rejected declines an open or accepted finding with the reason given. A finding already closed is refused. Needs the respondToSavingsItem capability (“Respond to savings”)."),
				Annotations: changes("Set savings item status", true, true),
			}, handle(h.setSavingsItemStatus))
		}, writes},
		{"assign_savings_item_owner", costfluent.CapabilityManageSavingsItems, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "assign_savings_item_owner",
				Description: writeText("Set who acts on a savings finding, as list_savings_items names it, replacing the owner it has, and return the finding as stored. A finding already closed is refused. Needs the manageSavingsItems capability (“Manage savings”)."),
				Annotations: changes("Assign savings item owner", true, true),
			}, handle(h.assignSavingsItemOwner))
		}, writes},
		// The docs tools come last: Build serves them after the gated tools, to any token served one.
		{"search_docs", NoCapability, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "search_docs",
				Description: docsText("Search Costfluent's documentation for how to set up and use Costfluent: connecting AWS, Azure, Google Cloud and Kubernetes, the Terraform provider, the Public API, cost reports, budgets, alerts and the rest. Returns the sections that match best, at most two of a page, each with its page title, heading, URL and an excerpt; get_doc_page reads one. The search matches words, not meaning: ask in the product's words (budget, cost report, connection), and try other words when nothing fits. Cite the URL in an answer. Pages may be up to an hour older than the site."),
				Annotations: readOnly("Search Costfluent docs"),
			}, handle(h.searchDocs))
		}, reads},
		{"get_doc_page", NoCapability, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_doc_page",
				Description: docsText("Read a page of Costfluent's documentation as markdown, by the URL search_docs returns or the page's path; a URL with a #section reads only that section. Lists every section of the page with its URL. A long page is cut at a section and says truncated, and a section's URL reads the rest. Reads only listed pages on the docs site, never another URL."),
				Annotations: readOnly("Read a Costfluent docs page"),
			}, handle(h.getDocPage))
		}, reads},
	}
}

var errWorkspaceRequired = errors.New("workspace_id is required with an organization token; list_workspaces names the workspaces")

// workspace is the workspace a workspace-scoped tool reads: the one asked for, else the token's.
func (h *handlers) workspace(asked string) (string, error) {
	if asked != "" {
		return asked, nil
	}
	if h.me.WorkspaceID != nil {
		return *h.me.WorkspaceID, nil
	}
	return "", errWorkspaceRequired
}

// Page is one page of a listing. NextPage is the page to ask for next, absent on the last.
type Page[T any] struct {
	Items    []T `json:"items"`
	NextPage int `json:"next_page,omitempty"`
}

func pageOf[T any](resp *costfluent.ListResponse[T], page int) Page[T] {
	p := Page[T]{Items: resp.Data}
	if p.Items == nil {
		p.Items = []T{}
	}
	if resp.Links.HasNextPage() {
		p.NextPage = page + 1
	}
	return p
}

// pageOrFirst is the page asked for, or the first when none was.
func pageOrFirst(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

type ListWorkspacesInput struct {
	Page int `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listWorkspaces(ctx context.Context, in ListWorkspacesInput) (Page[costfluent.Workspace], error) {
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListWorkspaces(ctx, &costfluent.PageOptions{Page: page, Limit: pageSize})
	if err != nil {
		return Page[costfluent.Workspace]{}, err
	}
	return pageOf(resp, page), nil
}

type QueryCostsInput struct {
	StartDate   string   `json:"start_date" jsonschema:"the first day, YYYY-MM-DD"`
	EndDate     string   `json:"end_date" jsonschema:"the last day, inclusive, YYYY-MM-DD"`
	WorkspaceID string   `json:"workspace_id,omitempty" jsonschema:"the workspace to read; omitted, a workspace token reads its own and an organization token every workspace"`
	Granularity string   `json:"granularity,omitempty" jsonschema:"the period of each row: day, week, month, quarter, or cumulative for daily points that each carry the window's running total; day when omitted"`
	GroupBy     []string `json:"group_by,omitempty" jsonschema:"up to three groupings, outermost first, such as ServiceName, Provider, Region, SubAccount, or Tag:<key> for a tag key"`
	Filter      string   `json:"filter,omitempty" jsonschema:"a Costfluent cost filter (CQL), such as costs.provider = 'aws' AND costs.service = 'AmazonEC2'"`
	Measure     string   `json:"measure,omitempty" jsonschema:"cost, usage (consumed quantity) or count (distinct resources); cost when omitted"`
	Amortize    *bool    `json:"amortize,omitempty" jsonschema:"true for effective cost with commitments spread over their term; the API default when omitted"`
	Page        int      `json:"page,omitempty" jsonschema:"the page to read, from 1"`
	Limit       int      `json:"limit,omitempty" jsonschema:"rows per page, at most 5000; 500 when omitted"`
}

// CostsResult is one page of cost rows and what the query covered.
type CostsResult struct {
	Rows     []costfluent.CostDataPoint `json:"rows"`
	Meta     costfluent.CostDataMeta    `json:"meta"`
	NextPage int                        `json:"next_page,omitempty"`
}

func (h *handlers) queryCosts(ctx context.Context, in QueryCostsInput) (CostsResult, error) {
	page := pageOrFirst(in.Page)
	limit := in.Limit
	if limit <= 0 {
		limit = defaultCostRows
	}
	limit = min(limit, maxCostRows)
	query := &costfluent.CostDataQuery{
		CostFilterOptions: costfluent.CostFilterOptions{
			StartDate: in.StartDate, EndDate: in.EndDate, WorkspaceID: in.WorkspaceID,
			Filter: in.Filter, Amortize: in.Amortize,
		},
		Granularity: in.Granularity,
		Measure:     in.Measure,
		GroupBy:     groupings(in.GroupBy),
		PageOptions: costfluent.PageOptions{Page: page, Limit: limit},
	}
	resp, err := h.client.QueryCostData(ctx, query)
	if err != nil {
		return CostsResult{}, err
	}
	result := CostsResult{Rows: resp.Data, Meta: resp.Meta}
	if result.Rows == nil {
		result.Rows = []costfluent.CostDataPoint{}
	}
	if resp.Links.HasNextPage() {
		result.NextPage = page + 1
	}
	return result, nil
}

// groupings reads Dimension or Dimension:key, the way a cost row's dimensions spell a slot.
func groupings(raw []string) []costfluent.Grouping {
	out := make([]costfluent.Grouping, 0, len(raw))
	for _, r := range raw {
		dim, key, keyed := strings.Cut(strings.TrimSpace(r), ":")
		g := costfluent.Grouping{Dimension: dim}
		if keyed && key != "" {
			g.Key = &key
		}
		out = append(out, g)
	}
	return out
}

type ListCostReportsInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Page        int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

// CostReports is one page of a workspace's saved cost reports. NextPage is the page to ask for next,
// absent on the last. It keeps the reports key it had before it paged, so a reader of one answer
// per workspace still reads it.
type CostReports struct {
	Reports  []costfluent.CostReport `json:"reports"`
	NextPage int                     `json:"next_page,omitempty"`
}

// listCostReports reads one page, as every list tool does: a workspace's reports are unbounded in
// number and each carries a filter of up to about a megabyte, so reading them all at once holds all
// of that in memory, which for the hosted server is a container every organization shares.
func (h *handlers) listCostReports(ctx context.Context, in ListCostReportsInput) (CostReports, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return CostReports{}, err
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListCostReportsPage(ctx, ws, nil, &costfluent.PageOptions{Page: page, Limit: pageSize})
	if err != nil {
		return CostReports{}, err
	}
	p := pageOf(resp, page)
	return CostReports{Reports: p.Items, NextPage: p.NextPage}, nil
}

type CostReportDataInput struct {
	CostReportID string `json:"cost_report_id" jsonschema:"the saved report's ID, as list_cost_reports names it"`
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"the report's workspace; a workspace token's own when omitted"`
}

// maxReportBytes is the most bytes get_cost_report_data's answer encodes to. The result travels
// twice, as structured content and as its text fallback, and this keeps both copies within an
// assistant's 25,000-token tool result limit, which a size test pins. It counts bytes rather than
// points because a report's keys are as long as what they name: a resource ARN or an Azure resource
// id outweighs a service name several times over, in every period and in the breakdown of up to 51
// groups and their names that travels whole beside the series.
const maxReportBytes = 31_250

// CostReportResult is what a saved report shows. When it would encode past maxReportBytes, the
// series, comparison, unit cost and forecast keep only their points dated from SeriesFrom on, and
// SeriesOmittedPeriods counts the dates left out. The summary, breakdown and names stay whole, so
// when they alone pass maxReportBytes the answer keeps the latest date only and is larger still.
type CostReportResult struct {
	costfluent.CostReportData
	SeriesOmittedPeriods int    `json:"series_omitted_periods,omitempty"`
	SeriesFrom           string `json:"series_from,omitempty"`
}

func (h *handlers) getCostReportData(ctx context.Context, in CostReportDataInput) (CostReportResult, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return CostReportResult{}, err
	}
	data, err := h.client.GetCostReportData(ctx, ws, in.CostReportID)
	if err != nil {
		return CostReportResult{}, err
	}
	return trimReportSeries(*data)
}

// The dated parts of a report's data, whose points trimReportSeries counts apart because each part
// is its own JSON array.
const (
	reportSeries = iota
	reportComparison
	reportUnitCost
	reportForecast
	reportParts
)

// pointSize is the bytes one dated point encodes to, and the part of the report it belongs to.
type pointSize struct{ part, bytes int }

// trimReportSeries keeps the most recent dates of data that fit in maxReportBytes beside its
// summary, breakdown and names, at least one, and returns data whole when every date fits. It
// encodes the report without its dated points once and each point once, then adds dates newest
// first, counting the comma before every point but the first of its part. It cuts by date, not by
// position, because a comparison, unit cost or forecast point carries the window date of the period
// it sits beside, so one date floor keeps them aligned. It walks the dates of all four because a
// comparison point can sit on a date the series has no period for: the API emits a period only for
// a day with cost. It keeps the latest dates because a cumulative report's last period is the
// window's running total. It builds new slices and never changes what data points to.
func trimReportSeries(data costfluent.CostReportData) (CostReportResult, error) {
	var enc sizer
	sizes := map[string][]pointSize{}
	measurePoints(&enc, sizes, reportSeries, data.Series, periodDate)
	if data.Comparison != nil {
		measurePoints(&enc, sizes, reportComparison, data.Comparison.Series, comparisonDate)
	}
	if data.UnitCost != nil {
		measurePoints(&enc, sizes, reportUnitCost, data.UnitCost.Series, unitCostDate)
	}
	if data.Forecast != nil {
		measurePoints(&enc, sizes, reportForecast, data.Forecast.Series, forecastDate)
	}
	size := enc.len(CostReportResult{CostReportData: reportKeeping(data, func(string) bool { return false })})
	unmarked := enc.len(CostReportResult{})

	dates := slices.Sorted(maps.Keys(sizes))
	var kept [reportParts]int
	from := len(dates)
	for ; from > 0; from-- {
		n, next := 0, kept
		for _, p := range sizes[dates[from-1]] {
			if next[p.part] > 0 {
				n++
			}
			n += p.bytes
			next[p.part]++
		}
		marker := 0
		if from > 1 {
			marker = enc.len(CostReportResult{SeriesOmittedPeriods: from - 1, SeriesFrom: dates[from-1]}) - unmarked
		}
		if size+n+marker > maxReportBytes && from < len(dates) {
			break
		}
		size, kept = size+n, next
	}
	if enc.err != nil {
		return CostReportResult{}, fmt.Errorf("measuring the report: %w", enc.err)
	}
	if from == 0 {
		return CostReportResult{CostReportData: data}, nil
	}

	// The dates are the API's YYYY-MM-DD, so they order as strings.
	floor := dates[from]
	return CostReportResult{
		CostReportData:       reportKeeping(data, func(date string) bool { return date >= floor }),
		SeriesOmittedPeriods: from,
		SeriesFrom:           floor,
	}, nil
}

func periodDate(p costfluent.CostReportPeriod) string              { return p.Date }
func comparisonDate(p costfluent.CostReportComparisonPoint) string { return p.Date }
func unitCostDate(p costfluent.CostReportUnitCostPoint) string     { return p.Date }
func forecastDate(p costfluent.CostReportForecastPoint) string     { return p.Date }

// sizer measures the bytes values encode to as JSON, as the MCP SDK encodes a tool's result, and
// keeps the first error, which trimReportSeries checks once after its last measure. Only a NaN or an
// infinite float fails to encode, and a report carries floats only in its summary and breakdown.
type sizer struct{ err error }

func (s *sizer) len(v any) int {
	b, err := json.Marshal(v)
	if s.err == nil {
		s.err = err
	}
	return len(b)
}

// measurePoints adds the encoded size of each point of s, which belongs to part, to sizes under the
// point's date.
func measurePoints[T any](enc *sizer, sizes map[string][]pointSize, part int, s []T, date func(T) string) {
	for _, p := range s {
		sizes[date(p)] = append(sizes[date(p)], pointSize{part, enc.len(p)})
	}
}

// reportKeeping is data with only the series, comparison, unit cost and forecast points whose date
// keep accepts.
func reportKeeping(data costfluent.CostReportData, keep func(date string) bool) costfluent.CostReportData {
	data.Series = keepDated(data.Series, periodDate, keep)
	if data.Comparison != nil {
		c := *data.Comparison
		c.Series = keepDated(c.Series, comparisonDate, keep)
		data.Comparison = &c
	}
	if data.UnitCost != nil {
		u := *data.UnitCost
		u.Series = keepDated(u.Series, unitCostDate, keep)
		data.UnitCost = &u
	}
	if data.Forecast != nil {
		f := *data.Forecast
		f.Series = keepDated(f.Series, forecastDate, keep)
		data.Forecast = &f
	}
	return data
}

// keepDated is the points of s whose date keep accepts, in their order, in a new slice. A nil s
// stays nil, so the report without its points encodes its parts as the whole report does, which
// trimReportSeries's count of bytes rests on.
func keepDated[T any](s []T, date func(T) string, keep func(string) bool) []T {
	if s == nil {
		return nil
	}
	out := make([]T, 0, len(s))
	for _, p := range s {
		if keep(date(p)) {
			out = append(out, p)
		}
	}
	return out
}

type ListAnnotationsInput struct {
	WorkspaceID     string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	CostReportID    string `json:"cost_report_id,omitempty" jsonschema:"only the annotations standing on this saved report, as list_cost_reports names it"`
	StartDate       string `json:"start_date,omitempty" jsonschema:"the first annotated day, YYYY-MM-DD"`
	EndDate         string `json:"end_date,omitempty" jsonschema:"the last annotated day, inclusive, YYYY-MM-DD"`
	IncludeResolved bool   `json:"include_resolved,omitempty" jsonschema:"true to list resolved annotations too; open ones only when omitted"`
	Page            int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listAnnotations(ctx context.Context, in ListAnnotationsInput) (Page[costfluent.Annotation], error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Page[costfluent.Annotation]{}, err
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListAnnotations(ctx, ws, &costfluent.ListAnnotationsOptions{
		CostReportID: in.CostReportID, StartDate: in.StartDate, EndDate: in.EndDate, IncludeResolved: in.IncludeResolved,
		PageOptions: costfluent.PageOptions{Page: page, Limit: pageSize},
	})
	if err != nil {
		return Page[costfluent.Annotation]{}, err
	}
	return pageOf(resp, page), nil
}

type ListBudgetsInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Page        int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listBudgets(ctx context.Context, in ListBudgetsInput) (Page[costfluent.Budget], error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Page[costfluent.Budget]{}, err
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListBudgets(ctx, ws, &costfluent.PageOptions{Page: page, Limit: pageSize})
	if err != nil {
		return Page[costfluent.Budget]{}, err
	}
	return pageOf(resp, page), nil
}

// anomalySeverities are the severities the API stores. It matches one case-insensitively and
// answers any other value with an empty page, which an assistant reports as no anomalies, so the
// tool refuses an unknown severity before it calls the API.
var anomalySeverities = []string{"low", "medium", "high", "critical"}

var errUnknownSeverity = errors.New("severity must be one of " + strings.Join(anomalySeverities, ", ") +
	", in any case; omit it for every severity")

type ListAnomaliesInput struct {
	StartDate          string `json:"start_date,omitempty" jsonschema:"the first anomaly day, YYYY-MM-DD"`
	EndDate            string `json:"end_date,omitempty" jsonschema:"the last anomaly day, YYYY-MM-DD"`
	Severity           string `json:"severity,omitempty" jsonschema:"only anomalies of this severity: low, medium, high, critical, in any case"`
	CloudAccountID     string `json:"cloud_account_id,omitempty" jsonschema:"only anomalies read through this Costfluent connection: its id (prv_...), the providerId an anomaly or commitment carries, not the cloud provider's account number"`
	UnacknowledgedOnly bool   `json:"unacknowledged_only,omitempty" jsonschema:"true for open anomalies only"`
	Page               int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listAnomalies(ctx context.Context, in ListAnomaliesInput) (Page[costfluent.Anomaly], error) {
	if in.Severity != "" && !slices.ContainsFunc(anomalySeverities, func(s string) bool { return strings.EqualFold(s, in.Severity) }) {
		return Page[costfluent.Anomaly]{}, errUnknownSeverity
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListAnomalies(ctx, &costfluent.ListAnomaliesOptions{
		CloudAccountID: in.CloudAccountID, Severity: in.Severity, UnacknowledgedOnly: in.UnacknowledgedOnly,
		StartDate: in.StartDate, EndDate: in.EndDate, PageOptions: costfluent.PageOptions{Page: page, Limit: pageSize},
	})
	if err != nil {
		return Page[costfluent.Anomaly]{}, err
	}
	return pageOf(resp, page), nil
}

// The tools below pass their enum arguments (tab, source, kind, sort, status, granularity, level)
// to the API as given and return its refusal: the API answers a value it does not know with a 400
// naming the values it takes, unlike the anomaly severity above, so a check here would only go
// stale as the API learns new values.

type ListSavingsItemsInput struct {
	WorkspaceID          string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Tab                  string `json:"tab,omitempty" jsonschema:"only one tab of the queue: toReview, inProgress, verified or closed; every status when omitted"`
	Source               string `json:"source,omitempty" jsonschema:"only findings from this source, such as operator, curated, billingAnalysis, kubernetesAnalysis, awsCostOptimizationHub, azureAdvisor or gcpRecommender"`
	Kind                 string `json:"kind,omitempty" jsonschema:"only findings of this kind, such as rightsizing, idle, generational, storage, commitmentPurchase, extendedSupport, pricingTier or other"`
	CloudAccountID       string `json:"cloud_account_id,omitempty" jsonschema:"only findings generated through this Costfluent connection: its id (prv_...), the providerId an anomaly or commitment carries, not the cloud provider's account number"`
	CostEstimateID       string `json:"cost_estimate_id,omitempty" jsonschema:"only findings on the resources of this cost estimate (est_...), whose resource equals, ignoring case, an id the estimate's resources were posted or applied with; another workspace's or an unknown estimate matches none"`
	MinimumMonthlySaving string `json:"minimum_monthly_saving,omitempty" jsonschema:"only findings estimated to save at least this much a month, a decimal in each finding's own currency"`
	Sort                 string `json:"sort,omitempty" jsonschema:"newest (the default) or largestSaving"`
	Page                 int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listSavingsItems(ctx context.Context, in ListSavingsItemsInput) (Page[costfluent.SavingsItem], error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Page[costfluent.SavingsItem]{}, err
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListSavingsItems(ctx, ws, &costfluent.ListSavingsItemsOptions{
		Tab: in.Tab, Source: in.Source, Kind: in.Kind, CloudAccountID: in.CloudAccountID,
		CostEstimateID: in.CostEstimateID, MinimumMonthlySaving: in.MinimumMonthlySaving, Sort: in.Sort, PageOptions: costfluent.PageOptions{Page: page, Limit: pageSize},
	})
	if err != nil {
		return Page[costfluent.SavingsItem]{}, err
	}
	return pageOf(resp, page), nil
}

type ListCommitmentsInput struct {
	WorkspaceID   string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Provider      string `json:"provider,omitempty" jsonschema:"only commitments held through this provider, such as aws, azure or gcp"`
	Status        string `json:"status,omitempty" jsonschema:"only commitments in this state: active, expiring, expired or unknown (no end stated)"`
	Type          string `json:"type,omitempty" jsonschema:"only this type, in the provider's word: ReservedInstance, SavingsPlan or CommittedUseDiscount"`
	Service       string `json:"service,omitempty" jsonschema:"only commitments for this service, as the provider names it"`
	Account       string `json:"account,omitempty" jsonschema:"only commitments held in this provider account"`
	Region        string `json:"region,omitempty" jsonschema:"only commitments in this region"`
	TermYears     int    `json:"term_years,omitempty" jsonschema:"only commitments of this term in years: 1, 3 or 5"`
	PaymentOption string `json:"payment_option,omitempty" jsonschema:"only commitments paid this way: All Upfront, Partial Upfront or No Upfront"`
	Page          int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listCommitments(ctx context.Context, in ListCommitmentsInput) (Page[costfluent.Commitment], error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Page[costfluent.Commitment]{}, err
	}
	page := pageOrFirst(in.Page)
	resp, err := h.client.ListCommitments(ctx, ws, &costfluent.ListCommitmentsOptions{
		Provider: in.Provider, Status: in.Status, Type: in.Type, Service: in.Service, Account: in.Account,
		Region: in.Region, TermYears: in.TermYears, PaymentOption: in.PaymentOption,
		PageOptions: costfluent.PageOptions{Page: page, Limit: pageSize},
	})
	if err != nil {
		return Page[costfluent.Commitment]{}, err
	}
	return pageOf(resp, page), nil
}

// defaultCoverageBin is the bin get_commitment_coverage asks for when the caller names none. The
// API's default is a day, and a daily series over a long range outgrows an assistant's context.
const defaultCoverageBin = "month"

type CommitmentCoverageInput struct {
	StartDate   string `json:"start_date" jsonschema:"the first day, YYYY-MM-DD"`
	EndDate     string `json:"end_date" jsonschema:"the last day, inclusive, YYYY-MM-DD"`
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Granularity string `json:"granularity,omitempty" jsonschema:"the period of each bin: day, week, month or quarter; month when omitted"`
	Filter      string `json:"filter,omitempty" jsonschema:"a Costfluent cost filter (CQL), such as costs.provider = 'aws'"`
}

func (h *handlers) getCommitmentCoverage(ctx context.Context, in CommitmentCoverageInput) (*costfluent.CommitmentCoverage, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	bin := in.Granularity
	if bin == "" {
		bin = defaultCoverageBin
	}
	return h.client.GetCommitmentCoverage(ctx, ws, &costfluent.CommitmentCoverageOptions{
		StartDate: in.StartDate, EndDate: in.EndDate, Granularity: bin, Filter: in.Filter,
	})
}

type KubernetesEfficiencyInput struct {
	StartDate   string `json:"start_date" jsonschema:"the first day, YYYY-MM-DD"`
	EndDate     string `json:"end_date" jsonschema:"the last day, inclusive, YYYY-MM-DD"`
	Level       string `json:"level" jsonschema:"what each row is: cluster, namespace or workload"`
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
	Filter      string `json:"filter,omitempty" jsonschema:"a Costfluent cost filter (CQL), such as costs.cluster = 'prod'"`
}

func (h *handlers) getKubernetesEfficiency(ctx context.Context, in KubernetesEfficiencyInput) (*costfluent.KubernetesEfficiency, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return h.client.GetKubernetesEfficiency(ctx, ws, &costfluent.KubernetesEfficiencyOptions{
		StartDate: in.StartDate, EndDate: in.EndDate, Level: in.Level, Filter: in.Filter,
	})
}
