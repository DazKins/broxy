package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCommandJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "broxy", "config.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd := NewRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"--config", configPath, "init", "--non-interactive", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v stderr=%s", err, stderr.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v body=%s", err, stdout.String())
	}
	if payload["config_path"] != configPath {
		t.Fatalf("config_path = %q, want %q", payload["config_path"], configPath)
	}
	if payload["state_dir"] != filepath.Join(filepath.Dir(configPath), "state") {
		t.Fatalf("state_dir = %q", payload["state_dir"])
	}
	if payload["admin_password"] == "" {
		t.Fatalf("admin_password should be set")
	}
}

func TestConfigPathCommandJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "broxy", "config.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd := NewRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"--config", configPath, "config", "path", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v stderr=%s", err, stderr.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v body=%s", err, stdout.String())
	}
	if payload["db_path"] != filepath.Join(filepath.Dir(configPath), "state", "broxy.db") {
		t.Fatalf("db_path = %q", payload["db_path"])
	}
}

func TestServiceInstallDryRun(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "broxy", "config.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd := NewRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"--config", configPath, "service", "install", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Service file: ") {
		t.Fatalf("dry-run output missing service file: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "serve") || !strings.Contains(stdout.String(), "--config") {
		t.Fatalf("dry-run output missing ExecStart/ProgramArguments: %s", stdout.String())
	}
}
