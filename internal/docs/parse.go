// Package docs reads Costfluent's public documentation for the docs tools: it fetches the portal's
// llms-full.txt, every listed page in both languages, splits it into pages and their sections, and
// searches them in process. Nothing a caller asks leaves the process: the one request it makes is
// for that file, from the one origin a Source is built with.
package docs

import (
	"errors"
	"strings"
)

// documentMarker opens every page in llms-full.txt, after a "---" line.
const documentMarker = "## Document: "

// urlPrefix starts the line that names a page's path, below its title and description.
const urlPrefix = "URL: "

// Document is one listed docs page.
type Document struct {
	Title       string
	Description string
	// Path is the page's path on the docs origin, "/developers/terraform" or "/de/connect/azure".
	Path string
	// Language is "de" for a page under /de, else "en".
	Language string
	// Markdown is the page as llms-full.txt carries it, from its own top heading on.
	Markdown string
	Sections []Section
}

// Section is a part of a page: its lede before the first "## " heading (Level 1, no Anchor), or a
// "## " or "### " heading and what follows it up to the next of either.
type Section struct {
	Level   int
	Heading string
	// Anchor is the heading's id on the rendered page, empty for the lede.
	Anchor string
	// Start and End are the section's byte offsets in the page's Markdown.
	Start, End int
}

// Text is the section's markdown, its heading line included.
func (d *Document) Text(s Section) string { return d.Markdown[s.Start:s.End] }

// errMalformed says the file is not the framing this package reads, so an upgrade of the portal's
// generator that changed it reads as an error rather than as docs with no pages.
var errMalformed = errors.New("it holds no page framed as \"## Document: <title>\" with a \"URL: /<path>\" line")

// Parse splits llms-full.txt into its pages. A file with no page that names its URL is malformed,
// whatever else it holds: an empty body, a login or error page in HTML, a changed framing.
func Parse(text string) ([]Document, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var docs []Document
	for _, chunk := range splitDocuments(text) {
		doc, ok := parseDocument(chunk)
		if ok {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		return nil, errMalformed
	}
	return docs, nil
}

// splitDocuments cuts text at every "---" line a document marker follows, and returns what follows
// each marker. It tracks no code fence: a page's unclosed fence would otherwise swallow every page
// after it, and no page's own text carries the marker's two lines.
func splitDocuments(text string) []string {
	parts := strings.Split(text, "\n---\n\n"+documentMarker)
	if strings.HasPrefix(text, "---\n\n"+documentMarker) {
		parts[0] = strings.TrimPrefix(parts[0], "---\n\n"+documentMarker)
		return parts
	}
	return parts[1:]
}

// parseDocument reads one page: its title line, then a description up to the URL line, then the
// page. A chunk with no URL line in its head is not a page.
func parseDocument(chunk string) (Document, bool) {
	title, rest, _ := strings.Cut(chunk, "\n")
	head, body, found := strings.Cut(rest, "\n"+urlPrefix)
	if !found {
		if !strings.HasPrefix(rest, urlPrefix) {
			return Document{}, false
		}
		head, body = "", strings.TrimPrefix(rest, urlPrefix)
	}
	path, markdown, _ := strings.Cut(body, "\n")
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t#?") {
		return Document{}, false
	}
	doc := Document{
		Title:       strings.TrimSpace(title),
		Description: strings.TrimSpace(head),
		Path:        path,
		Language:    languageOf(path),
		Markdown:    strings.Trim(markdown, "\n") + "\n",
	}
	doc.Sections = sections(doc.Markdown)
	return doc, true
}

func languageOf(path string) string {
	if path == "/de" || strings.HasPrefix(path, "/de/") {
		return "de"
	}
	return "en"
}

// sections splits a page's markdown at its "## " and "### " headings outside code fences. Every
// heading outside a fence counts toward the anchors, as rehype-slug numbers a repeated one on the
// rendered page whatever its level.
func sections(markdown string) []Section {
	var out []Section
	lede := Section{Level: 1}
	slugs := newSlugger()
	var fence fenceState
	offset := 0
	for _, raw := range strings.SplitAfter(markdown, "\n") {
		line := strings.TrimSuffix(raw, "\n")
		if !fence.advance(line) {
			if level, heading, ok := atxHeading(line); ok {
				anchor := slugs.slug(heading)
				switch {
				case level == 1 && lede.Heading == "" && offset == 0:
					lede.Heading = heading
				case level == 2 || level == 3:
					out = append(out, Section{Level: level, Heading: heading, Anchor: anchor, Start: offset})
				}
			}
		}
		offset += len(raw)
	}
	lede.End = len(markdown)
	if len(out) > 0 {
		lede.End = out[0].Start
	}
	for i := range out {
		out[i].End = len(markdown)
		if i+1 < len(out) {
			out[i].End = out[i+1].Start
		}
	}
	return append([]Section{lede}, out...)
}

// atxHeading reads a "# " to "###### " heading line, without its closing hashes.
func atxHeading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && level < 7 && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || (line[level] != ' ' && line[level] != '\t') {
		return 0, "", false
	}
	text := strings.TrimSpace(line[level:])
	if trimmed := strings.TrimRight(text, "#"); trimmed != text && (trimmed == "" || strings.HasSuffix(trimmed, " ")) {
		text = strings.TrimSpace(trimmed)
	}
	if text == "" {
		return 0, "", false
	}
	return level, text, true
}

// fenceState tracks whether a line is inside a ``` or ~~~ code fence, indented or not.
type fenceState struct {
	marker byte
	length int
}

func (f *fenceState) open() bool { return f.marker != 0 }

// advance reports whether line is part of a fence, its opening and closing lines included, and
// moves past it.
func (f *fenceState) advance(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	run := 0
	for run < len(trimmed) && (trimmed[run] == '`' || trimmed[run] == '~') && trimmed[run] == trimmed[0] {
		run++
	}
	if f.open() {
		if run >= f.length && trimmed[0] == f.marker && strings.TrimSpace(trimmed[run:]) == "" {
			f.marker, f.length = 0, 0
		}
		return true
	}
	if run >= 3 {
		f.marker, f.length = trimmed[0], run
		return true
	}
	return false
}
