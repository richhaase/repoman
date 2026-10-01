//go:build linux || darwin

package cleanup

import (
	"os"
	"syscall"
)

func openSnapshotFile(root *os.Root, name string) (*os.File, error) {
	// O_NONBLOCK prevents a file replaced with a FIFO between Lstat and Open
	// from hanging. O_NOFOLLOW refuses a final-component symlink race.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

func sameDevice(a, b os.FileInfo) bool {
	left, leftOK := a.Sys().(*syscall.Stat_t)
	right, rightOK := b.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && left.Dev == right.Dev
}
