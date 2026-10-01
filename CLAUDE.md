# CLAUDE.md

This file provides guidance for AI assistants working with this codebase.

## Project Overview

repoman is a conservative Go CLI for local clone/worktree inventory, safe GitHub synchronization, and plan-first cleanup. It was bootstrapped from richhaase/go-cli-template and follows its Cobra constructor and build conventions.

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
├── cmd/repoman/          # Main entry point (thin wrapper)
│   └── main.go         # Version injection, signal handling, calls cli.Execute()
├── internal/
│   ├── cli/            # Cobra command definitions
│   │   ├── root.go     # Root command, Execute(), slog setup
│   │   ├── version.go  # Version command
│   │   └── *.go        # Additional commands
│   ├── repository/     # Read-only Git/process inventory
│   ├── syncer/         # GitHub activity selection and safe clone/FF
│   ├── cleanup/        # Conservative cleanup proof and revalidation
│   ├── domain/         # Reserved core domain package
│   ├── config/         # Configuration loading
│   └── terminal/       # TTY detection helpers
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
  canceled (see `internal/cli/operations.go`)

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
- Use table-driven tests for multiple cases (see `internal/cli/operations_test.go`)
- Drive Cobra commands by calling `NewRootCmd` per invocation, then
  `SetArgs` + `SetOut`/`SetErr` buffers (see `executeCommand` in
  `internal/cli/operations_test.go`). Building a fresh tree each time is what
  keeps cases independent; there is no flag state to reset by hand.
- Use `t.TempDir()` for temp directories and `t.Setenv()` for env vars
  (see `internal/config/config_test.go`)

## Common Tasks

### Safety constraints

- Never invoke the original sync-repos/clean-repos scripts or mutate a user's real data in tests.
- Tests use t.TempDir Git fixtures and fake GitHub metadata; no network is needed.
- Keep unknown state distinct from clean. Cleanup must protect hidden index flags,
  local-only work, ignored/untracked files, locks, in-use paths, and primary clones.
- Keep cleanup default preview; apply revalidates identity, local state, and PR proof.
- Do not add forced checkout, reset, worktree removal, or primary clone deletion.
- Process observation explicitly covers the current OS user, not the whole system.
- JSON schema version 1 and human rendering share engine results. Stdout is data;
  diagnostics go to stderr. Preserve cancellation and partial failure exit codes.

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
