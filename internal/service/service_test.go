package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	cfg := &cfgpkg.Config{StateDir: filepath.Join("/Users/test", ".broxy")}
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

func TestResetLinuxStopsReinstallsAndStarts(t *testing.T) {
	tmpDir := t.TempDir()
	def := &Definition{
		Target:      TargetLinux,
		Label:       SystemdUnit,
		ServiceFile: filepath.Join(tmpDir, "broxy.service"),
		Executable:  "/tmp/broxy",
		ConfigPath:  "/tmp/broxy-config/config.json",
		StateDir:    tmpDir,
		StdoutPath:  filepath.Join(tmpDir, "stdout.log"),
		StderrPath:  filepath.Join(tmpDir, "stderr.log"),
	}
	if err := os.WriteFile(def.ServiceFile, []byte("old unit"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var calls []string
	originalRun := run
	run = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	defer func() {
		run = originalRun
	}()

	if err := Reset(def); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}

	want := []string{
		"systemctl --user stop broxy.service",
		"systemctl --user disable --now broxy.service",
		"systemctl --user daemon-reload",
		"systemctl --user daemon-reload",
		"systemctl --user enable broxy.service",
		"systemctl --user start broxy.service",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
	body, err := os.ReadFile(def.ServiceFile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(body), "ExecStart=\"/tmp/broxy\" serve --config \"/tmp/broxy-config/config.json\"") {
		t.Fatalf("reset did not install updated unit: %s", string(body))
	}
}

func TestResetStopsOnStopError(t *testing.T) {
	stopErr := errors.New("stop failed")
	originalRun := run
	run = func(name string, args ...string) error {
		return stopErr
	}
	defer func() {
		run = originalRun
	}()

	err := Reset(&Definition{Target: TargetLinux})
	if !errors.Is(err, stopErr) {
		t.Fatalf("Reset() error = %v, want %v", err, stopErr)
	}
	if !strings.Contains(err.Error(), "stop service") {
		t.Fatalf("Reset() error missing context: %v", err)
	}
}
