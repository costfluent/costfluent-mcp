package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/costfluent/costfluent-mcp/internal/login"
)

var clock = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// fakeAS is the authorization server the fake API names in its protected resource metadata, as the
// Backend API will be one: authorize answers at once, as a member who approves, or denies.
type fakeAS struct {
	srv    *httptest.Server
	apiURL string

	deny          bool
	refuseRefresh bool
	revokeFails   bool

	mu      sync.Mutex
	issued  int
	refresh string
	revoked []string
}

// signInAPI is a fake API that serves its protected resource metadata beside the reads, an
// authorization server it trusts, and an environment naming the API without a token. The API
// knows the access tokens the server issues and the environment token alike.
func signInAPI(t *testing.T) (*fakeAPI, *fakeAS, map[string]string) {
	t.Helper()
	tokens := map[string]token{tokenSecret: {name: "env token", capabilities: catalogCapabilities()}}
	for i := 1; i <= 3; i++ {
		tokens["cf_oat_"+strconv.Itoa(i)] = token{name: "costfluent-cli", org: true, capabilities: catalogCapabilities()}
	}
	api := &fakeAPI{tokens: tokens}
	as := &fakeAS{}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			answerJSON(w, http.StatusOK, map[string]any{"resource": as.apiURL, "authorization_servers": []string{as.srv.URL}})
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(apiSrv.Close)
	as.apiURL = apiSrv.URL
	as.srv = httptest.NewServer(as.mux())
	t.Cleanup(as.srv.Close)
	return api, as, map[string]string{"COSTFLUENT_BASE_URL": apiSrv.URL}
}

func answerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (as *fakeAS) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, map[string]any{
			"issuer": as.srv.URL, "authorization_endpoint": as.srv.URL + "/oauth/authorize", "token_endpoint": as.srv.URL + "/oauth/token",
			"revocation_endpoint": as.srv.URL + "/oauth/revoke", "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back := url.Values{"state": {q.Get("state")}, "iss": {as.srv.URL}}
		if as.deny {
			back.Set("error", "access_denied")
		} else {
			back.Set("code", "code-1")
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		as.mu.Lock()
		defer as.mu.Unlock()
		f := r.PostForm
		ok := f.Get("client_id") == login.ClientID && f.Get("resource") == as.apiURL
		switch f.Get("grant_type") {
		case "authorization_code":
			ok = ok && f.Get("code") == "code-1" && f.Get("code_verifier") != ""
		case "refresh_token":
			ok = ok && !as.refuseRefresh && f.Get("refresh_token") == as.refresh
		}
		if !ok {
			answerJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		as.issued++
		as.refresh = "cf_ort_" + strconv.Itoa(as.issued)
		answerJSON(w, http.StatusOK, map[string]any{"access_token": "cf_oat_" + strconv.Itoa(as.issued), "token_type": "Bearer",
			"expires_in": 3600, "refresh_token": as.refresh, "scope": "read offline_access"})
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		as.mu.Lock()
		as.revoked = append(as.revoked, r.PostForm.Get("token"))
		as.mu.Unlock()
		if as.revokeFails {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	return mux
}

// memKeyring is a credential store in memory; err answers every call when set, as a machine
// without one does, and deleteErr answers deletes.
type memKeyring struct {
	items          map[string]string
	err, deleteErr error
}

func (k *memKeyring) Get(service, user string) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	v, ok := k.items[service+" "+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *memKeyring) Set(service, user, password string) error {
	if k.err != nil {
		return k.err
	}
	k.items[service+" "+user] = password
	return nil
}

func (k *memKeyring) Delete(service, user string) error {
	if k.err != nil {
		return k.err
	}
	if k.deleteErr != nil {
		return k.deleteErr
	}
	delete(k.items, service+" "+user)
	return nil
}

// testSignIn is a sign-in over an in-memory credential store and a file in a temporary directory,
// whose browser approves at once and whose clock the test moves.
type testSignIn struct {
	*signIn
	keyring *memKeyring
	file    string
	now     time.Time
	opened  int
}

func newTestSignIn(t *testing.T) *testSignIn {
	t.Helper()
	ts := &testSignIn{keyring: &memKeyring{items: map[string]string{}}, file: filepath.Join(t.TempDir(), "costfluent", "credentials.json"), now: clock}
	ts.signIn = &signIn{
		session: &login.Session{
			Stores: &login.Stores{Keyring: login.KeyringStore{Keyring: ts.keyring}, File: login.FileStore{Path: ts.file}},
			HTTP:   login.NewHTTPClient(),
			Now:    func() time.Time { return ts.now },
		},
		openBrowser: func(address string) error {
			ts.opened++
			res, err := http.Get(address) //nolint:gosec,noctx // the test's own servers
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, res.Body)
			return res.Body.Close()
		},
	}
	return ts
}

// signedIn runs the CLI with a sign-in.
func signedIn(t *testing.T, ctx context.Context, ts *testSignIn, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	c := &cli{args: args, getenv: func(k string) string { return env[k] }, stdout: &stdout, stderr: &stderr}
	if ts != nil {
		c.signIn = ts.signIn
	}
	code := run(ctx, c)
	res := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	for _, secret := range []string{"cf_oat_", "cf_ort_", tokenSecret} {
		if strings.Contains(res.stdout+res.stderr, secret) {
			t.Errorf("%v printed a token: stdout %q, stderr %q", args, res.stdout, res.stderr)
		}
	}
	return res
}

func bearer(api *fakeAPI) string {
	return strings.TrimPrefix(api.last("/v1/me").Header.Get("Authorization"), "Bearer ")
}

func TestLoginThenCommandsUseTheSignIn(t *testing.T) {
	api, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	ctx := context.Background()

	res := signedIn(t, ctx, ts, env, "login")
	if res.code != exitOK || !strings.Contains(res.stdout, "Signed in to "+env["COSTFLUENT_BASE_URL"]) ||
		!strings.Contains(res.stdout, "the system credential store") {
		t.Fatalf("login: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, as.srv.URL+"/oauth/authorize?") || ts.opened != 1 {
		t.Errorf("login: stderr %q, browser opened %d times", res.stderr, ts.opened)
	}
	if len(ts.keyring.items) != 1 {
		t.Errorf("credential store holds %v", ts.keyring.items)
	}
	if _, err := os.Stat(ts.file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a sign-in without --insecure-storage wrote the file: %v", err)
	}

	res = signedIn(t, ctx, ts, env, "whoami")
	if res.code != exitOK || !strings.Contains(res.stdout, `"costfluent-cli"`) || bearer(api) != "cf_oat_1" {
		t.Fatalf("whoami: exit %d, stdout %q, stderr %q, bearer %q", res.code, res.stdout, res.stderr, bearer(api))
	}
	if res := signedIn(t, ctx, ts, env, "list-workspaces"); res.code != exitOK {
		t.Errorf("list-workspaces: exit %d, stderr %q", res.code, res.stderr)
	}
	if as.issued != 1 {
		t.Errorf("a live access token was refreshed: %d tokens issued", as.issued)
	}
}

func TestTheEnvironmentTokenWinsOverTheSignIn(t *testing.T) {
	api, _, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	env["COSTFLUENT_API_TOKEN"] = tokenSecret
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitOK || bearer(api) != tokenSecret {
		t.Fatalf("whoami: exit %d, bearer %q", res.code, bearer(api))
	}
	res := signedIn(t, context.Background(), ts, env, "login")
	if res.code != exitOK || !strings.Contains(res.stderr, "COSTFLUENT_API_TOKEN is set") {
		t.Errorf("login with the variable set: exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestAnExpiredSignInIsRefreshedAndTheRotatedTokenKept(t *testing.T) {
	api, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	ts.now = clock.Add(2 * time.Hour)
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitOK || bearer(api) != "cf_oat_2" {
		t.Fatalf("whoami: exit %d, stderr %q, bearer %q", res.code, res.stderr, bearer(api))
	}
	cred, _, err := ts.session.Stores.Load(context.Background(), env["COSTFLUENT_BASE_URL"])
	if err != nil || cred.RefreshToken != "cf_ort_2" || as.refresh != "cf_ort_2" {
		t.Fatalf("kept %v, %v; want the rotated refresh token", cred, err)
	}
}

func TestARefusedRefreshAsksForANewSignIn(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	ts.now, as.refuseRefresh = clock.Add(2*time.Hour), true
	res := signedIn(t, context.Background(), ts, env, "whoami")
	if res.code != exitAccess || !strings.Contains(res.stderr, "invalid_grant") || !strings.Contains(res.stderr, "run costfluent login to sign in again") {
		t.Fatalf("whoami: exit %d, stderr %q", res.code, res.stderr)
	}

	// An authorization server that cannot be reached is a failed call, not a refused sign-in.
	as.srv.Close()
	res = signedIn(t, context.Background(), ts, env, "whoami")
	if res.code != exitFailed || !strings.Contains(res.stderr, "refreshing the sign-in") {
		t.Fatalf("unreachable: exit %d, stderr %q", res.code, res.stderr)
	}
	res = signedIn(t, context.Background(), ts, env, "estimate", "apply", "--workspace-id", "ws_1", "--repository", "a/b",
		"--pull-request", "1", "--terraform-plan-json", "missing.json", "--terraform-state-json", "missing.json")
	if res.code != exitFailed || !strings.Contains(res.stderr, "refreshing the sign-in") {
		t.Errorf("estimate apply: exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestWithoutATokenOrSignInTheErrorNamesBoth(t *testing.T) {
	_, _, env := signInAPI(t)
	ts := newTestSignIn(t)
	res := signedIn(t, context.Background(), ts, env, "whoami")
	if res.code != exitAccess || !strings.Contains(res.stderr, "COSTFLUENT_API_TOKEN is not set and you are not signed in") ||
		!strings.Contains(res.stderr, "run costfluent login") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	// A machine without a credential store says why, so a locked keyring is not taken for no sign-in.
	ts.keyring.err = errors.New("no Secret Service")
	res = signedIn(t, context.Background(), ts, env, "whoami")
	if res.code != exitAccess || !strings.Contains(res.stderr, "(reading the system credential store: no Secret Service)") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestLogoutRevokesAndForgetsTheSignIn(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	res := signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitOK || !strings.Contains(res.stdout, "Signed out of") {
		t.Fatalf("logout: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if len(as.revoked) != 1 || as.revoked[0] != "cf_ort_1" || len(ts.keyring.items) != 0 {
		t.Errorf("revoked %v, credential store %v", as.revoked, ts.keyring.items)
	}
	res = signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitOK || !strings.Contains(res.stdout, "Not signed in to") || len(as.revoked) != 1 {
		t.Errorf("logout again: exit %d, stdout %q, revoked %v", res.code, res.stdout, as.revoked)
	}
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitAccess {
		t.Errorf("whoami after logout: exit %d", res.code)
	}
}

func TestLogoutForgetsTheSignInWhenRevocationFails(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	as.revokeFails = true
	res := signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitFailed || !strings.Contains(res.stderr, "could not revoke it at the server") ||
		!strings.Contains(res.stderr, "Settings, Connected apps") || len(ts.keyring.items) != 0 {
		t.Fatalf("exit %d, stderr %q, credential store %v", res.code, res.stderr, ts.keyring.items)
	}

	// A sign-in the store cannot forget is reported, not claimed forgotten.
	as.revokeFails = false
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	ts.keyring.deleteErr = errors.New("the keychain is locked")
	res = signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitFailed || !strings.Contains(res.stderr, "could not forget the sign-in") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
}

// A sign-in kept in the file while the credential store could not be reached leaves the credential
// store's older one in place; logout ends both, so neither keeps working after it.
func TestLogoutSignsOutOfBothStores(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	ts.keyring.err = errors.New("no Secret Service")
	res := signedIn(t, context.Background(), ts, env, "login", "--insecure-storage")
	if res.code != exitOK {
		t.Fatalf("login --insecure-storage: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "the system credential store") || !strings.Contains(res.stderr, "costfluent logout") {
		t.Errorf("login --insecure-storage did not say the credential store's copy was kept: stderr %q", res.stderr)
	}
	ts.keyring.err = nil

	res = signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitOK || !strings.Contains(res.stdout, "Signed out of") {
		t.Fatalf("logout: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	slices.Sort(as.revoked)
	if !slices.Equal(as.revoked, []string{"cf_ort_1", "cf_ort_2"}) || len(ts.keyring.items) != 0 {
		t.Errorf("revoked %v, credential store %v", as.revoked, ts.keyring.items)
	}
	if _, err := os.Stat(ts.file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file survived logout: %v", err)
	}
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitAccess {
		t.Errorf("whoami after logout: exit %d, stdout %q", res.code, res.stdout)
	}
}

// Each copy logout cannot end is reported, and the copies it can end are still ended.
func TestLogoutReportsEachCopyItCouldNotEnd(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	if res := signedIn(t, context.Background(), ts, env, "login"); res.code != exitOK {
		t.Fatalf("login: exit %d, %q", res.code, res.stderr)
	}
	ts.keyring.err = errors.New("no Secret Service")
	if res := signedIn(t, context.Background(), ts, env, "login", "--insecure-storage"); res.code != exitOK {
		t.Fatalf("login --insecure-storage: exit %d, %q", res.code, res.stderr)
	}
	ts.keyring.err = nil

	// The credential store cannot forget its copy: the file's is still revoked and forgotten.
	ts.keyring.deleteErr = errors.New("the keychain is locked")
	res := signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitFailed || !strings.Contains(res.stderr, "could not forget the sign-in") || !strings.Contains(res.stderr, "the keychain is locked") {
		t.Fatalf("logout: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(ts.file); !errors.Is(err, os.ErrNotExist) || len(as.revoked) != 2 {
		t.Errorf("file %v, revoked %v", err, as.revoked)
	}

	// A credential store that cannot be read is said, and does not hide that nothing else was found.
	ts.keyring.deleteErr, ts.keyring.err = nil, errors.New("no Secret Service")
	res = signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitOK || !strings.Contains(res.stdout, "Not signed in to") || !strings.Contains(res.stderr, "no Secret Service") {
		t.Errorf("logout without a credential store: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
}

// hungKeyring is a credential store that never answers, as a locked Secret Service collection whose
// unlock prompt no one answers; its calls return once the test ends.
type hungKeyring struct{ release chan struct{} }

func (k hungKeyring) Get(string, string) (string, error) { <-k.release; return "", keyring.ErrNotFound }
func (k hungKeyring) Set(string, string, string) error   { <-k.release; return nil }
func (k hungKeyring) Delete(string, string) error        { <-k.release; return nil }

// A credential store that never answers fails each command in its time and names the ways out.
func TestAHungCredentialStoreFailsTheCommandInTime(t *testing.T) {
	_, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	hung := hungKeyring{release: make(chan struct{})}
	t.Cleanup(func() { close(hung.release) })
	ts.session.Stores.Keyring = login.KeyringStore{Keyring: hung, Timeout: 50 * time.Millisecond}
	start := time.Now()

	res := signedIn(t, context.Background(), ts, env, "whoami")
	if res.code != exitAccess || !strings.Contains(res.stderr, "did not answer") || !strings.Contains(res.stderr, "costfluent login --insecure-storage") {
		t.Errorf("whoami: exit %d, stderr %q", res.code, res.stderr)
	}
	res = signedIn(t, context.Background(), ts, env, "login")
	if res.code != exitFailed || !strings.Contains(res.stderr, "did not answer") || len(as.revoked) != 1 {
		t.Errorf("login: exit %d, stderr %q, revoked %v", res.code, res.stderr, as.revoked)
	}
	res = signedIn(t, context.Background(), ts, env, "login", "--insecure-storage")
	if res.code != exitOK || !strings.Contains(res.stderr, "did not answer") {
		t.Errorf("login --insecure-storage: exit %d, stderr %q", res.code, res.stderr)
	}
	res = signedIn(t, context.Background(), ts, env, "logout")
	if res.code != exitOK || !strings.Contains(res.stdout, "Signed out of") || !strings.Contains(res.stderr, "did not answer") || len(as.revoked) != 2 {
		t.Errorf("logout: exit %d, stdout %q, stderr %q, revoked %v", res.code, res.stdout, res.stderr, as.revoked)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the commands took %s", elapsed)
	}
}

func TestLoginDeniedInTheBrowserExits3(t *testing.T) {
	_, as, env := signInAPI(t)
	as.deny = true
	ts := newTestSignIn(t)
	res := signedIn(t, context.Background(), ts, env, "login")
	if res.code != exitAccess || !strings.Contains(res.stderr, "access was denied in the browser") || len(ts.keyring.items) != 0 {
		t.Fatalf("exit %d, stderr %q, credential store %v", res.code, res.stderr, ts.keyring.items)
	}
}

func TestLoginWithoutACredentialStoreNeedsTheFlag(t *testing.T) {
	api, as, env := signInAPI(t)
	ts := newTestSignIn(t)
	ts.keyring.err = errors.New("no Secret Service")
	res := signedIn(t, context.Background(), ts, env, "login")
	if res.code != exitFailed || !strings.Contains(res.stderr, "costfluent login --insecure-storage") || !strings.Contains(res.stderr, ts.file) {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
	if _, err := os.Stat(ts.file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file was written without the flag: %v", err)
	}
	if len(as.revoked) != 1 {
		t.Errorf("a sign-in kept nowhere was not revoked: %v", as.revoked)
	}

	res = signedIn(t, context.Background(), ts, env, "login", "--insecure-storage")
	if res.code != exitOK || !strings.Contains(res.stdout, ts.file) {
		t.Fatalf("--insecure-storage: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(ts.file); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v, %v", info.Mode().Perm(), err)
		}
	}
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitOK || bearer(api) != "cf_oat_2" {
		t.Fatalf("whoami from the file: exit %d, stderr %q", res.code, res.stderr)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(ts.file, 0o644); err != nil {
			t.Fatal(err)
		}
		res = signedIn(t, context.Background(), ts, env, "whoami")
		if res.code != exitFailed || !strings.Contains(res.stderr, "chmod 600") {
			t.Errorf("a file others can read: exit %d, stderr %q", res.code, res.stderr)
		}
		res = signedIn(t, context.Background(), ts, env, "logout")
		if res.code != exitFailed || !strings.Contains(res.stderr, "chmod 600") {
			t.Errorf("logout of a file others can read: exit %d, stderr %q", res.code, res.stderr)
		}
	}
}

func TestLoginWithoutABrowserWaitsNoLongerThanItsTimeout(t *testing.T) {
	_, _, env := signInAPI(t)
	ts := newTestSignIn(t)
	res := signedIn(t, context.Background(), ts, env, "login", "--no-browser", "--timeout", "50ms")
	if res.code != exitFailed || !strings.Contains(res.stderr, "Open this address in a browser") ||
		!strings.Contains(res.stderr, "did not come back in time") || ts.opened != 0 {
		t.Fatalf("exit %d, stderr %q, browser opened %d times", res.code, res.stderr, ts.opened)
	}
}

func TestLoginStopsOnCtrlC(t *testing.T) {
	_, _, env := signInAPI(t)
	ts := newTestSignIn(t)
	ctx, cancel := context.WithCancel(context.Background())
	ts.openBrowser = func(string) error { cancel(); return nil }
	res := signedIn(t, ctx, ts, env, "login")
	if res.code != exitFailed || !strings.Contains(res.stderr, "interrupted") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestLoginReportsAnAPIWithoutSignIn(t *testing.T) {
	_, srv := newFakeAPI(t, nil)
	ts := newTestSignIn(t)
	res := signedIn(t, context.Background(), ts, map[string]string{"COSTFLUENT_BASE_URL": srv.URL}, "login")
	if res.code != exitFailed || !strings.Contains(res.stderr, "sign-in metadata") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestLoginAndLogoutCommandLines(t *testing.T) {
	ts := newTestSignIn(t)
	for name, tc := range map[string]struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		"login help":       {[]string{"login", "--help"}, exitOK, "Usage: costfluent login", ""},
		"help login":       {[]string{"help", "login"}, exitOK, "--insecure-storage", ""},
		"logout help":      {[]string{"logout", "-h"}, exitOK, "Usage: costfluent logout", ""},
		"help logout":      {[]string{"help", "logout"}, exitOK, "Revokes the sign-in", ""},
		"usage lists them": {[]string{"--help"}, exitOK, "logout            Revoke the sign-in", ""},
		"zero timeout":     {[]string{"login", "--timeout", "0s"}, exitUsage, "", "--timeout must be longer than zero"},
		"bad timeout":      {[]string{"login", "--timeout", "soon"}, exitUsage, "", "costfluent login --help"},
		"login argument":   {[]string{"login", "now"}, exitUsage, "", `unexpected argument "now"`},
		"logout flag":      {[]string{"logout", "--all"}, exitUsage, "", "costfluent logout --help"},
		"logout argument":  {[]string{"logout", "now"}, exitUsage, "", `unexpected argument "now"`},
		"login plain http": {[]string{"login"}, exitAccess, "", "COSTFLUENT_BASE_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"COSTFLUENT_BASE_URL": "http://api.costfluent.com"}
			res := signedIn(t, context.Background(), ts, env, tc.args...)
			if res.code != tc.code || !strings.Contains(res.stdout, tc.stdout) || !strings.Contains(res.stderr, tc.stderr) {
				t.Errorf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
			}
		})
	}
	env := map[string]string{"COSTFLUENT_BASE_URL": "http://api.costfluent.com"}
	if res := signedIn(t, context.Background(), ts, env, "logout"); res.code != exitAccess {
		t.Errorf("logout from a plain http API: exit %d", res.code)
	}
	if res := signedIn(t, context.Background(), ts, env, "whoami"); res.code != exitAccess {
		t.Errorf("whoami from a plain http API: exit %d", res.code)
	}
}

func TestLoginWithoutASignInBuild(t *testing.T) {
	for _, cmd := range []string{"login", "logout"} {
		if res := signedIn(t, context.Background(), nil, nil, cmd); res.code != exitFailed || !strings.Contains(res.stderr, "keeps no sign-in") {
			t.Errorf("%s: exit %d, stderr %q", cmd, res.code, res.stderr)
		}
	}
}

func TestNewSignInUsesTheSystemsStores(t *testing.T) {
	si := newSignIn(func(k string) string { return map[string]string{"XDG_CONFIG_HOME": "/xdg"}[k] })
	if got := si.session.Stores.File.Where(); got != filepath.Join("/xdg", "costfluent", "credentials.json") {
		t.Errorf("file %s", got)
	}
	if _, ok := si.session.Stores.Keyring.(login.KeyringStore); !ok || si.openBrowser == nil || si.session.Now == nil {
		t.Errorf("sign-in %+v", si.session)
	}
}

func TestBrowserCommand(t *testing.T) {
	const address = "https://app.costfluent.com/oauth/authorize?a=1&b=2"
	for goos, want := range map[string][]string{
		"darwin":  {"open", address},
		"linux":   {"xdg-open", address},
		"freebsd": {"xdg-open", address},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", address},
	} {
		name, args := browserCommand(goos, address)
		if got := append([]string{name}, args...); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: %v, want %v", goos, got, want)
		}
	}
}
