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

func TestAggressiveProtectsNestedRepositories(t *testing.T) {
	for _, kind := range []string{"git directory", "git routing file", "git symlink", "mixed-case marker", "bare", "mixed-case bare"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			state := syntheticState(t, root, "topic")
			state.Ignored = true
			nested := filepath.Join(state.Path, "ignored", "nested")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "git directory":
				testGit(t, nested, "init", "-b", "main")
			case "git routing file":
				if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: /does/not/matter"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "git symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(nested, ".git")); err != nil {
					t.Fatal(err)
				}
			case "mixed-case marker":
				if err := os.Mkdir(filepath.Join(nested, ".GiT"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "bare":
				testGit(t, nested, "init", "--bare")
			case "mixed-case bare":
				if err := os.Mkdir(filepath.Join(nested, "Objects"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "Head"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e := fixtureEngine([]repository.State{state})
			e.options = Options{Level: Aggressive, DiscardLocalChanges: true}
			e.lookup = func(context.Context, repository.State) (evidence, error) {
				t.Fatal("nested repository queried GitHub")
				return evidence{}, nil
			}
			e.remove = func(context.Context, repository.State) error { t.Fatal("nested repository removed"); return nil }
			results, err := e.run(context.Background(), root, true)
			if err == nil || results[0].Action != ActionKeep || results[0].Destructive || !strings.Contains(results[0].Reason, "Git repository") {
				t.Fatalf("results=%+v error=%v", results, err)
			}
			if _, err := os.Stat(nested); err != nil {
				t.Fatal("nested checkout not preserved", err)
			}
		})
	}
}

func TestSnapshotNeverFollowsSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	external := t.TempDir()
	path := filepath.Join(external, "valuable.txt")
	if err := os.WriteFile(path, []byte("valuable external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(state.Path, "outside")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotContent(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed outside the checkout"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.validate(context.Background(), state); err != nil {
		t.Fatalf("snapshot followed external target: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.validate(context.Background(), state); err == nil {
		t.Fatal("changed symlink target was not detected")
	}
}

func TestSnapshotLimitsAndCancellation(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	file, err := os.Create(filepath.Join(state.Path, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotContent(context.Background(), state); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized sparse file not protected: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snapshotContent(ctx, state); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error=%v", err)
	}
}

func TestSnapshotDetectsStagedIndexChange(t *testing.T) {
	_, state := realWorktree(t)
	if err := os.WriteFile(filepath.Join(state.Path, "tracked.txt"), []byte("local file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotContent(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	// Working-file bytes are unchanged, but the staged copy is newly changed.
	testGit(t, state.Path, "add", "tracked.txt")
	if err := snapshot.validate(context.Background(), state); err == nil {
		t.Fatal("changed staged index was not detected")
	}
}

func TestRealAggressivePreservesIgnoredNestedUnpublishedRepository(t *testing.T) {
	root, state := realWorktree(t)
	nested := filepath.Join(state.Path, "ignored", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	testGit(t, nested, "init", "-b", "main")
	testGit(t, nested, "config", "user.name", "Fixture")
	testGit(t, nested, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(nested, "valuable.txt"), []byte("unpublished work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, nested, "add", "valuable.txt")
	testGit(t, nested, "commit", "-m", "local only")
	head := testGit(t, nested, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(state.CommonDir, "info", "exclude"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := realEngine()
	e.options = Options{Level: Aggressive, DiscardLocalChanges: true}
	e.remove = func(ctx context.Context, s repository.State) error {
		return removeWorktreeWithOptions(ctx, s, e.options)
	}
	results, err := e.run(context.Background(), root, true)
	if err == nil || results[1].Action != ActionKeep || results[1].Destructive {
		t.Fatalf("results=%+v error=%v", results, err)
	}
	if got := testGit(t, nested, "rev-parse", "HEAD"); got != head {
		t.Fatalf("nested unpublished commit lost: %s != %s", got, head)
	}
}
