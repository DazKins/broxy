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
