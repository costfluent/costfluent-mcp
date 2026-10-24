// Package server is the Costfluent MCP server: read tools over the Public API, each listed only
// when the API token the server was started with carries the capability the tool's call needs.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
)

// New asks the API which token the client carries and returns a server holding the tools that
// token may call. A token no tool can use is an error naming the capabilities the tools need,
// because an assistant shows no log and a server with no tools only looks broken. A workspace token's client defaults to its own workspace, so a tool called
// without a workspace reads the token's.
func New(ctx context.Context, client *costfluent.Client, version string) (*mcp.Server, *costfluent.Me, error) {
	me, err := client.GetMe(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the API token's capabilities: %w", err)
	}
	if me.WorkspaceID != nil {
		client = client.Workspace(*me.WorkspaceID)
	}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "costfluent", Title: "Costfluent", Version: version},
		&mcp.ServerOptions{Instructions: instructions(me)},
	)
	h := &handlers{client: client, me: me}
	var needed []costfluent.Capability
	served := 0
	for _, t := range h.tools() {
		if !slices.Contains(needed, t.capability) {
			needed = append(needed, t.capability)
		}
		if me.Can(t.capability) {
			t.add(srv)
			served++
		}
	}
	if served == 0 {
		ids := make([]string, len(needed))
		for i, c := range needed {
			ids[i] = string(c)
		}
		return nil, nil, fmt.Errorf("the API token %q carries none of the capabilities the tools need: %s; "+
			"create one under Settings, API tokens from the Read only preset", me.TokenName, strings.Join(ids, ", "))
	}
	return srv, me, nil
}

// ToolNames lists the tools a token carrying me's capabilities is served, in registration order.
func ToolNames(me *costfluent.Me) []string {
	h := &handlers{me: me}
	var names []string
	for _, t := range h.tools() {
		if me.Can(t.capability) {
			names = append(names, t.name)
		}
	}
	return names
}

func instructions(me *costfluent.Me) string {
	scope := "an organization token: workspace tools need a workspace_id, which list_workspaces names"
	if me.WorkspaceID != nil {
		scope = "a workspace token for workspace " + *me.WorkspaceID + ", which every tool reads unless told otherwise; other workspaces are refused"
	}
	return "Read-only access to Costfluent cloud cost data through the Public API. This server runs with " +
		scope + ". Amounts are decimal strings in the currency the response names. Dates are YYYY-MM-DD."
}

// toolDef binds a tool to the capability its API call needs.
type toolDef struct {
	name       string
	capability costfluent.Capability
	add        func(*mcp.Server)
}

// readOnly marks a tool that reads and changes nothing.
func readOnly(title string) *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
}

// apiError is the Public API's ApiError shape, which a refused call passes to the client as is.
type apiError struct {
	Status    int                     `json:"status"`
	Code      string                  `json:"code,omitempty"`
	Title     string                  `json:"title,omitempty"`
	Detail    string                  `json:"detail,omitempty"`
	Retryable bool                    `json:"retryable"`
	TraceID   string                  `json:"traceId,omitempty"`
	Errors    []costfluent.FieldError `json:"errors,omitempty"`
}

// toolError is a failed call as the client reads it: the API's ApiError when the API refused the
// call, otherwise the error's text. Neither carries the token, which travels only in a request header.
func toolError(err error) error {
	var ae *costfluent.APIError
	if errors.As(err, &ae) {
		body, mErr := json.Marshal(apiError{
			Status: ae.StatusCode, Code: ae.Code, Title: ae.Title, Detail: ae.Message(),
			Retryable: ae.Retryable || costfluent.IsRateLimited(err), TraceID: ae.TraceID, Errors: ae.Errors,
		})
		if mErr == nil {
			return errors.New(string(body))
		}
	}
	return err
}

// handle adapts a call to a typed tool handler that reports a failed call as a tool error, which
// the assistant reads and can act on, rather than a protocol error.
func handle[In, Out any](call func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := call(ctx, in)
		if err != nil {
			// The failure goes back as the handler's error, not as an IsError result: the SDK
			// marshals Out into structuredContent whenever the error is nil, IsError or not, and a
			// zero Out there reads as an empty success. On an error it builds the IsError result
			// from Error() alone.
			var zero Out
			return nil, zero, toolError(err)
		}
		return nil, out, nil
	}
}
