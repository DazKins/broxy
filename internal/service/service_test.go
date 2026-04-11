package service

import (
	"path/filepath"
	"strings"
	"testing"

	cfgpkg "github.com/personal/broxy/internal/config"
)

func TestRenderLinuxService(t *testing.T) {
	cfg := &cfgpkg.Config{StateDir: "/tmp/broxy-state"}
	def, err := NewDefinition(
		TargetLinux,
		cfg,
		"/tmp/broxy-config/config.json",
		"/tmp/broxy",
		map[string]string{"HTTP_PROXY": "http://127.0.0.1:7890"},
	)
	if err != nil {
		t.Fatalf("NewDefinition() error = %v", err)
	}

	body, err := Render(def)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(body, "ExecStart=\"/tmp/broxy\" serve --config \"/tmp/broxy-config/config.json\"") {
		t.Fatalf("linux unit missing ExecStart: %s", body)
	}
	if !strings.Contains(body, "Environment=\"HTTP_PROXY=http://127.0.0.1:7890\"") {
		t.Fatalf("linux unit missing environment: %s", body)
	}
}

func TestRenderDarwinService(t *testing.T) {
	cfg := &cfgpkg.Config{StateDir: filepath.Join("/Users/test", "Library", "Application Support", "broxy")}
	def, err := NewDefinition(
		TargetDarwin,
		cfg,
		filepath.Join(cfg.StateDir, "config.json"),
		"/Users/test/.local/bin/broxy",
		nil,
	)
	if err != nil {
		t.Fatalf("NewDefinition() error = %v", err)
	}

	body, err := Render(def)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(body, "<string>com.broxy.agent</string>") {
		t.Fatalf("darwin plist missing label: %s", body)
	}
	if !strings.Contains(body, "<string>/Users/test/.local/bin/broxy</string>") {
		t.Fatalf("darwin plist missing binary path: %s", body)
	}
	if !strings.Contains(body, "<string>--config</string>") {
		t.Fatalf("darwin plist missing config args: %s", body)
	}
}

func TestCapturedEnvironmentIncludesLogLevel(t *testing.T) {
	t.Setenv("BROXY_LOG_LEVEL", "debug")

	env := CapturedEnvironment()
	if env["BROXY_LOG_LEVEL"] != "debug" {
		t.Fatalf("CapturedEnvironment() missing BROXY_LOG_LEVEL, got %#v", env)
	}
}

func TestCapturedEnvironmentIncludesBearerToken(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "token")

	env := CapturedEnvironment()
	if env["AWS_BEARER_TOKEN_BEDROCK"] != "token" {
		t.Fatalf("CapturedEnvironment() missing AWS_BEARER_TOKEN_BEDROCK, got %#v", env)
	}
}
