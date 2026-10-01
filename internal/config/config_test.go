package config

import (
	"os"
	"path/filepath"
	"testing"
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
		{"pattern", `{"targets":[{"dir":"/tmp","includes":["["]}]}`, true},
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
func TestNormalize(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, e := Normalize("~/src")
	if e != nil || p != filepath.Join(os.Getenv("HOME"), "src") {
		t.Fatalf("Normalize=%q,%v", p, e)
	}
}
func TestDefaultPath(t *testing.T) {
	t.Setenv("REPOMAN_CONFIG", "custom.json")
	if DefaultPath() != "custom.json" {
		t.Fatal(DefaultPath())
	}
}
