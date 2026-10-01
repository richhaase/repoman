// Package cleanup plans every selected repository before removing linked
// worktrees. The default policy follows clean-repos: preserve primary checkouts,
// explicit locks, current-user process working directories, and open GitHub PRs.
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

	"github.com/richhaase/repoman/internal/repopattern"
	"github.com/richhaase/repoman/internal/repository"
)

const (
	ActionKeep        = "keep"
	ActionWouldRemove = "would-remove"
	ActionRemoved     = "removed"
	ActionWouldPrune  = "would-prune"
	ActionPruned      = "pruned"
	ActionFailed      = "failed"
)

type Result struct {
	Path        string           `json:"path"`
	Action      string           `json:"action"`
	Reason      string           `json:"reason"`
	Level       Level            `json:"level"`
	Destructive bool             `json:"destructive"`
	State       repository.State `json:"state"`
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
	prune    func(context.Context, repository.State) error
	includes []string
	excludes []string
	options  Options
}

// Target describes one configured selection in an all-or-nothing preflight.
type Target struct {
	Root     string
	Includes []string
	Excludes []string
	Options  Options
}

func Run(ctx context.Context, root string, apply bool) ([]Result, error) {
	return RunFiltered(ctx, root, apply, nil, nil)
}
func RunFiltered(ctx context.Context, root string, apply bool, includes, excludes []string) ([]Result, error) {
	return RunWithOptions(ctx, root, apply, includes, excludes, Options{})
}
func RunWithOptions(ctx context.Context, root string, apply bool, includes, excludes []string, options Options) ([]Result, error) {
	return RunBatch(ctx, []Target{{Root: root, Includes: includes, Excludes: excludes, Options: options}}, apply)
}

// RunBatch completes inventory and PR preflight for ALL targets before mutation.
// A later removal failure stops the batch and retains truthful partial results.
func RunBatch(ctx context.Context, targets []Target, apply bool) ([]Result, error) {
	engines := make([]engine, len(targets))
	roots := make([]string, len(targets))
	for i, target := range targets {
		options, err := target.Options.validate(apply)
		if err != nil {
			return nil, err
		}
		engines[i] = engine{
			discover: repository.Discover, inspect: repository.Inspect,
			lookup: func(ctx context.Context, s repository.State) (evidence, error) {
				return lookupEvidenceForLevel(ctx, s, readPullRequests, options.Level)
			},
			remove:   func(ctx context.Context, s repository.State) error { return removeWorktreeWithOptions(ctx, s, options) },
			prune:    pruneWorktrees,
			includes: target.Includes, excludes: target.Excludes, options: options,
		}
		roots[i] = target.Root
	}
	return runEngines(ctx, engines, roots, apply)
}

func (e engine) run(ctx context.Context, root string, apply bool) ([]Result, error) {
	return runEngines(ctx, []engine{e}, []string{root}, apply)
}

type cleanupPlan struct {
	engine     engine
	root       pathIdentity
	states     []repository.State
	results    []Result
	identities map[string][]pathIdentity
}

func runEngines(ctx context.Context, engines []engine, roots []string, apply bool) ([]Result, error) {
	plans := make([]*cleanupPlan, 0, len(engines))
	var failures []error
	for i, e := range engines {
		p, err := e.plan(ctx, roots[i], apply)
		if p != nil {
			plans = append(plans, p)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", roots[i], err))
		}
	}
	collect := func(block string) []Result {
		results := []Result{}
		for _, p := range plans {
			if block != "" {
				p.results = blockPending(p.results, block)
			}
			results = append(results, p.results...)
		}
		return results
	}
	if err := errors.Join(failures...); err != nil {
		return collect("cleanup blocked by a failed preflight"), err
	}
	// Overlapping target roots can discover the same registration. Retain one
	// result and honor the stricter decision if any selected target protects it.
	type location struct {
		plan  *cleanupPlan
		index int
	}
	seen := map[string]location{}
	for _, p := range plans {
		filtered := make([]Result, 0, len(p.results))
		for _, r := range p.results {
			if previous, ok := seen[r.Path]; ok {
				prior := &previous.plan.results[previous.index]
				if r.Level != prior.Level {
					return collect("overlapping targets have conflicting cleanup levels"), fmt.Errorf("overlapping targets select %q with conflicting cleanup levels %s and %s", r.Path, prior.Level, r.Level)
				}
				if r.Action == ActionKeep {
					*prior = r
				}
				continue
			}
			filtered = append(filtered, r)
		}
		p.results = filtered
		for i, r := range p.results {
			seen[r.Path] = location{p, i}
		}
	}
	pending := false
	for _, p := range plans {
		for _, r := range p.results {
			if r.Action == ActionWouldRemove || r.Action == ActionWouldPrune {
				pending = true
			}
		}
	}
	// Planning already inspected every selected target and checked required
	// evidence. A wholly retained batch needs no mutation-time revalidation.
	if !apply || !pending {
		return collect(""), nil
	}
	for _, p := range plans {
		if err := p.preflight(ctx); err != nil {
			return collect("cleanup blocked during revalidation"), err
		}
	}
	for _, p := range plans {
		if err := p.apply(ctx); err != nil {
			return collect("cleanup stopped before remaining actions"), err
		}
	}
	return collect(""), nil
}

func (e engine) plan(ctx context.Context, root string, apply bool) (*cleanupPlan, error) {
	options, err := e.options.validate(apply)
	if err != nil {
		return nil, err
	}
	e.options = options
	for _, pattern := range append(append([]string(nil), e.includes...), e.excludes...) {
		if _, err := repopattern.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid repository selection pattern %q: %w", pattern, err)
		}
	}
	identity, err := captureIdentity(root)
	if err != nil {
		return nil, fmt.Errorf("inspect cleanup root: %w", err)
	}
	states, err := e.discover(ctx, identity.resolved)
	if err != nil {
		return nil, fmt.Errorf("discover cleanup inventory: %w", err)
	}
	p := &cleanupPlan{engine: e, root: identity, states: normalizeStates(states), identities: map[string][]pathIdentity{}}
	var failures []error
	for _, s := range p.states {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		r := Result{Path: s.Path, Action: ActionKeep, Level: options.Level, State: s}
		r.Reason = selectionProtection(p.states, s, e.includes, e.excludes)
		if r.Reason == "" {
			if issue := inspectionFailure(s, options.Level); issue != "" {
				r.Reason = issue
				failures = append(failures, fmt.Errorf("inspect %q: %s", s.Path, issue))
			} else {
				r.Reason = localProtectionForLevel(identity.resolved, s, options.Level)
			}
		}
		if r.Reason == "" && options.Level != Aggressive {
			r.Reason = sharedProtection(p.states, s)
		}
		if r.Reason != "" {
			p.results = append(p.results, r)
			continue
		}
		ids, err := captureStateIdentities(s)
		if err != nil {
			r.Reason = "cannot establish worktree and Git directory identity"
			failures = append(failures, fmt.Errorf("inspect %q: %w", s.Path, err))
			p.results = append(p.results, r)
			continue
		}
		if s.Missing && s.Prunable {
			r.Action = ActionWouldPrune
			r.Reason = "would prune stale metadata using Git's default expiry; recent registrations may remain"
			p.identities[s.Path] = ids
			p.results = append(p.results, r)
			continue
		}
		proof, lookupErr := e.lookup(ctx, s)
		if lookupErr != nil {
			r.Reason = "GitHub PR lookup failed; cleanup blocked"
			failures = append(failures, fmt.Errorf("verify PRs for %q: %w", s.Path, lookupErr))
		} else {
			r.Reason = proof.reason
			if proof.eligible {
				r.Action = ActionWouldRemove
				r.Destructive = options.Level == Aggressive
				if r.Destructive {
					r.Reason += "; force-removes checkout files, including local changes; branch refs are retained"
				}
				p.identities[s.Path] = ids
			}
		}
		p.results = append(p.results, r)
	}
	// Git's prune command operates on the whole common store. A protected
	// stale registration must therefore defer every prune in that store.
	protectedStale := map[string]string{}
	for _, r := range p.results {
		if r.Action == ActionKeep && r.State.Missing && r.State.Prunable {
			protectedStale[r.State.CommonDir] = r.Path
		}
	}
	for i := range p.results {
		r := &p.results[i]
		if path, ok := protectedStale[r.State.CommonDir]; ok && r.Action == ActionWouldPrune {
			r.Action = ActionKeep
			r.Reason = fmt.Sprintf("metadata pruning deferred because stale worktree %q is protected", path)
		}
	}
	return p, errors.Join(failures...)
}

func inspectionFailure(s repository.State, level Level) string {
	if len(s.IdentityProblems) > 0 {
		return "repository identity inspection failed: " + strings.Join(s.IdentityProblems, "; ")
	}
	if level == Aggressive {
		if !s.CwdInUseKnown || len(s.CwdProblems) > 0 {
			return "current-user working directories could not be observed: " + strings.Join(s.CwdProblems, "; ")
		}
	} else if !s.InUseKnown || len(s.SafetyProblems) > 0 {
		return "current-user process usage could not be observed: " + strings.Join(s.SafetyProblems, "; ")
	}
	return ""
}

func captureStateIdentities(s repository.State) ([]pathIdentity, error) {
	paths := []string{s.CommonDir, s.GitDir}
	if !s.Missing {
		paths = append(paths, s.Path)
	}
	ids := make([]pathIdentity, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("missing identity path")
		}
		id, err := captureIdentity(path)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (p *cleanupPlan) preflight(ctx context.Context) error {
	for _, r := range p.results {
		if r.Action != ActionWouldRemove {
			continue
		}
		proof, err := p.engine.lookup(ctx, r.State)
		if err != nil {
			return fmt.Errorf("revalidate PRs for %q: %w", r.Path, err)
		}
		if !proof.eligible {
			return fmt.Errorf("PR eligibility changed for %q: %s", r.Path, proof.reason)
		}
	}
	if err := p.root.validate(); err != nil {
		return err
	}
	fresh, err := p.engine.discover(ctx, p.root.resolved)
	if err != nil {
		return fmt.Errorf("revalidate inventory: %w", err)
	}
	fresh = normalizeStates(fresh)
	if !sameInventory(p.states, fresh, p.engine.options.Level) {
		return errors.New("cleanup inventory changed during planning; no worktrees removed")
	}
	for _, r := range p.results {
		if err := validateIdentities(p.identities[r.Path]); err != nil {
			return fmt.Errorf("revalidate %q: %w", r.Path, err)
		}
	}
	return nil
}

// Aggressive policy deliberately does not compare optional content diagnostics.
// Identity, registration, PR refs, explicit locks and cwd observation still must match.
func comparableState(s repository.State, level Level) repository.State {
	if level != Aggressive {
		return s
	}
	return repository.State{Path: s.Path, CommonDir: s.CommonDir, GitDir: s.GitDir, Head: s.Head, Branch: s.Branch, Origin: s.Origin,
		Primary: s.Primary, Bare: s.Bare, WorktreeLocked: s.WorktreeLocked, Prunable: s.Prunable, Missing: s.Missing,
		CwdInUse: s.CwdInUse, CwdInUseKnown: s.CwdInUseKnown, CwdProblems: s.CwdProblems,
		IdentityProblems: s.IdentityProblems, PRNumbers: s.PRNumbers}
}
func sameInventory(a, b []repository.State, level Level) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(comparableState(a[i], level), comparableState(b[i], level)) {
			return false
		}
	}
	return true
}

func (p *cleanupPlan) apply(ctx context.Context) error {
	for i := range p.results {
		r := &p.results[i]
		if r.Action != ActionWouldRemove && r.Action != ActionWouldPrune {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.root.validate(); err != nil {
			return err
		}
		if err := validateIdentities(p.identities[r.Path]); err != nil {
			return err
		}
		if r.Action == ActionWouldPrune {
			if err := p.applyPrune(ctx, r.State.CommonDir); err != nil {
				return err
			}
			continue
		}
		fresh := normalizeState(p.engine.inspect(ctx, r.Path))
		if !reflect.DeepEqual(comparableState(r.State, p.engine.options.Level), comparableState(fresh, p.engine.options.Level)) || localProtectionForLevel(p.root.resolved, fresh, p.engine.options.Level) != "" || inspectionFailure(fresh, p.engine.options.Level) != "" {
			return fmt.Errorf("worktree %q changed immediately before removal", r.Path)
		}
		if err := validateIdentities(p.identities[r.Path]); err != nil {
			return err
		}
		r.State = fresh
		if err := p.engine.remove(ctx, fresh); err != nil {
			r.Action = ActionFailed
			r.Reason = "git removal failed; inspect worktree before retrying"
			return fmt.Errorf("remove worktree %q: %w", r.Path, err)
		}
		r.Action = ActionRemoved
		if r.Destructive {
			r.Reason = "force-removed linked checkout and its local files; branch refs retained"
		} else {
			r.Reason = "removed clean linked worktree after verified policy checks; branch refs retained"
		}
	}
	return nil
}

func (p *cleanupPlan) applyPrune(ctx context.Context, common string) error {
	fresh, err := p.engine.discover(ctx, p.root.resolved)
	if err != nil {
		return fmt.Errorf("revalidate stale metadata: %w", err)
	}
	current := map[string]repository.State{}
	for _, s := range fresh {
		current[s.Path] = normalizeState(s)
		if s.CommonDir == common && s.Missing && s.Prunable {
			if reason := localProtectionForLevel(p.root.resolved, s, p.engine.options.Level); reason != "" {
				return fmt.Errorf("metadata pruning blocked by protected stale worktree %q: %s", s.Path, reason)
			}
			if reason := inspectionFailure(s, p.engine.options.Level); reason != "" {
				return fmt.Errorf("revalidate stale worktree %q: %s", s.Path, reason)
			}
		}
	}
	var candidate repository.State
	for _, r := range p.results {
		if r.Action != ActionWouldPrune || r.State.CommonDir != common {
			continue
		}
		s, ok := current[r.Path]
		if !ok || !reflect.DeepEqual(comparableState(r.State, p.engine.options.Level), comparableState(s, p.engine.options.Level)) || localProtectionForLevel(p.root.resolved, s, p.engine.options.Level) != "" {
			return fmt.Errorf("stale registration %q changed before pruning", r.Path)
		}
		if _, err := os.Lstat(r.Path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stale checkout %q is no longer absent", r.Path)
		}
		if err := validateIdentities(p.identities[r.Path]); err != nil {
			return err
		}
		candidate = s
	}
	if p.engine.prune == nil {
		return errors.New("metadata pruning is unavailable")
	}
	pruneErr := p.engine.prune(ctx, candidate)
	indices := []int{}
	for i := range p.results {
		r := &p.results[i]
		if r.Action == ActionWouldPrune && r.State.CommonDir == common {
			indices = append(indices, i)
			r.Action = ActionFailed
			r.Reason = "metadata pruning attempted; result could not be verified"
		}
	}
	fresh, err = p.engine.discover(ctx, p.root.resolved)
	if err != nil {
		return errors.Join(pruneErr, fmt.Errorf("verify metadata pruning: %w", err))
	}
	remaining := map[string]bool{}
	for _, s := range fresh {
		remaining[s.Path] = true
	}
	for _, i := range indices {
		r := &p.results[i]
		switch {
		case !remaining[r.Path]:
			r.Action = ActionPruned
			r.Reason = "pruned stale worktree registration; no checkout files or branches deleted"
		case pruneErr != nil:
			r.Action = ActionFailed
			r.Reason = "Git metadata pruning failed; registration remains"
		default:
			r.Action = ActionKeep
			r.Reason = "Git retained stale registration under its default prune expiry"
		}
	}
	if pruneErr != nil {
		return fmt.Errorf("prune worktree metadata: %w", pruneErr)
	}
	return nil
}

func blockPending(results []Result, reason string) []Result {
	for i := range results {
		if results[i].Action == ActionWouldRemove || results[i].Action == ActionWouldPrune {
			results[i].Action = ActionKeep
			results[i].Destructive = false
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
		if match, _ := repopattern.Match(pattern, name); match {
			return "repository excluded by configured selection"
		}
	}
	if len(includes) != 0 {
		for _, pattern := range includes {
			if match, _ := repopattern.Match(pattern, name); match {
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

func localProtectionForLevel(root string, state repository.State, level Level) string {
	switch {
	case state.Primary:
		return "primary clone is protected"
	case !filepath.IsAbs(state.Path), filepath.Clean(state.Path) == root:
		return "worktree path is not a distinct absolute checkout"
	case level != Aggressive && !within(root, state.Path):
		return "strict policy protects worktrees outside cleanup root"
	case len(state.IdentityProblems) > 0:
		return "repository identity inspection is incomplete"
	case state.CommonDir == "" || state.GitDir == "" || (!state.Missing && state.Head == ""):
		return "repository identity is incomplete"
	case state.WorktreeLocked || (level != Aggressive && state.Locked):
		return "worktree is locked"
	case state.Missing && !state.Prunable:
		return "missing worktree is not prunable"
	case level == Aggressive && state.CwdInUse:
		return "worktree is a current-user process working directory"
	case level == Aggressive && (!state.CwdInUseKnown || len(state.CwdProblems) > 0):
		return "working directory usage could not be established safely"
	case level != Aggressive && len(state.Problems) > 0:
		return "repository inspection is incomplete"
	case level != Aggressive && state.Dirty:
		return "worktree has modified or staged files"
	case level != Aggressive && state.Untracked:
		return "worktree has untracked files"
	case level != Aggressive && state.Ignored:
		return "worktree has ignored files"
	case level != Aggressive && state.Stash:
		return "repository has stashed work"
	case level != Aggressive && (state.Unique || state.Ahead > 0):
		return "repository has local-only commits"
	case level != Aggressive && state.InUse:
		return "worktree is in use"
	case level != Aggressive && (!state.InUseKnown || len(state.SafetyProblems) > 0):
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
	state.IdentityProblems = append([]string(nil), state.IdentityProblems...)
	state.CwdProblems = append([]string(nil), state.CwdProblems...)
	state.PRNumbers = append([]int(nil), state.PRNumbers...)
	sort.Strings(state.IdentityProblems)
	sort.Strings(state.CwdProblems)
	sort.Ints(state.PRNumbers)
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
	return removeWorktreeWithOptions(ctx, state, Options{Level: Conservative})
}

func removeWorktreeWithOptions(ctx context.Context, state repository.State, options Options) error {
	options, err := options.validate(true)
	if err != nil {
		return err
	}
	// Aggressive always uses one force. Never double-force locks or use a shell fallback.
	args := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "--git-dir=" + state.CommonDir, "worktree", "remove"}
	if options.Level == Aggressive {
		args = append(args, "--force")
	}
	args = append(args, "--", state.Path)
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed executable, validated options and paths; no shell.
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

func pruneWorktrees(ctx context.Context, state repository.State) error {
	args := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "--git-dir=" + state.CommonDir, "worktree", "prune"}
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed executable, validated Git directory, no shell.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree prune: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
