package docs

import "testing"

// The pairs are the rendered portal's own ids (built HTML, 2026-11-06), so a result's anchor lands
// on its heading rather than at the top of the page.
func TestSlugMatchesThePortalsIDs(t *testing.T) {
	for heading, want := range map[string]string{
		`What "Provider-reported, estimated" means`:      "what-provider-reported-estimated-means",
		"Read a folder's reports through a saved filter": "read-a-folders-reports-through-a-saved-filter",
		"Rechnung über ihre Kopfdaten erfassen":          "rechnung-über-ihre-kopfdaten-erfassen",
		"Das Azure-Abonnement … wird bereits erfasst":    "das-azure-abonnement--wird-bereits-erfasst",
		"snake_case and C# (beta)!":                      "snake_case-and-c-beta",
		"Größe 2.0":                                      "größe-20",
	} {
		if got := newSlugger().slug(heading); got != want {
			t.Errorf("slug(%q) = %q, want %q", heading, got, want)
		}
	}
}

func TestSlugNumbersARepeat(t *testing.T) {
	s := newSlugger()
	for _, want := range []string{"related", "related-1", "related-2"} {
		if got := s.slug("Related"); got != want {
			t.Fatalf("slug = %q, want %q", got, want)
		}
	}
	// A heading whose own id is a repeat's is numbered on in turn, as github-slugger does.
	if got := s.slug("Related 1"); got != "related-1-1" {
		t.Errorf("slug(Related 1) = %q, want related-1-1", got)
	}
}
