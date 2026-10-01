package cleanup

import (
	"context"
	"reflect"
	"testing"

	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repository"
)

func TestProgressWarningAndValidatedPlanPrecedeRemoval(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	state.CwdInUseKnown = false
	e := fixtureEngine([]repository.State{state})
	e.options = Options{Level: Aggressive}
	var phases []string
	ctx := progress.WithReporter(t.Context(), func(event progress.Event) { phases = append(phases, event.Phase) })
	e.remove = func(context.Context, repository.State) error {
		if !containsPhase(phases, "warning") || !containsPhase(phases, "clean-plan") {
			t.Fatalf("removing before warning/validated plan: %v", phases)
		}
		phases = append(phases, "actual-remove")
		return nil
	}
	got, err := e.run(ctx, root, true)
	if err != nil || len(got) != 1 || got[0].Action != ActionRemoved {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if !reflect.DeepEqual(phases[len(phases)-2:], []string{"actual-remove", "clean-result"}) {
		t.Fatalf("result precedes actual removal: %v", phases)
	}
}

func containsPhase(phases []string, want string) bool {
	for _, phase := range phases {
		if phase == want {
			return true
		}
	}
	return false
}

func TestProgressPreviewReportsNoRemovalAndKeepsResults(t *testing.T) {
	root := t.TempDir()
	state := syntheticState(t, root, "topic")
	e := fixtureEngine([]repository.State{state})
	want, err := e.run(t.Context(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := progress.WithReporter(t.Context(), func(event progress.Event) {
		if event.Phase == "remove" || event.Phase == "clean-result" || event.Phase == "clean-plan" {
			t.Fatalf("preview announced application: %+v", event)
		}
	})
	got, err := e.run(ctx, root, false)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("observer changed preview: %+v %v", got, err)
	}
}

func TestProgressWritesCannotBypassLateProtection(t *testing.T) {
	for _, phase := range []string{"remove", "warning"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			live := syntheticState(t, root, "topic")
			e := fixtureEngine([]repository.State{live})
			e.options = Options{Level: Aggressive}
			e.discover = func(context.Context, string) ([]repository.State, error) { return []repository.State{live}, nil }
			e.inspect = func(context.Context, string) repository.State {
				s := live
				if phase == "warning" {
					s.CwdInUseKnown = false
				}
				return s
			}
			removed := false
			e.remove = func(context.Context, repository.State) error { removed = true; return nil }
			ctx := progress.WithReporter(t.Context(), func(event progress.Event) {
				if event.Phase == phase {
					if phase == "remove" {
						live.WorktreeLocked = true
					} else {
						live.CwdInUse = true
					}
				}
			})
			_, _ = e.run(ctx, root, true)
			if removed {
				t.Fatal("a delayed progress write bypassed final protection")
			}
		})
	}
}

func TestProgressCancellationStopsBeforeMutation(t *testing.T) {
	for _, phase := range []string{"clean-plan", "remove", "warning"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			state := syntheticState(t, root, "topic")
			e := fixtureEngine([]repository.State{state})
			e.options = Options{Level: Aggressive}
			e.inspect = func(context.Context, string) repository.State {
				fresh := state
				if phase == "warning" {
					fresh.CwdInUseKnown = false
				}
				return fresh
			}
			removed := false
			e.remove = func(context.Context, repository.State) error { removed = true; return nil }
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := progress.WithReporter(base, func(event progress.Event) {
				if event.Phase == phase {
					cancel()
				}
			})
			_, err := e.run(ctx, root, true)
			if removed || err == nil {
				t.Fatalf("phase %s: removed=%t err=%v", phase, removed, err)
			}
		})
	}
}
