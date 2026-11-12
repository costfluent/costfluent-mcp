package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
)

// writeJSON prints a result as the tool returned it, indented. It reindents the JSON text rather
// than decoding it, so every key keeps its order and every number its digits. For a terminal it also
// escapes what escape does: Go's encoder escapes C0 controls but writes C1 controls, DEL and
// bidirectional overrides as they are, and those can only stand inside strings, where the \uXXXX
// form decodes to the same text.
func writeJSON(w io.Writer, raw []byte, terminal bool) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("the result is not JSON: %w", err)
	}
	buf.WriteByte('\n')
	text := buf.String()
	if terminal {
		text = escapeExcept(text, "\n")
	}
	_, err := io.WriteString(w, text)
	return err
}

// field is one member of a JSON object, in the order the object wrote it.
type field struct {
	key   string
	value json.RawMessage
}

// fieldsOf decodes a JSON object into its members in order; ok is false for anything else.
func fieldsOf(raw json.RawMessage) ([]field, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var fields []field
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		fields = append(fields, field{key: key, value: value})
	}
	return fields, true
}

func elementsOf(raw json.RawMessage) ([]json.RawMessage, bool) {
	if first(raw) != '[' {
		return nil, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, false
	}
	return elems, true
}

func first(raw json.RawMessage) byte {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

// nextPageKey is the member a listing names its next page in.
const nextPageKey = "next_page"

// writeTable prints a result for a person: its scalars and nested objects as "key: value" lines,
// each array of objects as a table under its key, and the next page last. Every value and key is
// escaped, because names and tag values are written by the customer's users and cloud accounts and
// a control character in one would otherwise reach the terminal.
func writeTable(w io.Writer, raw []byte) error {
	_, err := io.WriteString(w, table(raw))
	return err
}

func table(raw []byte) string {
	fields, ok := fieldsOf(raw)
	if !ok {
		return cell(raw) + "\n"
	}
	var lines strings.Builder
	type rows struct {
		key  string
		rows []json.RawMessage
	}
	var tables []rows
	nextPage := ""
	for _, f := range fields {
		if f.key == nextPageKey {
			nextPage = cell(f.value)
			continue
		}
		if elems, ok := elementsOf(f.value); ok && len(elems) > 0 && first(elems[0]) == '{' {
			tables = append(tables, rows{key: f.key, rows: elems})
			continue
		}
		for _, l := range flatten(f.key, f.value) {
			lines.WriteString(escape(l[0]) + ":\t" + l[1] + "\n")
		}
	}

	var b strings.Builder
	b.WriteString(aligned(lines.String()))
	for i, t := range tables {
		if i > 0 || lines.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(escape(t.key) + ":\n")
		b.WriteString(rowsTable(t.rows))
	}
	if nextPage != "" {
		b.WriteString("\nnext page: " + nextPage + "\n")
	}
	return b.String()
}

// aligned lines up the tab-separated columns of text.
func aligned(text string) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	// A tabwriter over a strings.Builder fails only when the builder does, which it never does.
	_, _ = tw.Write([]byte(text))
	_ = tw.Flush()
	return b.String()
}

// rowsTable prints objects as one table, a column per key any of them has, in the order the keys
// first appear; a nested object's members become columns of their own, named key.member.
func rowsTable(rows []json.RawMessage) string {
	var columns []string
	seen := map[string]bool{}
	cells := make([]map[string]string, len(rows))
	for i, row := range rows {
		cells[i] = map[string]string{}
		for _, kv := range flatten("", row) {
			if !seen[kv[0]] {
				seen[kv[0]] = true
				columns = append(columns, kv[0])
			}
			cells[i][kv[0]] = kv[1]
		}
	}
	var b strings.Builder
	header := make([]string, len(columns))
	for i, c := range columns {
		header[i] = escape(c)
	}
	b.WriteString(strings.Join(header, "\t") + "\n")
	for _, row := range cells {
		values := make([]string, len(columns))
		for i, c := range columns {
			values[i] = row[c]
		}
		b.WriteString(strings.Join(values, "\t") + "\n")
	}
	return aligned(b.String())
}

// flatten turns a value into key and cell pairs: an object into one pair per member, prefixed with
// its key, and anything else into one pair.
func flatten(key string, raw json.RawMessage) [][2]string {
	fields, ok := fieldsOf(raw)
	if !ok {
		return [][2]string{{key, cell(raw)}}
	}
	if len(fields) == 0 {
		return [][2]string{{key, ""}}
	}
	var out [][2]string
	for _, f := range fields {
		name := f.key
		if key != "" {
			name = key + "." + f.key
		}
		out = append(out, flatten(name, f.value)...)
	}
	return out
}

// cell is a value as one escaped table cell: a string unquoted, a number or boolean as written, null
// empty, a list of scalars joined with commas, and anything else as compact JSON.
func cell(raw json.RawMessage) string {
	switch first(raw) {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return escape(s)
		}
	case 'n':
		return ""
	case '[':
		if elems, ok := elementsOf(raw); ok {
			parts := make([]string, 0, len(elems))
			for _, e := range elems {
				if c := first(e); c == '{' || c == '[' {
					parts = nil
					break
				}
				parts = append(parts, cell(e))
			}
			if parts != nil || len(elems) == 0 {
				return strings.Join(parts, ", ")
			}
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return escape(string(raw))
	}
	return escape(buf.String())
}

// escape writes each control character, and each bidirectional override that could reorder what a
// terminal shows, as a \uXXXX escape.
func escape(s string) string { return escapeExcept(s, "") }

// escapeExcept escapes as escape does but for the runes in keep, such as the newlines that lay out
// indented JSON.
func escapeExcept(s, keep string) string {
	unsafe := func(r rune) bool { return unsafeRune(r) && !strings.ContainsRune(keep, r) }
	if !strings.ContainsFunc(s, unsafe) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafe(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

// errNoResult is a call that returned no content to print.
var errNoResult = errors.New("the tool returned no result")
