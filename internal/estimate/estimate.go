// Package estimate runs the cost estimate Action's two jq programs under gojq:
// infracost-to-estimate.jq turns Infracost's JSON into the body of POST /v1/cost-estimates, and
// terraform-apply.jq collects the body of POST /v1/cost-estimates/apply from the applied plan and
// the state after it. programs/ holds both as the Action ships them, and testdata/ the Action's
// fixtures and their golden outputs, so the Action under jq and the costfluent CLI under gojq
// convert with one program and are held to one output. Both are copies: a program changes in the
// Action, and an edit made here is overwritten.
package estimate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/itchyny/gojq"
)

//go:embed programs/infracost-to-estimate.jq
var estimateProgram string

//go:embed programs/terraform-apply.jq
var applyProgram string

// Args are the named arguments the Action passes infracost-to-estimate.jq beside Infracost's JSON.
// The program treats an empty one as absent, as the Action passes an unset input.
type Args struct {
	WorkspaceID   string
	Repository    string
	PullRequest   string
	Commit        string
	URL           string
	SourceVersion string
}

// Applied is what terraform-apply.jq gives: the apply's request body, and the addresses the plan
// creates or replaces that the state holds no id for, which the body leaves out.
type Applied struct {
	Body    json.RawMessage `json:"body"`
	Missing []string        `json:"missing"`
}

// The refusals Apply makes before running the program, as the Action makes them.
var (
	ErrNotAPlan  = errors.New("not `terraform show -json` of a saved plan")
	ErrNotAState = errors.New("not `terraform show -json` of the state")
)

type programs struct {
	estimate, apply *gojq.Code
}

// compiled compiles both programs once, on first use.
var compiled = sync.OnceValues(func() (*programs, error) {
	estimate, err := compile("infracost-to-estimate.jq", estimateProgram, "$ARGS")
	if err != nil {
		return nil, err
	}
	apply, err := compile("terraform-apply.jq", applyProgram, "$plan", "$state", "$workspaceId", "$repository", "$pullRequest")
	if err != nil {
		return nil, err
	}
	return &programs{estimate: estimate, apply: apply}, nil
})

func compile(name, source string, variables ...string) (*gojq.Code, error) {
	query, err := gojq.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	code, err := gojq.Compile(query, gojq.WithVariables(variables))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return code, nil
}

// Decode reads data as one JSON object, keeping every number's digits, as jq reads a file.
func Decode(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not JSON: more follows the first value")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return object, nil
}

// Estimate converts Infracost's JSON into the estimate document, reading the Terraform plan too
// when plan is not nil. A refused input returns the program's message, as the Action prints it.
func Estimate(infracost, plan map[string]any, args Args) (json.RawMessage, error) {
	p, err := compiled()
	if err != nil {
		return nil, err
	}
	named := map[string]any{
		"workspaceId":   args.WorkspaceID,
		"repository":    args.Repository,
		"pullRequest":   args.PullRequest,
		"commit":        args.Commit,
		"url":           args.URL,
		"sourceVersion": args.SourceVersion,
	}
	if plan != nil {
		// The Action passes the plan with --slurpfile, which makes it a one-element array.
		named["terraformPlan"] = []any{plan}
	}
	return first(p.estimate.Run(infracost, map[string]any{"named": named, "positional": []any{}}))
}

// Apply collects the apply's body from the plan that was applied and the state after it.
func Apply(plan, state map[string]any, workspaceID, repository string, pullRequest int) (*Applied, error) {
	if !has(plan, "resource_changes") && !has(plan, "planned_values") {
		return nil, ErrNotAPlan
	}
	if has(state, "resource_changes") || has(state, "planned_values") || !has(state, "format_version") {
		return nil, ErrNotAState
	}
	p, err := compiled()
	if err != nil {
		return nil, err
	}
	raw, err := first(p.apply.Run(nil, []any{plan}, []any{state}, workspaceID, repository, pullRequest))
	if err != nil {
		return nil, err
	}
	var applied Applied
	if err := json.Unmarshal(raw, &applied); err != nil {
		return nil, fmt.Errorf("terraform-apply.jq gave no body and missing list: %w", err)
	}
	return &applied, nil
}

func has(object map[string]any, key string) bool {
	_, ok := object[key]
	return ok
}

// first is a program's one output as JSON, or the error it raised.
func first(iter gojq.Iter) (json.RawMessage, error) {
	value, ok := iter.Next()
	if !ok {
		return nil, errors.New("the program gave no output")
	}
	if err, ok := value.(error); ok {
		// error("...") raises its message as the value; jq prints it after its own prefix.
		var valueErr gojq.ValueError
		if errors.As(err, &valueErr) {
			if message, ok := valueErr.Value().(string); ok {
				return nil, errors.New(message)
			}
		}
		return nil, err
	}
	return gojq.Marshal(value)
}
