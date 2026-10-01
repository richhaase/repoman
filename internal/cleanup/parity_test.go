package cleanup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

func aggressiveRealEngine() engine {
	e := realEngine()
	e.options = Options{Level: Aggressive}
	e.lookup = func(ctx context.Context, s repository.State) (evidence, error) {
		return lookupEvidenceForLevel(ctx, s, func(context.Context, string) ([]pullRequest, error) { return nil, nil }, Aggressive)
	}
	e.remove = func(ctx context.Context, s repository.State) error {
		return removeWorktreeWithOptions(ctx, s, e.options)
	}
	e.prune = pruneWorktrees
	return e
}

func TestAggressiveRealLocalDataMatrix(t *testing.T) {
	for _, kind := range []string{"unpublished branch", "unpublished detached", "assume-unchanged", "skip-worktree", "stash", "nested ignored repository", "large sparse file", "operation marker"} {
		t.Run(kind, func(t *testing.T) {
			root, s := realWorktree(t)
			switch kind {
			case "unpublished branch", "unpublished detached":
				if kind == "unpublished detached" {
					testGit(t, s.Path, "checkout", "--detach")
				}
				if err := os.WriteFile(filepath.Join(s.Path, "tracked.txt"), []byte("unpublished\n"), 0600); err != nil {
					t.Fatal(err)
				}
				testGit(t, s.Path, "commit", "-am", "unpublished temporary fixture")
			case "assume-unchanged", "skip-worktree":
				testGit(t, s.Path, "update-index", "--"+kind, "tracked.txt")
				if err := os.WriteFile(filepath.Join(s.Path, "tracked.txt"), []byte("hidden local data\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "stash":
				if err := os.WriteFile(filepath.Join(s.Path, "tracked.txt"), []byte("stash data\n"), 0600); err != nil {
					t.Fatal(err)
				}
				testGit(t, s.Path, "stash", "push")
			case "nested ignored repository":
				nested := filepath.Join(s.Path, "nested")
				if err := os.Mkdir(nested, 0700); err != nil {
					t.Fatal(err)
				}
				testGit(t, nested, "init", "-b", "main")
				testGit(t, nested, "config", "user.name", "Fixture")
				testGit(t, nested, "config", "user.email", "fixture@example.invalid")
				if err := os.WriteFile(filepath.Join(nested, "data.txt"), []byte("nested unpublished data"), 0600); err != nil {
					t.Fatal(err)
				}
				testGit(t, nested, "add", ".")
				testGit(t, nested, "commit", "-m", "temporary nested data")
				if err := os.WriteFile(filepath.Join(s.CommonDir, "info", "exclude"), []byte("nested/\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "large sparse file":
				f, err := os.Create(filepath.Join(s.Path, "large"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(1 << 30); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			case "operation marker":
				if err := os.WriteFile(filepath.Join(s.GitDir, "MERGE_HEAD"), []byte(s.Head+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			head := testGit(t, s.Path, "rev-parse", "HEAD")
			e := aggressiveRealEngine()
			preview, err := e.run(t.Context(), root, false)
			if err != nil || len(preview) != 2 || preview[1].Action != ActionWouldRemove || !preview[1].Destructive {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			if _, err := os.Stat(s.Path); err != nil {
				t.Fatal("preview changed checkout", err)
			}
			result, err := e.run(t.Context(), root, true)
			if err != nil || result[1].Action != ActionRemoved || !result[1].Destructive {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checkout remains: %v", err)
			}
			testGit(t, filepath.Join(root, "primary"), "show-ref", "--verify", "refs/heads/topic")
			if kind == "unpublished branch" && testGit(t, filepath.Join(root, "primary"), "rev-parse", "topic") != head {
				t.Fatal("branch commit changed")
			}
			if kind == "stash" {
				testGit(t, filepath.Join(root, "primary"), "rev-parse", "refs/stash")
			}
		})
	}
}

func TestAggressiveRegisteredOutsideRootAndEmptyPrimary(t *testing.T) {
	root, s := realWorktree(t)
	outside := filepath.Join(t.TempDir(), "outside-worktree")
	testGit(t, filepath.Join(root, "primary"), "worktree", "move", s.Path, outside)
	empty := filepath.Join(root, "empty-primary")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, empty, "init", "-b", "main")
	e := aggressiveRealEngine()
	results, err := e.run(t.Context(), root, true)
	if err != nil {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	removed := false
	for _, r := range results {
		if r.Path == outside {
			removed = r.Action == ActionRemoved
		}
	}
	if !removed {
		t.Fatalf("outside registered worktree was not removed: %+v", results)
	}
	if _, err := os.Stat(filepath.Join(empty, ".git")); err != nil {
		t.Fatal("empty primary lost", err)
	}
}

func TestAggressiveCwdAndOpenPRProtection(t *testing.T) {
	for _, kind := range []string{"locked", "open PR", "own cwd", "primary cwd", "open file only"} {
		t.Run(kind, func(t *testing.T) {
			root, s := realWorktree(t)
			e := aggressiveRealEngine()
			switch kind {
			case "locked":
				testGit(t, filepath.Join(root, "primary"), "worktree", "lock", s.Path)
			case "open PR":
				e.lookup = func(context.Context, repository.State) (evidence, error) { return evidence{reason: "open PR"}, nil }
			case "own cwd", "primary cwd":
				cwd := s.Path
				if kind == "primary cwd" {
					cwd = filepath.Join(root, "primary")
				}
				t.Chdir(cwd)
				e.discover = repository.Discover
				e.inspect = repository.Inspect
			case "open file only":
				baseDiscover, baseInspect := e.discover, e.inspect
				withFD := func(s repository.State) repository.State {
					s.InUse = true
					s.SafetyProblems = []string{"optional fd observation failed"}
					return s
				}
				e.discover = func(ctx context.Context, root string) ([]repository.State, error) {
					ss, err := baseDiscover(ctx, root)
					for i := range ss {
						ss[i] = withFD(ss[i])
					}
					return ss, err
				}
				e.inspect = func(ctx context.Context, path string) repository.State { return withFD(baseInspect(ctx, path)) }
			}
			results, err := e.run(t.Context(), root, true)
			if err != nil {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			expected := ActionKeep
			if kind == "primary cwd" || kind == "open file only" {
				expected = ActionRemoved
			}
			if len(results) != 2 || results[1].Action != expected {
				t.Fatalf("results=%+v", results)
			}
		})
	}
}

func TestOtherCurrentUserProcessCwdPreservesWorktree(t *testing.T) {
	root, s := realWorktree(t)
	child := exec.Command("sleep", "30")
	child.Dir = s.Path
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	e := aggressiveRealEngine()
	e.discover = repository.Discover
	e.inspect = repository.Inspect
	results, err := e.run(t.Context(), root, true)
	if err != nil || results[1].Action != ActionKeep || !results[1].State.CwdInUse {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestAggressiveRechecksExplicitLockCwdAndIdentity(t *testing.T) {
	for _, kind := range []string{"lock", "cwd", "identity failure", "process failure"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			s := syntheticState(t, root, "topic")
			e := fixtureEngine([]repository.State{s})
			e.options = Options{Level: Aggressive}
			e.inspect = func(context.Context, string) repository.State {
				switch kind {
				case "lock":
					s.WorktreeLocked = true
				case "cwd":
					s.CwdInUse = true
				case "identity failure":
					s.IdentityProblems = []string{"registration failed"}
				case "process failure":
					s.CwdInUseKnown = false
				}
				return s
			}
			e.remove = func(context.Context, repository.State) error { t.Fatal("changed checkout removed"); return nil }
			results, err := e.run(t.Context(), root, true)
			if err == nil || results[0].Action != ActionKeep {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestBatchPreflightFailsBeforeAnyRootMutation(t *testing.T) {
	for _, stage := range []string{"inventory", "PR planning", "PR revalidation", "process", "identity"} {
		t.Run(stage, func(t *testing.T) {
			first, firstState := realWorktree(t)
			second, _ := realWorktree(t)
			a, b := aggressiveRealEngine(), aggressiveRealEngine()
			calls := 0
			switch stage {
			case "inventory":
				b.discover = func(context.Context, string) ([]repository.State, error) {
					return nil, errors.New("unreadable second target")
				}
			case "PR planning", "PR revalidation":
				b.lookup = func(context.Context, repository.State) (evidence, error) {
					calls++
					if stage == "PR planning" || calls == 2 {
						return evidence{}, errors.New("API failed")
					}
					return evidence{eligible: true}, nil
				}
			case "process", "identity":
				discover := b.discover
				b.discover = func(ctx context.Context, root string) ([]repository.State, error) {
					ss, err := discover(ctx, root)
					for i := range ss {
						if stage == "process" {
							ss[i].CwdInUseKnown = false
						} else {
							ss[i].IdentityProblems = []string{"Git identity failed"}
						}
					}
					return ss, err
				}
			}
			results, err := runEngines(t.Context(), []engine{a, b}, []string{first, second}, true)
			if err == nil {
				t.Fatalf("expected batch failure: %+v", results)
			}
			for _, r := range results {
				if r.Action == ActionRemoved || r.Action == ActionWouldRemove || r.Destructive {
					t.Fatalf("unsafe partial action: %+v", results)
				}
			}
			if _, err := os.Stat(firstState.Path); err != nil {
				t.Fatal("first root changed before second preflight", err)
			}
		})
	}
}

func TestBatchDeduplicationAndConflictingPolicies(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "deduplicate", true: "conflict"}[conflict], func(t *testing.T) {
			root, s := realWorktree(t)
			a, b := aggressiveRealEngine(), aggressiveRealEngine()
			if conflict {
				b.options.Level = Balanced
			}
			results, err := runEngines(t.Context(), []engine{a, b}, []string{root, root}, true)
			if conflict {
				if err == nil || !strings.Contains(err.Error(), "conflicting cleanup levels") {
					t.Fatalf("results=%+v err=%v", results, err)
				}
				if _, err := os.Stat(s.Path); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil || len(results) != 2 || results[1].Action != ActionRemoved {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestRealPrunableMetadataUsesGitExpiryAndHonorsLocks(t *testing.T) {
	for _, kind := range []string{"expired", "recent", "locked"} {
		t.Run(kind, func(t *testing.T) {
			root, s := realWorktree(t)
			if kind == "locked" {
				testGit(t, filepath.Join(root, "primary"), "worktree", "lock", s.Path)
			}
			if err := os.RemoveAll(s.Path); err != nil {
				t.Fatal(err)
			}
			if kind == "expired" {
				old := time.Now().AddDate(-1, 0, 0)
				if err := os.Chtimes(filepath.Join(s.GitDir, "gitdir"), old, old); err != nil {
					t.Fatal(err)
				}
			}
			e := aggressiveRealEngine()
			preview, err := e.run(t.Context(), root, false)
			expected := ActionWouldPrune
			if kind == "locked" {
				expected = ActionKeep
			}
			if err != nil || preview[1].Action != expected || preview[1].Destructive {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			if _, err := os.Stat(s.GitDir); err != nil {
				t.Fatal("preview pruned metadata", err)
			}
			result, err := e.run(t.Context(), root, true)
			if err != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			_, statErr := os.Stat(s.GitDir)
			if errors.Is(statErr, os.ErrNotExist) {
				if result[1].Action != ActionPruned {
					t.Fatalf("pruned action missing: %+v", result)
				}
			} else if statErr != nil || result[1].Action != ActionKeep {
				t.Fatalf("retained registration misreported: %+v %v", result, statErr)
			}
			if kind == "expired" && result[1].Action != ActionPruned {
				t.Fatalf("expired metadata retained: %+v", result)
			}
			if kind == "locked" && result[1].Action != ActionKeep {
				t.Fatalf("lock not honored: %+v", result)
			}
			testGit(t, filepath.Join(root, "primary"), "show-ref", "--verify", "refs/heads/topic")
		})
	}
}

func TestAggressiveOptionalDiagnosticsDoNotBecomeIdentityFailures(t *testing.T) {
	root := t.TempDir()
	s := syntheticState(t, root, "topic")
	s.Problems = []string{"status failed", "index cannot be inspected", "upstream unavailable"}
	e := fixtureEngine([]repository.State{s})
	e.options = Options{Level: Aggressive}
	results, err := e.run(t.Context(), root, true)
	if err != nil || results[0].Action != ActionRemoved || !results[0].Destructive {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestBareAnchorDiscoversAndRemovesRegisteredWorktree(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "anchor.git")
	if err := os.Mkdir(bare, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, bare, "init", "--bare", "-b", "main")
	testGit(t, bare, "config", "user.name", "Fixture")
	testGit(t, bare, "config", "user.email", "fixture@example.invalid")
	outside := filepath.Join(t.TempDir(), "bare-linked")
	testGit(t, bare, "worktree", "add", "--orphan", "-b", "topic", outside)
	if err := os.WriteFile(filepath.Join(outside, "tracked.txt"), []byte("bare-anchor fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, outside, "add", ".")
	testGit(t, outside, "commit", "-m", "fixture")
	e := aggressiveRealEngine()
	results, err := e.run(t.Context(), root, true)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	for _, r := range results {
		if r.Path == bare && (!r.State.Bare || !r.State.Primary || r.Action != ActionKeep) {
			t.Fatalf("bare anchor not protected: %+v", r)
		}
		if r.Path == outside && r.Action != ActionRemoved {
			t.Fatalf("bare linked worktree not removed: %+v", r)
		}
	}
	testGit(t, bare, "show-ref", "--verify", "refs/heads/topic")
}

func TestCleanupBashNegatedSelection(t *testing.T) {
	root, s := realWorktree(t)
	e := aggressiveRealEngine()
	e.includes = []string{"[!a]*"}
	result, err := e.run(t.Context(), root, false)
	if err != nil || result[1].Path != s.Path || result[1].Action != ActionWouldRemove {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	e.includes = []string{"[!p]*"}
	result, err = e.run(t.Context(), root, false)
	if err != nil || result[1].Action != ActionKeep {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPruneFailureRetainsVerifiedPartialResults(t *testing.T) {
	root, s := realWorktree(t)
	if err := os.RemoveAll(s.Path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(-1, 0, 0)
	if err := os.Chtimes(filepath.Join(s.GitDir, "gitdir"), old, old); err != nil {
		t.Fatal(err)
	}
	e := aggressiveRealEngine()
	e.prune = func(ctx context.Context, s repository.State) error {
		if err := pruneWorktrees(ctx, s); err != nil {
			return err
		}
		return errors.New("failure after metadata mutation")
	}
	results, err := e.run(t.Context(), root, true)
	if err == nil || results[1].Action != ActionPruned {
		t.Fatalf("partial pruning misreported: %+v %v", results, err)
	}
}

func TestMissingNonPrunableRegistrationFailsBatch(t *testing.T) {
	root := t.TempDir()
	s := syntheticState(t, root, "missing")
	s.Missing = true
	s.IdentityProblems = []string{"registered worktree is missing but not prunable"}
	e := fixtureEngine([]repository.State{s})
	e.options = Options{Level: Aggressive}
	results, err := e.run(t.Context(), root, true)
	if err == nil || results[0].Action != ActionKeep {
		t.Fatalf("invalid missing registration accepted: %+v %v", results, err)
	}
}

func TestPruneDefersForProtectedDeletedWorkingDirectory(t *testing.T) {
	root, s := realWorktree(t)
	primary := filepath.Join(root, "primary")
	second := filepath.Join(root, "second-stale")
	testGit(t, primary, "worktree", "add", "-b", "second", second)
	secondState := repository.Inspect(t.Context(), second)
	child := exec.Command("sleep", "30")
	child.Dir = s.Path
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	for _, state := range []repository.State{s, secondState} {
		if err := os.RemoveAll(state.Path); err != nil {
			t.Fatal(err)
		}
		old := time.Now().AddDate(-1, 0, 0)
		if err := os.Chtimes(filepath.Join(state.GitDir, "gitdir"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	e := aggressiveRealEngine()
	e.discover = repository.Discover
	e.inspect = repository.Inspect
	for _, apply := range []bool{false, true} {
		results, err := e.run(t.Context(), root, apply)
		if err != nil {
			t.Fatalf("results=%+v err=%v", results, err)
		}
		for _, r := range results {
			if r.Action != ActionKeep {
				t.Fatalf("protected stale metadata was pruned: %+v", results)
			}
		}
		for _, state := range []repository.State{s, secondState} {
			if _, err := os.Stat(state.GitDir); err != nil {
				t.Fatal("registration lost", state.Path, err)
			}
		}
	}
}

func TestAllKeptApplyDoesNotRevalidateOrMutate(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	engines := make([]engine, len(roots))
	discoveries := make([]int, len(roots))
	for i, root := range roots {
		state := syntheticState(t, root, "retained")
		if i == 0 {
			state.Primary = true
		} else {
			state.Untracked = true
		}
		e := fixtureEngine([]repository.State{state})
		e.options = Options{Level: Balanced}
		e.discover = func(context.Context, string) ([]repository.State, error) {
			discoveries[i]++
			if discoveries[i] > 1 {
				t.Fatal("all-kept apply revalidated unchanged targets")
			}
			return []repository.State{state}, nil
		}
		e.inspect = func(context.Context, string) repository.State {
			t.Fatal("all-kept apply inspected a mutation candidate")
			return state
		}
		e.lookup = func(context.Context, repository.State) (evidence, error) {
			t.Fatal("protected checkout requested PR evidence")
			return evidence{}, nil
		}
		e.remove = func(context.Context, repository.State) error {
			t.Fatal("all-kept apply removed a checkout")
			return nil
		}
		e.prune = func(context.Context, repository.State) error { t.Fatal("all-kept apply pruned metadata"); return nil }
		engines[i] = e
	}
	results, err := runEngines(t.Context(), engines, roots, true)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	for i, result := range results {
		if discoveries[i] != 1 || result.Action != ActionKeep || result.Destructive {
			t.Fatalf("unexpected no-op result: discoveries=%v results=%+v", discoveries, results)
		}
	}
}
