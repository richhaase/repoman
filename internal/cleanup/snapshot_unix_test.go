//go:build linux || darwin

package cleanup

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSnapshotRejectsFIFOWithoutOpening(t *testing.T) {
	state := syntheticState(t, t.TempDir(), "topic")
	if err := syscall.Mkfifo(filepath.Join(state.Path, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotContent(context.Background(), state); err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("FIFO not protected: %v", err)
	}
}
