package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// megabyteOfDistinct is about 1 MiB of JSON, the hosted endpoint's body cap, as distinct strings
// item(i) makes.
func megabyteOfDistinct(item func(i int) string) []string {
	var out []string
	for size, i := 0, 1; size < 1<<20-1024; i++ {
		s := item(i)
		out = append(out, s)
		size += len(s) + 3
	}
	return out
}

// decodingBound is how long the attack's call may take: two seconds, or five times a call of the
// same size whose items all repeat, which decodes the same megabyte but merges one item, whichever
// is longer. -race and a loaded runner slow decoding a megabyte from about 0.1 s to over a second,
// which a fixed two seconds would not survive, while the quadratic merge took 16 to 50 times the
// repeating call under -race and 40 to 150 times without it, so the regression still fails.
func decodingBound(t *testing.T, cs *mcp.ClientSession, tool, idKey, id, addKey string, repeating []string) time.Duration {
	t.Helper()
	start := time.Now()
	if res := call(t, cs, tool, map[string]any{idKey: id, addKey: repeating}); res.IsError {
		t.Fatalf("%d repeats of one item refused: %.200s", len(repeating), text(res))
	}
	took := time.Since(start)
	t.Logf("%d repeats of one item answered after %s", len(repeating), took)
	return max(2*time.Second, 5*took)
}

// A hosted write's merge shares the container's CPU with every organization's calls, and nothing
// but the 1 MiB body bounds the list it adds, so a list far past what the API takes is refused, or
// merged in linear time, within two seconds or a few times what decoding the same body costs, not
// held for seconds to tens of seconds of a core.
func TestUpdateBudget_MegabyteOfDistinctThresholds_AnswersWithinTwoSeconds(t *testing.T) {
	api, cs := writer(t)
	add := megabyteOfDistinct(strconv.Itoa)
	bound := decodingBound(t, cs, "update_budget", "budget_id", "bud_1", "add_alert_thresholds", slices.Repeat([]string{"100000"}, len(add)))
	before := len(writesOf(api.received()))
	start := time.Now()
	res := call(t, cs, "update_budget", map[string]any{"budget_id": "bud_1", "add_alert_thresholds": add})
	took := time.Since(start)
	t.Logf("%d distinct thresholds: isError %v after %s, %d writes sent; answer starts %.120q",
		len(add), res.IsError, took, len(writesOf(api.received()))-before, text(res))
	if took > bound {
		t.Errorf("one update_budget call held its work slot and a core for %s; want a refusal or an answer within %s", took, bound)
	}
}

func TestUpdateCostAlert_MegabyteOfDistinctAppIDs_AnswersWithinTwoSeconds(t *testing.T) {
	api, cs := writer(t)
	add := megabyteOfDistinct(func(i int) string { return "app_" + strings.Repeat("0", 8-len(strconv.Itoa(i))) + strconv.Itoa(i) })
	bound := decodingBound(t, cs, "update_cost_alert", "cost_alert_id", "cal_1", "add_app_ids", slices.Repeat([]string{"app_00000001"}, len(add)))
	before := len(writesOf(api.received()))
	start := time.Now()
	res := call(t, cs, "update_cost_alert", map[string]any{"cost_alert_id": "cal_1", "add_app_ids": add})
	took := time.Since(start)
	t.Logf("%d distinct app ids: isError %v after %s, %d writes sent; answer starts %.120q",
		len(add), res.IsError, took, len(writesOf(api.received()))-before, text(res))
	if took > bound {
		t.Errorf("one update_cost_alert call held its work slot and a core for %s; want a refusal or an answer within %s", took, bound)
	}
}
