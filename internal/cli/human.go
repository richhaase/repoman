package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/richhaase/repoman/internal/cleanup"
	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repository"
	"github.com/richhaase/repoman/internal/syncer"
)

// Human output is intentionally plain, line-oriented text in terminals and
// pipes. No color or cursor control is needed to understand an outcome.
type humanReport struct {
	cmd           *cobra.Command
	verbose       bool
	roots         []string
	seen          map[string]string
	warnings      map[string]bool
	primary       map[string]string
	group         string
	checkout      map[string]string
	openPR        map[string]string
	syncFailures  map[string]bool
	handledErrors map[string]bool
}

func newHumanReport(cmd *cobra.Command, json bool) *humanReport {
	if json {
		return nil
	}
	verbose, _ := cmd.Flags().GetBool("verbose")
	return &humanReport{cmd: cmd, verbose: verbose, seen: map[string]string{}, warnings: map[string]bool{}, primary: map[string]string{}, checkout: map[string]string{}, openPR: map[string]string{}, syncFailures: map[string]bool{}, handledErrors: map[string]bool{}}
}

// Keep paths, branch names and diagnostics on one inert terminal line. JSON
// still contains the exact original values for machine consumers.
func humanText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			b.WriteString(strings.Trim(strconv.QuoteRune(r), "'"))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (h *humanReport) line(format string, args ...any) {
	if h != nil {
		fmt.Fprintf(h.cmd.OutOrStdout(), format+"\n", args...)
	}
}

func (h *humanReport) warning(s string) {
	if h == nil || h.warnings[s] {
		return
	}
	h.warnings[s] = true
	fmt.Fprintf(h.cmd.ErrOrStderr(), "warning: %s\n", humanText(s))
}

func (h *humanReport) target(root, operation string, adHoc bool) {
	if h == nil {
		return
	}
	if len(h.roots) > 0 {
		h.line("")
	}
	h.roots = append(h.roots, root)
	h.line("=== %s ===", humanText(root))
	selection := "configured target"
	if adHoc {
		selection = "ad hoc target"
	}
	h.line("%s · %s", operation, selection)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func (h *humanReport) syncHeader(t syncer.Target, policy syncPolicy, dry bool) {
	if h == nil {
		return
	}
	owner := t.Owner
	if owner == "" {
		owner = "authenticated GitHub user"
	}
	h.line("owner:    %s", humanText(owner))
	h.line("window:   %d days · push activity", t.Days)
	h.line("fetch:    %s · prune %s", humanText(policy.FetchScope), onOff(policy.Prune))
	h.line("force:    %s", onOff(policy.Force))
	h.line("cleanup:  %s (inactive clones)", onOff(policy.Cleanup))
	h.filters(t.Includes, t.Excludes)
	if policy.Force {
		h.line("! Force checkout can discard local changes and default-branch commits")
	}
	if policy.Cleanup {
		h.line("! Inactive cleanup can delete clone files and contained Git history")
	}
	if dry {
		h.line("Preview only · nothing will be changed or fetched")
	}
	h.line("")
}

func (h *humanReport) filters(includes, excludes []string) {
	inc, exc := "all", "none"
	if len(includes) > 0 {
		inc = strings.Join(includes, ", ")
	}
	if len(excludes) > 0 {
		exc = strings.Join(excludes, ", ")
	}
	h.line("include:  %s", humanText(inc))
	h.line("exclude:  %s", humanText(exc))
}

func (h *humanReport) event(e progress.Event) {
	if h == nil {
		return
	}
	switch e.Phase {
	case "checkout":
		h.checkout[e.Path] = e.Detail
	case "open-pr":
		h.openPR[e.Path] = e.Detail
	case "sync-result":
		if result, ok := e.Value.(syncer.Result); ok {
			h.syncResult(result)
		}
	case "clean-plan":
		if results, ok := e.Value.([]cleanup.Result); ok {

			h.line("\nChecked all targets · cleanup plan")
			h.cleanResults(results, true)
		}
	case "clean-result":
		if result, ok := e.Value.(cleanup.Result); ok {
			h.cleanResults([]cleanup.Result{result}, false)
		}
	case "warning":
		h.warning(e.Detail)
	default:
		detail := humanText(e.Detail)
		if e.Path != "" {
			h.line("  · %s (%s)", humanText(h.shortPath(e.Path)), detail)
		} else {
			h.line("  · %s", detail)
		}
	}
}

func (h *humanReport) shortPath(path string) string {
	for _, root := range h.roots {
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// Multiple targets can have the same relative name.
			if len(h.roots) == 1 {
				return rel
			}
		}
	}
	return path
}

func syncDescription(r syncer.Result) (string, string) {
	symbol, text := "~", r.Reason
	switch r.Action {
	case "cloned":
		symbol, text = "+", "cloned"
	case "up-to-date":
		symbol, text = "✓", "up to date"
	case "updated":
		symbol = "↓"
	case "forced":
		symbol, text = "↓", "forced · "+r.Reason
	case "would-clone":
		symbol, text = "+", "would clone"
	case "would-sync":
		symbol, text = "↓", "would fetch, then fast-forward if possible"
	case "would-force":
		symbol, text = "↓", r.Reason
	case "would-fetch":
		if _, reason, ok := strings.Cut(r.Reason, "; "); ok {
			text = reason + "; would fetch only"
		}
	case "would-remove":
		symbol, text = "-", "would remove inactive clone"
	case "removed":
		symbol, text = "-", "removed inactive clone"
	case "error":
		symbol, text = "x", "failed; details below"
	}
	if text == "" {
		text = strings.ReplaceAll(r.Action, "-", " ")
	}
	text = strings.ReplaceAll(text, "working tree has uncommitted or untracked changes", "dirty working tree")
	text = strings.ReplaceAll(text, "current branch is not the GitHub default branch", "on another branch")
	return symbol, text
}

func (h *humanReport) syncResult(r syncer.Result) {
	if h == nil {
		return
	}
	id := r.Action + "\x00" + r.Reason
	if h.seen[r.Path+"\x00"+r.Name] == id {
		return
	}
	h.seen[r.Path+"\x00"+r.Name] = id
	if !h.verbose && r.Action == "skipped" && (r.Reason == "inactive" || r.Reason == "excluded" || r.Reason == "not included" || r.Reason == "archived") {
		return
	}
	symbol, description := syncDescription(r)
	if branch, defaultBranch, ok := strings.Cut(h.checkout[r.Path], "\x00"); ok && strings.Contains(description, "on another branch") {
		description = strings.ReplaceAll(description, "on another branch", "on "+branch+", not "+defaultBranch)
	}
	h.line("  %s %s (%s)", symbol, humanText(r.Name), humanText(description))
	if r.Action == "error" {
		h.syncFailures[r.Name+": "+r.Reason] = true
		fmt.Fprintf(h.cmd.ErrOrStderr(), "error: %s: %s\n", humanText(r.Path), humanText(r.Reason))
	}
}

// The engine joins per-repository errors with target-level failures. Match
// complete leaf diagnostics rather than stripping substrings from error text.
func (h *humanReport) syncError(root string, err error) {
	if h == nil || err == nil {
		return
	}
	h.handledErrors[fmt.Sprintf("%s: %v", root, err)] = true
	var report func(error)
	report = func(e error) {
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, leaf := range joined.Unwrap() {
				report(leaf)
			}
			return
		}
		if !h.syncFailures[e.Error()] {
			fmt.Fprintf(h.cmd.ErrOrStderr(), "error: %s: %s\n", humanText(root), humanText(e.Error()))
		}
	}
	report(err)
}

func (h *humanReport) registerStates(states []repository.State) {
	for _, s := range states {
		if s.Primary {
			h.primary[s.CommonDir] = s.Path
		}
	}
}

func (h *humanReport) repoGroup(s repository.State) string {
	primary := h.primary[s.CommonDir]
	if primary == "" {
		primary = filepath.Dir(s.CommonDir)
	}
	if primary == "." || primary == "" {
		primary = s.Path
	}
	if h.group != primary {
		h.group = primary
		h.line("\nrepo: %s", humanText(primary))
	}
	for _, root := range h.roots {
		if insideHumanRoot(root, primary) && insideHumanRoot(root, s.Path) {
			if rel, err := filepath.Rel(primary, s.Path); err == nil {
				return rel
			}
		}
	}
	// Worktrees outside their primary's selected target keep absolute paths.
	return s.Path
}

func insideHumanRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cleanDescription(r cleanup.Result, planned bool) (string, string) {
	symbol, text := "=", r.Reason
	switch r.Action {
	case cleanup.ActionRemoved:
		symbol, text = "-", "removed checkout; branch refs retained"
		if r.Destructive {
			text = "force-removed checkout; branch refs retained"
		}
	case cleanup.ActionPruned:
		symbol, text = "-", "pruned stale registration"
	case cleanup.ActionWouldRemove:
		symbol, text = "-", "would remove checkout"
		if planned {
			text = "eligible for removal"
		}
		reason := strings.Split(r.Reason, "; force-removes")[0]
		if strings.HasPrefix(reason, "GitHub checks verified no open PR") {
			reason = "no open PR"
		}
		if strings.HasPrefix(reason, "clean linked worktree; exact current HEAD") {
			reason = "closed PR for this commit; no open PR"
		}
		if reason != "" {
			text += "; " + reason
		}
		if r.Destructive {
			text += "; includes local changes"
		}
	case cleanup.ActionWouldPrune:
		symbol, text = "-", "would prune stale metadata (Git expiry applies)"
		if planned {
			text = "eligible for metadata pruning (Git expiry applies)"
		}
	case cleanup.ActionFailed:
		symbol, text = "x", "failed; details below"
	default:
		text = strings.ReplaceAll(text, "primary clone is protected", "primary; keep")
		text = strings.ReplaceAll(text, "worktree is locked", "locked; keep")
		if r.Reason != "primary clone is protected" {
			symbol = "~"
			if !strings.HasSuffix(text, "keep") {
				text += "; keep"
			}
		}
	}
	return symbol, text
}

func (h *humanReport) cleanResults(results []cleanup.Result, planned bool) {
	if h == nil {
		return
	}
	for _, r := range results {
		if r.State.Primary {
			h.primary[r.State.CommonDir] = r.Path
		}
	}
	for _, r := range results {
		id := r.Action + "\x00" + r.Reason
		if h.seen[r.Path] == id {
			continue
		}
		h.seen[r.Path] = id
		state := r.State
		if state.Path == "" {
			state.Path = r.Path
		}
		name := h.repoGroup(state)
		branch := r.State.Branch
		if branch == "" {
			branch = "detached"
		}
		symbol, text := cleanDescription(r, planned)
		if r.Action == cleanup.ActionKeep && h.openPR[r.Path] != "" && strings.Contains(r.Reason, "open") {
			text = h.openPR[r.Path] + "; keep"
		}
		h.line("  %s %s (%s; %s)", symbol, humanText(name), humanText(branch), humanText(text))
	}
}

func (h *humanReport) statusStates(states []repository.State) {
	if h == nil {
		return
	}
	h.registerStates(states)
	for _, s := range states {
		name := h.repoGroup(s)
		branch := s.Branch
		if branch == "" {
			branch = "detached"
		}
		kind := "worktree"
		if s.Primary {
			kind = "primary"
		}
		flags := []string{}
		for _, f := range []struct {
			on    bool
			label string
		}{{s.Bare, "bare"}, {s.Missing, "missing"}, {s.Prunable, "stale registration"}, {s.Dirty, "modified files"}, {s.Untracked, "untracked files"}, {s.Ignored, "ignored files"}, {s.WorktreeLocked, "locked"}, {s.Locked && !s.WorktreeLocked, "Git lock/operation marker"}, {s.Stash, "stash"}, {s.Unique, "local-only commits"}, {s.CwdInUse, "working directory in use"}, {s.InUse && !s.CwdInUse, "open file/process reference"}, {!s.CwdInUseKnown, "working-directory use unknown"}, {!s.InUseKnown, "process use unknown"}, {len(s.Problems) > 0 || len(s.IdentityProblems) > 0, "inspection incomplete"}} {
			if f.on {
				flags = append(flags, f.label)
			}
		}
		if len(flags) == 0 {
			flags = append(flags, "clean")
		}
		distance := "no upstream comparison"
		if s.Ahead >= 0 && s.Behind >= 0 {
			distance = fmt.Sprintf("%d ahead · %d behind", s.Ahead, s.Behind)
		}
		h.line("  = %s (%s; %s)", humanText(name), kind, humanText(branch))
		h.line("    %s · %s", strings.Join(flags, ", "), distance)
		if h.verbose {
			h.line("    HEAD %s · origin %s", humanText(s.Head), humanText(s.Origin))
		}
		if len(s.SafetyProblems) > 0 {
			h.warning(s.Path + ": " + s.SafetyProblems[0])
		}
	}
}

func (h *humanReport) finish(env envelope) {
	if h == nil {
		return
	}
	for _, w := range env.Warnings {
		h.warning(w)
	}
	for _, err := range env.Errors {
		if h.handledErrors[err] {
			continue
		}
		for _, line := range strings.Split(err, "\n") {
			fmt.Fprintf(h.cmd.ErrOrStderr(), "error: %s\n", humanText(line))
		}
	}
	h.line("\n=== summary ===")
	counts := map[string]int{}
	switch items := env.Items.(type) {
	case []syncer.Result:
		for _, r := range items {
			key := r.Action
			if key == "skipped" && (r.Reason == "inactive" || r.Reason == "excluded" || r.Reason == "not included" || r.Reason == "archived") {
				key = r.Reason
			}
			counts[key]++
		}
		keys := []string{"cloned", "updated", "forced", "up-to-date", "fetched", "removed", "would-clone", "would-sync", "would-fetch", "would-force", "would-remove", "skipped", "inactive", "excluded", "not included", "archived", "error"}
		labels := map[string]string{"updated": "fast-forwarded", "up-to-date": "up to date", "fetched": "fetched only", "would-sync": "would fetch/update", "error": "failed"}
		for _, k := range keys {
			if counts[k] > 0 {
				label := labels[k]
				if label == "" {
					label = strings.ReplaceAll(k, "-", " ")
				}
				h.line("%-20s %d", label+":", counts[k])
			}
		}
		if len(items) == 0 && len(env.Errors) == 0 {
			h.line("No repositories found")
		}
	case []cleanup.Result:
		for _, r := range items {
			counts[r.Action]++
			if r.State.Primary {
				counts["repositories"]++
			}
		}
		h.line("repositories: %d", counts["repositories"])
		if env.DryRun {
			h.line("would remove: %d · would prune: %d · kept: %d", counts[cleanup.ActionWouldRemove], counts[cleanup.ActionWouldPrune], counts[cleanup.ActionKeep])
		} else {
			h.line("removed: %d · pruned: %d · kept: %d", counts[cleanup.ActionRemoved], counts[cleanup.ActionPruned], counts[cleanup.ActionKeep])
		}
		if counts[cleanup.ActionFailed] > 0 {
			h.line("failed: %d", counts[cleanup.ActionFailed])
		}
		if len(items) == 0 && len(env.Errors) == 0 {
			h.line("No repositories found")
		}
	case []repository.State:
		for _, s := range items {
			if s.Primary {
				counts["primary"]++
			} else {
				counts["worktree"]++
			}
		}
		h.line("repositories: %d · linked worktrees: %d", counts["primary"], counts["worktree"])
		if len(items) == 0 && len(env.Errors) == 0 {
			h.line("No repositories found")
		}
	}
	if h.cmd.Context() != nil && h.cmd.Context().Err() != nil {
		h.line("Interrupted · results above cover completed checks/actions only")
	} else if len(env.Errors) > 0 {
		h.line("Completed with errors · some actions were not completed")
	} else if env.DryRun && env.Command != "status" {
		h.line("Preview complete · nothing changed")
	} else {
		h.line("Done")
	}
}
