package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// kind is how a tool argument is written on the command line.
type kind int

const (
	kindString kind = iota
	kindInteger
	kindNumber
	kindBool
	kindStrings // an array of strings, one flag per element
)

// param is one property of a tool's input schema as a flag.
type param struct {
	name        string // the property, as the tool takes it
	flag        string // the property with hyphens
	kind        kind
	required    bool
	description string
}

// jsonSchema is the part of an input schema the flags read.
type jsonSchema struct {
	Type        json.RawMessage       `json:"type"`
	Description string                `json:"description"`
	Properties  map[string]jsonSchema `json:"properties"`
	Required    []string              `json:"required"`
	Items       *jsonSchema           `json:"items"`
}

// types is the schema's type, a string or a list, without "null": a nullable boolean is a boolean
// sent only when its flag is given.
func (s jsonSchema) types() ([]string, error) {
	var one string
	if err := json.Unmarshal(s.Type, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(s.Type, &many); err != nil {
		return nil, fmt.Errorf("type %s is neither a string nor a list", s.Type)
	}
	return slices.DeleteFunc(many, func(t string) bool { return t == "null" }), nil
}

// reservedFlags are the flags every command takes besides its tool's.
var reservedFlags = []string{"output", "help", "h"}

// paramsOf turns a tool's input schema into its flags, required ones first, each group by name: the
// schema an MCP client receives is a map, which keeps no order of its own.
func paramsOf(inputSchema any) ([]param, error) {
	raw, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, err
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	params := make([]param, 0, len(s.Properties))
	for name, prop := range s.Properties {
		k, err := kindOf(prop)
		if err != nil {
			return nil, fmt.Errorf("property %s: %w", name, err)
		}
		p := param{
			name: name, flag: strings.ReplaceAll(name, "_", "-"), kind: k,
			required: slices.Contains(s.Required, name), description: prop.Description,
		}
		if slices.Contains(reservedFlags, p.flag) {
			return nil, fmt.Errorf("property %s would be the reserved flag --%s", name, p.flag)
		}
		params = append(params, p)
	}
	slices.SortFunc(params, func(a, b param) int {
		if a.required != b.required {
			if a.required {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	return params, nil
}

func kindOf(s jsonSchema) (kind, error) {
	types, err := s.types()
	if err != nil {
		return 0, err
	}
	if len(types) != 1 {
		return 0, fmt.Errorf("type %v is not one type", types)
	}
	switch types[0] {
	case "string":
		return kindString, nil
	case "integer":
		return kindInteger, nil
	case "number":
		return kindNumber, nil
	case "boolean":
		return kindBool, nil
	case "array":
		if s.Items != nil {
			if items, err := s.Items.types(); err == nil && slices.Equal(items, []string{"string"}) {
				return kindStrings, nil
			}
		}
		return 0, errors.New("an array of anything but strings has no flag form")
	}
	return 0, fmt.Errorf("type %s has no flag form", types[0])
}

// usageError is a command line the command cannot run: exit status 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// outputFormat is how a result is printed.
type outputFormat string

const (
	outputJSON  outputFormat = "json"
	outputTable outputFormat = "table"
)

func (o *outputFormat) String() string { return string(*o) }

func (o *outputFormat) Set(s string) error {
	switch outputFormat(s) {
	case outputJSON, outputTable:
		*o = outputFormat(s)
		return nil
	}
	return fmt.Errorf("--output takes json or table, not %q", s)
}

// argValue sets one tool argument from its flag; only a flag given sets one, so an argument the
// user left out stays out and the tool applies its own default.
type argValue struct {
	p    param
	args map[string]any
}

func (v *argValue) String() string { return "" }

func (v *argValue) IsBoolFlag() bool { return v.p.kind == kindBool }

func (v *argValue) Set(s string) error {
	switch v.p.kind {
	case kindString:
		v.args[v.p.name] = s
	case kindInteger:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("--%s takes a whole number, not %q", v.p.flag, s)
		}
		v.args[v.p.name] = n
	case kindNumber:
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return fmt.Errorf("--%s takes a number, not %q", v.p.flag, s)
		}
		v.args[v.p.name] = json.Number(s)
	case kindBool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("--%s takes true or false, not %q", v.p.flag, s)
		}
		v.args[v.p.name] = b
	case kindStrings:
		list, _ := v.args[v.p.name].([]string)
		v.args[v.p.name] = append(list, s)
	}
	return nil
}

// errHelp asks for the command's help.
var errHelp = flag.ErrHelp

// parseArgs reads a command's flags into the tool's arguments and the output format, refusing an
// unknown flag, a malformed value, a positional argument and a missing required flag.
func parseArgs(name string, params []param, argv []string, output outputFormat) (map[string]any, outputFormat, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	args := map[string]any{}
	for _, p := range params {
		fs.Var(&argValue{p: p, args: args}, p.flag, p.description)
	}
	fs.Var(&output, "output", "json or table")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, "", errHelp
		}
		return nil, "", &usageError{msg: unprefixed(err)}
	}
	if fs.NArg() > 0 {
		return nil, "", usagef("unexpected argument %q: every argument is a --flag", fs.Arg(0))
	}
	var missing []string
	for _, p := range params {
		if _, ok := args[p.name]; p.required && !ok {
			missing = append(missing, "--"+p.flag)
		}
	}
	if len(missing) > 0 {
		return nil, "", usagef("missing %s", strings.Join(missing, ", "))
	}
	return args, output, nil
}

// unprefixed rewrites the flag package's errors, which name a flag with one dash and wrap the
// value's own message, into the two-dash form the help shows.
func unprefixed(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		return "unknown flag --" + strings.TrimPrefix(rest, "-")
	}
	if _, after, ok := strings.Cut(msg, ": "); ok && strings.HasPrefix(msg, "invalid ") {
		return after
	}
	return strings.ReplaceAll(msg, " -", " --")
}
