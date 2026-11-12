package login

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Credential is one sign-in as it is kept: the API it is for, where to refresh and revoke it, and
// its tokens. The endpoints are kept so a refresh needs no discovery; they came from a discovery
// that checked them.
type Credential struct {
	Resource           string    `json:"resource"`
	Issuer             string    `json:"issuer"`
	TokenEndpoint      string    `json:"token_endpoint"`
	RevocationEndpoint string    `json:"revocation_endpoint,omitempty"`
	AccessToken        string    `json:"access_token"`
	ExpiresAt          time.Time `json:"expires_at"`
	RefreshToken       string    `json:"refresh_token"`
}

// String leaves both tokens out, so a credential printed by mistake discloses neither.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{Resource: %s, Issuer: %s, ExpiresAt: %s}", c.Resource, c.Issuer, c.ExpiresAt.Format(time.RFC3339))
}

// GoString is String, for %#v.
func (c Credential) GoString() string { return c.String() }

// expirySkew is how long before its expiry an access token is refreshed, so one that is valid when
// read does not expire on its way to the API.
const expirySkew = time.Minute

// ErrRefused is a refresh the authorization server refused: the sign-in has ended, by its lifetime,
// a revocation, or a refresh token spent elsewhere, and only a new sign-in helps.
var ErrRefused = errors.New("the authorization server refused the stored sign-in")

// ErrNoRevocation is an authorization server that names no revocation endpoint.
var ErrNoRevocation = errors.New("the authorization server names no revocation endpoint")

// Refresh trades cred's refresh token for new tokens. The server rotates the refresh token on every
// use, so the old one is spent once this returns: the caller keeps the result before using it.
func Refresh(ctx context.Context, hc *http.Client, cred *Credential, now time.Time) (*Credential, error) {
	tok, err := requestToken(ctx, hc, cred.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
		"client_id":     {ClientID},
		"resource":      {cred.Resource},
	})
	var oauthErr *Error
	if errors.As(err, &oauthErr) {
		return nil, fmt.Errorf("%w (%w)", ErrRefused, oauthErr)
	}
	if err != nil {
		return nil, fmt.Errorf("refreshing the sign-in: %w", err)
	}
	next := *cred
	next.AccessToken = tok.AccessToken
	next.RefreshToken = tok.RefreshToken
	next.ExpiresAt = tok.expiresAt(now)
	return &next, nil
}

// Revoke revokes cred's refresh token (RFC 7009), which ends its grant at the server.
func Revoke(ctx context.Context, hc *http.Client, cred *Credential) error {
	if cred.RevocationEndpoint == "" {
		return ErrNoRevocation
	}
	status, body, err := postForm(ctx, hc, cred.RevocationEndpoint, url.Values{
		"token":           {cred.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {ClientID},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return answerError(status, body)
	}
	return nil
}

// Session hands out access tokens from the stored sign-ins, refreshing an expired one.
type Session struct {
	Stores *Stores
	HTTP   *http.Client
	Now    func() time.Time
}

// AccessToken returns an access token for resource: the stored one while it lives, else a fresh
// one, whose rotated refresh token is kept before the access token is returned, because the old
// one is spent the moment the server answers. Caching the access token for its hour, rather than
// refreshing on every run, keeps two commands run at once from racing to spend one refresh token,
// which the server answers by ending the sign-in once its grace has passed.
func (s *Session) AccessToken(ctx context.Context, resource string) (string, error) {
	cred, store, err := s.Stores.Load(ctx, resource)
	if err != nil {
		return "", err
	}
	now := s.Now()
	if cred.AccessToken != "" && now.Add(expirySkew).Before(cred.ExpiresAt) {
		return cred.AccessToken, nil
	}
	next, err := Refresh(ctx, s.HTTP, cred, now)
	if err != nil {
		return "", err
	}
	if err := store.Save(ctx, next); err != nil {
		return "", fmt.Errorf("keeping the refreshed sign-in in %s: %w; the previous one is spent, so sign in again", store.Where(), err)
	}
	return next.AccessToken, nil
}
