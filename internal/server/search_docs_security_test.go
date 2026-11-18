package server

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/docs"
)

// megabyteOfDistinctWords is about 1 MiB, the hosted endpoint's body cap, of distinct four-character
// words led by a few the docs hold, so the search and its excerpts run over real hits too.
func megabyteOfDistinctWords() string {
	var b strings.Builder
	b.WriteString("budget alerts terraform")
	for i := 0; b.Len() < 1<<20-64; i++ {
		w := strconv.FormatInt(int64(i), 36)
		b.WriteByte(' ')
		b.WriteString(strings.Repeat("0", 4-len(w)))
		b.WriteString(w)
	}
	return b.String()
}

// distinctWords is n distinct words that are no stop word, led by three the docs hold.
func distinctWords(n int) string {
	ws := []string{"budget", "alerts", "terraform"}
	for i := 0; len(ws) < n; i++ {
		ws = append(ws, "w"+strconv.Itoa(i))
	}
	return strings.Join(ws[:n], " ")
}

// megabyteOf repeats unit to about 1 MiB.
func megabyteOf(unit string) string {
	return strings.Repeat(unit, (1<<20-64)/len(unit))
}

// searchCallBound is what one search_docs call may take. Before the bound on distinct words, the
// megabyte of distinct words held a core for about 90 s; with it each megabyte query answers in well
// under two seconds, even under -race. Ten seconds keeps that regression failing by a wide margin while a
// loaded CI runner, slower many times over, still passes.
const searchCallBound = 10 * time.Second

// A hosted docs search shares the container's CPU with every organization's calls, and nothing but
// the 1 MiB body bounds its query, so one call must cost time linear in a small bound: a query of a
// megabyte of distinct words is refused or searched within searchCallBound, not held for a minute and more.
func TestSearchDocs_MegabyteQueryOfDistinctWords_AnswersWithinTheCallBound(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{Hosted: true, CallTimeout: 95 * time.Second}, costfluent.CapabilityViewBudgets)
	// Load the index first, so the timed call measures the search alone.
	searchResult(t, cs, map[string]any{"query": "budget alerts"})

	query := megabyteOfDistinctWords()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	res := call(t, cs, "search_docs", map[string]any{"query": query})
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	t.Logf("a %d-byte query of %d words: isError %v after %s, %d MiB allocated; answer starts %.120q",
		len(query), strings.Count(query, " ")+1, res.IsError, took, (after.TotalAlloc-before.TotalAlloc)>>20, text(res))
	if took > searchCallBound {
		t.Errorf("one search_docs call held its work slot and a core for %s; want a refusal or an answer within %s", took, searchCallBound)
	}
}

// The other megabyte queries an organization can send: the same words repeated still answer, and
// words too short to search, or more distinct ones than the bound, are refused; each call within searchCallBound.
func TestSearchDocs_MegabyteQueriesOfOtherShapes_AnswerOrAreRefusedWithinTheCallBound(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{Hosted: true, CallTimeout: 95 * time.Second}, costfluent.CapabilityViewBudgets)
	searchResult(t, cs, map[string]any{"query": "budget alerts"})

	for _, tc := range []struct {
		name, query, refusal string
	}{
		{"repeated words", megabyteOf("budget alerts terraform "), ""},
		{"repeated words among stop words", megabyteOf("how do I set the budget alerts with terraform "), ""},
		{"one-character words", megabyteOf("a b c "), docs.ErrNoWords.Error()},
		{"distinct two-character words", megabyteOfTwoCharacterWords(), "more than " + strconv.Itoa(docs.MaxQueryTerms)},
	} {
		start := time.Now()
		res := call(t, cs, "search_docs", map[string]any{"query": tc.query})
		took := time.Since(start)
		if took > searchCallBound {
			t.Errorf("%s: one call took %s; want an answer or a refusal within %s", tc.name, took, searchCallBound)
		}
		switch {
		case tc.refusal == "" && res.IsError:
			t.Errorf("%s: refused with %q, want the sections it asks for", tc.name, text(res))
		case tc.refusal == "" && !strings.Contains(text(res), "/developers/terraform"):
			t.Errorf("%s: answer %.200q, want the Terraform sections", tc.name, text(res))
		case tc.refusal != "" && (!res.IsError || !strings.Contains(text(res), tc.refusal)):
			t.Errorf("%s: isError %v, %.200q; want a refusal naming %q", tc.name, res.IsError, text(res), tc.refusal)
		}
	}
}

// megabyteOfTwoCharacterWords cycles every two-character word of base 36, the shortest searched.
func megabyteOfTwoCharacterWords() string {
	var b strings.Builder
	for i := 0; b.Len() < 1<<20-64; i++ {
		w := strconv.FormatInt(int64(i%(36*36)), 36)
		b.WriteString(strings.Repeat("0", 2-len(w)))
		b.WriteString(w)
		b.WriteByte(' ')
	}
	return b.String()
}

// The bound is on distinct words, stop words aside: a query of exactly that many still finds its
// sections, and one more is refused with an error that names the bound, so the assistant can ask again.
func TestSearchDocs_QueryAtTheWordBound_AnswersAndOneMoreIsRefused(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{Hosted: true, CallTimeout: 95 * time.Second}, costfluent.CapabilityViewBudgets)

	at := searchResult(t, cs, map[string]any{"query": distinctWords(docs.MaxQueryTerms) + " how do I the with"})
	if len(at.Results) == 0 || !strings.Contains(at.Results[0].URL, "/developers/terraform") {
		t.Errorf("a query of %d distinct words: %+v, want the Terraform sections", docs.MaxQueryTerms, at.Results)
	}
	res := call(t, cs, "search_docs", map[string]any{"query": distinctWords(docs.MaxQueryTerms + 1)})
	if !res.IsError || !strings.Contains(text(res), "more than "+strconv.Itoa(docs.MaxQueryTerms)) {
		t.Errorf("a query of %d distinct words: isError %v, %.200q; want a refusal naming %d", docs.MaxQueryTerms+1, res.IsError, text(res), docs.MaxQueryTerms)
	}
	empty := call(t, cs, "search_docs", map[string]any{"query": ""})
	if !empty.IsError || !strings.Contains(text(empty), docs.ErrNoWords.Error()) {
		t.Errorf("an empty query: isError %v, %q; want ErrNoWords", empty.IsError, text(empty))
	}
}
