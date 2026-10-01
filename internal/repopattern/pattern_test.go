package repopattern

import "testing"

func TestMatch(t *testing.T) {
	for _, tt := range []struct {
		pattern, name string
		want          bool
	}{
		{"service-*", "service-api", true},
		{"service-?", "service-ab", false},
		{"[!x]*", "alpha", true},
		{"[!x]*", "xray", false},
		{"[!a-c]*", "beta", false},
		{"[!a-c]*", "delta", true},
		{"[^x]*", "alpha", true},
		{"[^x]*", "xray", false},
		{"[a!]*", "alpha", true},
		{"[a!]*", "beta", false},
		{`\[!x\]`, "[!x]", true},
		{`[\!a]*`, "!literal", true},
		{`\@(one|two)`, "@(one|two)", true},
		{"*.[ch]", "main.go", false},
		{"*.[ch]", "main.c", true},
	} {
		t.Run(tt.pattern+"/"+tt.name, func(t *testing.T) {
			got, err := Match(tt.pattern, tt.name)
			if err != nil || got != tt.want {
				t.Fatalf("Match(%q, %q) = %v, %v; want %v", tt.pattern, tt.name, got, err, tt.want)
			}
		})
	}
}

func TestUnsupportedPatternsFailClearly(t *testing.T) {
	for _, pattern := range []string{"@(one|two)", "!(private-*)", "?(a)", "*(a)", "+(a)", "[[:alpha:]]*", "[![:digit:]]*", "[", "[!", `trailing\`} {
		t.Run(pattern, func(t *testing.T) {
			if _, err := Match(pattern, ""); err == nil {
				t.Fatalf("invalid or unsupported pattern %q accepted", pattern)
			}
		})
	}
}
