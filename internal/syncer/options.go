package syncer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// ParseFetchScope validates a fetch scope and resolves an empty value to origin.
func ParseFetchScope(scope string) (string, error) {
	if scope == "" {
		return "origin", nil
	}
	if scope != "origin" && scope != "all" {
		return "", fmt.Errorf("fetch scope must be origin or all, got %q", scope)
	}
	return scope, nil
}

func fetchOptions(target Target, options Options) (string, bool, error) {
	if _, err := ParseFetchScope(target.FetchScope); err != nil {
		return "", false, err
	}
	scope := options.FetchScope
	if scope == "" {
		scope = target.FetchScope
	}
	scope, err := ParseFetchScope(scope)
	if err != nil {
		return "", false, err
	}
	prune := target.Prune
	if options.Prune != nil {
		prune = *options.Prune
	}
	return scope, prune, nil
}

func fetchArgs(scope string, prune bool) []string {
	args := []string{"fetch"}
	if scope == "all" {
		args = append(args, "--all")
	} else {
		args = append(args, "--no-all")
	}
	if prune {
		args = append(args, "--prune")
	} else {
		args = append(args, "--no-prune")
	}
	// Tag-pruning config must not broaden branch pruning. Explicit configured
	// tag refspecs are still respected, as with ordinary git fetch.
	args = append(args, "--no-prune-tags", "--quiet")
	if scope == "origin" {
		// No branch refspec narrows the configured origin fetch refspecs.
		args = append(args, "origin")
	}
	return args
}

func fetchDescription(scope string, prune bool) string {
	description := "origin"
	if scope == "all" {
		description = "all remotes"
	}
	if prune {
		return description + " and prune"
	}
	return description + " without pruning"
}

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
