package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPTimeout bounds each request to the API's metadata and the authorization server.
const HTTPTimeout = 30 * time.Second

// maxResponseBytes bounds every answer read: metadata and token responses are a few hundred bytes.
const maxResponseBytes = 64 << 10

// NewHTTPClient is the client every call here makes: bounded per request, and following no
// redirect, because a 307 or 308 would resend a token request's body, its code, verifier or refresh
// token, to wherever the redirect points.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       HTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Error is an OAuth error response (RFC 6749 section 5.2, RFC 7009 section 2.2.1), or an error a
// callback carried (RFC 6749 section 4.1.2.1).
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *Error) Error() string {
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

// tokenResponse is a successful token response (RFC 6749 section 5.1).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// expiresAt is when the access token stops working; now when the response named no lifetime, so
// the next use refreshes it rather than guess one.
func (t *tokenResponse) expiresAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return now
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// requestToken posts a token request and checks the answer is one this client can keep.
func requestToken(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	status, body, err := postForm(ctx, hc, endpoint, form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, answerError(status, body)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("the token response is not JSON: %w", err)
	}
	switch {
	case !strings.EqualFold(tok.TokenType, "Bearer"):
		return nil, fmt.Errorf("the token response's token_type is %q, not Bearer", tok.TokenType)
	case tok.AccessToken == "":
		return nil, errors.New("the token response carries no access_token")
	case tok.RefreshToken == "":
		return nil, errors.New("the token response carries no refresh_token, so the sign-in would end within the hour")
	}
	return &tok, nil
}

// answerError is a refused request's error: the OAuth error its body carries, else its status.
func answerError(status int, body []byte) error {
	var e Error
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		return &e
	}
	return fmt.Errorf("the authorization server answered %d %s", status, http.StatusText(status))
}

// postForm posts form to endpoint and returns the answer's status and body.
func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return do(hc, req)
}

// getJSON reads the JSON document at address into v; anything but 200 is an error.
func getJSON(ctx context.Context, hc *http.Client, address string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	status, body, err := do(hc, req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s answered %d %s", address, status, http.StatusText(status))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s is not JSON: %w", address, err)
	}
	return nil
}

// do sends req and reads at most maxResponseBytes of its answer.
func do(hc *http.Client, req *http.Request) (int, []byte, error) {
	res, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer of %s %s: %w", req.Method, req.URL, err)
	}
	if len(body) > maxResponseBytes {
		return 0, nil, fmt.Errorf("the answer of %s %s is larger than %d bytes", req.Method, req.URL, maxResponseBytes)
	}
	return res.StatusCode, body, nil
}
