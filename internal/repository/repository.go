// Package repository inventories Git checkouts without changing repository data.
// A State with any Problems is incomplete and must never authorize deletion.
package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// State describes a single checkout, including shared repository protections.
// Successfully resolved paths are absolute and canonical. Ahead and Behind are
// -1 when there is no
// upstream or the comparison could not be made. Problems always fail closed.
// InUseKnown covers the invoking OS user's observable processes, not all users;
// ProcessScope documents that boundary. SafetyProblems are cleanup-only issues.
type State struct {
	Path           string   `json:"path"`
	CommonDir      string   `json:"common_dir"`
	GitDir         string   `json:"git_dir"`
	Head           string   `json:"head"`
	Branch         string   `json:"branch"`
	Origin         string   `json:"origin"`
	Primary        bool     `json:"primary"`
	Dirty          bool     `json:"dirty"`
	Untracked      bool     `json:"untracked"`
	Ignored        bool     `json:"ignored"`
	Locked         bool     `json:"locked"`
	Stash          bool     `json:"stash"`
	Unique         bool     `json:"unique"`
	InUse          bool     `json:"in_use"`
	InUseKnown     bool     `json:"in_use_known"`
	ProcessScope   string   `json:"process_scope"`
	SafetyProblems []string `json:"safety_problems,omitempty"`
	Ahead          int      `json:"ahead"`
	Behind         int      `json:"behind"`
	Problems       []string `json:"problems,omitempty"`
}

// Inspect examines exactly path, never an enclosing checkout. Git failures are
// retained in Problems; process observation failures are in SafetyProblems.
// Neither type of uncertainty may be interpreted as safe for deletion.
// The result is a snapshot; callers must inspect again immediately before acting.
func Inspect(ctx context.Context, path string) State {
	s := inspectGit(ctx, path)
	if s.GitDir != "" && s.CommonDir != "" {
		use := observeProcesses(ctx)
		applyUsage(&s, use)
	}
	return s
}

func inspectGit(ctx context.Context, path string) State {
	s := State{Path: path, Ahead: -1, Behind: -1}
	if err := ctx.Err(); err != nil {
		s.problem("inspection canceled: %v", err)
		return s
	}
	canonical, err := canonicalCheckout(path)
	if err != nil {
		s.problem("checkout path: %v", err)
		return s
	}
	s.Path = canonical
	marker, markerErr := os.Lstat(filepath.Join(s.Path, ".git"))
	if markerErr != nil {
		s.problem("read checkout Git marker: %v", markerErr)
		return s
	}
	if marker.Mode()&os.ModeSymlink != 0 {
		s.problem("symlink Git markers are not accepted")
		return s
	}
	top, err := gitLine(ctx, s.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		s.problem("find checkout root: %v", err)
		return s
	}
	top, err = canonicalDirectory(top)
	if err != nil || top != s.Path {
		s.problem("path is not an exact Git checkout root")
		return s
	}
	for _, target := range []struct {
		flag string
		dest *string
	}{
		{"--git-dir", &s.GitDir},
		{"--git-common-dir", &s.CommonDir},
	} {
		value, dirErr := gitLine(ctx, s.Path, "rev-parse", "--path-format=absolute", target.flag)
		if dirErr == nil {
			value, dirErr = canonicalDirectory(value)
		}
		if dirErr != nil {
			s.problem("resolve %s: %v", target.flag, dirErr)
			return s
		}
		*target.dest = value
	}
	s.Primary = s.GitDir == s.CommonDir
	if value, headErr := gitLine(ctx, s.Path, "rev-parse", "--verify", "HEAD"); headErr != nil {
		s.problem("read HEAD: %v", headErr)
	} else {
		s.Head = value
	}
	if value, branchErr := gitLine(ctx, s.Path, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil {
		s.Branch = value
	} else if !exitCode(branchErr, 1) {
		s.problem("read branch: %v", branchErr)
	}
	if value, originErr := gitLine(ctx, s.Path, "config", "--get", "remote.origin.url"); originErr == nil {
		s.Origin = value
	} else if !exitCode(originErr, 1) {
		s.problem("read origin: %v", originErr)
	}
	inspectStatus(ctx, &s)
	inspectIndexProtections(ctx, &s)
	inspectRefs(ctx, &s)
	inspectLocks(&s)
	if records, wtErr := listWorktrees(ctx, s.Path); wtErr != nil {
		s.problem("list worktrees: %v", wtErr)
	} else {
		found := false
		for _, record := range records {
			real, resolveErr := canonicalDirectory(record.path)
			if resolveErr == nil && real == s.Path {
				found = true
				s.Locked = s.Locked || record.locked
				if record.prunable {
					s.problem("worktree is marked prunable")
				}
			}
		}
		if !found {
			s.problem("checkout is absent from its repository worktree list")
		}
	}
	return s
}

// Discover finds repositories immediately below root and every linked worktree
// registered to them, including worktrees outside root. It does not recurse into
// arbitrary directories and does not follow symlink entries. Results are sorted
// by canonical checkout path and deduplicated. Unreadable or invalid Git entries
// are returned as protected States; root enumeration failures return an error.
func Discover(ctx context.Context, root string) ([]State, error) {
	canonical, err := canonicalDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("resolve inventory root: %w", err)
	}
	entries, err := os.ReadDir(canonical)
	if err != nil {
		return nil, fmt.Errorf("read inventory root: %w", err)
	}
	states := make(map[string]State)
	var seeds []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(canonical, entry.Name())
		if _, statErr := os.Lstat(filepath.Join(path, ".git")); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			s := State{Path: path, Ahead: -1, Behind: -1}
			s.problem("read Git marker: %v", statErr)
			states[path] = s
			continue
		}
		seeds = append(seeds, path)
	}
	seenCommon := make(map[string]bool)
	for _, path := range seeds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, exists := states[path]
		if !exists {
			s = inspectGit(ctx, path)
			states[s.Path] = s
		}
		if s.CommonDir == "" || seenCommon[s.CommonDir] {
			continue
		}
		seenCommon[s.CommonDir] = true
		records, wtErr := listWorktrees(ctx, s.Path)
		if wtErr != nil {
			s.problem("discover linked worktrees: %v", wtErr)
			states[s.Path] = s
			continue
		}
		for _, record := range records {
			key, resolveErr := canonicalDirectory(record.path)
			if resolveErr != nil {
				key = filepath.Clean(record.path)
			}
			if _, exists := states[key]; exists {
				continue
			}
			linked := inspectGit(ctx, record.path)
			linked.Locked = linked.Locked || record.locked
			if record.prunable {
				linked.problem("worktree is marked prunable")
			}
			if linked.CommonDir != "" && linked.CommonDir != s.CommonDir {
				linked.problem("worktree registration points to a different repository")
			}
			states[linked.Path] = linked
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	usage := observeProcesses(ctx)
	result := make([]State, 0, len(states))
	for _, s := range states {
		applyUsage(&s, usage)
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func canonicalCheckout(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink checkout roots are not accepted: %q", absolute)
	}
	return canonicalDirectory(absolute)
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %q", path)
	}
	return canonical, nil
}

func (s *State) problem(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, existing := range s.Problems {
		if existing == message {
			return
		}
	}
	s.Problems = append(s.Problems, message)
}

func beneath(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
