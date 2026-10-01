package syncer

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFetchScope(t *testing.T) {
	for _, tt := range []struct {
		input, want string
	}{
		{"", "origin"}, {"origin", "origin"}, {"all", "all"},
		{"ALL", ""}, {" origin", ""}, {"secondary", ""}, {"--all", ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseFetchScope(tt.input)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("ParseFetchScope(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
			}
		})
	}
}

func TestInvalidFetchScopeStopsBeforeCommands(t *testing.T) {
	for _, tt := range []struct {
		name    string
		target  Target
		options Options
	}{
		{"target", Target{FetchScope: "secondary"}, Options{}},
		{"override", Target{}, Options{FetchScope: "secondary"}},
		{"invalid-target-with-override", Target{FetchScope: "secondary"}, Options{FetchScope: "origin"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := engine{command: func(context.Context, string, string, ...string) ([]byte, error) {
				t.Fatal("invalid fetch scope reached a command")
				return nil, nil
			}}
			if _, err := e.runWithOptions(context.Background(), tt.target, tt.options); err == nil || !strings.Contains(err.Error(), "fetch scope") {
				t.Fatalf("expected fetch scope error, got %v", err)
			}
		})
	}
}

func TestRealGitFetchPolicies(t *testing.T) {
	enabled, disabled := true, false
	for _, tt := range []struct {
		name         string
		target       Target
		options      Options
		wantAll      bool
		wantPrune    bool
		ambientPrune bool
	}{
		{name: "defaults-ignore-ambient", ambientPrune: true},
		{name: "all-does-not-enable-prune", options: Options{FetchScope: "all"}, wantAll: true, ambientPrune: true},
		{name: "prune-does-not-enable-all", options: Options{Prune: &enabled}, wantPrune: true},
		{name: "explicit-all-and-prune", options: Options{FetchScope: "all", Prune: &enabled}, wantAll: true, wantPrune: true},
		{name: "target-all-only", target: Target{FetchScope: "all"}, wantAll: true, ambientPrune: true},
		{name: "target-prune-only", target: Target{Prune: true}, wantPrune: true},
		{name: "target-all-and-prune", target: Target{FetchScope: "all", Prune: true}, wantAll: true, wantPrune: true},
		{name: "override-both-off", target: Target{FetchScope: "all", Prune: true}, options: Options{FetchScope: "origin", Prune: &disabled}, ambientPrune: true},
		{name: "override-both-on", target: Target{FetchScope: "origin"}, options: Options{FetchScope: "all", Prune: &enabled}, wantAll: true, wantPrune: true},
		{name: "override-scope-preserves-target-prune", target: Target{FetchScope: "all", Prune: true}, options: Options{FetchScope: "origin"}, wantPrune: true},
		{name: "override-prune-preserves-target-scope", target: Target{FetchScope: "all", Prune: true}, options: Options{Prune: &disabled}, wantAll: true, ambientPrune: true},
		{name: "force-does-not-enable-all-or-prune", options: Options{Force: true}, ambientPrune: true},
		{name: "cleanup-does-not-enable-all-or-prune", options: Options{Cleanup: true}, ambientPrune: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitFixture(t)
			// Global and per-remote settings deliberately disagree with the
			// requested branch-prune behavior. Both tag-prune settings are on.
			pruneValue := "false"
			if tt.ambientPrune {
				pruneValue = "true"
			}
			f.write(f.root, filepath.Base(f.globalConfig), "[fetch]\nall = true\nprune = "+pruneValue+"\npruneTags = true\n")
			for _, remote := range []string{"origin", "secondary"} {
				f.git(f.dest, "config", "remote."+remote+".prune", pruneValue)
				f.git(f.dest, "config", "remote."+remote+".pruneTags", "true")
			}
			target := tt.target
			target.Dir, target.Owner, target.Days = f.clones, "alice", 45
			previewOptions := tt.options
			previewOptions.DryRun = true
			preview, err := f.engine().runWithOptions(context.Background(), target, previewOptions)
			wantDescription := "origin"
			if tt.wantAll {
				wantDescription = "all remotes"
			}
			if tt.wantPrune {
				wantDescription += " and prune"
			} else {
				wantDescription += " without pruning"
			}
			if err != nil || len(preview) != 1 || !strings.Contains(preview[0].Reason, "would fetch "+wantDescription) {
				t.Fatalf("preview=%+v err=%v, want fetch %s", preview, err, wantDescription)
			}
			if f.fetches != 0 || f.git(f.dest, "rev-parse", "HEAD") != f.initial || f.git(f.dest, "rev-parse", "refs/remotes/origin/main") != f.initial {
				t.Fatal("dry run changed HEAD or fetched refs")
			}
			results, err := f.engine().runWithOptions(context.Background(), target, tt.options)
			wantAction := "updated"
			if tt.options.Force {
				wantAction = "forced"
			}
			if err != nil || len(results) != 1 || results[0].Action != wantAction {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			wantArgs := []string{"fetch", "--no-all", "--no-prune", "--no-prune-tags", "--quiet"}
			if tt.wantAll {
				wantArgs[1] = "--all"
			} else {
				wantArgs = append(wantArgs, "origin")
			}
			if tt.wantPrune {
				wantArgs[2] = "--prune"
			}
			if f.fetches != 1 || !reflect.DeepEqual(f.fetchCommands[0], wantArgs) {
				t.Fatalf("fetches=%v, want %v", f.fetchCommands, wantArgs)
			}
			for _, ref := range []string{"HEAD", "refs/remotes/origin/main", "refs/remotes/origin/feature", "refs/tags/remote-tag"} {
				if got := f.git(f.dest, "rev-parse", ref); got != f.remote {
					t.Fatalf("%s=%s, want %s", ref, got, f.remote)
				}
			}
			secondaryHead := f.initial
			if tt.wantAll {
				secondaryHead = f.remote
			}
			if got := f.git(f.dest, "rev-parse", "refs/remotes/secondary/main"); got != secondaryHead {
				t.Fatalf("secondary/main=%s, want %s", got, secondaryHead)
			}
			assertFixtureRef(t, f, "refs/remotes/secondary/feature", tt.wantAll, f.remote)
			assertFixtureRef(t, f, "refs/remotes/origin/obsolete", !tt.wantPrune, f.initial)
			assertFixtureRef(t, f, "refs/remotes/secondary/obsolete", !tt.wantAll || !tt.wantPrune, f.initial)
			assertFixtureRef(t, f, "refs/tags/local-only", true, f.initial)
		})
	}
}

func assertFixtureRef(t *testing.T, f *gitFixture, ref string, exists bool, want string) {
	t.Helper()
	_, err := f.command(context.Background(), f.dest, "git", "show-ref", "--verify", "--quiet", ref)
	if !exists {
		if !commandExitOne(err) {
			t.Fatalf("%s should be absent, got %v", ref, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s should exist: %v", ref, err)
	}
	if got := f.git(f.dest, "rev-parse", ref); got != want {
		t.Fatalf("%s=%s, want %s", ref, got, want)
	}
}

func TestRealGitDefaultIgnoresUnavailableSecondary(t *testing.T) {
	for _, scope := range []string{"origin", "all"} {
		t.Run(scope, func(t *testing.T) {
			f := newGitFixture(t)
			f.git(f.dest, "remote", "set-url", "secondary", filepath.Join(f.root, "unavailable.git"))
			f.write(f.root, filepath.Base(f.globalConfig), "[fetch]\nall = true\nprune = true\n")
			results, err := f.run(Options{FetchScope: scope})
			wantHead := f.remote
			if scope == "origin" {
				if err != nil || len(results) != 1 || results[0].Action != "updated" {
					t.Fatalf("default fetch contacted unavailable remote: results=%+v err=%v", results, err)
				}
			} else {
				wantHead = f.initial
				if err == nil || len(results) != 1 || results[0].Action != "error" || !strings.Contains(err.Error(), "fetch all remotes") {
					t.Fatalf("all-remotes fetch ignored unavailable remote: results=%+v err=%v", results, err)
				}
			}
			if got := f.git(f.dest, "rev-parse", "HEAD"); got != wantHead {
				t.Fatalf("HEAD=%s, want %s", got, wantHead)
			}
			assertFixtureRef(t, f, "refs/remotes/secondary/main", true, f.initial)
			assertFixtureRef(t, f, "refs/remotes/origin/obsolete", true, f.initial)
		})
	}
}
