package cleanup

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

func checkNestedMounts(path string) error {
	// Darwin MNT_NOWAIT avoids waiting for unresponsive network filesystems.
	const mntNoWait = 2
	count, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil {
		return fmt.Errorf("cannot enumerate mounted filesystems: %w", err)
	}
	if count < 1 || count > 4096 {
		return errors.New("mount enumeration is empty or exceeds inspection limit")
	}
	// Leave spare capacity for mounts added between the two calls. A full
	// response may be truncated, so it cannot establish safe absence.
	mounts := make([]syscall.Statfs_t, count+16)
	n, err := syscall.Getfsstat(mounts, mntNoWait)
	if err != nil {
		return fmt.Errorf("cannot inspect mounted filesystems: %w", err)
	}
	if n < 1 || n >= len(mounts) {
		return errors.New("mount enumeration changed or may be incomplete")
	}
	for _, mount := range mounts[:n] {
		var name []byte
		terminated := false
		for _, value := range mount.Mntonname {
			if value == 0 {
				terminated = true
				break
			}
			name = append(name, byte(int(value)&0xff))
		}
		if !terminated || !filepath.IsAbs(string(name)) {
			return errors.New("mount path is invalid or truncated")
		}
		resolved, err := filepath.EvalSymlinks(string(name))
		if err != nil {
			return fmt.Errorf("cannot resolve mounted filesystem: %w", err)
		}
		if within(path, string(name)) || within(path, resolved) {
			return fmt.Errorf("mounted filesystem at %q is protected", name)
		}
	}
	return nil
}
