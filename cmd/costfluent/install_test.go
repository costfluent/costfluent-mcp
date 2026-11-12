package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// installEnv is a machine for mcp install: a home and a working directory under t's temporary
// directory, costfluent-mcp on PATH at a fixed path, and a token in the environment, which no entry
// may carry.
type installEnv struct {
	home, wd string
	env      map[string]string
	exe      string
	onPath   string
}

const fixedMCP = "/opt/costfluent/bin/costfluent-mcp"

func newInstallEnv(t *testing.T) *installEnv {
	root := t.TempDir()
	e := &installEnv{
		home: filepath.Join(root, "home"), wd: filepath.Join(root, "project"),
		env:    map[string]string{"COSTFLUENT_API_TOKEN": tokenSecret},
		onPath: fixedMCP,
	}
	for _, dir := range []string{e.home, e.wd} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *installEnv) run(t *testing.T, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &cli{
		args: append([]string{"mcp", "install"}, args...), getenv: func(k string) string { return e.env[k] },
		stdout: &stdout, stderr: &stderr,
		executable: func() (string, error) {
			if e.exe == "" {
				return "", errors.New("unknown")
			}
			return e.exe, nil
		},
		lookPath: func(string) (string, error) {
			if e.onPath == "" {
				return "", errors.New("not found")
			}
			return e.onPath, nil
		},
		homeDir:    func() (string, error) { return e.home, nil },
		workingDir: func() (string, error) { return e.wd, nil },
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "install", name)
	if *update {
		write(t, path, got)
	}
	if want := read(t, path); got != want {
		t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
	}
}

// files is where each client's file lands without --project.
var files = map[string]func(e *installEnv) string{
	clientClaudeCode: func(e *installEnv) string { return filepath.Join(e.wd, ".mcp.json") },
	clientCursor:     func(e *installEnv) string { return filepath.Join(e.home, ".cursor", "mcp.json") },
	clientCodex:      func(e *installEnv) string { return filepath.Join(e.home, ".codex", "config.toml") },
	clientVSCode:     func(e *installEnv) string { return filepath.Join(e.wd, ".vscode", "mcp.json") },
	clientCopilot:    func(e *installEnv) string { return filepath.Join(e.home, ".copilot", "mcp-config.json") },
}

func goldenName(client string) string {
	if client == clientCodex {
		return client + ".toml"
	}
	return client + ".json"
}

func TestEachClientsEntry(t *testing.T) {
	for _, client := range clientNames {
		t.Run(client, func(t *testing.T) {
			e := newInstallEnv(t)
			res := e.run(t, "--client", client)
			if res.code != exitOK {
				t.Fatalf("exit %d: %s", res.code, res.stderr)
			}
			golden(t, goldenName(client), res.stdout)
			if !strings.Contains(res.stderr, files[client](e)) {
				t.Errorf("stderr does not name the file %s: %q", files[client](e), res.stderr)
			}
			if _, err := os.Stat(files[client](e)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("printing the entry wrote the file: %v", err)
			}

			// --write into an empty home creates the file holding exactly the printed entry,
			// readable by its owner alone.
			w := e.run(t, "--client", client, "--write")
			if w.code != exitOK {
				t.Fatalf("--write exit %d: %s", w.code, w.stderr)
			}
			if got := read(t, files[client](e)); got != res.stdout {
				t.Errorf("written:\n%s\nprinted:\n%s", got, res.stdout)
			}
			if info, _ := os.Stat(files[client](e)); info.Mode().Perm() != 0o600 {
				t.Errorf("a new file is %v, want 0600", info.Mode().Perm())
			}
			if strings.Contains(res.stdout+res.stderr+w.stdout+w.stderr+read(t, files[client](e)), tokenSecret) {
				t.Error("an entry carries the token")
			}
		})
	}
}

func TestProjectFiles(t *testing.T) {
	for client, want := range map[string]string{
		clientCursor: filepath.Join(".cursor", "mcp.json"), clientCodex: filepath.Join(".codex", "config.toml"),
		clientClaudeCode: ".mcp.json", clientVSCode: filepath.Join(".vscode", "mcp.json"),
	} {
		e := newInstallEnv(t)
		if res := e.run(t, "--client", client, "--project", "--write"); res.code != exitOK {
			t.Fatalf("%s: exit %d: %s", client, res.code, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(e.wd, want)); err != nil {
			t.Errorf("%s --project: %v", client, err)
		}
	}
	e := newInstallEnv(t)
	e.env["COPILOT_HOME"] = filepath.Join(e.home, "elsewhere")
	if res := e.run(t, "--client", clientCopilot, "--write"); res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.home, "elsewhere", "mcp-config.json")); err != nil {
		t.Errorf("COPILOT_HOME: %v", err)
	}
}

func TestAMergeKeepsEveryOtherServer(t *testing.T) {
	e := newInstallEnv(t)
	path := files[clientCursor](e)
	write(t, path, `{"theme":"dark","mcpServers":{"github":{"command":"gh-mcp","env":{"N":1.50}}},"z":[1,2]}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := e.run(t, "--client", clientCursor, "--write"); res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	golden(t, "cursor-merged.json", read(t, path))
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("the file's mode changed to %v", info.Mode().Perm())
	}

	// VS Code gains the token prompt once, beside any input already there.
	e = newInstallEnv(t)
	path = files[clientVSCode](e)
	write(t, path, `{"inputs":[{"type":"promptString","id":"other"}],"servers":{"db":{"command":"db-mcp"}}}`)
	for _, args := range [][]string{{"--write"}, {"--write", "--force"}} {
		if res := e.run(t, append([]string{"--client", clientVSCode}, args...)...); res.code != exitOK {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	}
	golden(t, "vscode-merged.json", read(t, path))
}

func TestAnExistingEntryNeedsForce(t *testing.T) {
	for _, client := range clientNames {
		t.Run(client, func(t *testing.T) {
			e := newInstallEnv(t)
			path := files[client](e)
			old := `{"` + mcpClients[client].serversKey + `":{"costfluent":{"command":"old"}}}`
			if client == clientCodex {
				old = "[mcp_servers.costfluent]\ncommand = \"old\"\n\n[mcp_servers.costfluent.env]\nX = \"1\"\n\n[mcp_servers.other]\ncommand = \"other\"\n"
			}
			write(t, path, old)
			res := e.run(t, "--client", client, "--write")
			if res.code != exitFailed || !strings.Contains(res.stderr, "--force replaces it") {
				t.Errorf("exit %d, stderr %q", res.code, res.stderr)
			}
			if read(t, path) != old {
				t.Error("a refused write changed the file")
			}
			if res := e.run(t, "--client", client, "--write", "--force"); res.code != exitOK {
				t.Fatalf("--force exit %d: %s", res.code, res.stderr)
			}
			got := read(t, path)
			if strings.Contains(got, `"old"`) || !strings.Contains(got, fixedMCP) {
				t.Errorf("--force did not replace the entry:\n%s", got)
			}
			if client == clientCodex {
				golden(t, "codex-forced.toml", got)
			}
		})
	}
}

func TestAFileItCannotParseIsLeftUnchanged(t *testing.T) {
	for _, text := range []string{
		"{\n  // my servers\n  \"servers\": {}\n}\n",
		`{"servers":{},}`,
		`[1,2]`,
		`{"servers":[]}`,
		`{"inputs":{},"servers":{}}`,
	} {
		e := newInstallEnv(t)
		path := files[clientVSCode](e)
		write(t, path, text)
		res := e.run(t, "--client", clientVSCode, "--write", "--force")
		if res.code != exitFailed || !strings.Contains(res.stderr, "left unchanged") {
			t.Errorf("%q: exit %d, stderr %q", text, res.code, res.stderr)
		}
		if read(t, path) != text {
			t.Errorf("%q was changed", text)
		}
	}
}

func TestACodexFileKeepsItsOtherTables(t *testing.T) {
	e := newInstallEnv(t)
	path := files[clientCodex](e)
	write(t, path, "model = \"gpt-5\"\n\n[mcp_servers.github]\ncommand = \"gh-mcp\"\n")
	if res := e.run(t, "--client", clientCodex, "--write"); res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	golden(t, "codex-merged.toml", read(t, path))
}

func TestTheCommandItNames(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "costfluent")
	beside := filepath.Join(dir, mcpBinary)
	write(t, beside, "")
	link := filepath.Join(t.TempDir(), mcpBinary)
	if err := os.Symlink(beside, link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		exe, onPath, want string
		warns             bool
	}{
		"beside, also on PATH":    {exe, link, link, false},
		"beside, another on PATH": {exe, fixedMCP, beside, false},
		"beside only":             {exe, "", beside, false},
		"on PATH only":            {filepath.Join(t.TempDir(), "costfluent"), fixedMCP, fixedMCP, false},
		"nowhere":                 {"", "", mcpBinary, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.exe, e.onPath = tc.exe, tc.onPath
			res := e.run(t, "--client", clientCursor)
			if res.code != exitOK {
				t.Fatalf("exit %d: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, `"command": "`+tc.want+`"`) {
				t.Errorf("want %s:\n%s", tc.want, res.stdout)
			}
			if warned := strings.Contains(res.stderr, "warning"); warned != tc.warns {
				t.Errorf("warning %v, want %v: %q", warned, tc.warns, res.stderr)
			}
		})
	}
}

func TestInstallUsage(t *testing.T) {
	e := newInstallEnv(t)
	for _, args := range [][]string{{}, {"--client", "emacs"}, {"--client", clientCopilot, "--project"}, {"--client", clientCursor, "extra"}, {"--nope"}} {
		if res := e.run(t, args...); res.code != exitUsage {
			t.Errorf("%v: exit %d, want 2", args, res.code)
		}
	}
	if res := e.run(t, "--help"); res.code != exitOK || !strings.Contains(res.stdout, "--client <client>") {
		t.Errorf("--help: exit %d, %q", res.code, res.stdout)
	}
	if res := invoke(t, nil, false, "mcp"); res.code != exitUsage || !strings.Contains(res.stderr, "install") {
		t.Errorf("mcp alone: exit %d, %q", res.code, res.stderr)
	}
}

// codexEntries counts the [mcp_servers.costfluent] tables a config.toml defines, read the way a TOML
// parser reads headers: a comment after it, spaces around the dot and either quote style.
func codexEntries(t *testing.T, text string) int {
	t.Helper()
	n := 0
	for _, line := range scanTOML(text) {
		if line.kind == tomlHeader && isCodexEntry(line.path) && len(line.path) == 2 {
			n++
		}
	}
	return n
}

func TestACodexEntryInAnyHeaderFormIsReplacedOnce(t *testing.T) {
	for name, old := range map[string]string{
		"a trailing comment":    "[mcp_servers.costfluent] # mine\ncommand = \"old\"\n",
		"spaces around the dot": "[ mcp_servers . costfluent ]\ncommand = \"old\"\n",
		"single quotes":         "[mcp_servers.'costfluent']\ncommand = \"old\"\n",
		"double quotes":         "[mcp_servers.\"costfluent\"]\ncommand = \"old\"\n",
		"a multi-line string":   "[mcp_servers.costfluent]\ncommand = \"old\"\nnote = \"\"\"\n[x]\n\"\"\"\n\n[other]\nk = 1\n",
		"a literal multi-line":  "[mcp_servers.costfluent]\ncommand = \"old\"\nnote = '''\n[x]\n'''\n\n[other]\nk = 1\n",
		"a subtable":            "[mcp_servers.costfluent]\ncommand = \"old\"\n\n[mcp_servers.costfluent.env]\nX = \"1\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newInstallEnv(t)
			path := files[clientCodex](e)
			write(t, path, old)
			if res := e.run(t, "--client", clientCodex, "--write"); res.code != exitFailed || !strings.Contains(res.stderr, "--force replaces it") {
				t.Fatalf("without --force: exit %d, stderr %q", res.code, res.stderr)
			}
			if res := e.run(t, "--client", clientCodex, "--write", "--force"); res.code != exitOK {
				t.Fatalf("--force: exit %d, stderr %q", res.code, res.stderr)
			}
			got := read(t, path)
			if n := codexEntries(t, got); n != 1 || strings.Contains(got, `"old"`) || strings.Contains(got, "[x]") {
				t.Errorf("%d entries after --force:\n%s", n, got)
			}
			if strings.Contains(old, "[other]") && !strings.Contains(got, "[other]\nk = 1\n") {
				t.Errorf("the next table was lost:\n%s", got)
			}
		})
	}
}

func TestACodexEntryWrittenAsAKeyIsLeftUnchanged(t *testing.T) {
	for name, old := range map[string]string{
		"an inline table":     "[mcp_servers]\ncostfluent = { command = \"old\" }\n",
		"a dotted key":        "mcp_servers.costfluent.command = \"old\"\n",
		"a quoted dotted key": "[mcp_servers]\n\"costfluent\".command = \"old\"\n",
		"an open string":      "[a]\nnote = \"\"\"\nnever closed\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newInstallEnv(t)
			path := files[clientCodex](e)
			write(t, path, old)
			for _, args := range [][]string{{"--write"}, {"--write", "--force"}} {
				res := e.run(t, append([]string{"--client", clientCodex}, args...)...)
				if res.code != exitFailed || !strings.Contains(res.stderr, "left unchanged") {
					t.Errorf("%v: exit %d, stderr %q", args, res.code, res.stderr)
				}
			}
			if read(t, path) != old {
				t.Error("the file was changed")
			}
		})
	}
}

func TestACodexHeaderInsideAStringIsNoEntry(t *testing.T) {
	e := newInstallEnv(t)
	path := files[clientCodex](e)
	write(t, path, "[a]\ntext = '''\n[mcp_servers.costfluent]\n'''\n")
	if res := e.run(t, "--client", clientCodex, "--write"); res.code != exitOK {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if got := read(t, path); codexEntries(t, got) != 1 || !strings.HasPrefix(got, "[a]\ntext = '''\n[mcp_servers.costfluent]\n'''\n") {
		t.Errorf("got:\n%s", got)
	}
}

func TestForceKeepsTheCommentsOfTheNextTable(t *testing.T) {
	e := newInstallEnv(t)
	path := files[clientCodex](e)
	write(t, path, "[mcp_servers.costfluent]\ncommand = \"old\"\n\n# keep me\n[mcp_servers.gh]\ncommand = \"gh\"\n")
	if res := e.run(t, "--client", clientCodex, "--write", "--force"); res.code != exitOK {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if got := read(t, path); !strings.Contains(got, "# keep me\n[mcp_servers.gh]\ncommand = \"gh\"\n") {
		t.Errorf("the next table's comment was lost:\n%s", got)
	}
}

func TestTheCodexCommandIsATOMLString(t *testing.T) {
	e := newInstallEnv(t)
	e.onPath = "/opt/a \"b\"\\c\x01/costfluent-mcp"
	res := e.run(t, "--client", clientCodex)
	if res.code != exitOK || !strings.Contains(res.stdout, `command = "/opt/a \"b\"\\c\u0001/costfluent-mcp"`) {
		t.Errorf("exit %d, stdout %q", res.code, res.stdout)
	}
	e.onPath = "/opt/\xff/costfluent-mcp"
	if res := e.run(t, "--client", clientCodex); res.code != exitFailed {
		t.Errorf("a path that is not UTF-8: exit %d, stdout %q", res.code, res.stdout)
	}
}

func TestWriteFollowsASymlinkedFile(t *testing.T) {
	e := newInstallEnv(t)
	target := filepath.Join(t.TempDir(), "dotfiles", "mcp.json")
	write(t, target, `{"mcpServers":{"gh":{"command":"gh-mcp"}}}`)
	link := files[clientCursor](e)
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if res := e.run(t, "--client", clientCursor, "--write"); res.code != exitOK {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a file: %v", err)
	}
	if got := read(t, target); !strings.Contains(got, `"gh"`) || !strings.Contains(got, `"costfluent"`) {
		t.Errorf("the link's target:\n%s", got)
	}
}

func TestEmbedTokenWritesItIntoAFileOnlyItsOwnerReads(t *testing.T) {
	for _, client := range clientNames {
		t.Run(client, func(t *testing.T) {
			e := newInstallEnv(t)
			res := e.run(t, "--client", client, "--write", "--embed-token")
			if res.code != exitOK {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
			}
			if strings.Contains(res.stdout+res.stderr, tokenSecret) {
				t.Error("the token was printed")
			}
			if !strings.Contains(res.stderr, "holds the API token") {
				t.Errorf("no warning that the file holds the token: %q", res.stderr)
			}
			path := files[client](e)
			got := read(t, path)
			want := `"COSTFLUENT_API_TOKEN": "` + tokenSecret + `"`
			if client == clientCodex {
				want = `env = { COSTFLUENT_API_TOKEN = "` + tokenSecret + `" }`
			}
			if !strings.Contains(got, want) {
				t.Errorf("the file lacks %s:\n%s", want, got)
			}
			for _, reference := range []string{"${", "env_vars", "inputs", "promptString"} {
				if strings.Contains(got, reference) {
					t.Errorf("an embedded entry still reads the token from elsewhere (%s):\n%s", reference, got)
				}
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Errorf("mode %v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func TestEmbedTokenTightensAnExistingFile(t *testing.T) {
	e := newInstallEnv(t)
	path := files[clientCursor](e)
	write(t, path, `{"mcpServers":{"github":{"command":"gh-mcp"}}}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := e.run(t, "--client", clientCursor, "--write", "--embed-token"); res.code != exitOK {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600 once the file holds the token", info.Mode().Perm())
	}
	if got := read(t, path); !strings.Contains(got, `"github"`) || !strings.Contains(got, tokenSecret) {
		t.Errorf("file:\n%s", got)
	}
}

func TestEmbedTokenNeedsWriteAndAToken(t *testing.T) {
	e := newInstallEnv(t)
	res := e.run(t, "--client", clientCursor, "--embed-token")
	if res.code != exitUsage || strings.Contains(res.stdout+res.stderr, tokenSecret) || !strings.Contains(res.stderr, "--write") {
		t.Errorf("without --write: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	e.env = map[string]string{}
	res = e.run(t, "--client", clientCursor, "--write", "--embed-token")
	if res.code != exitAccess || !strings.Contains(res.stderr, "COSTFLUENT_API_TOKEN") {
		t.Errorf("without a token: exit %d, stderr %q", res.code, res.stderr)
	}
	if _, err := os.Stat(files[clientCursor](e)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused install wrote the file: %v", err)
	}
}
