package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type gitFixture struct {
	t               *testing.T
	root, source    string
	bare, clones    string
	dest            string
	initial, remote string
	fetches         int
	fetchCommands   [][]string
	globalConfig    string
	repos           []remoteRepo
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	f := &gitFixture{t: t, root: syncTempDir(t), repos: []remoteRepo{activeRepo("project")}}
	f.globalConfig = filepath.Join(f.root, "global.gitconfig")
	f.source, f.bare = filepath.Join(f.root, "source"), filepath.Join(f.root, "remote.git")
	f.clones = filepath.Join(f.root, "clones")
	f.dest = filepath.Join(f.clones, "project")
	f.git(f.root, "init", "--quiet", "--initial-branch=main", f.source)
	f.write(f.source, "tracked.txt", "initial\n")
	f.write(f.source, ".gitignore", "ignored.txt\n")
	f.commit(f.source, "initial")
	f.git(f.source, "branch", "obsolete")
	f.git(f.root, "clone", "--quiet", "--bare", f.source, f.bare)
	if err := os.Mkdir(f.clones, 0o700); err != nil {
		t.Fatal(err)
	}
	f.git(f.root, "clone", "--quiet", f.bare, f.dest)
	f.git(f.dest, "remote", "set-url", "origin", "https://github.com/alice/project.git")
	f.git(f.dest, "remote", "add", "secondary", f.bare)
	f.git(f.dest, "fetch", "--quiet", "secondary")
	f.initial = f.git(f.dest, "rev-parse", "HEAD")
	f.git(f.dest, "tag", "local-only")
	f.write(f.source, "tracked.txt", "remote change\n")
	f.commit(f.source, "remote change")
	f.remote = f.git(f.source, "rev-parse", "HEAD")
	f.git(f.source, "branch", "feature")
	f.git(f.source, "tag", "remote-tag")
	f.git(f.source, "push", "--quiet", f.bare, "main", "feature", "refs/tags/remote-tag", ":refs/heads/obsolete")
	return f
}

func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.command(context.Background(), dir, "git", args...)
	if err != nil {
		f.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) command(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
	// Use production Git safeguards with an isolated global config. This allows
	// actual ambient-config tests without reading or writing the user's config.
	cmd := exec.CommandContext(ctx, program, args...) // #nosec G204 -- fixed Git executable and arguments from temporary test fixtures.
	cmd.Dir = dir
	cmd.Env = append(commandEnvironment(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+f.globalConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (f *gitFixture) write(dir, name, contents string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *gitFixture) commit(dir, message string) {
	f.t.Helper()
	f.git(dir, "add", ".")
	f.git(dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", message)
}
func (f *gitFixture) engine() *engine {
	return &engine{now: func() time.Time { return testNow }, command: func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
		if program == "gh" {
			return []byte(page(f.t, f.repos, false, "")), nil
		}
		if len(args) > 0 && args[0] == "fetch" {
			f.fetches++
			f.fetchCommands = append(f.fetchCommands, append([]string(nil), args...))
			// The production fetch command runs unchanged; only the test's
			// origin transport is redirected to a temporary bare repository.
			args = append([]string{"-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "url." + f.bare + ".insteadOf=https://github.com/alice/project.git"}, args...)
		}
		if len(args) > 0 && (args[0] == "reset" || args[0] == "clean" || args[0] == "branch") {
			f.t.Fatalf("unexpected destructive command: %v", args)
		}
		return f.command(ctx, dir, program, args...)
	}}
}
func (f *gitFixture) run(options Options) ([]Result, error) {
	return f.engine().runWithOptions(context.Background(), Target{Dir: f.clones, Owner: "alice", Days: 45}, options)
}
func (f *gitFixture) assertFetched() {
	f.t.Helper()
	if f.fetches != 1 {
		f.t.Fatalf("fetch count %d", f.fetches)
	}
	if want := []string{"fetch", "--no-all", "--no-prune", "--no-prune-tags", "--quiet", "origin"}; !reflect.DeepEqual(f.fetchCommands[0], want) {
		f.t.Fatalf("unexpected fetch: %v", f.fetchCommands[0])
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/feature", "refs/tags/remote-tag"} {
		if got := f.git(f.dest, "rev-parse", ref); got != f.remote {
			f.t.Fatalf("%s=%s want %s", ref, got, f.remote)
		}
	}
	for _, ref := range []string{"refs/remotes/origin/obsolete", "refs/remotes/secondary/main", "refs/remotes/secondary/obsolete", "refs/tags/local-only"} {
		if got := f.git(f.dest, "rev-parse", ref); got != f.initial {
			f.t.Fatalf("%s changed to %s, want %s", ref, got, f.initial)
		}
	}
}

func TestFetchAndCheckoutSafety(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty", "untracked", "nondefault", "detached", "ahead", "diverged", "ignored", "linked", "no-default", "missing-origin-default"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			wantHead := f.initial
			wantContent := "initial\n"
			switch scenario {
			case "dirty":
				wantContent = "local change\n"
				f.write(f.dest, "tracked.txt", wantContent)
			case "untracked":
				f.write(f.dest, "untracked.txt", "local file\n")
			case "nondefault":
				f.git(f.dest, "switch", "--quiet", "-c", "feature")
			case "detached":
				f.git(f.dest, "switch", "--quiet", "--detach")
			case "ahead", "diverged":
				if scenario == "ahead" {
					f.git(f.dest, "fetch", "--quiet", f.bare, "main")
					f.git(f.dest, "merge", "--ff-only", "--quiet", "FETCH_HEAD")
				}
				wantContent = "local commit\n"
				f.write(f.dest, "tracked.txt", wantContent)
				f.commit(f.dest, "local commit")
				wantHead = f.git(f.dest, "rev-parse", "HEAD")
			case "ignored":
				f.write(f.dest, "ignored.txt", "ignored content\n")
			case "linked":
				f.git(f.dest, "worktree", "add", "--quiet", "-b", "feature", filepath.Join(f.root, "linked"))
			case "no-default":
				f.repos[0].DefaultBranch = nil
			case "missing-origin-default":
				f.repos[0].DefaultBranch.Name = "missing"
			}
			f.git(f.dest, "config", "branch.main.mergeOptions", "--squash --autostash")
			f.write(filepath.Join(f.dest, ".git", "hooks"), "post-merge", "#!/bin/sh\nprintf ran > \"$0.ran\"\n")
			if err := os.Chmod(filepath.Join(f.dest, ".git", "hooks", "post-merge"), 0o700); err != nil { // #nosec G302 -- executable Git hook fixture, used only to verify hooks are disabled.
				t.Fatal(err)
			}
			results, err := f.run(Options{})
			if err != nil {
				t.Fatal(err)
			}
			f.assertFetched()
			if scenario == "clean" || scenario == "ignored" || scenario == "linked" {
				wantHead = f.remote
				wantContent = "remote change\n"
				if results[0].Action != "updated" {
					t.Fatalf("%+v", results)
				}
			} else if results[0].Action != "fetched" {
				t.Fatalf("%+v", results)
			}
			if got := f.git(f.dest, "rev-parse", "HEAD"); got != wantHead {
				t.Fatalf("HEAD=%s want=%s", got, wantHead)
			}
			if got, err := os.ReadFile(filepath.Join(f.dest, "tracked.txt")); err != nil || string(got) != wantContent {
				t.Fatalf("content=%q err=%v", got, err)
			}
			if _, err := os.Stat(filepath.Join(f.dest, ".git", "hooks", "post-merge.ran")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("post-merge hook ran: %v", err)
			}
			if scenario == "ignored" {
				if _, err := os.Stat(filepath.Join(f.dest, "ignored.txt")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "untracked" {
				if _, err := os.Stat(filepath.Join(f.dest, "untracked.txt")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestScriptForceAndDryRun(t *testing.T) {
	for _, scenario := range []string{"dirty", "nondefault", "ahead", "diverged", "renamed-default"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			switch scenario {
			case "dirty":
				f.write(f.dest, "tracked.txt", "uncommitted\n")
			case "nondefault":
				f.git(f.dest, "switch", "--quiet", "-c", "feature")
			case "ahead", "diverged":
				if scenario == "ahead" {
					f.git(f.dest, "fetch", "--quiet", f.bare, "main")
					f.git(f.dest, "merge", "--ff-only", "--quiet", "FETCH_HEAD")
				}
				f.write(f.dest, "tracked.txt", "local commit\n")
				f.commit(f.dest, "local")
			case "renamed-default":
				f.git(f.dest, "branch", "-m", "main", "master")
			}
			before := f.git(f.dest, "rev-parse", "HEAD")
			f.write(f.dest, "untracked.txt", "keep untracked\n")
			f.write(f.dest, "ignored.txt", "keep ignored\n")
			preview, err := f.run(Options{DryRun: true, Force: true})
			if err != nil || preview[0].Action != "would-force" || f.fetches != 0 || f.git(f.dest, "rev-parse", "HEAD") != before {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			results, err := f.run(Options{Force: true})
			if err != nil || results[0].Action != "forced" {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			f.assertFetched()
			if got := f.git(f.dest, "rev-parse", "HEAD"); got != f.remote {
				t.Fatalf("HEAD=%s", got)
			}
			if got := f.git(f.dest, "branch", "--show-current"); got != "main" {
				t.Fatalf("branch=%s", got)
			}
			if got := f.git(f.dest, "rev-parse", "--abbrev-ref", "main@{upstream}"); got != "origin/main" {
				t.Fatalf("upstream=%s", got)
			}
			for _, name := range []string{"untracked.txt", "ignored.txt"} {
				if _, err := os.Stat(filepath.Join(f.dest, name)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestScriptInactiveCleanup(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty", "untracked", "ignored", "unique", "http-origin", "linked", "wrong-origin", "linked-destination"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			old := testNow.AddDate(0, 0, -46)
			f.repos[0].PushedAt = &old
			switch scenario {
			case "dirty":
				f.write(f.dest, "tracked.txt", "local\n")
			case "untracked":
				f.write(f.dest, "untracked.txt", "local\n")
			case "ignored":
				f.write(f.dest, "ignored.txt", "ignored\n")
			case "unique":
				f.write(f.dest, "tracked.txt", "local commit\n")
				f.commit(f.dest, "local")
			case "linked":
				f.git(f.dest, "worktree", "add", "--quiet", "-b", "feature", filepath.Join(f.root, "linked"))
			case "http-origin":
				f.git(f.dest, "remote", "set-url", "origin", "http://github.com/alice/project.git")
			case "wrong-origin":
				f.git(f.dest, "remote", "set-url", "origin", "https://github.com/bob/project.git")
			case "linked-destination":
				primary := filepath.Join(f.root, "primary")
				if err := os.Rename(f.dest, primary); err != nil {
					t.Fatal(err)
				}
				f.git(primary, "worktree", "add", "--quiet", "-b", "feature", f.dest)
			}
			removable := scenario == "clean" || scenario == "ignored" || scenario == "unique" || scenario == "http-origin"
			preview, err := f.run(Options{Cleanup: true, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if (preview[0].Action == "would-remove") != removable {
				t.Fatalf("preview=%+v", preview)
			}
			if _, err := os.Stat(f.dest); err != nil {
				t.Fatal(err)
			}
			results, err := f.run(Options{Cleanup: true, Force: true})
			if err != nil || (results[0].Action == "removed") != removable {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			_, err = os.Stat(f.dest)
			if errors.Is(err, os.ErrNotExist) != removable {
				t.Fatalf("destination err=%v removable=%v", err, removable)
			}
			if f.fetches != 0 {
				t.Fatalf("inactive clone fetched %d times", f.fetches)
			}
		})
	}
}

func TestScriptUnbornCloneFetchAndCleanup(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "fetch", true: "cleanup"}[cleanup], func(t *testing.T) {
			f := newGitFixture(t)
			if err := os.RemoveAll(f.dest); err != nil {
				t.Fatal(err)
			}
			f.git(f.root, "init", "--quiet", "--initial-branch=main", f.dest)
			f.git(f.dest, "remote", "add", "origin", "https://github.com/alice/project.git")
			f.repos[0].DefaultBranch = nil
			if cleanup {
				old := testNow.AddDate(0, 0, -46)
				f.repos[0].PushedAt = &old
			}
			preview, err := f.run(Options{DryRun: true, Cleanup: cleanup})
			wantPreview := "would-fetch"
			wantApply := "fetched"
			if cleanup {
				wantPreview = "would-remove"
				wantApply = "removed"
			}
			if err != nil || preview[0].Action != wantPreview {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			result, err := f.run(Options{Cleanup: cleanup})
			if err != nil || result[0].Action != wantApply {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestScriptInactiveCleanupSelection(t *testing.T) {
	for _, scenario := range []string{"not-included", "excluded", "negated-class", "archived", "active"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			old := testNow.AddDate(0, 0, -46)
			f.repos[0].PushedAt = &old
			target := Target{Dir: f.clones, Owner: "alice", Days: 45}
			switch scenario {
			case "not-included":
				target.Includes = []string{"other-*"}
			case "excluded":
				target.Includes = []string{"p*"}
				target.Excludes = []string{"*"}
			case "negated-class":
				target.Includes = []string{"[!x]*"}
				target.Excludes = []string{"[!z]*"}
			case "archived":
				f.repos[0].Archived = true
			case "active":
				f.repos[0] = activeRepo("project")
			}
			results, err := f.engine().runWithOptions(context.Background(), target, Options{Cleanup: true})
			if err != nil || len(results) != 1 || results[0].Action == "removed" {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			if _, err := os.Stat(f.dest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestScriptUnknownStatusDoesNotMeanClean(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "cleanup"}[cleanup], func(t *testing.T) {
			f := newGitFixture(t)
			if cleanup {
				old := testNow.AddDate(0, 0, -46)
				f.repos[0].PushedAt = &old
			}
			e := f.engine()
			base := e.command
			e.command = func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
				if program == "git" && len(args) > 0 && args[0] == "status" {
					return nil, errors.New("status failed")
				}
				return base(ctx, dir, program, args...)
			}
			results, err := e.runWithOptions(context.Background(), Target{Dir: f.clones, Owner: "alice", Days: 45}, Options{Cleanup: cleanup, Force: true})
			if err == nil || results[0].Action != "error" || !strings.Contains(results[0].Reason, "status failed") {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			if got := f.git(f.dest, "rev-parse", "HEAD"); got != f.initial {
				t.Fatalf("HEAD changed: %s", got)
			}
			if !cleanup {
				f.assertFetched()
			} else if f.fetches != 0 {
				t.Fatal("inactive repository fetched")
			}
		})
	}
}

func TestScriptWrongOriginPreventsSyncMutation(t *testing.T) {
	for _, scenario := range []string{"wrong-origin", "rewritten-origin", "linked-destination"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			switch scenario {
			case "wrong-origin":
				f.git(f.dest, "remote", "set-url", "origin", "https://github.com/bob/project.git")
			case "rewritten-origin":
				f.git(f.dest, "config", "url.https://example.invalid/.insteadOf", "https://github.com/")
			case "linked-destination":
				primary := filepath.Join(f.root, "primary")
				if err := os.Rename(f.dest, primary); err != nil {
					t.Fatal(err)
				}
				f.git(primary, "worktree", "add", "--quiet", "-b", "feature", f.dest)
			}
			results, err := f.run(Options{Force: true})
			if err != nil || results[0].Action != "skipped" || f.fetches != 0 {
				t.Fatalf("results=%+v err=%v fetches=%d", results, err, f.fetches)
			}
			if got := f.git(f.dest, "rev-parse", "HEAD"); got != f.initial {
				t.Fatalf("HEAD changed: %s", got)
			}
		})
	}
}
