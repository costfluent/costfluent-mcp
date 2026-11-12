package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/costfluent/costfluent-mcp/internal/config"
	"github.com/costfluent/costfluent-mcp/internal/login"
)

const loginHelp = `Usage: costfluent login [--no-browser] [--timeout <duration>] [--insecure-storage]

Signs in to Costfluent through the browser, where you approve read access for one organization, and
keeps the sign-in so commands run without COSTFLUENT_API_TOKEN. A COSTFLUENT_API_TOKEN that is set
still wins. COSTFLUENT_BASE_URL chooses the API, as for every command, and each API keeps its own
sign-in. It never prints a token.

The sign-in is kept in the operating system's credential store: the macOS Keychain, the Secret
Service on Linux, or the Windows Credential Manager.

Flags:
  --no-browser          print the address to open instead of opening the browser
  --timeout <duration>  how long to wait for the browser, such as 10m (default 5m)
  --insecure-storage    keep the sign-in in costfluent/credentials.json under XDG_CONFIG_HOME, or
                        the system's configuration directory, readable by you alone, instead of the
                        credential store: for a machine that has none
`

const logoutHelp = `Usage: costfluent logout

Revokes the sign-in costfluent login kept for the API COSTFLUENT_BASE_URL names, in the credential
store and in the file --insecure-storage keeps it in, and forgets each on this machine, also when the
revocation cannot reach the server.
`

// signIn is what login, logout and a command without COSTFLUENT_API_TOKEN reach the stored sign-ins
// and the authorization server through.
type signIn struct {
	session     *login.Session
	openBrowser func(address string) error
}

// newSignIn is the sign-in main runs with: the operating system's credential store, the file in
// the configuration directory, the system's clock and browser. A variable so the tests that run
// main as a child process keep the developer's own sign-in out of it.
var newSignIn = func(getenv func(string) string) *signIn {
	// A machine without a configuration directory leaves the path empty; the file store then says so.
	path, _ := login.FilePath(getenv, os.UserConfigDir)
	return &signIn{
		session: &login.Session{
			Stores: &login.Stores{Keyring: login.KeyringStore{Keyring: login.SystemKeyring{}}, File: login.FileStore{Path: path}},
			HTTP:   login.NewHTTPClient(),
			Now:    time.Now,
		},
		openBrowser: openBrowser,
	}
}

// errNoSignIn is a cli built without a sign-in, as the tests that need none build it.
var errNoSignIn = errors.New("this build keeps no sign-in")

// callError is a token the stored sign-in could not be refreshed for by a failure that is not a
// refusal, such as a network error: a failed call, not a lack of access.
type callError struct{ err error }

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// accessExit is the exit status for an error resolving the token a call carries.
func accessExit(err error) int {
	var ce *callError
	if errors.As(err, &ce) {
		return exitFailed
	}
	return exitAccess
}

// credentials is the token a call carries and the API it goes to: COSTFLUENT_API_TOKEN when set,
// else the access token of the sign-in costfluent login kept for the API, refreshed when it expired.
func (c *cli) credentials(ctx context.Context) (token, baseURL string, err error) {
	if c.signIn == nil || strings.TrimSpace(c.getenv(config.EnvAPIToken)) != "" {
		cfg, err := config.Load(c.getenv)
		return cfg.APIToken, cfg.BaseURL, err
	}
	if baseURL, err = config.BaseURL(c.getenv); err != nil {
		return "", "", err
	}
	token, err = c.signIn.session.AccessToken(ctx, baseURL)
	switch {
	case errors.Is(err, login.ErrNotSignedIn):
		msg := fmt.Sprintf("%s is not set and you are not signed in to %s: run costfluent login, or create an API token under "+
			"Settings, API tokens, and set it in the environment the command runs in", config.EnvAPIToken, baseURL)
		// A credential store that could not be read says why, after the not signed in it wraps.
		if detail, ok := strings.CutPrefix(err.Error(), login.ErrNotSignedIn.Error()+" "); ok {
			msg += " " + detail
		}
		return "", "", errors.New(msg)
	case errors.Is(err, login.ErrRefused):
		return "", "", fmt.Errorf("%w; run costfluent login to sign in again", err)
	case err != nil:
		return "", "", &callError{err: err}
	}
	return token, baseURL, nil
}

// loginOptions is a login command line.
type loginOptions struct {
	noBrowser       bool
	timeout         time.Duration
	insecureStorage bool
}

func parseLogin(argv []string) (loginOptions, error) {
	o := loginOptions{timeout: login.DefaultTimeout}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.noBrowser, "no-browser", false, "")
	fs.DurationVar(&o.timeout, "timeout", o.timeout, "")
	fs.BoolVar(&o.insecureStorage, "insecure-storage", false, "")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, errHelp
		}
		return o, &usageError{msg: unprefixed(err)}
	}
	if fs.NArg() > 0 {
		return o, usagef("unexpected argument %q", fs.Arg(0))
	}
	if o.timeout <= 0 {
		return o, usagef("--timeout must be longer than zero, such as 10m")
	}
	return o, nil
}

// login signs in through the browser and keeps the sign-in.
func (c *cli) login(ctx context.Context, argv []string) int {
	o, err := parseLogin(argv)
	if errors.Is(err, errHelp) {
		return c.print(loginHelp)
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent login --help lists its flags")
	}
	if c.signIn == nil {
		return c.fail(exitFailed, errNoSignIn.Error())
	}
	resource, err := config.BaseURL(c.getenv)
	if err != nil {
		return c.fail(exitAccess, err.Error())
	}
	if strings.TrimSpace(c.getenv(config.EnvAPIToken)) != "" {
		c.warn("costfluent: " + config.EnvAPIToken + " is set, so commands run where it is set use it rather than this sign-in\n")
	}
	var open func(string) error
	if !o.noBrowser {
		open = c.signIn.openBrowser
	}
	session := c.signIn.session
	flow := login.NewFlow(open, c.stderr, o.timeout)
	flow.HTTP, flow.Now = session.HTTP, session.Now
	cred, err := flow.Login(ctx, resource)
	switch {
	case errors.Is(err, login.ErrDenied):
		return c.fail(exitAccess, err.Error())
	case err != nil && ctx.Err() != nil:
		return c.fail(exitFailed, "interrupted")
	case err != nil:
		return c.fail(exitFailed, err.Error())
	}
	store, err := session.Stores.Save(ctx, cred, o.insecureStorage)
	if err != nil && store != nil {
		// The new sign-in is kept and works, so the login stands; the older copy is logout's to end
		// once its store can be reached, and failing here would leave a machine without a credential
		// store unable to sign in at all.
		c.warn("costfluent: " + escape(err.Error()) + "; costfluent logout revokes and forgets that copy once it can be read\n")
	} else if err != nil {
		// A sign-in kept nowhere is one no one can revoke from here, so it ends now.
		_ = login.Revoke(ctx, session.HTTP, cred)
		msg := "signed in, but the sign-in could not be kept: " + err.Error()
		if !o.insecureStorage {
			msg += "; on a machine without a credential store, run costfluent login --insecure-storage to keep it in " +
				session.Stores.File.Where() + ", readable by you alone"
		}
		return c.fail(exitFailed, msg)
	}
	return c.print(fmt.Sprintf("Signed in to %s. The sign-in is kept in %s; costfluent whoami shows what it can read.\n", resource, store.Where()))
}

// logout revokes the stored sign-in and forgets it.
func (c *cli) logout(ctx context.Context, argv []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	err := fs.Parse(argv)
	switch {
	case errors.Is(err, errHelp):
	case err != nil:
		err = &usageError{msg: unprefixed(err)}
	case fs.NArg() > 0:
		err = usagef("unexpected argument %q", fs.Arg(0))
	}
	if errors.Is(err, errHelp) {
		return c.print(logoutHelp)
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent logout --help says what it does")
	}
	if c.signIn == nil {
		return c.fail(exitFailed, errNoSignIn.Error())
	}
	resource, err := config.BaseURL(c.getenv)
	if err != nil {
		return c.fail(exitAccess, err.Error())
	}
	session := c.signIn.session
	// Both stores can hold a sign-in for the API, as when one was kept in the file while the
	// credential store could not forget its own, so each is ended. A credential store that cannot be
	// read is the machine without one that Stores.Load reads past, so it is said but fails nothing;
	// a file that cannot be read is the user's to fix, as every command says.
	var failures []string
	found := false
	for _, s := range []struct {
		store    login.Store
		optional bool
	}{{session.Stores.Keyring, true}, {session.Stores.File, false}} {
		cred, err := s.store.Load(ctx, resource)
		switch {
		case errors.Is(err, login.ErrNotSignedIn):
			continue
		case err != nil && s.optional:
			c.warn(fmt.Sprintf("costfluent: a sign-in to %s that %s may hold is neither revoked nor forgotten: %s\n", resource, s.store.Where(), escape(err.Error())))
			continue
		case err != nil:
			failures = append(failures, err.Error())
			continue
		}
		found = true
		revokeErr := login.Revoke(ctx, session.HTTP, cred)
		deleteErr := s.store.Delete(ctx, resource)
		switch {
		case deleteErr != nil && revokeErr != nil:
			failures = append(failures, fmt.Sprintf("could not forget the sign-in to %s in %s: %v, nor revoke it at the server: %v; "+
				"revoke it under Settings, Connected apps", resource, s.store.Where(), deleteErr, revokeErr))
		case deleteErr != nil:
			failures = append(failures, fmt.Sprintf("could not forget the sign-in to %s in %s: %v", resource, s.store.Where(), deleteErr))
		case revokeErr != nil:
			failures = append(failures, fmt.Sprintf("forgot the sign-in to %s in %s, but could not revoke it at the server: %v; "+
				"revoke it under Settings, Connected apps", resource, s.store.Where(), revokeErr))
		}
	}
	if len(failures) > 0 {
		return c.fail(exitFailed, strings.Join(failures, "; "))
	}
	if !found {
		return c.print(fmt.Sprintf("Not signed in to %s.\n", resource))
	}
	return c.print(fmt.Sprintf("Signed out of %s.\n", resource))
}

// openBrowser opens address in the user's browser without waiting for it.
func openBrowser(address string) error {
	name, args := browserCommand(runtime.GOOS, address)
	cmd := exec.Command(name, args...) //nolint:gosec // the platform's own opener, given the authorization address as one argument
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand is the command that opens an address on goos. The address is one argument, never
// a shell's input, so its & and ? stay part of it.
func browserCommand(goos, address string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{address}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", address}
	}
	return "xdg-open", []string{address}
}
