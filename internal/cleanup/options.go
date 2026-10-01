package cleanup

import (
	"fmt"
	"strings"
)

// Level controls which positively established states qualify for cleanup.
// Unknown state is protected at every level.
type Level string

const (
	Conservative Level = "conservative"
	Balanced     Level = "balanced"
	Aggressive   Level = "aggressive"
)

// Options controls cleanup eligibility. DiscardLocalChanges is an invocation-only
// acknowledgement: persisted configuration must never enable it.
type Options struct {
	Level               Level
	DiscardLocalChanges bool
}

// ParseLevel validates a cleanup level. Empty values use the conservative default.
func ParseLevel(raw string) (Level, error) {
	level := Level(strings.ToLower(strings.TrimSpace(raw)))
	if level == "" {
		level = Conservative
	}
	switch level {
	case Conservative, Balanced, Aggressive:
		return level, nil
	default:
		return "", fmt.Errorf("invalid cleanup level %q: choose conservative, balanced, or aggressive", raw)
	}
}

func (o Options) validate(apply bool) (Options, error) {
	level, err := ParseLevel(string(o.Level))
	if err != nil {
		return Options{}, err
	}
	o.Level = level
	if o.DiscardLocalChanges && level != Aggressive {
		return Options{}, fmt.Errorf("--discard-local-changes requires the aggressive cleanup level")
	}
	if apply && level == Aggressive && !o.DiscardLocalChanges {
		return Options{}, fmt.Errorf("applying aggressive cleanup requires --discard-local-changes; modified, staged, untracked, and ignored files may be permanently deleted")
	}
	return o, nil
}
