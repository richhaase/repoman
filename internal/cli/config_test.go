package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/config"
)

func TestConfigCommandLifecycleAndPrecedence(t *testing.T) {
	defaultPath := filepath.Join(t.TempDir(), "default.json")
	explicitPath := filepath.Join(t.TempDir(), "explicit.json")
	t.Setenv("REPOMAN_CONFIG", defaultPath)
	dir := filepath.Join(t.TempDir(), "a&b")
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("  # ignored\n\n  vendor-* \r\nlast-pattern"), 0600); err != nil {
		t.Fatal(err)
	}
	out, diag, err := executeCommand(t, t.Context(), "config", "--config", explicitPath, "add", dir, "--owner", "me", "--days", "17", "--include", "a*", "-e", "skip*", "--excludes-file", excludes, "--cleanup-level", "balanced", "--json")
	if err != nil || diag != "" {
		t.Fatalf("add=%q,%q,%v", out, diag, err)
	}
	var c config.Config
	if err = json.Unmarshal([]byte(out), &c); err != nil || len(c.Targets) != 1 || strings.Contains(out, "schema_version") || strings.Contains(out, `\u0026`) {
		t.Fatalf("add JSON=%s, %v", out, err)
	}
	target := c.Targets[0]
	if target.Dir != dir || target.Owner != "me" || target.Days != 17 || target.Events == nil || *target.Events || target.CleanupLevel != "balanced" || strings.Join(target.Includes, ",") != "a*" || strings.Join(target.Excludes, ",") != "skip*,vendor-*,last-pattern" {
		t.Fatalf("saved target=%+v", target)
	}
	if _, err = os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("explicit --config did not override environment: %v", err)
	}
	out, diag, err = executeCommand(t, t.Context(), "config", "list", "--config", explicitPath)
	if err != nil || diag != "" || !strings.Contains(out, dir) || (!strings.Contains(out, "owner:    me") || !strings.Contains(out, "window:   17 days · push activity")) || !strings.Contains(out, "clean:    balanced") {
		t.Fatalf("list=%q,%q,%v", out, diag, err)
	}
	out, _, err = executeCommand(t, t.Context(), "config", "add", dir, "--config", explicitPath, "--owner", "other", "--json")
	if err != nil {
		t.Fatal(err)
	}
	c = config.Config{}
	if err = json.Unmarshal([]byte(out), &c); err != nil || len(c.Targets) != 1 || c.Targets[0].Days != 45 || c.Targets[0].Owner != "other" || c.Targets[0].CleanupLevel != "balanced" || len(c.Targets[0].Includes) != 0 || len(c.Targets[0].Excludes) != 0 {
		t.Fatalf("replacement defaults=%s,%v", out, err)
	}
	out, _, err = executeCommand(t, t.Context(), "config", "remove", dir, "--config", explicitPath, "--json")
	if err != nil || strings.TrimSpace(out) != "{\n  \"targets\": []\n}" {
		t.Fatalf("remove=%q,%v", out, err)
	}
}

func TestConfigEnvironmentAndEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", "config.json")
	t.Setenv("REPOMAN_CONFIG", path)
	out, diag, err := executeCommand(t, t.Context(), "config", "list", "--json")
	if err != nil || diag != "" || !strings.Contains(out, `"targets": []`) {
		t.Fatalf("empty list=%q,%q,%v", out, diag, err)
	}
	if _, err = os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("list created config storage: %v", err)
	}
	dir := t.TempDir()
	if _, _, err = executeCommand(t, t.Context(), "config", "add", dir, "--owner", "me"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil || c.Targets[0].Dir != dir {
		t.Fatalf("environment path=%+v,%v", c, err)
	}
}

func TestConfigFetchControlsRoundTripAndPreservation(t *testing.T) {
	path, dir := filepath.Join(t.TempDir(), "config.json"), t.TempDir()
	for _, step := range []struct {
		name  string
		flags []string
		scope string
		prune bool
	}{
		{name: "new legacy defaults"},
		{name: "opt in", flags: []string{"--fetch-scope", "all", "--prune"}, scope: "all", prune: true},
		{name: "preserve omitted", scope: "all", prune: true},
		{name: "disable only prune", flags: []string{"--prune=false"}, scope: "all"},
		{name: "override only scope", flags: []string{"--fetch-scope", "origin"}, scope: "origin"},
		{name: "enable only prune", flags: []string{"--prune=true"}, scope: "origin", prune: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			args := append([]string{"config", "add", dir, "--config", path, "--owner", "me", "--json"}, step.flags...)
			out, diag, err := executeCommand(t, t.Context(), args...)
			if err != nil || diag != "" {
				t.Fatalf("add=%s,%s,%v", out, diag, err)
			}
			var added config.Config
			if err := json.Unmarshal([]byte(out), &added); err != nil || len(added.Targets) != 1 || added.Targets[0].FetchScope != step.scope || added.Targets[0].Prune != step.prune {
				t.Fatalf("saved=%s,%v", out, err)
			}
			listed, diag, err := executeCommand(t, t.Context(), "config", "list", "--config", path, "--json")
			if err != nil || diag != "" || listed != out {
				t.Fatalf("list changed values: added=%s listed=%s diag=%s err=%v", out, listed, diag, err)
			}
			loaded, err := config.Load(path)
			if err != nil || loaded.Targets[0].FetchScope != step.scope || loaded.Targets[0].Prune != step.prune {
				t.Fatalf("loaded=%+v,%v", loaded, err)
			}
			human, _, err := executeCommand(t, t.Context(), "config", "list", "--config", path)
			scope := step.scope
			if scope == "" {
				scope = "origin"
			}
			prune := "off"
			if step.prune {
				prune = "on"
			}
			if err != nil || !strings.Contains(human, "fetch:    "+scope+" · prune "+prune) {
				t.Fatalf("human list=%s,%v", human, err)
			}
		})
	}
}

func TestConfigAuthenticatedOwnerFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf 'authenticated-user\\n'\n"), 0700); err != nil { // #nosec G306 -- executable fixture in an isolated test directory.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "config.json")
	dir := t.TempDir()
	if _, _, err := executeCommand(t, t.Context(), "config", "add", dir, "--config", path); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil || c.Targets[0].Owner != "authenticated-user" {
		t.Fatalf("authenticated owner=%+v,%v", c, err)
	}
	if err = os.WriteFile(gh, []byte("#!/bin/sh\nexit 91\n"), 0700); err != nil { // #nosec G306 -- executable fixture in an isolated test directory.
		t.Fatal(err)
	}
	if _, _, err = executeCommand(t, t.Context(), "config", "add", dir, "--config", path, "--owner", "explicit"); err != nil {
		t.Fatalf("explicit owner should skip gh: %v", err)
	}
	newPath := filepath.Join(t.TempDir(), "must-not-exist.json")
	if _, _, err = executeCommand(t, t.Context(), "config", "add", dir, "--config", newPath); err == nil {
		t.Fatal("authentication failure ignored")
	}
	if _, err = os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("authentication failure created config: %v", err)
	}
}

func TestInvalidConfigFlagsDoNotWrite(t *testing.T) {
	for _, flags := range [][]string{
		{"--days", "0"},
		{"--days", "-1"},
		{"--owner", "bad owner"},
		{"--include", "["},
		{"--exclude", ""},
		{"--cleanup-level", "reckless"},
		{"--cleanup-level", ""},
		{"--fetch-scope", "invalid"},
		{"--fetch-scope", ""},
		{"--excludes-file", ""},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			args := []string{"config", "add", t.TempDir(), "--config", path, "--owner", "me"}
			args = append(args, flags...)
			_, _, err := executeCommand(t, t.Context(), args...)
			var coded *ExitError
			if !errors.As(err, &coded) || coded.Code != 2 {
				t.Fatalf("invalid flags error=%v", err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid flags wrote config: %v", err)
			}
		})
	}
}

func TestConfigCommandsCanceled(t *testing.T) {
	for _, subcommand := range []string{"list", "add", "remove"} {
		t.Run(subcommand, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := []string{"config", subcommand, "--config", path}
			if subcommand != "list" {
				args = append(args, t.TempDir())
			}
			_, _, err := executeCommand(t, ctx, args...)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("canceled command wrote config: %v", err)
			}
		})
	}
}

func TestConfigAddRejectsInvalidDirectoryBeforeWriting(t *testing.T) {
	for name, dir := range map[string]string{
		"NUL":             "bad\x00dir",
		"newline":         "bad\ndir",
		"carriage return": "bad\rdir",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "absent", "config.json")
			_, _, err := executeCommand(t, t.Context(), "config", "add", dir, "--owner", "me", "--config", path)
			var coded *ExitError
			if !errors.As(err, &coded) || coded.Code != 2 {
				t.Fatalf("invalid directory error=%v", err)
			}
			if _, err = os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("invalid directory created config storage: %v", err)
			}
		})
	}
}
