package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/richhaase/repoman/internal/syncer"
)

func syncCLIFixture(t *testing.T, names ...string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell GitHub fixture")
	}
	root, err := os.MkdirTemp("/tmp", "repoman-sync-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []map[string]any{}
	for _, name := range names {
		nodes = append(nodes, map[string]any{"name": name, "nameWithOwner": "alice/" + name, "pushedAt": time.Now().UTC().Format(time.RFC3339), "isArchived": false, "defaultBranchRef": map[string]string{"name": "main"}})
	}
	listing, _ := json.Marshal(map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{"repositories": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}})
	response := filepath.Join(root, "response.json")
	if err := os.WriteFile(response, listing, 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "gh.log")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REPOMAN_TEST_GH_LOG\"\nif [ \"$4\" = user ]; then printf 'alice\\n'; else cat \"$REPOMAN_TEST_GH_RESPONSE\"; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- executable fake gh inside a private temporary fixture.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REPOMAN_TEST_GH_LOG", log)
	t.Setenv("REPOMAN_TEST_GH_RESPONSE", response)
	return root, log
}

func TestSyncScriptFlagsOwnerFallbackAndExcludesFile(t *testing.T) {
	root, log := syncCLIFixture(t, "keep", "skip-file", "skip-flag")
	patterns := filepath.Join(root, "excludes")
	if err := os.WriteFile(patterns, []byte(" # comment\n\n skip-file \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "new-clones")
	out, diag, err := executeCommand(t, t.Context(), "sync", "-t", target, "-d", "30", "-n", "-f", "-c", "--include", "*", "-e", "skip-flag", "--excludes-file", patterns, "--json")
	if err != nil || diag != "" {
		t.Fatalf("out=%s diag=%s err=%v", out, diag, err)
	}
	var report struct {
		SchemaVersion int             `json:"schema_version"`
		DryRun        bool            `json:"dry_run"`
		Items         []syncer.Result `json:"items"`
		Errors        []string        `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || !report.DryRun || len(report.Errors) != 0 || len(report.Items) != 3 {
		t.Fatalf("report=%+v", report)
	}
	for _, item := range report.Items {
		if item.Name == "keep" {
			if item.Action != "would-clone" {
				t.Fatalf("%+v", item)
			}
		} else if item.Reason != "excluded" {
			t.Fatalf("%+v", item)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created target: %v", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(calls), "api --hostname github.com user --jq .login") {
		t.Fatalf("calls=%s err=%v", calls, err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, t.Context(), "sync", "-t", target, "-o", "alice", "-n"); err != nil {
		t.Fatal(err)
	}
	calls, err = os.ReadFile(log)
	if err != nil || strings.Contains(string(calls), "user --jq") {
		t.Fatalf("explicit owner fallback calls=%s err=%v", calls, err)
	}
}

func TestSyncLegacyEventsNoticeOnceAndJSONUnchanged(t *testing.T) {
	root, _ := syncCLIFixture(t)
	configPath := filepath.Join(root, "config.json")
	config := map[string]any{"targets": []any{
		map[string]any{"dir": filepath.Join(root, "one"), "owner": "alice", "events": true},
		map[string]any{"dir": filepath.Join(root, "two"), "owner": "alice", "events": true},
	}}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		args := []string{"sync", "--config", configPath, "--json", "--dry-run"}
		if explicit {
			args = append(args, "--no-events")
		}
		out, diag, err := executeCommand(t, t.Context(), args...)
		if err != nil || !json.Valid([]byte(out)) {
			t.Fatalf("out=%s diag=%s err=%v", out, diag, err)
		}
		wantNotices := 1
		if explicit {
			wantNotices = 0
		}
		if got := strings.Count(diag, "legacy events=true"); got != wantNotices {
			t.Fatalf("notices=%d diag=%q", got, diag)
		}
	}
}

func TestSyncUnreadableExcludesFileIsUsageError(t *testing.T) {
	root, log := syncCLIFixture(t)
	_, _, err := executeCommand(t, t.Context(), "sync", "-t", filepath.Join(root, "clones"), "--excludes-file", filepath.Join(root, "missing"), "-n")
	var coded *ExitError
	if !errors.As(err, &coded) || coded.Code != 2 {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub called before file validation: %v", err)
	}
}
