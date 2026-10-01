package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

const testOID = "1111111111111111111111111111111111111111"
const fetchedOID = "2222222222222222222222222222222222222222"

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Use an independent filesystem temporary directory: Go's GOTMPDIR can live
// inside the source checkout, which correctly fails the parent-repository guard.
func syncTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "repoman-sync-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

type call struct {
	dir, program string
	args         []string
}

func page(t *testing.T, repos []remoteRepo, more bool, cursor string) string {
	t.Helper()
	value := map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{"repositories": map[string]any{
		"nodes": repos, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor},
	}}}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func activeRepo(name string) remoteRepo {
	pushed := testNow.Add(-time.Hour)
	return remoteRepo{Name: name, FullName: "alice/" + name, PushedAt: &pushed, DefaultBranch: &struct {
		Name string `json:"name"`
	}{Name: "main"}}
}

func TestSameOrigin(t *testing.T) {
	for _, origin := range []string{"https://github.com/alice/project.git", "http://github.com/alice/project.git", "https://github.com/Alice/Project", "git@github.com:alice/project.git", "ssh://git@github.com/alice/project.git"} {
		if !sameOrigin(origin, "alice/project") {
			t.Errorf("rejected valid origin %q", origin)
		}
	}
	for _, origin := range []string{"https://github.com.evil/alice/project.git", "https://github.com/bob/project.git", "https://github.com/alice/other.git", "https://user@github.com/alice/project.git", "https://github.com/alice/project.git?x=y", "https://github.com/alice/project.git#x", "https://github.com/alice/project.git/", "https://github.com/alice%2fproject.git", "git://github.com/alice/project.git", "http://github.com/bob/project.git", "http://github.com.evil/alice/project.git", "http://user@github.com/alice/project.git", "ssh://root@github.com/alice/project.git", "ssh://git@github.com:2222/alice/project.git", "/tmp/project.git", "ext::arbitrary command", "https://github.com/alice/project.git\nhttps://github.com/alice/project.git"} {
		if sameOrigin(origin, "alice/project") {
			t.Errorf("accepted unsafe origin %q", origin)
		}
	}
}

func TestListPaginationAndDeterministicOrder(t *testing.T) {
	output := page(t, []remoteRepo{activeRepo("zeta")}, true, "next") + "\n" + page(t, []remoteRepo{activeRepo("alpha")}, false, "")
	e := engine{command: func(_ context.Context, _ string, program string, args ...string) ([]byte, error) {
		if program != "gh" || !reflect.DeepEqual(args[:6], []string{"api", "--hostname", "github.com", "graphql", "--paginate", "-f"}) {
			t.Fatalf("unexpected command %s %v", program, args)
		}
		if !strings.Contains(strings.Join(args, " "), "$endCursor") {
			t.Fatal("query must declare pagination cursor")
		}
		return []byte(output), nil
	}}
	repos, err := e.list(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "alpha" || repos[1].Name != "zeta" {
		t.Fatalf("unexpected repos %+v", repos)
	}
}

func TestListFailsClosed(t *testing.T) {
	badName := activeRepo("../bad")
	badOwner := activeRepo("project")
	badOwner.FullName = "bob/project"
	tests := map[string]string{
		"empty":             "",
		"api error":         `{"errors":[{"message":"denied"}]}`,
		"missing owner":     `{"data":{"repositoryOwner":null}}`,
		"missing page info": `{"data":{"repositoryOwner":{"repositories":{"nodes":[]}}}}`,
		"truncated":         page(t, []remoteRepo{activeRepo("project")}, true, "next"),
		"cursor missing":    page(t, []remoteRepo{activeRepo("project")}, true, ""),
		"repeated cursor":   page(t, []remoteRepo{activeRepo("first")}, true, "next") + page(t, []remoteRepo{activeRepo("second")}, true, "next"),
		"duplicate":         page(t, []remoteRepo{activeRepo("project"), activeRepo("PROJECT")}, false, ""),
		"unexpected owner":  page(t, []remoteRepo{badOwner}, false, ""),
		"unsafe name":       page(t, []remoteRepo{badName}, false, ""),
		"extra page":        page(t, nil, false, "") + page(t, nil, false, ""),
		"malformed":         "{",
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			e := engine{command: func(context.Context, string, string, ...string) ([]byte, error) { return []byte(output), nil }}
			if _, err := e.list(context.Background(), "alice"); err == nil {
				t.Fatal("expected incomplete listing error")
			}
		})
	}
}

func TestDryRunDoesNotCreateTarget(t *testing.T) {
	dir := filepath.Join(syncTempDir(t), "absent", "repos")
	var calls []call
	e := engine{now: func() time.Time { return testNow }, command: func(_ context.Context, dir, program string, args ...string) ([]byte, error) {
		calls = append(calls, call{dir, program, args})
		if program == "gh" && args[0] == "api" {
			return []byte(page(t, []remoteRepo{activeRepo("project")}, false, "")), nil
		}
		if program == "git" && args[0] == "check-ref-format" {
			return nil, nil
		}
		t.Fatalf("dry run issued unexpected command: %s %v", program, args)
		return nil, nil
	}}
	got, err := e.run(context.Background(), Target{Dir: dir, Owner: "alice", Days: 45}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "would-clone" {
		t.Fatalf("unexpected results %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dir))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created target parents: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("unexpected calls %+v", calls)
	}
}

func TestFilters(t *testing.T) {
	recent := activeRepo("app-one")
	excluded := activeRepo("app-skip")
	archived := activeRepo("app-archived")
	archived.Archived = true
	old := activeRepo("app-old")
	pushed := testNow.AddDate(0, 0, -46)
	old.PushedAt = &pushed
	noBranch := activeRepo("app-empty")
	noBranch.DefaultBranch = nil
	e := engine{now: func() time.Time { return testNow }, command: func(_ context.Context, _ string, program string, args ...string) ([]byte, error) {
		if program == "gh" {
			return []byte(page(t, []remoteRepo{recent, excluded, archived, old, noBranch, activeRepo("other")}, false, "")), nil
		}
		return nil, nil
	}}
	results, err := e.run(context.Background(), Target{Dir: syncTempDir(t), Owner: "alice", Days: 45, Includes: []string{"app-*"}, Excludes: []string{"*-skip"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, result := range results {
		reasons[result.Name] = result.Reason
	}
	want := map[string]string{"app-one": "active repository", "app-skip": "excluded", "app-archived": "archived", "app-old": "inactive", "app-empty": "active repository", "other": "not included"}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("got %v want %v", reasons, want)
	}
}

func TestUnsafePathsAndInput(t *testing.T) {
	root := syncTempDir(t)
	if err := os.Mkdir(filepath.Join(root, "parent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "parent"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo", ".git"), []byte("gitdir: elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []Target{
		{Dir: filepath.Join(root, "link", "clones"), Owner: "alice", Days: 45},
		{Dir: filepath.Join(root, "repo", "clones"), Owner: "alice", Days: 45},
		{Dir: root, Owner: "alice/../bob", Days: 45},
		{Dir: root, Owner: "alice", Days: 0},
		{Dir: root, Owner: "alice", Days: 45, Includes: []string{"["}},
		{Dir: root, Owner: "alice", Days: 45, Excludes: []string{"../*"}},
		{Dir: "/", Owner: "alice", Days: 45},
	}
	e := engine{command: func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("invalid target reached a command")
		return nil, nil
	}}
	for _, target := range tests {
		if _, err := e.run(context.Background(), target, true); err == nil {
			t.Errorf("accepted unsafe target %+v", target)
		}
	}
}

func cloneState(t *testing.T) (string, repository.State) {
	t.Helper()
	dir := filepath.Join(syncTempDir(t), "project")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, repository.State{Path: dir, CommonDir: filepath.Join(dir, ".git"), GitDir: filepath.Join(dir, ".git"), Primary: true, Head: testOID, Branch: "main", Origin: "https://github.com/alice/project.git"}
}

func fakeCommands(t *testing.T, dir string, calls *[]call) func(context.Context, string, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, wd, program string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{wd, program, append([]string{}, args...)})
		command := strings.Join(args, " ")
		switch {
		case program == "git" && strings.HasPrefix(command, "check-ref-format "):
			return nil, nil
		case command == "remote get-url --all origin":
			return []byte("https://github.com/alice/project.git\n"), nil
		case command == "worktree list --porcelain":
			return []byte("worktree " + dir + "\nHEAD " + testOID + "\nbranch refs/heads/main\n"), nil
		case strings.HasPrefix(command, "rev-list --left-right --count "):
			return []byte("0\t1\n"), nil
		case strings.HasPrefix(command, "fetch "):
			return nil, nil
		case command == "show-ref --verify --quiet refs/remotes/origin/main":
			return nil, nil
		case command == "rev-parse --verify refs/remotes/origin/main^{commit}":
			return []byte(fetchedOID + "\n"), nil
		case command == "merge-base --is-ancestor HEAD "+fetchedOID:
			return nil, nil
		case command == "-c merge.autostash=false -c branch.main.mergeOptions= merge --ff-only --no-autostash --no-edit --quiet -- "+fetchedOID:
			return nil, nil
		default:
			t.Errorf("unexpected command %s %v", program, args)
			return nil, errors.New("unexpected command")
		}
	}
}

func TestMismatchedExistingClonesAreNotFetched(t *testing.T) {
	tests := map[string]func(*repository.State){
		"wrong origin":         func(s *repository.State) { s.Origin = "https://github.com/alice/elsewhere.git" },
		"inherited repository": func(s *repository.State) { s.Path = filepath.Dir(s.Path) },
		"linked destination":   func(s *repository.State) { s.Primary = false },
		"external git dir":     func(s *repository.State) { s.GitDir = "/elsewhere" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			dir, state := cloneState(t)
			change(&state)
			var calls []call
			e := engine{command: fakeCommands(t, dir, &calls), inspect: func(context.Context, string) (repository.State, error) { return state, nil }}
			result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
			if err != nil || result.Action != "skipped" || result.Reason == "" {
				t.Fatalf("mismatched repository not skipped: %+v, %v", result, err)
			}
			for _, c := range calls {
				if c.args[0] == "fetch" || c.args[0] == "-c" {
					t.Fatalf("identity mismatch mutated: %+v", c)
				}
			}
		})
	}
}

func TestCheckoutEligibilityDoesNotPreventFetch(t *testing.T) {
	for name, change := range map[string]func(*repository.State){
		"dirty":        func(s *repository.State) { s.Dirty = true },
		"untracked":    func(s *repository.State) { s.Untracked = true },
		"detached":     func(s *repository.State) { s.Branch = "" },
		"other branch": func(s *repository.State) { s.Branch = "feature" },
	} {
		t.Run(name, func(t *testing.T) {
			dir, state := cloneState(t)
			change(&state)
			var calls []call
			e := engine{command: fakeCommands(t, dir, &calls), inspect: func(context.Context, string) (repository.State, error) { return state, nil }}
			result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
			if err != nil || result.Action != "fetched" || !strings.Contains(result.Reason, "fetched only") {
				t.Fatalf("expected fetch only: %+v, %v", result, err)
			}
			fetches := 0
			for _, c := range calls {
				if c.args[0] == "fetch" {
					fetches++
				}
				if c.args[0] == "-c" {
					t.Fatal("updated ineligible checkout")
				}
			}
			if fetches != 1 {
				t.Fatalf("fetches=%d", fetches)
			}
		})
	}
}

func TestAheadDivergedAndUnknownHistoryAreFetched(t *testing.T) {
	for _, history := range []string{"1\t0", "1\t2", "bad", "-1\t0"} {
		t.Run(history, func(t *testing.T) {
			dir, state := cloneState(t)
			var calls []call
			base := fakeCommands(t, dir, &calls)
			e := engine{inspect: func(context.Context, string) (repository.State, error) { return state, nil }, command: func(ctx context.Context, wd, program string, args ...string) ([]byte, error) {
				if args[0] == "rev-list" {
					return []byte(history), nil
				}
				return base(ctx, wd, program, args...)
			}}
			result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
			unknown := history == "bad" || history == "-1\t0"
			if (err != nil) != unknown || result.Action != "fetched" {
				t.Fatalf("got %+v, %v", result, err)
			}
			fetches := 0
			for _, c := range calls {
				if c.args[0] == "fetch" {
					fetches++
				}
				if c.args[0] == "-c" {
					t.Fatal("changed local history")
				}
			}
			if fetches != 1 {
				t.Fatalf("fetches=%d", fetches)
			}
		})
	}
}

func TestDryRunExistingCloneOnlyReads(t *testing.T) {
	dir, state := cloneState(t)
	var calls []call
	e := engine{inspect: func(context.Context, string) (repository.State, error) { return state, nil }, command: fakeCommands(t, dir, &calls)}
	result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), true)
	if err != nil || result.Action != "would-sync" {
		t.Fatalf("got %+v, %v", result, err)
	}
	for _, c := range calls {
		if c.args[0] == "fetch" || c.args[0] == "merge" || c.args[0] == "-c" {
			t.Fatalf("dry run mutated %+v", c)
		}
	}
}

func TestAllRemotesFetchPruneAndFastForward(t *testing.T) {
	dir, state := cloneState(t)
	var calls []call
	e := engine{inspect: func(context.Context, string) (repository.State, error) { return state, nil }, command: fakeCommands(t, dir, &calls)}
	result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
	if err != nil || result.Action != "updated" {
		t.Fatalf("got %+v, %v", result, err)
	}
	fetches, merges := 0, 0
	for _, c := range calls {
		if c.args[0] == "fetch" {
			fetches++
			want := []string{"fetch", "--all", "--prune", "--quiet"}
			if !reflect.DeepEqual(c.args, want) {
				t.Fatalf("unsafe fetch %v", c.args)
			}
		}
		if c.args[0] == "-c" {
			merges++
			if !strings.Contains(strings.Join(c.args, " "), "--ff-only") {
				t.Fatalf("not FF-only %v", c.args)
			}
		}
	}
	if fetches != 1 || merges != 1 {
		t.Fatalf("fetches=%d merges=%d", fetches, merges)
	}
}

func TestChangesDuringFetchPreventMerge(t *testing.T) {
	dir, state := cloneState(t)
	var calls []call
	inspections := 0
	e := engine{inspect: func(context.Context, string) (repository.State, error) {
		inspections++
		if inspections > 1 {
			state.Head = fetchedOID
		}
		return state, nil
	}, command: fakeCommands(t, dir, &calls)}
	result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
	if err != nil || result.Action != "fetched" || !strings.Contains(result.Reason, "HEAD changed") {
		t.Fatalf("got %+v, %v", result, err)
	}
	for _, c := range calls {
		if c.args[0] == "-c" {
			t.Fatal("merged after concurrent change")
		}
	}
}

func TestRewrittenOriginIsSkipped(t *testing.T) {
	for name, override := range map[string]map[string]string{
		"rewritten":        {"remote get-url --all origin": "https://example.com/alice/project.git\n"},
		"multiple origins": {"remote get-url --all origin": "https://github.com/alice/project.git\nhttps://github.com/alice/project.git\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, state := cloneState(t)
			var calls []call
			base := fakeCommands(t, dir, &calls)
			e := engine{inspect: func(context.Context, string) (repository.State, error) { return state, nil }, command: func(ctx context.Context, wd, program string, args ...string) ([]byte, error) {
				if out, ok := override[strings.Join(args, " ")]; ok {
					return []byte(out), nil
				}
				return base(ctx, wd, program, args...)
			}}
			result, err := e.syncOne(context.Background(), Target{Dir: filepath.Dir(dir)}, activeRepo("project"), false)
			if err != nil || result.Action != "skipped" {
				t.Fatalf("got %+v, %v", result, err)
			}
			for _, c := range calls {
				if c.args[0] == "fetch" {
					t.Fatal("unsafe fetch")
				}
			}
		})
	}
}

func TestFailedRepositoryReturnsResultsAndError(t *testing.T) {
	dir, state := cloneState(t)
	var calls []call
	base := fakeCommands(t, dir, &calls)
	e := engine{now: func() time.Time { return testNow }, inspect: func(context.Context, string) (repository.State, error) { return state, nil }, command: func(ctx context.Context, wd, program string, args ...string) ([]byte, error) {
		if program == "gh" {
			return []byte(page(t, []remoteRepo{activeRepo("project")}, false, "")), nil
		}
		if args[0] == "fetch" {
			return nil, errors.New("offline")
		}
		return base(ctx, wd, program, args...)
	}}
	results, err := e.run(context.Background(), Target{Dir: filepath.Dir(dir), Owner: "alice", Days: 45}, false)
	if err == nil || len(results) != 1 || results[0].Action != "error" {
		t.Fatalf("got %+v, %v", results, err)
	}
}

func TestCommandEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "/unexpected")
	t.Setenv("GIT_WORK_TREE", "/unexpected")
	t.Setenv("GIT_CONFIG_COUNT", "100")
	t.Setenv("GH_HOST", "unexpected.example")
	env := commandEnvironment()
	values := map[string]string{}
	for _, item := range env {
		k, v, _ := strings.Cut(item, "=")
		values[k] = v
	}
	if _, ok := values["GIT_DIR"]; ok {
		t.Fatal("inherited GIT_DIR")
	}
	if _, ok := values["GIT_WORK_TREE"]; ok {
		t.Fatal("inherited GIT_WORK_TREE")
	}
	for key, want := range map[string]string{"GIT_CONFIG_COUNT": "5", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null", "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0", "GH_PROMPT_DISABLED": "1", "GH_HOST": "github.com", "GCM_INTERACTIVE": "never"} {
		if values[key] != want {
			t.Errorf("%s=%q, want %q", key, values[key], want)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := engine{now: func() time.Time { return testNow }, command: func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte(page(t, []remoteRepo{activeRepo("project")}, false, "")), nil
	}}
	_, err := e.run(ctx, Target{Dir: syncTempDir(t), Owner: "alice", Days: 45}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRunCommandCapturesOutput(t *testing.T) {
	out, err := runCommand(context.Background(), "", "git", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "git version ") {
		t.Fatalf("unexpected output %q", out)
	}
	_, err = runCommand(context.Background(), "", "git", "repoman-nonexistent-command")
	if err == nil || !strings.Contains(err.Error(), "git:") {
		t.Fatalf("expected command error, got %v", err)
	}
}

func TestLargePositiveActivityWindows(t *testing.T) {
	old := activeRepo("project")
	pushed := testNow.AddDate(-80, 0, 0)
	old.PushedAt = &pushed
	for _, days := range []int{40000, int(^uint(0) >> 1)} {
		e := engine{now: func() time.Time { return testNow }, command: func(context.Context, string, string, ...string) ([]byte, error) {
			return []byte(page(t, []remoteRepo{old}, false, "")), nil
		}}
		results, err := e.run(context.Background(), Target{Dir: syncTempDir(t), Owner: "alice", Days: days}, true)
		if err != nil || len(results) != 1 || results[0].Action != "would-clone" {
			t.Fatalf("days=%d results=%+v err=%v", days, results, err)
		}
	}
}
