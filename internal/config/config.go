package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	ListenAddr    string         `json:"listen_addr"`
	ConfigDir     string         `json:"config_dir"`
	StateDir      string         `json:"state_dir"`
	DataDir       string         `json:"-"`
	DBPath        string         `json:"db_path"`
	SessionSecret string         `json:"session_secret"`
	PricingPath   string         `json:"pricing_path"`
	Upstream      UpstreamConfig `json:"upstream"`
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
	ListenAddr    string         `json:"listen_addr"`
	ConfigDir     string         `json:"config_dir"`
	StateDir      string         `json:"state_dir,omitempty"`
	DataDir       string         `json:"data_dir,omitempty"`
	DBPath        string         `json:"db_path"`
	SessionSecret string         `json:"session_secret"`
	PricingPath   string         `json:"pricing_path"`
	Upstream      UpstreamConfig `json:"upstream"`
}

func DefaultPaths() (Paths, error) {
	return PathsForConfigPath("")
}

func LegacyPaths() (Paths, error) {
	return LegacyPathsForConfigPath("")
}

func PathsForConfigPath(path string) (Paths, error) {
	defaultPaths, err := platformPaths()
	if err != nil {
		return Paths{}, err
	}
	if path == "" || path == defaultPaths.ConfigPath {
		return defaultPaths, nil
	}

	baseDir := filepath.Dir(path)
	stateDir := filepath.Join(baseDir, "state")
	return Paths{
		ConfigDir:   baseDir,
		ConfigPath:  path,
		StateDir:    stateDir,
		DBPath:      filepath.Join(stateDir, "broxy.db"),
		PricingPath: filepath.Join(baseDir, "pricing.json"),
		LogDir:      filepath.Join(stateDir, "logs"),
	}, nil
}

func LegacyPathsForConfigPath(path string) (Paths, error) {
	legacyPaths, err := platformLegacyPaths()
	if err != nil {
		return Paths{}, err
	}
	if path == "" || path == legacyPaths.ConfigPath {
		return legacyPaths, nil
	}

	baseDir := filepath.Dir(path)
	dataDir := filepath.Join(baseDir, "data")
	return Paths{
		ConfigDir:   baseDir,
		ConfigPath:  path,
		StateDir:    dataDir,
		DBPath:      filepath.Join(dataDir, "broxy.db"),
		PricingPath: filepath.Join(baseDir, "pricing.json"),
		LogDir:      filepath.Join(dataDir, "logs"),
	}, nil
}

func platformPaths() (Paths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("get user home dir: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		root := filepath.Join(homeDir, "Library", "Application Support", "broxy")
		return Paths{
			ConfigDir:   root,
			ConfigPath:  filepath.Join(root, "config.json"),
			StateDir:    root,
			DBPath:      filepath.Join(root, "broxy.db"),
			PricingPath: filepath.Join(root, "pricing.json"),
			LogDir:      filepath.Join(root, "logs"),
		}, nil
	case "linux":
		configHome := envDefault("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
		stateHome := envDefault("XDG_STATE_HOME", filepath.Join(homeDir, ".local", "state"))
		configDir := filepath.Join(configHome, "broxy")
		stateDir := filepath.Join(stateHome, "broxy")
		return Paths{
			ConfigDir:   configDir,
			ConfigPath:  filepath.Join(configDir, "config.json"),
			StateDir:    stateDir,
			DBPath:      filepath.Join(stateDir, "broxy.db"),
			PricingPath: filepath.Join(configDir, "pricing.json"),
			LogDir:      filepath.Join(stateDir, "logs"),
		}, nil
	default:
		userConfigDir, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("get user config dir: %w", err)
		}
		configDir := filepath.Join(userConfigDir, "broxy")
		stateDir := filepath.Join(configDir, "state")
		return Paths{
			ConfigDir:   configDir,
			ConfigPath:  filepath.Join(configDir, "config.json"),
			StateDir:    stateDir,
			DBPath:      filepath.Join(stateDir, "broxy.db"),
			PricingPath: filepath.Join(configDir, "pricing.json"),
			LogDir:      filepath.Join(stateDir, "logs"),
		}, nil
	}
}

func platformLegacyPaths() (Paths, error) {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("get user config dir: %w", err)
	}
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, fmt.Errorf("get user cache dir: %w", err)
	}

	configDir := filepath.Join(userConfigDir, "broxy")
	stateDir := filepath.Join(userCacheDir, "broxy")
	return Paths{
		ConfigDir:   configDir,
		ConfigPath:  filepath.Join(configDir, "config.json"),
		StateDir:    stateDir,
		DBPath:      filepath.Join(stateDir, "broxy.db"),
		PricingPath: filepath.Join(configDir, "pricing.json"),
		LogDir:      filepath.Join(stateDir, "logs"),
	}, nil
}

func Default() (*Config, error) { return DefaultForPath("") }

func DefaultForPath(path string) (*Config, error) {
	paths, err := PathsForConfigPath(path)
	if err != nil {
		return nil, err
	}
	return &Config{
		ListenAddr:  "127.0.0.1:8080",
		ConfigDir:   paths.ConfigDir,
		StateDir:    paths.StateDir,
		DBPath:      paths.DBPath,
		PricingPath: paths.PricingPath,
		Upstream: UpstreamConfig{
			Mode:        UpstreamAuthAWS,
			Region:      envDefault("AWS_REGION", envDefault("AWS_DEFAULT_REGION", "us-east-1")),
			Profile:     os.Getenv("AWS_PROFILE"),
			BearerToken: os.Getenv("AWS_BEARER_TOKEN_BEDROCK"),
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
	cfg, err := DefaultForPath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var raw fileConfig
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := applyFileConfig(cfg, path, raw); err != nil {
		return nil, err
	}
	overrideFromEnv(cfg)
	return cfg, nil
}

func Save(path string, cfg *Config) error {
	if err := applyDefaults(cfg, path); err != nil {
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
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
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

func MigrateLegacyState(path string, cfg *Config) error {
	defaultPaths, err := PathsForConfigPath(path)
	if err != nil {
		return err
	}
	legacyPaths, err := LegacyPathsForConfigPath(path)
	if err != nil {
		return err
	}
	if cfg.DBPath != defaultPaths.DBPath || legacyPaths.DBPath == defaultPaths.DBPath {
		return nil
	}

	legacyInfo, err := os.Stat(legacyPaths.DBPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat legacy db %s: %w", legacyPaths.DBPath, err)
	}
	if legacyInfo.IsDir() {
		return nil
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat new db %s: %w", cfg.DBPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return fmt.Errorf("mkdir new db dir: %w", err)
	}
	if err := moveFile(legacyPaths.DBPath, cfg.DBPath); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		src := legacyPaths.DBPath + suffix
		if _, err := os.Stat(src); err == nil {
			if err := moveFile(src, cfg.DBPath+suffix); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat legacy sqlite sidecar %s: %w", src, err)
		}
	}
	return nil
}

func (cfg *Config) LogDir() string {
	return filepath.Join(cfg.StateDir, "logs")
}

func applyFileConfig(cfg *Config, path string, raw fileConfig) error {
	defaultPaths, err := PathsForConfigPath(path)
	if err != nil {
		return err
	}
	legacyPaths, err := LegacyPathsForConfigPath(path)
	if err != nil {
		return err
	}

	if raw.ListenAddr != "" {
		cfg.ListenAddr = raw.ListenAddr
	}
	if raw.ConfigDir != "" {
		cfg.ConfigDir = raw.ConfigDir
	}

	stateDir := defaultPaths.StateDir
	switch {
	case raw.StateDir != "":
		stateDir = raw.StateDir
	case raw.DataDir != "":
		stateDir = raw.DataDir
	}
	if stateDir == legacyPaths.StateDir {
		stateDir = defaultPaths.StateDir
	}
	cfg.StateDir = stateDir
	cfg.DataDir = raw.DataDir

	if raw.DBPath != "" {
		cfg.DBPath = raw.DBPath
	}
	if cfg.DBPath == legacyPaths.DBPath {
		cfg.DBPath = filepath.Join(cfg.StateDir, "broxy.db")
	}
	if raw.SessionSecret != "" {
		cfg.SessionSecret = raw.SessionSecret
	}
	if raw.PricingPath != "" {
		cfg.PricingPath = raw.PricingPath
	}
	cfg.Upstream = mergeUpstream(cfg.Upstream, raw.Upstream)

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
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = defaultPaths.ConfigDir
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultPaths.StateDir
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.StateDir, "broxy.db")
	}
	if cfg.PricingPath == "" {
		cfg.PricingPath = filepath.Join(cfg.ConfigDir, "pricing.json")
	}
	if cfg.Upstream.Mode == "" {
		cfg.Upstream.Mode = UpstreamAuthAWS
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

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrInvalid) {
		var linkErr *os.LinkError
		if !errors.As(err, &linkErr) {
			return fmt.Errorf("rename %s -> %s: %w", src, dst, err)
		}
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove legacy file %s: %w", src, err)
	}
	return nil
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

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
