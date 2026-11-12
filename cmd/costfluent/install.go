package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/costfluent/costfluent-mcp/internal/config"
)

// serverName is the name every client's entry gives the server.
const serverName = "costfluent"

// mcpBinary is the stdio server the entries start.
const mcpBinary = "costfluent-mcp"

// The clients mcp install writes for, as each one's docs described its file on 2026-11-08, and
// Copilot CLI's on 2026-11-09.
const (
	clientClaudeCode = "claude-code"
	clientCursor     = "cursor"
	clientCodex      = "codex"
	clientVSCode     = "vscode"
	clientCopilot    = "copilot"
)

var clientNames = []string{clientClaudeCode, clientCursor, clientCodex, clientVSCode, clientCopilot}

// vscodeInputID is the prompt VS Code asks the token with.
const vscodeInputID = "costfluent-api-token"

// mcpClient is one client's configuration file and its entry for the server. An entry names the
// variable the client reads the token from, or VS Code's password prompt; it holds the token itself
// only when the user asks with --embed-token.
type mcpClient struct {
	// userFile and projectFile are the file's path under the home directory and under the working
	// directory; empty when the client keeps no such file. Claude Code's user file is
	// ~/.claude.json, which holds the client's own state, so mcp install never writes it.
	userFile, projectFile string
	// serversKey is the JSON member that maps server names to entries; empty for Codex's TOML.
	serversKey string
	entry      func(command string) jsonEntry
}

// jsonEntry is a JSON client's entry for the server, its members in the order its docs write them.
type jsonEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Tools   []string          `json:"tools,omitempty"`
}

// tokenFrom is an entry's environment: the token read from ref.
func tokenFrom(ref string) map[string]string {
	return map[string]string{config.EnvAPIToken: ref}
}

var mcpClients = map[string]mcpClient{
	clientClaudeCode: {projectFile: ".mcp.json", serversKey: "mcpServers", entry: func(command string) jsonEntry {
		return jsonEntry{Type: "stdio", Command: command, Args: []string{}, Env: tokenFrom("${" + config.EnvAPIToken + "}")}
	}},
	clientCursor: {userFile: ".cursor/mcp.json", projectFile: ".cursor/mcp.json", serversKey: "mcpServers", entry: func(command string) jsonEntry {
		return jsonEntry{Command: command, Args: []string{}, Env: tokenFrom("${env:" + config.EnvAPIToken + "}")}
	}},
	clientCodex: {userFile: ".codex/config.toml", projectFile: ".codex/config.toml"},
	clientVSCode: {projectFile: ".vscode/mcp.json", serversKey: "servers", entry: func(command string) jsonEntry {
		return jsonEntry{Type: "stdio", Command: command, Args: []string{}, Env: tokenFrom("${input:" + vscodeInputID + "}")}
	}},
	clientCopilot: {userFile: ".copilot/mcp-config.json", serversKey: "mcpServers", entry: func(command string) jsonEntry {
		return jsonEntry{Type: "local", Command: command, Args: []string{}, Env: tokenFrom("${" + config.EnvAPIToken + "}"), Tools: []string{"*"}}
	}},
}

// vscodeInput is the password prompt VS Code shows for the token.
var vscodeInput = struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Description string `json:"description"`
	Password    bool   `json:"password"`
}{Type: "promptString", ID: vscodeInputID, Description: "Costfluent API token", Password: true}

// installOptions is the mcp install command line.
type installOptions struct {
	client     string
	project    bool
	write      bool
	force      bool
	embedToken bool
}

func parseInstall(argv []string) (installOptions, error) {
	var o installOptions
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.client, "client", "", "")
	fs.BoolVar(&o.project, "project", false, "")
	fs.BoolVar(&o.write, "write", false, "")
	fs.BoolVar(&o.force, "force", false, "")
	fs.BoolVar(&o.embedToken, "embed-token", false, "")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, errHelp
		}
		return o, &usageError{msg: unprefixed(err)}
	}
	if fs.NArg() > 0 {
		return o, usagef("unexpected argument %q", fs.Arg(0))
	}
	if !slices.Contains(clientNames, o.client) {
		return o, usagef("--client takes one of %s", strings.Join(clientNames, ", "))
	}
	if o.project && mcpClients[o.client].projectFile == "" {
		return o, usagef("%s keeps no project file; drop --project", o.client)
	}
	// Printed, the token would reach the terminal's scrollback and any log of the session.
	if o.embedToken && !o.write {
		return o, usagef("--embed-token writes the token into the file and never prints it; add --write")
	}
	return o, nil
}

const installHelp = `Usage: costfluent mcp install --client <client> [--project] [--write [--force] [--embed-token]]

Prints the entry that starts costfluent-mcp in an MCP client, or with --write adds it to the
client's configuration file. The entry does not hold the token: the client reads it from
COSTFLUENT_API_TOKEN, or VS Code prompts for it. With --embed-token the entry holds the token from
COSTFLUENT_API_TOKEN, for a client started where that variable is not set, and the file is made
readable by its owner alone.

Clients and their files:
  claude-code  .mcp.json in the working directory
  cursor       ~/.cursor/mcp.json, or .cursor/mcp.json with --project
  codex        ~/.codex/config.toml, or .codex/config.toml with --project
  vscode       .vscode/mcp.json in the working directory
  copilot      ~/.copilot/mcp-config.json, or mcp-config.json in COPILOT_HOME

Flags:
  --client <client>  the client: claude-code, cursor, codex, vscode or copilot
  --project          the project's file in the working directory rather than the user's
  --write            add the entry to the file, keeping every other server in it
  --force            with --write, replace an existing costfluent entry
  --embed-token      with --write, write the token itself into the entry; keep that file out of
                     version control and revoke the token when you stop using the assistant
`

// install prints the entry for o.client, or adds it to the client's file.
func (c *cli) install(argv []string) int {
	o, err := parseInstall(argv)
	if errors.Is(err, errHelp) {
		return c.print(installHelp)
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent mcp install --help lists its flags")
	}
	token := ""
	if o.embedToken {
		if token = strings.TrimSpace(c.getenv(config.EnvAPIToken)); token == "" {
			return c.fail(exitAccess, config.EnvAPIToken+" is not set: --embed-token writes that token into the file")
		}
	}
	command := c.mcpCommand()
	client := mcpClients[o.client]
	path, err := c.configPath(o, client)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	if !o.write {
		text, err := merged(nil, o.client, command, false, "")
		if err != nil {
			return c.fail(exitFailed, err.Error())
		}
		c.warn("costfluent: add this to " + path + ", or run again with --write\n")
		return c.print(string(text))
	}
	current, err := os.ReadFile(path) //nolint:gosec // the path is the client's own file, built from a fixed name
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.fail(exitFailed, err.Error())
	}
	text, err := merged(current, o.client, command, o.force, token)
	if err != nil {
		return c.fail(exitFailed, path+": "+err.Error())
	}
	if err := writeFileAtomic(path, text, token != ""); err != nil {
		return c.fail(exitFailed, err.Error())
	}
	if token != "" {
		c.warn("costfluent: " + path + " now holds the API token, readable by you alone; keep it out of version control, " +
			"and revoke the token under Settings, API tokens when you stop using the assistant\n")
	}
	return c.print("costfluent: added " + serverName + " to " + path + "\n")
}

// configPath is the file o names for client.
func (c *cli) configPath(o installOptions, client mcpClient) (string, error) {
	if o.client == clientCopilot {
		if home := c.getenv("COPILOT_HOME"); home != "" {
			return filepath.Join(home, "mcp-config.json"), nil
		}
	}
	if client.userFile == "" || o.project {
		wd, err := c.workingDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(wd, filepath.FromSlash(client.projectFile)), nil
	}
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(client.userFile)), nil
}

// mcpCommand is the costfluent-mcp an entry starts: the one beside this executable, else the one
// on PATH, else the bare name, with a warning. When the one beside this executable is also the
// one PATH finds, the PATH entry is named, because a package manager links it there from a
// versioned directory the next upgrade removes.
func (c *cli) mcpCommand() string {
	var beside string
	if exe, err := c.executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), mcpBinary+filepath.Ext(exe))
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			beside = candidate
		}
	}
	onPath, err := c.lookPath(mcpBinary)
	if err == nil {
		if abs, err := filepath.Abs(onPath); err == nil {
			onPath = abs
		}
	} else {
		onPath = ""
	}
	switch {
	case beside != "" && onPath != "" && sameFile(beside, onPath):
		return onPath
	case beside != "":
		return beside
	case onPath != "":
		return onPath
	}
	c.warn("costfluent: warning: " + mcpBinary + " is neither beside this executable nor on PATH; the entry names it bare\n")
	return mcpBinary
}

func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// merged is current, a client's file, with the server's entry added. Every other member and server
// keeps its place and value. An existing entry is refused unless force replaces it, and a file
// that is not plain JSON (VS Code and Cursor accept comments) is refused whatever force says,
// because rewriting it would drop what it holds. A token, when given, is written into the entry in
// place of the client's reference to the variable, and VS Code then needs no prompt.
func merged(current []byte, client, command string, force bool, token string) ([]byte, error) {
	if client == clientCodex {
		return mergedTOML(current, command, force, token)
	}
	spec := mcpClients[client]
	value := spec.entry(command)
	if token != "" {
		value.Env = tokenFrom(token)
	}
	entry, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	top := []field{}
	if len(bytes.TrimSpace(current)) > 0 {
		fields, ok := fieldsOf(current)
		if !ok || !json.Valid(current) {
			return nil, errors.New("not plain JSON (comments or trailing commas, perhaps), so it was left unchanged; add the entry by hand")
		}
		top = fields
	}
	servers := []field{}
	at := -1
	for i, f := range top {
		if f.key != spec.serversKey {
			continue
		}
		at = i
		var ok bool
		if servers, ok = fieldsOf(f.value); !ok {
			return nil, fmt.Errorf("%s is not an object, so the file was left unchanged", spec.serversKey)
		}
	}
	replaced := false
	for i, s := range servers {
		if s.key == serverName {
			if !force {
				return nil, errors.New("already has a " + serverName + " server; --force replaces it")
			}
			servers[i].value = entry
			replaced = true
		}
	}
	if !replaced {
		servers = append(servers, field{key: serverName, value: entry})
	}
	if at < 0 {
		top = append(top, field{key: spec.serversKey})
		at = len(top) - 1
	}
	top[at].value = encodeObject(servers)
	if client == clientVSCode && token == "" {
		if top, err = withVSCodeInput(top); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, encodeObject(top), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// withVSCodeInput adds the token prompt to VS Code's inputs unless one with its id is there.
func withVSCodeInput(top []field) ([]field, error) {
	input, err := json.Marshal(vscodeInput)
	if err != nil {
		return nil, err
	}
	for i, f := range top {
		if f.key != "inputs" {
			continue
		}
		elems, ok := elementsOf(f.value)
		if !ok {
			return nil, errors.New("inputs is not a list, so the file was left unchanged")
		}
		for _, e := range elems {
			var in struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(e, &in) == nil && in.ID == vscodeInputID {
				return top, nil
			}
		}
		top[i].value, err = json.Marshal(append(elems, input))
		return top, err
	}
	list, err := json.Marshal([]json.RawMessage{input})
	return append([]field{{key: "inputs", value: list}}, top...), err
}

// encodeObject writes fields as one JSON object, in their order.
func encodeObject(fields []field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(f.key)
		b.Write(key)
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// codexTable is the table header of the server's entry in Codex's config.toml.
const codexTable = "[mcp_servers." + serverName + "]"

// codexUnchanged ends every refusal of a Codex file.
const codexUnchanged = "so it was left unchanged; add the entry by hand"

// mergedTOML appends the server's table to Codex's config.toml. With force, an existing
// [mcp_servers.costfluent] table and its subtables are removed first, up to the next other table,
// whose leading comments stay with it; without it, an existing one is refused. An entry written as
// a key (an inline table or a dotted key) and a file whose multi-line string never closes are
// refused whatever force says: the line-level merge cannot replace them, and appending a table would
// define the server twice, which Codex refuses to load.
func mergedTOML(current []byte, command string, force bool, token string) ([]byte, error) {
	quoted, err := tomlQuote(command)
	if err != nil {
		return nil, err
	}
	envVar, err := tomlQuote(config.EnvAPIToken)
	if err != nil {
		return nil, err
	}
	env := "env_vars = [" + envVar + "]\n"
	if token != "" {
		value, err := tomlQuote(token)
		if err != nil {
			return nil, err
		}
		env = "env = { " + config.EnvAPIToken + " = " + value + " }\n"
	}
	entry := codexTable + "\n" + "command = " + quoted + "\n" + "args = []\n" + env

	lines, err := scanTOMLChecked(string(current))
	if err != nil {
		return nil, fmt.Errorf("%w, %s", err, codexUnchanged)
	}
	var kept, removed []string
	inEntry := false
	for _, line := range lines {
		switch line.kind {
		case tomlHeader:
			wasInEntry := inEntry
			inEntry = isCodexEntry(line.path)
			if inEntry {
				if !force {
					return nil, errors.New("already has a " + serverName + " server; --force replaces it")
				}
			} else if wasInEntry {
				// The comments that end the removed table lead into this one, with the blank lines
				// between them, unless a blank line already ends what is kept or nothing is kept yet.
				trivia := trailingTrivia(&removed)
				for len(trivia) > 0 && strings.TrimSpace(trivia[0]) == "" &&
					(len(kept) == 0 || strings.TrimSpace(kept[len(kept)-1]) == "") {
					trivia = trivia[1:]
				}
				kept = append(kept, trivia...)
			}
		case tomlKey:
			if !inEntry && isCodexEntry(append(append([]string{}, line.table...), line.path...)) {
				return nil, errors.New("defines " + serverName + " under mcp_servers as a key rather than a " +
					codexTable + " table, " + codexUnchanged)
			}
		}
		if inEntry {
			removed = append(removed, line.text)
		} else {
			kept = append(kept, line.text)
		}
	}
	text := strings.TrimRight(strings.Join(kept, ""), "\n")
	if text != "" {
		text += "\n\n"
	}
	merged := text + entry
	if n := countCodexTables(merged); n != 1 {
		return nil, fmt.Errorf("the merge would define %d %s tables, %s", n, codexTable, codexUnchanged)
	}
	return []byte(merged), nil
}

// trailingTrivia takes the blank and comment lines that end removed off it and returns them.
func trailingTrivia(removed *[]string) []string {
	lines := *removed
	start := len(lines)
	for start > 0 {
		trimmed := strings.TrimSpace(lines[start-1])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		start--
	}
	trivia := append([]string{}, lines[start:]...)
	*removed = nil
	return trivia
}

// countCodexTables counts the [mcp_servers.costfluent] headers text holds outside strings.
func countCodexTables(text string) int {
	n := 0
	for _, line := range scanTOML(text) {
		if line.kind == tomlHeader && isCodexEntry(line.path) && len(line.path) == 2 {
			n++
		}
	}
	return n
}

// writeFileAtomic writes data to path through a temporary file in the same directory and a rename,
// so a client never reads half a file. An existing file keeps its permissions, unless private, which
// a file holding the token is, makes it readable by its owner alone; a new one is created so.
func writeFileAtomic(path string, data []byte, private bool) error {
	// A config file linked from a dotfiles repository is written through the link, so the link and
	// the repository's copy stay one file.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if info, err := os.Stat(path); err == nil && !private {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
