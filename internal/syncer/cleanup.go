package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/richhaase/repoman/internal/progress"
)

// cleanupOne follows the original inactive-clone dirty/untracked guard. Ignored
// files and local-only commits do not block explicit --cleanup. A clone that
// supplies linked worktrees is protected because removing it breaks them too.
func (e *engine) cleanupOne(ctx context.Context, target Target, repo remoteRepo, dryRun bool) (Result, error) {
	result := Result{Name: repo.Name, Path: filepath.Join(target.Dir, repo.Name), Action: "skipped", Reason: "inactive"}
	progress.Report(ctx, progress.Event{Phase: "inactive", Path: result.Path, Detail: "checking inactive clone"})
	if err := checkRoot(target.Dir); err != nil {
		return result, err
	}
	info, err := os.Lstat(result.Path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("inspect inactive destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		result.Reason = "inactive destination is not a real directory"
		return result, nil
	}
	// Recheck status and identity immediately before apply. A failed Git query
	// is an error, never evidence that a clone is clean enough to remove.
	checks := 1
	if !dryRun {
		checks = 2
	}
	for range checks {
		state, reason, err := e.primaryState(ctx, result.Path, repo)
		if err != nil || reason != "" {
			result.Reason = reason
			return result, err
		}
		if len(state.Problems) > 0 {
			return result, fmt.Errorf("inspect inactive checkout: %s", strings.Join(state.Problems, "; "))
		}
		if state.Dirty || state.Untracked {
			result.Reason = "inactive but has uncommitted or untracked changes; keeping"
			return result, nil
		}
		linked, err := e.hasLinkedWorktrees(ctx, result.Path)
		if err != nil {
			return result, err
		}
		if linked {
			result.Reason = "inactive primary clone supplies linked worktrees; keeping"
			return result, nil
		}
	}
	if dryRun {
		result.Action = "would-remove"
		result.Reason = "inactive primary clone with no uncommitted or untracked changes; ignored files and local commits may be removed"
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkRoot(target.Dir); err != nil {
		return result, err
	}
	latest, err := os.Lstat(result.Path)
	if err != nil || !os.SameFile(info, latest) {
		return result, errors.New("inactive destination changed before removal; left untouched")
	}
	if err := os.RemoveAll(result.Path); err != nil {
		return result, fmt.Errorf("remove inactive clone: %w", err)
	}
	result.Action = "removed"
	result.Reason = "removed inactive primary clone"
	return result, nil
}

func (e *engine) hasLinkedWorktrees(ctx context.Context, dir string) (bool, error) {
	out, err := e.command(ctx, dir, "git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, fmt.Errorf("inspect inactive clone worktrees: %w", err)
	}
	if !strings.HasSuffix(string(out), "\x00\x00") {
		return false, errors.New("incomplete inactive clone worktree listing")
	}
	count := 0
	primaryFound := false
	for _, field := range strings.Split(string(out), "\x00") {
		if name, ok := strings.CutPrefix(field, "worktree "); ok {
			count++
			primaryFound = primaryFound || filepath.Clean(name) == dir
		}
	}
	if count == 0 || !primaryFound {
		return false, errors.New("inactive clone is absent from its worktree listing")
	}
	return count > 1, nil
}
