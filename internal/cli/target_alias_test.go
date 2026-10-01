package cli

import (
	"path/filepath"
	"testing"
)

func TestTargetAliasesBypassConfig(t *testing.T) {
	t.Setenv("REPOMAN_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, flag := range []string{"--root", "--target", "-t"} {
		t.Run(flag, func(t *testing.T) {
			_, _, err := executeCommand(t, t.Context(), "status", flag, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTargetAliasesConflict(t *testing.T) {
	_, _, err := executeCommand(t, t.Context(), "status", "--root", t.TempDir(), "--target", t.TempDir())
	if err == nil {
		t.Fatal("conflicting target aliases must be rejected")
	}
}
