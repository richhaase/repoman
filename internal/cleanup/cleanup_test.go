package cleanup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/repository"
)

func syntheticState(t *testing.T, root, name string) repository.State {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = canonical
	path := filepath.Join(root, name)
	common := filepath.Join(root, "primary", ".git")
	gitDir := filepath.Join(common, "worktrees", name)
	for _, directory := range []string{path, gitDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return repository.State{Path: path, CommonDir: common, GitDir: gitDir,
		Head: strings.Repeat("a", 40), Branch: name, Origin: "git@github.com:owner/repo.git", InUseKnown: true}
}

func fixtureEngine(states []repository.State) engine {
	return engine{
		discover: func(context.Context, string) ([]repository.State, error) { return states, nil },
		inspect: func(_ context.Context, path string) repository.State {
			for _, state := range states {
				if state.Path == path {
					return state
				}
			}
			return repository.State{Path: path, Problems: []string{"missing fixture"}}
		},
		lookup: func(context.Context, repository.State) (evidence, error) {
			return evidence{eligible: true, reason: "terminal PR"}, nil
		},
		remove: func(context.Context, repository.State) error { return nil },
	}
}

func TestLocalProtection(t *testing.T) {
	root := t.TempDir()
	base := syntheticState(t, root, "topic")
	tests := []struct {
		name   string
		modify func(*repository.State)
	}{
		{"primary", func(s *repository.State) { s.Primary = true }},
		{"outside", func(s *repository.State) { s.Path = filepath.Join(filepath.Dir(root), "outside") }},
		{"root", func(s *repository.State) { s.Path = root }},
		{"relative", func(s *repository.State) { s.Path = "relative" }},
		{"problem", func(s *repository.State) { s.Problems = []string{"unknown"} }},
		{"missing HEAD", func(s *repository.State) { s.Head = "" }},
		{"missing common", func(s *repository.State) { s.CommonDir = "" }},
		{"missing git", func(s *repository.State) { s.GitDir = "" }},
		{"locked", func(s *repository.State) { s.Locked = true }},
		{"dirty", func(s *repository.State) { s.Dirty = true }},
		{"untracked", func(s *repository.State) { s.Untracked = true }},
		{"ignored", func(s *repository.State) { s.Ignored = true }},
		{"stash", func(s *repository.State) { s.Stash = true }},
		{"unique", func(s *repository.State) { s.Unique = true }},
		{"ahead", func(s *repository.State) { s.Ahead = 1 }},
		{"in use", func(s *repository.State) { s.InUse = true }},
		{"unknown use", func(s *repository.State) { s.InUseKnown = false }},
		{"safety problems", func(s *repository.State) { s.SafetyProblems = []string{"unknown"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := base
			test.modify(&state)
			e := fixtureEngine([]repository.State{state})
			e.lookup = func(context.Context, repository.State) (evidence, error) {
				t.Fatal("protected worktree must not query GitHub")
				return evidence{}, nil
			}
			e.remove = func(context.Context, repository.State) error {
				t.Fatal("protected worktree must not be removed")
				return nil
			}
			results, err := e.run(context.Background(), root, true)
			if err != nil || len(results) != 1 || results[0].Action != ActionKeep || results[0].Reason == "" {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestPreviewAndApply(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[apply], func(t *testing.T) {
			root := t.TempDir()
			a := syntheticState(t, root, "a")
			z := syntheticState(t, root, "z")
			e := fixtureEngine([]repository.State{z, a})
			var order []string
			e.lookup = func(_ context.Context, s repository.State) (evidence, error) {
				order = append(order, "lookup:"+filepath.Base(s.Path))
				return evidence{eligible: true, reason: "terminal"}, nil
			}
			e.remove = func(_ context.Context, s repository.State) error {
				order = append(order, "remove:"+filepath.Base(s.Path))
				return nil
			}
			results, err := e.run(context.Background(), root, apply)
			if err != nil || len(results) != 2 || results[0].Path != a.Path || results[1].Path != z.Path {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			want := []string{"lookup:a", "lookup:z"}
			action := ActionWouldRemove
			if apply {
				want = append(want, "lookup:a", "lookup:z", "remove:a", "remove:z")
				action = ActionRemoved
			}
			if !reflect.DeepEqual(order, want) || results[0].Action != action || results[1].Action != action {
				t.Fatalf("order=%v want=%v results=%+v", order, want, results)
			}
		})
	}
}

func TestLookupFailureBlocksWholePlan(t *testing.T) {
	for _, failureCall := range []int{2, 4} {
		t.Run(map[int]string{2: "planning", 4: "revalidation"}[failureCall], func(t *testing.T) {
			root := t.TempDir()
			e := fixtureEngine([]repository.State{syntheticState(t, root, "a"), syntheticState(t, root, "z")})
			calls := 0
			e.lookup = func(context.Context, repository.State) (evidence, error) {
				calls++
				if calls == failureCall {
					return evidence{}, errors.New("offline")
				}
				return evidence{eligible: true}, nil
			}
			e.remove = func(context.Context, repository.State) error { t.Fatal("unexpected removal"); return nil }
			results, err := e.run(context.Background(), root, true)
			if err == nil || len(results) != 2 || results[0].Action != ActionKeep || results[1].Action != ActionKeep {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestChangedInventoryBlocksAllRemovals(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	e := fixtureEngine([]repository.State{state})
	calls := 0
	e.discover = func(context.Context, string) ([]repository.State, error) {
		calls++
		if calls == 2 {
			state.Dirty = true
		}
		return []repository.State{state}, nil
	}
	e.remove = func(context.Context, repository.State) error { t.Fatal("unexpected removal"); return nil }
	results, err := e.run(context.Background(), root, true)
	if err == nil || results[0].Action != ActionKeep {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestImmediateReinspectionAndPartialResults(t *testing.T) {
	root := t.TempDir()
	a, z := syntheticState(t, root, "a"), syntheticState(t, root, "z")
	e := fixtureEngine([]repository.State{a, z})
	e.inspect = func(_ context.Context, path string) repository.State {
		if path == a.Path {
			return a
		}
		changed := z
		changed.Untracked = true
		return changed
	}
	removed := 0
	e.remove = func(context.Context, repository.State) error { removed++; return nil }
	results, err := e.run(context.Background(), root, true)
	if err == nil || removed != 1 || results[0].Action != ActionRemoved || results[1].Action != ActionKeep {
		t.Fatalf("results=%+v removed=%d err=%v", results, removed, err)
	}
}

func TestPathReplacementBlocksRemoval(t *testing.T) {
	for _, replaceDuringInspect := range []bool{false, true} {
		t.Run(map[bool]string{false: "preflight", true: "immediate"}[replaceDuringInspect], func(t *testing.T) {
			root := t.TempDir()
			state := syntheticState(t, root, "topic")
			e := fixtureEngine([]repository.State{state})
			replace := func() {
				if err := os.Rename(state.Path, state.Path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(state.Path+"-original", state.Path); err != nil {
					t.Fatal(err)
				}
			}
			if replaceDuringInspect {
				e.inspect = func(context.Context, string) repository.State { replace(); return state }
			} else {
				calls := 0
				e.lookup = func(context.Context, repository.State) (evidence, error) {
					calls++
					if calls == 2 {
						replace()
					}
					return evidence{eligible: true}, nil
				}
			}
			e.remove = func(context.Context, repository.State) error { t.Fatal("unexpected removal"); return nil }
			results, err := e.run(context.Background(), root, true)
			if err == nil || results[0].Action != ActionKeep {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestFiltersAndSharedProtection(t *testing.T) {
	for _, test := range []struct {
		name     string
		includes []string
		excludes []string
		inUse    bool
	}{
		{name: "include mismatch", includes: []string{"other*"}},
		{name: "exclude match", includes: []string{"primary"}, excludes: []string{"pri*"}},
		{name: "primary in use", inUse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state := syntheticState(t, root, "topic")
			primary := state
			primary.Path, primary.GitDir, primary.Primary = filepath.Dir(state.CommonDir), state.CommonDir, true
			primary.InUse = test.inUse
			e := fixtureEngine([]repository.State{state, primary})
			e.includes, e.excludes = test.includes, test.excludes
			e.lookup = func(context.Context, repository.State) (evidence, error) {
				t.Fatal("unexpected lookup")
				return evidence{}, nil
			}
			results, err := e.run(context.Background(), root, true)
			if err != nil || len(results) != 2 || results[0].Action != ActionKeep || results[1].Action != ActionKeep {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestInvalidFilterAndCancellation(t *testing.T) {
	e := fixtureEngine(nil)
	e.includes = []string{"["}
	if _, err := e.run(context.Background(), t.TempDir(), true); err == nil {
		t.Fatal("invalid glob accepted")
	}
	root := t.TempDir()
	e = fixtureEngine([]repository.State{syntheticState(t, root, "topic")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.run(ctx, root, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
}

func testGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...) // #nosec G204 -- fixed git executable; test-owned arguments and temporary fixture paths only.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func realWorktree(t *testing.T) (string, repository.State) {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	if err := os.Mkdir(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	testGit(t, primary, "init", "-b", "main")
	testGit(t, primary, "config", "user.name", "Fixture")
	testGit(t, primary, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(primary, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, primary, "add", "tracked.txt")
	testGit(t, primary, "commit", "-m", "fixture")
	testGit(t, primary, "remote", "add", "origin", "https://github.com/owner/repo.git")
	testGit(t, primary, "update-ref", "refs/remotes/origin/main", "HEAD")
	path := filepath.Join(root, "topic")
	testGit(t, primary, "worktree", "add", "-b", "topic", path)
	state := repository.Inspect(context.Background(), path)
	if len(state.Problems) != 0 || state.Primary {
		t.Fatalf("invalid fixture: %+v", state)
	}
	return root, state
}

func TestRealGitPreviewApplyAndBranchesPreserved(t *testing.T) {
	root, state := realWorktree(t)
	// Process visibility is independently tested by repository. Keep this
	// temporary-fixture integration test deterministic in restricted CI.
	knownUsage := func(s repository.State) repository.State {
		s.InUse, s.InUseKnown, s.SafetyProblems = false, true, nil
		return s
	}
	e := fixtureEngine(nil)
	e.discover = func(ctx context.Context, root string) ([]repository.State, error) {
		states, err := repository.Discover(ctx, root)
		for i := range states {
			states[i] = knownUsage(states[i])
		}
		return states, err
	}
	e.inspect = func(ctx context.Context, path string) repository.State {
		return knownUsage(repository.Inspect(ctx, path))
	}
	e.remove = removeWorktree
	results, err := e.run(context.Background(), root, false)
	if err != nil || len(results) != 2 || results[1].Action != ActionWouldRemove {
		t.Fatalf("preview=%+v err=%v", results, err)
	}
	if _, err := os.Stat(state.Path); err != nil {
		t.Fatal("preview removed worktree", err)
	}
	results, err = e.run(context.Background(), root, true)
	if err != nil || results[1].Action != ActionRemoved || results[0].Action != ActionKeep {
		t.Fatalf("apply=%+v err=%v", results, err)
	}
	if _, err := os.Stat(state.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still exists: %v", err)
	}
	primary := filepath.Join(root, "primary")
	testGit(t, primary, "show-ref", "--verify", "refs/heads/topic")
	if got := testGit(t, primary, "worktree", "list", "--porcelain"); strings.Contains(got, state.Path) {
		t.Fatalf("removed worktree remains registered: %s", got)
	}
}

func TestRealGitNeverForcesUnsafeRemoval(t *testing.T) {
	for _, kind := range []string{"dirty", "untracked", "locked"} {
		t.Run(kind, func(t *testing.T) {
			root, state := realWorktree(t)
			switch kind {
			case "dirty":
				if err := os.WriteFile(filepath.Join(state.Path, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				if err := os.WriteFile(filepath.Join(state.Path, "important.txt"), []byte("untracked\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "locked":
				testGit(t, filepath.Join(root, "primary"), "worktree", "lock", state.Path)
			}
			if err := removeWorktree(context.Background(), state); err == nil {
				t.Fatal("unsafe git removal succeeded")
			}
			if _, err := os.Stat(state.Path); err != nil {
				t.Fatal("worktree not preserved", err)
			}
		})
	}
}

func TestRemovalIgnoresInheritedGitRouting(t *testing.T) {
	_, state := realWorktree(t)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "missing-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	if err := removeWorktree(context.Background(), state); err != nil {
		t.Fatalf("inherited Git overrides affected removal: %v", err)
	}
}

func TestFailedRemovalReportsAttemptSeparately(t *testing.T) {
	root := t.TempDir()
	a, b, c := syntheticState(t, root, "a"), syntheticState(t, root, "b"), syntheticState(t, root, "c")
	e := fixtureEngine([]repository.State{a, b, c})
	e.remove = func(_ context.Context, state repository.State) error {
		if state.Path == b.Path {
			return errors.New("removal failed")
		}
		if state.Path == c.Path {
			t.Fatal("must stop after failed removal")
		}
		return nil
	}
	results, err := e.run(context.Background(), root, true)
	if err == nil || len(results) != 3 || results[0].Action != ActionRemoved || results[1].Action != ActionFailed || results[2].Action != ActionKeep {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}
