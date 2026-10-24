package server

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
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
}

func (h *handlers) tools() []toolDef {
	return []toolDef{
		{"list_workspaces", costfluent.CapabilityViewWorkspace, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_workspaces",
				Description: "List the workspaces this API token can read, with their currency and time zone. A workspace token sees only its own. Needs the viewWorkspace capability (“View workspaces”).",
				Annotations: readOnly("List workspaces"),
			}, handle(h.listWorkspaces))
		}},
		{"query_costs", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "query_costs",
				Description: "Query cost, usage or resource count over a date range, per period and optionally grouped, narrowed by a Costfluent cost filter. Needs the viewCosts capability (“View costs”).",
				Annotations: readOnly("Query costs"),
			}, handle(h.queryCosts))
		}},
		{"list_cost_reports", costfluent.CapabilityViewCostReports, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_cost_reports",
				Description: "List a workspace's saved cost reports: their window, grouping, filter and settings. Needs the viewCostReports capability (“View cost reports”).",
				Annotations: readOnly("List cost reports"),
			}, handle(h.listCostReports))
		}},
		{"get_cost_report_data", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_cost_report_data",
				Description: "Compute the numbers a saved cost report shows now: its total, series per period and breakdown by group. Needs the viewCosts capability (“View costs”).",
				Annotations: readOnly("Get cost report data"),
			}, handle(h.getCostReportData))
		}},
		{"list_budgets", costfluent.CapabilityViewBudgets, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_budgets",
				Description: "List a workspace's budgets with their measure, amount, current spend, amount budgeted to date and state (onTrack, atRisk, over), the spend in other currencies it does not count (unconverted), and the date its spend counts from when the plan's history starts inside the period (historyFloor). The current spend and state leave the unconverted spend out, so a budget can read onTrack while its scope is over. A budget whose measure is usage counts a quantity in its usageUnit, not money in its currency. Needs the viewBudgets capability (“View budgets”).",
				Annotations: readOnly("List budgets"),
			}, handle(h.listBudgets))
		}},
		{"list_anomalies", costfluent.CapabilityViewAnomalies, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_anomalies",
				Description: "List detected cost anomalies, newest first, with expected and actual cost. A workspace token sees only its workspace's. Needs the viewAnomalies capability (“View anomalies”).",
				Annotations: readOnly("List anomalies"),
			}, handle(h.listAnomalies))
		}},
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

type WorkspaceInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace to read; a workspace token's own when omitted"`
}

// CostReports is a workspace's saved cost reports.
type CostReports struct {
	Reports []costfluent.CostReport `json:"reports"`
}

func (h *handlers) listCostReports(ctx context.Context, in WorkspaceInput) (CostReports, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return CostReports{}, err
	}
	reports, err := h.client.ListCostReports(ctx, ws)
	if err != nil {
		return CostReports{}, err
	}
	if reports == nil {
		reports = []costfluent.CostReport{}
	}
	return CostReports{Reports: reports}, nil
}

type CostReportDataInput struct {
	CostReportID string `json:"cost_report_id" jsonschema:"the saved report's ID, as list_cost_reports names it"`
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"the report's workspace; a workspace token's own when omitted"`
}

func (h *handlers) getCostReportData(ctx context.Context, in CostReportDataInput) (*costfluent.CostReportData, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return h.client.GetCostReportData(ctx, ws, in.CostReportID)
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

type ListAnomaliesInput struct {
	StartDate          string `json:"start_date,omitempty" jsonschema:"the first anomaly day, YYYY-MM-DD"`
	EndDate            string `json:"end_date,omitempty" jsonschema:"the last anomaly day, YYYY-MM-DD"`
	Severity           string `json:"severity,omitempty" jsonschema:"only anomalies of this severity"`
	CloudAccountID     string `json:"cloud_account_id,omitempty" jsonschema:"only anomalies in this connected cloud account"`
	UnacknowledgedOnly bool   `json:"unacknowledged_only,omitempty" jsonschema:"true for open anomalies only"`
	Page               int    `json:"page,omitempty" jsonschema:"the page to read, from 1"`
}

func (h *handlers) listAnomalies(ctx context.Context, in ListAnomaliesInput) (Page[costfluent.Anomaly], error) {
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
