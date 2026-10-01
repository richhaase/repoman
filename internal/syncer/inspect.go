package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/richhaase/repoman/internal/repository"
)

// inspectClone reads the identity and status needed by the sync script. It does
// not impose history, ignored-file, linked-worktree, or worktree-cleanup policy.
func (e *engine) inspectClone(ctx context.Context, dir string) (repository.State, error) {
	state := repository.State{}
	for _, field := range []struct {
		flag string
		dest *string
	}{
		{"--show-toplevel", &state.Path},
		{"--git-dir", &state.GitDir},
		{"--git-common-dir", &state.CommonDir},
	} {
		out, err := e.command(ctx, dir, "git", "rev-parse", "--path-format=absolute", field.flag)
		if err != nil {
			return state, fmt.Errorf("read %s: %w", field.flag, err)
		}
		value := strings.TrimSuffix(string(out), "\n")
		canonical, err := filepath.EvalSymlinks(value)
		if err != nil {
			return state, fmt.Errorf("resolve %s: %w", field.flag, err)
		}
		*field.dest = canonical
	}
	state.Primary = state.GitDir == state.CommonDir
	out, err := e.command(ctx, dir, "git", "config", "--get", "remote.origin.url")
	if err != nil && !commandExitOne(err) {
		return state, fmt.Errorf("read origin: %w", err)
	}
	state.Origin = strings.TrimSpace(string(out))
	out, err = e.command(ctx, dir, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && !commandExitOne(err) {
		state.Problems = append(state.Problems, "read branch: "+err.Error())
	} else {
		state.Branch = strings.TrimSpace(string(out))
	}
	out, err = e.command(ctx, dir, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		// An unborn branch has no HEAD yet. Fetch and clean inactive eviction
		// remain valid; failures reading an existing branch are still errors.
		_, branchErr := e.command(ctx, dir, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+state.Branch)
		if state.Branch == "" || !commandExitOne(branchErr) {
			state.Problems = append(state.Problems, "read HEAD: "+err.Error())
		}
	} else {
		state.Head = strings.TrimSpace(string(out))
	}
	out, err = e.command(ctx, dir, "git", "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		state.Problems = append(state.Problems, "read checkout status: "+err.Error())
	} else if err := cloneStatus(out, &state); err != nil {
		state.Problems = append(state.Problems, "read checkout status: "+err.Error())
	}
	return state, nil
}

func commandExitOne(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

func cloneStatus(out []byte, state *repository.State) error {
	if len(out) == 0 {
		return nil
	}
	if out[len(out)-1] != 0 {
		return errors.New("unterminated status record")
	}
	records := bytes.Split(out[:len(out)-1], []byte{0})
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' {
			return errors.New("malformed status record")
		}
		switch string(record[:2]) {
		case "??":
			state.Untracked = true
		case "!!":
			state.Ignored = true
		default:
			state.Dirty = true
		}
		if bytes.ContainsAny(record[:2], "RC") {
			i++
			if i >= len(records) || len(records[i]) == 0 {
				return errors.New("missing rename source in status")
			}
		}
	}
	return nil
}
