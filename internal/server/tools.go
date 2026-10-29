package server

import (
	"context"
	"errors"
	"slices"
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
				Description: tenantText("List the workspaces this API token can read, with their currency and time zone. A workspace token sees only its own. Needs the viewWorkspace capability (“View workspaces”)."),
				Annotations: readOnly("List workspaces"),
			}, handle(h.listWorkspaces))
		}},
		{"query_costs", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "query_costs",
				Description: tenantText("Query cost, usage or resource count over a date range, per period and optionally grouped, narrowed by a Costfluent cost filter. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Query costs"),
			}, handle(h.queryCosts))
		}},
		{"list_cost_reports", costfluent.CapabilityViewCostReports, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_cost_reports",
				Description: tenantText("List a workspace's saved cost reports: their window, grouping, filter and settings. Needs the viewCostReports capability (“View cost reports”)."),
				Annotations: readOnly("List cost reports"),
			}, handle(h.listCostReports))
		}},
		{"get_cost_report_data", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_cost_report_data",
				Description: tenantText("Compute the numbers a saved cost report shows now: its total, series per period and breakdown by group. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Get cost report data"),
			}, handle(h.getCostReportData))
		}},
		{"list_annotations", costfluent.CapabilityViewCostReports, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_annotations",
				Description: tenantText("List a workspace's annotations by date: the notes members pin to a day on its cost reports and the markers a budget or cost alert leaves as it fires, each with its source, text or the budget or alert that fired, and the cost reports it stands on. Narrowed to one cost report or a date range; resolved annotations are left out unless include_resolved is true. Needs the viewCostReports capability (“View cost reports”)."),
				Annotations: readOnly("List annotations"),
			}, handle(h.listAnnotations))
		}},
		{"list_budgets", costfluent.CapabilityViewBudgets, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_budgets",
				Description: tenantText("List a workspace's budgets with their measure, amount, current spend, amount budgeted to date and state (onTrack, atRisk, over), the spend in other currencies it does not count (unconverted), and the date its spend counts from when the plan's history starts inside the period (historyFloor). The current spend and state leave the unconverted spend out, so a budget can read onTrack while its scope is over. A budget whose measure is usage counts a quantity in its usageUnit, not money in its currency. Needs the viewBudgets capability (“View budgets”)."),
				Annotations: readOnly("List budgets"),
			}, handle(h.listBudgets))
		}},
		{"list_anomalies", costfluent.CapabilityViewAnomalies, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_anomalies",
				Description: tenantText("List detected cost anomalies, newest first, with expected and actual cost. A workspace token sees only its workspace's. Needs the viewAnomalies capability (“View anomalies”)."),
				Annotations: readOnly("List anomalies"),
			}, handle(h.listAnomalies))
		}},
		{"list_savings_items", costfluent.CapabilityViewSavingsItems, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_savings_items",
				Description: tenantText("List a workspace's savings queue: each finding's title, evidence, estimated monthly saving in its currency, confidence, risk, effort, status and source, newest first or largest saving first, narrowed by tab, source, kind, connection or a minimum monthly saving. Needs the viewSavingsItems capability (“View savings”)."),
				Annotations: readOnly("List savings items"),
			}, handle(h.listSavingsItems))
		}},
		{"list_commitments", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "list_commitments",
				Description: tenantText("List the reservations, savings plans and committed use discounts held through a workspace's connections, earliest end first, with their term, payment option, hourly commitment, utilization, unused cost and days until expiry. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("List commitments"),
			}, handle(h.listCommitments))
		}},
		{"get_commitment_coverage", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_commitment_coverage",
				Description: tenantText("Read how much of a workspace's eligible usage its commitments covered over a date range, with the effective savings rate, utilization and unused commitment, per period and provider and in total, at amortized cost in the presentation currency. The figures leave out the spend listed as unconverted and, when the plan's history starts inside the range, the days before historyFloor. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Get commitment coverage"),
			}, handle(h.getCommitmentCoverage))
		}},
		{"get_kubernetes_efficiency", costfluent.CapabilityViewCosts, func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{
				Name:        "get_kubernetes_efficiency",
				Description: tenantText("Read the cost, idle cost and CPU and memory used and peak against requested of a workspace's Kubernetes clusters over a date range, per cluster, namespace or workload, largest cost first. It returns at most 500 rows: truncated is true when more matched and the smallest were left out. The figures leave out the spend listed as unconverted and the days before historyFloor. Needs the viewCosts capability (“View costs”)."),
				Annotations: readOnly("Get Kubernetes efficiency"),
			}, handle(h.getKubernetesEfficiency))
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
	CloudAccountID     string `json:"cloud_account_id,omitempty" jsonschema:"only anomalies in this connected cloud account"`
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
	Source               string `json:"source,omitempty" jsonschema:"only findings from this source, such as operator, curated, billingAnalysis, awsCostOptimizationHub, azureAdvisor or gcpRecommender"`
	Kind                 string `json:"kind,omitempty" jsonschema:"only findings of this kind, such as rightsizing, idle, generational, storage, commitmentPurchase, extendedSupport or other"`
	CloudAccountID       string `json:"cloud_account_id,omitempty" jsonschema:"only findings generated through this connected cloud account"`
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
		MinimumMonthlySaving: in.MinimumMonthlySaving, Sort: in.Sort, PageOptions: costfluent.PageOptions{Page: page, Limit: pageSize},
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
