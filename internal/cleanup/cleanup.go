// Package cleanup builds conservative worktree cleanup plans and applies them
// only after revalidating the complete inventory. It never removes branches or
// primary clones, prunes registrations, or uses git's force option.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/richhaase/repoman/internal/repository"
)

const (
	ActionKeep        = "keep"
	ActionWouldRemove = "would-remove"
	ActionRemoved     = "removed"
	ActionFailed      = "failed"
)

// Result describes what happened to a worktree, or what a preview would do.
type Result struct {
	Path   string           `json:"path"`
	Action string           `json:"action"`
	Reason string           `json:"reason"`
	State  repository.State `json:"state"`
}

type evidence struct {
	eligible bool
	reason   string
}

type engine struct {
	discover func(context.Context, string) ([]repository.State, error)
	inspect  func(context.Context, string) repository.State
	lookup   func(context.Context, repository.State) (evidence, error)
	remove   func(context.Context, repository.State) error
	includes []string
	excludes []string
}

// Run previews cleanup unless apply is explicitly true. A lookup or preflight
// failure prevents all removals. A subsequent local change or removal failure
// stops immediately and returns results recording any removals already made.
func Run(ctx context.Context, root string, apply bool) ([]Result, error) {
	return RunFiltered(ctx, root, apply, nil, nil)
}

// RunFiltered applies selection patterns to primary clone basenames. Excluded
// repositories and all their linked worktrees remain protected.
func RunFiltered(ctx context.Context, root string, apply bool, includes, excludes []string) ([]Result, error) {
	return engine{
		discover: repository.Discover,
		inspect:  repository.Inspect,
		lookup:   githubEvidence,
		remove:   removeWorktree,
		includes: includes,
		excludes: excludes,
	}.run(ctx, root, apply)
}

func (e engine) run(ctx context.Context, root string, apply bool) ([]Result, error) {
	for _, pattern := range append(append([]string(nil), e.includes...), e.excludes...) {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid repository selection pattern %q: %w", pattern, err)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve cleanup root: %w", err)
	}
	root = filepath.Clean(root)
	rootIdentity, err := captureIdentity(root)
	if err != nil {
		return nil, fmt.Errorf("inspect cleanup root: %w", err)
	}
	states, err := e.discover(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("discover cleanup inventory: %w", err)
	}
	states = normalizeStates(states)
	results := make([]Result, 0, len(states))
	identities := make(map[string][]pathIdentity)
	var lookupErrors []error
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return blockPending(results, "cleanup canceled"), err
		}
		result := Result{Path: state.Path, Action: ActionKeep, State: state}
		result.Reason = selectionProtection(states, state, e.includes, e.excludes)
		if result.Reason == "" {
			result.Reason = localProtection(root, state)
		}
		if result.Reason == "" {
			result.Reason = sharedProtection(states, state)
		}
		if result.Reason != "" {
			results = append(results, result)
			continue
		}
		identity, identityErr := captureIdentity(state.Path)
		if identityErr != nil {
			result.Reason = "cannot establish path identity"
			lookupErrors = append(lookupErrors, fmt.Errorf("inspect %q: %w", state.Path, identityErr))
			results = append(results, result)
			continue
		}
		if !within(rootIdentity.resolved, identity.resolved) || identity.resolved == rootIdentity.resolved {
			result.Reason = "worktree resolves outside cleanup root or is the root"
			results = append(results, result)
			continue
		}
		commonIdentity, commonErr := captureIdentity(state.CommonDir)
		gitIdentity, gitErr := captureIdentity(state.GitDir)
		if identityErr = errors.Join(commonErr, gitErr); identityErr != nil {
			result.Reason = "cannot establish Git directory identity"
			lookupErrors = append(lookupErrors, fmt.Errorf("inspect Git directories for %q: %w", state.Path, identityErr))
			results = append(results, result)
			continue
		}
		proof, lookupErr := e.lookup(ctx, state)
		if lookupErr != nil {
			result.Reason = "GitHub PR lookup failed; cleanup blocked"
			lookupErrors = append(lookupErrors, fmt.Errorf("verify PRs for %q: %w", state.Path, lookupErr))
		} else {
			result.Reason = proof.reason
			if proof.eligible {
				result.Action = ActionWouldRemove
				identities[state.Path] = []pathIdentity{identity, commonIdentity, gitIdentity}
			}
		}
		results = append(results, result)
	}
	if err := errors.Join(lookupErrors...); err != nil {
		return blockPending(results, "cleanup blocked by a failed safety check"), err
	}
	if !apply || len(identities) == 0 {
		return results, nil
	}

	// Finish every remote lookup before the first mutation, so any unavailable
	// GitHub evidence blocks the entire operation rather than a later subset.
	for _, result := range results {
		if result.Action != ActionWouldRemove {
			continue
		}
		proof, lookupErr := e.lookup(ctx, result.State)
		if lookupErr != nil {
			return blockPending(results, "cleanup blocked during PR revalidation"), fmt.Errorf("revalidate PRs for %q: %w", result.Path, lookupErr)
		}
		if !proof.eligible {
			return blockPending(results, "PR eligibility changed; rerun preview"), fmt.Errorf("PR eligibility changed for %q: %s", result.Path, proof.reason)
		}
	}
	if err := rootIdentity.validate(); err != nil {
		return blockPending(results, "cleanup root identity changed"), err
	}
	fresh, err := e.discover(ctx, root)
	if err != nil {
		return blockPending(results, "inventory revalidation failed"), fmt.Errorf("revalidate inventory: %w", err)
	}
	if !reflect.DeepEqual(states, normalizeStates(fresh)) {
		return blockPending(results, "inventory changed; rerun preview"), errors.New("cleanup inventory changed during planning; no worktrees removed")
	}
	for _, result := range results {
		if err := validateIdentities(identities[result.Path]); err != nil {
			return blockPending(results, "worktree path identity changed"), fmt.Errorf("revalidate %q: %w", result.Path, err)
		}
	}
	for i := range results {
		if results[i].Action != ActionWouldRemove {
			continue
		}
		if err := ctx.Err(); err != nil {
			return blockPending(results, "cleanup canceled"), err
		}
		if err := rootIdentity.validate(); err != nil {
			return blockPending(results, "cleanup root identity changed"), err
		}
		if err := validateIdentities(identities[results[i].Path]); err != nil {
			return blockPending(results, "worktree path identity changed"), err
		}
		freshState := normalizeState(e.inspect(ctx, results[i].Path))
		if !reflect.DeepEqual(results[i].State, freshState) || localProtection(root, freshState) != "" {
			return blockPending(results, "worktree changed; rerun preview"), fmt.Errorf("worktree %q changed immediately before removal", results[i].Path)
		}
		if err := validateIdentities(identities[results[i].Path]); err != nil {
			return blockPending(results, "worktree path identity changed"), err
		}
		if err := e.remove(ctx, freshState); err != nil {
			results[i].Action = ActionFailed
			results[i].Reason = "git removal failed; inspect worktree before retrying"
			return blockPending(results, "cleanup stopped after git refused removal"), fmt.Errorf("remove worktree %q: %w", results[i].Path, err)
		}
		results[i].Action = ActionRemoved
		results[i].Reason = "removed clean linked worktree with verified terminal PR"
	}
	return results, nil
}

func blockPending(results []Result, reason string) []Result {
	for i := range results {
		if results[i].Action == ActionWouldRemove {
			results[i].Action = ActionKeep
			results[i].Reason = reason
		}
	}
	return results
}

func selectionProtection(states []repository.State, state repository.State, includes, excludes []string) string {
	if len(includes) == 0 && len(excludes) == 0 {
		return ""
	}
	name := ""
	for _, primary := range states {
		if primary.Primary && primary.CommonDir == state.CommonDir {
			if name != "" {
				return "primary clone selection is ambiguous"
			}
			name = filepath.Base(primary.Path)
		}
	}
	if name == "" {
		return "primary clone could not be identified for selection"
	}
	for _, pattern := range excludes {
		if match, _ := filepath.Match(pattern, name); match {
			return "repository excluded by configured selection"
		}
	}
	if len(includes) != 0 {
		for _, pattern := range includes {
			if match, _ := filepath.Match(pattern, name); match {
				return ""
			}
		}
		return "repository does not match configured selection"
	}
	return ""
}

func sharedProtection(states []repository.State, state repository.State) string {
	for _, related := range states {
		if related.CommonDir != state.CommonDir {
			continue
		}
		if related.InUse {
			return "a worktree sharing this repository is in use"
		}
		if !related.InUseKnown || len(related.SafetyProblems) != 0 || len(related.Problems) != 0 {
			return "shared repository safety inspection is incomplete"
		}
	}
	return ""
}

func validateIdentities(identities []pathIdentity) error {
	for _, identity := range identities {
		if err := identity.validate(); err != nil {
			return err
		}
	}
	return nil
}

func localProtection(root string, state repository.State) string {
	switch {
	case state.Primary:
		return "primary clone is protected"
	case !filepath.IsAbs(state.Path), !within(root, state.Path), filepath.Clean(state.Path) == root:
		return "worktree is outside cleanup root or is the root"
	case len(state.Problems) != 0:
		return "repository inspection is incomplete"
	case state.Head == "" || state.CommonDir == "" || state.GitDir == "":
		return "repository identity is incomplete"
	case state.Locked:
		return "worktree is locked"
	case state.Dirty:
		return "worktree has modified or staged files"
	case state.Untracked:
		return "worktree has untracked files"
	case state.Ignored:
		return "worktree has ignored files"
	case state.Stash:
		return "repository has stashed work"
	case state.Unique || state.Ahead > 0:
		return "repository has local-only commits"
	case state.InUse:
		return "worktree is in use"
	case !state.InUseKnown || len(state.SafetyProblems) != 0:
		return "worktree usage could not be established safely"
	default:
		return ""
	}
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func normalizeStates(states []repository.State) []repository.State {
	copyStates := make([]repository.State, len(states))
	for i := range states {
		copyStates[i] = normalizeState(states[i])
	}
	sort.Slice(copyStates, func(i, j int) bool { return copyStates[i].Path < copyStates[j].Path })
	return copyStates
}

func normalizeState(state repository.State) repository.State {
	state.Problems = append([]string(nil), state.Problems...)
	state.SafetyProblems = append([]string(nil), state.SafetyProblems...)
	sort.Strings(state.Problems)
	sort.Strings(state.SafetyProblems)
	return state
}

type pathIdentity struct {
	path     string
	resolved string
	parts    []pathPart
}

type pathPart struct {
	path string
	info os.FileInfo
}

// Snapshot every ancestor as well as the directory. Replacing a symlink,
// directory, mount path, or ancestor makes this identity invalid.
func captureIdentity(path string) (pathIdentity, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return pathIdentity{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return pathIdentity{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return pathIdentity{}, err
	}
	if !info.IsDir() {
		return pathIdentity{}, fmt.Errorf("%q is not a directory", absolute)
	}
	identity := pathIdentity{path: absolute, resolved: resolved}
	for part := absolute; ; part = filepath.Dir(part) {
		info, err := os.Lstat(part)
		if err != nil {
			return pathIdentity{}, err
		}
		identity.parts = append(identity.parts, pathPart{path: part, info: info})
		if part == filepath.Dir(part) {
			break
		}
	}
	return identity, nil
}

func (identity pathIdentity) validate() error {
	current, err := captureIdentity(identity.path)
	if err != nil {
		return fmt.Errorf("path identity changed for %q: %w", identity.path, err)
	}
	if current.resolved != identity.resolved || len(current.parts) != len(identity.parts) {
		return fmt.Errorf("path identity changed for %q", identity.path)
	}
	for i := range identity.parts {
		if !os.SameFile(identity.parts[i].info, current.parts[i].info) || identity.parts[i].info.Mode() != current.parts[i].info.Mode() {
			return fmt.Errorf("path identity changed for %q", identity.path)
		}
	}
	return nil
}

func removeWorktree(ctx context.Context, state repository.State) error {
	// Git remains the final safety gate. Never add --force or a shell fallback.
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath="+os.DevNull, "--git-dir="+state.CommonDir, "worktree", "remove", "--", state.Path) // #nosec G204 -- fixed executable and arguments, without a shell.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
