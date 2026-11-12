package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

// TestSmokeWritesAgainstADeployedAPI calls every write tool once against a deployed Public API,
// on objects it names mcp-smoke-<run>, and deletes what it created through the SDK. It runs only
// with a token whose organization lets AI agents create and change objects and which carries every
// write capability, in an organization holding an anomaly and an open savings finding: the
// implementer mints that token for the run in a dev organization and revokes it after, so no
// stored secret can write. Acknowledging the anomaly and accepting the finding are not undone.
func TestSmokeWritesAgainstADeployedAPI(t *testing.T) {
	token := os.Getenv(smokeWriteTokenEnv)
	if token == "" {
		t.Skip(smokeWriteTokenEnv + " is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cs, stderr := smokeSession(ctx, t, token)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := servedWrites(tools.Tools)
	if missing := slices.DeleteFunc(writeTools(), func(name string) bool { return slices.Contains(served, name) }); len(missing) > 0 {
		t.Fatalf("the token is not served the write tools %v: its organization must let AI agents create and change objects and it must carry every write capability\n%s", missing, stderr)
	}
	workspace := smokeWorkspace(ctx, t, cs, tools.Tools)
	name := fmt.Sprintf("mcp-smoke-%d", time.Now().UTC().Unix())
	inWorkspace := func(args map[string]any) map[string]any {
		if workspace != "" {
			args["workspace_id"] = workspace
		}
		return args
	}

	sdk := costfluent.NewClient(costfluent.WithAPIKey(token), costfluent.WithBaseURL(smokeBase()))
	var cleanups []func(context.Context) error
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, cleanup := range slices.Backward(cleanups) {
			if err := cleanup(cleanupCtx); err != nil {
				t.Errorf("deleting what the smoke created: %v", err)
			}
		}
	})
	called := map[string]bool{}
	call := func(tool string, args map[string]any) string {
		called[tool] = true
		return smokeCall(ctx, t, cs, tool, args)
	}

	report := smokeID(t, "create_cost_report", call("create_cost_report", inWorkspace(map[string]any{"title": name})))
	cleanups = append(cleanups, func(ctx context.Context) error { return sdk.DeleteCostReport(ctx, workspace, report) })
	call("update_cost_report", inWorkspace(map[string]any{"cost_report_id": report, "title": name + "-updated"}))

	annotation := smokeID(t, "create_annotation", call("create_annotation", inWorkspace(map[string]any{
		"date": time.Now().UTC().Format(time.DateOnly), "text": name, "cost_report_ids": []string{report},
	})))
	cleanups = append(cleanups, func(ctx context.Context) error { return sdk.DeleteAnnotation(ctx, workspace, annotation) })

	budget := smokeID(t, "create_budget", call("create_budget", inWorkspace(map[string]any{
		"name": name, "currency": "USD", "amount": "100", "cost_report_id": report,
	})))
	cleanups = append(cleanups, func(ctx context.Context) error { return sdk.DeleteBudget(ctx, budget) })
	call("update_budget", map[string]any{"budget_id": budget, "add_alert_thresholds": []string{"80"}})

	alert := smokeID(t, "create_cost_alert", call("create_cost_alert", inWorkspace(map[string]any{
		"name": name, "threshold_type": "absolute", "threshold_value": "100",
	})))
	cleanups = append(cleanups, func(ctx context.Context) error { return sdk.DeleteCostAlert(ctx, workspace, alert) })
	call("update_cost_alert", inWorkspace(map[string]any{"cost_alert_id": alert, "name": name + "-updated"}))
	call("pause_cost_alert", inWorkspace(map[string]any{"cost_alert_id": alert}))
	call("resume_cost_alert", inWorkspace(map[string]any{"cost_alert_id": alert}))

	dashboard := smokeID(t, "create_dashboard", call("create_dashboard", inWorkspace(map[string]any{"title": name})))
	cleanups = append(cleanups, func(ctx context.Context) error { return sdk.DeleteDashboard(ctx, workspace, dashboard) })
	call("add_dashboard_widget", inWorkspace(map[string]any{"dashboard_id": dashboard, "title": name, "cost_report_id": report}))

	anomaly := smokeFirst(t, call("list_anomalies", inWorkspace(map[string]any{})), func(status string) bool { return status != "" })
	call("acknowledge_anomaly", inWorkspace(map[string]any{"anomaly_id": anomaly}))

	finding := smokeFirst(t, call("list_savings_items", inWorkspace(map[string]any{})), func(status string) bool { return status == "open" })
	call("assign_savings_item_owner", inWorkspace(map[string]any{"savings_item_id": finding, "owner": name}))
	call("set_savings_item_status", inWorkspace(map[string]any{"savings_item_id": finding, "status": "accepted"}))

	if uncalled := slices.DeleteFunc(writeTools(), func(tool string) bool { return called[tool] }); len(uncalled) > 0 {
		t.Errorf("the smoke never called the write tools %v; give each a call here", uncalled)
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the server logged its token")
	}
}

// smokeID reads the id of the object a create tool answers with.
func smokeID(t *testing.T, tool, text string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(text), &created) != nil || created.ID == "" {
		t.Fatalf("%s answered no id: %s", tool, text)
	}
	return created.ID
}

// smokeFirst reads the id of the first listed item whose status matches, failing when none does.
func smokeFirst(t *testing.T, text string, matches func(status string) bool) string {
	t.Helper()
	var listed struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("reading the list: %v: %s", err, text)
	}
	for _, item := range listed.Items {
		if matches(item.Status) {
			return item.ID
		}
	}
	t.Fatalf("the list holds no item to act on; smoke in an organization with an anomaly and an open savings finding: %s", text)
	return ""
}
