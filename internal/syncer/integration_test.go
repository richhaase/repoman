package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

// Every Git transport in these integration tests points to a temporary local
// repository. The real Inspect/fetch/merge implementations run; GitHub is never
// contacted and no user checkout is read or changed.
func TestRealGitFastForwardAndPreservation(t *testing.T) {
	for _, scenario := range []string{"fast-forward", "dirty", "ahead", "diverged", "hidden-dirty", "wrong-branch", "rewritten-origin"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			root := syncTempDir(t)
			source := filepath.Join(root, "source")
			bare := filepath.Join(root, "remote.git")
			clones := filepath.Join(root, "clones")
			dest := filepath.Join(clones, "project")
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := runCommand(ctx, dir, "git", args...)
				if err != nil {
					t.Fatalf("git %v: %v", args, err)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			commit := func(dir, message string) {
				t.Helper()
				git(dir, "add", "tracked.txt")
				git(dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", message)
			}
			git(root, "init", "--quiet", "--initial-branch=main", source)
			write(filepath.Join(source, "tracked.txt"), "initial\n")
			commit(source, "initial")
			git(root, "clone", "--quiet", "--bare", source, bare)
			if err := os.Mkdir(clones, 0o700); err != nil {
				t.Fatal(err)
			}
			git(root, "clone", "--quiet", bare, dest)
			git(dest, "remote", "set-url", "origin", "https://github.com/alice/project.git")
			// User merge defaults must not turn the intended fast-forward into a
			// squash/staged change or enable autostashing.
			git(dest, "config", "branch.main.mergeOptions", "--squash --autostash")
			originalHead := git(dest, "rev-parse", "HEAD")
			write(filepath.Join(source, "tracked.txt"), "remote change\n")
			commit(source, "remote change")
			expectedHead := git(source, "rev-parse", "HEAD")
			// Updating the temporary bare fixture is a local filesystem-only transport.
			git(source, "push", "--quiet", bare, "main")
			hookMarker := filepath.Join(root, "hook-ran")
			hook := filepath.Join(dest, ".git", "hooks", "post-merge")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+hookMarker+"'\n"), 0o700); err != nil { // #nosec G306 -- executable hook fixture inside an isolated test directory.
				t.Fatal(err)
			}
			wantContent := "initial\n"
			switch scenario {
			case "wrong-branch":
				git(dest, "switch", "--quiet", "-c", "feature")
			case "rewritten-origin":
				git(dest, "config", "url.https://example.invalid/.insteadOf", "https://github.com/")
			case "dirty", "hidden-dirty":
				wantContent = "uncommitted local work\n"
				write(filepath.Join(dest, "tracked.txt"), wantContent)
				if scenario == "hidden-dirty" {
					git(dest, "update-index", "--assume-unchanged", "tracked.txt")
				}
			case "ahead", "diverged":
				wantContent = "local commit\n"
				write(filepath.Join(dest, "tracked.txt"), wantContent)
				commit(dest, "local work")
				originalHead = git(dest, "rev-parse", "HEAD")
				if scenario == "diverged" {
					git(dest, "fetch", "--quiet", bare, "refs/heads/main:refs/remotes/origin/main")
				}
			}
			fetches := 0
			e := engine{now: func() time.Time { return testNow }, inspect: func(ctx context.Context, path string) (repository.State, error) {
				return repository.Inspect(ctx, path), nil
			}, command: func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
				if program == "gh" {
					return []byte(page(t, []remoteRepo{activeRepo("project")}, false, "")), nil
				}
				if len(args) > 0 && args[0] == "fetch" {
					fetches++
					replaced := append([]string{}, args...)
					if replaced[len(replaced)-2] != "origin" {
						t.Fatalf("fetch did not target origin: %v", args)
					}
					replaced[len(replaced)-2] = bare
					return runCommand(ctx, dir, program, replaced...)
				}
				return runCommand(ctx, dir, program, args...)
			}}
			results, err := e.run(ctx, Target{Dir: clones, Owner: "alice", Days: 45}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 {
				t.Fatalf("unexpected results %+v", results)
			}
			if scenario == "fast-forward" {
				if results[0].Action != "updated" || fetches != 1 {
					t.Fatalf("did not fast-forward: %+v fetches=%d", results, fetches)
				}
				if got := git(dest, "rev-parse", "HEAD"); got != expectedHead {
					t.Fatalf("HEAD %s want %s", got, expectedHead)
				}
				wantContent = "remote change\n"
			} else {
				if results[0].Action != "skipped" || fetches != 0 {
					t.Fatalf("unsafe clone not skipped: %+v fetches=%d", results, fetches)
				}
				if got := git(dest, "rev-parse", "HEAD"); got != originalHead {
					t.Fatalf("changed local HEAD: %s want %s", got, originalHead)
				}
			}
			got, err := os.ReadFile(filepath.Join(dest, "tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != wantContent {
				t.Fatalf("changed content %q want %q", got, wantContent)
			}
			if _, err := os.Stat(hookMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("post-merge hook ran: %v", err)
			}
		})
	}
}

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
