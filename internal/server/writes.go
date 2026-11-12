package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

// The write tools create and change Costfluent objects. None deletes one, and none takes an input
// whose omission removes something: an update sends only the fields the call names, and where the
// API replaces a whole list (a budget's thresholds, a cost alert's channels) the tool reads the
// object first and sends back what it holds with what the call adds, under If-Match, so a change
// made in between fails the call rather than being overwritten.

// optional is s as an optional field: nil when the call left it out.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optionalDecimal is s as an optional amount: nil when the call left it out.
func optionalDecimal(s string) *costfluent.Decimal {
	if s == "" {
		return nil
	}
	d := costfluent.Decimal(s)
	return &d
}

var errNothingToChange = errors.New("name at least one field to change; every field left out keeps its value")

// createdError keeps the ID of an object a create made when reading it back failed, so the
// assistant reports it rather than creating it again.
func createdError(err error) error {
	var created *costfluent.CreatedButUnreadError
	if errors.As(err, &created) {
		return fmt.Errorf("created %s, but reading it back failed: %s; it exists, so do not create it again",
			created.ID, ToolError(created.Err).Error())
	}
	return err
}

type CreateAnnotationInput struct {
	WorkspaceID   string   `json:"workspace_id,omitempty" jsonschema:"the workspace; a workspace token's own when omitted"`
	Date          string   `json:"date" jsonschema:"the day the note stands on, YYYY-MM-DD"`
	Text          string   `json:"text" jsonschema:"the note, at most 1,000 characters"`
	CostReportIDs []string `json:"cost_report_ids" jsonschema:"the saved reports the note stands on, one to ten, as list_cost_reports names them"`
}

func (h *handlers) createAnnotation(ctx context.Context, in CreateAnnotationInput) (*costfluent.Annotation, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return h.client.CreateAnnotation(ctx, &costfluent.CreateAnnotationInput{
		WorkspaceID: ws, Date: in.Date, Text: in.Text, CostReportIDs: in.CostReportIDs,
	})
}

// ReportFields are a cost report's definition as a create or an update takes it. A field left out
// is the API's default on a create and the report's own on an update.
type ReportFields struct {
	ChartType           string   `json:"chart_type,omitempty" jsonschema:"line, bar, stackedBar, area, pie or table; a new report draws stackedBar"`
	DateBin             string   `json:"date_bin,omitempty" jsonschema:"the period of each point: day, week, month, quarter, or cumulative for daily points that each carry the window's running total"`
	DateInterval        string   `json:"date_interval,omitempty" jsonschema:"the window: thisMonth, lastMonth, last3Days, last7Days, last14Days, last30Days, last90Days, last3Months, last6Months, last12Months, last24Months, last36Months, last3CompleteMonths, last6CompleteMonths, last12CompleteMonths, last24CompleteMonths, last36CompleteMonths, thisQuarter, lastQuarter, thisYear, lastYear, yearToDate, throughMonthEnd, throughNextMonth, throughNext3Months, throughNext6Months, or custom with start_date and end_date"`
	StartDate           string   `json:"start_date,omitempty" jsonschema:"a custom window's first day, YYYY-MM-DD"`
	EndDate             string   `json:"end_date,omitempty" jsonschema:"a custom window's last day, inclusive, YYYY-MM-DD"`
	Filter              string   `json:"filter,omitempty" jsonschema:"a Costfluent cost filter (CQL), such as costs.provider = 'aws' AND costs.service = 'AmazonEC2', or its JSON document as list_cost_reports shows it"`
	GroupBy             []string `json:"group_by,omitempty" jsonschema:"up to three groupings, outermost first, such as ServiceName, Provider, Region, SubAccount, or Tag:<key> for a tag key"`
	FolderID            string   `json:"folder_id,omitempty" jsonschema:"the folder to file the report in"`
	Measure             string   `json:"measure,omitempty" jsonschema:"what the report counts: cost, usage (consumed quantity) or count (distinct resources)"`
	Amortize            *bool    `json:"amortize,omitempty" jsonschema:"true for effective cost with commitments spread over their term"`
	IncludeCredits      *bool    `json:"include_credits,omitempty" jsonschema:"whether credits count"`
	IncludeRefunds      *bool    `json:"include_refunds,omitempty" jsonschema:"whether refunds count"`
	IncludeTax          *bool    `json:"include_tax,omitempty" jsonschema:"whether tax counts"`
	ShowForecast        *bool    `json:"show_forecast,omitempty" jsonschema:"whether the chart shows a forecast"`
	CompareToLastPeriod *bool    `json:"compare_to_last_period,omitempty" jsonschema:"whether the report compares with the period before"`
}

// settingsGiven reports whether the call names any of the report's settings.
func (f ReportFields) settingsGiven() bool {
	return f.Measure != "" || f.Amortize != nil || f.IncludeCredits != nil || f.IncludeRefunds != nil ||
		f.IncludeTax != nil || f.ShowForecast != nil || f.CompareToLastPeriod != nil
}

func (f ReportFields) settings() *costfluent.CostReportSettingsInput {
	if !f.settingsGiven() {
		return nil
	}
	return &costfluent.CostReportSettingsInput{
		Measure: f.Measure, Amortize: f.Amortize, IncludeCredits: f.IncludeCredits, IncludeRefunds: f.IncludeRefunds,
		IncludeTax: f.IncludeTax, ShowForecast: f.ShowForecast, CompareToLastPeriod: f.CompareToLastPeriod,
	}
}

// groupBy is the grouping the call names, or nil when it names none: an empty grouping asks the API
// for one total, which only a call that says so should get.
func (f ReportFields) groupBy() []costfluent.Grouping {
	if f.GroupBy == nil {
		return nil
	}
	return groupings(f.GroupBy)
}

type CreateCostReportInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the workspace; a workspace token's own when omitted"`
	Title       string `json:"title" jsonschema:"the report's title"`
	ReportFields
}

func (h *handlers) createCostReport(ctx context.Context, in CreateCostReportInput) (*costfluent.CostReport, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	report, err := h.client.CreateCostReport(ctx, &costfluent.CreateCostReportInput{
		WorkspaceID: ws, Title: in.Title, FolderID: optional(in.FolderID), ChartType: optional(in.ChartType),
		DateBin: optional(in.DateBin), DateInterval: optional(in.DateInterval), StartDate: optional(in.StartDate),
		EndDate: optional(in.EndDate), Filter: optional(in.Filter), GroupBy: in.groupBy(), Settings: in.settings(),
	})
	return report, createdError(err)
}

type UpdateCostReportInput struct {
	CostReportID string `json:"cost_report_id" jsonschema:"the saved report to change, as list_cost_reports names it"`
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"the report's workspace; a workspace token's own when omitted"`
	Title        string `json:"title,omitempty" jsonschema:"the report's new title"`
	ReportFields
}

func (h *handlers) updateCostReport(ctx context.Context, in UpdateCostReportInput) (*costfluent.CostReport, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	input := &costfluent.UpdateCostReportInput{
		Title: optional(in.Title), FolderID: optional(in.FolderID), ChartType: optional(in.ChartType),
		DateBin: optional(in.DateBin), DateInterval: optional(in.DateInterval), StartDate: optional(in.StartDate),
		EndDate: optional(in.EndDate), Filter: optional(in.Filter), GroupBy: in.groupBy(), Settings: in.settings(),
	}
	if reflect.ValueOf(*input).IsZero() {
		return nil, errNothingToChange
	}
	var opts []costfluent.RequestOption
	if input.Settings != nil {
		// The API takes a report's unit cost metric and comparison window as the settings send them,
		// so settings that leave them out would remove them: they go back as the report holds them.
		current, err := h.client.GetCostReport(ctx, ws, in.CostReportID)
		if err != nil {
			return nil, err
		}
		held := current.Settings
		input.Settings.UnitCostMetricID = held.UnitCostMetricID
		input.Settings.UnitCostLabelFilter = held.UnitCostLabelFilter
		input.Settings.ComparisonStartDate = held.ComparisonStartDate
		input.Settings.ComparisonEndDate = held.ComparisonEndDate
		opts = append(opts, costfluent.IfMatch(current.ETag))
	}
	return h.client.UpdateCostReport(ctx, ws, in.CostReportID, input, opts...)
}

// thresholds reads budget alert thresholds written as a percent, or percent:basis.
func thresholds(raw []string) ([]costfluent.BudgetAlertInput, error) {
	out := make([]costfluent.BudgetAlertInput, 0, len(raw))
	for _, r := range raw {
		percent, basis, _ := strings.Cut(strings.TrimSpace(r), ":")
		n, err := strconv.Atoi(percent)
		if err != nil {
			return nil, fmt.Errorf("alert threshold %q is not a whole percent, optionally followed by :actual or :forecast", r)
		}
		out = append(out, costfluent.BudgetAlertInput{ThresholdPercent: n, Basis: cmp.Or(basis, costfluent.BudgetAlertBasisActual)})
	}
	return out, nil
}

type CreateBudgetInput struct {
	WorkspaceID     string   `json:"workspace_id,omitempty" jsonschema:"the workspace; a workspace token's own when omitted"`
	Name            string   `json:"name" jsonschema:"the budget's name"`
	Currency        string   `json:"currency" jsonschema:"the budget's ISO 4217 currency, such as USD"`
	Amount          string   `json:"amount" jsonschema:"every period's amount, a decimal, such as 5000 or 1250.50"`
	StartDate       string   `json:"start_date,omitempty" jsonschema:"the first period's first day, YYYY-MM-DD; today when omitted"`
	IntervalCount   int      `json:"interval_count,omitempty" jsonschema:"how many interval units a period lasts; 1 when omitted"`
	IntervalUnit    string   `json:"interval_unit,omitempty" jsonschema:"day, week, month or year; month when omitted, so a quarter is 3 months"`
	Measure         string   `json:"measure,omitempty" jsonschema:"cost (the default) or usage, which counts usage_unit on cost_report_id's rows"`
	UsageUnit       string   `json:"usage_unit,omitempty" jsonschema:"the one usage unit a usage budget counts, such as Tokens"`
	CostReportID    string   `json:"cost_report_id,omitempty" jsonschema:"the saved report whose costs the budget measures, as list_cost_reports names it; the whole workspace when neither it nor segment_id is given"`
	SegmentID       string   `json:"segment_id,omitempty" jsonschema:"the allocation segment whose costs the budget measures"`
	AlertThresholds []string `json:"alert_thresholds,omitempty" jsonschema:"the thresholds that notify, each a whole percent of the period's amount from 1 to 200, held against the spend so far, or followed by :forecast to be held against the forecast for the period's end, which a usage budget refuses, such as 80 or 100:forecast"`
	AppIDs          []string `json:"app_ids,omitempty" jsonschema:"the notification channels every threshold notifies"`
}

func (h *handlers) createBudget(ctx context.Context, in CreateBudgetInput) (*costfluent.Budget, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	alerts, err := thresholds(in.AlertThresholds)
	if err != nil {
		return nil, err
	}
	return h.client.CreateBudget(ctx, &costfluent.CreateBudgetInput{
		WorkspaceID: ws, Name: in.Name, Currency: in.Currency, Amount: optionalDecimal(in.Amount),
		StartDate: optional(in.StartDate), IntervalCount: in.IntervalCount, IntervalUnit: in.IntervalUnit,
		Measure: in.Measure, UsageUnit: optional(in.UsageUnit), CostReportID: optional(in.CostReportID),
		SegmentID: optional(in.SegmentID), Alerts: alerts, AppIDs: in.AppIDs,
	})
}

type UpdateBudgetInput struct {
	BudgetID           string   `json:"budget_id" jsonschema:"the budget to change, as list_budgets names it"`
	Name               string   `json:"name,omitempty" jsonschema:"the budget's new name"`
	Amount             string   `json:"amount,omitempty" jsonschema:"every period's new amount, a decimal"`
	CostReportID       string   `json:"cost_report_id,omitempty" jsonschema:"move the budget onto this saved report's costs"`
	SegmentID          string   `json:"segment_id,omitempty" jsonschema:"move the budget onto this allocation segment's costs"`
	AddAlertThresholds []string `json:"add_alert_thresholds,omitempty" jsonschema:"thresholds to add beside the budget's own, which all stay, each a whole percent of the period's amount from 1 to 200, held against the spend so far, or followed by :forecast to be held against the forecast for the period's end, which a usage budget refuses, such as 80 or 100:forecast"`
}

func (h *handlers) updateBudget(ctx context.Context, in UpdateBudgetInput) (*costfluent.Budget, error) {
	add, err := thresholds(in.AddAlertThresholds)
	if err != nil {
		return nil, err
	}
	input := &costfluent.UpdateBudgetInput{
		Name: optional(in.Name), Amount: optionalDecimal(in.Amount),
		CostReportID: optional(in.CostReportID), SegmentID: optional(in.SegmentID),
	}
	if reflect.ValueOf(*input).IsZero() && len(add) == 0 {
		return nil, errNothingToChange
	}
	var opts []costfluent.RequestOption
	if len(add) > 0 {
		// The API replaces a budget's thresholds with the list an update sends, so the list goes
		// with every threshold the budget holds: one the call does not name is never removed.
		current, err := h.client.GetBudget(ctx, in.BudgetID)
		if err != nil {
			return nil, err
		}
		alerts := make([]costfluent.BudgetAlertInput, 0, len(current.Alerts)+len(add))
		for _, a := range current.Alerts {
			alerts = append(alerts, costfluent.BudgetAlertInput{ThresholdPercent: a.ThresholdPercent, Basis: a.Basis})
		}
		for _, a := range add {
			if !slices.Contains(alerts, a) {
				alerts = append(alerts, a)
			}
		}
		input.Alerts = &alerts
		opts = append(opts, costfluent.IfMatch(current.ETag))
	}
	return h.client.UpdateBudget(ctx, in.BudgetID, input, opts...)
}

type CreateCostAlertInput struct {
	WorkspaceID                string   `json:"workspace_id,omitempty" jsonschema:"the workspace; a workspace token's own when omitted"`
	Name                       string   `json:"name" jsonschema:"the alert's name"`
	ThresholdType              string   `json:"threshold_type" jsonschema:"absolute (an amount of cost), percentageIncrease (a percent increase over comparison_period), budgetPercentage (the month-to-date spend reaching threshold_value as an amount) or tagCoverageBelow (a percent of cost carrying tags)"`
	ThresholdValue             string   `json:"threshold_value" jsonschema:"the threshold, a decimal: an amount or a percent as threshold_type says"`
	ComparisonPeriod           string   `json:"comparison_period,omitempty" jsonschema:"what a percentageIncrease alert compares with: previousDay, previousWeek, previousMonth, sameDayLastMonth or previousQuarter"`
	MinimumAmount              string   `json:"minimum_amount,omitempty" jsonschema:"for percentageIncrease, the increase in currency the cost must also reach, a decimal"`
	ProviderIDs                []string `json:"provider_ids,omitempty" jsonschema:"the Costfluent connections (prv_...) to watch; every connection when omitted"`
	Filter                     string   `json:"filter,omitempty" jsonschema:"a Costfluent cost filter (CQL) narrowing what the alert watches, such as costs.service = 'AmazonEC2'"`
	AppIDs                     []string `json:"app_ids,omitempty" jsonschema:"the notification channels it notifies"`
	EvaluationFrequencyMinutes *int     `json:"evaluation_frequency_minutes,omitempty" jsonschema:"how often it is evaluated, in minutes; the API's default when omitted"`
}

func (h *handlers) createCostAlert(ctx context.Context, in CreateCostAlertInput) (*costfluent.CostAlert, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	alert, err := h.client.CreateCostAlert(ctx, &costfluent.CreateCostAlertInput{
		WorkspaceID: ws, Name: in.Name, ThresholdType: in.ThresholdType, ThresholdValue: costfluent.Decimal(in.ThresholdValue),
		ComparisonPeriod: optional(in.ComparisonPeriod), MinimumAmount: optionalDecimal(in.MinimumAmount),
		ProviderIDs: in.ProviderIDs, Filter: optional(in.Filter), AppIDs: in.AppIDs,
		EvaluationFrequencyMinutes: in.EvaluationFrequencyMinutes,
	})
	return alert, createdError(err)
}

var errEmptyProviders = errors.New("provider_ids names at least one connection; omit it to keep the ones the alert watches")

type UpdateCostAlertInput struct {
	CostAlertID                string   `json:"cost_alert_id" jsonschema:"the cost alert to change"`
	WorkspaceID                string   `json:"workspace_id,omitempty" jsonschema:"the alert's workspace; a workspace token's own when omitted"`
	Name                       string   `json:"name,omitempty" jsonschema:"the alert's new name"`
	ThresholdType              string   `json:"threshold_type,omitempty" jsonschema:"absolute (an amount of cost), percentageIncrease (a percent increase over comparison_period), budgetPercentage (the month-to-date spend reaching threshold_value as an amount) or tagCoverageBelow (a percent of cost carrying tags)"`
	ThresholdValue             string   `json:"threshold_value,omitempty" jsonschema:"the new threshold, a decimal: an amount or a percent as threshold_type says"`
	ComparisonPeriod           string   `json:"comparison_period,omitempty" jsonschema:"what a percentageIncrease alert compares with: previousDay, previousWeek, previousMonth, sameDayLastMonth or previousQuarter"`
	MinimumAmount              string   `json:"minimum_amount,omitempty" jsonschema:"for percentageIncrease, the increase in currency the cost must also reach, a decimal; 0 removes the floor"`
	ProviderIDs                []string `json:"provider_ids,omitempty" jsonschema:"the Costfluent connections (prv_...) to watch instead of the alert's, at least one"`
	Filter                     string   `json:"filter,omitempty" jsonschema:"a new Costfluent cost filter (CQL) narrowing what the alert watches"`
	AddAppIDs                  []string `json:"add_app_ids,omitempty" jsonschema:"notification channels to add beside the alert's own, which all stay"`
	EvaluationFrequencyMinutes *int     `json:"evaluation_frequency_minutes,omitempty" jsonschema:"how often it is evaluated, in minutes"`
}

func (h *handlers) updateCostAlert(ctx context.Context, in UpdateCostAlertInput) (*costfluent.CostAlert, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if in.ProviderIDs != nil && len(in.ProviderIDs) == 0 {
		return nil, errEmptyProviders
	}
	input := &costfluent.UpdateCostAlertInput{
		Name: optional(in.Name), ThresholdType: optional(in.ThresholdType), ThresholdValue: optionalDecimal(in.ThresholdValue),
		ComparisonPeriod: optional(in.ComparisonPeriod), MinimumAmount: optionalDecimal(in.MinimumAmount),
		Filter: optional(in.Filter), EvaluationFrequencyMinutes: in.EvaluationFrequencyMinutes,
	}
	if in.ProviderIDs != nil {
		input.ProviderIDs = &in.ProviderIDs
	}
	if reflect.ValueOf(*input).IsZero() && len(in.AddAppIDs) == 0 {
		return nil, errNothingToChange
	}
	var opts []costfluent.RequestOption
	if len(in.AddAppIDs) > 0 {
		// The API replaces an alert's channels with the list an update sends, so the list goes with
		// every channel the alert holds: one the call does not name is never removed.
		current, err := h.client.GetCostAlert(ctx, ws, in.CostAlertID)
		if err != nil {
			return nil, err
		}
		apps := slices.Clone(current.AppIDs)
		for _, id := range in.AddAppIDs {
			if !slices.Contains(apps, id) {
				apps = append(apps, id)
			}
		}
		input.AppIDs = &apps
		opts = append(opts, costfluent.IfMatch(current.ETag))
	}
	return h.client.UpdateCostAlert(ctx, ws, in.CostAlertID, input, opts...)
}

type CostAlertInput struct {
	CostAlertID string `json:"cost_alert_id" jsonschema:"the cost alert"`
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"the alert's workspace; a workspace token's own when omitted"`
}

// Changed is what a write that answers with no object did.
type Changed struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (h *handlers) pauseCostAlert(ctx context.Context, in CostAlertInput) (Changed, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Changed{}, err
	}
	if err := h.client.PauseCostAlert(ctx, ws, in.CostAlertID); err != nil {
		return Changed{}, err
	}
	return Changed{ID: in.CostAlertID, Status: "paused"}, nil
}

func (h *handlers) resumeCostAlert(ctx context.Context, in CostAlertInput) (Changed, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Changed{}, err
	}
	if err := h.client.ResumeCostAlert(ctx, ws, in.CostAlertID); err != nil {
		return Changed{}, err
	}
	return Changed{ID: in.CostAlertID, Status: "active"}, nil
}

type CreateDashboardInput struct {
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"the workspace; a workspace token's own when omitted"`
	Title        string `json:"title" jsonschema:"the dashboard's title"`
	DateInterval string `json:"date_interval,omitempty" jsonschema:"the range every widget shows, as a cost report's date_interval, such as last30Days, or custom with start_date and end_date"`
	DateBin      string `json:"date_bin,omitempty" jsonschema:"day, week, month, quarter or cumulative"`
	StartDate    string `json:"start_date,omitempty" jsonschema:"a custom range's first day, YYYY-MM-DD"`
	EndDate      string `json:"end_date,omitempty" jsonschema:"a custom range's last day, inclusive, YYYY-MM-DD"`
}

func (h *handlers) createDashboard(ctx context.Context, in CreateDashboardInput) (*costfluent.Dashboard, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	dashboard, err := h.client.CreateDashboard(ctx, &costfluent.CreateDashboardInput{
		WorkspaceID: ws, Title: in.Title, DateInterval: optional(in.DateInterval), DateBin: optional(in.DateBin),
		StartDate: optional(in.StartDate), EndDate: optional(in.EndDate),
	})
	return dashboard, createdError(err)
}

// A widget placed without a position goes below the others at the size the console gives a new
// one: half of the dashboard's 12 columns, 4 rows high.
const (
	newWidgetWidth  = 6
	newWidgetHeight = 4
)

var errPartialPosition = errors.New("give column, row, width and height together, or none of them to place the widget below the others")

type AddDashboardWidgetInput struct {
	DashboardID  string `json:"dashboard_id" jsonschema:"the dashboard to add the widget to"`
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"the dashboard's workspace; a workspace token's own when omitted"`
	Title        string `json:"title" jsonschema:"the widget's title"`
	Type         string `json:"type,omitempty" jsonschema:"costReport (the default) for a saved report, or text"`
	CostReportID string `json:"cost_report_id,omitempty" jsonschema:"the saved report a costReport widget draws, as list_cost_reports names it"`
	Display      string `json:"display,omitempty" jsonschema:"how a costReport widget draws its report: chart, table or kpi"`
	Content      string `json:"content,omitempty" jsonschema:"a text widget's words, at most 10,000 characters"`
	Column       *int   `json:"column,omitempty" jsonschema:"the widget's first column, from 0 of the dashboard's 12"`
	Row          *int   `json:"row,omitempty" jsonschema:"the widget's first row, from 0"`
	Width        *int   `json:"width,omitempty" jsonschema:"the widget's width in columns, 1 to 12"`
	Height       *int   `json:"height,omitempty" jsonschema:"the widget's height in rows"`
}

func (h *handlers) addDashboardWidget(ctx context.Context, in AddDashboardWidgetInput) (*costfluent.Widget, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	var position costfluent.WidgetPosition
	switch given := []*int{in.Column, in.Row, in.Width, in.Height}; {
	case !slices.Contains(given, nil):
		position = costfluent.WidgetPosition{X: *in.Column, Y: *in.Row, W: *in.Width, H: *in.Height}
	case slices.ContainsFunc(given, func(p *int) bool { return p != nil }):
		return nil, errPartialPosition
	default:
		dashboard, err := h.client.GetDashboard(ctx, ws, in.DashboardID)
		if err != nil {
			return nil, err
		}
		below := 0
		for _, w := range dashboard.Widgets {
			below = max(below, w.Position.Y+w.Position.H)
		}
		position = costfluent.WidgetPosition{X: 0, Y: below, W: newWidgetWidth, H: newWidgetHeight}
	}
	widget, err := h.client.CreateDashboardWidget(ctx, ws, in.DashboardID, &costfluent.CreateDashboardWidgetInput{
		Type: in.Type, CostReportID: optional(in.CostReportID), Display: optional(in.Display),
		Content: optional(in.Content), Title: in.Title, Position: position,
	})
	return widget, createdError(err)
}

type AcknowledgeAnomalyInput struct {
	AnomalyID string `json:"anomaly_id" jsonschema:"the anomaly to acknowledge, as list_anomalies names it"`
}

func (h *handlers) acknowledgeAnomaly(ctx context.Context, in AcknowledgeAnomalyInput) (Changed, error) {
	if err := h.client.AcknowledgeAnomaly(ctx, in.AnomalyID); err != nil {
		return Changed{}, err
	}
	return Changed{ID: in.AnomalyID, Status: "acknowledged"}, nil
}

// The statuses set_savings_item_status sets. Verified is the measurement's to set, and withdrawn
// the finding's source's, so neither is offered.
const (
	savingsAccepted    = "accepted"
	savingsImplemented = "implemented"
	savingsRejected    = "rejected"
)

var (
	errUnknownSavingsStatus = errors.New("status is accepted, implemented or rejected")
	errRejectionReason      = errors.New("rejecting a finding needs a reason, and only rejecting takes one")
)

type SetSavingsItemStatusInput struct {
	SavingsItemID string `json:"savings_item_id" jsonschema:"the finding, as list_savings_items names it"`
	WorkspaceID   string `json:"workspace_id,omitempty" jsonschema:"the finding's workspace; a workspace token's own when omitted"`
	Status        string `json:"status" jsonschema:"accepted (the workspace will act on an open finding), implemented (an accepted finding's change was made) or rejected (declined, with a reason)"`
	Reason        string `json:"reason,omitempty" jsonschema:"why the finding is declined, at most 1,000 characters; given when status is rejected and only then"`
}

func (h *handlers) setSavingsItemStatus(ctx context.Context, in SetSavingsItemStatusInput) (Changed, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return Changed{}, err
	}
	if !slices.Contains([]string{savingsAccepted, savingsImplemented, savingsRejected}, in.Status) {
		return Changed{}, errUnknownSavingsStatus
	}
	if (in.Status == savingsRejected) != (strings.TrimSpace(in.Reason) != "") {
		return Changed{}, errRejectionReason
	}
	switch in.Status {
	case savingsAccepted:
		err = h.client.AcceptSavingsItem(ctx, ws, in.SavingsItemID)
	case savingsImplemented:
		err = h.client.ImplementSavingsItem(ctx, ws, in.SavingsItemID)
	default:
		err = h.client.RejectSavingsItem(ctx, ws, in.SavingsItemID, in.Reason)
	}
	if err != nil {
		return Changed{}, err
	}
	return Changed{ID: in.SavingsItemID, Status: in.Status}, nil
}

type AssignSavingsItemOwnerInput struct {
	SavingsItemID string `json:"savings_item_id" jsonschema:"the finding, as list_savings_items names it"`
	WorkspaceID   string `json:"workspace_id,omitempty" jsonschema:"the finding's workspace; a workspace token's own when omitted"`
	Owner         string `json:"owner" jsonschema:"who acts on the finding, free text such as a team or a person, at most 200 characters"`
}

func (h *handlers) assignSavingsItemOwner(ctx context.Context, in AssignSavingsItemOwnerInput) (*costfluent.SavingsItem, error) {
	ws, err := h.workspace(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return h.client.AssignSavingsItemOwner(ctx, ws, in.SavingsItemID, in.Owner)
}
