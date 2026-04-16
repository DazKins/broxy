package config

import (
	"os"
	"path/filepath"
	"testing"
)

func testBroxyRoot(t *testing.T) string {
	t.Helper()

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	return filepath.Join(homeDir, ".broxy")
}

func TestDefaultForPathUsesBroxyRoot(t *testing.T) {
	root := testBroxyRoot(t)

	cfg, err := DefaultForPath("")
	if err != nil {
		t.Fatalf("DefaultForPath() error = %v", err)
	}

	if cfg.ConfigDir != root {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, root)
	}
	if cfg.StateDir != root {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DBPath != filepath.Join(root, "broxy.db") {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.PricingPath != filepath.Join(root, "pricing.json") {
		t.Fatalf("PricingPath = %q", cfg.PricingPath)
	}
}

func TestConfigPathRejectsOutsideBroxyRoot(t *testing.T) {
	testBroxyRoot(t)

	if _, err := ConfigPath(filepath.Join(t.TempDir(), "config.json")); err == nil {
		t.Fatalf("ConfigPath() error = nil, want outside root error")
	}
}

func TestDefaultForPathUsesBearerModeWhenBearerEnvSet(t *testing.T) {
	testBroxyRoot(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "env-token")

	cfg, err := DefaultForPath("")
	if err != nil {
		t.Fatalf("DefaultForPath() error = %v", err)
	}

	if cfg.Upstream.Mode != UpstreamAuthBearer {
		t.Fatalf("Upstream.Mode = %q", cfg.Upstream.Mode)
	}
	if cfg.Upstream.BearerToken != "env-token" {
		t.Fatalf("Upstream.BearerToken = %q", cfg.Upstream.BearerToken)
	}
}

func TestLoadIgnoresStoredPathFields(t *testing.T) {
	root := testBroxyRoot(t)
	configPath := filepath.Join(root, "config.json")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "/tmp/other-broxy",
  "state_dir": "/tmp/other-broxy/state",
  "db_path": "/tmp/other-broxy/state/broxy.db",
  "pricing_path": "/tmp/other-broxy/pricing.json",
  "session_secret": "secret",
  "upstream": {
    "mode": "aws",
    "region": "us-east-1"
  }
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ConfigDir != root {
		t.Fatalf("ConfigDir = %q", cfg.ConfigDir)
	}
	if cfg.StateDir != root {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DBPath != filepath.Join(root, "broxy.db") {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.PricingPath != filepath.Join(root, "pricing.json") {
		t.Fatalf("PricingPath = %q", cfg.PricingPath)
	}
}

func TestLoadBearerTokenImpliesBearerMode(t *testing.T) {
	root := testBroxyRoot(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")

	configPath := filepath.Join(root, "config.json")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "` + root + `",
  "state_dir": "` + root + `",
  "db_path": "` + filepath.Join(root, "broxy.db") + `",
  "pricing_path": "` + filepath.Join(root, "pricing.json") + `",
  "session_secret": "secret",
  "upstream": {
    "mode": "aws",
    "region": "us-east-1",
    "bearer_token": "file-token"
  }
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Upstream.Mode != UpstreamAuthBearer {
		t.Fatalf("Upstream.Mode = %q", cfg.Upstream.Mode)
	}
	if cfg.Upstream.BearerToken != "file-token" {
		t.Fatalf("Upstream.BearerToken = %q", cfg.Upstream.BearerToken)
	}
}

func TestLoadEnvBlockOverridesKnownEnvironmentSettings(t *testing.T) {
	root := testBroxyRoot(t)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")

	configPath := filepath.Join(root, "config.json")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "` + root + `",
  "state_dir": "` + root + `",
  "db_path": "` + filepath.Join(root, "broxy.db") + `",
  "pricing_path": "` + filepath.Join(root, "pricing.json") + `",
  "session_secret": "secret",
  "upstream": {
    "mode": "aws",
    "region": "us-east-1"
  },
  "env": {
    "AWS_PROFILE": "sso-prod",
    "AWS_REGION": "eu-west-1",
    "AWS_BEARER_TOKEN_BEDROCK": "token"
  }
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Env["AWS_PROFILE"] != "sso-prod" {
		t.Fatalf("Env[AWS_PROFILE] = %q", cfg.Env["AWS_PROFILE"])
	}
	if cfg.Upstream.Region != "eu-west-1" {
		t.Fatalf("Upstream.Region = %q", cfg.Upstream.Region)
	}
	if cfg.Upstream.Profile != "sso-prod" {
		t.Fatalf("Upstream.Profile = %q", cfg.Upstream.Profile)
	}
	if cfg.Upstream.Mode != UpstreamAuthBearer {
		t.Fatalf("Upstream.Mode = %q", cfg.Upstream.Mode)
	}
	if cfg.Upstream.BearerToken != "token" {
		t.Fatalf("Upstream.BearerToken = %q", cfg.Upstream.BearerToken)
	}
}

func TestApplyEnvSetsArbitraryValues(t *testing.T) {
	t.Setenv("BROXY_TEST_ENV", "original")

	if err := ApplyEnv(map[string]string{
		"BROXY_TEST_ENV":       "from-config",
		"BROXY_TEST_EMPTY_ENV": "",
	}); err != nil {
		t.Fatalf("ApplyEnv() error = %v", err)
	}

	if got := os.Getenv("BROXY_TEST_ENV"); got != "from-config" {
		t.Fatalf("BROXY_TEST_ENV = %q", got)
	}
	if got, ok := os.LookupEnv("BROXY_TEST_EMPTY_ENV"); !ok || got != "" {
		t.Fatalf("BROXY_TEST_EMPTY_ENV = %q, ok=%t", got, ok)
	}
}

func TestLoadRejectsInvalidEnvKey(t *testing.T) {
	root := testBroxyRoot(t)
	configPath := filepath.Join(root, "config.json")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "env": {
    "BAD=KEY": "value"
  }
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatalf("Load() error = nil, want invalid env key")
	}
}
