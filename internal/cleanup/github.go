package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repository"
)

var githubName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var commitSHA = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
var basenamePRNumber = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]])pr[-_]?([0-9]+)`)
var githubLikeOrigin = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://)?(?:[^/@]*@)?(?:[^/:?#]*\.)?github\.com(?:[/:?#.]|$)`)
var directPullRequestEndpoint = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pulls/[1-9][0-9]*$`)

type githubRepo struct {
	FullName string `json:"full_name"`
}

type pullRequest struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	ClosedAt *string `json:"closed_at"`
	Head     struct {
		SHA  string      `json:"sha"`
		Ref  string      `json:"ref"`
		Repo *githubRepo `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo *githubRepo `json:"repo"`
	} `json:"base"`
}

type apiReader func(context.Context, string) ([]pullRequest, error)

func lookupEvidence(ctx context.Context, state repository.State, read apiReader) (evidence, error) {
	return lookupEvidenceForLevel(ctx, state, read, Conservative)
}

func lookupEvidenceForLevel(ctx context.Context, state repository.State, read apiReader, level Level) (evidence, error) {
	repo, ok := githubOrigin(state.Origin)
	if !ok {
		if githubLikeOrigin.MatchString(strings.TrimSpace(state.Origin)) {
			return evidence{}, errors.New("GitHub origin is malformed or unsupported; PR association cannot be verified")
		}
		if level == Aggressive {
			return evidence{eligible: true, reason: "no GitHub origin; no PR association is available"}, nil
		}
		return evidence{reason: "origin is not an unambiguous github.com repository"}, nil
	}
	if !commitSHA.MatchString(state.Head) {
		return evidence{}, errors.New("HEAD is not a complete Git commit ID")
	}
	numbers, err := prNumberHints(state)
	if err != nil {
		return evidence{}, err
	}
	associated, err := read(ctx, "repos/"+repo+"/commits/"+state.Head+"/pulls?per_page=100")
	if err != nil {
		return evidence{}, fmt.Errorf("read PRs associated with current HEAD: %w", err)
	}
	terminal := false
	openReason := ""
	check := func(pr pullRequest) error {
		if err := validatePRForRepo(pr, repo); err != nil {
			return err
		}
		if pr.State == "closed" && strings.EqualFold(pr.Head.SHA, state.Head) && pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, repo) {
			terminal = true
		}
		return nil
	}
	for _, pr := range associated {
		if err := check(pr); err != nil {
			return evidence{}, err
		}
		// Any open PR associated with this commit protects it, even if its
		// branch has advanced beyond the locally checked-out commit.
		if pr.State == "open" {
			openReason = "current commit is associated with an open PR"
			progress.Report(ctx, progress.Event{Phase: "open-pr", Path: state.Path, Detail: fmt.Sprintf("PR #%d open", pr.Number)})
		}
	}
	if state.Branch != "" {
		// The REST head filter requires an owner:branch pair. Filtering on the
		// origin owner misses PRs from foreign forks, so inspect every open PR
		// in the base repository and match its branch locally instead.
		query := url.Values{"state": {"open"}, "per_page": {"100"}}
		open, err := read(ctx, "repos/"+repo+"/pulls?"+query.Encode())
		if err != nil {
			return evidence{}, fmt.Errorf("read open PRs for current branch: %w", err)
		}
		for _, pr := range open {
			if err := validatePRForRepo(pr, repo); err != nil {
				return evidence{}, err
			}
			if pr.State != "open" {
				return evidence{}, errors.New("GitHub returned a non-open PR for the open PR query")
			}
			if pr.Head.Ref == state.Branch && openReason == "" {
				openReason = "current branch has an open PR"
				progress.Report(ctx, progress.Event{Phase: "open-pr", Path: state.Path, Detail: fmt.Sprintf("PR #%d open", pr.Number)})
			}
		}
	}
	// Numeric hints remain independent evidence: a stale closed PR must never
	// hide another open PR found from the basename or a ref containing HEAD.
	for _, number := range numbers {
		prs, err := read(ctx, "repos/"+repo+"/pulls/"+strconv.Itoa(number))
		if err != nil {
			return evidence{}, fmt.Errorf("read hinted PR #%d: %w", number, err)
		}
		if len(prs) != 1 || prs[0].Number != number {
			return evidence{}, fmt.Errorf("GitHub response did not match requested PR #%d", number)
		}
		if err := check(prs[0]); err != nil {
			return evidence{}, err
		}
		if prs[0].State == "open" && openReason == "" {
			openReason = fmt.Sprintf("associated PR #%d is open", number)
			progress.Report(ctx, progress.Event{Phase: "open-pr", Path: state.Path, Detail: fmt.Sprintf("PR #%d open", number)})
		}
	}
	if openReason != "" {
		return evidence{reason: openReason}, nil
	}
	if level == Balanced || level == Aggressive {
		return evidence{eligible: true, reason: "GitHub checks verified no open PR for current commit, branch, or PR-number hints"}, nil
	}
	if !terminal {
		return evidence{reason: "no terminal PR matches exact current HEAD and origin repository"}, nil
	}
	return evidence{eligible: true, reason: "clean linked worktree; exact current HEAD has a terminal PR and no open PR"}, nil
}

func prNumberHints(state repository.State) ([]int, error) {
	numbers := make(map[int]struct{}, len(state.PRNumbers))
	for _, number := range state.PRNumbers {
		if number <= 0 {
			return nil, errors.New("local PR reference has an invalid PR number")
		}
		numbers[number] = struct{}{}
	}
	for _, match := range basenamePRNumber.FindAllStringSubmatch(filepath.Base(state.Path), -1) {
		number, err := strconv.Atoi(match[1])
		if err != nil || number <= 0 {
			return nil, errors.New("worktree basename has an invalid PR number")
		}
		numbers[number] = struct{}{}
	}
	result := make([]int, 0, len(numbers))
	for number := range numbers {
		result = append(result, number)
	}
	sort.Ints(result)
	return result, nil
}

func validatePRForRepo(pr pullRequest, repo string) error {
	if err := validatePR(pr); err != nil {
		return err
	}
	if !strings.EqualFold(pr.Base.Repo.FullName, repo) {
		return errors.New("GitHub returned a PR from a different base repository")
	}
	return nil
}

func validatePR(pr pullRequest) error {
	if pr.Number <= 0 || pr.Base.Repo == nil || pr.Base.Repo.FullName == "" || !commitSHA.MatchString(pr.Head.SHA) || pr.Head.Ref == "" {
		return errors.New("GitHub returned incomplete PR identity")
	}
	if pr.State != "open" && pr.State != "closed" {
		return errors.New("GitHub returned an unknown PR state")
	}
	if pr.State == "closed" && (pr.ClosedAt == nil || *pr.ClosedAt == "") {
		return errors.New("GitHub returned a closed PR without terminal evidence")
	}
	if pr.State == "closed" {
		if _, err := time.Parse(time.RFC3339, *pr.ClosedAt); err != nil {
			return errors.New("GitHub returned an invalid PR closing timestamp")
		}
	}
	return nil
}

func githubOrigin(origin string) (string, bool) {
	var path string
	if strings.HasPrefix(origin, "git@github.com:") {
		path = strings.TrimPrefix(origin, "git@github.com:")
	} else {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, "github.com") || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "", false
		}
		if u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git") {
			return "", false
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !githubName.MatchString(parts[0]) || !githubName.MatchString(parts[1]) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", false
	}
	return path, true
}

func readPullRequests(ctx context.Context, endpoint string) ([]pullRequest, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "--paginate", "--slurp", "-H", "Accept: application/vnd.github+json", endpoint) // #nosec G204 -- fixed gh executable and validated repository, commit, and PR identities; no shell.
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	// Do not leak authentication diagnostics or raw PR bodies into normal output.
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("GitHub API lookup failed: %w", err)
	}
	if directPullRequestEndpoint.MatchString(endpoint) {
		return decodePullRequest(output)
	}
	return decodePages(output)
}

func decodePullRequest(output []byte) ([]pullRequest, error) {
	// gh api --paginate --slurp wraps a direct /pulls/N object in a
	// one-element array, while list endpoints yield arrays of pages.
	var pages []json.RawMessage
	if err := json.Unmarshal(output, &pages); err != nil || len(pages) != 1 {
		return nil, errors.New("GitHub API did not return a single PR object")
	}
	var pr pullRequest
	if err := json.Unmarshal(pages[0], &pr); err != nil {
		return nil, errors.New("GitHub API returned an invalid PR object")
	}
	if err := validatePR(pr); err != nil {
		return nil, err
	}
	return []pullRequest{pr}, nil
}

func decodePages(output []byte) ([]pullRequest, error) {
	var pages []json.RawMessage
	if err := json.Unmarshal(output, &pages); err != nil || pages == nil || len(pages) == 0 {
		return nil, errors.New("GitHub API did not return a paginated JSON array")
	}
	all := make([]pullRequest, 0)
	for _, raw := range pages {
		var page []pullRequest
		if err := json.Unmarshal(raw, &page); err != nil || page == nil {
			return nil, errors.New("GitHub API returned an invalid PR page")
		}
		for _, pr := range page {
			if err := validatePR(pr); err != nil {
				return nil, err
			}
		}
		all = append(all, page...)
	}
	return all, nil
}
