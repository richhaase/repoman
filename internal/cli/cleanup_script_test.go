package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/cleanup"
)

func cleanupCLIGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...) // #nosec G204 -- fixed executable and private test fixture paths, no shell.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func cleanupCLIFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	linked := filepath.Join(root, "linked")
	if err := os.Mkdir(primary, 0700); err != nil {
		t.Fatal(err)
	}
	cleanupCLIGit(t, primary, "init", "-b", "main")
	cleanupCLIGit(t, primary, "config", "user.name", "Fixture")
	cleanupCLIGit(t, primary, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(primary, "tracked.txt"), []byte("test-only content"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupCLIGit(t, primary, "add", ".")
	cleanupCLIGit(t, primary, "commit", "-m", "fixture")
	cleanupCLIGit(t, primary, "worktree", "add", "-b", "topic", linked)
	if err := os.WriteFile(filepath.Join(linked, "untracked.txt"), []byte("discardable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, linked
}

func TestCleanCLIRealDefaultApplyAndPreview(t *testing.T) {
	root, linked := cleanupCLIFixture(t)
	for _, args := range [][]string{{"--dry-run"}, {"--apply=false"}, {"--level", "conservative"}, {"--level", "balanced"}, nil} {
		invocation := append([]string{"clean", "--root", root, "--json"}, args...)
		out, diag, err := executeCommand(t, t.Context(), invocation...)
		if diag != "" {
			t.Fatalf("args=%v out=%s diag=%s err=%v", args, out, diag, err)
		}
		var report struct {
			SchemaVersion int              `json:"schema_version"`
			DryRun        bool             `json:"dry_run"`
			Items         []cleanup.Result `json:"items"`
			Errors        []string         `json:"errors"`
		}
		if parseErr := json.Unmarshal([]byte(out), &report); parseErr != nil {
			t.Fatalf("invalid report: %v; command error: %v; output: %s", parseErr, err, out)
		}
		if err != nil {
			// A live current-user process can transiently deny fd access in CI. Strict
			// policies correctly fail closed while the default cwd-only scope remains
			// observable. Accept only that explicitly reported failure, never a skip or
			// a generic command error; deterministic engine tests prove local retention.
			strict := len(args) == 2 && args[0] == "--level"
			var coded *ExitError
			if !strict || !errors.As(err, &coded) || coded.Code != 3 || len(report.Errors) == 0 {
				t.Fatalf("args=%v out=%s err=%v", args, out, err)
			}
			for _, failure := range report.Errors {
				for _, line := range strings.Split(failure, "\n") {
					if !strings.Contains(line, "current-user process usage could not be observed:") {
						t.Fatalf("unrelated failure must not be tolerated: %s", out)
					}
				}
			}
			unknownUse := false
			for _, item := range report.Items {
				if !item.State.InUseKnown && len(item.State.SafetyProblems) > 0 {
					unknownUse = true
				}
				if item.Action != cleanup.ActionKeep || item.Destructive {
					t.Fatalf("unknown process state allowed a mutation: %s", out)
				}
			}
			if !unknownUse {
				t.Fatalf("process failure lacks unknown-usage evidence: %s", out)
			}
		}
		found := false
		for _, r := range report.Items {
			if r.Path != linked {
				continue
			}
			found = true
			if len(args) == 0 {
				if r.Action != cleanup.ActionRemoved || !r.Destructive || report.DryRun {
					t.Fatalf("default did not remove: %s", out)
				}
			} else if r.Action == cleanup.ActionRemoved {
				t.Fatalf("preview or strict mode removed local data: %s", out)
			}
		}
		if !found {
			t.Fatal("missing linked result", out)
		}
		_, statErr := os.Stat(linked)
		if len(args) == 0 {
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("default retained checkout: %v", statErr)
			}
		} else {
			if statErr != nil {
				t.Fatalf("preview/strict changed fixture: %v", statErr)
			}
			content, readErr := os.ReadFile(filepath.Join(linked, "untracked.txt"))
			if readErr != nil || string(content) != "discardable fixture" {
				t.Fatalf("preview/strict changed local content: %q %v", content, readErr)
			}
		}
	}
	cleanupCLIGit(t, filepath.Join(root, "primary"), "show-ref", "--verify", "refs/heads/topic")
}

func TestCleanCLIPreflightsEveryConfiguredRoot(t *testing.T) {
	root, linked := cleanupCLIFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"targets": []any{map[string]any{"dir": root}, map[string]any{"dir": filepath.Join(t.TempDir(), "missing")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCommand(t, t.Context(), "clean", "--config", config, "--json")
	var coded *ExitError
	if !errors.As(err, &coded) || coded.Code != 3 || !json.Valid([]byte(out)) {
		t.Fatalf("out=%s err=%v", out, err)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("first configured root changed before all roots were checked: %v", err)
	}
	if strings.Contains(out, `"action": "removed"`) {
		t.Fatal("removal misreported", out)
	}
}
