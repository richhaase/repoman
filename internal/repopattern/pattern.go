// Package repopattern matches repository-name globs shared by configuration,
// synchronization, and cleanup. It accepts Bash's common [!...] negation as
// well as Go's [^...] spelling without silently treating ! as a class member.
package repopattern

import (
	"fmt"
	"path"
	"strings"
)

// Match supports *, ?, ranges, bracket negation, and backslash escaping.
// Extended shell patterns and POSIX named classes are rejected explicitly.
func Match(pattern, name string) (bool, error) {
	var normalized strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		if ch == '\\' {
			normalized.WriteByte(ch)
			if i+1 < len(pattern) {
				i++
				normalized.WriteByte(pattern[i])
			}
			continue
		}
		if !inClass && strings.ContainsRune("@+?!*", rune(ch)) && i+1 < len(pattern) && pattern[i+1] == '(' {
			return false, fmt.Errorf("extended shell patterns are unsupported; use separate include/exclude patterns")
		}
		if ch == '[' && !inClass {
			inClass = true
			normalized.WriteByte(ch)
			if i+1 < len(pattern) && pattern[i+1] == '!' {
				i++
				normalized.WriteByte('^')
			}
			continue
		}
		if inClass && ch == '[' && i+1 < len(pattern) && strings.ContainsRune(":.=", rune(pattern[i+1])) {
			return false, fmt.Errorf("named or locale-dependent character classes are unsupported; use explicit character ranges")
		}
		if ch == ']' {
			inClass = false
		}
		normalized.WriteByte(ch)
	}
	return path.Match(normalized.String(), name)
}
