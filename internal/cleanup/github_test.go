package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/repository"
)

func testPR(state, sha, ref, repo string) pullRequest {
	pr := pullRequest{Number: 42, State: state}
	pr.Head.SHA, pr.Head.Ref, pr.Head.Repo = sha, ref, &githubRepo{FullName: repo}
	pr.Base.Repo = &githubRepo{FullName: "owner/repo"}
	if state == "closed" {
		closed := "2026-09-30T12:34:56Z"
		pr.ClosedAt = &closed
	}
	return pr
}

func TestGitHubEvidence(t *testing.T) {
	head := strings.Repeat("a", 40)
	terminal := testPR("closed", head, "topic", "owner/repo")
	state := repository.State{Head: head, Branch: "topic", Origin: "git@github.com:owner/repo.git"}
	tests := []struct {
		name       string
		associated []pullRequest
		branchOpen []pullRequest
		errAt      int
		eligible   bool
		wantError  bool
	}{
		{name: "exact terminal head", associated: []pullRequest{terminal}, eligible: true},
		{name: "no PR evidence"},
		{name: "older terminal head", associated: []pullRequest{testPR("closed", strings.Repeat("b", 40), "topic", "owner/repo")}},
		{name: "same SHA different origin", associated: []pullRequest{testPR("closed", head, "topic", "fork/repo")}},
		{name: "open commit PR", associated: []pullRequest{terminal, testPR("open", head, "other", "fork/repo")}},
		{name: "reused branch open PR", associated: []pullRequest{terminal}, branchOpen: []pullRequest{testPR("open", strings.Repeat("c", 40), "topic", "owner/repo")}},
		{name: "wrong branch response", associated: []pullRequest{terminal}, branchOpen: []pullRequest{testPR("open", head, "different", "owner/repo")}, wantError: true},
		{name: "commit API failure", errAt: 1, wantError: true},
		{name: "branch API failure", associated: []pullRequest{terminal}, errAt: 2, wantError: true},
		{name: "malformed PR", associated: []pullRequest{{}}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			proof, err := lookupEvidence(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
				calls++
				if calls == test.errAt {
					return nil, errors.New("unavailable")
				}
				if calls == 1 {
					if !strings.Contains(endpoint, "/commits/"+head+"/pulls?") || !strings.Contains(endpoint, "per_page=100") {
						t.Fatalf("unexpected endpoint %q", endpoint)
					}
					return test.associated, nil
				}
				if !strings.Contains(endpoint, "head=owner%3Atopic") || !strings.Contains(endpoint, "state=open") {
					t.Fatalf("unexpected branch endpoint %q", endpoint)
				}
				return test.branchOpen, nil
			})
			if (err != nil) != test.wantError || proof.eligible != test.eligible {
				t.Fatalf("proof=%+v err=%v", proof, err)
			}
		})
	}
}

func TestGitHubEvidenceDetachedAndEncodedBranch(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, branch := range []string{"", "topic/with&symbols"} {
		t.Run(fmt.Sprintf("branch=%q", branch), func(t *testing.T) {
			calls := 0
			state := repository.State{Head: head, Branch: branch, Origin: "https://github.com/owner/repo.git"}
			proof, err := lookupEvidence(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
				calls++
				if calls == 1 {
					return []pullRequest{testPR("closed", head, "topic", "owner/repo")}, nil
				}
				if !strings.Contains(endpoint, "head=owner%3Atopic%2Fwith%26symbols") {
					t.Fatalf("branch was not URL-encoded: %q", endpoint)
				}
				return nil, nil
			})
			wantCalls := 2
			if branch == "" {
				wantCalls = 1
			}
			if err != nil || !proof.eligible || calls != wantCalls {
				t.Fatalf("proof=%+v err=%v calls=%d", proof, err, calls)
			}
		})
	}
}

func TestMalformedPREvidenceFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*pullRequest)
	}{
		{"no close timestamp", func(pr *pullRequest) { pr.ClosedAt = nil }},
		{"invalid timestamp", func(pr *pullRequest) { s := "yesterday"; pr.ClosedAt = &s }},
		{"unknown state", func(pr *pullRequest) { pr.State = "merged" }},
		{"incomplete SHA", func(pr *pullRequest) { pr.Head.SHA = "aabbcc" }},
		{"missing base", func(pr *pullRequest) { pr.Base.Repo = nil }},
		{"wrong base", func(pr *pullRequest) { pr.Base.Repo.FullName = "other/repo" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			head := strings.Repeat("a", 40)
			pr := testPR("closed", head, "topic", "owner/repo")
			test.modify(&pr)
			proof, err := lookupEvidence(context.Background(), repository.State{Head: head, Origin: "git@github.com:owner/repo.git"}, func(context.Context, string) ([]pullRequest, error) {
				return []pullRequest{pr}, nil
			})
			if err == nil || proof.eligible {
				t.Fatalf("accepted malformed PR: proof=%+v err=%v", proof, err)
			}
		})
	}
}

func TestGitHubOrigins(t *testing.T) {
	for _, origin := range []string{"git@github.com:owner/repo.git", "https://github.com/owner/repo.git", "ssh://git@github.com/owner/repo.git", "https://GITHUB.com/owner/repo"} {
		if got, ok := githubOrigin(origin); !ok || got != "owner/repo" {
			t.Errorf("%q got %q %v", origin, got, ok)
		}
	}
	for _, origin := range []string{"", "../repo", "https://gitlab.com/owner/repo", "https://github.com/owner/repo?x=1", "https://github.com/owner/repo#x", "https://token@github.com/owner/repo", "https://github.com/owner/repo/more", "https://github.com/../repo", "git@github.com:owner/repo;echo", "https://github.com.evil.invalid/owner/repo"} {
		if got, ok := githubOrigin(origin); ok {
			t.Errorf("unsafe origin %q accepted as %q", origin, got)
		}
	}
}

func TestDecodeAllGitHubPages(t *testing.T) {
	head := strings.Repeat("a", 40)
	pages := [][]pullRequest{{testPR("closed", head, "topic", "owner/repo")}, {testPR("open", head, "topic", "owner/repo")}, {}}
	encoded, err := json.Marshal(pages)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodePages(encoded)
	if err != nil || len(got) != 2 || got[1].State != "open" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	for _, raw := range []string{"", "null", "[]", "[null]", "{}", "[{}]", "[[{}]", "[[]]garbage", `{"message":"rate limit exceeded"}`} {
		if _, err := decodePages([]byte(raw)); err == nil {
			t.Errorf("accepted malformed response %q", raw)
		}
	}
	if got, err := decodePages([]byte("[[]]")); err != nil || len(got) != 0 {
		t.Fatalf("empty successful response rejected: %v %v", got, err)
	}
}

func TestGHCommandUsesPaginationAndNoninteractiveMode(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\n[ \"$GH_PROMPT_DISABLED\" = 1 ] || exit 90\n[ \"$GIT_TERMINAL_PROMPT\" = 0 ] || exit 91\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\nprintf '[[]]'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- executable fixture script in an isolated test directory.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARGS_FILE", argsFile)
	if _, err := readPullRequests(context.Background(), "repos/owner/repo/commits/"+strings.Repeat("a", 40)+"/pulls?per_page=100"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--hostname\ngithub.com\n", "--paginate\n", "--slurp\n", "Accept: application/vnd.github+json\n"} {
		if !strings.Contains(string(args), required) {
			t.Errorf("missing %q in %q", required, args)
		}
	}
}
