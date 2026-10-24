package config

import (
	"fmt"
	"strings"
	"testing"
)

const token = "cfk_test_secret_value"

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadDefaultsToThePublicAPI(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvAPIToken: token}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken != token || cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("loaded %+v", cfg)
	}
}

func TestLoadRequiresAToken(t *testing.T) {
	for _, value := range []string{"", "   "} {
		if _, err := Load(env(map[string]string{EnvAPIToken: value})); err == nil || !strings.Contains(err.Error(), EnvAPIToken) {
			t.Errorf("token %q: err = %v, want one naming %s", value, err, EnvAPIToken)
		}
	}
}

func TestLoadAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.dev.costfluent.com/": "https://api.dev.costfluent.com",
		"http://localhost:5100":           "http://localhost:5100",
		"http://127.0.0.1:5100":           "http://127.0.0.1:5100",
		"http://[::1]:5100":               "http://[::1]:5100",
	} {
		cfg, err := Load(env(map[string]string{EnvAPIToken: token, EnvBaseURL: raw}))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if cfg.BaseURL != want {
			t.Errorf("%s: base URL %q, want %q", raw, cfg.BaseURL, want)
		}
	}
}

func TestLoadRefusesABaseURLThatWouldSendTheTokenInTheClear(t *testing.T) {
	for _, raw := range []string{"http://api.costfluent.com", "ftp://api.costfluent.com", "api.costfluent.com", "https://", "http://localhost.evil.example"} {
		_, err := Load(env(map[string]string{EnvAPIToken: token, EnvBaseURL: raw}))
		if err == nil {
			t.Errorf("%s: accepted", raw)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%s: the error discloses the token: %v", raw, err)
		}
	}
}

func TestConfigNeverPrintsTheToken(t *testing.T) {
	cfg := Config{APIToken: token, BaseURL: DefaultBaseURL}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), cfg.String()} {
		if strings.Contains(s, token) {
			t.Errorf("printed %q", s)
		}
	}
}
