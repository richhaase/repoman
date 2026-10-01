package syncer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// ReadExcludesFile reads one repository-name pattern per line. Blank lines and
// comments beginning with # after whitespace are ignored.
func ReadExcludesFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read excludes file: %w", err)
	}
	defer func() { _ = file.Close() }()
	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pattern := strings.TrimSpace(scanner.Text())
		if pattern != "" && !strings.HasPrefix(pattern, "#") {
			patterns = append(patterns, pattern)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read excludes file: %w", err)
	}
	return patterns, nil
}

// AuthenticatedOwner resolves the GitHub login used by the authenticated gh CLI.
func AuthenticatedOwner(ctx context.Context) (string, error) {
	return (&engine{command: runCommand}).authenticatedOwner(ctx)
}

func (e *engine) authenticatedOwner(ctx context.Context) (string, error) {
	output, err := e.command(ctx, "", "gh", "api", "--hostname", "github.com", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("resolve authenticated GitHub owner: %w", err)
	}
	owner := strings.TrimSpace(string(output))
	if !ownerPattern.MatchString(owner) {
		return "", fmt.Errorf("authenticated GitHub owner is not a valid login")
	}
	return owner, nil
}
