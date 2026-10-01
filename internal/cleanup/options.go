package cleanup

import (
	"fmt"
	"strings"
)

// Level controls which worktrees qualify for cleanup.
// Identity and PR failures block every level. Strict levels also require
// complete process visibility; aggressive keeps observed cwd matches and warns.
type Level string

const (
	Conservative Level = "conservative"
	Balanced     Level = "balanced"
	Aggressive   Level = "aggressive"
)

// Options controls cleanup eligibility. DiscardLocalChanges is a deprecated
// compatibility alias for aggressive cleanup, not a required acknowledgement.
type Options struct {
	Level               Level
	DiscardLocalChanges bool
}

// ParseLevel validates a cleanup level. Empty values use the aggressive default.
func ParseLevel(raw string) (Level, error) {
	level := Level(strings.ToLower(strings.TrimSpace(raw)))
	if level == "" {
		level = Aggressive
	}
	switch level {
	case Conservative, Balanced, Aggressive:
		return level, nil
	default:
		return "", fmt.Errorf("invalid cleanup level %q: choose conservative, balanced, or aggressive", raw)
	}
}

func (o Options) validate(_ bool) (Options, error) {
	level, err := ParseLevel(string(o.Level))
	if err != nil {
		return Options{}, err
	}
	o.Level = level
	if o.DiscardLocalChanges && level != Aggressive {
		return Options{}, fmt.Errorf("--discard-local-changes requires the aggressive cleanup level")
	}

	return o, nil
}
