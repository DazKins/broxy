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
	if !strings.Contains(body, "ExecStart=\"/usr/local/bin/broxy\" serve --config \"/tmp/broxy-config/config.json\"") {
		t.Fatalf("linux unit missing ExecStart: %s", body)
	}
	if !strings.Contains(body, "Environment=\"HTTP_PROXY=http://127.0.0.1:7890\"") {
		t.Fatalf("linux unit missing environment: %s", body)
	}
	if !strings.Contains(body, "WantedBy=multi-user.target") {
		t.Fatalf("linux unit missing system install target: %s", body)
	}
	if !strings.Contains(body, "User=broxy") || !strings.Contains(body, "Group=broxy") {
		t.Fatalf("linux unit missing service user/group: %s", body)
	}
}

func TestRenderDarwinService(t *testing.T) {
	cfg := &cfgpkg.Config{StateDir: "/var/lib/broxy", LogDirPath: "/var/log/broxy"}
	def, err := NewDefinition(
		TargetDarwin,
		cfg,
		"/etc/broxy/config.json",
		"/usr/local/bin/broxy",
		nil,
	)
	if err != nil {
		t.Fatalf("NewDefinition() error = %v", err)
	}

	body, err := Render(def)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(body, "<string>com.broxy.daemon</string>") {
		t.Fatalf("darwin plist missing label: %s", body)
	}
	if !strings.Contains(body, "<key>UserName</key>") || !strings.Contains(body, "<string>broxy</string>") {
		t.Fatalf("darwin plist missing service user: %s", body)
	}
	if !strings.Contains(body, "<key>GroupName</key>") || !strings.Contains(body, "<string>broxy</string>") {
		t.Fatalf("darwin plist missing service group: %s", body)
	}
	if !strings.Contains(body, "<string>/usr/local/bin/broxy</string>") {
		t.Fatalf("darwin plist missing binary path: %s", body)
	}
	if !strings.Contains(body, "<string>--config</string>") {
		t.Fatalf("darwin plist missing config args: %s", body)
	}
	if !strings.Contains(body, "<string>/var/log/broxy/stdout.log</string>") {
		t.Fatalf("darwin plist missing global log path: %s", body)
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
		"systemctl stop broxy.service",
		"systemctl disable --now broxy.service",
		"systemctl daemon-reload",
		"systemctl daemon-reload",
		"systemctl enable broxy.service",
		"systemctl start broxy.service",
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

func TestRestartDarwinReloadsLaunchDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	def := &Definition{
		Target:      TargetDarwin,
		Label:       LaunchdLabel,
		ServiceFile: filepath.Join(tmpDir, "com.broxy.daemon.plist"),
		Executable:  "/usr/local/bin/broxy",
		ConfigPath:  "/etc/broxy/config.json",
		StateDir:    tmpDir,
		StdoutPath:  filepath.Join(tmpDir, "stdout.log"),
		StderrPath:  filepath.Join(tmpDir, "stderr.log"),
	}
	if err := os.WriteFile(def.ServiceFile, []byte(renderDarwin(def)), 0o644); err != nil {
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

	if err := Restart(def); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	want := []string{
		"launchctl bootout system/com.broxy.daemon",
		"launchctl bootstrap system " + def.ServiceFile,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}
