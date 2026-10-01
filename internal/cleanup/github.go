package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

var githubName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var commitSHA = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

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

func githubEvidence(ctx context.Context, state repository.State) (evidence, error) {
	return lookupEvidence(ctx, state, readPullRequests)
}

func lookupEvidence(ctx context.Context, state repository.State, read apiReader) (evidence, error) {
	return lookupEvidenceForLevel(ctx, state, read, Conservative)
}

func lookupEvidenceForLevel(ctx context.Context, state repository.State, read apiReader, level Level) (evidence, error) {
	repo, ok := githubOrigin(state.Origin)
	if !ok {
		return evidence{reason: "origin is not an unambiguous github.com repository"}, nil
	}
	if !commitSHA.MatchString(state.Head) {
		return evidence{}, errors.New("HEAD is not a complete Git commit ID")
	}
	associated, err := read(ctx, "repos/"+repo+"/commits/"+state.Head+"/pulls?per_page=100")
	if err != nil {
		return evidence{}, fmt.Errorf("read PRs associated with current HEAD: %w", err)
	}
	terminal := false
	for _, pr := range associated {
		if err := validatePR(pr); err != nil {
			return evidence{}, err
		}
		if !strings.EqualFold(pr.Base.Repo.FullName, repo) {
			return evidence{}, errors.New("GitHub returned a PR from a different base repository")
		}
		// Any open PR associated with this commit protects it, even if its
		// branch has advanced beyond the locally checked-out commit.
		if pr.State == "open" {
			return evidence{reason: "current commit is associated with an open PR"}, nil
		}
		if strings.EqualFold(pr.Head.SHA, state.Head) && pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, repo) {
			terminal = true
		}
	}
	if state.Branch != "" {
		owner, _, _ := strings.Cut(repo, "/")
		query := url.Values{"state": {"open"}, "head": {owner + ":" + state.Branch}, "per_page": {"100"}}
		open, err := read(ctx, "repos/"+repo+"/pulls?"+query.Encode())
		if err != nil {
			return evidence{}, fmt.Errorf("read open PRs for current branch: %w", err)
		}
		for _, pr := range open {
			if err := validatePR(pr); err != nil {
				return evidence{}, err
			}
			if pr.State != "open" || !strings.EqualFold(pr.Base.Repo.FullName, repo) || pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, repo) || pr.Head.Ref != state.Branch {
				return evidence{}, errors.New("GitHub branch PR response did not match requested branch and repository")
			}
		}
		if len(open) > 0 {
			return evidence{reason: "current branch has an open PR"}, nil
		}
	}
	if level == Balanced || level == Aggressive {
		return evidence{eligible: true, reason: "GitHub checks verified no open PR for current commit or branch"}, nil
	}
	if !terminal {
		return evidence{reason: "no terminal PR matches exact current HEAD and origin repository"}, nil
	}
	return evidence{eligible: true, reason: "clean linked worktree; exact current HEAD has a terminal PR and no open PR"}, nil
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
		if err != nil || !strings.EqualFold(u.Host, "github.com") || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
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
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "--paginate", "--slurp", "-H", "Accept: application/vnd.github+json", endpoint) // #nosec G204 -- fixed gh executable, validated repository/commit IDs, and URL-encoded branch parameters; no shell.
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	// Do not leak authentication diagnostics or raw PR bodies into normal output.
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("GitHub API lookup failed: %w", err)
	}
	return decodePages(output)
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
		all = append(all, page...)
	}
	return all, nil
}
