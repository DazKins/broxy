package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultForPathUsesStateDirForCustomConfigPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "custom", "config.json")
	cfg, err := DefaultForPath(configPath)
	if err != nil {
		t.Fatalf("DefaultForPath() error = %v", err)
	}

	baseDir := filepath.Dir(configPath)
	if cfg.ConfigDir != baseDir {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, baseDir)
	}
	if cfg.StateDir != filepath.Join(baseDir, "state") {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DBPath != filepath.Join(baseDir, "state", "broxy.db") {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
}

func TestDefaultForPathUsesBearerModeWhenBearerEnvSet(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "env-token")

	configPath := filepath.Join(t.TempDir(), "custom", "config.json")
	cfg, err := DefaultForPath(configPath)
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

func TestLoadMigratesLegacyDataDirAliasForCustomConfigPath(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "app")
	configPath := filepath.Join(baseDir, "config.json")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "` + baseDir + `",
  "data_dir": "` + filepath.Join(baseDir, "data") + `",
  "db_path": "` + filepath.Join(baseDir, "data", "broxy.db") + `",
  "pricing_path": "` + filepath.Join(baseDir, "pricing.json") + `",
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

	if cfg.StateDir != filepath.Join(baseDir, "state") {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DBPath != filepath.Join(baseDir, "state", "broxy.db") {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
}

func TestLoadBearerTokenImpliesBearerMode(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")

	baseDir := filepath.Join(t.TempDir(), "app")
	configPath := filepath.Join(baseDir, "config.json")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "` + baseDir + `",
  "state_dir": "` + filepath.Join(baseDir, "state") + `",
  "db_path": "` + filepath.Join(baseDir, "state", "broxy.db") + `",
  "pricing_path": "` + filepath.Join(baseDir, "pricing.json") + `",
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
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")

	baseDir := filepath.Join(t.TempDir(), "app")
	configPath := filepath.Join(baseDir, "config.json")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := `{
  "listen_addr": "127.0.0.1:8080",
  "config_dir": "` + baseDir + `",
  "state_dir": "` + filepath.Join(baseDir, "state") + `",
  "db_path": "` + filepath.Join(baseDir, "state", "broxy.db") + `",
  "pricing_path": "` + filepath.Join(baseDir, "pricing.json") + `",
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
	baseDir := filepath.Join(t.TempDir(), "app")
	configPath := filepath.Join(baseDir, "config.json")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
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

func TestMigrateLegacyStateMovesDatabase(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "app")
	configPath := filepath.Join(baseDir, "config.json")
	cfg, err := DefaultForPath(configPath)
	if err != nil {
		t.Fatalf("DefaultForPath() error = %v", err)
	}

	legacyPaths, err := LegacyPathsForConfigPath(configPath)
	if err != nil {
		t.Fatalf("LegacyPathsForConfigPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyPaths.DBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(legacyPaths.DBPath, []byte("legacy"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := MigrateLegacyState(configPath, cfg); err != nil {
		t.Fatalf("MigrateLegacyState() error = %v", err)
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Fatalf("new db missing: %v", err)
	}
	if _, err := os.Stat(legacyPaths.DBPath); !os.IsNotExist(err) {
		t.Fatalf("legacy db still exists, stat err=%v", err)
	}
}
