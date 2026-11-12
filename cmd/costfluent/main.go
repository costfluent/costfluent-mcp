// Command costfluent runs each Costfluent MCP tool as a command: costfluent query-costs calls the
// query_costs tool in process, with flags from its input schema, and prints its result as JSON, or
// as a table on a terminal. The tools, their capability checks, validation and output shapes are the
// MCP server's own, so a tool added there is a command here the same day.
package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/costfluent/costfluent-mcp/internal/docs"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// version is set at release by -ldflags "-X main.version=...".
var version = "dev"

// The exit statuses, which scripts and agents branch on.
const (
	exitOK     = 0
	exitFailed = 1 // the call failed
	exitUsage  = 2 // the command line is wrong
	exitAccess = 3 // no token, a base URL refused, 401 or 403, or a tool the token is not served
	// a threshold the user set was exceeded: estimate submit's --fail-above
	exitThreshold = 4
)

// apiAttemptTimeout bounds each attempt at an API call; a variable only so tests can shorten it.
var apiAttemptTimeout = server.APIAttemptTimeout

// maxRetryAfter is the longest Retry-After a call waits out before sending again, the stdio
// server's: under the 30 seconds the API asks for with every 503 and 504, so a read the API gave up
// on returns at once with its advice to narrow the read instead of running twice.
const maxRetryAfter = 29 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, &cli{
		args:       os.Args[1:],
		getenv:     os.Getenv,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		stdoutTTY:  isTerminal(os.Stdout),
		stderrTTY:  isTerminal(os.Stderr),
		executable: os.Executable,
		lookPath:   exec.LookPath,
		homeDir:    os.UserHomeDir,
		workingDir: os.Getwd,
		signIn:     newSignIn(os.Getenv),
	})
	stop()
	os.Exit(code)
}

// cli is one invocation: its arguments, environment, streams, where mcp install looks, and the
// stored sign-in.
type cli struct {
	args       []string
	getenv     func(string) string
	stdout     io.Writer
	stderr     io.Writer
	stdoutTTY  bool
	stderrTTY  bool
	executable func() (string, error)
	lookPath   func(string) (string, error)
	homeDir    func() (string, error)
	workingDir func() (string, error)
	// signIn reaches the sign-in costfluent login keeps; nil leaves commands to COSTFLUENT_API_TOKEN
	// alone, as the tests that need no sign-in build the cli.
	signIn *signIn
	// docs is where the docs commands read the documentation; the public portal when nil.
	docs *docs.Source
}

// isTerminal reports whether f is a character device, as a terminal is and a pipe or file is not.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
