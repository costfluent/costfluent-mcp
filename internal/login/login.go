// Package login signs the costfluent command in to the Public API through the browser, as OAuth 2.1
// has a native app do it: it reads the API's protected resource metadata (RFC 9728) and its
// authorization server's metadata (RFC 8414), opens the authorization request with PKCE (RFC 7636)
// and the API as its resource (RFC 8707), takes the code on a loopback redirect (RFC 8252), checks
// the issuer the callback names (RFC 9207), exchanges the code, refreshes the access token, and
// revokes the refresh token at logout (RFC 7009). Every HTTP call of the sign-in lives here; the
// tools still reach the API only through the SDK copy.
package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The client the authorization server knows the costfluent command as: public, so it holds no
// secret, and allowed any port on the loopback address.
const (
	ClientID = "costfluent-cli"
	// Scope asks for reading and for a refresh token, so one sign-in outlives the hour an access
	// token lives.
	Scope = "read offline_access"
)

// DefaultTimeout is how long Login waits for the browser to come back by default: long enough to
// sign in, pass a second factor and approve, short enough that a forgotten terminal frees its port.
const DefaultTimeout = 5 * time.Minute

// callbackPath is the redirect URI's path on the loopback listener.
const callbackPath = "/callback"

// Errors a caller tells apart.
var (
	// ErrDenied is a sign-in the user, or the authorization server, refused in the browser.
	ErrDenied = errors.New("access was denied in the browser")
	// ErrTimeout is a browser that did not come back within the wait.
	ErrTimeout = errors.New("the browser did not come back in time")
)

// Flow is one way of signing in: its HTTP client, clock, randomness, browser and where it tells the
// user what to do. Each is a field so a test can replace it.
type Flow struct {
	HTTP *http.Client
	Now  func() time.Time
	Rand io.Reader
	// OpenBrowser opens an address in the user's browser; nil when the user asked for none.
	OpenBrowser func(address string) error
	// Out is where the address to open is printed: the terminal, never stdout's data.
	Out io.Writer
	// Timeout bounds the wait for the browser's callback; DefaultTimeout when zero.
	Timeout time.Duration
}

// NewFlow is the flow the command runs: the system's randomness and clock, out to w.
func NewFlow(openBrowser func(string) error, w io.Writer, timeout time.Duration) Flow {
	return Flow{HTTP: NewHTTPClient(), Now: time.Now, Rand: rand.Reader, OpenBrowser: openBrowser, Out: w, Timeout: timeout}
}

// Login signs in to the API at resource, its base URL, and returns the credential to keep.
func (f Flow) Login(ctx context.Context, resource string) (*Credential, error) {
	md, err := Discover(ctx, f.HTTP, resource)
	if err != nil {
		return nil, err
	}
	state, err := randomString(f.Rand)
	if err != nil {
		return nil, err
	}
	verifier, err := randomString(f.Rand)
	if err != nil {
		return nil, err
	}
	// RFC 8252 section 7.3: the IP literal, not localhost, which a resolver could point elsewhere.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("opening a port on 127.0.0.1 for the browser to return to: %w", err)
	}
	redirectURI := "http://" + ln.Addr().String() + callbackPath
	address, err := authorizeURL(md, resource, redirectURI, state, challenge(verifier))
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	results := make(chan callbackResult, 1)
	srv := &http.Server{Handler: callbackHandler(md.Issuer, state, results), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	f.prompt(address)

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var code string
	select {
	case r := <-results:
		if r.err != nil {
			return nil, r.err
		}
		code = r.code
	case <-wait.Done():
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("sign-in interrupted: %w", err)
		}
		return nil, fmt.Errorf("%w: no sign-in arrived within %s; run the command again, or raise --timeout", ErrTimeout, timeout)
	}

	tok, err := requestToken(ctx, f.HTTP, md.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {ClientID},
		"code_verifier": {verifier},
		"resource":      {resource},
	})
	if err != nil {
		return nil, fmt.Errorf("exchanging the sign-in for a token: %w", err)
	}
	return &Credential{
		Resource:           resource,
		Issuer:             md.Issuer,
		TokenEndpoint:      md.TokenEndpoint,
		RevocationEndpoint: md.RevocationEndpoint,
		AccessToken:        tok.AccessToken,
		ExpiresAt:          tok.expiresAt(f.Now()),
		RefreshToken:       tok.RefreshToken,
	}, nil
}

// prompt tells the user where to sign in. The address is always printed, because a browser opened
// on another display, or by an xdg-open that only pretends, shows the user nothing.
func (f Flow) prompt(address string) {
	opened := false
	if f.OpenBrowser != nil {
		if err := f.OpenBrowser(address); err != nil {
			_, _ = fmt.Fprintf(f.Out, "The browser could not be opened: %v\n", err)
		} else {
			opened = true
		}
	}
	if opened {
		_, _ = fmt.Fprintf(f.Out, "Your browser has been opened to sign in to Costfluent. If it shows nothing, open:\n\n  %s\n\n", address)
	} else {
		_, _ = fmt.Fprintf(f.Out, "Open this address in a browser to sign in to Costfluent:\n\n  %s\n\n", address)
	}
	_, _ = fmt.Fprintln(f.Out, "Waiting for the browser...")
}

// authorizeURL is the authorization request, its parameters added to any the endpoint carries.
func authorizeURL(md *Metadata, resource, redirectURI, state, codeChallenge string) (string, error) {
	u, err := url.Parse(md.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("authorization_endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scope)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// callbackResult is what the browser brought back: a code, or why there is none.
type callbackResult struct {
	code string
	err  error
}

// callbackHandler takes the first request to the callback path and sends its result; every later
// one is told the sign-in is over. The page is plain text, so nothing the request carries can
// become markup.
func callbackHandler(issuer, state string, results chan<- callbackResult) http.Handler {
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		first := false
		once.Do(func() { first = true })
		if !first {
			page(w, http.StatusConflict, "This sign-in has already finished. Return to the terminal.")
			return
		}
		res := readCallback(r.URL.Query(), issuer, state)
		results <- res
		if res.err != nil {
			page(w, http.StatusBadRequest, "Costfluent sign-in failed: "+res.err.Error()+"\n\nReturn to the terminal.")
			return
		}
		page(w, http.StatusOK, "Signed in to Costfluent. You can close this tab and return to the terminal.")
	})
	return mux
}

// readCallback checks a callback in the order that keeps a forged one from steering the sign-in:
// the state proves this login sent the request, the issuer that the expected server answered
// (RFC 9207, which the Costfluent authorization server always follows), and only then does an
// error or a code count.
func readCallback(q url.Values, issuer, state string) callbackResult {
	if got := q.Get("state"); got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
		return callbackResult{err: errors.New("the browser returned a state this sign-in did not send; start the sign-in again")}
	}
	switch got := q.Get("iss"); {
	case got == "":
		return callbackResult{err: errors.New("the browser returned no iss, which the authorization server always sends; start the sign-in again")}
	case got != issuer:
		return callbackResult{err: fmt.Errorf("the browser returned the issuer %q, not %q; start the sign-in again", got, issuer)}
	}
	if code := q.Get("error"); code != "" {
		e := &Error{Code: code, Description: q.Get("error_description")}
		if code == "access_denied" {
			return callbackResult{err: fmt.Errorf("%w (%w)", ErrDenied, e)}
		}
		return callbackResult{err: fmt.Errorf("the authorization server refused the sign-in: %w", e)}
	}
	code := q.Get("code")
	if code == "" {
		return callbackResult{err: errors.New("the browser returned neither a code nor an error")}
	}
	return callbackResult{code: code}
}

func page(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)+1))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text+"\n")
}

// randomString is 32 random bytes, base64url without padding: 43 characters, a PKCE verifier's
// length range (RFC 7636 section 4.1) and a state no one guesses.
func randomString(r io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challenge is a verifier's S256 code challenge (RFC 7636 section 4.2).
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
