package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type UpstreamAuthMode string

const (
	UpstreamAuthAWS    UpstreamAuthMode = "aws"
	UpstreamAuthBearer UpstreamAuthMode = "bearer"
)

type UpstreamConfig struct {
	Mode             UpstreamAuthMode `json:"mode"`
	Region           string           `json:"region"`
	Profile          string           `json:"profile,omitempty"`
	BearerToken      string           `json:"bearer_token,omitempty"`
	EndpointOverride string           `json:"endpoint_override,omitempty"`
}

type Config struct {
	ListenAddr    string            `json:"listen_addr"`
	ConfigDir     string            `json:"config_dir"`
	StateDir      string            `json:"state_dir"`
	DBPath        string            `json:"db_path"`
	SessionSecret string            `json:"session_secret"`
	PricingPath   string            `json:"pricing_path"`
	Upstream      UpstreamConfig    `json:"upstream"`
	Env           map[string]string `json:"env"`
}

type Paths struct {
	ConfigDir   string
	ConfigPath  string
	StateDir    string
	DBPath      string
	PricingPath string
	LogDir      string
}

type fileConfig struct {
	ListenAddr    string            `json:"listen_addr"`
	ConfigDir     string            `json:"config_dir"`
	StateDir      string            `json:"state_dir,omitempty"`
	DBPath        string            `json:"db_path"`
	SessionSecret string            `json:"session_secret"`
	PricingPath   string            `json:"pricing_path"`
	Upstream      UpstreamConfig    `json:"upstream"`
	Env           map[string]string `json:"env"`
}

func DefaultPaths() (Paths, error) {
	return PathsForConfigPath("")
}

func PathsForConfigPath(path string) (Paths, error) {
	defaultPaths, err := platformPaths()
	if err != nil {
		return Paths{}, err
	}
	if path == "" {
		return defaultPaths, nil
	}

	configPath, err := cleanAbs(path)
	if err != nil {
		return Paths{}, err
	}
	if !pathWithinDir(configPath, defaultPaths.ConfigDir) {
		return Paths{}, fmt.Errorf("config path %s must be inside %s", configPath, defaultPaths.ConfigDir)
	}

	defaultPaths.ConfigPath = configPath
	return defaultPaths, nil
}

func platformPaths() (Paths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("get user home dir: %w", err)
	}

	root := filepath.Join(homeDir, ".broxy")
	return Paths{
		ConfigDir:   root,
		ConfigPath:  filepath.Join(root, "config.json"),
		StateDir:    root,
		DBPath:      filepath.Join(root, "broxy.db"),
		PricingPath: filepath.Join(root, "pricing.json"),
		LogDir:      filepath.Join(root, "logs"),
	}, nil
}

func Default() (*Config, error) { return DefaultForPath("") }

func DefaultForPath(path string) (*Config, error) {
	paths, err := PathsForConfigPath(path)
	if err != nil {
		return nil, err
	}
	bearerToken := os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	mode := UpstreamAuthAWS
	if bearerToken != "" {
		mode = UpstreamAuthBearer
	}
	return &Config{
		ListenAddr:  "127.0.0.1:8080",
		ConfigDir:   paths.ConfigDir,
		StateDir:    paths.StateDir,
		DBPath:      paths.DBPath,
		PricingPath: paths.PricingPath,
		Env:         map[string]string{},
		Upstream: UpstreamConfig{
			Mode:        mode,
			Region:      envDefault("AWS_REGION", envDefault("AWS_DEFAULT_REGION", "us-east-1")),
			Profile:     os.Getenv("AWS_PROFILE"),
			BearerToken: bearerToken,
		},
	}, nil
}

func ConfigPath(override string) (string, error) {
	paths, err := PathsForConfigPath(override)
	if err != nil {
		return "", err
	}
	return paths.ConfigPath, nil
}

func Load(path string) (*Config, error) {
	paths, err := PathsForConfigPath(path)
	if err != nil {
		return nil, err
	}
	cfg, err := DefaultForPath(paths.ConfigPath)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", paths.ConfigPath, err)
	}

	var raw fileConfig
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", paths.ConfigPath, err)
	}
	if err := applyFileConfig(cfg, paths.ConfigPath, raw); err != nil {
		return nil, err
	}
	overrideFromEnv(cfg)
	overrideFromEnvMap(cfg, cfg.Env)
	return cfg, nil
}

func Save(path string, cfg *Config) error {
	if err := applyDefaults(cfg, path); err != nil {
		return err
	}
	paths, err := PathsForConfigPath(path)
	if err != nil {
		return err
	}
	if err := EnsureLayout(cfg); err != nil {
		return err
	}

	body, err := json.MarshalIndent(fileConfig{
		ListenAddr:    cfg.ListenAddr,
		ConfigDir:     cfg.ConfigDir,
		StateDir:      cfg.StateDir,
		DBPath:        cfg.DBPath,
		SessionSecret: cfg.SessionSecret,
		PricingPath:   cfg.PricingPath,
		Upstream:      cfg.Upstream,
		Env:           cloneEnv(cfg.Env),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(paths.ConfigPath, body, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func EnsureLayout(cfg *Config) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := os.MkdirAll(cfg.LogDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return fmt.Errorf("mkdir db dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PricingPath), 0o755); err != nil {
		return fmt.Errorf("mkdir pricing dir: %w", err)
	}
	return nil
}

func (cfg *Config) LogDir() string {
	return filepath.Join(cfg.StateDir, "logs")
}

func applyFileConfig(cfg *Config, path string, raw fileConfig) error {
	if raw.ListenAddr != "" {
		cfg.ListenAddr = raw.ListenAddr
	}
	if raw.SessionSecret != "" {
		cfg.SessionSecret = raw.SessionSecret
	}
	cfg.Upstream = mergeUpstream(cfg.Upstream, raw.Upstream)
	if raw.Env != nil {
		cfg.Env = cloneEnv(raw.Env)
	}

	return applyDefaults(cfg, path)
}

func applyDefaults(cfg *Config, path string) error {
	defaultPaths, err := PathsForConfigPath(path)
	if err != nil {
		return err
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8080"
	}
	cfg.ConfigDir = defaultPaths.ConfigDir
	cfg.StateDir = defaultPaths.StateDir
	cfg.DBPath = defaultPaths.DBPath
	cfg.PricingPath = defaultPaths.PricingPath
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	if err := validateEnv(cfg.Env); err != nil {
		return err
	}
	if cfg.Upstream.Mode == "" {
		cfg.Upstream.Mode = UpstreamAuthAWS
	}
	if cfg.Upstream.BearerToken != "" {
		cfg.Upstream.Mode = UpstreamAuthBearer
	}
	if cfg.Upstream.Region == "" {
		cfg.Upstream.Region = envDefault("AWS_REGION", envDefault("AWS_DEFAULT_REGION", "us-east-1"))
	}
	return nil
}

func mergeUpstream(defaults, overrides UpstreamConfig) UpstreamConfig {
	out := defaults
	if overrides.Mode != "" {
		out.Mode = overrides.Mode
	}
	if overrides.Region != "" {
		out.Region = overrides.Region
	}
	if overrides.Profile != "" {
		out.Profile = overrides.Profile
	}
	if overrides.BearerToken != "" {
		out.BearerToken = overrides.BearerToken
	}
	if overrides.EndpointOverride != "" {
		out.EndpointOverride = overrides.EndpointOverride
	}
	return out
}

func ApplyEnv(env map[string]string) error {
	if err := validateEnv(env); err != nil {
		return err
	}
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set env %q: %w", key, err)
		}
	}
	return nil
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func validateEnv(env map[string]string) error {
	for key, value := range env {
		if key == "" {
			return fmt.Errorf("env contains empty key")
		}
		if containsNUL(key) {
			return fmt.Errorf("env key %q contains NUL byte", key)
		}
		if containsNUL(value) {
			return fmt.Errorf("env value for %q contains NUL byte", key)
		}
		if containsEqual(key) {
			return fmt.Errorf("env key %q is invalid", key)
		}
	}
	return nil
}

func containsNUL(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == 0 {
			return true
		}
	}
	return false
}

func containsEqual(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '=' {
			return true
		}
	}
	return false
}

func cleanAbs(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve config path: %w", err)
		}
		path = abs
	}
	return filepath.Clean(path), nil
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func overrideFromEnv(cfg *Config) {
	if v := os.Getenv("BEDROCK_PROXY_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("BEDROCK_PROXY_UPSTREAM_MODE"); v != "" {
		cfg.Upstream.Mode = UpstreamAuthMode(v)
	}
	if v := os.Getenv("BEDROCK_PROXY_BEDROCK_REGION"); v != "" {
		cfg.Upstream.Region = v
	}
	if v := os.Getenv("AWS_PROFILE"); v != "" {
		cfg.Upstream.Profile = v
	}
	if v := os.Getenv("AWS_BEARER_TOKEN_BEDROCK"); v != "" {
		cfg.Upstream.BearerToken = v
		cfg.Upstream.Mode = UpstreamAuthBearer
	}
}

func overrideFromEnvMap(cfg *Config, env map[string]string) {
	if v := env["BEDROCK_PROXY_LISTEN_ADDR"]; v != "" {
		cfg.ListenAddr = v
	}
	if v := env["BEDROCK_PROXY_UPSTREAM_MODE"]; v != "" {
		cfg.Upstream.Mode = UpstreamAuthMode(v)
	}
	if v := env["AWS_DEFAULT_REGION"]; v != "" {
		cfg.Upstream.Region = v
	}
	if v := env["AWS_REGION"]; v != "" {
		cfg.Upstream.Region = v
	}
	if v := env["BEDROCK_PROXY_BEDROCK_REGION"]; v != "" {
		cfg.Upstream.Region = v
	}
	if v := env["AWS_PROFILE"]; v != "" {
		cfg.Upstream.Profile = v
	}
	if v := env["AWS_BEARER_TOKEN_BEDROCK"]; v != "" {
		cfg.Upstream.BearerToken = v
		cfg.Upstream.Mode = UpstreamAuthBearer
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
