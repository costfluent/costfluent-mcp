// Package config reads the server's settings from its environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// The environment variables the server reads. The token variable deliberately differs from the
// Terraform provider's COSTFLUENT_API_KEY, so an assistant never inherits a pipeline's read-write
// key from a shared environment.
const (
	EnvAPIToken = "COSTFLUENT_API_TOKEN" //nolint:gosec // the variable's name, not a credential
	EnvBaseURL  = "COSTFLUENT_BASE_URL"
)

// DefaultBaseURL is the Public API the server calls unless EnvBaseURL names another.
const DefaultBaseURL = "https://api.costfluent.com"

// Config is what the server is started with.
type Config struct {
	APIToken string
	BaseURL  string
}

// String leaves the token out, so a Config printed by mistake does not disclose it.
func (c Config) String() string {
	return fmt.Sprintf("Config{BaseURL: %s}", c.BaseURL)
}

// Load reads the configuration through getenv, such as os.Getenv.
func Load(getenv func(string) string) (Config, error) {
	token := strings.TrimSpace(getenv(EnvAPIToken))
	if token == "" {
		return Config{}, errors.New(EnvAPIToken + " is not set: create an API token under Settings, API tokens, and set it in the environment the MCP server or the costfluent command runs in")
	}
	baseURL, err := BaseURL(getenv)
	if err != nil {
		return Config{}, err
	}
	return Config{APIToken: token, BaseURL: baseURL}, nil
}

// BaseURL reads the Public API's base URL through getenv, DefaultBaseURL when EnvBaseURL is unset,
// without a trailing slash. It needs no token, so costfluent login reads the API it signs in to here.
func BaseURL(getenv func(string) string) (string, error) {
	baseURL := strings.TrimSpace(getenv(EnvBaseURL))
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if err := CheckURL(baseURL); err != nil {
		return "", fmt.Errorf("%s: %w", EnvBaseURL, err)
	}
	return baseURL, nil
}

// CheckURL refuses a URL that would send a token in the clear, except to the local machine, where a
// development stack listens: the API's base URL, and the authorization server's endpoints the
// costfluent login sends codes and tokens to.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host; write it as https://<host>", raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()):
		return nil
	}
	return fmt.Errorf("%q must use https, because a token travels with every request to it; plain http is allowed only for localhost, 127.0.0.1 and ::1", raw)
}
