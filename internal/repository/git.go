package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// runGit removes inherited Git routing overrides, suppresses optional index
// writes, and disables fsmonitor hooks. All callers use read-only Git commands.
func runGit(ctx context.Context, path string, args ...string) ([]byte, error) {
	base := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-C", path}
	command := exec.CommandContext(ctx, "git", append(base, args...)...) // #nosec G204 -- executable is fixed; args never go through a shell.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func gitLine(ctx context.Context, path string, args ...string) (string, error) {
	output, err := runGit(ctx, path, args...)
	// Remove the protocol's newline, not whitespace that belongs to a path.
	return strings.TrimSuffix(string(output), "\n"), err
}

func exitOne(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

func inspectStatus(ctx context.Context, s *State) {
	output, err := runGit(ctx, s.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		s.problem("read checkout status: %v", err)
		return
	}
	if err := parseStatus(output, s); err != nil {
		s.problem("read checkout status: %v", err)
	}
}

// Porcelain omits files hidden by index flags, even when they contain changes.
// Reject those checkouts instead of clearing flags or refreshing their index.
func inspectIndexProtections(ctx context.Context, s *State) {
	output, err := runGit(ctx, s.Path, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		s.problem("read index protections: %v", err)
		return
	}
	if len(output) == 0 {
		return
	}
	if output[len(output)-1] != 0 {
		s.problem("unterminated index listing")
		return
	}
	for _, record := range bytes.Split(output[:len(output)-1], []byte{0}) {
		if len(record) < 10 || record[1] != ' ' {
			s.problem("malformed index listing")
			return
		}
		if record[0] == 'S' || (record[0] >= 'a' && record[0] <= 'z') {
			s.problem("index contains skip-worktree or assume-unchanged entries that may hide local data")
		}
		if bytes.HasPrefix(record[2:], []byte("160000 ")) {
			s.problem("checkout contains submodules requiring independent data inspection")
		}
	}
}

func parseStatus(output []byte, s *State) error {
	if len(output) == 0 {
		return nil
	}
	if output[len(output)-1] != 0 {
		return errors.New("unterminated status record")
	}
	records := bytes.Split(output[:len(output)-1], []byte{0})
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' {
			return errors.New("malformed status record")
		}
		switch string(record[:2]) {
		case "??":
			s.Untracked = true
		case "!!":
			s.Ignored = true
		default:
			s.Dirty = true
		}
		// Porcelain v1 -z renames and copies have a second, unprefixed path.
		if bytes.ContainsAny(record[:2], "RC") {
			i++
			if i >= len(records) || len(records[i]) == 0 {
				return errors.New("missing rename source in status")
			}
		}
	}
	return nil
}

func inspectRefs(ctx context.Context, s *State) {
	stash, err := gitLine(ctx, s.Path, "for-each-ref", "--format=%(objectname)", "refs/stash")
	if err != nil {
		s.problem("read stash: %v", err)
	} else {
		s.Stash = stash != ""
	}
	shallow, err := gitLine(ctx, s.Path, "rev-parse", "--is-shallow-repository")
	if err != nil {
		s.problem("read history completeness: %v", err)
	} else if shallow != "false" {
		s.problem("repository history is shallow or its completeness is unknown")
	}
	if s.Head != "" {
		count, uniqueErr := gitLine(ctx, s.Path, "rev-list", "--count", "HEAD", "--branches", "--not", "--remotes")
		if uniqueErr != nil {
			s.problem("compare local commits to remote refs: %v", uniqueErr)
		} else if value, parseErr := strconv.ParseUint(count, 10, 64); parseErr != nil {
			s.problem("invalid local commit count: %v", parseErr)
		} else {
			s.Unique = value != 0
		}
	}
	if s.Branch == "" || s.Head == "" {
		return
	}
	upstream, err := gitLine(ctx, s.Path, "for-each-ref", "--format=%(upstream)", "refs/heads/"+s.Branch)
	if err != nil {
		s.problem("read upstream: %v", err)
		return
	}
	if upstream == "" {
		return
	}
	counts, err := gitLine(ctx, s.Path, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		s.problem("compare upstream: %v", err)
		return
	}
	parts := strings.Fields(counts)
	if len(parts) != 2 {
		s.problem("invalid upstream comparison")
		return
	}
	ahead, aErr := strconv.Atoi(parts[0])
	behind, bErr := strconv.Atoi(parts[1])
	if aErr != nil || bErr != nil || ahead < 0 || behind < 0 {
		s.problem("invalid upstream commit counts")
		return
	}
	s.Ahead, s.Behind = ahead, behind
}

type worktree struct {
	head     string
	branch   string
	path     string
	locked   bool
	prunable bool
}

func listWorktrees(ctx context.Context, path string) ([]worktree, error) {
	output, err := runGit(ctx, path, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(output)
}

func parseWorktrees(output []byte) ([]worktree, error) {
	if len(output) == 0 || !bytes.HasSuffix(output, []byte{0, 0}) {
		return nil, errors.New("missing or unterminated worktree listing")
	}
	var result []worktree
	var current *worktree
	for _, raw := range bytes.Split(output, []byte{0}) {
		field := string(raw)
		if field == "" {
			if current != nil {
				result = append(result, *current)
				current = nil
			}
			continue
		}
		if strings.HasPrefix(field, "worktree ") {
			if current != nil {
				return nil, errors.New("missing worktree record separator")
			}
			path := strings.TrimPrefix(field, "worktree ")
			if path == "" || !filepath.IsAbs(path) {
				return nil, errors.New("worktree path is not absolute")
			}
			current = &worktree{path: path}
			continue
		}
		if current == nil {
			return nil, errors.New("worktree metadata has no path")
		}
		switch {
		case field == "locked" || strings.HasPrefix(field, "locked "):
			current.locked = true
		case field == "prunable" || strings.HasPrefix(field, "prunable "):
			current.prunable = true
		case strings.HasPrefix(field, "HEAD "):
			current.head = strings.TrimPrefix(field, "HEAD ")
		case strings.HasPrefix(field, "branch "):
			current.branch = strings.TrimPrefix(field, "branch refs/heads/")
		case field == "bare", field == "detached":
			// Git metadata that is not needed for path discovery.
		default:
			return nil, fmt.Errorf("unrecognized worktree metadata %q", field)
		}
	}
	if current != nil || len(result) == 0 {
		return nil, errors.New("incomplete worktree listing")
	}
	return result, nil
}

func inspectLocks(s *State) {
	seen := make(map[string]bool)
	for _, dir := range []string{s.GitDir, s.CommonDir} {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(filepath.Join(dir, "info", "grafts")); err == nil && info.Size() > 0 {
			s.problem("legacy grafts can obscure commit reachability")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.problem("read legacy grafts: %v", err)
		}
		walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".lock") || operationMarker(name) {
				s.Locked = true
			}
			if path != dir && entry.IsDir() {
				switch name {
				case "objects", "logs", "hooks", "worktrees", "modules":
					return filepath.SkipDir
				}
			}
			if entry.Type()&os.ModeSymlink != 0 {
				// Git administrative symlinks obscure which metadata was inspected.
				return fmt.Errorf("symlink in Git metadata: %q", path)
			}
			return nil
		})
		if walkErr != nil {
			s.problem("inspect operation locks: %v", walkErr)
		}
	}
}

func operationMarker(name string) bool {
	switch name {
	case "locked", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_LOG", "rebase-apply", "rebase-merge", "sequencer", "gc.pid":
		return true
	default:
		return false
	}
}

var prReference = regexp.MustCompile(`(?:^|/)pr/([0-9]+)$`)

func inspectPRRefs(ctx context.Context, s *State) {
	if s.Head == "" {
		return
	}
	output, err := gitLine(ctx, s.Path, "for-each-ref", "--contains", s.Head, "--format=%(refname)")
	if err != nil {
		s.identityProblem("read PR reference associations: %v", err)
		return
	}
	seen := make(map[int]bool)
	for _, ref := range strings.Split(output, "\n") {
		match := prReference.FindStringSubmatch(ref)
		if len(match) == 0 {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number <= 0 {
			s.identityProblem("invalid PR reference number: %q", ref)
			continue
		}
		if !seen[number] {
			s.PRNumbers = append(s.PRNumbers, number)
			seen[number] = true
		}
	}
}
