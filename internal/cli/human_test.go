package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/richhaase/repoman/internal/cleanup"
	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repository"
	"github.com/richhaase/repoman/internal/syncer"
)

func TestHumanSyncHeaderPrecedesWorkAndResultsStreamOnce(t *testing.T) {
	root := t.TempDir()
	var out, diag bytes.Buffer
	result := syncer.Result{Path: filepath.Join(root, "alpha"), Name: "alpha", Action: "up-to-date"}
	cmd := newSyncCmdWithRunner(func(ctx context.Context, _ syncer.Target, _ syncer.Options) ([]syncer.Result, error) {
		for _, want := range []string{"Sync · apply", "fetch:    origin · prune off", "force:    on", "cleanup:  on", "discard local changes", "contained Git history"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("header missing before runner: %q", want)
			}
		}
		progress.Report(ctx, progress.Event{Phase: "sync-result", Value: result})
		if !strings.Contains(out.String(), "✓ alpha (up to date)") {
			t.Fatal("result did not stream before runner returned")
		}
		return []syncer.Result{result}, nil
	})
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs([]string{"--root", root, "--owner", "me", "--force", "--cleanup"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "✓ alpha (up to date)") != 1 || strings.Contains(out.String(), "alpha:") {
		t.Fatalf("duplicate or raw action row: %s", out.String())
	}
	if diag.Len() != 0 || !strings.Contains(out.String(), "up to date:") {
		t.Fatalf("bad summary: %s %s", out.String(), diag.String())
	}
}

func TestHumanSyncSkipsAreCountedAndVerboseShowsThem(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.Flags().Bool("verbose", verbose, "")
		cmd.SetOut(&out)
		h := newHumanReport(cmd, false)
		items := []syncer.Result{{Name: "old", Path: "/src/old", Action: "skipped", Reason: "inactive"}, {Name: "excluded-repo", Path: "/src/excluded-repo", Action: "skipped", Reason: "excluded"}, {Name: "new", Path: "/src/new", Action: "would-clone"}}
		for _, r := range items {
			h.syncResult(r)
		}
		h.finish(envelope{Command: "sync", DryRun: true, Items: items, Errors: []string{}})
		if strings.Contains(out.String(), "old (inactive)") != verbose {
			t.Fatal("routine skip visibility ignores verbose")
		}
		for _, want := range []string{"inactive:", "excluded:", "would clone:", "Preview complete"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, out.String())
			}
		}
		if strings.Contains(out.String(), "cloned:") {
			t.Fatal("preview claims completed clone")
		}
	}
}

func TestHumanErrorsAreReportedOnceAndKeepExitCodes(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		var out, diag bytes.Buffer
		result := syncer.Result{Name: "broken", Path: "/src/broken", Action: "error", Reason: "unique fixture failure"}
		cmd := newSyncCmdWithRunner(func(ctx context.Context, _ syncer.Target, _ syncer.Options) ([]syncer.Result, error) {
			progress.Report(ctx, progress.Event{Phase: "sync-result", Value: result})
			return []syncer.Result{result}, errors.New("broken: unique fixture failure")
		})
		args := []string{"--root", t.TempDir(), "--owner", "me"}
		if jsonMode {
			args = append(args, "--json")
		}
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&diag)
		err := cmd.ExecuteContext(t.Context())
		if code := reportExecution(err, &diag); code != 3 {
			t.Fatalf("exit=%d err=%v", code, err)
		}
		if jsonMode {
			var env envelope
			if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.SchemaVersion != 1 || len(env.Errors) != 1 {
				t.Fatalf("JSON contaminated: %s %v", out.String(), err)
			}
			if strings.Contains(out.String(), "===") || !strings.Contains(diag.String(), "Error: completed with errors") {
				t.Fatal("JSON output/exit diagnostic contract changed")
			}
		} else {
			if strings.Count(out.String()+diag.String(), "unique fixture failure") != 1 || strings.Contains(diag.String(), "Error: completed") {
				t.Fatalf("duplicate errors: %s %s", out.String(), diag.String())
			}
		}
	}
}

func TestHumanCancellationDoesNotClaimCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out, diag bytes.Buffer
	cmd := newSyncCmdWithRunner(func(context.Context, syncer.Target, syncer.Options) ([]syncer.Result, error) {
		cancel()
		return nil, context.Canceled
	})
	cmd.SetArgs([]string{"--root", t.TempDir(), "--owner", "me"})
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	err := cmd.ExecuteContext(ctx)
	if reportExecution(err, &diag) != 130 || !errors.Is(err, context.Canceled) || !strings.Contains(out.String(), "Interrupted") || strings.Contains(out.String(), "Done") {
		t.Fatalf("cancellation: %s %s %v", out.String(), diag.String(), err)
	}
}

func TestHumanCleanWarningPrecedesRunnerAndPlanIsNotCompletion(t *testing.T) {
	root := t.TempDir()
	var out, diag bytes.Buffer
	primary := repository.State{Path: filepath.Join(root, "repo"), CommonDir: filepath.Join(root, "repo/.git"), Primary: true, Branch: "main"}
	linked := repository.State{Path: filepath.Join(root, "topic"), CommonDir: primary.CommonDir, Branch: "topic"}
	plan := []cleanup.Result{{Path: primary.Path, State: primary, Action: cleanup.ActionKeep, Reason: "primary clone is protected"}, {Path: linked.Path, State: linked, Action: cleanup.ActionWouldRemove, Destructive: true, Reason: "eligible"}}
	cmd := newCleanCmdWithRunner(func(ctx context.Context, _ []cleanup.Target, apply bool) ([]cleanup.Result, error) {
		if !apply || !strings.Contains(out.String(), "including local changes") || !strings.Contains(out.String(), "policy:   aggressive") {
			t.Fatal("destructive warning/policy was not printed before runner")
		}
		progress.Report(ctx, progress.Event{Phase: "warning", Detail: "fixture incomplete observation"})
		progress.Report(ctx, progress.Event{Phase: "clean-plan", Value: plan})
		if strings.Contains(out.String(), "removed checkout") {
			t.Fatal("plan claims removal")
		}
		plan[1].Action = cleanup.ActionRemoved
		plan[1].Reason = "force-removed linked checkout and its local files; branch refs retained"
		progress.Report(ctx, progress.Event{Phase: "clean-result", Value: plan[1]})
		plan[0].Warnings = []string{"fixture incomplete observation"}
		return plan, nil
	})
	cmd.SetArgs([]string{"--root", root})
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "removed checkout; branch refs retained") != 1 || !strings.Contains(out.String(), "removed: 1 · pruned: 0 · kept: 1") || strings.Count(diag.String(), "fixture incomplete observation") != 1 {
		t.Fatalf("bad cleanup accounting/warnings: %s %s", out.String(), diag.String())
	}
	if !strings.Contains(out.String(), "../topic") {
		t.Fatal("sibling worktree should be relative to its named repository")
	}
}

func TestHumanCleanupPartialFailureNeverClaimsPendingActionsRemoved(t *testing.T) {
	var out, diag bytes.Buffer
	cmd := newCleanCmdWithRunner(func(context.Context, []cleanup.Target, bool) ([]cleanup.Result, error) {
		return []cleanup.Result{{Path: "/a", Action: cleanup.ActionRemoved}, {Path: "/b", Action: cleanup.ActionFailed}, {Path: "/c", Action: cleanup.ActionKeep, Reason: "cleanup stopped before remaining actions"}}, errors.New("fixture removal failed")
	})
	cmd.SetArgs([]string{"--root", t.TempDir()})
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	if reportExecution(cmd.ExecuteContext(t.Context()), &diag) != 3 {
		t.Fatal("partial error exit changed")
	}
	for _, want := range []string{"removed: 1 · pruned: 0 · kept: 1", "failed: 1", "cleanup stopped before remaining actions"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Done") {
		t.Fatal("partial failure claims done")
	}
}

func TestHumanTextEscapesControlsAndStatusKeepsSafetyEvidence(t *testing.T) {
	var out, diag bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	h := newHumanReport(cmd, false)
	s := repository.State{Path: "/src/repo\x1b[31m\nspoof", CommonDir: "/src/repo/.git", Primary: true, Branch: "topic\rspoof", InUseKnown: true, CwdInUseKnown: true, Locked: true, InUse: true, Problems: []string{"failed inspection"}}
	h.statusStates([]repository.State{s})
	for _, want := range []string{"inspection incomplete", "Git lock/operation marker", "open file/process reference", `\x1b`, `\n`, `\r`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "clean") || strings.ContainsAny(out.String(), "\x1b\r") {
		t.Fatal("status claimed clean or emitted terminal controls")
	}
}
