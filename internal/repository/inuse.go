package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type processUsage struct {
	cwdOnly  bool
	paths    []string
	problems []string
}

func observeProcesses(ctx context.Context) processUsage { return observeProcessMode(ctx, false) }

func observeWorkingDirectories(ctx context.Context) processUsage {
	return observeProcessMode(ctx, true)
}

func observeProcessMode(ctx context.Context, cwdOnly bool) processUsage {
	usage := processUsage{cwdOnly: cwdOnly}
	cwd, err := os.Getwd()
	if err != nil {
		usage.problems = append(usage.problems, "cannot determine current working directory: "+err.Error())
	} else {
		usage.addPath(cwd)
	}
	switch runtime.GOOS {
	case "linux":
		usage.observeProc(ctx, "/proc")
	case "darwin":
		usage.observeLsof(ctx)
	default:
		usage.problems = append(usage.problems, "process-use observation is unsupported on "+runtime.GOOS)
	}
	return usage
}

func (u *processUsage) addPath(path string) {
	path = strings.TrimSuffix(path, " (deleted)")
	if !filepath.IsAbs(path) {
		return // Pipes, sockets, anonymous descriptors, and lsof annotations.
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	u.paths = append(u.paths, filepath.Clean(path))
}

func (u *processUsage) observeProc(ctx context.Context, root string) {
	processes, err := os.ReadDir(root)
	if err != nil {
		u.problems = append(u.problems, "cannot enumerate processes: "+err.Error())
		return
	}
	observed := 0
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			u.problems = append(u.problems, "process observation canceled: "+err.Error())
			return
		}
		if _, err := strconv.Atoi(process.Name()); err != nil || !process.IsDir() {
			continue
		}
		dir := filepath.Join(root, process.Name())
		status, statusErr := os.ReadFile(filepath.Join(dir, "status"))
		if statusErr != nil {
			if !errors.Is(statusErr, os.ErrNotExist) {
				u.observationError(dir, "owner", statusErr)
			}
			continue
		}
		own, ownerErr := processOwnedBy(status, os.Geteuid())
		if ownerErr != nil {
			u.observationError(dir, "owner", ownerErr)
			continue
		}
		if !own {
			continue // The supported observation contract covers the invoking user.
		}
		observed++
		names := []string{"cwd", "exe"}
		if u.cwdOnly {
			names = []string{"cwd"}
		}
		for _, name := range names {
			path, linkErr := os.Readlink(filepath.Join(dir, name))
			if linkErr == nil {
				u.addPath(path)
			} else if !errors.Is(linkErr, os.ErrNotExist) || (name == "cwd" && liveProcess(dir)) {
				u.observationError(dir, name, linkErr)
			}
		}
		if u.cwdOnly {
			continue
		}
		fds, readErr := os.ReadDir(filepath.Join(dir, "fd"))
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) || liveProcess(dir) {
				u.observationError(dir, "open files", readErr)
			}
			continue
		}
		for _, fd := range fds {
			path, linkErr := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if linkErr == nil {
				u.addPath(path)
			} else if !errors.Is(linkErr, os.ErrNotExist) {
				u.observationError(dir, "open file", linkErr)
			}
		}
	}
	if observed == 0 {
		u.problems = append(u.problems, "no current-user processes could be observed")
	}
}

func processOwnedBy(status []byte, uid int) (bool, error) {
	for _, line := range bytes.Split(status, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("Uid:")) {
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) != 5 {
			return false, errors.New("malformed process UID record")
		}
		own := false
		for _, field := range fields[1:] {
			value, err := strconv.Atoi(field)
			if err != nil || value < 0 {
				return false, errors.New("invalid process UID")
			}
			own = own || value == uid
		}
		return own, nil
	}
	return false, errors.New("process UID is unavailable")
}

// Kernel threads and zombies have no userspace cwd or descriptors. For a
// surviving userspace process, missing cwd/fd observations remain uncertain.
func liveProcess(dir string) bool {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || len(stat) <= end+2 {
		return true
	}
	if stat[end+2] == 'Z' || stat[end+2] == 'X' {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	return err != nil || len(cmdline) != 0
}

func (u *processUsage) observationError(dir, subject string, err error) {
	// A process disappearing during observation is expected and is no longer a
	// deletion blocker. An existing but inaccessible process remains unknown.
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		return
	}
	message := fmt.Sprintf("cannot observe process %s %s: %v", filepath.Base(dir), subject, err)
	u.problems = append(u.problems, message)
}

func (u *processUsage) observeLsof(ctx context.Context) {
	args := []string{"-nP", "-u", strconv.Itoa(os.Geteuid()), "-F0pn"}
	if u.cwdOnly {
		args = append(args, "-a", "-d", "cwd")
	}
	command := exec.CommandContext(ctx, "lsof", args...) // #nosec G204 -- fixed executable; UID is an OS-provided integer and no shell is used.
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		u.problems = append(u.problems, "cannot observe processes with lsof: "+err.Error())
		return
	}
	if stderr.Len() > 0 {
		u.problems = append(u.problems, "lsof observation was incomplete: "+strings.TrimSpace(stderr.String()))
	}
	foundProcess := false
	for _, field := range bytes.Split(output, []byte{0}) {
		// lsof -F0 terminates fields with NUL, and process/file sets with LF.
		field = bytes.TrimPrefix(field, []byte{'\n'})
		if len(field) < 2 {
			continue
		}
		switch field[0] {
		case 'p':
			foundProcess = true
		case 'n':
			u.addPath(string(field[1:]))
		}
	}
	if !foundProcess {
		u.problems = append(u.problems, "lsof returned no observable processes")
	}
}

func applyUsage(s *State, usage processUsage) {
	s.ProcessScope = "current-user"
	s.InUseKnown = len(usage.problems) == 0
	s.SafetyProblems = append([]string(nil), usage.problems...)
	for _, path := range usage.paths {
		if beneath(path, s.Path) || beneath(path, s.GitDir) || beneath(path, s.CommonDir) {
			s.InUse = true
			return
		}
	}
}

func applyCwdUsage(s *State, usage processUsage) {
	s.CwdInUseKnown = len(usage.problems) == 0
	s.CwdProblems = append([]string(nil), usage.problems...)
	for _, path := range usage.paths {
		if beneath(path, s.Path) {
			s.CwdInUse = true
			return
		}
	}
}
