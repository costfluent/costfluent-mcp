package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// mainEnv makes the test binary run main instead of the tests, so a test can run the CLI as a
// real process and read its exit status, the way a script does.
const mainEnv = "COSTFLUENT_CLI_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) == "1" {
		// The developer's own sign-in never reaches a test, as their token does not (withoutCostfluent).
		newSignIn = func(func(string) string) *signIn { return nil }
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExitStatuses(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]token{tokenSecret: {name: "cli", capabilities: catalogCapabilities()}})
	for name, tc := range map[string]struct {
		env  []string
		args []string
		want int
	}{
		"success":      {[]string{"COSTFLUENT_API_TOKEN=" + tokenSecret, "COSTFLUENT_BASE_URL=" + srv.URL}, []string{"list-budgets"}, exitOK},
		"version":      {nil, []string{"--version"}, exitOK},
		"failed call":  {[]string{"COSTFLUENT_API_TOKEN=" + tokenSecret, "COSTFLUENT_BASE_URL=http://127.0.0.1:1"}, []string{"list-budgets"}, exitFailed},
		"usage":        {nil, []string{"list-budgets", "--nope"}, exitUsage},
		"no arguments": {nil, nil, exitUsage},
		"no token":     {nil, []string{"list-budgets"}, exitAccess},
		"refused":      {[]string{"COSTFLUENT_API_TOKEN=cfk_ws_wrong", "COSTFLUENT_BASE_URL=" + srv.URL}, []string{"list-budgets"}, exitAccess},
	} {
		t.Run(name, func(t *testing.T) {
			// The child runs main before the testing package parses a flag, so it reads args as the CLI's.
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(withoutCostfluent(os.Environ()), append(tc.env, mainEnv+"=1")...)
			out, err := cmd.Output()
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.want {
				t.Errorf("exit %d, want %d; stdout %q", code, tc.want, out)
			}
		})
	}
}

// withoutCostfluent drops the variables the CLI reads, so the developer's own token never reaches a
// test.
func withoutCostfluent(env []string) []string {
	var kept []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "COSTFLUENT_") {
			kept = append(kept, kv)
		}
	}
	return kept
}
