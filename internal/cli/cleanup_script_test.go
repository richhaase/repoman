package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/cleanup"
	"github.com/richhaase/repoman/internal/repository"
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
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
		out, diag, err := executeCleanupFixtureCommand(t, []string{root}, invocation...)
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
			t.Fatalf("args=%v out=%s err=%v", args, out, err)
		}
		if len(report.Errors) != 0 {
			t.Fatalf("unexpected structured errors: %s", out)
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
	missing := filepath.Join(t.TempDir(), "missing")
	data, err := json.Marshal(map[string]any{"targets": []any{map[string]any{"dir": root}, map[string]any{"dir": missing}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeCleanupFixtureCommand(t, []string{root, missing}, "clean", "--config", config, "--json")
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

// fixtureCleanupInventory keeps all Git inspection real and bounds access to
// test-created roots. These fixtures have no running processes in their
// checkouts. Host-wide visibility is tested independently; privileged runner
// processes must not make these Git/CLI integration tests nondeterministic.
type fixtureCleanupInventory struct {
	t     *testing.T
	roots []string
}

func (f fixtureCleanupInventory) checkPath(path string, exact bool) {
	f.t.Helper()
	for _, root := range f.roots {
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			root = canonical
		}
		if path == root {
			return
		}
		relative, err := filepath.Rel(root, path)
		if !exact && err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return
		}
	}
	f.t.Fatalf("cleanup test attempted inventory outside its fixtures: %q", path)
}

func fixtureKnownUsage(s repository.State) repository.State {
	s.InUse, s.InUseKnown, s.SafetyProblems = false, true, nil
	s.CwdInUse, s.CwdInUseKnown, s.CwdProblems = false, true, nil
	return s
}

func (f fixtureCleanupInventory) Discover(ctx context.Context, root string) ([]repository.State, error) {
	f.checkPath(root, true)
	states, err := repository.Discover(ctx, root)
	for i := range states {
		f.checkPath(states[i].Path, false)
		states[i] = fixtureKnownUsage(states[i])
	}
	return states, err
}

func (f fixtureCleanupInventory) Inspect(ctx context.Context, path string) repository.State {
	f.checkPath(path, false)
	return fixtureKnownUsage(repository.Inspect(ctx, path))
}

func executeCleanupFixtureCommand(t *testing.T, roots []string, args ...string) (string, string, error) {
	t.Helper()
	inventory := fixtureCleanupInventory{t: t, roots: roots}
	return executeCleanCommandWithInventory(t, inventory, args...)
}

func executeCleanCommandWithInventory(t *testing.T, inventory cleanup.Inventory, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{Version: "test"})
	for _, child := range cmd.Commands() {
		if child.Name() == "clean" {
			cmd.RemoveCommand(child)
		}
	}
	cmd.AddCommand(newCleanCmdWithRunner(func(ctx context.Context, targets []cleanup.Target, apply bool) ([]cleanup.Result, error) {
		return cleanup.RunBatchWithInventory(ctx, targets, apply, inventory)
	}))
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), diag.String(), err
}

// The injected failures are deterministic equivalents of inaccessible same-user
// processes. The actual cleanup engine must still block the complete batch.
type failedCleanupObservation struct {
	inventory fixtureCleanupInventory
	cwd       bool
	inUse     bool
}

func (f failedCleanupObservation) state(s repository.State) repository.State {
	s.CwdInUse = f.inUse
	if f.cwd {
		s.CwdInUseKnown = false
		s.CwdProblems = []string{"fixture cwd observation denied"}
	} else {
		s.InUseKnown = false
		s.SafetyProblems = []string{"fixture process observation denied"}
	}
	return s
}
func (f failedCleanupObservation) Discover(ctx context.Context, root string) ([]repository.State, error) {
	states, err := f.inventory.Discover(ctx, root)
	for i := range states {
		states[i] = f.state(states[i])
	}
	return states, err
}
func (f failedCleanupObservation) Inspect(ctx context.Context, path string) repository.State {
	return f.state(f.inventory.Inspect(ctx, path))
}

func TestCleanCLIObservationPolicyUsesActualGit(t *testing.T) {
	for _, test := range []struct {
		level string
		inUse bool
	}{{"aggressive", false}, {"aggressive", true}, {"balanced", false}, {"conservative", false}} {
		name := test.level
		if test.inUse {
			name += " observed cwd"
		}
		t.Run(name, func(t *testing.T) {
			root, linked := cleanupCLIFixture(t)
			inventory := failedCleanupObservation{inventory: fixtureCleanupInventory{t: t, roots: []string{root}}, cwd: test.level == "aggressive", inUse: test.inUse}
			out, diag, err := executeCleanCommandWithInventory(t, inventory, "clean", "--root", root, "--level", test.level, "--json")
			var report struct {
				Items    []cleanup.Result `json:"items"`
				Errors   []string         `json:"errors"`
				Warnings []string         `json:"warnings"`
			}
			if parseErr := json.Unmarshal([]byte(out), &report); parseErr != nil {
				t.Fatal(parseErr)
			}
			if len(report.Items) != 2 {
				t.Fatal("missing result evidence", out)
			}
			if test.level == "aggressive" {
				if err != nil || len(report.Errors) != 0 || len(report.Warnings) != 1 || strings.Count(diag, "warning:") != 1 || !strings.Contains(diag, "Unobserved processes may still use eligible worktrees") {
					t.Fatalf("best-effort observation not reported: out=%s diag=%s err=%v", out, diag, err)
				}
			} else {
				var coded *ExitError
				if !errors.As(err, &coded) || coded.Code != 3 || diag != "" || len(report.Errors) == 0 {
					t.Fatalf("strict unknown observation not blocked: out=%s diag=%s err=%v", out, diag, err)
				}
			}
			for _, item := range report.Items {
				expected := cleanup.ActionKeep
				if item.Path == linked && test.level == "aggressive" && !test.inUse {
					expected = cleanup.ActionRemoved
				}
				if item.Action != expected {
					t.Fatalf("unexpected observation policy: %s", out)
				}
				if test.level == "aggressive" && item.State.CwdInUseKnown || test.level != "aggressive" && item.State.InUseKnown {
					t.Fatal("unknown state was hidden", out)
				}
			}
			content, readErr := os.ReadFile(filepath.Join(linked, "untracked.txt"))
			if test.level == "aggressive" && !test.inUse {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("default did not remove fixture: %v", readErr)
				}
			} else if readErr != nil || string(content) != "discardable fixture" {
				t.Fatalf("protected local data changed: %q %v", content, readErr)
			}
			cleanupCLIGit(t, filepath.Join(root, "primary"), "show-ref", "--verify", "refs/heads/topic")
		})
	}
}
