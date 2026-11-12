package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const costsResult = `{"rows":[{"date":"2026-10-01","cost":"12345678901234.12345678","dimensions":{"ServiceName":"AmazonEC2"}},` +
	`{"date":"2026-10-02","cost":"0.00000001","dimensions":{"Region":"eu-central-1"},"percent":12.345678901234567890}],` +
	`"meta":{"totalCost":"25.00000000","currency":"USD","totalRecords":2},"next_page":2}`

func TestJSONKeepsTheResultAsWritten(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, []byte(costsResult), false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`"cost": "12345678901234.12345678"`, `"percent": 12.345678901234567890`, `"next_page": 2`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON lacks %s:\n%s", want, out)
		}
	}
	if strings.Index(out, `"rows"`) > strings.Index(out, `"meta"`) {
		t.Errorf("the keys were reordered:\n%s", out)
	}
	if err := writeJSON(&buf, []byte("not json"), false); err == nil {
		t.Error("invalid JSON was printed")
	}
}

func TestATablePerArray(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTable(&buf, []byte(costsResult)); err != nil {
		t.Fatal(err)
	}
	want := "meta.totalCost:     25.00000000\n" +
		"meta.currency:      USD\n" +
		"meta.totalRecords:  2\n" +
		"\n" +
		"rows:\n" +
		"date        cost                     dimensions.ServiceName  dimensions.Region  percent\n" +
		"2026-10-01  12345678901234.12345678  AmazonEC2                                  \n" +
		"2026-10-02  0.00000001                                       eu-central-1       12.345678901234567890\n" +
		"\n" +
		"next page: 2\n"
	if buf.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestATableOfSeveralArraysAndNestedValues(t *testing.T) {
	var buf bytes.Buffer
	raw := `{"currency":"USD","data":[],"totals":[{"provider":"aws","keys":["a","b"],"nested":[{"x":1}],"empty":{}}],` +
		`"breakdown":[{"cost":"1"}],"note":null,"tags":["x","y"]}`
	if err := writeTable(&buf, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"currency:", "data:      \n", "tags:      x, y", "totals:\n", "breakdown:\n",
		"a, b", `[{"x":1}]`} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "next page") {
		t.Errorf("a last page names a next page:\n%s", out)
	}
}

func TestTablesEscapeControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	raw := `{"items":[{"name":"evil\u001b]0;pwned\u0007","tag\tkey":"line\nbreak","rtl":"a\u202eb"}],"title":"x\u001b[2J"}`
	if err := writeTable(&buf, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{"\x1b", "\x07", "\u202e", "line\nbreak", "tag\tkey"} {
		if strings.Contains(out, bad) {
			t.Errorf("table carries %q unescaped:\n%q", bad, out)
		}
	}
	for _, want := range []string{`evil\u001b]0;pwned\u0007`, `tag\u0009key`, `line\u000abreak`, `a\u202eb`, `x\u001b[2J`} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %s:\n%s", want, out)
		}
	}
}

func TestATableOfAScalarResult(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTable(&buf, []byte(`"just\u001btext"`)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "just\\u001btext\n" {
		t.Errorf("got %q", buf.String())
	}
}

// Go's JSON encoder escapes C0 controls but writes C1 controls, DEL and bidirectional overrides as
// they are, and a terminal acts on them.
const unsafeJSON = "{\"name\":\"a\u0085b\u202ec\u007fd\u009b2J\",\"note\":\"café\"}"

func TestJSONOnATerminalEscapesWhatATerminalActsOn(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, []byte(unsafeJSON), true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.ContainsAny(out, "\u0085\u202e\u007f\u009b") {
		t.Errorf("a control reached the terminal: %q", out)
	}
	if !strings.Contains(out, `"a\u0085b\u202ec\u007fd\u009b2J"`) || !strings.Contains(out, `"café"`) {
		t.Errorf("JSON on a terminal: %s", out)
	}
	var decoded map[string]string
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil || decoded["name"] != "a\u0085b\u202ec\u007fd\u009b2J" {
		t.Errorf("the escaped JSON decodes to %q, %v", decoded["name"], err)
	}

	// Off a terminal the bytes are the tool's, for a program to read.
	buf.Reset()
	if err := writeJSON(&buf, []byte(unsafeJSON), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "a\u0085b\u202ec\u007fd\u009b2J") {
		t.Errorf("JSON off a terminal changed: %q", buf.String())
	}
}
