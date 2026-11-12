package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/docs"
)

var docsTools = []string{"search_docs", "get_doc_page"}

// docsOrigin is a TLS docs portal serving body as its llms-full.txt, recording the path of every
// request it is sent.
type docsOrigin struct {
	*httptest.Server
	mu    sync.Mutex
	body  string
	code  int
	paths []string
}

func newDocsOrigin(t *testing.T, body string) *docsOrigin {
	t.Helper()
	o := &docsOrigin{body: body, code: http.StatusOK}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.paths = append(o.paths, r.URL.RequestURI())
		if r.URL.Path != "/llms-full.txt" || o.code != http.StatusOK {
			w.WriteHeader(cmpCode(o.code, r.URL.Path))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(o.body))
	}))
	t.Cleanup(o.Close)
	return o
}

func cmpCode(code int, path string) int {
	if path != "/llms-full.txt" {
		return http.StatusNotFound
	}
	return code
}

func (o *docsOrigin) requested() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.paths)
}

func (o *docsOrigin) source() *docs.Source {
	return docs.NewSource(docs.Config{Origin: o.URL, Transport: o.Client().Transport})
}

// publishedDocs is the docs package's cut of the portal's published llms-full.txt.
func publishedDocs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../docs/testdata/llms-full.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// docsSession serves a token with capabilities, its docs read from source.
func docsSession(t *testing.T, source *docs.Source, opts Options, capabilities ...costfluent.Capability) *mcp.ClientSession {
	t.Helper()
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {workspace: ownWS, capabilities: capabilities}})
	client := costfluent.NewClient(costfluent.WithAPIKey(tokenSecret), costfluent.WithBaseURL(srv.URL))
	me, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opts.Version, opts.Docs = "test", source
	built, err := Build(client, me, opts)
	if err != nil {
		t.Fatal(err)
	}
	return connectTo(t, built)
}

func searchResult(t *testing.T, cs *mcp.ClientSession, args map[string]any) SearchDocsResult {
	t.Helper()
	res := call(t, cs, "search_docs", args)
	if res.IsError {
		t.Fatalf("search_docs %v: %s", args, text(res))
	}
	var out SearchDocsResult
	decode(t, res, &out)
	return out
}

func TestDocsToolsAreServedBesideTheGatedTools(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{}, costfluent.CapabilityViewBudgets)
	// tools/list names tools in alphabetical order; ToolNames keeps the registry's, docs last.
	if got, want := listed(t, cs), []string{"get_doc_page", "list_budgets", "search_docs"}; !slices.Equal(got, want) {
		t.Fatalf("a token that may read budgets is served %v, want %v", got, want)
	}
	if got := ToolNames(&costfluent.Me{Capabilities: []costfluent.Capability{costfluent.CapabilityViewBudgets}}); !slices.Equal(got, []string{"list_budgets", "search_docs", "get_doc_page"}) {
		t.Errorf("ToolNames = %v", got)
	}

	// The docs alone are no Costfluent server: a token no gated tool serves is refused, as before.
	for name, me := range map[string]*costfluent.Me{
		"no capability":                   {TokenName: "narrow", Capabilities: []costfluent.Capability{costfluent.CapabilityViewProviders}},
		"writes the organization refuses": {TokenName: "writer", Capabilities: []costfluent.Capability{costfluent.CapabilityCreateBudget}},
	} {
		if _, err := Build(costfluent.NewClient(), me, Options{Version: "test", Docs: o.source()}); err == nil {
			t.Errorf("%s: Build served the docs tools alone", name)
		}
		if got := ToolNames(me); got != nil {
			t.Errorf("%s: ToolNames = %v, want none", name, got)
		}
	}
	if paths := o.requested(); len(paths) != 0 {
		t.Errorf("serving the tools fetched the docs: %v", paths)
	}

	instructions := cs.InitializeResult().Instructions
	if !strings.Contains(instructions, "search_docs and get_doc_page read Costfluent's public documentation") {
		t.Errorf("the instructions do not name the docs tools: %q", instructions)
	}
}

func TestOnlyTheDocsToolsNeedNoCapability(t *testing.T) {
	var ungated []string
	for _, e := range Catalog() {
		if e.Capability == NoCapability {
			ungated = append(ungated, e.Name)
			if e.Writes {
				t.Errorf("%s needs no capability and writes", e.Name)
			}
		}
	}
	if !slices.Equal(ungated, docsTools) {
		t.Fatalf("tools needing no capability %v, want exactly %v", ungated, docsTools)
	}
	all := Catalog()
	if tail := all[len(all)-2:]; tail[0].Name != "search_docs" || tail[1].Name != "get_doc_page" {
		t.Errorf("the docs tools are not registered last: %v", tail)
	}
}

func TestSearchDocsFindsTheSectionThatAnswers(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{}, allCapabilities...)

	got := searchResult(t, cs, map[string]any{"query": "how do I manage budgets with Terraform?"})
	if len(got.Results) == 0 || got.Results[0].URL != o.URL+"/developers/terraform#manage-objects" {
		t.Fatalf("results %+v, want /developers/terraform#manage-objects first", got.Results)
	}
	first := got.Results[0]
	if first.Title != "Terraform" || first.Heading != "Manage objects" || first.Excerpt == "" || len([]rune(first.Excerpt)) > 300 {
		t.Errorf("first result %+v", first)
	}
	if len(got.Results) > 5 {
		t.Errorf("%d results by default, want at most 5", len(got.Results))
	}

	de := searchResult(t, cs, map[string]any{"query": "Budgets mit Terraform verwalten", "language": "de", "limit": 2})
	if len(de.Results) != 2 {
		t.Fatalf("limit 2 returned %d", len(de.Results))
	}
	for _, r := range de.Results {
		if !strings.HasPrefix(r.URL, o.URL+"/de/") {
			t.Errorf("a German search returned %s", r.URL)
		}
	}

	if none := searchResult(t, cs, map[string]any{"query": "xylophone"}); none.Results == nil || len(none.Results) != 0 {
		t.Errorf("no match is %#v, want an empty list", none.Results)
	}

	for args, want := range map[string]string{
		`{"query":"?!"}`:                        "holds no word",
		`{"query":"terraform","language":"fr"}`: "language must be one of en, de",
		`{"query":"terraform","limit":11}`:      "between 1 and 10",
	} {
		var a map[string]any
		_ = json.Unmarshal([]byte(args), &a)
		res := call(t, cs, "search_docs", a)
		if !res.IsError || !strings.Contains(text(res), want) {
			t.Errorf("search_docs %s: IsError %v, %q; want %q", args, res.IsError, text(res), want)
		}
	}
	// One fetch served every call.
	if paths := o.requested(); !slices.Equal(paths, []string{"/llms-full.txt"}) {
		t.Errorf("the docs origin was asked %v", paths)
	}
}

func TestGetDocPageReadsOnlyTheDocsIndex(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	cs := docsSession(t, o.source(), Options{}, allCapabilities...)

	hit := searchResult(t, cs, map[string]any{"query": "how do I manage budgets with Terraform?"}).Results[0]
	res := call(t, cs, "get_doc_page", map[string]any{"url": hit.URL})
	if res.IsError {
		t.Fatal(text(res))
	}
	var page docs.Page
	decode(t, res, &page)
	if page.URL != hit.URL || page.Title != "Terraform" || !strings.HasPrefix(page.Markdown, "## Manage objects\n") || page.Truncated {
		t.Errorf("page %s %q truncated %v, markdown %.60q", page.URL, page.Title, page.Truncated, page.Markdown)
	}
	if len(page.Sections) < 5 || !strings.HasPrefix(page.Sections[0].URL, o.URL+"/developers/terraform#") {
		t.Errorf("sections %+v", page.Sections)
	}

	whole := call(t, cs, "get_doc_page", map[string]any{"url": "/de/developers/terraform.md"})
	if whole.IsError || !strings.Contains(text(whole), `"title":"Terraform"`) || !strings.Contains(text(whole), "Objekte verwalten") {
		t.Errorf("the German page by path: %s", text(whole))
	}

	for _, url := range []string{
		"https://docs.costfluent.com.evil.example/developers/terraform",
		"http://" + strings.TrimPrefix(o.URL, "https://") + "/developers/terraform",
		"https://169.254.169.254/latest/meta-data/",
		o.URL + "/llms-full.txt",
		o.URL + "/connect/openai.md",
	} {
		res := call(t, cs, "get_doc_page", map[string]any{"url": url})
		if !res.IsError {
			t.Errorf("get_doc_page %s succeeded: %.80s", url, text(res))
		}
	}
	// Nothing the model passed was fetched: the origin saw only the one index request.
	if paths := o.requested(); !slices.Equal(paths, []string{"/llms-full.txt"}) {
		t.Errorf("the docs origin was asked %v", paths)
	}
}

// A page's answer is bounded like get_cost_report_data's, its two copies within an assistant's tool
// result limit, however much of its markdown JSON escapes.
func TestGetDocPageBoundsALongPage(t *testing.T) {
	escaped := strings.Repeat("<Callout type=\"note\">a & b</Callout>\n", 40)
	var md strings.Builder
	md.WriteString("# Long\n\nLede.\n")
	for i := range 60 {
		md.WriteString("\n## Part " + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "\n\n" + escaped)
	}
	body := publishedDocs(t) + "\n---\n\n## Document: Long\n\nA long page.\n\nURL: /long\n\n\n" + md.String()
	if md.Len() < 2*docs.DefaultPageBytes {
		t.Fatalf("the long page is %d bytes", md.Len())
	}
	o := newDocsOrigin(t, body)
	cs := docsSession(t, o.source(), Options{}, allCapabilities...)
	for _, url := range []string{"/de/connect/azure", "/long"} {
		res := call(t, cs, "get_doc_page", map[string]any{"url": url})
		if res.IsError {
			t.Fatalf("%s: %s", url, text(res))
		}
		var page docs.Page
		decode(t, res, &page)
		size := encoded(t, page)
		if size > maxReportBytes || !page.Truncated {
			t.Errorf("%s answers %d bytes, truncated %v; want within %d and cut", url, size, page.Truncated, maxReportBytes)
		}
		if len(text(res)) > maxReportBytes {
			t.Errorf("%s's text fallback is %d bytes", url, len(text(res)))
		}
		if !strings.HasPrefix(page.Markdown, "# ") || len(page.Markdown) < docs.DefaultPageBytes/4 {
			t.Errorf("%s kept %d bytes of markdown", url, len(page.Markdown))
		}
	}
}

func TestDocsToolsReportAnUnreadableIndexAsAToolError(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"404":       {code: http.StatusNotFound, want: "answered 404 Not Found"},
		"malformed": {body: "<!doctype html><title>Sign in</title>", code: http.StatusOK, want: "is malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			o := newDocsOrigin(t, tc.body)
			o.code = tc.code
			cs := docsSession(t, o.source(), Options{}, allCapabilities...)
			for tool, args := range map[string]map[string]any{"search_docs": {"query": "terraform"}, "get_doc_page": {"url": "/developers/terraform"}} {
				res := call(t, cs, tool, args)
				if !res.IsError || !strings.Contains(text(res), tc.want) || !strings.Contains(text(res), o.URL) || !strings.Contains(text(res), "try again") {
					t.Errorf("%s: IsError %v, %q; want a retryable error naming %s and %q", tool, res.IsError, text(res), o.URL, tc.want)
				}
				if res.StructuredContent != nil {
					t.Errorf("%s: a failed call carried structured content", tool)
				}
			}
			// The calls fetched once; the failure is retried a minute later, not on every call.
			if got := o.requested(); len(got) != 1 {
				t.Errorf("the docs origin was asked %v", got)
			}
		})
	}
}

// The hosted server answers a failure to reach the Public API with one generic error, so a docs
// failure must not read as one: the caller would be told the API is down.
func TestBuildHostedTellsACallerTheDocsWereUnreachable(t *testing.T) {
	o := newDocsOrigin(t, publishedDocs(t))
	o.Close()
	cs := docsSession(t, o.source(), Options{Hosted: true}, allCapabilities...)
	res := call(t, cs, "search_docs", map[string]any{"query": "terraform"})
	if !res.IsError || text(res) == errAPIUnreachable.Error() || !strings.Contains(text(res), "Costfluent's docs at "+o.URL+" could not be read") {
		t.Fatalf("hosted: IsError %v, %q; want the docs named as unreachable", res.IsError, text(res))
	}
	res = call(t, cs, "get_doc_page", map[string]any{"url": "%zz"})
	if !res.IsError || text(res) == errAPIUnreachable.Error() {
		t.Errorf("hosted: a URL that does not parse reads %q", text(res))
	}
}
