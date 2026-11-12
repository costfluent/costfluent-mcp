package login

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// authServer is a fake API and the authorization server it trusts, as the plan's contract has them:
// the API's protected resource metadata, the server's metadata, authorize, token and revoke.
type authServer struct {
	api, as *httptest.Server

	// Tampering, each nil or zero for the contract's own answer.
	resourceMeta func(m map[string]any)
	serverMeta   func(m map[string]any)
	callback     func(q url.Values) // edits the parameters authorize redirects back with
	tokenStatus  int                // answers every token request with this status and tokenBody
	tokenBody    string
	tokenEdit    func(m map[string]any)
	revokeStatus int

	mu            sync.Mutex
	authorize     url.Values
	challenge     string
	redirectURI   string
	tokenForms    []url.Values
	revoked       []url.Values
	refresh       string // the one refresh token that is live
	issued        int
	callbackPages []string
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	s := &authServer{}
	s.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			http.NotFound(w, r)
			return
		}
		m := map[string]any{"resource": s.api.URL, "authorization_servers": []string{s.as.URL}, "bearer_methods_supported": []string{"header"}}
		if s.resourceMeta != nil {
			s.resourceMeta(m)
		}
		writeJSON(w, http.StatusOK, m)
	}))
	t.Cleanup(s.api.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		m := map[string]any{
			"issuer":                           s.as.URL,
			"authorization_endpoint":           s.as.URL + "/api/oauth/authorize",
			"token_endpoint":                   s.as.URL + "/api/oauth/token",
			"revocation_endpoint":              s.as.URL + "/api/oauth/revoke",
			"code_challenge_methods_supported": []string{"S256"},
		}
		if s.serverMeta != nil {
			s.serverMeta(m)
		}
		writeJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /api/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		s.authorize, s.challenge, s.redirectURI = q, q.Get("code_challenge"), q.Get("redirect_uri")
		s.mu.Unlock()
		back := url.Values{"code": {"code-1"}, "state": {q.Get("state")}, "iss": {s.as.URL}}
		if s.callback != nil {
			s.callback(back)
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	})
	mux.HandleFunc("POST /api/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tokenForms = append(s.tokenForms, r.PostForm)
		if s.tokenStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.tokenStatus)
			_, _ = io.WriteString(w, s.tokenBody)
			return
		}
		f := r.PostForm
		ok := f.Get("client_id") == ClientID && f.Get("resource") == s.api.URL
		switch f.Get("grant_type") {
		case "authorization_code":
			ok = ok && f.Get("code") == "code-1" && f.Get("redirect_uri") == s.redirectURI && challenge(f.Get("code_verifier")) == s.challenge
		case "refresh_token":
			ok = ok && s.refresh != "" && f.Get("refresh_token") == s.refresh
		default:
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refused"})
			return
		}
		s.issued++
		s.refresh = "cf_ort_" + strconv.Itoa(s.issued)
		m := map[string]any{"access_token": "cf_oat_" + strconv.Itoa(s.issued), "token_type": "Bearer", "expires_in": 3600,
			"refresh_token": s.refresh, "scope": "read offline_access"}
		if s.tokenEdit != nil {
			s.tokenEdit(m)
		}
		writeJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("POST /api/oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.revoked = append(s.revoked, r.PostForm)
		s.mu.Unlock()
		if s.revokeStatus != 0 {
			writeJSON(w, s.revokeStatus, map[string]any{"error": "unsupported_token_type"})
		}
	})
	s.as = httptest.NewServer(mux)
	t.Cleanup(s.as.Close)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// flow is a flow whose browser follows the authorization address, as a user who approves does,
// and records the page the callback answered.
func (s *authServer) flow(out io.Writer) Flow {
	return Flow{
		HTTP: NewHTTPClient(),
		Now:  func() time.Time { return clock },
		Rand: rand.Reader,
		OpenBrowser: func(address string) error {
			res, err := http.Get(address) //nolint:gosec,noctx // the test's own server
			if err != nil {
				return err
			}
			defer func() { _ = res.Body.Close() }()
			body, _ := io.ReadAll(res.Body)
			s.mu.Lock()
			s.callbackPages = append(s.callbackPages, fmt.Sprintf("%d %s %s", res.StatusCode, res.Header.Get("Content-Type"), body))
			s.mu.Unlock()
			return nil
		},
		Out:     out,
		Timeout: 5 * time.Second,
	}
}

func TestLoginSignsInWithPKCEAndTheResource(t *testing.T) {
	s := newAuthServer(t)
	var out bytes.Buffer
	cred, err := s.flow(&out).Login(context.Background(), s.api.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := Credential{
		Resource: s.api.URL, Issuer: s.as.URL, TokenEndpoint: s.as.URL + "/api/oauth/token", RevocationEndpoint: s.as.URL + "/api/oauth/revoke",
		AccessToken: "cf_oat_1", ExpiresAt: clock.Add(time.Hour), RefreshToken: "cf_ort_1",
	}
	if *cred != want {
		t.Errorf("credential %#v, want %#v", *cred, want)
	}
	q := s.authorize
	for k, v := range map[string]string{"response_type": "code", "client_id": "costfluent-cli", "scope": "read offline_access",
		"code_challenge_method": "S256", "resource": s.api.URL} {
		if q.Get(k) != v {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), v)
		}
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" || redirect.Path != "/callback" {
		t.Errorf("redirect_uri %q, want http://127.0.0.1:<port>/callback", q.Get("redirect_uri"))
	}
	if len(q.Get("state")) != 43 || len(q.Get("code_challenge")) != 43 {
		t.Errorf("state %q, code_challenge %q: want 43 characters each", q.Get("state"), q.Get("code_challenge"))
	}
	form := s.tokenForms[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code_verifier") == "" || form.Get("resource") != s.api.URL {
		t.Errorf("token request %v", form)
	}
	if page := s.callbackPages[0]; !strings.HasPrefix(page, "200 text/plain") || !strings.Contains(page, "return to the terminal") {
		t.Errorf("callback page %q", page)
	}
	if !strings.Contains(out.String(), "Your browser has been opened") || !strings.Contains(out.String(), s.as.URL+"/api/oauth/authorize?") {
		t.Errorf("printed %q, want the address", out.String())
	}
	if strings.Contains(out.String(), "cf_oat_") || strings.Contains(out.String(), "cf_ort_") {
		t.Errorf("printed a token: %q", out.String())
	}
}

func TestLoginKeepsTheAuthorizationEndpointsOwnQuery(t *testing.T) {
	s := newAuthServer(t)
	s.serverMeta = func(m map[string]any) { m["authorization_endpoint"] = s.as.URL + "/api/oauth/authorize?tenant=x" }
	if _, err := s.flow(io.Discard).Login(context.Background(), s.api.URL); err != nil {
		t.Fatal(err)
	}
	if s.authorize.Get("tenant") != "x" || s.authorize.Get("client_id") != ClientID {
		t.Errorf("authorize query %v", s.authorize)
	}
}

func TestLoginRefusesACallbackThatIsNotThisSignIns(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(q url.Values)
		want string
	}{
		"wrong state":   {func(q url.Values) { q.Set("state", "forged") }, "state"},
		"missing state": {func(q url.Values) { q.Del("state") }, "state"},
		"wrong iss":     {func(q url.Values) { q.Set("iss", "https://evil.example") }, `issuer "https://evil.example"`},
		"missing iss":   {func(q url.Values) { q.Del("iss") }, "no iss"},
		"no code":       {func(q url.Values) { q.Del("code") }, "neither a code nor an error"},
		// A forged error is refused for its state before it can end the sign-in as a denial.
		"denial with a wrong state": {func(q url.Values) {
			q.Set("state", "forged")
			q.Del("code")
			q.Set("error", "access_denied")
		}, "state"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAuthServer(t)
			s.callback = tc.edit
			_, err := s.flow(io.Discard).Login(context.Background(), s.api.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrDenied) {
				t.Fatalf("err = %v, want one naming %s", err, tc.want)
			}
			if len(s.tokenForms) != 0 {
				t.Errorf("a refused callback still exchanged its code")
			}
			if page := s.callbackPages[0]; !strings.HasPrefix(page, "400 text/plain") || !strings.Contains(page, "sign-in failed") {
				t.Errorf("callback page %q", page)
			}
		})
	}
}

func TestLoginEndsWhenTheBrowserDeniesAccess(t *testing.T) {
	s := newAuthServer(t)
	s.callback = func(q url.Values) {
		q.Del("code")
		q.Set("error", "access_denied")
		q.Set("error_description", "The user denied the request")
	}
	_, err := s.flow(io.Discard).Login(context.Background(), s.api.URL)
	if !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "The user denied the request") {
		t.Fatalf("err = %v, want ErrDenied with the description", err)
	}

	s = newAuthServer(t)
	s.callback = func(q url.Values) {
		q.Del("code")
		q.Set("error", "invalid_target")
	}
	_, err = s.flow(io.Discard).Login(context.Background(), s.api.URL)
	var oauthErr *Error
	if errors.Is(err, ErrDenied) || !errors.As(err, &oauthErr) || oauthErr.Code != "invalid_target" {
		t.Fatalf("err = %v, want the invalid_target error, not a denial", err)
	}
}

func TestLoginReportsATokenEndpointError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		edit   func(m map[string]any)
		want   string
	}{
		"invalid_grant":    {status: 400, body: `{"error":"invalid_grant","error_description":"code expired"}`, want: "invalid_grant: code expired"},
		"not JSON":         {status: 502, body: `<html>bad gateway</html>`, want: "answered 502 Bad Gateway"},
		"200 not JSON":     {status: 200, body: `nope`, want: "not JSON"},
		"no refresh token": {edit: func(m map[string]any) { delete(m, "refresh_token") }, want: "no refresh_token"},
		"no access token":  {edit: func(m map[string]any) { delete(m, "access_token") }, want: "no access_token"},
		"not Bearer":       {edit: func(m map[string]any) { m["token_type"] = "DPoP" }, want: `"DPoP", not Bearer`},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAuthServer(t)
			s.tokenStatus, s.tokenBody, s.tokenEdit = tc.status, tc.body, tc.edit
			_, err := s.flow(io.Discard).Login(context.Background(), s.api.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoginWaitsNoLongerThanItsTimeout(t *testing.T) {
	s := newAuthServer(t)
	f := s.flow(io.Discard)
	f.OpenBrowser, f.Timeout = nil, 50*time.Millisecond
	var out bytes.Buffer
	f.Out = &out
	_, err := f.Login(context.Background(), s.api.URL)
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(out.String(), "Open this address in a browser") || !strings.Contains(out.String(), "/api/oauth/authorize?") {
		t.Errorf("without a browser it printed %q, want the address", out.String())
	}
}

func TestLoginStopsWhenInterrupted(t *testing.T) {
	s := newAuthServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := s.flow(io.Discard)
	f.OpenBrowser = func(string) error { cancel(); return nil }
	_, err := f.Login(ctx, s.api.URL)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLoginPrintsTheAddressWhenTheBrowserFails(t *testing.T) {
	s := newAuthServer(t)
	f := s.flow(io.Discard)
	var out bytes.Buffer
	f.Out, f.Timeout = &out, 50*time.Millisecond
	f.OpenBrowser = func(string) error { return errors.New("no display") }
	_, _ = f.Login(context.Background(), s.api.URL)
	if !strings.Contains(out.String(), "could not be opened: no display") || !strings.Contains(out.String(), "Open this address") {
		t.Errorf("printed %q", out.String())
	}
}

func TestLoginTakesOneCallback(t *testing.T) {
	s := newAuthServer(t)
	f := s.flow(io.Discard)
	var redirect string
	open := f.OpenBrowser
	f.OpenBrowser = func(address string) error {
		if err := open(address); err != nil {
			return err
		}
		redirect = s.redirectURI
		// A second visit, as a reload of the callback page makes, is told the sign-in is over.
		res, err := http.Get(redirect + "?" + url.Values{"code": {"code-2"}, "state": {s.authorize.Get("state")}, "iss": {s.as.URL}}.Encode()) //nolint:gosec,noctx // the test's own listener
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusConflict {
			t.Errorf("a second callback answered %d, want 409", res.StatusCode)
		}
		return nil
	}
	cred, err := f.Login(context.Background(), s.api.URL)
	if err != nil || cred.AccessToken != "cf_oat_1" {
		t.Fatalf("credential %v, err %v", cred, err)
	}
}

func TestLoginFailsWithoutRandomness(t *testing.T) {
	s := newAuthServer(t)
	f := s.flow(io.Discard)
	f.Rand = strings.NewReader("short")
	if _, err := f.Login(context.Background(), s.api.URL); err == nil || !strings.Contains(err.Error(), "randomness") {
		t.Fatalf("err = %v", err)
	}
	f.Rand = strings.NewReader(strings.Repeat("x", 40))
	if _, err := f.Login(context.Background(), s.api.URL); err == nil || !strings.Contains(err.Error(), "randomness") {
		t.Fatalf("a verifier without randomness: err = %v", err)
	}
}

func TestNewFlowUsesTheSystemsClockAndRandomness(t *testing.T) {
	f := NewFlow(nil, io.Discard, time.Minute)
	if f.HTTP == nil || f.Now == nil || f.Rand != rand.Reader || f.Timeout != time.Minute || f.OpenBrowser != nil {
		t.Errorf("flow %+v", f)
	}
	if f.HTTP.Timeout != HTTPTimeout || f.HTTP.CheckRedirect == nil {
		t.Errorf("the client is unbounded or follows redirects")
	}
}

func TestDiscoverRefusesMetadataThatDoesNotMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		resource func(s *authServer, m map[string]any)
		server   func(s *authServer, m map[string]any)
		want     string
	}{
		"another resource":         {resource: func(_ *authServer, m map[string]any) { m["resource"] = "https://api.evil.example" }, want: `resource "https://api.evil.example"`},
		"no authorization server":  {resource: func(_ *authServer, m map[string]any) { m["authorization_servers"] = []string{} }, want: "no authorization server"},
		"plain http issuer":        {resource: func(_ *authServer, m map[string]any) { m["authorization_servers"] = []string{"http://as.example"} }, want: "must use https"},
		"another issuer":           {server: func(_ *authServer, m map[string]any) { m["issuer"] = "https://evil.example" }, want: `issuer "https://evil.example"`},
		"issuer with a slash":      {server: func(s *authServer, m map[string]any) { m["issuer"] = s.as.URL + "/" }, want: "names the issuer"},
		"plain PKCE only":          {server: func(_ *authServer, m map[string]any) { m["code_challenge_methods_supported"] = []string{"plain"} }, want: "S256"},
		"no PKCE":                  {server: func(_ *authServer, m map[string]any) { delete(m, "code_challenge_methods_supported") }, want: "S256"},
		"no token endpoint":        {server: func(_ *authServer, m map[string]any) { delete(m, "token_endpoint") }, want: "no token_endpoint"},
		"plain http authorization": {server: func(_ *authServer, m map[string]any) { m["authorization_endpoint"] = "http://as.example/authorize" }, want: "authorization_endpoint"},
		"plain http revocation":    {server: func(_ *authServer, m map[string]any) { m["revocation_endpoint"] = "http://as.example/revoke" }, want: "revocation_endpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAuthServer(t)
			if tc.resource != nil {
				s.resourceMeta = func(m map[string]any) { tc.resource(s, m) }
			}
			if tc.server != nil {
				s.serverMeta = func(m map[string]any) { tc.server(s, m) }
			}
			_, err := Discover(context.Background(), NewHTTPClient(), s.api.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestDiscoverWithoutARevocationEndpoint(t *testing.T) {
	s := newAuthServer(t)
	s.serverMeta = func(m map[string]any) { delete(m, "revocation_endpoint") }
	md, err := Discover(context.Background(), NewHTTPClient(), s.api.URL)
	if err != nil || md.RevocationEndpoint != "" {
		t.Fatalf("metadata %+v, err %v", md, err)
	}
}

func TestDiscoverReadsBoundedJSONAndFollowsNoRedirect(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"missing": {func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "answered 404 Not Found"},
		"not JSON": {func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") },
			"not JSON"},
		"too large": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"resource":"`+strings.Repeat("a", maxResponseBytes)+`"}`)
		}, "larger than"},
		"redirected": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/", http.StatusFound)
		},
			"answered 302 Found"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := Discover(context.Background(), NewHTTPClient(), srv.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := Discover(context.Background(), NewHTTPClient(), "http://127.0.0.1:1"); err == nil {
		t.Errorf("an API that does not answer: no error")
	}
	if _, err := Discover(context.Background(), NewHTTPClient(), "http://bad host"); err == nil {
		t.Errorf("an unparseable resource: no error")
	}
}

func TestWellKnownKeepsTheBasePath(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.costfluent.com":         "https://api.costfluent.com/.well-known/oauth-protected-resource",
		"https://api.costfluent.com/":        "https://api.costfluent.com/.well-known/oauth-protected-resource",
		"http://localhost:5100/public":       "http://localhost:5100/.well-known/oauth-protected-resource/public",
		"https://app.costfluent.com?x=1#top": "https://app.costfluent.com/.well-known/oauth-protected-resource",
		"%zz":                                "%zz",
	} {
		if got := wellKnown(base, "oauth-protected-resource"); got != want {
			t.Errorf("%s: %s, want %s", base, got, want)
		}
	}
}

func TestChallengeIsRFC7636AppendixB(t *testing.T) {
	if got := challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge %s", got)
	}
}

func TestErrorReadsWithAndWithoutADescription(t *testing.T) {
	if got := (&Error{Code: "invalid_grant"}).Error(); got != "invalid_grant" {
		t.Errorf("%q", got)
	}
	if got := (&Error{Code: "invalid_grant", Description: "spent"}).Error(); got != "invalid_grant: spent" {
		t.Errorf("%q", got)
	}
}

func TestExpiresAtWithoutALifetimeIsNow(t *testing.T) {
	if got := (&tokenResponse{}).expiresAt(clock); !got.Equal(clock) {
		t.Errorf("%s", got)
	}
}
