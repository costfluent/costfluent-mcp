package docs

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// published is a cut of the llms-full.txt the portal serves: four real pages, both languages.
func published(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/llms-full.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// framed builds an llms-full.txt the way the portal frames it.
func framed(pages ...[3]string) string {
	var b strings.Builder
	b.WriteString("# Costfluent Docs\n\n> Complete documentation for Large Language Models\n\n")
	for _, p := range pages {
		b.WriteString("\n---\n\n## Document: " + p[0] + "\n\nA page.\n\nURL: " + p[1] + "\n\n\n" + p[2] + "\n")
	}
	return b.String()
}

func TestParseReadsThePublishedFraming(t *testing.T) {
	docs, err := Parse(published(t))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, d := range docs {
		paths = append(paths, d.Path+" "+d.Language)
	}
	want := "/developers/terraform en,/connect/azure en,/de/developers/terraform de,/de/connect/azure de"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("pages %s, want %s", got, want)
	}
	tf := docs[0]
	if tf.Title != "Terraform" || !strings.HasPrefix(tf.Markdown, "# Terraform\n") || tf.Description == "" {
		t.Errorf("title %q, description %q, markdown starts %q", tf.Title, tf.Description, tf.Markdown[:20])
	}
	if strings.Contains(tf.Markdown, "## Document: ") {
		t.Error("a page's markdown runs into the next page")
	}
	// The terraform page's HCL comment "# The principal ..." sits in a fence, so it is no heading.
	var headings []string
	for _, s := range tf.Sections {
		headings = append(headings, s.Heading)
	}
	if strings.Contains(strings.Join(headings, "|"), "The principal your role must trust") {
		t.Errorf("a comment inside a fence read as a heading: %v", headings)
	}
	if tf.Sections[0].Level != 1 || tf.Sections[0].Heading != "Terraform" || tf.Sections[0].Start != 0 {
		t.Errorf("lede %+v", tf.Sections[0])
	}
}

func TestParseTreatsCRLFAsLF(t *testing.T) {
	text := framed([3]string{"One", "/one", "# One\n\nLede.\n\n## First part\n\nBody."})
	lf, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := Parse(strings.ReplaceAll(text, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(crlf) != 1 || crlf[0].Path != "/one" || crlf[0].Markdown != lf[0].Markdown || len(crlf[0].Sections) != 2 {
		t.Fatalf("CRLF page %+v, LF page %+v", crlf, lf)
	}
}

func TestParseRefusesAFileWithNoPage(t *testing.T) {
	for name, text := range map[string]string{
		"empty":         "",
		"html":          "<!doctype html><html><head><title>Sign in</title></head><body>Sign in</body></html>",
		"no URL line":   "# Costfluent Docs\n\n---\n\n## Document: One\n\nA page.\n\n# One\n",
		"relative URL":  "# Costfluent Docs\n\n---\n\n## Document: One\n\nA page.\n\nURL: one\n\n# One\n",
		"other framing": "# Costfluent Docs\n\n---\n\n## Page: One\n\nURL: /one\n\n# One\n",
	} {
		t.Run(name, func(t *testing.T) {
			if docs, err := Parse(text); !errors.Is(err, errMalformed) {
				t.Fatalf("Parse = %v, %v; want errMalformed", docs, err)
			}
		})
	}
}

func TestParseSkipsAPageWithNoURLAndKeepsTheRest(t *testing.T) {
	text := framed([3]string{"One", "/one", "# One\n"}) + "\n---\n\n## Document: Broken\n\nNo URL here.\n\n# Broken\n" +
		strings.TrimPrefix(framed([3]string{"Two", "/de/two", "# Two\n"}), "# Costfluent Docs\n\n> Complete documentation for Large Language Models\n\n")
	docs, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Path != "/one" || docs[1].Path != "/de/two" || docs[1].Language != "de" {
		t.Fatalf("pages %+v", docs)
	}
}

func TestSectionsSplitAtLevelTwoAndThreeOutsideFences(t *testing.T) {
	md := "# Page\n\nLede.\n\n## Install\n\n```bash\n## not a heading\n# nor this\n```\n\n   ~~~~\n   ### indented fence\n   ~~~\n   ~~~~\n\n### Step one\n\nText.\n\n#### Detail\n\nDeep.\n\n## Install\n\nAgain.\n##NoSpace\n"
	got := sections(md)
	type s struct {
		level           int
		heading, anchor string
	}
	var have []s
	for _, x := range got {
		have = append(have, s{x.Level, x.Heading, x.Anchor})
	}
	want := []s{{1, "Page", ""}, {2, "Install", "install"}, {3, "Step one", "step-one"}, {2, "Install", "install-1"}}
	if len(have) != len(want) {
		t.Fatalf("sections %+v, want %+v", have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, have[i], want[i])
		}
	}
	// Each section runs to the next "## " or "### ", the "#### " inside it included.
	if text := md[got[2].Start:got[2].End]; !strings.Contains(text, "#### Detail") || strings.Contains(text, "Again.") {
		t.Errorf("step one's text %q", text)
	}
	if got[0].End != got[1].Start || got[3].End != len(md) {
		t.Errorf("offsets %+v", got)
	}
}

func TestAtxHeadingDropsClosingHashes(t *testing.T) {
	for line, want := range map[string]string{
		"## Title ##":    "Title",
		"## C#":          "C#",
		"### Tabs\there": "Tabs\there",
		"#######  Too":   "",
		"#":              "",
		"## ":            "",
	} {
		_, got, ok := atxHeading(line)
		if got != want || ok != (want != "") {
			t.Errorf("atxHeading(%q) = %q, %v; want %q", line, got, ok, want)
		}
	}
}
