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
		{"default", "", "", "aggressive"},
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
			if len(report.CleanupPolicies) != 1 || string(report.CleanupPolicies[0].Level) != tt.want || report.DryRun || report.CleanupPolicies[0].DiscardLocalChanges {
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
func TestCleanApplyAndPreviewFlags(t *testing.T) {
	for _, test := range []struct {
		flags []string
		dry   bool
		bad   bool
	}{
		{flags: nil}, {flags: []string{"--apply"}}, {flags: []string{"--apply=true"}},
		{flags: []string{"--dry-run"}, dry: true}, {flags: []string{"-n"}, dry: true},
		{flags: []string{"--apply=false"}, dry: true}, {flags: []string{"--dry-run=false"}},
		{flags: []string{"--apply=false", "--dry-run=true"}, dry: true},
		{flags: []string{"--apply=true", "--dry-run=true"}, bad: true},
		{flags: []string{"--discard-local-changes"}}, {flags: []string{"--discard-local-changes=false"}},
	} {
		args := append([]string{"clean", "--root", t.TempDir(), "--json"}, test.flags...)
		out, _, err := executeCommand(t, t.Context(), args...)
		if (err != nil) != test.bad {
			t.Fatalf("flags=%v err=%v", test.flags, err)
		}
		if test.bad {
			continue
		}
		var report envelope
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.DryRun != test.dry || report.CleanupPolicies[0].Level != "aggressive" {
			t.Fatalf("flags=%v report=%s", test.flags, out)
		}
	}
}
func TestInvalidCleanupOptionsBeforeInspection(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, flags := range [][]string{{"--level", "reckless"}, {"--level", ""}, {"--level", " "}, {"--level", "balanced", "--discard-local-changes"}, {"--apply", "--dry-run"}, {"--days", "0"}, {"-d", "-1"}} {
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
	for _, want := range []string{"Clean · apply", "policy:   aggressive", "including local changes", "branch refs are retained"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q from %s", want, out)
		}
	}
}
func TestCleanupLevelFlagsDoNotLeak(t *testing.T) {
	root := t.TempDir()
	_, _, err := executeCommand(t, t.Context(), "clean", "--root", root, "--level", "conservative", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCommand(t, t.Context(), "clean", "--root", root)
	if err != nil || !strings.Contains(out, "policy:   aggressive") || !strings.Contains(out, "Clean · apply") {
		t.Fatalf("out=%s err=%v", out, err)
	}
}
