package cleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/repository"
)

func TestCleanupLevelOptions(t *testing.T) {
	for _, test := range []struct {
		name    string
		options Options
		apply   bool
		want    Level
		bad     bool
	}{
		{name: "default", want: Conservative},
		{name: "conservative apply", options: Options{Level: Conservative}, apply: true, want: Conservative},
		{name: "balanced apply", options: Options{Level: Balanced}, apply: true, want: Balanced},
		{name: "aggressive preview", options: Options{Level: Aggressive}, want: Aggressive},
		{name: "aggressive acknowledged", options: Options{Level: Aggressive, DiscardLocalChanges: true}, apply: true, want: Aggressive},
		{name: "aggressive unacknowledged", options: Options{Level: Aggressive}, apply: true, bad: true},
		{name: "irrelevant acknowledgement", options: Options{Level: Balanced, DiscardLocalChanges: true}, bad: true},
		{name: "default acknowledgement", options: Options{DiscardLocalChanges: true}, bad: true},
		{name: "invalid", options: Options{Level: "reckless"}, bad: true},
		{name: "normalized", options: Options{Level: " BALANCED "}, want: Balanced},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := test.options.validate(test.apply)
			if (err != nil) != test.bad || !test.bad && options.Level != test.want {
				t.Fatalf("options=%+v error=%v", options, err)
			}
			if test.bad {
				e := fixtureEngine(nil)
				e.options = test.options
				e.discover = func(context.Context, string) ([]repository.State, error) {
					t.Fatal("invalid options must fail before inventory")
					return nil, nil
				}
				if _, err := e.run(context.Background(), t.TempDir(), test.apply); err == nil {
					t.Fatal("invalid options accepted by engine")
				}
			}
		})
	}
}

func TestProtectionsAtEveryLevel(t *testing.T) {
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, test := range []struct {
			name   string
			modify func(*repository.State)
		}{
			{"primary", func(s *repository.State) { s.Primary = true }},
			{"locked", func(s *repository.State) { s.Locked = true }},
			{"stash", func(s *repository.State) { s.Stash = true }},
			{"unique", func(s *repository.State) { s.Unique = true }},
			{"ahead", func(s *repository.State) { s.Ahead = 1 }},
			{"in use", func(s *repository.State) { s.InUse = true }},
			{"unknown use", func(s *repository.State) { s.InUseKnown = false }},
			{"unknown state", func(s *repository.State) { s.Problems = []string{"unknown"} }},
			{"safety problem", func(s *repository.State) { s.SafetyProblems = []string{"unknown"} }},
			{"hidden index", func(s *repository.State) { s.Problems = []string{"skip-worktree"} }},
			{"submodule", func(s *repository.State) { s.Problems = []string{"submodule"} }},
			{"identity", func(s *repository.State) { s.Head = "" }},
			{"relative", func(s *repository.State) { s.Path = "relative" }},
		} {
			t.Run(string(level)+"/"+test.name, func(t *testing.T) {
				root := t.TempDir()
				state := syntheticState(t, root, "topic")
				state.Dirty, state.Untracked, state.Ignored = level == Aggressive, level == Aggressive, level == Aggressive
				test.modify(&state)
				e := fixtureEngine([]repository.State{state})
				e.options = Options{Level: level, DiscardLocalChanges: level == Aggressive}
				e.lookup = func(context.Context, repository.State) (evidence, error) {
					t.Fatal("protected state queried GitHub")
					return evidence{}, nil
				}
				e.remove = func(context.Context, repository.State) error { t.Fatal("protected state removed"); return nil }
				results, err := e.run(context.Background(), root, true)
				if err != nil || len(results) != 1 || results[0].Action != ActionKeep || results[0].Destructive || results[0].Level != level {
					t.Fatalf("results=%+v error=%v", results, err)
				}
			})
		}
	}
}

func TestLocalContentEligibilityByLevel(t *testing.T) {
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, kind := range []string{"clean", "dirty", "untracked", "ignored"} {
			t.Run(string(level)+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				state := syntheticState(t, root, "topic")
				state.Dirty, state.Untracked, state.Ignored = kind == "dirty", kind == "untracked", kind == "ignored"
				e := fixtureEngine([]repository.State{state})
				e.options = Options{Level: level}
				results, err := e.run(context.Background(), root, false)
				wantAction := ActionKeep
				if level == Aggressive || kind == "clean" {
					wantAction = ActionWouldRemove
				}
				if err != nil || results[0].Action != wantAction || results[0].Destructive != (level == Aggressive && kind != "clean") {
					t.Fatalf("results=%+v error=%v", results, err)
				}
			})
		}
	}
}

func TestLevelGitHubProofMatrix(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, test := range []struct {
			name       string
			associated []pullRequest
			branch     []pullRequest
			origin     string
			failAt     int
			eligible   bool
			bad        bool
		}{
			{name: "known no PR", eligible: level != Conservative},
			{name: "exact terminal", associated: []pullRequest{testPR("closed", head, "topic", "owner/repo")}, eligible: true},
			{name: "older terminal", associated: []pullRequest{testPR("closed", strings.Repeat("b", 40), "topic", "owner/repo")}, eligible: level != Conservative},
			{name: "open commit", associated: []pullRequest{testPR("open", head, "topic", "owner/repo")}},
			{name: "open branch", branch: []pullRequest{testPR("open", head, "topic", "owner/repo")}},
			{name: "unsupported origin", origin: "https://gitlab.com/owner/repo"},
			{name: "missing origin", origin: "none"},
			{name: "commit failure", failAt: 1, bad: true},
			{name: "branch failure", failAt: 2, bad: true},
			{name: "incomplete commit response", associated: []pullRequest{{}}, bad: true},
			{name: "incomplete branch response", branch: []pullRequest{{}}, bad: true},
		} {
			t.Run(string(level)+"/"+test.name, func(t *testing.T) {
				origin := test.origin
				if origin == "" {
					origin = "https://github.com/owner/repo.git"
				}
				if origin == "none" {
					origin = ""
				}
				state := repository.State{Head: head, Branch: "topic", Origin: origin}
				calls := 0
				proof, err := lookupEvidenceForLevel(context.Background(), state, func(context.Context, string) ([]pullRequest, error) {
					calls++
					if calls == test.failAt {
						return nil, errors.New("offline")
					}
					if calls == 1 {
						return test.associated, nil
					}
					return test.branch, nil
				}, level)
				if (err != nil) != test.bad || proof.eligible != test.eligible {
					t.Fatalf("proof=%+v error=%v calls=%d", proof, err, calls)
				}
				if proof.eligible && calls != 2 {
					t.Fatalf("eligible without complete commit and branch checks: %d", calls)
				}
			})
		}
	}
}

func TestAggressiveDetectsContentChangesWithUnchangedStatus(t *testing.T) {
	for _, phase := range []string{"remote revalidation", "immediate inspection", "after first removal"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			a, b := syntheticState(t, root, "a"), syntheticState(t, root, "b")
			a.Dirty, b.Dirty = true, true
			path := filepath.Join(b.Path, "dirty.txt")
			if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			change := func() {
				if err := os.WriteFile(path, []byte("newest"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Same length and restored timestamp defeats metadata-only checks.
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			e := fixtureEngine([]repository.State{a, b})
			e.options = Options{Level: Aggressive, DiscardLocalChanges: true}
			calls, removed := 0, 0
			e.lookup = func(context.Context, repository.State) (evidence, error) {
				calls++
				if phase == "remote revalidation" && calls == 3 {
					change()
				}
				return evidence{eligible: true}, nil
			}
			e.inspect = func(_ context.Context, path string) repository.State {
				if phase == "immediate inspection" {
					change()
				}
				if path == a.Path {
					return a
				}
				return b
			}
			e.remove = func(context.Context, repository.State) error {
				removed++
				if phase == "after first removal" {
					change()
				}
				return nil
			}
			results, err := e.run(context.Background(), root, true)
			wantRemoved := 1
			if phase == "remote revalidation" {
				wantRemoved = 0
			}
			if err == nil || removed != wantRemoved || results[1].Action != ActionKeep || results[1].Destructive {
				t.Fatalf("removed=%d results=%+v error=%v", removed, results, err)
			}
			if removed == 1 && (results[0].Action != ActionRemoved || !results[0].Destructive) {
				t.Fatalf("partial result lost actual destruction: %+v", results)
			}
		})
	}
}

func realEngine() engine {
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
	return e
}

func TestRealAggressiveCleanupRequiresAcknowledgementAndPreservesBranches(t *testing.T) {
	for _, kind := range []string{"dirty", "staged", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			root, state := realWorktree(t)
			path := filepath.Join(state.Path, "tracked.txt")
			if kind == "untracked" || kind == "ignored" {
				path = filepath.Join(state.Path, "disposable.txt")
			}
			if kind == "ignored" {
				if err := os.WriteFile(filepath.Join(state.CommonDir, "info", "exclude"), []byte("disposable.txt\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte("test-owned disposable content\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "staged" {
				testGit(t, state.Path, "add", "tracked.txt")
			}
			e := realEngine()
			e.options = Options{Level: Aggressive}
			e.remove = func(ctx context.Context, s repository.State) error {
				return removeWorktreeWithOptions(ctx, s, e.options)
			}
			if _, err := e.run(context.Background(), root, true); err == nil {
				t.Fatal("aggressive apply accepted without invocation acknowledgement")
			}
			results, err := e.run(context.Background(), root, false)
			if err != nil || results[1].Action != ActionWouldRemove || !results[1].Destructive {
				t.Fatalf("preview=%+v error=%v", results, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("preview removed local content: %v", err)
			}
			e.options.DiscardLocalChanges = true
			results, err = e.run(context.Background(), root, true)
			if err != nil || results[0].Action != ActionKeep || results[1].Action != ActionRemoved || !results[1].Destructive {
				t.Fatalf("apply=%+v error=%v", results, err)
			}
			if _, err := os.Stat(state.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree remains: %v", err)
			}
			testGit(t, filepath.Join(root, "primary"), "show-ref", "--verify", "refs/heads/topic")
		})
	}
}

func TestRealAggressiveNeverDoubleForcesLock(t *testing.T) {
	root, state := realWorktree(t)
	state.Dirty = true
	if err := os.WriteFile(filepath.Join(state.Path, "tracked.txt"), []byte("local content"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, filepath.Join(root, "primary"), "worktree", "lock", state.Path)
	if err := removeWorktreeWithOptions(context.Background(), state, Options{Level: Aggressive, DiscardLocalChanges: true}); err == nil {
		t.Fatal("locked worktree was force removed")
	}
	if _, err := os.Stat(state.Path); err != nil {
		t.Fatal("locked worktree lost", err)
	}
}

func TestRootAliasUsesCanonicalContainment(t *testing.T) {
	root, state := realWorktree(t)
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	e := realEngine()
	results, err := e.run(context.Background(), alias, false)
	if err != nil || len(results) != 2 || results[1].Path != state.Path || results[1].Action != ActionWouldRemove {
		t.Fatalf("canonical root alias not accepted: results=%+v error=%v", results, err)
	}
}

func TestChangedRootAliasBlocksApply(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	e := fixtureEngine([]repository.State{state})
	calls := 0
	e.lookup = func(context.Context, repository.State) (evidence, error) {
		calls++
		if calls == 2 {
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), alias); err != nil {
				t.Fatal(err)
			}
		}
		return evidence{eligible: true}, nil
	}
	e.remove = func(context.Context, repository.State) error { t.Fatal("changed root alias removed"); return nil }
	results, err := e.run(context.Background(), alias, true)
	if err == nil || results[0].Action != ActionKeep {
		t.Fatalf("changed alias accepted: results=%+v error=%v", results, err)
	}
}

func TestFailedProofBlocksAllLevelsBeforeAnyDeletion(t *testing.T) {
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, failAt := range []int{2, 4} {
			t.Run(string(level)+map[int]string{2: "/planning", 4: "/revalidation"}[failAt], func(t *testing.T) {
				root := t.TempDir()
				a, b := syntheticState(t, root, "a"), syntheticState(t, root, "b")
				a.Dirty, b.Dirty = level == Aggressive, level == Aggressive
				e := fixtureEngine([]repository.State{a, b})
				e.options = Options{Level: level, DiscardLocalChanges: level == Aggressive}
				calls := 0
				e.lookup = func(context.Context, repository.State) (evidence, error) {
					calls++
					if calls == failAt {
						return evidence{}, errors.New("offline")
					}
					return evidence{eligible: true}, nil
				}
				e.remove = func(context.Context, repository.State) error { t.Fatal("partial proof authorized removal"); return nil }
				results, err := e.run(context.Background(), root, true)
				if err == nil || len(results) != 2 {
					t.Fatalf("results=%+v error=%v", results, err)
				}
				for _, result := range results {
					if result.Action != ActionKeep || result.Destructive {
						t.Fatalf("unsafe pending plan: %+v", results)
					}
				}
			})
		}
	}
}

func TestRealHiddenIndexAndSubmoduleProtectionsAtEveryLevel(t *testing.T) {
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, kind := range []string{"assume-unchanged", "skip-worktree", "submodule"} {
			t.Run(string(level)+"/"+kind, func(t *testing.T) {
				root, state := realWorktree(t)
				if kind == "submodule" {
					testGit(t, state.Path, "update-index", "--add", "--cacheinfo", "160000,"+state.Head+",submodule")
				} else {
					testGit(t, state.Path, "update-index", "--"+kind, "tracked.txt")
					if err := os.WriteFile(filepath.Join(state.Path, "tracked.txt"), []byte("hidden local work"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				e := realEngine()
				e.options = Options{Level: level, DiscardLocalChanges: level == Aggressive}
				e.lookup = func(context.Context, repository.State) (evidence, error) {
					t.Fatal("protected index queried GitHub")
					return evidence{}, nil
				}
				e.remove = func(context.Context, repository.State) error { t.Fatal("protected index removed"); return nil }
				results, err := e.run(context.Background(), root, true)
				if err != nil || results[1].Action != ActionKeep || results[1].Destructive {
					t.Fatalf("results=%+v error=%v", results, err)
				}
			})
		}
	}
}
