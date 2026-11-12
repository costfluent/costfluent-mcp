package docs

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// DefaultPageBytes is how much of a page's markdown get_doc_page returns before it cuts the rest at
// a section and says so.
const DefaultPageBytes = 24_000

// Page is a docs page as get_doc_page returns it.
type Page struct {
	Title       string `json:"title" jsonschema:"the page's title"`
	URL         string `json:"url" jsonschema:"the page, or the section asked for, on the docs site"`
	Description string `json:"description,omitempty" jsonschema:"the page's summary"`
	Markdown    string `json:"markdown" jsonschema:"the page's text, or the section's"`
	// Truncated says the markdown stops before the end; Sections names what follows.
	Truncated bool         `json:"truncated,omitempty" jsonschema:"true when the markdown was cut at a section to bound the answer; get_doc_page with a section's URL reads the rest"`
	Sections  []SectionRef `json:"sections" jsonschema:"every section of the page, in order"`
}

// SectionRef names a section of a page and where it is.
type SectionRef struct {
	Heading string `json:"heading"`
	URL     string `json:"url"`
}

// Page returns the page ref names, a URL on the index's origin or a path: ".md", a trailing slash
// and a query are ignored, and a fragment selects that section. It never fetches ref: the page is
// looked up in the index, so an unlisted page or another host is refused rather than read. The
// markdown is cut at the last whole section within maxBytes; a section longer than that alone is
// cut at a line.
func (ix *Index) Page(ref string, maxBytes int) (*Page, error) {
	path, fragment, err := ix.resolve(ref)
	if err != nil {
		return nil, err
	}
	doc, ok := ix.pages[path]
	if !ok {
		return nil, fmt.Errorf("no listed docs page is at %s; search_docs finds the page that answers", path)
	}

	start, end := 0, len(doc.Markdown)
	pageURL := ix.url(doc.Path, "")
	if fragment != "" {
		i := -1
		for j, s := range doc.Sections {
			if s.Anchor == fragment {
				i = j
				break
			}
		}
		if i < 0 {
			return nil, fmt.Errorf("the page %s has no section #%s; get_doc_page without the fragment lists its sections", pageURL, fragment)
		}
		s := doc.Sections[i]
		start, end = s.Start, len(doc.Markdown)
		for _, next := range doc.Sections[i+1:] {
			if next.Level <= s.Level {
				end = next.Start
				break
			}
		}
		pageURL = ix.url(doc.Path, s.Anchor)
	}

	p := &Page{Title: doc.Title, URL: pageURL, Description: doc.Description, Sections: []SectionRef{}}
	for _, s := range doc.Sections {
		if s.Level == 1 {
			continue
		}
		p.Sections = append(p.Sections, SectionRef{Heading: s.Heading, URL: ix.url(doc.Path, s.Anchor)})
	}
	cut := end
	if end-start > maxBytes {
		cut = start
		for _, s := range doc.Sections {
			if s.Start > start && s.Start <= start+maxBytes && s.Start < end {
				cut = s.Start
			}
		}
		if cut == start {
			cut = lineCut(doc.Markdown, start, start+maxBytes)
		}
		p.Truncated = true
	}
	p.Markdown = doc.Markdown[start:cut]
	return p, nil
}

// lineCut is the last line end at or before limit in text after start, else the last character
// boundary, so a cut never splits a character.
func lineCut(text string, start, limit int) int {
	if nl := strings.LastIndexByte(text[start:limit], '\n'); nl >= 0 {
		return start + nl + 1
	}
	for limit > start && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return limit
}

// resolve reads ref as a page path and fragment. A URL must be https on the index's own host, with
// no user information and no other port.
func (ix *Index) resolve(ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("url is required: a page on %s, or its path", ix.origin)
	}
	u, err := url.Parse(ref)
	if err != nil {
		// The parse error's text alone: it is a *url.Error, which the hosted server reports as an
		// unreachable Public API.
		return "", "", fmt.Errorf("url %q is not a URL or a path: %s", ref, err.Error())
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		origin, _ := url.Parse(ix.origin)
		if u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, origin.Host) {
			return "", "", fmt.Errorf("get_doc_page reads only Costfluent's docs at %s, not %q", ix.origin, ref)
		}
	}
	if u.Opaque != "" {
		return "", "", fmt.Errorf("get_doc_page reads only Costfluent's docs at %s, not %q", ix.origin, ref)
	}
	path := u.Path
	if u.Host == "" {
		// A URL written without its scheme reads as a path that starts with the host.
		origin, _ := url.Parse(ix.origin)
		path = strings.TrimPrefix(path, origin.Host+"/")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".md")
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	return path, u.Fragment, nil
}
