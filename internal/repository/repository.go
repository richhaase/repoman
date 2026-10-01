// Package repository inventories Git checkouts without changing repository data.
// Identity problems are distinct from optional local-content diagnostics.
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
// -1 when no upstream exists or the comparison could not be made. Problems
// includes local-content diagnostics used by strict policies. IdentityProblems
// identifies required Git identity and registration failures for every policy.
// InUseKnown covers all observable current-user process paths; CwdInUseKnown
// covers only working directories, as required by default aggressive cleanup.
type State struct {
	Bare             bool     `json:"bare"`
	IdentityProblems []string `json:"identity_problems,omitempty"`
	WorktreeLocked   bool     `json:"worktree_locked"`
	CwdInUse         bool     `json:"cwd_in_use"`
	CwdInUseKnown    bool     `json:"cwd_in_use_known"`
	CwdProblems      []string `json:"cwd_problems,omitempty"`
	Prunable         bool     `json:"prunable"`
	Missing          bool     `json:"missing"`
	PRNumbers        []int    `json:"pr_numbers,omitempty"`
	Path             string   `json:"path"`
	CommonDir        string   `json:"common_dir"`
	GitDir           string   `json:"git_dir"`
	Head             string   `json:"head"`
	Branch           string   `json:"branch"`
	Origin           string   `json:"origin"`
	Primary          bool     `json:"primary"`
	Dirty            bool     `json:"dirty"`
	Untracked        bool     `json:"untracked"`
	Ignored          bool     `json:"ignored"`
	Locked           bool     `json:"locked"`
	Stash            bool     `json:"stash"`
	Unique           bool     `json:"unique"`
	InUse            bool     `json:"in_use"`
	InUseKnown       bool     `json:"in_use_known"`
	ProcessScope     string   `json:"process_scope"`
	SafetyProblems   []string `json:"safety_problems,omitempty"`
	Ahead            int      `json:"ahead"`
	Behind           int      `json:"behind"`
	Problems         []string `json:"problems,omitempty"`
}

// Inspect examines exactly path, never an enclosing checkout. Git failures are
// retained in Problems and, when required for identity, IdentityProblems.
// Process observation failures are retained separately for each observation scope.
// The result is a snapshot; callers must inspect again immediately before acting.
func Inspect(ctx context.Context, path string) State {
	s := inspectGit(ctx, path)
	if s.GitDir != "" && s.CommonDir != "" {
		use := observeProcesses(ctx)
		applyUsage(&s, use)
		applyCwdUsage(&s, observeWorkingDirectories(ctx))
	}
	return s
}

func inspectGit(ctx context.Context, path string) State {
	s := State{Path: path, Ahead: -1, Behind: -1}
	if err := ctx.Err(); err != nil {
		s.identityProblem("inspection canceled: %v", err)
		return s
	}
	canonical, err := canonicalCheckout(path)
	if err != nil {
		s.identityProblem("checkout path: %v", err)
		return s
	}
	s.Path = canonical
	marker, markerErr := os.Lstat(filepath.Join(s.Path, ".git"))
	if markerErr != nil {
		if errors.Is(markerErr, os.ErrNotExist) {
			if bare, bareErr := gitLine(ctx, s.Path, "rev-parse", "--is-bare-repository"); bareErr == nil && bare == "true" {
				return inspectBare(ctx, s)
			}
		}
		s.identityProblem("read checkout Git marker: %v", markerErr)
		return s
	}
	if marker.Mode()&os.ModeSymlink != 0 {
		s.identityProblem("symlink Git markers are not accepted")
		return s
	}
	top, err := gitLine(ctx, s.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		s.identityProblem("find checkout root: %v", err)
		return s
	}
	top, err = canonicalDirectory(top)
	if err != nil || top != s.Path {
		s.identityProblem("path is not an exact Git checkout root")
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
			s.identityProblem("resolve %s: %v", target.flag, dirErr)
			return s
		}
		*target.dest = value
	}
	s.Primary = s.GitDir == s.CommonDir
	head, headErr := gitLine(ctx, s.Path, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		s.Head = head
	}
	if value, branchErr := gitLine(ctx, s.Path, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil {
		s.Branch = value
	} else if !exitOne(branchErr) {
		s.identityProblem("read branch: %v", branchErr)
	}
	if headErr != nil {
		_, branchExistsErr := gitLine(ctx, s.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+s.Branch)
		if !s.Primary || s.Branch == "" || !exitOne(branchExistsErr) {
			s.identityProblem("read HEAD: %v", headErr)
		}
	}
	if value, originErr := gitLine(ctx, s.Path, "config", "--get", "remote.origin.url"); originErr == nil {
		s.Origin = value
	} else if !exitOne(originErr) {
		s.identityProblem("read origin: %v", originErr)
	}
	inspectPRRefs(ctx, &s)
	inspectStatus(ctx, &s)
	inspectIndexProtections(ctx, &s)
	inspectRefs(ctx, &s)
	inspectLocks(&s)
	if records, wtErr := listWorktrees(ctx, s.Path); wtErr != nil {
		s.identityProblem("list worktrees: %v", wtErr)
	} else {
		found := false
		for _, record := range records {
			real, resolveErr := canonicalDirectory(record.path)
			if resolveErr == nil && real == s.Path {
				found = true
				s.Locked = s.Locked || record.locked
				s.WorktreeLocked = record.locked
				s.Prunable = record.prunable
				if !s.Primary {
					registered, registrationErr := registrationDirectory(s.CommonDir, record.path)
					if registrationErr != nil {
						s.identityProblem("verify registration: %v", registrationErr)
					} else if registered != s.GitDir {
						s.identityProblem("checkout Git directory differs from its registered metadata")
					}
				}
				if record.prunable {
					s.identityProblem("worktree is marked prunable")
				}
			}
		}
		if !found {
			s.identityProblem("checkout is absent from its repository worktree list")
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
				// Bare repository anchors have no .git child but can own linked worktrees.
				_, headErr := os.Lstat(filepath.Join(path, "HEAD"))
				objects, objectsErr := os.Stat(filepath.Join(path, "objects"))
				if headErr == nil && objectsErr == nil && objects.IsDir() {
					if bare, bareErr := gitLine(ctx, path, "rev-parse", "--is-bare-repository"); bareErr == nil && bare == "true" {
						seeds = append(seeds, path)
					}
				}
				continue
			}
			s := State{Path: path, Ahead: -1, Behind: -1}
			s.identityProblem("read Git marker: %v", statErr)
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
			s.identityProblem("discover linked worktrees: %v", wtErr)
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
			if _, pathErr := os.Lstat(record.path); errors.Is(pathErr, os.ErrNotExist) {
				linked = State{Path: filepath.Clean(record.path), CommonDir: s.CommonDir,
					Head: record.head, Branch: record.branch, Origin: s.Origin, Missing: true,
					Prunable: record.prunable, Ahead: -1, Behind: -1}
				linked.GitDir, pathErr = registrationDirectory(s.CommonDir, record.path)
				if pathErr != nil {
					linked.identityProblem("find stale registration: %v", pathErr)
				}
				if !record.prunable && !record.locked {
					linked.identityProblem("registered worktree is missing but not prunable")
				}
			}
			linked.WorktreeLocked = record.locked
			linked.Locked = linked.Locked || record.locked
			linked.Prunable = record.prunable
			if linked.CommonDir != "" && linked.CommonDir != s.CommonDir {
				linked.identityProblem("worktree registration points to a different repository")
			}
			states[linked.Path] = linked
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	usage := observeProcesses(ctx)
	cwdUsage := observeWorkingDirectories(ctx)
	result := make([]State, 0, len(states))
	for _, s := range states {
		applyUsage(&s, usage)
		applyCwdUsage(&s, cwdUsage)
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

func (s *State) identityProblem(format string, args ...any) {
	s.problem(format, args...)
	s.IdentityProblems = append(s.IdentityProblems, fmt.Sprintf(format, args...))
}

// registrationDirectory resolves an absent checkout's exact registered metadata.
func registrationDirectory(common, path string) (string, error) {
	base := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		data, readErr := os.ReadFile(filepath.Join(dir, "gitdir"))
		if readErr != nil {
			return "", readErr
		}
		registeredPath := strings.TrimSuffix(string(data), "\n")
		if !filepath.IsAbs(registeredPath) {
			registeredPath = filepath.Join(dir, registeredPath)
		}
		if filepath.Clean(registeredPath) == filepath.Join(path, ".git") {
			if found != "" {
				return "", errors.New("duplicate worktree registrations")
			}
			found = dir
		}
	}
	if found == "" {
		return "", errors.New("worktree registration not found")
	}
	return canonicalDirectory(found)
}

func inspectBare(ctx context.Context, s State) State {
	s.Bare, s.Primary = true, true
	dir, err := gitLine(ctx, s.Path, "rev-parse", "--absolute-git-dir")
	if err == nil {
		dir, err = canonicalDirectory(dir)
	}
	if err != nil || dir != s.Path {
		s.identityProblem("bare repository is not an exact Git directory: %v", err)
		return s
	}
	s.GitDir, s.CommonDir = dir, dir
	head, headErr := gitLine(ctx, s.Path, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		s.Head = head
	}
	branch, branchErr := gitLine(ctx, s.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr == nil {
		s.Branch = branch
	} else if !exitOne(branchErr) {
		s.identityProblem("read bare branch: %v", branchErr)
	}
	if headErr != nil {
		_, existsErr := gitLine(ctx, s.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+s.Branch)
		if s.Branch == "" || !exitOne(existsErr) {
			s.identityProblem("read bare HEAD: %v", headErr)
		}
	}
	origin, originErr := gitLine(ctx, s.Path, "config", "--get", "remote.origin.url")
	if originErr == nil {
		s.Origin = origin
	} else if !exitOne(originErr) {
		s.identityProblem("read bare origin: %v", originErr)
	}
	if _, err := listWorktrees(ctx, s.Path); err != nil {
		s.identityProblem("list bare worktrees: %v", err)
	}
	inspectLocks(&s)
	return s
}
