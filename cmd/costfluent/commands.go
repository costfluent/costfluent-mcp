package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// command is one tool as a command.
type command struct {
	name       string // the tool's name with hyphens
	tool       *mcp.Tool
	capability costfluent.Capability
	writes     bool // the tool creates or changes objects, which only an organization that lets AI agents write is served
	params     []param
}

// commandName is a tool's name as a command.
func commandName(tool string) string { return strings.ReplaceAll(tool, "_", "-") }

// loadCommands lists every tool in the registry as a command, in the registry's order. It builds a
// server for a token carrying every capability the tools need, in an organization that lets AI
// agents write, without asking the API, which Build never does, so help and flag parsing need no
// token.
func loadCommands(ctx context.Context) ([]*command, error) {
	catalog := server.Catalog()
	srv, err := server.Build(costfluent.NewClient(), everyCapability(), server.Options{Version: version})
	if err != nil {
		return nil, err
	}
	cs, done, err := connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer done()
	tools := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listing the tools: %w", err)
		}
		tools[tool.Name] = tool
	}
	commands := make([]*command, 0, len(catalog))
	for _, e := range catalog {
		tool, ok := tools[e.Name]
		if !ok {
			return nil, fmt.Errorf("the server does not list the tool %s its catalog names", e.Name)
		}
		params, err := paramsOf(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", e.Name, err)
		}
		commands = append(commands, &command{name: commandName(e.Name), tool: tool, capability: e.Capability, writes: e.Writes, params: params})
	}
	return commands, nil
}

// everyCapability is a token carrying every capability the tools need, in an organization that lets
// AI agents write: what a server built without asking the API serves, every tool.
func everyCapability() *costfluent.Me {
	me := &costfluent.Me{AgentWritesAllowed: true}
	for _, e := range server.Catalog() {
		me.Capabilities = append(me.Capabilities, e.Capability)
	}
	return me
}

// connect connects an MCP client to srv in process; done closes both ends.
func connect(ctx context.Context, srv *mcp.Server) (*mcp.ClientSession, func(), error) {
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "costfluent", Version: version}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		_ = ss.Close()
		return nil, nil, err
	}
	return cs, func() {
		_ = cs.Close()
		_ = ss.Close()
	}, nil
}

func run(ctx context.Context, c *cli) int {
	if len(c.args) == 0 {
		commands, err := loadCommands(ctx)
		if err != nil {
			return c.fail(exitFailed, err.Error())
		}
		c.warn(usageText(commands))
		return exitUsage
	}
	switch c.args[0] {
	case "--version", "-version", "version":
		return c.print(version + "\n")
	}
	commands, err := loadCommands(ctx)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	switch c.args[0] {
	case "help", "--help", "-help", "-h":
		if len(c.args) > 1 {
			switch c.args[1] {
			case "whoami":
				return c.print(whoamiHelp)
			case "mcp":
				return c.print(installHelp)
			case "estimate":
				return c.print(estimateHelp)
			case "login":
				return c.print(loginHelp)
			case "logout":
				return c.print(logoutHelp)
			}
			if cmd := find(commands, c.args[1]); cmd != nil {
				return c.print(commandHelp(cmd))
			}
			return c.usage(commands, usagef("unknown command %q", c.args[1]))
		}
		return c.print(usageText(commands))
	}
	output := outputJSON
	if c.stdoutTTY {
		output = outputTable
	}
	switch c.args[0] {
	case "whoami":
		return c.whoami(ctx, c.args[1:], output)
	case "estimate":
		return c.estimate(ctx, c.args[1:], output)
	case "login":
		return c.login(ctx, c.args[1:])
	case "logout":
		return c.logout(ctx, c.args[1:])
	case "mcp":
		if len(c.args) > 1 && c.args[1] == "install" {
			return c.install(c.args[2:])
		}
		return c.usage(commands, usagef("mcp takes one subcommand, install"))
	}
	cmd := find(commands, c.args[0])
	if cmd == nil {
		return c.usage(commands, usagef("unknown command %q", c.args[0]))
	}
	args, output, err := parseArgs(cmd.name, cmd.params, c.args[1:], output)
	if errors.Is(err, errHelp) {
		return c.print(commandHelp(cmd))
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent "+cmd.name+" --help lists its flags")
	}
	return c.call(ctx, cmd, args, output)
}

func find(commands []*command, name string) *command {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd
		}
	}
	return nil
}

// usage reports a command line naming no command this binary has.
func (c *cli) usage(commands []*command, err error) int {
	c.warn("costfluent: " + err.Error() + "\n\n" + usageText(commands))
	return exitUsage
}

// client is the API client a call uses, for the token credentials resolves and the base URL the
// environment names.
func (c *cli) client(ctx context.Context) (*costfluent.Client, error) {
	token, baseURL, err := c.credentials(ctx)
	if err != nil {
		return nil, err
	}
	return costfluent.NewClient(
		costfluent.WithAPIKey(token),
		costfluent.WithBaseURL(baseURL),
		costfluent.WithUserAgent("costfluent/"+version),
		costfluent.WithTimeout(apiAttemptTimeout),
		costfluent.WithMaxRetryAfter(maxRetryAfter),
	), nil
}

// me asks the API which token the environment carries. On failure it returns the exit status the
// failure has already been reported with.
func (c *cli) me(ctx context.Context) (*costfluent.Client, *costfluent.Me, int) {
	client, err := c.client(ctx)
	if err != nil {
		return nil, nil, c.fail(accessExit(err), err.Error())
	}
	me, err := client.GetMe(ctx)
	if err != nil {
		return nil, nil, c.failure(server.ToolError(err).Error())
	}
	return client, me, exitOK
}

// call runs cmd's tool for the environment's token and prints its result. A tool the token is not
// served is refused before the tool reads anything. A tool that needs no capability, a docs
// command, needs no token either: it reads Costfluent's public documentation and never the API.
func (c *cli) call(ctx context.Context, cmd *command, args map[string]any, output outputFormat) int {
	if cmd.capability == server.NoCapability {
		srv, err := server.Build(costfluent.NewClient(), everyCapability(), server.Options{Version: version, Docs: c.docs})
		if err != nil {
			return c.fail(exitFailed, err.Error())
		}
		return c.callTool(ctx, srv, cmd, args, output)
	}
	client, me, code := c.me(ctx)
	if code != exitOK {
		return code
	}
	if !me.Can(cmd.capability) {
		preset := "from the Read only preset"
		if cmd.writes {
			preset = "carrying it"
		}
		return c.fail(exitAccess, fmt.Sprintf("the API token %q lacks the %s capability %s needs; "+
			"create one under Settings, API tokens %s", me.TokenName, cmd.capability, cmd.name, preset))
	}
	if cmd.writes && !me.AgentWritesAllowed {
		return c.fail(exitAccess, fmt.Sprintf("%s creates or changes objects, which this organization does not let AI agents do; "+
			"an Owner can turn on %s", cmd.name, server.AgentWritesSetting))
	}
	srv, err := server.Build(client, me, server.Options{Version: version, Docs: c.docs})
	if err != nil {
		return c.fail(exitAccess, err.Error())
	}
	return c.callTool(ctx, srv, cmd, args, output)
}

// callTool calls cmd's tool on srv and prints its result.
func (c *cli) callTool(ctx context.Context, srv *mcp.Server, cmd *command, args map[string]any, output outputFormat) int {
	cs, done, err := connect(ctx, srv)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	defer done()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: cmd.tool.Name, Arguments: args})
	if err != nil {
		if ctx.Err() != nil {
			return c.fail(exitFailed, "interrupted")
		}
		return c.fail(exitFailed, err.Error())
	}
	text, err := resultText(res)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	if res.IsError {
		// The SDK refuses arguments its input schema does not take before the tool runs, which is
		// a wrong command line, as a flag the parser refuses is.
		if strings.HasPrefix(text, sdkArgumentsRefused) {
			return c.fail(exitUsage, text+"; costfluent "+cmd.name+" --help lists its flags")
		}
		return c.failure(text)
	}
	return c.writeResult([]byte(text), output)
}

// sdkArgumentsRefused starts the error the MCP SDK returns for arguments the input schema refuses.
const sdkArgumentsRefused = `validating "arguments"`

// writeResult prints a result in output's form.
func (c *cli) writeResult(raw []byte, output outputFormat) int {
	var err error
	if output == outputTable {
		err = writeTable(c.stdout, raw)
	} else {
		err = writeJSON(c.stdout, raw, c.stdoutTTY)
	}
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	return exitOK
}

// resultText is a result's JSON, or its error's text: the text content, which the server writes from
// the same bytes as the structured content but which, unlike the client's decoded structured
// content, keeps every number's digits.
func resultText(res *mcp.CallToolResult) (string, error) {
	for _, content := range res.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			return t.Text, nil
		}
	}
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		return string(raw), err
	}
	return "", errNoResult
}

// apiFailure is the ApiError a refused call carries, as server.ToolError writes it.
type apiFailure struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	TraceID string `json:"traceId"`
}

// failure reports a failed call: the ApiError JSON as is off a terminal, one line on one, or the
// error's text when it is no ApiError. 401 and 403 are access failures; the rest failed calls.
func (c *cli) failure(text string) int {
	var f apiFailure
	if err := json.Unmarshal([]byte(text), &f); err != nil || f.Status == 0 {
		return c.fail(exitFailed, text)
	}
	code := exitFailed
	if f.Status == 401 || f.Status == 403 {
		code = exitAccess
	}
	if !c.stderrTTY {
		c.warn(text + "\n")
		return code
	}
	line := fmt.Sprintf("%d", f.Status)
	if f.Code != "" {
		line += " " + f.Code
	}
	// The detail already carries the field errors it does not state itself (costfluent.APIError.Message).
	line += ": " + firstNonEmpty(f.Detail, f.Title)
	if f.TraceID != "" {
		line += " (trace " + f.TraceID + ")"
	}
	return c.fail(code, line)
}

// fail prints one line to stderr and returns code.
func (c *cli) fail(code int, msg string) int {
	c.warn("costfluent: " + escape(msg) + "\n")
	return code
}

// warn writes text to stderr. A stderr that cannot be written leaves nowhere to say so, and the exit
// status still tells.
func (c *cli) warn(text string) {
	_, _ = io.WriteString(c.stderr, text)
}

// print writes text to stdout: a success, unless stdout cannot be written.
func (c *cli) print(text string) int {
	if _, err := io.WriteString(c.stdout, text); err != nil {
		return c.fail(exitFailed, err.Error())
	}
	return exitOK
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// usageText lists the commands.
func usageText(commands []*command) string {
	var list strings.Builder
	for _, cmd := range commands {
		list.WriteString("  " + cmd.name + "\t" + title(cmd.tool) + "\n")
	}
	return `costfluent runs the Costfluent MCP server's tools from the command line.

Usage: costfluent <command> [flags]

Run 'costfluent login' to sign in through the browser, or set COSTFLUENT_API_TOKEN to an API token
(Settings, API tokens, the Read only preset), which wins over a sign-in. Set COSTFLUENT_BASE_URL only
to reach another API than https://api.costfluent.com.

Commands:
` + aligned(list.String()) + `
  login             Sign in through the browser, so commands run without COSTFLUENT_API_TOKEN
  logout            Revoke the sign-in and forget it on this machine
  whoami            Show what the API says of the token: its name, workspace and capabilities
  estimate submit   Post a pull request's Infracost estimate, from any CI
  estimate apply    Record that a pull request's change was applied, with its resources' ids
  mcp install       Print or write the entry that starts costfluent-mcp in Claude Code, Cursor,
                    Codex, VS Code or Copilot CLI

Output is JSON unless stdout is a terminal; --output json or --output table chooses.
Run 'costfluent <command> --help' for a command's flags, and 'costfluent --version' for the version.

Exit status: 0 success, 1 the call failed, 2 a wrong command line, 3 no access (no token or
sign-in, a refused base URL, a token or sign-in the API refuses or the browser denied, a token
without the capability the command needs, or a command that changes objects where the organization
does not let AI agents do that; run costfluent login again when a sign-in ended), 4 a
threshold you set was exceeded (estimate submit --fail-above).
`
}

// commandHelp describes one command and its flags.
func commandHelp(cmd *command) string {
	synopsis := "costfluent " + cmd.name
	for _, p := range cmd.params {
		switch {
		case p.required && p.kind == kindBool:
			synopsis += " --" + p.flag
		case p.required:
			synopsis += " --" + p.flag + " <" + placeholder(p.kind) + ">"
		}
	}
	var flags strings.Builder
	for _, p := range cmd.params {
		flagText := "--" + p.flag
		if p.kind != kindBool {
			flagText += " <" + placeholder(p.kind) + ">"
		}
		desc := p.description
		if p.required {
			desc = "required: " + desc
		}
		if p.kind == kindStrings {
			desc += " (repeat the flag for each)"
		}
		flags.WriteString("  " + flagText + "\t" + desc + "\n")
	}
	flags.WriteString("  --output <json|table>\tJSON unless stdout is a terminal\n")
	return "Usage: " + synopsis + " [flags]\n\n" + cmd.tool.Description + "\n\nFlags:\n" + aligned(flags.String())
}

func placeholder(k kind) string {
	switch k {
	case kindInteger:
		return "integer"
	case kindNumber:
		return "number"
	case kindBool:
		return "true|false"
	}
	return "string"
}

func title(tool *mcp.Tool) string {
	if tool.Annotations != nil && tool.Annotations.Title != "" {
		return tool.Annotations.Title
	}
	return tool.Title
}
