package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/richhaase/repoman/internal/syncer"
)

func TestRegisterEmptyAndRemoveLast(t *testing.T) {
	for _, initial := range []string{"missing", "", " \n", `{"targets":[]}`} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if initial != "missing" {
				writeConfig(t, path, initial)
			}
			targetDir := filepath.Join(t.TempDir(), "not-created")
			c, err := Register(path, syncer.Target{Dir: targetDir, Owner: "me", Days: 45})
			if err != nil || len(c.Targets) != 1 {
				t.Fatalf("Register=%+v,%v", c, err)
			}
			if c.Targets[0].Events == nil || *c.Targets[0].Events {
				t.Fatalf("new registrations must explicitly disable events: %+v", c)
			}
			if _, err = os.Stat(targetDir); !os.IsNotExist(err) {
				t.Fatalf("registration created the target directory: %v", err)
			}
			if _, err = Load(path); err != nil {
				t.Fatal(err)
			}
			c, err = Remove(path, targetDir)
			if err != nil || c.Targets == nil || len(c.Targets) != 0 {
				t.Fatalf("Remove=%+v,%v", c, err)
			}
			stored, err := List(path)
			if err != nil || stored.Targets == nil || len(stored.Targets) != 0 {
				t.Fatalf("List=%+v,%v", stored, err)
			}
			if _, err = Load(path); err == nil {
				t.Fatal("operational config load must still reject no targets")
			}
		})
	}
}

func TestRegisterPreservesUntouchedValuesAndReplacementOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HOME", t.TempDir())
	writeConfig(t, path, `{"targets":[{"dir":"~/first","owner":"old","days":10,"events":true,"includes":["old*"],"excludes":["skip*"],"cleanup_level":"balanced","fetch_scope":"all","prune":true},{"dir":"~/second","owner":"another","days":0,"events":true,"includes":["a*"],"excludes":["b*"],"cleanup_level":"aggressive","fetch_scope":"origin","prune":true}]}`)
	c, err := Register(path, syncer.Target{Dir: "~/first", Owner: "new", Days: 45, Includes: []string{"new*"}})
	if err != nil {
		t.Fatal(err)
	}
	untouched, replaced := c.Targets[0], c.Targets[1]
	if untouched.Dir != "~/second" || untouched.Days != 0 || untouched.Owner != "another" || untouched.Events == nil || !*untouched.Events || untouched.CleanupLevel != "aggressive" || untouched.FetchScope != "origin" || !untouched.Prune || strings.Join(untouched.Includes, ",") != "a*" || strings.Join(untouched.Excludes, ",") != "b*" {
		t.Fatalf("untouched target changed: %+v", untouched)
	}
	if replaced.Dir != filepath.Join(os.Getenv("HOME"), "first") || replaced.Owner != "new" || replaced.Days != 45 || replaced.CleanupLevel != "balanced" || replaced.FetchScope != "all" || !replaced.Prune || replaced.Events == nil || *replaced.Events || len(replaced.Excludes) != 0 || strings.Join(replaced.Includes, ",") != "new*" {
		t.Fatalf("replacement=%+v", replaced)
	}
	c, err = Register(path, syncer.Target{Dir: "~/first", Owner: "new", Days: 45, CleanupLevel: "conservative"})
	if err != nil || c.Targets[1].CleanupLevel != "conservative" {
		t.Fatalf("explicit cleanup override=%+v,%v", c, err)
	}
}

func TestRegisterExplicitFetchOverrides(t *testing.T) {
	path, dir := filepath.Join(t.TempDir(), "config.json"), t.TempDir()
	target := syncer.Target{Dir: dir, Owner: "me", Days: 45, FetchScope: "all", Prune: true}
	if _, err := Register(path, target); err != nil {
		t.Fatal(err)
	}
	noPrune := false
	target.FetchScope, target.Prune = "origin", false
	c, err := RegisterWithOptions(path, target, RegisterOptions{Prune: &noPrune})
	if err != nil || c.Targets[0].FetchScope != "origin" || c.Targets[0].Prune {
		t.Fatalf("explicit false override=%+v,%v", c, err)
	}
	target.Prune = true
	c, err = Register(path, target)
	if err != nil || !c.Targets[0].Prune {
		t.Fatalf("explicit true target=%+v,%v", c, err)
	}
}

func TestConfigMutationRejectsInvalidWithoutLosingData(t *testing.T) {
	for _, initial := range []string{
		`{"future":true,"targets":[]}`,
		`{"targets":[{"dir":"src","owner":"me","future":true}]}`,
		`{"targets":[{"dir":"src","cleanup_level":"reckless"}]}`,
		`{"targets":[{"dir":"src","fetch_scope":"invalid"}]}`,
		`{"targets":[{"dir":"src"},{"dir":"./src"}]}`,
		`{"targets":[]} {}`,
		`null`,
		`broken`,
	} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeConfig(t, path, initial)
			if _, err := Register(path, syncer.Target{Dir: t.TempDir(), Owner: "me", Days: 45}); err == nil {
				t.Fatal("Register accepted invalid existing config")
			}
			if _, err := Remove(path, "src"); err == nil {
				t.Fatal("Remove accepted invalid existing config")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != initial {
				t.Fatalf("file changed: %q, %v", data, err)
			}
		})
	}
}

func TestRemoveLeavesDirectoryAndRejectsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	dir := t.TempDir()
	marker := filepath.Join(dir, "keep")
	writeConfig(t, marker, "keep me")
	if _, err := Register(path, syncer.Target{Dir: dir, Owner: "me", Days: 45}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := Remove(path, t.TempDir()); err == nil {
		t.Fatal("Remove accepted an unknown directory")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("unknown removal changed config")
	}
	if _, err := Remove(path, dir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me" {
		t.Fatalf("directory contents changed: %q,%v", data, err)
	}
}

func TestRemoveMissingConfigDoesNotCreateStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "config.json")
	if _, err := Remove(path, t.TempDir()); err == nil {
		t.Fatal("missing config removal succeeded")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("removal created configuration storage: %v", err)
	}
}

func TestRegisterRejectsInvalidTargetDirectories(t *testing.T) {
	for name, dir := range map[string]string{
		"NUL":             "bad\x00dir",
		"newline":         "bad\ndir",
		"carriage return": "bad\rdir",
		"before cleaning": "bad\ndir/../valid",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			initial := `{"targets":[]}`
			writeConfig(t, path, initial)
			if _, err := Register(path, syncer.Target{Dir: dir, Owner: "me", Days: 45}); err == nil {
				t.Fatal("Register accepted an invalid directory")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != initial {
				t.Fatalf("invalid target changed config: %q, %v", data, err)
			}
		})
	}
}

func TestConfigFileModesAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and unprivileged symlinks")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	target := syncer.Target{Dir: t.TempDir(), Owner: "me", Days: 45}
	if _, err := Register(path, target); err != nil {
		t.Fatal(err)
	}
	assertMode := func(want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("mode=%v,%v; want %v", info, err, want)
		}
	}
	assertMode(0600)
	if err := os.Chmod(path, 0640); err != nil { // #nosec G302 -- fixture verifies existing group-read permissions are preserved.
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	target.Owner = "another"
	if _, err := Register(link, target); err != nil {
		t.Fatal(err)
	}
	assertMode(0640)
	if destination, err := os.Readlink(link); err != nil || destination != path {
		t.Fatalf("symlink replaced: %q,%v", destination, err)
	}
	c, err := Load(path)
	if err != nil || c.Targets[0].Owner != "another" {
		t.Fatalf("destination not updated: %+v,%v", c, err)
	}
	if _, err = Remove(link, target.Dir); err != nil {
		t.Fatal(err)
	}
	assertMode(0640)
	if _, err = os.Readlink(link); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentConfigUpdatesAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"targets":[]}`)
	writeConfig(t, path+".lock", "")
	if _, err := Register(path, syncer.Target{Dir: t.TempDir(), Owner: "me", Days: 45}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("concurrent update not refused: %v", err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	external := `{"targets":[{"dir":"external","owner":"other"}]}`
	_, err := update(path, func(c Config) (Config, error) {
		writeConfig(t, path, external)
		return c, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("external change not detected: %v", err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != external {
		t.Fatalf("external update overwritten: %q,%v", data, readErr)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("temporary files leaked: %+v,%v", entries, err)
	}
}

func TestConfigJSONPreservesReadableCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	target := syncer.Target{Dir: filepath.Join(t.TempDir(), "a&b"), Owner: "me", Days: 45, Excludes: []string{"a&b"}}
	if _, err := Register(path, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) || bytes.Contains(data, []byte(`\u0026`)) {
		t.Fatalf("config=%s, %v", data, err)
	}
}

func writeConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
