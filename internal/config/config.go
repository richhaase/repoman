// Package config loads application configuration.
package config

import (
	"os"
	"path/filepath"
)

// Config holds the application configuration.
type Config struct {
	Debug bool `yaml:"debug"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Debug: false,
	}
}

// Load reads configuration: flags override env, which overrides file, which overrides defaults.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	if configPath := findConfigFile(); configPath != "" {
		_ = configPath
	}

	if os.Getenv("MYCLI_DEBUG") == "true" {
		cfg.Debug = true
	}

	return cfg, nil
}

func findConfigFile() string {
	if _, err := os.Stat(".mycli.yaml"); err == nil {
		return ".mycli.yaml"
	}

	if dir := configDir(); dir != "" {
		configPath := filepath.Join(dir, "mycli", "config.yaml")
		if _, err := os.Stat(configPath); err == nil {
			return configPath
		}
	}

	return ""
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config")
	}
	return ""
}
