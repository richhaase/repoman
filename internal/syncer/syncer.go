// Package syncer conservatively clones and fast-forwards active GitHub repositories.
// It never resets, forces a checkout, deletes a repository, or pushes a ref.
package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richhaase/repoman/internal/repository"
)

// Target identifies a GitHub owner and the parent directory for its clones.
// Includes and Excludes match repository names using path.Match globs.
// Event-based activity is deliberately unsupported; nil or false means pushes only.
type Target struct {
	Dir          string   `json:"dir"`
	Owner        string   `json:"owner"`
	Days         int      `json:"days"`
	Includes     []string `json:"includes,omitempty"`
	Excludes     []string `json:"excludes,omitempty"`
	Events       *bool    `json:"events,omitempty"`
	CleanupLevel string   `json:"cleanup_level,omitempty"`
}

// Result records either a completed action, a dry-run plan, or a reason for skipping.
type Result struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

type remoteRepo struct {
	Name          string     `json:"name"`
	FullName      string     `json:"nameWithOwner"`
	PushedAt      *time.Time `json:"pushedAt"`
	Archived      bool       `json:"isArchived"`
	DefaultBranch *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
}

type engine struct {
	command func(context.Context, string, string, ...string) ([]byte, error)
	inspect func(context.Context, string) (repository.State, error)
	now     func() time.Time
}

// Run lists every accessible repository belonging to the owner, then processes
// them in name order. Any failed listing is fatal before filesystem mutations.
// Per-repository failures are returned both as results and a joined error.
// Dry runs never create directories, fetch, clone, merge, or write Git metadata.
func Run(ctx context.Context, target Target, dryRun bool) ([]Result, error) {
	return (&engine{command: runCommand, inspect: func(ctx context.Context, path string) (repository.State, error) {
		return repository.Inspect(ctx, path), nil
	}, now: time.Now}).run(ctx, target, dryRun)
}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (e *engine) run(ctx context.Context, target Target, dryRun bool) ([]Result, error) {
	if err := validateTarget(&target); err != nil {
		return nil, err
	}
	if err := checkRoot(target.Dir); err != nil {
		return nil, err
	}
	repos, err := e.list(ctx, target.Owner)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(repos))
	var failures []error
	cutoff := e.now().UTC().AddDate(0, 0, -target.Days)
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return results, errors.Join(append(failures, err)...)
		}
		result := Result{Name: repo.Name, Path: filepath.Join(target.Dir, repo.Name), Action: "skipped"}
		switch {
		case !matches(target.Includes, repo.Name, true):
			result.Reason = "not included"
		case matches(target.Excludes, repo.Name, false):
			result.Reason = "excluded"
		case repo.Archived:
			result.Reason = "archived"
		case repo.PushedAt == nil || repo.PushedAt.Before(cutoff):
			result.Reason = "inactive"
		case repo.DefaultBranch == nil || repo.DefaultBranch.Name == "":
			result.Reason = "no default branch"
		default:
			result, err = e.syncOne(ctx, target, repo, dryRun)
			if err != nil {
				result.Action = "error"
				result.Reason = err.Error()
				failures = append(failures, fmt.Errorf("%s: %w", repo.Name, err))
			}
		}
		results = append(results, result)
	}
	return results, errors.Join(failures...)
}

func validateTarget(target *Target) error {
	if !ownerPattern.MatchString(target.Owner) {
		return errors.New("owner must be a GitHub user or organization login")
	}
	if target.Days < 1 || target.Days > 36500 {
		return errors.New("activity days must be between 1 and 36500")
	}
	if target.Events != nil && *target.Events {
		return errors.New("event-based activity is unsupported; choose push-only activity (events=false or --no-events)")
	}
	if strings.TrimSpace(target.Dir) == "" || strings.ContainsAny(target.Dir, "\x00\r\n") {
		return errors.New("target directory must be a non-empty single-line path")
	}
	abs, err := filepath.Abs(target.Dir)
	if err != nil {
		return fmt.Errorf("resolve target directory: %w", err)
	}
	target.Dir = filepath.Clean(abs)
	if filepath.Dir(target.Dir) == target.Dir {
		return errors.New("the filesystem root cannot be a sync target")
	}
	for _, pattern := range append(append([]string{}, target.Includes...), target.Excludes...) {
		if pattern == "" || strings.ContainsAny(pattern, "\x00\r\n/") {
			return fmt.Errorf("invalid repository pattern %q", pattern)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid repository pattern %q: %w", pattern, err)
		}
	}
	return nil
}

func validName(name string) bool {
	return name != "." && name != ".." && !strings.EqualFold(name, ".git") && len(name) <= 100 && namePattern.MatchString(name)
}

func matches(patterns []string, name string, empty bool) bool {
	if len(patterns) == 0 {
		return empty
	}
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

const repoQuery = `query($owner:String!,$endCursor:String){repositoryOwner(login:$owner){repositories(first:100,after:$endCursor){nodes{name nameWithOwner pushedAt isArchived defaultBranchRef{name}}pageInfo{hasNextPage endCursor}}}}`

func (e *engine) list(ctx context.Context, owner string) ([]remoteRepo, error) {
	out, err := e.command(ctx, "", "gh", "api", "--hostname", "github.com", "graphql", "--paginate", "-f", "query="+repoQuery, "-f", "owner="+owner)
	if err != nil {
		return nil, fmt.Errorf("list GitHub repositories: %w", err)
	}
	// gh api --paginate emits one JSON object per page. Decode the complete stream
	// rather than imposing a result cap that could silently hide repositories.
	decoder := json.NewDecoder(bytes.NewReader(out))
	var repos []remoteRepo
	seen := make(map[string]bool)
	cursors := make(map[string]bool)
	pages, more := 0, true
	for {
		var page struct {
			Data *struct {
				Owner *struct {
					Repos *struct {
						Nodes    []remoteRepo `json:"nodes"`
						PageInfo *struct {
							More   *bool   `json:"hasNextPage"`
							Cursor *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"repositories"`
				} `json:"repositoryOwner"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		err := decoder.Decode(&page)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode GitHub repository listing: %w", err)
		}
		if !more {
			return nil, errors.New("GitHub returned unexpected data after its final repository page")
		}
		if len(page.Errors) > 0 || page.Data == nil || page.Data.Owner == nil || page.Data.Owner.Repos == nil || page.Data.Owner.Repos.PageInfo == nil {
			return nil, errors.New("GitHub repository listing was incomplete or returned an API error")
		}
		connection := page.Data.Owner.Repos
		if connection.PageInfo.More == nil || connection.Nodes == nil {
			return nil, errors.New("GitHub repository listing omitted required pagination data")
		}
		pages++
		more = *connection.PageInfo.More
		if more {
			cursor := connection.PageInfo.Cursor
			if cursor == nil || *cursor == "" || cursors[*cursor] || len(connection.Nodes) == 0 {
				return nil, errors.New("GitHub repository pagination did not advance")
			}
			cursors[*cursor] = true
		}
		for _, repo := range connection.Nodes {
			if !validName(repo.Name) || !strings.EqualFold(repo.FullName, owner+"/"+repo.Name) {
				return nil, fmt.Errorf("GitHub returned an unsafe or unexpected repository identity %q", repo.FullName)
			}
			key := strings.ToLower(repo.Name)
			if seen[key] {
				return nil, fmt.Errorf("GitHub returned duplicate repository %q", repo.Name)
			}
			seen[key] = true
			repos = append(repos, repo)
		}
	}
	if pages == 0 || more {
		return nil, errors.New("GitHub repository listing ended before pagination completed")
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, nil
}

func (e *engine) syncOne(ctx context.Context, target Target, repo remoteRepo, dryRun bool) (Result, error) {
	result := Result{Name: repo.Name, Path: filepath.Join(target.Dir, repo.Name), Action: "skipped"}
	branch := repo.DefaultBranch.Name
	if branch == "HEAD" {
		result.Reason = "unsafe default branch"
		return result, nil
	}
	if _, err := e.command(ctx, "", "git", "check-ref-format", "refs/heads/"+branch); err != nil {
		result.Reason = "invalid or unverifiable default branch"
		return result, nil
	}
	if err := checkRoot(target.Dir); err != nil {
		return result, err
	}
	info, err := os.Lstat(result.Path)
	if errors.Is(err, os.ErrNotExist) {
		if dryRun {
			result.Action = "would-clone"
			result.Reason = "active repository"
			return result, nil
		}
		if err := os.MkdirAll(target.Dir, 0o700); err != nil {
			return result, fmt.Errorf("create target: %w", err)
		}
		if err := checkRoot(target.Dir); err != nil {
			return result, err
		}
		// Do not let git clone reuse even an empty destination supplied by someone else.
		if _, err := os.Lstat(result.Path); !errors.Is(err, os.ErrNotExist) {
			return result, errors.New("destination appeared before cloning; left untouched")
		}
		_, err := e.command(ctx, "", "gh", "repo", "clone", "https://github.com/"+repo.FullName+".git", result.Path, "--", "--quiet", "--no-recurse-submodules", "--origin", "origin")
		if err != nil {
			return result, fmt.Errorf("clone failed (any partial destination was preserved): %w", err)
		}
		result.Action = "cloned"
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("inspect destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		result.Reason = "destination is not a real directory"
		return result, nil
	}
	state, reason := e.safeState(ctx, result.Path, repo, branch)
	if reason != "" {
		result.Reason = reason
		return result, nil
	}
	ref := "refs/remotes/origin/" + branch
	ahead, behind, err := e.distance(ctx, result.Path, ref)
	if err != nil {
		result.Reason = "cannot verify local history against origin default branch"
		return result, nil
	}
	if ahead > 0 {
		result.Reason = historyReason(ahead, behind)
		return result, nil
	}
	if dryRun {
		result.Action = "would-sync"
		result.Reason = "would fetch origin and fast-forward if still safe; remote freshness is unknown"
		return result, nil
	}
	if err := checkRoot(target.Dir); err != nil {
		return result, err
	}
	// Explicit refspecs avoid trusting a local remote's configured fetch targets.
	// No pruning, forced ref update, tags, submodules, or other remotes are involved.
	_, err = e.command(ctx, result.Path, "git", "fetch", "--quiet", "--no-tags", "--no-prune", "--no-prune-tags", "--no-recurse-submodules", "origin", "refs/heads/"+branch+":"+ref)
	if err != nil {
		return result, fmt.Errorf("fetch origin failed: %w", err)
	}
	after, reason := e.safeState(ctx, result.Path, repo, branch)
	if reason != "" {
		result.Reason = "after fetch: " + reason
		return result, nil
	}
	if after.Head != state.Head {
		result.Reason = "HEAD changed while fetching"
		return result, nil
	}
	ahead, behind, err = e.distance(ctx, result.Path, ref)
	if err != nil {
		result.Reason = "cannot verify fetched default branch"
		return result, nil
	}
	if ahead > 0 {
		result.Reason = historyReason(ahead, behind)
		return result, nil
	}
	if behind == 0 {
		result.Action = "up-to-date"
		return result, nil
	}
	// Resolve to an immutable commit so a concurrent fetch cannot change what is merged.
	commit, err := e.command(ctx, result.Path, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return result, fmt.Errorf("resolve fetched commit: %w", err)
	}
	oid := strings.TrimSpace(string(commit))
	if !validOID(oid) {
		return result, errors.New("fetched commit has an invalid object ID")
	}
	if _, err := e.command(ctx, result.Path, "git", "merge-base", "--is-ancestor", "HEAD", oid); err != nil {
		result.Reason = "fetched history is not a verified fast-forward"
		return result, nil
	}
	latest, reason := e.safeState(ctx, result.Path, repo, branch)
	if reason != "" {
		result.Reason = "before fast-forward: " + reason
		return result, nil
	}
	if latest.Head != state.Head {
		result.Reason = "HEAD changed before fast-forward"
		return result, nil
	}
	_, err = e.command(ctx, result.Path, "git", "-c", "merge.autostash=false", "-c", "branch."+branch+".mergeOptions=", "merge", "--ff-only", "--no-autostash", "--no-edit", "--no-overwrite-ignore", "--quiet", "--", oid)
	if err != nil {
		return result, fmt.Errorf("fast-forward failed; no reset or cleanup attempted: %w", err)
	}
	result.Action = "updated"
	result.Reason = "fast-forwarded " + branch
	return result, nil
}

func (e *engine) safeState(ctx context.Context, dir string, repo remoteRepo, branch string) (repository.State, string) {
	var empty repository.State
	if err := checkPath(dir); err != nil {
		return empty, err.Error()
	}
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return empty, "destination is not a primary clone with a real .git directory"
	}
	state, err := e.inspect(ctx, dir)
	if err != nil {
		return state, "repository inspection failed: " + err.Error()
	}
	if !state.Primary || filepath.Clean(state.Path) != dir || filepath.Clean(state.GitDir) != gitDir || filepath.Clean(state.CommonDir) != gitDir {
		return state, "destination is not the exact top-level primary clone"
	}
	if len(state.Problems) > 0 {
		return state, "repository state is uncertain: " + strings.Join(state.Problems, "; ")
	}
	if state.Locked {
		return state, "repository is locked or has an operation in progress"
	}
	if state.Dirty || state.Untracked || state.Ignored {
		return state, "working tree has changed, untracked, or ignored files"
	}
	if state.Branch == "" {
		return state, "detached HEAD"
	}
	if state.Branch != branch {
		return state, "current branch is not the GitHub default branch"
	}
	if !validOID(state.Head) {
		return state, "HEAD cannot be verified"
	}
	if !sameOrigin(state.Origin, repo.FullName) {
		return state, "origin does not match the requested GitHub repository"
	}
	urls, err := e.command(ctx, dir, "git", "remote", "get-url", "--all", "origin")
	if err != nil || !sameOrigin(strings.TrimSpace(string(urls)), repo.FullName) {
		return state, "effective origin is missing, ambiguous, or does not match GitHub"
	}
	trees, err := e.command(ctx, dir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return state, "cannot verify linked worktrees"
	}
	count := 0
	for _, line := range strings.Split(string(trees), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			count++
		}
	}
	if count != 1 {
		return state, "repository has linked worktrees or uncertain worktree metadata"
	}
	return state, ""
}

func (e *engine) distance(ctx context.Context, dir, ref string) (int, int, error) {
	out, err := e.command(ctx, dir, "git", "rev-list", "--left-right", "--count", "HEAD..."+ref, "--")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(out))
	if len(parts) != 2 {
		return 0, 0, errors.New("invalid history counts")
	}
	ahead, err := strconv.Atoi(parts[0])
	if err != nil || ahead < 0 {
		return 0, 0, errors.New("invalid ahead count")
	}
	behind, err := strconv.Atoi(parts[1])
	if err != nil || behind < 0 {
		return 0, 0, errors.New("invalid behind count")
	}
	return ahead, behind, nil
}

func historyReason(ahead, behind int) string {
	if ahead > 0 && behind > 0 {
		return "local and origin histories have diverged"
	}
	return "local branch is ahead of origin"
}

func validOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func sameOrigin(origin, fullName string) bool {
	if strings.ContainsAny(origin, "\r\n\t ") {
		return false
	}
	var repositoryPath string
	if strings.HasPrefix(origin, "git@github.com:") {
		repositoryPath = strings.TrimPrefix(origin, "git@github.com:")
	} else {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
			return false
		}
		switch parsed.Scheme {
		case "https":
			if parsed.User != nil {
				return false
			}
		case "ssh":
			if parsed.User == nil || parsed.User.String() != "git" {
				return false
			}
		default:
			return false
		}
		repositoryPath = strings.TrimPrefix(parsed.Path, "/")
	}
	repositoryPath = strings.TrimSuffix(repositoryPath, ".git")
	return strings.EqualFold(repositoryPath, fullName)
}

// checkPath checks every existing path component, including broken symlinks.
func checkPath(dir string) error {
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect path %s: %w", current, err)
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return fmt.Errorf("path component %s is not a real directory", current)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func checkRoot(dir string) error {
	if err := checkPath(dir); err != nil {
		return err
	}
	for current := dir; ; current = filepath.Dir(current) {
		gitPath := filepath.Join(current, ".git")
		if info, err := os.Lstat(gitPath); err == nil {
			// An empty directory named .git is not a Git repository. Some
			// sandboxes install empty read-only sentinels at workspace roots.
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("sync target is inside an existing Git working tree: %s", current)
			}
			if _, err := os.Lstat(filepath.Join(gitPath, "HEAD")); err == nil {
				return fmt.Errorf("sync target is inside an existing Git working tree: %s", current)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect parent Git HEAD: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect parent Git metadata: %w", err)
		}
		// Bare repositories have no .git entry, but must not be used as clone roots.
		if _, err := os.Lstat(filepath.Join(current, "HEAD")); err == nil {
			if info, err := os.Lstat(filepath.Join(current, "objects")); err == nil && info.IsDir() {
				return fmt.Errorf("sync target may be inside a bare Git repository: %s", current)
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func runCommand(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
	// Executable names and every option are constructed by this package, never a shell.
	cmd := exec.CommandContext(ctx, program, args...) // #nosec G204 -- no shell; fixed git/gh executables and validated arguments.
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = commandEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2000 {
			detail = detail[:2000] + "…"
		}
		if detail != "" {
			return nil, fmt.Errorf("%s: %w: %s", program, err, detail)
		}
		return nil, fmt.Errorf("%s: %w", program, err)
	}
	return stdout.Bytes(), nil
}

func commandEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+16)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "GH_HOST" || key == "GH_PROMPT_DISABLED" || key == "GH_PAGER" || key == "GCM_INTERACTIVE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GCM_INTERACTIVE=never",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes", "GH_HOST=github.com", "GH_PROMPT_DISABLED=1", "GH_PAGER=cat",
		"GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=gc.auto", "GIT_CONFIG_VALUE_2=0",
		"GIT_CONFIG_KEY_3=credential.interactive", "GIT_CONFIG_VALUE_3=false",
		"GIT_CONFIG_KEY_4=core.fsmonitor", "GIT_CONFIG_VALUE_4=false")
}
