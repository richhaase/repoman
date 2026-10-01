package syncer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

// Every Git transport in these integration tests points to a temporary local
// repository. The real Inspect/fetch/merge implementations run; GitHub is never
// contacted and no user checkout is read or changed.
func TestRealGitCloneWithLocalTransport(t *testing.T) {
	ctx := context.Background()
	root := syncTempDir(t)
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "not-created-yet", "clones")
	git := func(dir string, args ...string) {
		t.Helper()
		if _, err := runCommand(ctx, dir, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "init", "--quiet", "--initial-branch=main", source)
	git(source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "initial")
	e := engine{now: func() time.Time { return testNow }, inspect: func(ctx context.Context, path string) (repository.State, error) {
		return repository.Inspect(ctx, path), nil
	}, command: func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
		if program == "gh" && args[0] == "api" {
			return []byte(page(t, []remoteRepo{activeRepo("project")}, false, "")), nil
		}
		if program == "gh" && args[0] == "repo" && args[1] == "clone" {
			if args[2] != "https://github.com/alice/project.git" {
				t.Fatalf("wrong clone source %v", args)
			}
			return runCommand(ctx, "", "git", "clone", "--quiet", "--no-recurse-submodules", source, args[3])
		}
		return runCommand(ctx, dir, program, args...)
	}}
	results, err := e.run(ctx, Target{Dir: target, Owner: "alice", Days: 45}, false)
	if err != nil || len(results) != 1 || results[0].Action != "cloned" {
		t.Fatalf("got %+v, %v", results, err)
	}
	state := repository.Inspect(ctx, filepath.Join(target, "project"))
	if !state.Primary || state.Branch != "main" || state.Dirty || len(state.Problems) != 0 {
		t.Fatalf("invalid clone %+v", state)
	}
}
