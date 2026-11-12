package docs

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func publishedIndex(t *testing.T) *Index {
	t.Helper()
	docs, err := Parse(published(t))
	if err != nil {
		t.Fatal(err)
	}
	return NewIndex(Origin, docs)
}

func fixtureIndex(t *testing.T, pages ...[3]string) *Index {
	t.Helper()
	docs, err := Parse(framed(pages...))
	if err != nil {
		t.Fatal(err)
	}
	return NewIndex("https://docs.example.test", docs)
}

func search(t *testing.T, ix *Index, query, language string, limit int) []Hit {
	t.Helper()
	hits, err := ix.Search(query, language, limit)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return hits
}

func TestSearchFindsTheTerraformSectionInEitherLanguage(t *testing.T) {
	ix := publishedIndex(t)
	for _, tc := range []struct{ query, language, want string }{
		{"how do I manage budgets with Terraform?", "", Origin + "/developers/terraform#manage-objects"},
		{"Budgets mit Terraform verwalten", "de", Origin + "/de/developers/terraform#objekte-verwalten"},
		{"Cost Management Reader role", "en", Origin + "/connect/azure#"},
	} {
		hits := search(t, ix, tc.query, tc.language, 5)
		if len(hits) == 0 || !strings.HasPrefix(hits[0].URL, tc.want) {
			t.Errorf("%q: first hit %+v, want %s", tc.query, hits, tc.want)
		}
		for _, h := range hits {
			if lang := languageOf(strings.TrimPrefix(h.URL, Origin)); lang != cmpOr(tc.language, "en") {
				t.Errorf("%q in %q returned %s, a %s page", tc.query, tc.language, h.URL, lang)
			}
		}
	}
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func TestSearchRanksTitleAndHeadingAboveBody(t *testing.T) {
	ix := fixtureIndex(t,
		[3]string{"Mentions", "/mentions", "# Mentions\n\nIntro.\n\n## Other things\n\nThis part says webhooks once among many other words about something else entirely."},
		[3]string{"Guide", "/guide", "# Guide\n\nIntro.\n\n## Webhooks\n\nHow to set them up, with many other words about something else entirely."},
		[3]string{"Webhooks", "/webhooks", "# Webhooks\n\nIntro.\n\n## Setup\n\nHow to set them up, with many other words about something else entirely."},
	)
	hits := search(t, ix, "webhooks", "en", 5)
	var urls []string
	for _, h := range hits {
		urls = append(urls, h.URL)
	}
	if len(urls) < 3 || urls[len(urls)-1] != "https://docs.example.test/mentions#other-things" {
		t.Fatalf("hits %v: the section that only mentions the word must rank last", urls)
	}
	if !strings.HasPrefix(urls[0], "https://docs.example.test/guide#webhooks") && !strings.HasPrefix(urls[0], "https://docs.example.test/webhooks") {
		t.Errorf("first hit %s, want a heading or title match", urls[0])
	}
}

func TestSearchKeepsAtMostTwoSectionsOfAPageAndTheLimit(t *testing.T) {
	var md strings.Builder
	md.WriteString("# Alerts\n\nAlerts lede.\n")
	for _, h := range []string{"Alerts one", "Alerts two", "Alerts three", "Alerts four"} {
		md.WriteString("\n## " + h + "\n\nAlerts text.\n")
	}
	ix := fixtureIndex(t, [3]string{"Alerts", "/alerts", md.String()}, [3]string{"Budgets", "/budgets", "# Budgets\n\nAlerts on budgets.\n"})
	hits := search(t, ix, "alerts", "en", 10)
	perPage := map[string]int{}
	for _, h := range hits {
		perPage[strings.SplitN(h.URL, "#", 2)[0]]++
	}
	if perPage["https://docs.example.test/alerts"] != 2 || perPage["https://docs.example.test/budgets"] != 1 {
		t.Errorf("per page %v, want two of /alerts and the one of /budgets", perPage)
	}
	if hits := search(t, ix, "alerts", "en", 1); len(hits) != 1 {
		t.Errorf("limit 1 returned %d", len(hits))
	}
	if hits := search(t, ix, "alerts", "en", 0); len(hits) != 3 {
		t.Errorf("the default limit returned %d of 3", len(hits))
	}
}

func TestSearchMatchesLongerWordsAQueryWordBegins(t *testing.T) {
	ix := fixtureIndex(t,
		[3]string{"Kosten", "/de/kosten", "# Kosten\n\nLede.\n\n## Kostenstellen zuordnen\n\nText.\n"},
		[3]string{"Cloud", "/cloud", "# Cloud\n\nLede about aws.\n\n## Awsome\n\nawsome text.\n\n## Budget\n\nOne budget.\n"},
	)
	var found []string
	for _, h := range search(t, ix, "kosten", "de", 5) {
		found = append(found, strings.TrimPrefix(h.URL, "https://docs.example.test"))
	}
	slices.Sort(found)
	if want := []string{"/de/kosten", "/de/kosten#kostenstellen-zuordnen"}; !slices.Equal(found, want) {
		t.Errorf("kosten found %v, want %v", found, want)
	}
	// A word shorter than four letters matches only itself.
	if hits := search(t, ix, "aws", "en", 5); len(hits) != 1 || hits[0].URL != "https://docs.example.test/cloud" {
		t.Errorf("aws matched %+v, want only the lede that says aws", hits)
	}
	// A plural meets its singular either way round.
	if hits := search(t, ix, "budgets", "en", 5); len(hits) == 0 || !strings.HasSuffix(hits[0].URL, "#budget") {
		t.Errorf("budgets: %+v, want #budget", hits)
	}
}

func TestSearchLeavesOutLinkListsAndLinkTargets(t *testing.T) {
	ix := fixtureIndex(t,
		[3]string{"Errors", "/api/errors", "# Errors\n\nLede.\n\n## Retry\n\nRead [the budget](/api/rate-limits) first.\n\n## Related\n\n- [Rate limits](/api/rate-limits)\n- [Pagination](/api/pagination)\n"},
	)
	for _, h := range search(t, ix, "rate limits pagination", "en", 5) {
		if strings.HasSuffix(h.URL, "#related") {
			t.Errorf("the link list %s was returned", h.URL)
		}
	}
	if hits, _ := ix.Search("rate limits", "en", 5); len(hits) != 0 {
		t.Errorf("a link target matched: %+v", hits)
	}
	if hits := search(t, ix, "budget", "en", 5); len(hits) != 1 || hits[0].Excerpt != "Read the budget first." {
		t.Errorf("budget: %+v, want the link's text in the excerpt", hits)
	}
}

func TestSearchRefusesAQueryWithNoWordAndBadBounds(t *testing.T) {
	ix := publishedIndex(t)
	for _, q := range []string{"", "  ", "?!.-", "how do I", "a #"} {
		if _, err := ix.Search(q, "en", 5); !errors.Is(err, ErrNoWords) {
			t.Errorf("Search(%q) = %v, want ErrNoWords", q, err)
		}
	}
	for _, tc := range []struct {
		language string
		limit    int
		want     string
	}{{"fr", 5, "language must be one of en, de"}, {"en", 11, "between 1 and 10"}, {"en", -1, "between 1 and 10"}} {
		if _, err := ix.Search("terraform", tc.language, tc.limit); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Search(%s, %d) = %v, want %q", tc.language, tc.limit, err, tc.want)
		}
	}
	if hits := search(t, ix, "xylophone", "en", 5); hits == nil || len(hits) != 0 {
		t.Errorf("no match is %#v, want an empty list", hits)
	}
}

func TestExcerptStartsAtTheFirstHitWithinTheBound(t *testing.T) {
	long := strings.Repeat("filler word ", 60) + "The budget threshold fires an alert. " + strings.Repeat("after ", 100)
	ix := fixtureIndex(t, [3]string{"Page", "/page", "# Page\n\nLede.\n\n## Part\n\nFirst line.\n" + long})
	hits := search(t, ix, "threshold", "en", 1)
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	ex := hits[0].Excerpt
	if n := utf8.RuneCountInString(ex); n > maxExcerpt {
		t.Errorf("excerpt is %d characters, past %d", n, maxExcerpt)
	}
	if !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") || !strings.Contains(ex, "budget threshold fires") {
		t.Errorf("excerpt %q, want the hit with markers at both cuts", ex)
	}
	if i := strings.Index(ex, "threshold"); i > 200 {
		t.Errorf("the hit is at %d of the excerpt, want it near the start: %q", i, ex)
	}

	// A short section is its whole text, whitespace folded, with no markers.
	ix = fixtureIndex(t, [3]string{"Page", "/page", "# Page\n\nLede.\n\n## Part\n\nA  short\n\nthreshold   text."})
	if ex := search(t, ix, "threshold", "en", 1)[0].Excerpt; ex != "A short threshold text." {
		t.Errorf("short excerpt %q", ex)
	}
	// Characters beyond ASCII count as one each and are never split.
	ix = fixtureIndex(t, [3]string{"Seite", "/de/seite", "# Seite\n\nLede.\n\n## Teil\n\n" + strings.Repeat("Größenänderung ", 40)})
	if ex := search(t, ix, "größenänderung", "de", 1)[0].Excerpt; utf8.RuneCountInString(ex) > maxExcerpt || !utf8.ValidString(ex) {
		t.Errorf("excerpt %q", ex)
	}
}
