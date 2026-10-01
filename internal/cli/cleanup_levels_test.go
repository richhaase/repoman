package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func levelConfig(t *testing.T, levels ...string) string {
	t.Helper()
	targets := make([]map[string]any, 0, len(levels))
	for _, level := range levels {
		targets = append(targets, map[string]any{"dir": t.TempDir(), "cleanup_level": level})
	}
	data, err := json.Marshal(map[string]any{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestCleanupPolicyPrecedence(t *testing.T) {
	for _, tt := range []struct{ name, configured, override, want string }{
		{"default", "", "", "conservative"},
		{"configured balanced", "balanced", "", "balanced"},
		{"configured aggressive", "aggressive", "", "aggressive"},
		{"override less aggressive", "aggressive", "conservative", "conservative"},
		{"override more aggressive", "conservative", "aggressive", "aggressive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"clean", "--config", levelConfig(t, tt.configured), "--json"}
			if tt.override != "" {
				args = append(args, "--level", tt.override)
			}
			out, diag, err := executeCommand(t, t.Context(), args...)
			if err != nil {
				t.Fatalf("err=%v stderr=%s", err, diag)
			}
			var report envelope
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.CleanupPolicies) != 1 || string(report.CleanupPolicies[0].Level) != tt.want || !report.DryRun || report.CleanupPolicies[0].DiscardLocalChanges {
				t.Fatalf("report=%s", out)
			}
			if report.SchemaVersion != 1 {
				t.Fatalf("schema=%d", report.SchemaVersion)
			}
		})
	}
}
func TestCleanupMixedPolicies(t *testing.T) {
	out, _, err := executeCommand(t, t.Context(), "clean", "--config", levelConfig(t, "conservative", "balanced", "aggressive"), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report envelope
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.CleanupPolicies) != 3 {
		t.Fatal(out)
	}
	for i, want := range []string{"conservative", "balanced", "aggressive"} {
		if string(report.CleanupPolicies[i].Level) != want {
			t.Fatal(out)
		}
	}
}
func TestAggressiveApplyRequiresInvocationAcknowledgement(t *testing.T) {
	config := levelConfig(t, "aggressive")
	_, _, err := executeCommand(t, t.Context(), "clean", "--config", config, "--apply")
	var coded *ExitError
	if !errors.As(err, &coded) || coded.Code != 2 || !strings.Contains(err.Error(), "--discard-local-changes") {
		t.Fatalf("error=%v", err)
	}
	out, _, err := executeCommand(t, t.Context(), "clean", "--config", config, "--apply", "--discard-local-changes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report envelope
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.DryRun || !report.CleanupPolicies[0].DiscardLocalChanges {
		t.Fatal(out)
	}
	// An override to conservative restores the non-destructive default behavior.
	_, _, err = executeCommand(t, t.Context(), "clean", "--config", config, "--level", "conservative", "--apply")
	if err != nil {
		t.Fatal(err)
	}
}
func TestInvalidCleanupOptionsBeforeInspection(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, flags := range [][]string{{"--level", "reckless"}, {"--level", ""}, {"--level", " "}, {"--level", "balanced", "--discard-local-changes"}, {"--level", "aggressive", "--apply"}, {"--discard-local-changes"}} {
		args := append([]string{"clean", "--root", missing}, flags...)
		_, _, err := executeCommand(t, t.Context(), args...)
		var coded *ExitError
		if !errors.As(err, &coded) || coded.Code != 2 {
			t.Fatalf("args=%v error=%v", args, err)
		}
		if strings.Contains(err.Error(), "no such file") {
			t.Fatalf("inspected root before option validation: %v", err)
		}
	}
}
func TestCleanupHumanPolicyAndWarningEvenWhenEmpty(t *testing.T) {
	out, _, err := executeCommand(t, t.Context(), "clean", "--root", t.TempDir(), "--level", "aggressive")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level=aggressive", "Preview only", "tracked changes, untracked files, and ignored files", "--discard-local-changes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q from %s", want, out)
		}
	}
}
func TestCleanupLevelFlagsDoNotLeak(t *testing.T) {
	root := t.TempDir()
	_, _, err := executeCommand(t, t.Context(), "clean", "--root", root, "--level", "aggressive", "--discard-local-changes")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCommand(t, t.Context(), "clean", "--root", root)
	if err != nil || !strings.Contains(out, "level=conservative") || strings.Contains(out, "WARNING") {
		t.Fatalf("out=%s err=%v", out, err)
	}
}
