//go:build !linux && !darwin

package cleanup

import (
	"errors"
	"os"
)

func checkNestedMounts(string) error {
	return errors.New("aggressive content inspection is unsupported on this operating system")
}

func openSnapshotFile(*os.Root, string) (*os.File, error) {
	return nil, errors.New("safe content inspection is unsupported on this operating system")
}

func sameDevice(os.FileInfo, os.FileInfo) bool { return false }
