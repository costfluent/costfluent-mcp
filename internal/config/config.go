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
	cfg := Config{
		APIToken: strings.TrimSpace(getenv(EnvAPIToken)),
		BaseURL:  strings.TrimSpace(getenv(EnvBaseURL)),
	}
	if cfg.APIToken == "" {
		return Config{}, errors.New(EnvAPIToken + " is not set: create an API token under Settings, API tokens, and pass it in the server's environment")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvBaseURL, err)
	}
	return cfg, nil
}

// checkBaseURL refuses a base URL that would send the token in the clear, except to the local
// machine, where a development stack listens.
func checkBaseURL(raw string) error {
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
	return fmt.Errorf("%q must use https, because the API token travels with every request; plain http is allowed only for localhost, 127.0.0.1 and ::1", raw)
}
