# repoman

A small Go CLI for managing local clones and linked worktrees. Built from
[richhaase/go-cli-template](https://github.com/richhaase/go-cli-template), using
Cobra, constructor-scoped flags, context cancellation, structured output, and the
template's build/release tooling. Template baseline: `27ed4591accb7628894d0a1cc23fff45840fe31b`.

This is a conservative MVP inspired by `sync-repos` and `clean-repos` in
[richhaase/dotfiles](https://github.com/richhaase/dotfiles). It does not install,
replace, or invoke those scripts. Script reference baseline:
`da7f57469464da9edf37bfed010f0286c56ec836`.

## Build

Requires the Go version pinned in `go.mod` (or newer), Git, and GitHub CLI (`gh`)
for GitHub-backed operations. Authenticate `gh` separately; repoman never starts
an interactive login flow. `status` needs only Git.

```sh
make build
./bin/repoman --help
./bin/repoman version
make check
```

## Quick start

```sh
# Read-only, offline inventory of direct child repositories and linked worktrees
repoman status --root ~/src
repoman status --root ~/src --json

# Preview remote activity decisions without fetching or changing files
repoman sync --root ~/src --owner richhaase --days 45 --dry-run

# Clone recently pushed, non-archived repos; fetch and safely fast-forward clones
repoman sync --root ~/src --owner richhaase --exclude 'prototype-*'

# Cleanup is a preview unless explicitly applied
repoman clean --root ~/src
repoman clean --root ~/src --json
repoman clean --root ~/src --apply
```

No command prompts for input. Command data goes to stdout, diagnostics to stderr.
Use `--json` for automation; do not parse the human display.

## Configuration

The default is `$XDG_CONFIG_HOME/repoman/config.json` (or the OS user config
location). Override with `REPOMAN_CONFIG` or `--config FILE`. The file is plain
JSON, read-only to repoman; there is no implicit migration or registry write.

```json
{
  "targets": [
    {
      "dir": "~/src",
      "owner": "richhaase",
      "days": 45,
      "events": false,
      "includes": [],
      "excludes": ["prototype-*"]
    }
  ]
}
```

With no positional directory or `--root`, commands select all configured targets.
A positional `DIR` selects that configured target. `--root DIR` bypasses config
lookup for an ad hoc target. `clean --apply` requires exactly one selected target.
Unknown config fields and malformed patterns are rejected rather than ignored.
Includes/excludes are Go filepath globs on repository names; exclusions win.
Sync and clean honor these selections (clean groups worktrees by primary clone).
Status intentionally inventories every local checkout in the selected roots.

Existing sync-repos JSON can be read explicitly:

```sh
repoman status --config ~/.config/sync-repos/config.json
repoman sync ~/src --config ~/.config/sync-repos/config.json --no-events --dry-run
```

This MVP uses GitHub push activity only. An explicit legacy `events: true` is
rejected by sync unless `--no-events` is supplied. Days default to 45. Unlike the
original scripts, there is no force mode, automatic inactive-clone deletion,
config mutation, or event-API fallback. Primary clones are inventoried and always
kept by cleanup; retirement of inactive primary clones is deferred.

## Safety contract

### Status

Discovers direct children of each root and their registered linked worktrees.
Reports identity, branch/HEAD, origin, tracked/untracked/ignored state, upstream
ahead/behind, local-only commits, stash, locks, and process-use observation.
Git errors are unknown states, never clean states. Hidden index flags such as
assume-unchanged and skip-worktree protect a checkout because status can conceal
modified tracked files. `ahead`/`behind` of -1 mean an
upstream comparison was unavailable. Local-only detection uses locally available
remote refs; status does not fetch. Non-repository directories are not recursively
searched. Symlink checkout entries and Git discovery inherited from a parent are rejected;
inventory roots are canonicalized. Sync rejects symlink path components.

### Sync

Uses authenticated GitHub API metadata on github.com; repositories are selected
by pushed time and include/exclude patterns. Empty, archived, and inactive repos
are skipped. Existing destinations must be primary clones of the expected origin.
Only clean default branches can fast-forward. Detached HEADs, other branches,
local changes, ahead/diverged histories, locks, wrong origin, and unknown state
are skipped or reported as errors. No hard reset, forced checkout, branch deletion,
or all-remotes fetch is used. Dry-run performs remote reads but no clone/fetch.

### Clean

Never removes primary checkouts. A linked worktree must be inside the selected
root, observable and clean, with no untracked or ignored files, local-only commits,
stash, locks, or process-use evidence. An in-use sibling checkout or shared Git
metadata protects all worktrees of that repository. Unknown process observations
protect it.
GitHub must confirm a terminal PR for the exact current HEAD in the same repository,
with no open PR on the branch. No PR, missing access, API failure, unsupported
origin, uncertain identity, or missing metadata means keep/error, never permission
to remove. Remote tracking refs are not refreshed during cleanup.

`--apply` creates a fresh plan, validates the complete inventory and GitHub evidence
again before mutation, and rechecks each candidate immediately before `git worktree
remove` without `--force`. It never deletes branches or prunes metadata. JSON from a
preview is an inspection artifact, not a replayable authorization token. Concurrent
external Git/filesystem activity cannot be made atomic with a CLI check; stop other
writers before applying. A late change aborts further removals and reports any
already completed removals honestly. No rollback is promised.

This is deliberately stricter than clean-repos's personal force-clean policy.
No aggressive policy is implemented. Process-use detection is scoped to the current OS user (Linux /proc, macOS lsof).
It does not claim visibility into other users or prevent new processes starting
after inspection. Actual observation errors protect candidates; stop other writers
before applying. No elevated privileges are needed or recommended.

## Output and exit codes

All three commands return one JSON object with `schema_version: 1`, `command`,
`dry_run`, `items` (always an array), and `errors` (always an array). Human and JSON
renderers consume the same results. Item actions explain planned, skipped, or
completed work; a skipped safety check is not a successful update/removal.

- `0`: completed inspection/action; intentional safety skips may remain
- `1`: command/usage/runtime failure before a complete report
- `2`: invalid target/config selection
- `3`: report emitted with operation or inspection errors (possibly partial work)
- `130`: canceled by SIGINT/SIGTERM

Consumers must check both exit status and individual item actions. Schema additions
may be backward-compatible; incompatible changes will increment `schema_version`.

## Development and scope

```sh
make fmt
make check                # fmt-check, vet, pinned lint, race tests
make build
make vuln
make release-snapshot     # requires GoReleaser; local only
```

Tests create isolated temporary Git repositories and fake remote responses. They
do not touch a user's clone registry or invoke the original cleanup scripts.
No background service, task orchestration, editor integration, force cleanup,
GitHub Enterprise support, or release installation is included.
