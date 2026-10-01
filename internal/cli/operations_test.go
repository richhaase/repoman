package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeCommand(t *testing.T, ctx context.Context, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "test", Commit: "abc123", Date: "unknown"})
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), diag.String(), err
}
func TestEmptyStructuredReports(t *testing.T) {
	for _, name := range []string{"status", "clean"} {
		t.Run(name, func(t *testing.T) {
			out, diag, err := executeCommand(t, t.Context(), name, "--root", t.TempDir(), "--json")
			if err != nil {
				t.Fatalf("err=%v stderr=%s", err, diag)
			}
			var got map[string]json.RawMessage
			if e := json.Unmarshal([]byte(out), &got); e != nil {
				t.Fatal(e)
			}
			if string(got["schema_version"]) != "1" || string(got["items"]) != "[]" || string(got["errors"]) != "[]" {
				t.Fatalf("report=%s", out)
			}
			if diag != "" {
				t.Fatalf("unexpected stderr %q", diag)
			}
		})
	}
}
func TestConfigSelection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(map[string]any{"targets": []any{map[string]any{"dir": dir, "owner": "me"}}})
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	_, _, e := executeCommand(t, t.Context(), "status", dir, "--config", p)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = executeCommand(t, t.Context(), "status", t.TempDir(), "--config", p)
	var coded *ExitError
	if !errors.As(e, &coded) || coded.Code != 2 {
		t.Fatalf("error=%v", e)
	}
}
func TestConfigPathPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name, xdg, selected string
		unsetXDG, env, flag bool
	}{
		{name: "absolute XDG", xdg: "absolute", selected: "xdg"},
		{name: "unset XDG", unsetXDG: true, selected: "home"},
		{name: "empty XDG", selected: "home"},
		{name: "relative XDG", xdg: "relative", selected: "home"},
		{name: "environment overrides defaults", xdg: "absolute", env: true, selected: "env"},
		{name: "flag overrides environment and defaults", xdg: "absolute", env: true, flag: true, selected: "flag"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, target := t.TempDir(), t.TempDir()
			t.Chdir(base)
			home, xdg := filepath.Join(base, "home"), filepath.Join(base, "xdg")
			paths := map[string]string{
				"home":     filepath.Join(home, ".config", "repoman", "config.json"),
				"xdg":      filepath.Join(xdg, "repoman", "config.json"),
				"relative": filepath.Join(base, "relative", "repoman", "config.json"),
				"env":      filepath.Join(base, "env.json"),
				"flag":     filepath.Join(base, "flag.json"),
			}
			data, err := json.Marshal(map[string]any{"targets": []any{map[string]any{"dir": target}}})
			if err != nil {
				t.Fatal(err)
			}
			for name, path := range paths {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				content := []byte(`{"unsupported":true}`)
				if name == tt.selected {
					content = data
				}
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("REPOMAN_CONFIG", "")
			if tt.env {
				t.Setenv("REPOMAN_CONFIG", paths["env"])
			}
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			if tt.xdg == "absolute" {
				t.Setenv("XDG_CONFIG_HOME", xdg)
			}
			if tt.unsetXDG {
				if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			// Selecting the configured root proves the intended file was loaded;
			// every other candidate contains invalid configuration.
			args := []string{"status", target, "--json"}
			if tt.flag {
				args = append(args, "--config", paths["flag"])
			}
			out, diag, err := executeCommand(t, t.Context(), args...)
			if err != nil || diag != "" {
				t.Fatalf("err=%v stderr=%q stdout=%s", err, diag, out)
			}
			var report envelope
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if report.Command != "status" || len(report.Errors) != 0 {
				t.Fatalf("report=%s", out)
			}
		})
	}
}
func TestRootBypassesConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"unsupported":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOMAN_CONFIG", p)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "missing"))
	for _, name := range []string{"status", "clean"} {
		t.Run(name, func(t *testing.T) {
			out, diag, err := executeCommand(t, t.Context(), name, "--root", t.TempDir(), "--config", p, "--json")
			if err != nil || diag != "" {
				t.Fatalf("err=%v stderr=%q stdout=%s", err, diag, out)
			}
		})
	}
}
func TestJSONFlagsDoNotLeak(t *testing.T) {
	dir := t.TempDir()
	_, _, e := executeCommand(t, t.Context(), "status", "--root", dir, "--json")
	if e != nil {
		t.Fatal(e)
	}
	out, _, e := executeCommand(t, t.Context(), "status", "--root", dir)
	if e != nil || !strings.Contains(out, "No repositories") {
		t.Fatalf("out=%q err=%v", out, e)
	}
}
func TestConflictingSelection(t *testing.T) {
	_, _, e := executeCommand(t, t.Context(), "status", "/tmp", "--root", "/tmp")
	var coded *ExitError
	if !errors.As(e, &coded) || coded.Code != 2 {
		t.Fatalf("error=%v", e)
	}
}
func TestCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, e := executeCommand(t, ctx, "status", "--root", t.TempDir())
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("error=%v", e)
	}
}
func TestVersionAndHelp(t *testing.T) {
	for _, arg := range []string{"version", "--version", "--help"} {
		out, _, e := executeCommand(t, t.Context(), arg)
		if e != nil || !strings.Contains(out, "repoman") {
			t.Fatalf("%s out=%q err=%v", arg, out, e)
		}
	}
}
func TestCleanMultipleApplyRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(map[string]any{"targets": []any{map[string]any{"dir": t.TempDir()}, map[string]any{"dir": t.TempDir()}}})
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	_, _, e := executeCommand(t, t.Context(), "clean", "--config", p, "--apply")
	var coded *ExitError
	if !errors.As(e, &coded) || coded.Code != 2 {
		t.Fatalf("error=%v", e)
	}
}
