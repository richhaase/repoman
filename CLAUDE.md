# CLAUDE.md

This file provides guidance for AI assistants working with this codebase.

## Project Overview

This is a Go CLI application built with Cobra. It follows standard Go project layout conventions.

## Build & Test Commands

```bash
make build             # Build with version info to bin/
make install           # Install to GOBIN
make test              # Run tests
make lint              # Run golangci-lint (pinned version)
make check             # Run all quality checks (fmt-check, vet, lint, test) — non-mutating
make fmt               # Format code (rewrites files)
make vuln              # Run govulncheck
make release-snapshot  # Verify GoReleaser config locally (no publish)
make deps-update       # Update all dependencies to latest
make clean             # Clean build artifacts
```

## Project Structure

```
.
├── cmd/mycli/          # Main entry point (thin wrapper)
│   └── main.go         # Version injection, signal handling, calls cli.Execute()
├── internal/
│   ├── cli/            # Cobra command definitions
│   │   ├── root.go     # Root command, Execute(), slog setup
│   │   ├── version.go  # Version command
│   │   └── *.go        # Additional commands
│   ├── domain/         # Core business logic (no external deps)
│   ├── config/         # Configuration loading
│   └── terminal/       # TTY detection helpers
├── scripts/            # Bootstrap tooling (setup.sh)
└── .github/workflows/  # CI and release automation
```

## Key Patterns

### Adding a New Command

1. Create a new file in `internal/cli/` (e.g., `mycommand.go`)
2. Write a `newMyCommandCmd() *cobra.Command` constructor
3. Declare flag variables as locals inside it and bind them with
   `cmd.Flags().TypeVarP(&local, ...)`, so the `RunE` closure reads them
4. Register it in `NewRootCmd`'s `AddCommand` call

Commands are built by constructor rather than declared as package-level vars
wired up in `init()`. Flag values then live in the closure instead of in
globals, which is what lets a test build the tree as many times as it likes in
one process. With globals, pflag writes the parsed value into the shared
variable and nothing resets it, so a later run reading the same flag sees the
previous run's value.

### Context and Cancellation

- `main()` installs `signal.NotifyContext` (SIGINT/SIGTERM) and calls
  `rootCmd.ExecuteContext(ctx)`
- Long-running commands should read `cmd.Context()` and stop when it is
  canceled (see `internal/cli/example.go`)

### Logging

- Use `log/slog`; the default handler writes to stderr and is configured in
  the root command's `PersistentPreRun`
- The persistent `--verbose`/`-v` flag switches the level from Info to Debug
- stdout is reserved for command output; logs always go to stderr

### Output

- Write command output with `fmt.Fprintf(cmd.OutOrStdout(), ...)` rather than
  `fmt.Printf` so tests can capture it

### JSON Output

If the CLI grows a `--json` mode, three standard-library behaviours will bite:

- `encoding/json` HTML-escapes `<`, `>` and `&`, so user-supplied text comes
  back as `\u003c`. Encode through a `json.Encoder` with
  `SetEscapeHTML(false)` and route every call site through it.
- Marshalling a `map` sorts its keys. Payloads whose field order is part of the
  contract must be structs, where order follows declaration.
- A custom `MarshalJSON` that calls `json.Marshal` internally re-introduces
  escaping, because the outer encoder's `SetEscapeHTML(false)` cannot undo work
  already done inside.

And when caching an API response, unmarshalling into a struct silently drops
fields the struct does not model. Keeping the raw bytes alongside the parsed
form, and re-marshalling those, preserves what the struct does not know about.

### Version Injection

Version info is injected at build time via ldflags:
- `-X main.version=...`
- `-X main.commit=...`
- `-X main.date=...`

Falls back to `debug.ReadBuildInfo()` for `go install` builds.
`--version` is also supported (rootCmd.Version is set in Execute()).

### Go Version

`go.mod` pins a full patch version (e.g. `go 1.26.5`), not just `1.26`. Every
workflow resolves its toolchain with `go-version-file: go.mod`, so that line
also decides which Go the CI jobs install. Lowering it to the bare language
version silently builds against an unpatched standard library, and the `vuln`
job will then fail on any project whose code actually reaches the affected
stdlib paths. Bump it to a current patch release; do not round it down.

### Error Handling

- Commands should use `RunE` and return errors
- Wrap errors with context: `fmt.Errorf("action failed: %w", err)`
- Exit codes are handled in `cli.Execute()`; the root command sets
  `SilenceErrors`/`SilenceUsage` so errors are printed exactly once

## Testing

- Tests live alongside code: `foo.go` → `foo_test.go`
- Use table-driven tests for multiple cases (see `internal/cli/example_test.go`)
- Drive Cobra commands by calling `NewRootCmd` per invocation, then
  `SetArgs` + `SetOut`/`SetErr` buffers (see `executeCommand` in
  `internal/cli/example_test.go`). Building a fresh tree each time is what
  keeps cases independent; there is no flag state to reset by hand.
- Use `t.TempDir()` for temp directories and `t.Setenv()` for env vars
  (see `internal/config/config_test.go`)

## Common Tasks

### Rename the CLI

Use the bootstrap script, which handles all of the below (module path,
binary name, env-var prefix, README cleanup, LICENSE):

```bash
./scripts/setup.sh -o myuser -r my-cli
```

Manual equivalent:
1. Update `BINARY` in `Makefile`
2. Update `Use` in `internal/cli/root.go`
3. Update `main` and `binary` in `.goreleaser.yaml`
4. Update module path in `go.mod` and all imports
5. Update the `MYCLI_*` env-var prefix in `internal/config/config.go`

### Add a Dependency

```bash
go get github.com/some/package
go mod tidy
```

### Create a Release

```bash
make release-snapshot   # local dry-run of the GoReleaser config
git tag v1.0.0
git push origin v1.0.0
# GitHub Actions runs GoReleaser automatically
```
