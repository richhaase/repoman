package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		{name: "unrelated open branch", associated: []pullRequest{terminal}, branchOpen: []pullRequest{testPR("open", head, "different", "owner/repo")}, eligible: true},
		{name: "foreign fork branch", associated: []pullRequest{terminal}, branchOpen: []pullRequest{testPR("open", strings.Repeat("b", 40), "topic", "fork/repo")}},
		{name: "closed in open response", associated: []pullRequest{terminal}, branchOpen: []pullRequest{terminal}, wantError: true},
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
				if endpoint != "repos/owner/repo/pulls?per_page=100&state=open" {
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

func TestGitHubEvidenceDetachedAndSpecialBranch(t *testing.T) {
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
				if endpoint != "repos/owner/repo/pulls?per_page=100&state=open" {
					t.Fatalf("branch query did not cover every fork: %q", endpoint)
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

func TestGitHubEvidencePRNumberHints(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, test := range []struct {
			name       string
			path       string
			numbers    []int
			openNumber int
			want       []int
		}{
			{name: "hyphen basename", path: "/work/pr-42", want: []int{42}},
			{name: "underscore basename", path: "/work/pr_42", want: []int{42}},
			{name: "compact basename", path: "/work/pr42", want: []int{42}},
			{name: "all basename hints", path: "/work/repo-pr42-pr_43-pr-44", want: []int{42, 43, 44}, openNumber: 44},
			{name: "containing refs", path: "/work/topic", numbers: []int{43, 42}, want: []int{42, 43}, openNumber: 43},
			{name: "deduplicated combined hints", path: "/work/repo-pr42", numbers: []int{43, 42, 43}, want: []int{42, 43}, openNumber: 43},
			{name: "parent name ignored", path: "/work/pr42/topic"},
		} {
			t.Run(string(level)+"/"+test.name, func(t *testing.T) {
				state := repository.State{Path: test.path, Head: head, Branch: "topic", Origin: "git@github.com:owner/repo.git", PRNumbers: test.numbers}
				var got []int
				calls := 0
				proof, err := lookupEvidenceForLevel(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
					calls++
					if strings.Contains(endpoint, "/commits/") {
						return []pullRequest{testPR("closed", head, "topic", "owner/repo")}, nil
					}
					if strings.Contains(endpoint, "?") {
						return nil, nil
					}
					number, parseErr := strconv.Atoi(strings.TrimPrefix(endpoint, "repos/owner/repo/pulls/"))
					if parseErr != nil {
						t.Fatalf("unexpected endpoint %q: %v", endpoint, parseErr)
					}
					got = append(got, number)
					status := "closed"
					if number == test.openNumber {
						status = "open"
					}
					pr := testPR(status, strings.Repeat("b", 40), "advanced", "fork/repo")
					pr.Number = number
					return []pullRequest{pr}, nil
				}, level)
				if err != nil || proof.eligible != (test.openNumber == 0) || !reflect.DeepEqual(got, test.want) || calls != 2+len(test.want) {
					t.Fatalf("proof=%+v err=%v hints=%v want=%v calls=%d", proof, err, got, test.want, calls)
				}
			})
		}
	}
}

func TestGitHubHintFailuresProtectEveryLevel(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, kind := range []string{"API failure", "missing response", "multiple responses", "wrong number", "wrong base", "incomplete identity"} {
			t.Run(string(level)+"/"+kind, func(t *testing.T) {
				state := repository.State{Path: "/work/pr42", Head: head, Origin: "https://github.com/owner/repo"}
				calls := 0
				proof, err := lookupEvidenceForLevel(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
					calls++
					if strings.Contains(endpoint, "/commits/") {
						return []pullRequest{testPR("closed", head, "topic", "owner/repo")}, nil
					}
					pr := testPR("closed", head, "topic", "owner/repo")
					switch kind {
					case "API failure":
						return nil, errors.New("unavailable")
					case "missing response":
						return nil, nil
					case "multiple responses":
						return []pullRequest{pr, pr}, nil
					case "wrong number":
						pr.Number = 43
					case "wrong base":
						pr.Base.Repo.FullName = "other/repo"
					case "incomplete identity":
						pr.Head.SHA = ""
					}
					return []pullRequest{pr}, nil
				}, level)
				if err == nil || proof.eligible || calls != 2 {
					t.Fatalf("proof=%+v err=%v calls=%d", proof, err, calls)
				}
			})
		}
	}
}

func TestOpenPRDoesNotHideLaterHintOrFailure(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			state := repository.State{Path: "/work/pr43", Head: head, Branch: "topic", Origin: "https://github.com/owner/repo", PRNumbers: []int{42}}
			calls := 0
			proof, err := lookupEvidenceForLevel(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
				calls++
				pr := testPR("open", head, "topic", "owner/repo")
				if strings.HasSuffix(endpoint, "/43") {
					if fail {
						return nil, errors.New("unavailable")
					}
					pr.Number = 43
				}
				return []pullRequest{pr}, nil
			}, Aggressive)
			if (err != nil) != fail || proof.eligible || calls != 4 {
				t.Fatalf("proof=%+v err=%v calls=%d", proof, err, calls)
			}
		})
	}
}

func TestInvalidPRNumberHintsFailBeforeAPI(t *testing.T) {
	for _, state := range []repository.State{
		{Path: "/work/pr0"},
		{Path: "/work/pr99999999999999999999999999999"},
		{PRNumbers: []int{0}},
		{PRNumbers: []int{-1}},
	} {
		state.Head, state.Origin = strings.Repeat("a", 40), "https://github.com/owner/repo"
		proof, err := lookupEvidenceForLevel(context.Background(), state, func(context.Context, string) ([]pullRequest, error) {
			t.Fatal("invalid hint queried GitHub")
			return nil, nil
		}, Aggressive)
		if err == nil || proof.eligible {
			t.Fatalf("invalid hint accepted: state=%+v proof=%+v err=%v", state, proof, err)
		}
	}
}

func TestNumericHintConservativeRequiresExactTerminalHEAD(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, kind := range []string{"exact", "older", "fork"} {
		t.Run(kind, func(t *testing.T) {
			state := repository.State{Path: "/work/pr42", Head: head, Origin: "https://github.com/owner/repo"}
			proof, err := lookupEvidence(context.Background(), state, func(_ context.Context, endpoint string) ([]pullRequest, error) {
				if strings.Contains(endpoint, "/commits/") {
					return nil, nil
				}
				pr := testPR("closed", head, "topic", "owner/repo")
				switch kind {
				case "older":
					pr.Head.SHA = strings.Repeat("b", 40)
				case "fork":
					pr.Head.Repo.FullName = "fork/repo"
				}
				return []pullRequest{pr}, nil
			})
			if err != nil || proof.eligible != (kind == "exact") {
				t.Fatalf("proof=%+v err=%v", proof, err)
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
	for _, origin := range []string{"git@github.com:owner/repo.git", "https://github.com/owner/repo.git", "http://github.com/owner/repo.git", "ssh://git@github.com/owner/repo.git", "https://GITHUB.com/owner/repo"} {
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

func TestOriginEligibilityByLevel(t *testing.T) {
	for _, level := range []Level{Conservative, Balanced, Aggressive} {
		for _, origin := range []string{"", "/local/repo", "https://gitlab.com/owner/repo", "https://gitlab.com/owner/github.com-tools"} {
			t.Run(string(level)+"/"+origin, func(t *testing.T) {
				proof, err := lookupEvidenceForLevel(context.Background(), repository.State{Head: strings.Repeat("a", 40), Origin: origin}, func(context.Context, string) ([]pullRequest, error) {
					t.Fatal("non-GitHub origin queried GitHub")
					return nil, nil
				}, level)
				if err != nil || proof.eligible != (level == Aggressive) {
					t.Fatalf("proof=%+v err=%v", proof, err)
				}
			})
		}
	}
}

func TestMalformedGitHubOriginFailsClosed(t *testing.T) {
	for _, origin := range []string{
		"git@github.com:owner/repo/more",
		"https://github.com/owner/repo?x=1",
		"https://github.com/owner/repo?",
		"https://github.com/owner/repo#x",
		"https://token@github.com/owner/repo",
		"https://github.com:443/owner/repo",
		"https://github.com/owner",
		"http://github.com/owner/repo/more",
		"git://github.com/owner/repo",
		"https://github.com.evil.invalid/owner/repo",
		"https://github.com/%invalid/repo",
		" https://github.com/owner/repo",
	} {
		t.Run(origin, func(t *testing.T) {
			proof, err := lookupEvidenceForLevel(context.Background(), repository.State{Head: strings.Repeat("a", 40), Origin: origin}, func(context.Context, string) ([]pullRequest, error) {
				t.Fatal("malformed GitHub origin queried GitHub")
				return nil, nil
			}, Aggressive)
			if err == nil || proof.eligible {
				t.Fatalf("malformed GitHub origin accepted: proof=%+v err=%v", proof, err)
			}
		})
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
	for _, pages := range []any{
		[]any{[]pullRequest{testPR("closed", head, "topic", "owner/repo")}, nil},
		[][]pullRequest{{testPR("closed", head, "topic", "owner/repo")}, {{}}},
		[]any{testPR("closed", head, "topic", "owner/repo"), []pullRequest{}},
	} {
		encoded, err := json.Marshal(pages)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodePages(encoded); err == nil {
			t.Errorf("accepted invalid later page %s", encoded)
		}
	}
}

func TestDecodeDirectPullRequest(t *testing.T) {
	pr := testPR("open", strings.Repeat("a", 40), "topic", "fork/repo")
	encoded, err := json.Marshal([]pullRequest{pr})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodePullRequest(encoded)
	if err != nil || !reflect.DeepEqual(got, []pullRequest{pr}) {
		t.Fatalf("direct PR response=%+v err=%v", got, err)
	}
	if _, err := decodePages(encoded); err == nil {
		t.Fatal("list endpoint accepted a direct PR object")
	}
	for _, raw := range []string{"", "null", "[]", "[null]", "{}", "[{}]", "[[]]", "[[{}]]", string(encoded) + "garbage"} {
		if _, err := decodePullRequest([]byte(raw)); err == nil {
			t.Errorf("accepted invalid direct PR response %q", raw)
		}
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

func TestGHCommandDecodesDirectAndPaginatedResponses(t *testing.T) {
	head := strings.Repeat("a", 40)
	closed := testPR("closed", head, "topic", "owner/repo")
	open := testPR("open", head, "topic", "fork/repo")
	for _, test := range []struct {
		name     string
		endpoint string
		response any
		want     []pullRequest
	}{
		{"direct PR object", "repos/owner/repo/pulls/42", []pullRequest{open}, []pullRequest{open}},
		{"open PR on later page", "repos/owner/repo/commits/" + head + "/pulls?per_page=100", [][]pullRequest{{closed}, {}, {open}}, []pullRequest{closed, open}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			responseFile := filepath.Join(bin, "response.json")
			response, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(responseFile, response, 0o600); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n[ \"$GH_PROMPT_DISABLED\" = 1 ] || exit 90\n[ \"$GIT_TERMINAL_PROMPT\" = 0 ] || exit 91\ncat \"$RESPONSE_FILE\"\n"
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- executable fixture script in an isolated test directory.
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("RESPONSE_FILE", responseFile)
			got, err := readPullRequests(context.Background(), test.endpoint)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("response=%+v want=%+v err=%v", got, test.want, err)
			}
		})
	}
}

func TestGHCommandFailureDoesNotExposeDiagnostics(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'private-auth-diagnostics' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- executable fixture script in an isolated test directory.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := readPullRequests(context.Background(), "repos/owner/repo/pulls/42"); err == nil || strings.Contains(err.Error(), "private-auth-diagnostics") {
		t.Fatalf("API failure did not remain safely blocked: %v", err)
	}
}

func TestOrdinaryBasenameDoesNotImplyPRNumber(t *testing.T) {
	for _, name := range []string{"apr2026", "sprintpr42", "repr-17"} {
		numbers, err := prNumberHints(repository.State{Path: "/tmp/" + name})
		if err != nil || len(numbers) != 0 {
			t.Fatalf("ordinary basename %q produced PR hints: %v %v", name, numbers, err)
		}
	}
}
