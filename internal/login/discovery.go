package login

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/costfluent/costfluent-mcp/internal/config"
)

// Metadata is what discovery found: the authorization server the API trusts and its endpoints.
type Metadata struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	// RevocationEndpoint is empty when the server names none; logout then only forgets the sign-in.
	RevocationEndpoint string
}

// Discover reads which authorization server the API at resource trusts, from its protected
// resource metadata (RFC 9728), and that server's endpoints, from its metadata (RFC 8414). Each
// document must name exactly the URL it was read for, so a document served for another resource or
// issuer is refused, and no endpoint path is assumed.
func Discover(ctx context.Context, hc *http.Client, resource string) (*Metadata, error) {
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, wellKnown(resource, "oauth-protected-resource"), &prm); err != nil {
		return nil, fmt.Errorf("reading the API's sign-in metadata: %w", err)
	}
	if prm.Resource != resource {
		return nil, fmt.Errorf("the API's metadata is for the resource %q, not %q", prm.Resource, resource)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("the API's metadata names no authorization server")
	}
	issuer := prm.AuthorizationServers[0]
	if err := config.CheckURL(issuer); err != nil {
		return nil, fmt.Errorf("the API's authorization server: %w", err)
	}

	var asm struct {
		Issuer                        string   `json:"issuer"`
		AuthorizationEndpoint         string   `json:"authorization_endpoint"`
		TokenEndpoint                 string   `json:"token_endpoint"`
		RevocationEndpoint            string   `json:"revocation_endpoint"`
		CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	}
	if err := getJSON(ctx, hc, wellKnown(issuer, "oauth-authorization-server"), &asm); err != nil {
		return nil, fmt.Errorf("reading the authorization server's metadata: %w", err)
	}
	if asm.Issuer != issuer {
		return nil, fmt.Errorf("the authorization server's metadata names the issuer %q, not %q", asm.Issuer, issuer)
	}
	if !slices.Contains(asm.CodeChallengeMethodsSupported, "S256") {
		return nil, errors.New("the authorization server does not support PKCE with S256")
	}
	for name, endpoint := range map[string]string{"authorization_endpoint": asm.AuthorizationEndpoint, "token_endpoint": asm.TokenEndpoint} {
		if endpoint == "" {
			return nil, fmt.Errorf("the authorization server's metadata names no %s", name)
		}
		if err := config.CheckURL(endpoint); err != nil {
			return nil, fmt.Errorf("the authorization server's %s: %w", name, err)
		}
	}
	if asm.RevocationEndpoint != "" {
		if err := config.CheckURL(asm.RevocationEndpoint); err != nil {
			return nil, fmt.Errorf("the authorization server's revocation_endpoint: %w", err)
		}
	}
	return &Metadata{
		Issuer:                issuer,
		AuthorizationEndpoint: asm.AuthorizationEndpoint,
		TokenEndpoint:         asm.TokenEndpoint,
		RevocationEndpoint:    asm.RevocationEndpoint,
	}, nil
}

// wellKnown is the metadata address for base: the well-known path between its host and its own
// path (RFC 8414 section 3.1, RFC 9728 section 3.1), so a base with a path keeps it.
func wellKnown(base, name string) string {
	u, err := url.Parse(base)
	if err != nil {
		// The callers checked base with config.CheckURL, which parses it.
		return base
	}
	u.Path = "/.well-known/" + name + strings.TrimSuffix(u.Path, "/")
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}
