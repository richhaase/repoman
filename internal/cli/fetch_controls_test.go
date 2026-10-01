package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/config"
	"github.com/richhaase/repoman/internal/syncer"
)

func fetchConfig(t *testing.T, targets ...syncer.Target) string {
	t.Helper()
	for i := range targets {
		if targets[i].Dir == "" {
			targets[i].Dir = t.TempDir()
		}
	}
	data, err := json.Marshal(config.Config{Targets: targets})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncFetchPolicyPrecedence(t *testing.T) {
	for _, test := range []struct {
		name       string
		target     syncer.Target
		flags      []string
		scope      string
		prune      bool
		force      bool
		cleanup    bool
		pruneSet   bool
		fetchScope string
	}{
		{name: "legacy defaults", scope: "origin"},
		{name: "configured all and prune", target: syncer.Target{FetchScope: "all", Prune: true}, scope: "all", prune: true},
		{name: "configured origin without prune", target: syncer.Target{FetchScope: "origin"}, scope: "origin"},
		{name: "scope override preserves pruning", target: syncer.Target{FetchScope: "all", Prune: true}, flags: []string{"--fetch-scope", "origin"}, scope: "origin", prune: true, fetchScope: "origin"},
		{name: "scope all leaves pruning off", flags: []string{"--fetch-scope", "all"}, scope: "all", fetchScope: "all"},
		{name: "enable prune preserves scope", target: syncer.Target{FetchScope: "all"}, flags: []string{"--prune"}, scope: "all", prune: true, pruneSet: true},
		{name: "explicit false overrides true", target: syncer.Target{FetchScope: "all", Prune: true}, flags: []string{"--prune=false"}, scope: "all", pruneSet: true},
		{name: "both overrides", target: syncer.Target{FetchScope: "all", Prune: true}, flags: []string{"--fetch-scope=origin", "--prune=false"}, scope: "origin", pruneSet: true, fetchScope: "origin"},
		{name: "force remains separate", flags: []string{"--force"}, scope: "origin", force: true},
		{name: "cleanup remains separate", flags: []string{"--cleanup"}, scope: "origin", cleanup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := fetchConfig(t, test.target)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			cmd := newSyncCmdWithRunner(func(_ context.Context, target syncer.Target, options syncer.Options) ([]syncer.Result, error) {
				calls++
				if target.FetchScope != test.target.FetchScope || target.Prune != test.target.Prune {
					t.Fatalf("stored controls changed before runner: %+v", target)
				}
				if options.FetchScope != test.fetchScope || (options.Prune != nil) != test.pruneSet || (options.Prune != nil && *options.Prune != test.prune) || options.Force != test.force || options.Cleanup != test.cleanup || !options.DryRun {
					t.Fatalf("runner options=%+v", options)
				}
				return []syncer.Result{}, nil
			})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{"--config", path, "--dry-run", "--json"}, test.flags...))
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			var report envelope
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(report.SyncPolicies) != 1 || report.SchemaVersion != 1 || !report.DryRun {
				t.Fatalf("calls=%d report=%s", calls, out.String())
			}
			policy := report.SyncPolicies[0]
			if policy.FetchScope != test.scope || policy.Prune != test.prune || policy.Force != test.force || policy.Cleanup != test.cleanup {
				t.Fatalf("policy=%+v", policy)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("sync rewrote config: %s, %v", after, err)
			}
		})
	}
}

func TestSyncMixedFetchPolicies(t *testing.T) {
	root, _ := syncCLIFixture(t)
	path := fetchConfig(t,
		syncer.Target{Dir: filepath.Join(root, "one"), Owner: "alice", FetchScope: "all", Prune: true},
		syncer.Target{Dir: filepath.Join(root, "two"), Owner: "alice", FetchScope: "origin"},
		syncer.Target{Dir: filepath.Join(root, "three"), Owner: "alice"},
	)
	for _, override := range []bool{false, true} {
		args := []string{"sync", "--config", path, "--json", "--dry-run"}
		if override {
			args = append(args, "--fetch-scope=origin", "--prune=false")
		}
		out, diag, err := executeCommand(t, t.Context(), args...)
		if err != nil || diag != "" {
			t.Fatalf("out=%s diag=%s err=%v", out, diag, err)
		}
		var report envelope
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		want := []syncPolicy{
			{Root: filepath.Join(root, "one"), FetchScope: "all", Prune: true},
			{Root: filepath.Join(root, "two"), FetchScope: "origin"},
			{Root: filepath.Join(root, "three"), FetchScope: "origin"},
		}
		if override {
			want[0].FetchScope = "origin"
			want[0].Prune = false
		}
		if !reflect.DeepEqual(report.SyncPolicies, want) {
			t.Fatalf("policies=%+v want=%+v", report.SyncPolicies, want)
		}
	}
}

func TestInvalidFetchScopeBeforeAPIOrTargetInspection(t *testing.T) {
	root, log := syncCLIFixture(t)
	for _, value := range []string{"other", "", " ", "ALL"} {
		for _, operation := range []string{"sync", "config add"} {
			t.Run(operation+" "+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "absent", "config.json")
				args := []string{"sync", "--root", filepath.Join(root, "missing"), "--fetch-scope", value, "--config", path}
				if operation == "config add" {
					args = []string{"config", "add", filepath.Join(root, "missing"), "--fetch-scope", value, "--config", path}
				}
				_, _, err := executeCommand(t, t.Context(), args...)
				var coded *ExitError
				if !errors.As(err, &coded) || coded.Code != 2 || !strings.Contains(err.Error(), "scope") {
					t.Fatalf("error=%v", err)
				}
				if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("GitHub called before validation: %v", err)
				}
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid flags created config storage: %v", err)
				}
			})
		}
	}
}

func TestInvalidStoredFetchScopePreflightsAllTargets(t *testing.T) {
	path := fetchConfig(t, syncer.Target{FetchScope: "origin"}, syncer.Target{FetchScope: "other"})
	called := false
	cmd := newSyncCmdWithRunner(func(context.Context, syncer.Target, syncer.Options) ([]syncer.Result, error) {
		called = true
		return nil, nil
	})
	cmd.SetArgs([]string{"--config", path, "--fetch-scope", "origin"})
	err := cmd.ExecuteContext(t.Context())
	var coded *ExitError
	if called || !errors.As(err, &coded) || coded.Code != 2 || !strings.Contains(err.Error(), "fetch_scope") {
		t.Fatalf("called=%t err=%v", called, err)
	}
}

func TestSyncHumanEffectiveFetchControlsAndFlagIsolation(t *testing.T) {
	root, _ := syncCLIFixture(t)
	for _, override := range []bool{true, false} {
		args := []string{"sync", "--root", filepath.Join(root, "clones"), "--owner", "alice", "--dry-run"}
		want := "fetch:    origin · prune off\nforce:    off\ncleanup:  off"
		if override {
			args = append(args, "--fetch-scope", "all", "--prune", "--force", "--cleanup")
			want = "fetch:    all · prune on\nforce:    on\ncleanup:  on"
		}
		out, diag, err := executeCommand(t, t.Context(), args...)
		if err != nil || diag != "" || !strings.Contains(out, want) || !strings.Contains(out, "No repositories found") {
			t.Fatalf("out=%s diag=%s err=%v", out, diag, err)
		}
	}
}
