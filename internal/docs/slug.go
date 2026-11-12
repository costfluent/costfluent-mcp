package docs

import (
	"strconv"
	"strings"
	"unicode"
)

// slugger gives a page's headings the ids the rendered page gives them: Zudoku's rehype-slug runs
// github-slugger, which lowercases a heading, drops every character but letters, marks, numbers,
// connector punctuation, hyphens and spaces, turns spaces into hyphens, and numbers a repeat
// "-1", "-2" on. A result URL whose anchor differs from the page's lands at the top of the page.
type slugger struct{ seen map[string]int }

func newSlugger() *slugger { return &slugger{seen: map[string]int{}} }

func (s *slugger) slug(heading string) string {
	base := slugOf(heading)
	result := base
	for {
		if _, taken := s.seen[result]; !taken {
			break
		}
		s.seen[base]++
		result = base + "-" + strconv.Itoa(s.seen[base])
	}
	s.seen[result] = 0
	return result
}

func slugOf(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
