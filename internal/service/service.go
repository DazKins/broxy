package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	LaunchdLabel   = "com.broxy.daemon"
	SystemdUnit    = "broxy.service"
	ServiceUser    = "broxy"
	ServiceGroup   = "broxy"
	ServiceBinPath = "/usr/local/bin/broxy"
	DefaultLogTail = 50
)

type Definition struct {
	Target      Target
	Label       string
	ServiceFile string
	Executable  string
	SourcePath  string
	ConfigPath  string
	ConfigDir   string
	StateDir    string
	DBPath      string
	PricingPath string
	LogDir      string
	StdoutPath  string
	StderrPath  string
	User        string
	Group       string
	Env         map[string]string
}

type Status struct {
	Manager      string
	Name         string
	State        string
	SubState     string
	Enabled      string
	PID          string
	LastExitCode string
	Result       string
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
	sourcePath := executable
	if target == TargetLinux || target == TargetDarwin {
		executable = ServiceBinPath
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
		SourcePath:  sourcePath,
		ConfigPath:  configPath,
		ConfigDir:   cfg.ConfigDir,
		StateDir:    cfg.StateDir,
		DBPath:      cfg.DBPath,
		PricingPath: cfg.PricingPath,
		LogDir:      cfg.LogDir(),
		StdoutPath:  filepath.Join(cfg.LogDir(), "stdout.log"),
		StderrPath:  filepath.Join(cfg.LogDir(), "stderr.log"),
		User:        ServiceUser,
		Group:       ServiceGroup,
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
		return run("systemctl", "start", SystemdUnit)
	case TargetDarwin:
		return startDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Stop(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return run("systemctl", "stop", SystemdUnit)
	case TargetDarwin:
		return stopDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Restart(def *Definition) error {
	switch def.Target {
	case TargetLinux:
		return run("systemctl", "restart", SystemdUnit)
	case TargetDarwin:
		_ = stopDarwin(def)
		return bootstrapDarwin(def)
	default:
		return fmt.Errorf("unsupported service target %s", def.Target)
	}
}

func Reset(def *Definition) error {
	if err := Stop(def); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := Uninstall(def); err != nil {
		return fmt.Errorf("uninstall service: %w", err)
	}
	if err := Install(def); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	if err := Start(def); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
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

func renderLinux(def *Definition) string {
	var builder strings.Builder
	builder.WriteString("[Unit]\n")
	builder.WriteString("Description=broxy local Bedrock proxy\n")
	builder.WriteString("After=network-online.target\n")
	builder.WriteString("Wants=network-online.target\n\n")
	builder.WriteString("[Service]\n")
	builder.WriteString("Type=simple\n")
	if def.User != "" {
		builder.WriteString("User=" + def.User + "\n")
	}
	if def.Group != "" {
		builder.WriteString("Group=" + def.Group + "\n")
	}
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
	builder.WriteString("WantedBy=multi-user.target\n")
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
	if def.User != "" {
		builder.WriteString("  <key>UserName</key>\n")
		builder.WriteString("  <string>" + xmlEscape(def.User) + "</string>\n")
	}
	if def.Group != "" {
		builder.WriteString("  <key>GroupName</key>\n")
		builder.WriteString("  <string>" + xmlEscape(def.Group) + "</string>\n")
	}
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
	if err := installServiceExecutable(def); err != nil {
		return err
	}
	if err := prepareLinuxServiceAccount(def); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(def.ServiceFile), 0o755); err != nil {
		return fmt.Errorf("mkdir systemd unit dir: %w", err)
	}
	if err := os.WriteFile(def.ServiceFile, []byte(renderLinux(def)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", def.ServiceFile, err)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "enable", SystemdUnit)
}

func installDarwin(def *Definition) error {
	if err := installServiceExecutable(def); err != nil {
		return err
	}
	if err := prepareDarwinServiceAccount(def); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(def.ServiceFile), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchDaemons dir: %w", err)
	}
	if err := os.WriteFile(def.ServiceFile, []byte(renderDarwin(def)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", def.ServiceFile, err)
	}
	return nil
}

func installServiceExecutable(def *Definition) error {
	if def.SourcePath == "" || def.Executable == "" || def.SourcePath == def.Executable {
		return nil
	}
	source, err := os.Open(def.SourcePath)
	if err != nil {
		return fmt.Errorf("open service executable source %s: %w", def.SourcePath, err)
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(def.Executable), 0o755); err != nil {
		return fmt.Errorf("mkdir service executable dir: %w", err)
	}
	target, err := os.OpenFile(def.Executable, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("open service executable target %s: %w", def.Executable, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy service executable to %s: %w", def.Executable, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close service executable target %s: %w", def.Executable, err)
	}
	if err := os.Chmod(def.Executable, 0o755); err != nil {
		return fmt.Errorf("chmod service executable %s: %w", def.Executable, err)
	}
	return nil
}

func prepareLinuxServiceAccount(def *Definition) error {
	if def.User == "" || def.Group == "" {
		return nil
	}
	if _, err := commandOutput("getent", "group", def.Group); err != nil {
		if err := run("groupadd", "--system", def.Group); err != nil {
			return fmt.Errorf("create service group %s: %w", def.Group, err)
		}
	}
	if _, err := commandOutput("id", "-u", def.User); err != nil {
		if err := run("useradd", "--system", "--gid", def.Group, "--home-dir", def.StateDir, "--shell", "/usr/sbin/nologin", def.User); err != nil {
			return fmt.Errorf("create service user %s: %w", def.User, err)
		}
	}
	return applyServicePermissions(def)
}

func prepareDarwinServiceAccount(def *Definition) error {
	if def.User == "" || def.Group == "" {
		return nil
	}
	if _, err := commandOutput("dscl", ".", "-read", "/Groups/"+def.Group); err != nil {
		gid, gidErr := nextDarwinID("/Groups", "PrimaryGroupID")
		if gidErr != nil {
			return gidErr
		}
		if err := run("dscl", ".", "-create", "/Groups/"+def.Group); err != nil {
			return fmt.Errorf("create service group %s: %w", def.Group, err)
		}
		if err := run("dscl", ".", "-create", "/Groups/"+def.Group, "RealName", "Broxy Service"); err != nil {
			return fmt.Errorf("set service group name %s: %w", def.Group, err)
		}
		if err := run("dscl", ".", "-create", "/Groups/"+def.Group, "PrimaryGroupID", strconv.Itoa(gid)); err != nil {
			return fmt.Errorf("set service group id %s: %w", def.Group, err)
		}
	}
	if _, err := commandOutput("dscl", ".", "-read", "/Users/"+def.User); err != nil {
		uid, uidErr := nextDarwinID("/Users", "UniqueID")
		if uidErr != nil {
			return uidErr
		}
		gid, gidErr := darwinGroupID(def.Group)
		if gidErr != nil {
			return gidErr
		}
		commands := [][]string{
			{".", "-create", "/Users/" + def.User},
			{".", "-create", "/Users/" + def.User, "UserShell", "/usr/bin/false"},
			{".", "-create", "/Users/" + def.User, "RealName", "Broxy Service"},
			{".", "-create", "/Users/" + def.User, "UniqueID", strconv.Itoa(uid)},
			{".", "-create", "/Users/" + def.User, "PrimaryGroupID", strconv.Itoa(gid)},
			{".", "-create", "/Users/" + def.User, "NFSHomeDirectory", def.StateDir},
			{".", "-create", "/Users/" + def.User, "Password", "*"},
			{".", "-create", "/Users/" + def.User, "IsHidden", "1"},
		}
		for _, args := range commands {
			if err := run("dscl", args...); err != nil {
				return fmt.Errorf("create service user %s: %w", def.User, err)
			}
		}
	}
	return applyServicePermissions(def)
}

func applyServicePermissions(def *Definition) error {
	for _, dir := range []string{def.ConfigDir, def.StateDir, def.LogDir, filepath.Dir(def.DBPath), filepath.Dir(def.PricingPath)} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if def.Group != "" {
		for _, path := range []string{def.ConfigDir, def.ConfigPath, def.PricingPath} {
			if err := chownIfExists("root:"+def.Group, path); err != nil {
				return err
			}
		}
		for _, path := range []string{def.StateDir, def.LogDir, filepath.Dir(def.DBPath), def.DBPath} {
			if err := chownIfExists(def.User+":"+def.Group, path); err != nil {
				return err
			}
		}
	}
	if err := chmodIfExists(def.ConfigDir, 0o750); err != nil {
		return err
	}
	if err := chmodIfExists(def.ConfigPath, 0o640); err != nil {
		return err
	}
	if err := chmodIfExists(def.PricingPath, 0o660); err != nil {
		return err
	}
	for _, path := range []string{def.StateDir, def.LogDir, filepath.Dir(def.DBPath)} {
		if err := chmodIfExists(path, 0o750); err != nil {
			return err
		}
	}
	if err := chmodIfExists(def.DBPath, 0o640); err != nil {
		return err
	}
	return nil
}

func nextDarwinID(recordPath, idAttribute string) (int, error) {
	output, err := commandOutput("dscl", ".", "-list", recordPath, idAttribute)
	if err != nil {
		return 0, fmt.Errorf("list %s ids: %w", recordPath, err)
	}
	used := map[int]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(fields[len(fields)-1])
		if err == nil {
			used[id] = true
		}
	}
	for id := 300; id < 500; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no available macOS system id for %s", recordPath)
}

func darwinGroupID(group string) (int, error) {
	output, err := commandOutput("dscl", ".", "-read", "/Groups/"+group, "PrimaryGroupID")
	if err != nil {
		return 0, fmt.Errorf("read service group id %s: %w", group, err)
	}
	for _, field := range strings.Fields(output) {
		id, err := strconv.Atoi(field)
		if err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("service group %s has no PrimaryGroupID", group)
}

func chownIfExists(owner, path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := run("chown", owner, path); err != nil {
		return fmt.Errorf("chown %s %s: %w", owner, path, err)
	}
	return nil
}

func chmodIfExists(path string, mode os.FileMode) error {
	if path == "" {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func uninstallLinux(def *Definition) error {
	_ = run("systemctl", "disable", "--now", SystemdUnit)
	if err := os.Remove(def.ServiceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", def.ServiceFile, err)
	}
	return run("systemctl", "daemon-reload")
}

func uninstallDarwin(def *Definition) error {
	_ = stopDarwin(def)
	if err := os.Remove(def.ServiceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", def.ServiceFile, err)
	}
	return nil
}

func startDarwin(def *Definition) error {
	_ = stopDarwin(def)
	return bootstrapDarwin(def)
}

func bootstrapDarwin(def *Definition) error {
	if _, err := os.Stat(def.ServiceFile); err != nil {
		return fmt.Errorf("service definition not found at %s: %w", def.ServiceFile, err)
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
	output, err := commandOutput("systemctl", "show", SystemdUnit, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=UnitFileState", "--property=ExecMainStatus", "--property=Result")
	if err != nil {
		return nil, err
	}
	values := parseKeyValueLines(output)
	return &Status{
		Manager:      "systemd",
		Name:         SystemdUnit,
		State:        values["ActiveState"],
		SubState:     values["SubState"],
		Enabled:      values["UnitFileState"],
		PID:          values["MainPID"],
		LastExitCode: values["ExecMainStatus"],
		Result:       values["Result"],
	}, nil
}

func statusDarwin(def *Definition) (*Status, error) {
	output, err := commandOutput("launchctl", "print", launchdDomain()+"/"+def.Label)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &Status{
		Manager:      "launchd",
		Name:         def.Label,
		State:        launchctlValue(output, "state = "),
		SubState:     "",
		Enabled:      "loaded",
		PID:          launchctlValue(output, "pid = "),
		LastExitCode: launchctlValue(output, "last exit code = "),
	}, nil
}

func serviceFilePath(target Target) (string, error) {
	switch target {
	case TargetLinux:
		return filepath.Join(string(os.PathSeparator), "etc", "systemd", "system", SystemdUnit), nil
	case TargetDarwin:
		return filepath.Join(string(os.PathSeparator), "Library", "LaunchDaemons", LaunchdLabel+".plist"), nil
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
	return "system"
}

var run = defaultRun

func defaultRun(name string, args ...string) error {
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
		if os.IsPermission(err) {
			return "", fmt.Errorf("read %s: permission denied; try running this command with sudo", path)
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
