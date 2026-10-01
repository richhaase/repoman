// Package syncer clones and updates active GitHub repositories, with explicit
// options for forced default-branch checkout and inactive-clone cleanup.
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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repopattern"
	"github.com/richhaase/repoman/internal/repository"
)

// Target identifies a GitHub owner and the parent directory for its clones.
// Includes and Excludes match repository names using repository-name globs.
// Activity is push-based. Legacy Events values are accepted for config compatibility.
type Target struct {
	Dir          string   `json:"dir"`
	Owner        string   `json:"owner"`
	Days         int      `json:"days"`
	Includes     []string `json:"includes,omitempty"`
	Excludes     []string `json:"excludes,omitempty"`
	Events       *bool    `json:"events,omitempty"`
	CleanupLevel string   `json:"cleanup_level,omitempty"`
	FetchScope   string   `json:"fetch_scope,omitempty"`
	Prune        bool     `json:"prune,omitempty"`
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
	return RunWithOptions(ctx, target, Options{DryRun: dryRun})
}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Options controls this invocation. Empty FetchScope and nil Prune use the
// target's settings. Force and Cleanup are never saved on a target.
type Options struct {
	DryRun     bool
	Force      bool
	Cleanup    bool
	FetchScope string
	Prune      *bool
}

// RunWithOptions runs sync with fetch, force, and cleanup choices. DryRun
// previews all actions without writing the filesystem or Git metadata.
func RunWithOptions(ctx context.Context, target Target, options Options) ([]Result, error) {
	return (&engine{command: runCommand, now: time.Now}).runWithOptions(ctx, target, options)
}

func (e *engine) run(ctx context.Context, target Target, dryRun bool) ([]Result, error) {
	return e.runWithOptions(ctx, target, Options{DryRun: dryRun})
}

func (e *engine) runWithOptions(ctx context.Context, target Target, options Options) ([]Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, _, err := fetchOptions(target, options); err != nil {
		return nil, err
	}
	if target.Owner == "" {
		owner, err := e.authenticatedOwner(ctx)
		if err != nil {
			return nil, err
		}
		target.Owner = owner
	}
	if err := validateTarget(&target); err != nil {
		return nil, err
	}
	if err := checkRoot(target.Dir); err != nil {
		return nil, err
	}
	progress.Report(ctx, progress.Event{Phase: "list", Detail: "Listing GitHub repositories for " + target.Owner})
	repos, err := e.list(ctx, target.Owner)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(repos))
	var failures []error
	now := e.now().UTC()
	// GitHub JSON timestamps have four-digit, non-negative years. Saturating
	// windows older than that range avoids integer overflow for huge day counts.
	cutoff := time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)
	if target.Days <= 366*(now.Year()+1) {
		cutoff = now.AddDate(0, 0, -target.Days)
	}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return results, errors.Join(append(failures, err)...)
		}
		result := Result{Name: repo.Name, Path: filepath.Join(target.Dir, repo.Name), Action: "skipped"}
		var actionErr error
		switch {
		case !matches(target.Includes, repo.Name, true):
			result.Reason = "not included"
		case matches(target.Excludes, repo.Name, false):
			result.Reason = "excluded"
		case repo.Archived:
			result.Reason = "archived"
		case repo.PushedAt == nil || !repo.PushedAt.After(cutoff):
			result.Reason = "inactive"
			if options.Cleanup {
				result, actionErr = e.cleanupOne(ctx, target, repo, options.DryRun)
			}
		default:
			result, actionErr = e.syncOneWithOptions(ctx, target, repo, options)
		}
		if actionErr != nil {
			result.Action = "error"
			result.Reason = actionErr.Error()
			failures = append(failures, fmt.Errorf("%s: %w", repo.Name, actionErr))
		}
		results = append(results, result)
		progress.Report(ctx, progress.Event{Phase: "sync-result", Value: result})
	}
	return results, errors.Join(failures...)
}

func validateTarget(target *Target) error {
	if !ownerPattern.MatchString(target.Owner) {
		return errors.New("owner must be a GitHub user or organization login")
	}
	if target.Days < 1 {
		return errors.New("activity days must be positive")
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
		if _, err := repopattern.Match(pattern, ""); err != nil {
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
		if matched, _ := repopattern.Match(pattern, name); matched {
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
	return e.syncOneWithOptions(ctx, target, repo, Options{DryRun: dryRun})
}

func (e *engine) syncOneWithOptions(ctx context.Context, target Target, repo remoteRepo, options Options) (Result, error) {
	result := Result{Name: repo.Name, Path: filepath.Join(target.Dir, repo.Name), Action: "skipped"}
	checkout := ""
	defer func() {
		if checkout != "" {
			progress.Report(ctx, progress.Event{Phase: "checkout", Path: result.Path, Detail: checkout})
		}
	}()
	scope, prune, err := fetchOptions(target, options)
	if err != nil {
		return result, err
	}
	phase := "syncing"
	if options.DryRun {
		phase = "checking preview"
	}
	progress.Report(ctx, progress.Event{Phase: "sync-start", Path: result.Path, Detail: phase})
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkRoot(target.Dir); err != nil {
		return result, err
	}
	info, err := os.Lstat(result.Path)
	if errors.Is(err, os.ErrNotExist) {
		if options.DryRun {
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
	state, reason, err := e.primaryState(ctx, result.Path, repo)
	if err != nil || reason != "" {
		result.Reason = reason
		return result, err
	}
	branch := ""
	if repo.DefaultBranch != nil {
		branch = repo.DefaultBranch.Name
	}
	checkout = state.Branch + "\x00" + branch
	if options.DryRun {
		result.Action = "would-fetch"
		fetch := "would fetch " + fetchDescription(scope, prune)
		result.Reason = fetch + "; " + checkoutReason(state, branch)
		if len(state.Problems) > 0 {
			return result, fmt.Errorf("inspect checkout: %s", strings.Join(state.Problems, "; "))
		}
		if branch != "" && options.Force {
			result.Action = "would-force"
			result.Reason = fetch + ", then force " + branch + " to origin/" + branch + "; local changes and default-branch commits may be discarded"
		} else if checkoutReason(state, branch) == "" {
			result.Action = "would-sync"
			result.Reason = fetch + ", then fast-forward if possible; remote freshness is unknown"
		}
		return result, nil
	}
	// Fetch is independent of checkout eligibility: dirty, detached, non-default,
	// ahead and diverged clones still refresh the selected remotes. Explicit
	// --no-prune overrides both fetch.prune and remote.<name>.prune config.
	if _, err := e.command(ctx, result.Path, "git", fetchArgs(scope, prune)...); err != nil {
		return result, fmt.Errorf("fetch %s failed: %w", fetchDescription(scope, prune), err)
	}
	result.Action = "fetched"
	after, reason, err := e.primaryState(ctx, result.Path, repo)
	if err != nil || reason != "" {
		result.Reason = "after fetch: " + reason
		return result, err
	}
	if len(after.Problems) > 0 {
		return result, fmt.Errorf("inspect checkout after fetch: %s", strings.Join(after.Problems, "; "))
	}
	checkout = after.Branch + "\x00" + branch
	if !options.Force {
		if reason := checkoutReason(after, branch); reason != "" {
			result.Reason = reason + "; fetched only"
			return result, nil
		}
		if after.Head != state.Head || after.Branch != state.Branch {
			result.Reason = "HEAD changed while fetching; fetched only"
			return result, nil
		}
	}
	if branch == "" {
		result.Reason = "no default branch; fetched only"
		return result, nil
	}
	if branch == "HEAD" {
		return result, errors.New("unsafe default branch")
	}
	if _, err := e.command(ctx, "", "git", "check-ref-format", "refs/heads/"+branch); err != nil {
		return result, fmt.Errorf("invalid or unverifiable default branch: %w", err)
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := e.command(ctx, result.Path, "git", "show-ref", "--verify", "--quiet", ref); err != nil {
		if commandExitOne(err) {
			result.Reason = "no origin/" + branch + "; fetched only"
			return result, nil
		}
		return result, fmt.Errorf("check fetched default branch: %w", err)
	}
	commit, err := e.command(ctx, result.Path, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return result, fmt.Errorf("resolve fetched default branch: %w", err)
	}
	oid := strings.TrimSpace(string(commit))
	if !validOID(oid) {
		return result, errors.New("fetched commit has an invalid object ID")
	}
	if options.Force {
		if after.Head == oid && after.Branch == branch && !after.Dirty && !after.Untracked {
			result.Action = "up-to-date"
			return result, nil
		}
		if _, err := e.command(ctx, result.Path, "git", "checkout", "-q", "-f", "-B", branch, ref); err != nil {
			return result, fmt.Errorf("force checkout failed: %w", err)
		}
		result.Action = "forced"
		result.Reason = "checked out " + branch + " at origin/" + branch
		return result, nil
	}
	ahead, behind, err := e.distance(ctx, result.Path, oid)
	if err != nil {
		return result, fmt.Errorf("compare fetched default branch: %w", err)
	}
	if ahead > 0 {
		result.Reason = historyReason(ahead, behind) + "; fetched only"
		return result, nil
	}
	if behind == 0 {
		result.Action = "up-to-date"
		return result, nil
	}
	latest, reason, err := e.primaryState(ctx, result.Path, repo)
	if err != nil || reason != "" {
		result.Reason = "before fast-forward: " + reason
		return result, err
	}
	if len(latest.Problems) > 0 {
		return result, fmt.Errorf("inspect checkout before fast-forward: %s", strings.Join(latest.Problems, "; "))
	}
	if reason := checkoutReason(latest, branch); reason != "" {
		result.Reason = reason + "; fetched only"
		return result, nil
	}
	if latest.Head != after.Head {
		result.Reason = "HEAD changed before fast-forward; fetched only"
		return result, nil
	}
	_, err = e.command(ctx, result.Path, "git", "-c", "merge.autostash=false", "-c", "branch."+branch+".mergeOptions=", "merge", "--ff-only", "--no-autostash", "--no-edit", "--quiet", "--", oid)
	if err != nil {
		return result, fmt.Errorf("fast-forward failed: %w", err)
	}
	result.Action = "updated"
	result.Reason = "fast-forwarded " + branch
	return result, nil
}

func checkoutReason(state repository.State, branch string) string {
	if state.Dirty || state.Untracked {
		return "working tree has uncommitted or untracked changes"
	}
	if branch == "" {
		return "no default branch"
	}
	if state.Branch == "" {
		return "detached HEAD"
	}
	if state.Branch != branch {
		return "current branch is not the GitHub default branch"
	}
	if !validOID(state.Head) {
		return "HEAD cannot be verified"
	}
	return ""
}

// primaryState establishes identity, not checkout eligibility. General inventory
// and worktree-cleanup policy do not decide whether a clone can fetch or sync.
func (e *engine) primaryState(ctx context.Context, dir string, repo remoteRepo) (repository.State, string, error) {
	var empty repository.State
	if err := checkPath(dir); err != nil {
		return empty, "", err
	}
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitDir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0)) {
		return empty, "destination is not a primary clone with a real .git directory", nil
	}
	if err != nil {
		return empty, "", fmt.Errorf("inspect Git directory: %w", err)
	}
	inspect := e.inspect
	if inspect == nil {
		inspect = e.inspectClone
	}
	state, err := inspect(ctx, dir)
	if err != nil {
		return state, "", fmt.Errorf("repository inspection failed: %w", err)
	}
	if !state.Primary || filepath.Clean(state.Path) != dir || filepath.Clean(state.GitDir) != gitDir || filepath.Clean(state.CommonDir) != gitDir {
		return state, "destination is not the exact top-level primary clone", nil
	}
	if !sameOrigin(state.Origin, repo.FullName) {
		return state, "origin does not match the requested GitHub repository", nil
	}
	urls, err := e.command(ctx, dir, "git", "remote", "get-url", "--all", "origin")
	if err != nil {
		return state, "", fmt.Errorf("read effective origin: %w", err)
	}
	if !sameOrigin(strings.TrimSpace(string(urls)), repo.FullName) {
		return state, "effective origin is ambiguous or does not match GitHub", nil
	}
	return state, "", nil
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
		case "https", "http":
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
