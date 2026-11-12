package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The CLI reads Codex's config.toml line by line instead of through a TOML module: it needs to know
// only where each table starts, which keys a line defines and which lines sit inside a multi-line
// string, and a dependency for that would outweigh the reading.

// tomlKind is what one line of a TOML file is.
type tomlKind int

const (
	tomlOther  tomlKind = iota // a blank line, a comment, or a line it cannot read as a key
	tomlHeader                 // [table] or [[array of tables]]
	tomlKey                    // key = value, the key possibly dotted
	tomlString                 // a line inside a multi-line string a key above opened
)

// tomlLine is one line of a TOML file and what it defines.
type tomlLine struct {
	text  string
	kind  tomlKind
	path  []string // a header's table, or a key's path inside its table
	table []string // the table a key line sits in
}

var errOpenString = errors.New("a multi-line string is never closed")

// scanTOML reads text into lines. A key's segments are unquoted, so [mcp_servers.'costfluent'],
// [ mcp_servers . costfluent ] and [mcp_servers."costfluent"] # note read alike.
func scanTOML(text string) []tomlLine {
	lines, _ := scanTOMLChecked(text)
	return lines
}

func scanTOMLChecked(text string) ([]tomlLine, error) {
	var lines []tomlLine
	var table []string
	open := "" // the delimiter of the multi-line string the scan is inside, if any
	for _, raw := range strings.SplitAfter(text, "\n") {
		if raw == "" {
			continue
		}
		line := tomlLine{text: raw}
		if open != "" {
			line.kind = tomlString
			if strings.Count(raw, open)%2 == 1 {
				open = ""
			}
			lines = append(lines, line)
			continue
		}
		trimmed := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(trimmed, "["):
			inner := strings.TrimPrefix(strings.TrimPrefix(trimmed, "["), "[")
			if path, rest, ok := keyPath(inner); ok && strings.HasPrefix(rest, "]") {
				line.kind, line.path, table = tomlHeader, path, path
			}
		case trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			if path, rest, ok := keyPath(trimmed); ok && strings.HasPrefix(rest, "=") {
				line.kind, line.path, line.table = tomlKey, path, table
				open = opensString(rest)
			}
		}
		lines = append(lines, line)
	}
	if open != "" {
		return lines, errOpenString
	}
	return lines, nil
}

// opensString is the delimiter of a multi-line string a value starts and does not close on its
// line, or "".
func opensString(value string) string {
	for _, delimiter := range []string{`"""`, `'''`} {
		if strings.Count(value, delimiter)%2 == 1 {
			return delimiter
		}
	}
	return ""
}

// keyPath reads a dotted key from the start of s: bare, "basic" and 'literal' segments, with spaces
// around the dots. It returns the segments and what follows the key, trimmed.
func keyPath(s string) (path []string, rest string, ok bool) {
	s = strings.TrimSpace(s)
	for {
		var segment string
		switch {
		case strings.HasPrefix(s, `"`):
			end := closingQuote(s[1:])
			if end < 0 {
				return nil, "", false
			}
			segment, s = strings.ReplaceAll(s[1:1+end], `\"`, `"`), s[2+end:]
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, "", false
			}
			segment, s = s[1:1+end], s[2+end:]
		default:
			end := strings.IndexFunc(s, func(r rune) bool { return !bareKeyRune(r) })
			if end < 0 {
				end = len(s)
			}
			if end == 0 {
				return nil, "", false
			}
			segment, s = s[:end], s[end:]
		}
		path = append(path, segment)
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, ".") {
			return path, s, true
		}
		s = strings.TrimSpace(s[1:])
	}
}

// bareKeyRune reports whether r may appear in an unquoted TOML key.
func bareKeyRune(r rune) bool {
	return r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// closingQuote is the index of the first " in s that no backslash escapes, or -1.
func closingQuote(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// isCodexEntry reports whether a full key path is the server's entry or lies inside it.
func isCodexEntry(path []string) bool {
	return len(path) >= 2 && path[0] == "mcp_servers" && path[1] == serverName
}

// tomlQuote writes s as a TOML basic string. Go's strconv.Quote writes escapes such as \x01 and \a
// that TOML refuses, so control characters become \uXXXX; a string that is not UTF-8 has no TOML
// form at all.
func tomlQuote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%q is not UTF-8, which a TOML string must be", s)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}
