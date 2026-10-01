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
