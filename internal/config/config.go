// Package config loads repoman's JSON target configuration without modifying it.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/richhaase/repoman/internal/cleanup"
	"github.com/richhaase/repoman/internal/syncer"
)

type Config struct {
	Targets []syncer.Target `json:"targets"`
}

func DefaultPath() string {
	if p := os.Getenv("REPOMAN_CONFIG"); p != "" {
		return p
	}
	// Use the same XDG location on macOS and Linux. Relative XDG paths are
	// invalid, so treat them like an unset value and fall back to ~/.config.
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "repoman", "config.json")
}

func Normalize(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if path == "" {
		return "", fmt.Errorf("empty target directory")
	}
	return filepath.Abs(filepath.Clean(path))
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("read config %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("config must contain one JSON object")
	}
	if len(c.Targets) == 0 {
		return c, fmt.Errorf("config has no targets; use --root DIR for an ad hoc target")
	}
	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		if _, levelErr := cleanup.ParseLevel(t.CleanupLevel); levelErr != nil {
			return c, fmt.Errorf("target %q cleanup_level: %w", t.Dir, levelErr)
		}
		t.Dir, err = Normalize(t.Dir)
		if err != nil {
			return c, err
		}
		if seen[t.Dir] {
			return c, fmt.Errorf("duplicate target %q", t.Dir)
		}
		seen[t.Dir] = true
		if t.Days == 0 {
			t.Days = 45
		}
		if t.Days < 1 {
			return c, fmt.Errorf("days must be positive")
		}
		for _, p := range append(append([]string{}, t.Includes...), t.Excludes...) {
			if p == "" || strings.ContainsAny(p, "\r\n") {
				return c, fmt.Errorf("invalid empty or multiline pattern")
			}
			if _, err = filepath.Match(p, ""); err != nil {
				return c, fmt.Errorf("invalid pattern %q: %w", p, err)
			}
		}
	}
	return c, nil
}
