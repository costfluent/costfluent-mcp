package docs

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Languages the portal is written in; the first is the default.
var Languages = []string{"en", "de"}

// Search bounds: results by default and at most, and the longest excerpt in characters.
const (
	DefaultLimit   = 5
	MaxLimit       = 10
	maxExcerpt     = 300
	sectionsOfPage = 2
)

// BM25F's saturation and length normalisation, and how much more a word counts in a section's
// heading and its page's title than in its text: a section titled with the words asked for is the
// answer more often than one that only mentions them.
const (
	k1            = 1.2
	b             = 0.75
	headingWeight = 3
	titleWeight   = 2
	// prefixMin is the shortest query word that also matches the longer words it begins, so
	// "budget" finds "budgets" and "Kosten" finds "Kostenstelle", while "aws" stays itself.
	prefixMin = 4
	// prefixWeight is how much a longer word a query word begins counts against the word itself, so
	// "manage" still finds "managed" without "management" outranking it.
	prefixWeight = 0.5
)

// Index is every page of one llms-full.txt, searchable per language.
type Index struct {
	origin string
	pages  map[string]*Document
	langs  map[string]*corpus
}

// corpus is the inverted index over one language's sections.
type corpus struct {
	entries  []entry
	postings map[string][]posting
	terms    []string // the keys of postings, sorted, for prefix lookups
	avgLen   float64
}

type entry struct {
	doc     *Document
	section Section
	length  float64
}

type posting struct {
	entry int
	tf    float64 // weighted: a word in the heading or title counts headingWeight or titleWeight times
}

// Hit is one section search_docs returns.
type Hit struct {
	Title   string `json:"title" jsonschema:"the page's title"`
	Heading string `json:"heading" jsonschema:"the section's heading"`
	URL     string `json:"url" jsonschema:"the section on the docs site; get_doc_page reads it"`
	Excerpt string `json:"excerpt" jsonschema:"the section's text around the first word that matched"`
}

// NewIndex indexes docs, whose URLs are on origin.
func NewIndex(origin string, docs []Document) *Index {
	ix := &Index{origin: origin, pages: map[string]*Document{}, langs: map[string]*corpus{}}
	for _, lang := range Languages {
		ix.langs[lang] = &corpus{postings: map[string][]posting{}}
	}
	for i := range docs {
		doc := &docs[i]
		ix.pages[doc.Path] = doc
		c := ix.langs[doc.Language]
		title := doc.Title
		for _, s := range doc.Sections {
			body := sectionBody(doc, s)
			if len(words(links.ReplaceAllString(body, ""))) == 0 {
				// A lede-less page's empty lede, or a list of links such as "Related", which answers
				// nothing itself and, being short, would outrank the pages it links to.
				continue
			}
			body = plain(body)
			tf := map[string]float64{}
			length := 0.0
			add := func(text string, weight float64) {
				for _, w := range words(text) {
					tf[w.term] += weight
					length += weight
				}
			}
			add(body, 1)
			add(s.Heading, headingWeight)
			add(title, titleWeight)
			n := len(c.entries)
			c.entries = append(c.entries, entry{doc: doc, section: s, length: length})
			for term, f := range tf {
				c.postings[term] = append(c.postings[term], posting{entry: n, tf: f})
			}
		}
	}
	for _, c := range ix.langs {
		total := 0.0
		for _, e := range c.entries {
			total += e.length
		}
		if len(c.entries) > 0 {
			c.avgLen = total / float64(len(c.entries))
		}
		for term := range c.postings {
			c.terms = append(c.terms, term)
		}
		slices.Sort(c.terms)
	}
	return ix
}

// Pages is how many pages the index holds.
func (ix *Index) Pages() int { return len(ix.pages) }

// sectionBody is a section's text without its heading line, which is indexed as the heading.
func sectionBody(doc *Document, s Section) string {
	text := doc.Text(s)
	if s.Level == 1 {
		if _, _, ok := atxHeading(strings.SplitN(text, "\n", 2)[0]); ok {
			_, text, _ = strings.Cut(text, "\n")
		}
		return text
	}
	_, text, _ = strings.Cut(text, "\n")
	return text
}

// ErrNoWords is a query that holds no word to search for.
var ErrNoWords = errors.New("the query holds no word to search for; ask in words, such as \"budget alerts terraform\"")

// Search ranks language's sections for query and returns the best limit, at most two of a page.
func (ix *Index) Search(query, language string, limit int) ([]Hit, error) {
	language = cmp.Or(language, Languages[0])
	c, ok := ix.langs[language]
	if !ok {
		return nil, fmt.Errorf("language must be one of %s, not %q", strings.Join(Languages, ", "), language)
	}
	limit = cmp.Or(limit, DefaultLimit)
	if limit < 1 || limit > MaxLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d, not %d", MaxLimit, limit)
	}
	var terms []string
	for _, w := range words(query) {
		if !stopWords[w.term] && !slices.Contains(terms, w.term) {
			terms = append(terms, w.term)
		}
	}
	if len(terms) == 0 {
		return nil, ErrNoWords
	}

	scores := map[int]float64{}
	n := float64(len(c.entries))
	for _, q := range terms {
		tf := map[int]float64{}
		for _, term := range c.matching(q) {
			weight := 1.0
			if term != q {
				weight = prefixWeight
			}
			for _, p := range c.postings[term] {
				tf[p.entry] += weight * p.tf
			}
		}
		if len(tf) == 0 {
			continue
		}
		df := float64(len(tf))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for e, f := range tf {
			norm := k1 * (1 - b + b*c.entries[e].length/c.avgLen)
			scores[e] += idf * f * (k1 + 1) / (f + norm)
		}
	}

	ranked := make([]int, 0, len(scores))
	for e := range scores {
		ranked = append(ranked, e)
	}
	slices.SortFunc(ranked, func(x, y int) int {
		if c := cmp.Compare(scores[y], scores[x]); c != 0 {
			return c
		}
		return cmp.Compare(x, y)
	})

	hits := []Hit{}
	perPage := map[string]int{}
	for _, e := range ranked {
		en := c.entries[e]
		if perPage[en.doc.Path] == sectionsOfPage {
			continue
		}
		perPage[en.doc.Path]++
		hits = append(hits, Hit{
			Title:   en.doc.Title,
			Heading: cmp.Or(en.section.Heading, en.doc.Title),
			URL:     ix.url(en.doc.Path, en.section.Anchor),
			Excerpt: excerpt(plain(sectionBody(en.doc, en.section)), terms),
		})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

// matching lists the indexed terms query word q matches: itself, and when it is long enough every
// longer term it begins.
func (c *corpus) matching(q string) []string {
	if utf8.RuneCountInString(q) < prefixMin {
		if _, ok := c.postings[q]; ok {
			return []string{q}
		}
		return nil
	}
	var out []string
	for i := sort.SearchStrings(c.terms, q); i < len(c.terms) && strings.HasPrefix(c.terms[i], q); i++ {
		out = append(out, c.terms[i])
	}
	return out
}

func (ix *Index) url(path, anchor string) string {
	u := ix.origin + path
	if anchor != "" {
		u += "#" + anchor
	}
	return u
}

// excerpt is body, its whitespace folded, when that is at most maxExcerpt characters; otherwise
// that many from around its first word a query term matches, starting at the beginning of that
// word's line when it is near.
func excerpt(body string, terms []string) string {
	if whole := strings.Join(strings.Fields(body), " "); utf8.RuneCountInString(whole) <= maxExcerpt {
		return whole
	}
	at := -1
	for _, w := range words(body) {
		if slices.ContainsFunc(terms, func(q string) bool {
			return q == w.term || (utf8.RuneCountInString(q) >= prefixMin && strings.HasPrefix(w.term, q))
		}) {
			at = w.start
			break
		}
	}
	start := 0
	if at > 0 {
		lineStart := strings.LastIndexByte(body[:at], '\n') + 1
		start = lineStart
		if utf8.RuneCountInString(body[lineStart:at]) > maxExcerpt/3 {
			start = at
			// Back up a few words for context, never into the line before.
			for back := 0; back < 40 && start > lineStart; back++ {
				_, size := utf8.DecodeLastRuneInString(body[:start])
				start -= size
			}
			if sp := strings.IndexAny(body[start:at], " \t"); sp >= 0 {
				start += sp + 1
			}
		}
	}
	text := strings.Join(strings.Fields(body[start:]), " ")
	prefix := ""
	if strings.TrimSpace(body[:start]) != "" {
		prefix = "…"
	}
	budget := maxExcerpt - utf8.RuneCountInString(prefix)
	if utf8.RuneCountInString(text) <= budget {
		return prefix + text
	}
	runes := []rune(text)[:budget-1]
	cut := string(runes)
	if sp := strings.LastIndexByte(cut, ' '); sp > len(cut)/2 {
		cut = cut[:sp]
	}
	return prefix + cut + "…"
}

// links matches a markdown link or image, whose text is read and whose target is not: a path such
// as /api/rate-limits would otherwise make every page that links there match its words.
var links = regexp.MustCompile(`!?\[([^\]]*)\]\([^)\s]*\)`)

// plain is text with each link reduced to its text.
func plain(text string) string { return links.ReplaceAllString(text, "$1") }

// stopWords are left out of a query: they match nearly every section and say nothing about which
// one answers.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an and are as at be by can do does for from how in is it of on or
		that the this to what when where which who why will with you your my me we our use using
		der die das den dem des ein eine einen einem einer und oder ist sind wie was wo wer warum mit
		für von zu im in an am auf bei ich sie ihr ihre ihren mein wir es kann können man`) {
		m[fold(w)] = true
	}
	return m
}()

// fold reduces a word to the form it is indexed and searched by: lowercased, and a plural "s" of a
// word of five letters or more dropped, so "budgets" and "budget" meet whichever is asked.
func fold(term string) string {
	term = strings.ToLower(term)
	if utf8.RuneCountInString(term) >= 5 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") {
		term = term[:len(term)-1]
	}
	return term
}

// word is a run of letters and digits, lowercased, and where it starts in the text it came from.
type word struct {
	term  string
	start int
}

// words splits text into its words: runs of letters, marks and digits of two characters or more,
// so punctuation, markup and single letters never match.
func words(text string) []word {
	var out []word
	start := -1
	flush := func(end int) {
		if start >= 0 && utf8.RuneCountInString(text[start:end]) >= 2 {
			out = append(out, word{term: fold(text[start:end]), start: start})
		}
		start = -1
	}
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(text))
	return out
}
