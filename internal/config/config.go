package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	DataDir       string         `json:"data_dir"`
	DBPath        string         `json:"db_path"`
	SessionSecret string         `json:"session_secret"`
	PricingPath   string         `json:"pricing_path"`
	Upstream      UpstreamConfig `json:"upstream"`
}

func DefaultPaths() (configDir, dataDir string, err error) {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("get user config dir: %w", err)
	}
	userDataDir, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("get user cache dir: %w", err)
	}
	configDir = filepath.Join(userConfigDir, "broxy")
	dataDir = filepath.Join(userDataDir, "broxy")
	return configDir, dataDir, nil
}

func Default() (*Config, error) {
	configDir, dataDir, err := DefaultPaths()
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		ListenAddr:  "127.0.0.1:8080",
		ConfigDir:   configDir,
		DataDir:     dataDir,
		DBPath:      filepath.Join(dataDir, "broxy.db"),
		PricingPath: filepath.Join(configDir, "pricing.json"),
		Upstream: UpstreamConfig{
			Mode:        UpstreamAuthAWS,
			Region:      envDefault("AWS_REGION", envDefault("AWS_DEFAULT_REGION", "us-east-1")),
			Profile:     os.Getenv("AWS_PROFILE"),
			BearerToken: os.Getenv("AWS_BEARER_TOKEN_BEDROCK"),
		},
	}
	return cfg, nil
}

func ConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	configDir, _, err := DefaultPaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "config.json"), nil
}

func Load(path string) (*Config, error) {
	cfg, err := Default()
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(content, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	overrideFromEnv(cfg)
	return cfg, nil
}

func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
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
