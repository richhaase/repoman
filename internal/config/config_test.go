package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/richhaase/repoman/internal/syncer"
)

func TestLoad(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		bad         bool
	}{
		{"valid", `{"targets":[{"dir":"~/src","owner":"me","events":false}]}`, false},
		{"legacy", `{"targets":[{"dir":"~/src","owner":"me","days":45,"events":true,"includes":["a*"],"excludes":[]}]}`, false},
		{"empty", `{"targets":[]}`, true},
		{"unknown", `{"targets":[{"dir":"/tmp","owenr":"me"}]}`, true},
		{"trailing", `{"targets":[]} {}`, true},
		{"duplicate", `{"targets":[{"dir":"/tmp/a"},{"dir":"/tmp/a/../a"}]}`, true},
		{"negative", `{"targets":[{"dir":"/tmp","days":-1}]}`, true},
		{"cleanup level", `{"targets":[{"dir":"/tmp","cleanup_level":"aggressive"}]}`, false},
		{"invalid cleanup level", `{"targets":[{"dir":"/tmp","cleanup_level":"reckless"}]}`, true},
		{"cannot persist discard", `{"targets":[{"dir":"/tmp","cleanup_level":"aggressive","discard_local_changes":true}]}`, true},
		{"pattern", `{"targets":[{"dir":"/tmp","includes":["["]}]}`, true},
		{"NUL directory", `{"targets":[{"dir":"/tmp/bad\u0000dir"}]}`, true},
		{"newline directory", `{"targets":[{"dir":"/tmp/bad\ndir"}]}`, true},
		{"carriage return directory", `{"targets":[{"dir":"/tmp/bad\rdir"}]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			if e := os.WriteFile(p, []byte(tt.input), 0600); e != nil {
				t.Fatal(e)
			}
			c, e := Load(p)
			if (e != nil) != tt.bad {
				t.Fatalf("Load=%+v,%v", c, e)
			}
			if !tt.bad && (c.Targets[0].Days != 45 || !filepath.IsAbs(c.Targets[0].Dir)) {
				t.Fatalf("defaults=%+v", c)
			}
		})
	}
}
func TestLoadSyncReposConfigUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The original script's targets schema needs no conversion, including its
	// events=true default and manually configured include patterns.
	data := []byte(`{
  "targets": [
    {
      "dir": "~/src",
      "owner": "me",
      "days": 45,
      "events": true,
      "includes": ["app-*"],
      "excludes": ["prototype-*"]
    },
    {
      "dir": "~/work",
      "owner": "team",
      "days": 7,
      "events": false,
      "includes": [],
      "excludes": ["archived-*"]
    }
  ]
}
`)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	events, noEvents := true, false
	want := Config{Targets: []syncer.Target{
		{Dir: filepath.Join(home, "src"), Owner: "me", Days: 45, Events: &events, Includes: []string{"app-*"}, Excludes: []string{"prototype-*"}},
		{Dir: filepath.Join(home, "work"), Owner: "team", Days: 7, Events: &noEvents, Includes: []string{}, Excludes: []string{"archived-*"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, data) {
		t.Fatal("loading configuration changed its file contents")
	}
}
func TestNormalize(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, e := Normalize("~/src")
	if e != nil || p != filepath.Join(os.Getenv("HOME"), "src") {
		t.Fatalf("Normalize=%q,%v", p, e)
	}
}
func TestDefaultPath(t *testing.T) {
	home, xdg := t.TempDir(), t.TempDir()
	for _, tt := range []struct {
		name, xdg, override, want string
		unsetXDG                  bool
	}{
		{name: "absolute XDG", xdg: xdg, want: filepath.Join(xdg, "repoman", "config.json")},
		{name: "unset XDG", unsetXDG: true, want: filepath.Join(home, ".config", "repoman", "config.json")},
		{name: "empty XDG", want: filepath.Join(home, ".config", "repoman", "config.json")},
		{name: "relative XDG", xdg: "relative/config", want: filepath.Join(home, ".config", "repoman", "config.json")},
		{name: "tilde XDG is relative", xdg: "~/.config", want: filepath.Join(home, ".config", "repoman", "config.json")},
		{name: "relative override", xdg: xdg, override: "custom.json", want: "custom.json"},
		{name: "absolute override", xdg: xdg, override: filepath.Join(home, "custom.json"), want: filepath.Join(home, "custom.json")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("REPOMAN_CONFIG", tt.override)
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			if tt.unsetXDG {
				if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			if got := DefaultPath(); got != tt.want {
				t.Fatalf("DefaultPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
