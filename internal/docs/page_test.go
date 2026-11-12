package docs

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPageReadsAPathInEveryForm(t *testing.T) {
	ix := publishedIndex(t)
	for _, ref := range []string{
		"/developers/terraform",
		"developers/terraform",
		"/developers/terraform/",
		"/developers/terraform.md",
		"/developers/terraform?ref=agent",
		"https://docs.costfluent.com/developers/terraform",
		"HTTPS://DOCS.COSTFLUENT.COM/developers/terraform.md",
		"docs.costfluent.com/developers/terraform",
		" https://docs.costfluent.com/developers/terraform/ ",
	} {
		p, err := ix.Page(ref, DefaultPageBytes)
		if err != nil {
			t.Errorf("Page(%q): %v", ref, err)
			continue
		}
		if p.Title != "Terraform" || p.URL != Origin+"/developers/terraform" || p.Truncated || !strings.HasPrefix(p.Markdown, "# Terraform\n") {
			t.Errorf("Page(%q) = %q at %s, truncated %v", ref, p.Title, p.URL, p.Truncated)
		}
	}
}

func TestPageListsEverySectionAndAFragmentSelectsOne(t *testing.T) {
	ix := publishedIndex(t)
	whole, err := ix.Page("/developers/terraform", DefaultPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	var headings []string
	for _, s := range whole.Sections {
		headings = append(headings, s.Heading)
		if !strings.HasPrefix(s.URL, Origin+"/developers/terraform#") {
			t.Errorf("section URL %s", s.URL)
		}
	}
	if !strings.Contains(strings.Join(headings, "|"), "Manage objects|Bring existing objects under Terraform") {
		t.Errorf("sections %v", headings)
	}

	p, err := ix.Page("https://docs.costfluent.com/developers/terraform#manage-objects", DefaultPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != Origin+"/developers/terraform#manage-objects" || !strings.HasPrefix(p.Markdown, "## Manage objects\n") ||
		strings.Contains(p.Markdown, "## Bring existing objects") || len(p.Sections) != len(whole.Sections) {
		t.Errorf("fragment page at %s, markdown %.80q, %d sections", p.URL, p.Markdown, len(p.Sections))
	}

	// A "## " section holds its "### " subsections; a "### " ends at the next of either.
	ix = fixtureIndex(t, [3]string{"Page", "/page", "# Page\n\nLede.\n\n## Parent\n\nP.\n\n### Child\n\nC.\n\n### Sibling\n\nS.\n\n## Next\n\nN."})
	if p, _ := ix.Page("/page#parent", DefaultPageBytes); p.Markdown != "## Parent\n\nP.\n\n### Child\n\nC.\n\n### Sibling\n\nS.\n\n" {
		t.Errorf("parent %q", p.Markdown)
	}
	if p, _ := ix.Page("/page#child", DefaultPageBytes); p.Markdown != "### Child\n\nC.\n\n" {
		t.Errorf("child %q", p.Markdown)
	}
	if _, err := ix.Page("/page#missing", DefaultPageBytes); err == nil || !strings.Contains(err.Error(), "no section #missing") {
		t.Errorf("missing fragment: %v", err)
	}
}

func TestPageCutsALongPageAtASection(t *testing.T) {
	ix := publishedIndex(t)
	p, err := ix.Page("/de/connect/azure", DefaultPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	full := ix.pages["/de/connect/azure"].Markdown
	if len(full) <= DefaultPageBytes {
		t.Fatalf("the fixture page is %d bytes, no longer than the cut", len(full))
	}
	if !p.Truncated || len(p.Markdown) > DefaultPageBytes || !strings.HasPrefix(full, p.Markdown) {
		t.Fatalf("truncated %v, %d bytes", p.Truncated, len(p.Markdown))
	}
	rest := full[len(p.Markdown):]
	if !strings.HasPrefix(rest, "## ") && !strings.HasPrefix(rest, "### ") {
		t.Errorf("the cut is not at a section: the rest starts %.40q", rest)
	}

	// A section longer than the bound alone is cut at a line, and a line longer than that at a
	// character, never inside one.
	ix = fixtureIndex(t, [3]string{"Long", "/long", "# Long\n\n" + strings.Repeat("ä line\n", 100) + strings.Repeat("ö", 500)})
	if p, _ := ix.Page("/long", 70); !p.Truncated || len(p.Markdown) > 70 || !strings.HasSuffix(p.Markdown, "\n") {
		t.Errorf("long section cut to %q", p.Markdown)
	}
	ix = fixtureIndex(t, [3]string{"Wide", "/wide", strings.Repeat("ö", 500)})
	if p, _ := ix.Page("/wide", 71); !p.Truncated || len(p.Markdown) > 71 || !utf8.ValidString(p.Markdown) {
		t.Errorf("wide line cut to %d bytes, valid %v", len(p.Markdown), utf8.ValidString(p.Markdown))
	}
}

// get_doc_page never fetches what it is given: a URL must name a listed page on the docs origin,
// so an unlisted page, another host or a trick of the URL is refused before any lookup.
func TestPageRefusesWhatIsNotAListedPageOnTheOrigin(t *testing.T) {
	ix := publishedIndex(t)
	for ref, want := range map[string]string{
		"": "url is required",
		"http://docs.costfluent.com/developers/terraform":          "reads only Costfluent's docs",
		"ftp://docs.costfluent.com/developers/terraform":           "reads only Costfluent's docs",
		"file:///etc/passwd":                                       "reads only Costfluent's docs",
		"https://docs.costfluent.dev/developers/terraform":         "reads only Costfluent's docs",
		"https://evil.example/developers/terraform":                "reads only Costfluent's docs",
		"https://docs.costfluent.com.evil.example/x":               "reads only Costfluent's docs",
		"https://user:pw@docs.costfluent.com/developers/terraform": "reads only Costfluent's docs",
		"https://docs.costfluent.com@evil.example/":                "reads only Costfluent's docs",
		"https://docs.costfluent.com:8443/developers/terraform":    "reads only Costfluent's docs",
		"//evil.example/developers/terraform":                      "reads only Costfluent's docs",
		"mailto:support@costfluent.com":                            "reads only Costfluent's docs",
		"https://docs.costfluent.com/connect/openai":               "no listed docs page is at /connect/openai",
		"/connect/openai.md":                                       "no listed docs page is at /connect/openai",
		"/llms-full.txt":                                           "no listed docs page is at /llms-full.txt",
		"https://docs.costfluent.com/":                             "no listed docs page is at /",
		"%zz":                                                      "is not a URL or a path",
	} {
		_, err := ix.Page(ref, DefaultPageBytes)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Page(%q) = %v, want %q", ref, err, want)
		}
		var transport *url.Error
		if errors.As(err, &transport) {
			t.Errorf("Page(%q) wraps a *url.Error, which the hosted server reports as an unreachable API", ref)
		}
	}
}
