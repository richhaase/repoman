package syncer

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/richhaase/repoman/internal/progress"
)

func TestProgressPreservesPreviewResults(t *testing.T) {
	f := newGitFixture(t)
	target := Target{Dir: f.clones, Owner: "alice", Days: 45}
	want, err := f.engine().runWithOptions(t.Context(), target, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	ctx := progress.WithReporter(t.Context(), func(event progress.Event) { phases = append(phases, event.Phase) })
	got, err := f.engine().runWithOptions(ctx, target, Options{DryRun: true})
	if err != nil || !reflect.DeepEqual(got, want) || f.fetches != 0 {
		t.Fatalf("progress changed preview: %+v %v", got, err)
	}
	if len(phases) < 3 || phases[0] != "list" || phases[len(phases)-1] != "sync-result" {
		t.Fatalf("event order: %v", phases)
	}
}

func TestProgressRechecksDestinationBeforeClone(t *testing.T) {
	root := t.TempDir()
	repo := activeRepo("project")
	target := Target{Dir: root, Owner: "alice", Days: 45}
	called := false
	e := engine{command: func(context.Context, string, string, ...string) ([]byte, error) { called = true; return nil, nil }}
	ctx := progress.WithReporter(t.Context(), func(event progress.Event) {
		if event.Phase == "sync-start" {
			if err := os.Mkdir(event.Path, 0700); err != nil {
				t.Fatal(err)
			}
		}
	})
	result, err := e.syncOneWithOptions(ctx, target, repo, Options{})
	if err != nil || called || result.Action != "skipped" {
		t.Fatalf("destination changed during output: %+v %v called=%t", result, err, called)
	}
}
