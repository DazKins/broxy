package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	cfgpkg "github.com/personal/broxy/internal/config"
)

type Target string

const (
	TargetDarwin Target = "darwin"
	TargetLinux  Target = "linux"
)

const (
	LaunchdLabel   = "com.broxy.agent"
	SystemdUnit    = "broxy.service"
	DefaultLogTail = 50
)

type Definition struct {
	Target      Target
	Label       string
	ServiceFile string
	Executable  string
	ConfigPath  string
	StateDir    string
	StdoutPath  string
	StderrPath  string
	Env         map[string]string
}

type Status struct {
	Manager  string
	Name     string
	State    string
	SubState string
	Enabled  string
	PID      string
}

func CurrentTarget() (Target, error) {
	switch runtime.GOOS {
	case "darwin":
		return TargetDarwin, nil
	case "linux":
		return TargetLinux, nil
	default:
		return "", fmt.Errorf("service management is not supported on %s", runtime.GOOS)
	}
}

func NewDefinition(target Target, cfg *cfgpkg.Config, configPath, executable string, env map[string]string) (*Definition, error) {
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	if err == nil {
		executable = resolvedExecutable
	}
	serviceFile, err := serviceFilePath(target)
	if err != nil {
		return nil, err
	}
	return &Definition{
		Target:      target,
		Label:       serviceLabel(target),
		ServiceFile: serviceFile,
		Executable:  executable,
		ConfigPath:  configPath,
		StateDir:    cfg.StateDir,
		StdoutPath:  filepath.Join(cfg.LogDir(), "stdout.log"),
		StderrPath:  filepath.Join(cfg.LogDir(), "stderr.log"),
		Env:         env,
	}, nil
}

func Install(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return installLinux(def)
	case TargetDarwin:
		return installDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Uninstall(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return uninstallLinux(def)
	case TargetDarwin:
		return uninstallDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Start(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return run("systemctl", "--user", "start", SystemdUnit)
	case TargetDarwin:
		return startDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Stop(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return run("systemctl", "--user", "stop", SystemdUnit)
	case TargetDarwin:
		return stopDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Restart(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return run("systemctl", "--user", "restart", SystemdUnit)
	case TargetDarwin:
		if _, err := statusDarwin(def); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return startDarwin(def)
			}
			return err
		}
		return run("launchctl", "kickstart", "-k", launchdDomain()+"/"+def.Label)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func GetStatus(def *Definition) (*Status, error) {
	switch def.Target {
	case TargetLinux:
		return statusLinux(def)
	case TargetDarwin:
		return statusDarwin(def)
	default:
		return nil, fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Render(def *Definition) (string, error) {
	switch def.Target {
	case TargetLinux:
		return renderLinux(def), nil
	case TargetDarwin:
		return renderDarwin(def), nil
	default:
		return "", fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func TailLogs(def *Definition, stream string, lines int) (string, error) {
	if lines <= 0 {
		lines = DefaultLogTail
	}
	var sections []string
	switch stream {
	case "stdout":
		body, err := tailFile(def.StdoutPath, lines)
		if err != nil {
			return "", err
		}
		sections = append(sections, body)
	case "stderr":
		body, err := tailFile(def.StderrPath, lines)
		if err != nil {
			return "", err
		}
		sections = append(sections, body)
	case "both", "":
		stdout, err := tailFile(def.StdoutPath, lines)
		if err != nil {
			return "", err
		}
		stderr, err := tailFile(def.StderrPath, lines)
		if err != nil {
			return "", err
		}
		sections = append(sections, "== stdout ==\n"+stdout, "== stderr ==\n"+stderr)
	default:
		return "", fmt.Errorf("invalid stream %q", stream)
	}
	return strings.Join(sections, "\n"), nil
}

func CapturedEnvironment() map[string]string {
	keys := []string{
		"HOME",
		"PATH",
		"USER",
		"LOGNAME",
		"AWS_PROFILE",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"HTTP_PROXY",
		"http_proxy",
		"HTTPS_PROXY",
		"https_proxy",
		"ALL_PROXY",
		"all_proxy",
		"NO_PROXY",
		"no_proxy",
	}
	env := make(map[string]string)
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	return env
}

func renderLinux(def *Definition) string {
	var builder strings.Builder
	builder.WriteString("[Unit]\n")
	builder.WriteString("Description=broxy local Bedrock proxy\n")
	builder.WriteString("After=network-online.target\n")
	builder.WriteString("Wants=network-online.target\n\n")
	builder.WriteString("[Service]\n")
	builder.WriteString("Type=simple\n")
	builder.WriteString("WorkingDirectory=" + systemdEscape(def.StateDir) + "\n")
	builder.WriteString("ExecStart=" + systemdExecStart(def.Executable, def.ConfigPath) + "\n")
	builder.WriteString("Restart=on-failure\n")
	builder.WriteString("RestartSec=5\n")
	builder.WriteString("StandardOutput=append:" + def.StdoutPath + "\n")
	builder.WriteString("StandardError=append:" + def.StderrPath + "\n")
	for _, key := range sortedEnvKeys(def.Env) {
		builder.WriteString(fmt.Sprintf("Environment=%q\n", key+"="+def.Env[key]))
	}
	builder.WriteString("\n[Install]\n")
	builder.WriteString("WantedBy=default.target\n")
	return builder.String()
}

func renderDarwin(def *Definition) string {
	var builder strings.Builder
	builder.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	builder.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	builder.WriteString("<plist version=\"1.0\">\n")
	builder.WriteString("<dict>\n")
	builder.WriteString("  <key>Label</key>\n")
	builder.WriteString("  <string>" + xmlEscape(def.Label) + "</string>\n")
	builder.WriteString("  <key>ProgramArguments</key>\n")
	builder.WriteString("  <array>\n")
	for _, value := range []string{def.Executable, "serve", "--config", def.ConfigPath} {
		builder.WriteString("    <string>" + xmlEscape(value) + "</string>\n")
	}
	builder.WriteString("  </array>\n")
	builder.WriteString("  <key>WorkingDirectory</key>\n")
	builder.WriteString("  <string>" + xmlEscape(def.StateDir) + "</string>\n")
	builder.WriteString("  <key>RunAtLoad</key>\n")
	builder.WriteString("  <true/>\n")
	builder.WriteString("  <key>KeepAlive</key>\n")
	builder.WriteString("  <true/>\n")
	builder.WriteString("  <key>StandardOutPath</key>\n")
	builder.WriteString("  <string>" + xmlEscape(def.StdoutPath) + "</string>\n")
	builder.WriteString("  <key>StandardErrorPath</key>\n")
	builder.WriteString("  <string>" + xmlEscape(def.StderrPath) + "</string>\n")
	if len(def.Env) > 0 {
		builder.WriteString("  <key>EnvironmentVariables</key>\n")
		builder.WriteString("  <dict>\n")
		for _, key := range sortedEnvKeys(def.Env) {
			builder.WriteString("    <key>" + xmlEscape(key) + "</key>\n")
			builder.WriteString("    <string>" + xmlEscape(def.Env[key]) + "</string>\n")
		}
		builder.WriteString("  </dict>\n")
	}
	builder.WriteString("</dict>\n")
	builder.WriteString("</plist>\n")
	return builder.String()
}

func installLinux(def *Definition) error {
	if err := os.MkdirAll(filepath.Dir(def.ServiceFile), 0o755); err != nil {
		return fmt.Errorf("mkdir systemd user dir: %w", err)
	}
	if err := os.WriteFile(def.ServiceFile, []byte(renderLinux(def)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", def.ServiceFile, err)
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "--user", "enable", SystemdUnit)
}

func installDarwin(def *Definition) error {
	if err := os.MkdirAll(filepath.Dir(def.ServiceFile), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(def.ServiceFile, []byte(renderDarwin(def)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", def.ServiceFile, err)
	}
	return nil
}

func uninstallLinux(def *Definition) error {
	_ = run("systemctl", "--user", "disable", "--now", SystemdUnit)
	if err := os.Remove(def.ServiceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", def.ServiceFile, err)
	}
	return run("systemctl", "--user", "daemon-reload")
}

func uninstallDarwin(def *Definition) error {
	_ = stopDarwin(def)
	if err := os.Remove(def.ServiceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", def.ServiceFile, err)
	}
	return nil
}

func startDarwin(def *Definition) error {
	if _, err := os.Stat(def.ServiceFile); err != nil {
		return fmt.Errorf("service definition not found at %s: %w", def.ServiceFile, err)
	}
	if _, err := statusDarwin(def); err == nil {
		return run("launchctl", "kickstart", "-k", launchdDomain()+"/"+def.Label)
	}
	return run("launchctl", "bootstrap", launchdDomain(), def.ServiceFile)
}

func stopDarwin(def *Definition) error {
	err := run("launchctl", "bootout", launchdDomain()+"/"+def.Label)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
	}
	return nil
}

func statusLinux(def *Definition) (*Status, error) {
	output, err := commandOutput("systemctl", "--user", "show", SystemdUnit, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=UnitFileState")
	if err != nil {
		return nil, err
	}
	values := parseKeyValueLines(output)
	return &Status{
		Manager:  "systemd",
		Name:     SystemdUnit,
		State:    values["ActiveState"],
		SubState: values["SubState"],
		Enabled:  values["UnitFileState"],
		PID:      values["MainPID"],
	}, nil
}

func statusDarwin(def *Definition) (*Status, error) {
	output, err := commandOutput("launchctl", "print", launchdDomain()+"/"+def.Label)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &Status{
		Manager:  "launchd",
		Name:     def.Label,
		State:    launchctlValue(output, "state = "),
		SubState: "",
		Enabled:  "loaded",
		PID:      launchctlValue(output, "pid = "),
	}, nil
}

func serviceFilePath(target Target) (string, error) {
	switch target {
	case TargetLinux:
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get user home dir: %w", err)
		}
		configHome := envOr("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
		return filepath.Join(configHome, "systemd", "user", SystemdUnit), nil
	case TargetDarwin:
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get user home dir: %w", err)
		}
		return filepath.Join(homeDir, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
	default:
		return "", fmt.Errorf("unsupported service target %s", target)
	}
}

func serviceLabel(target Target) string {
	if target == TargetLinux {
		return SystemdUnit
	}
	return LaunchdLabel
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func systemdExecStart(executable, configPath string) string {
	return strconv.Quote(executable) + " serve --config " + strconv.Quote(configPath)
}

func systemdEscape(value string) string {
	return strings.ReplaceAll(value, " ", "\\x20")
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(value)
}

func launchdDomain() string {
	currentUser, err := user.Current()
	if err != nil {
		return "gui"
	}
	return "gui/" + currentUser.Uid
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func commandOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func parseKeyValueLines(body string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

func launchctlValue(body, prefix string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func tailFile(path string, lines int) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "(log file not found)\n", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	bodyLines := bytes.Split(bytes.TrimRight(content, "\n"), []byte("\n"))
	if len(bodyLines) == 1 && len(bodyLines[0]) == 0 {
		return "(log file is empty)\n", nil
	}
	if len(bodyLines) > lines {
		bodyLines = bodyLines[len(bodyLines)-lines:]
	}
	return string(bytes.Join(bodyLines, []byte("\n"))) + "\n", nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
