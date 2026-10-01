package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- test-only Git argument arrays.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %q: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSuffix(string(output), "\n")
}

func writeTest(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (root, repo string) {
	t.Helper()
	root = t.TempDir()
	repo = filepath.Join(root, "clone")
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTest(t, root, "init", "--bare", "--initial-branch=main", remote)
	gitTest(t, root, "init", "--initial-branch=main", repo)
	writeTest(t, filepath.Join(repo, "tracked"), "original\n")
	writeTest(t, filepath.Join(repo, ".gitignore"), "ignored/\n*.ignored\n")
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "initial")
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	return root, repo
}

func assertKnownGit(t *testing.T, state State) {
	t.Helper()
	if len(state.Problems) != 0 {
		t.Fatalf("unexpected Git problems: %v", state.Problems)
	}
}

func TestInspectClean(t *testing.T) {
	_, repo := fixture(t)
	state := Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if state.Path != repo || !state.Primary || state.Branch != "main" || state.Head == "" || state.Origin == "" {
		t.Fatalf("invalid identity: %+v", state)
	}
	if state.Dirty || state.Untracked || state.Ignored || state.Locked || state.Stash || state.Unique || state.Ahead != 0 || state.Behind != 0 {
		t.Fatalf("clean clone is not clean: %+v", state)
	}
	if state.CommonDir != filepath.Join(repo, ".git") || state.GitDir != state.CommonDir {
		t.Fatalf("incorrect administrative paths: %+v", state)
	}
}

func TestInspectPreservesWhitespacePaths(t *testing.T) {
	root, repo := fixture(t)
	path := filepath.Join(root, " spaces\tand\nnewlines \n")
	if err := os.Rename(repo, path); err != nil {
		t.Fatal(err)
	}
	state := Inspect(context.Background(), path)
	assertKnownGit(t, state)
	if state.Path != path || state.GitDir != filepath.Join(path, ".git") {
		t.Fatalf("path bytes were lost: %+v", state)
	}
}

func TestInspectStatusProtections(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		check  func(State) bool
	}{
		{"modified", func(t *testing.T, repo string) { writeTest(t, filepath.Join(repo, "tracked"), "changed") }, func(s State) bool { return s.Dirty }},
		{"untracked", func(t *testing.T, repo string) { writeTest(t, filepath.Join(repo, "new\nfile"), "new") }, func(s State) bool { return s.Untracked }},
		{"ignored", func(t *testing.T, repo string) { writeTest(t, filepath.Join(repo, "secret.ignored"), "ignored") }, func(s State) bool { return s.Ignored }},
		{"staged rename", func(t *testing.T, repo string) { gitTest(t, repo, "mv", "tracked", "renamed\nfile") }, func(s State) bool { return s.Dirty }},
		{"stash", func(t *testing.T, repo string) {
			writeTest(t, filepath.Join(repo, "tracked"), "changed")
			gitTest(t, repo, "stash", "push", "-m", "keep me")
		}, func(s State) bool { return s.Stash && !s.Dirty }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, repo := fixture(t)
			test.mutate(t, repo)
			state := Inspect(context.Background(), repo)
			assertKnownGit(t, state)
			if !test.check(state) {
				t.Fatalf("missing protection: %+v", state)
			}
		})
	}
}

func TestInspectUniqueIncludesUncheckedOutBranches(t *testing.T) {
	_, repo := fixture(t)
	gitTest(t, repo, "checkout", "-b", "local-only")
	writeTest(t, filepath.Join(repo, "tracked"), "private branch commit")
	gitTest(t, repo, "commit", "-am", "local only")
	uniqueHead := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "checkout", "main")
	state := Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if !state.Unique || state.Ahead != 0 {
		t.Fatalf("unchecked-out branch was not protected: %+v", state)
	}
	gitTest(t, repo, "update-ref", "refs/remotes/another/topic", uniqueHead)
	state = Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if state.Unique {
		t.Fatalf("commit reachable from another remote was treated as unique: %+v", state)
	}
}

func TestInspectDetachedUniqueAndNoRemotes(t *testing.T) {
	_, repo := fixture(t)
	gitTest(t, repo, "checkout", "--detach")
	writeTest(t, filepath.Join(repo, "tracked"), "detached commit")
	gitTest(t, repo, "commit", "-am", "detached")
	state := Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if !state.Unique || state.Branch != "" || state.Ahead != -1 || state.Behind != -1 {
		t.Fatalf("detached unique HEAD was not protected: %+v", state)
	}
	gitTest(t, repo, "checkout", "main")
	gitTest(t, repo, "remote", "remove", "origin")
	state = Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if !state.Unique || state.Origin != "" {
		t.Fatalf("repository without remotes was not protected: %+v", state)
	}
}

func TestInspectAheadBehind(t *testing.T) {
	_, repo := fixture(t)
	writeTest(t, filepath.Join(repo, "tracked"), "ahead")
	gitTest(t, repo, "commit", "-am", "ahead")
	state := Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if state.Ahead != 1 || state.Behind != 0 {
		t.Fatalf("wrong ahead count: %+v", state)
	}
	gitTest(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitTest(t, repo, "reset", "--hard", "HEAD~1")
	state = Inspect(context.Background(), repo)
	assertKnownGit(t, state)
	if state.Ahead != 0 || state.Behind != 1 {
		t.Fatalf("wrong behind count: %+v", state)
	}
}

func TestInspectOperationLocks(t *testing.T) {
	for _, name := range []string{"index.lock", "HEAD.lock", "refs/heads/main.lock", "MERGE_HEAD", "CHERRY_PICK_HEAD", "rebase-merge", "gc.pid"} {
		t.Run(name, func(t *testing.T) {
			_, repo := fixture(t)
			path := filepath.Join(repo, ".git", name)
			if name == "rebase-merge" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTest(t, path, "")
			}
			state := Inspect(context.Background(), repo)
			if !state.Locked {
				t.Fatalf("lock %s was not found: %+v", name, state)
			}
		})
	}
}

func TestInspectRejectsInheritedRepositoryAndSymlinks(t *testing.T) {
	root, repo := fixture(t)
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	state := Inspect(context.Background(), nested)
	if len(state.Problems) == 0 || state.GitDir != "" {
		t.Fatalf("inherited repository was accepted: %+v", state)
	}
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	state = Inspect(context.Background(), link)
	if len(state.Problems) == 0 {
		t.Fatalf("symlink root was accepted: %+v", state)
	}
	states, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Path != repo {
		t.Fatalf("symlink duplicate discovered: %+v", states)
	}
}

func TestDiscoverLinkedWorktreesOutsideRootAndDedup(t *testing.T) {
	root, repo := fixture(t)
	outside := filepath.Join(t.TempDir(), "linked \t\n checkout\n")
	inside := filepath.Join(root, "second")
	gitTest(t, repo, "worktree", "add", "--detach", outside)
	gitTest(t, repo, "worktree", "add", "--detach", inside)
	gitTest(t, repo, "worktree", "lock", "--reason", "keep\nthis", outside)
	states, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 {
		t.Fatalf("expected three unique worktrees: %+v", states)
	}
	paths := make(map[string]State)
	for i, state := range states {
		assertKnownGit(t, state)
		paths[state.Path] = state
		if i > 0 && states[i-1].Path >= state.Path {
			t.Fatal("results are not uniquely sorted")
		}
	}
	if !paths[repo].Primary || paths[outside].Primary || !paths[outside].Locked || paths[inside].GitDir == paths[inside].CommonDir {
		t.Fatalf("wrong worktree state: %+v", states)
	}
	// Starting from a direct-child linked checkout must find the main checkout.
	onlyLinkedRoot := filepath.Dir(outside)
	states, err = Discover(context.Background(), onlyLinkedRoot)
	if err != nil || len(states) != 3 {
		t.Fatalf("linked-root discovery: %v %+v", err, states)
	}
}

func TestDiscoverInvalidAndMissingWorktree(t *testing.T) {
	root, repo := fixture(t)
	linked := filepath.Join(t.TempDir(), "missing")
	gitTest(t, repo, "worktree", "add", "--detach", linked)
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(root, "invalid")
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(invalid, ".git"), "not a git file")
	states, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 {
		t.Fatalf("expected missing and invalid records: %+v", states)
	}
	for _, state := range states {
		if state.Path != repo && len(state.Problems) == 0 {
			t.Fatalf("invalid state was unprotected: %+v", state)
		}
	}
}

func TestInspectGitErrorsFailClosed(t *testing.T) {
	_, repo := fixture(t)
	writeTest(t, filepath.Join(repo, ".git", "index"), "broken index")
	state := Inspect(context.Background(), repo)
	if len(state.Problems) == 0 {
		t.Fatalf("Git status failure was hidden: %+v", state)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state = Inspect(ctx, repo)
	if len(state.Problems) == 0 {
		t.Fatal("cancellation was not protected")
	}
	if _, err := Discover(ctx, filepath.Dir(repo)); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
}

func TestInspectIgnoresInheritedGitEnvironment(t *testing.T) {
	_, first := fixture(t)
	_, second := fixture(t)
	t.Setenv("GIT_DIR", filepath.Join(first, ".git"))
	t.Setenv("GIT_WORK_TREE", first)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(first, ".git", "index"))
	state := Inspect(context.Background(), second)
	assertKnownGit(t, state)
	if state.Path != second || state.GitDir != filepath.Join(second, ".git") {
		t.Fatalf("Git environment redirected inspection: %+v", state)
	}
}

func TestInspectDoesNotRewriteIndex(t *testing.T) {
	_, repo := fixture(t)
	index := filepath.Join(repo, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeData, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(repo, "tracked"), "original\n") // Encourage a stat refresh.
	assertKnownGit(t, Inspect(context.Background(), repo))
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	afterData, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || !reflect.DeepEqual(beforeData, afterData) {
		t.Fatal("read-only inspection rewrote Git index")
	}
}

func TestInUseDetectsOpenFile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires process observation")
	}
	_, repo := fixture(t)
	file, err := os.Open(filepath.Join(repo, "tracked"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	state := Inspect(context.Background(), repo)
	if !state.InUse {
		t.Fatalf("open file was not detected: %+v", state)
	}
}

func TestApplyUsageBoundariesAndUnknown(t *testing.T) {
	state := State{Path: "/tmp/repo", GitDir: "/tmp/common/worktrees/repo"}
	applyUsage(&state, processUsage{paths: []string{"/tmp/repository/file"}})
	if state.InUse || !state.InUseKnown {
		t.Fatalf("prefix collision: %+v", state)
	}
	applyUsage(&state, processUsage{paths: []string{"/tmp/common/worktrees/repo/index"}, problems: []string{"observation denied"}})
	if !state.InUse || state.InUseKnown || len(state.SafetyProblems) == 0 || len(state.Problems) != 0 {
		t.Fatalf("uncertainty not isolated: %+v", state)
	}
}

func TestObserveProcFailure(t *testing.T) {
	var usage processUsage
	usage.observeProc(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if len(usage.problems) == 0 {
		t.Fatal("unknown process inventory was treated as safe")
	}
}

func TestParsersRejectTruncation(t *testing.T) {
	for _, input := range []string{"", "worktree /tmp/repo", "HEAD abc\x00\x00", "worktree relative\x00\x00", "worktree /tmp/repo\x00unexpected\x00\x00"} {
		if _, err := parseWorktrees([]byte(input)); err == nil {
			t.Errorf("accepted malformed worktree data %q", input)
		}
	}
	for _, input := range []string{"?? no terminator", "x\x00", "R  new\x00"} {
		if err := parseStatus([]byte(input), &State{}); err == nil {
			t.Errorf("accepted malformed status %q", input)
		}
	}
}

func TestInspectHiddenIndexChangesFailClosed(t *testing.T) {
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(flag, func(t *testing.T) {
			_, repo := fixture(t)
			gitTest(t, repo, "update-index", flag, "tracked")
			writeTest(t, filepath.Join(repo, "tracked"), "private data hidden from status")
			if status := gitTest(t, repo, "status", "--porcelain"); status != "" {
				t.Fatalf("fixture should hide local data: %q", status)
			}
			state := Inspect(context.Background(), repo)
			if len(state.Problems) == 0 {
				t.Fatalf("hidden data accepted as clean: %+v", state)
			}
			data, err := os.ReadFile(filepath.Join(repo, "tracked"))
			if err != nil || string(data) != "private data hidden from status" {
				t.Fatalf("inspection changed local data: %q %v", data, err)
			}
		})
	}
}

func TestSharedGitActivityProtectsLinkedWorktrees(t *testing.T) {
	state := State{Path: "/tmp/linked", GitDir: "/tmp/clone/.git/worktrees/linked", CommonDir: "/tmp/clone/.git"}
	applyUsage(&state, processUsage{paths: []string{"/tmp/clone/.git/objects/pack/data"}})
	if !state.InUse || !state.InUseKnown {
		t.Fatalf("shared Git store activity was ignored: %+v", state)
	}
}

func TestProcessOwnershipScope(t *testing.T) {
	for _, test := range []struct {
		status string
		own    bool
		bad    bool
	}{
		{"Name:\ttest\nUid:\t1000\t1000\t1000\t1000\n", true, false},
		{"Uid:\t2000\t2000\t2000\t2000\n", false, false},
		{"Uid:\t1000\t0\t0\t0\n", true, false},
		{"Uid:\t1000\n", false, true},
		{"Uid:\t1000\tbad\t1000\t1000\n", false, true},
		{"Name:\ttest\n", false, true},
	} {
		own, err := processOwnedBy([]byte(test.status), 1000)
		if own != test.own || (err != nil) != test.bad {
			t.Errorf("status %q: got %v %v", test.status, own, err)
		}
	}
}

func TestObserveProcCurrentUserAndUnknown(t *testing.T) {
	root := t.TempDir()
	ownDir := filepath.Join(root, "123")
	otherDir := filepath.Join(root, "456")
	for _, dir := range []string{ownDir, otherDir} {
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	uid := os.Geteuid()
	writeTest(t, filepath.Join(ownDir, "status"), fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid))
	writeTest(t, filepath.Join(otherDir, "status"), fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d\n", uid+1, uid+1, uid+1, uid+1))
	writeTest(t, filepath.Join(ownDir, "stat"), "123 (test worker) S 0")
	writeTest(t, filepath.Join(ownDir, "cmdline"), "test\x00")
	if err := os.Symlink("/tmp/current-user-repo", filepath.Join(ownDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/current-user-repo/file", filepath.Join(ownDir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/other-user-repo", filepath.Join(otherDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	var usage processUsage
	usage.observeProc(context.Background(), root)
	if len(usage.problems) != 0 || !reflect.DeepEqual(usage.paths, []string{"/tmp/current-user-repo", "/tmp/current-user-repo/file"}) {
		t.Fatalf("wrong process scope: %+v", usage)
	}
	if err := os.Remove(filepath.Join(ownDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	usage = processUsage{}
	usage.observeProc(context.Background(), root)
	if len(usage.problems) == 0 {
		t.Fatal("missing live process cwd was silently ignored")
	}
}

func TestLsofCurrentUserScopeAndFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses POSIX shell")
	}
	bin := t.TempDir()
	executable := filepath.Join(bin, "lsof")
	script := "#!/bin/sh\n[ \"$1\" = '-nP' ] && [ \"$2\" = '-u' ] && [ \"$3\" = '" + strconv.Itoa(os.Geteuid()) + "' ] && [ \"$4\" = '-F0pn' ] || exit 2\nprintf 'p123\\000ncwd-path\\000\\nn/tmp/repo with\\nnewline/file\\000\\n'\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil { // #nosec G306 -- executable fixture script in an isolated test directory.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var usage processUsage
	usage.observeLsof(context.Background())
	if len(usage.problems) != 0 || !reflect.DeepEqual(usage.paths, []string{"/tmp/repo with\nnewline/file"}) {
		t.Fatalf("lsof parsing or scope: %+v", usage)
	}
	for _, script := range []string{"#!/bin/sh\nexit 1\n", "#!/bin/sh\nprintf 'p123\\000'\necho warning >&2\n"} {
		if err := os.WriteFile(executable, []byte(script), 0o700); err != nil { // #nosec G306 -- executable fixture script in an isolated test directory.
			t.Fatal(err)
		}
		usage = processUsage{}
		usage.observeLsof(context.Background())
		if len(usage.problems) == 0 {
			t.Fatal("incomplete lsof observation succeeded")
		}
	}
}
