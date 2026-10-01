# repoman

A Go CLI combining the repository synchronization and worktree cleanup workflows
from [richhaase/dotfiles](https://github.com/richhaase/dotfiles). Built from
[richhaase/go-cli-template](https://github.com/richhaase/go-cli-template), with
Cobra, context cancellation, and human or structured JSON output.

Script reference baseline: `da7f57469464da9edf37bfed010f0286c56ec836`.
Template baseline: `27ed4591accb7628894d0a1cc23fff45840fe31b`.
Repoman does not install or invoke the original scripts.

## Build

Requires the Go version pinned in `go.mod` (or newer), Git, and GitHub CLI (`gh`)
for GitHub-backed operations. Authenticate `gh` separately; repoman never starts
an interactive login flow. macOS cleanup also requires `lsof` for process-cwd
observation. Offline `status` needs only Git; missing process visibility is
reported rather than represented as unused.

```sh
make build
./bin/repoman --help
make check
```

## Quick start

```sh
# Register a root; owner defaults to your authenticated GitHub account
repoman config add ~/src
repoman config list

# Inspect every configured root without network access
repoman status
repoman status --root ~/src --json

# Preview synchronization, then clone/fetch/fast-forward active repositories
repoman sync --dry-run
repoman sync

# Preview worktree cleanup, then remove eligible linked worktrees
repoman clean --dry-run
repoman clean

# Opt into a stricter retention policy
repoman clean --level conservative --dry-run
```

**`clean` removes eligible linked worktrees by default**, matching `clean-repos`.
Its default `aggressive` policy can discard dirty, untracked, and ignored files
and detached local commits. Named branch refs are retained. Use `--dry-run`/`-n`
to inspect the plan first, or select a stricter policy below.

No command prompts for input. Command data goes to stdout, diagnostics to stderr.
Use `--json` for automation; do not parse the human display.

## Configuration

On both macOS and Linux, configuration lookup uses this order:

1. Explicit `--config FILE`
2. Non-empty `REPOMAN_CONFIG`
3. `$XDG_CONFIG_HOME/repoman/config.json` when `XDG_CONFIG_HOME` is absolute
4. `$HOME/.config/repoman/config.json` when it is unset, empty, or relative

```json
{
  "targets": [
    {
      "dir": "~/src",
      "owner": "richhaase",
      "days": 45,
      "events": false,
      "cleanup_level": "aggressive",
      "includes": [],
      "excludes": ["prototype-*"]
    }
  ]
}
```

An original `sync-repos` JSON file can be copied unchanged to this path. The
`targets` fields `dir`, `owner`, `days`, `events`, `includes`, and `excludes` are
accepted directly, without conversion. You can also keep reading the original:

```sh
repoman status --config ~/.config/sync-repos/config.json
repoman sync --config ~/.config/sync-repos/config.json --dry-run
```

Activity uses GitHub pushes only. Legacy `events: true` is accepted and produces
one stderr notice per sync command explaining that event fallback is unsupported.
`--no-events` explicitly selects push-only behavior and suppresses that notice.
New registrations write `events: false`. No event API request is made.

With no `DIR`, status/sync/clean select every configured target in config order.
A positional `DIR` selects one registered target. `--root DIR` or its
`--target`/`-t` alias selects an ad hoc root without loading config. Sync uses
`--owner`/`-o` or the authenticated GitHub user when no owner is configured.

Unknown fields and malformed patterns are rejected. Includes/excludes use Go
globs on repository names; exclusions win. Common `*`, `?`, character ranges,
backslash escapes, and Bash negated classes such as `[!x]` work as before; `[^x]`
is also accepted. Extended shell patterns such as `@(one|two)` and POSIX/locale
classes such as `[[:alpha:]]` are rejected with a clear error. Use separate
patterns or explicit character ranges for those cases. Sync and clean honor this selection
(clean groups worktrees by primary clone). Status inventories all local checkouts
under the selected roots. `days` defaults to 45 and affects sync activity, not
linked-worktree retention.

### Manage targets

```sh
repoman config add ~/src --days 45 --exclude 'prototype-*'
repoman config add ~/work --owner my-org --include 'service-*'
repoman config add ~/work --owner my-org --excludes-file ./excluded-repos.txt
repoman config list --json
repoman config remove ~/work
```

`add` registers or replaces a target and appends it in config order. It replaces
that target's sync settings; an existing `cleanup_level` is retained unless
`--cleanup-level` overrides it. `remove` changes registration only; it does not
delete clones or worktrees. `list --json` emits the configuration object for reuse.
Exclusion files trim whitespace and ignore blank lines and lines starting with
`#`; their patterns are appended to supplied exclusions.

Only these management commands write config. Writes are atomic and retain an
existing file's permissions. Existing symlinks are preserved by updating their
resolved destination; dangling symlinks produce an error. Unrelated target values
are preserved. Status, sync, and clean never rewrite the registry.

## Sync behavior

Select non-archived repositories with pushes inside the activity window. Fetch
all pages of GitHub results, without the original script's 500-repository cap.
Clone missing active repositories. For existing active primary clones, fetch all
configured remotes and prune obsolete remote refs before deciding whether to
update the checkout.

A clean default branch fast-forwards. Dirty/untracked files, another branch,
detached HEAD, or ahead/diverged history leave the checkout as-is after fetching.
Ignored build files and linked worktrees do not prevent ordinary synchronization.
No default branch or missing origin default ref yields a fetch-only result.
Destinations must still be the expected primary clone with the expected origin;
identity errors are never treated as permission to modify another repository.

```sh
# Preview an explicit reset of active clones to their origin default branch
repoman sync ~/src --force --dry-run

# Discard tracked changes/divergence and switch to the default branch
repoman sync ~/src --force

# Preview deletion of inactive primary clones
repoman sync ~/src --cleanup --dry-run
repoman sync ~/src --cleanup
```

`--force`/`-f` uses forced checkout of the default branch at origin's current
commit, as `sync-repos` did. It does not run `git clean -fdx`. Git may remove
untracked/ignored files that obstruct that checkout, but unrelated untracked
content is not swept away.

`--cleanup`/`-c` removes selected inactive primary clones when they have no tracked
modifications or untracked files. Ignored files and local commits alone do not
protect an inactive primary. **This deletes the clone and its contained Git
history.** `--force` does not override the inactive-clone dirty check.

One intentional fix to the original script: a primary with registered linked
worktrees is retained, because deleting its shared Git metadata would break those
worktrees. Normal fetching still works for that primary. Other deliberate fixes
include complete pagination, exact repository identity checks, and a dry run that
does not create directories or write Git metadata.

## Clean behavior

`clean` manages registered linked worktrees; primary clones are always retained
here. Choose `cleanup_level` per target or override with `--level`. An existing
explicit level remains respected; the aggressive default applies when no level
is configured.

- **aggressive (default):** the original `clean-repos` policy. Remove a linked
  worktree unless it is explicitly locked, is a current-user process's working
  directory, or is associated with an open GitHub PR. Dirty/untracked/ignored
  files, local commits, stash, recent activity, or having no PR do not independently
  retain it. No extra discard acknowledgement is required
- **balanced:** retain local files and local-only committed work, and require
  successful checks showing no open PR. This is an optional stricter policy
- **conservative:** retain the balanced protections and require terminal PR
  evidence for the current HEAD. This is the strictest optional policy

```sh
repoman clean --dry-run --json
repoman clean ~/src
repoman clean ~/src --level balanced --dry-run
repoman clean ~/src --level conservative
```

The previous `--apply` flag remains a compatibility alias: `--apply=false`
previews, while `--apply=true` applies. It cannot be combined with
`--dry-run=true`. `--discard-local-changes` remains an optional deprecated
aggressive-mode alias and is no longer required. `--days`/`-d` is accepted for
script compatibility but has no effect on cleanup retention.

The operation follows registrations from discovered repositories, including
bare-repository anchors and linked worktrees outside the selected parent. It never deletes branch refs or
fetches remotes. Aggressive removal uses one `git worktree remove --force`; it
never overrides explicit locks with double force or falls back to shell deletion.
An open shell in the primary does not protect an unrelated linked worktree.

PR checks consider branch/commit associations and explicit PR-number hints from
worktree names and local refs. Any relevant open PR retains the worktree. No
match is distinct from a failed lookup. Non-GitHub repositories have no GitHub
PR evidence and remain eligible under the default policy.

The complete selected batch is inventoried and checked before any removal.
Inventory, identity, or GitHub lookup failures abort that batch. Default
aggressive cleanup checks process use on a best-effort basis, matching the
original script: observed cwd matches are retained, while incomplete visibility
produces a stderr warning and JSON warnings instead of stopping all cleanup.
Unobserved processes may still use eligible worktrees. Balanced/conservative
require complete process observation and abort when it is unavailable. Registration identity, locks, and cwd use are checked again before
removing each candidate. Stale registrations are reported and pruned using Git's
normal expiry rules. If another stale registration in the same repository is
protected, bulk pruning is deferred so it cannot remove that registration. An
unlocked missing worktree not marked prunable is an inventory error.

Strict modes retain additional local-state protections. The default aggressive
mode does not impose content-snapshot quotas or require all local commits to be
published. A preview is not a saved plan; applying builds a fresh plan.

Process observation covers the current OS user (Linux `/proc`, macOS `lsof`).
Concurrent writers can still change state between checks and Git's operation.
Failures stop further removals and report completed work; rollback is not promised.

## Status and structured output

Status discovers direct child repositories and registered worktrees, including
identity, branch/HEAD, origin, tracked/untracked/ignored state, upstream distance,
local-only commits, stash, locks, and process use. Git errors remain unknown
states. Remote refs are not refreshed; `ahead`/`behind` of -1 means comparison was
unavailable. Non-repository directories are not recursively scanned.

Status/sync/clean emit one JSON object with `schema_version: 1`, `command`,
`dry_run`, `items` (always an array), and `errors` (always an array). Human and JSON
renderers consume the same results. Cleanup also reports effective policies for
every selected root, including empty roots, and per-item destructive indicators. Non-empty `warnings` arrays
report incomplete best-effort process visibility on cleanup items and the command
envelope. Each state reflects its latest observation; warnings can also reflect
incomplete earlier checks. Check actions and `dry_run` to distinguish plans from completed mutations.

- `0`: completed; intentional skips may remain
- `1`: command/usage/runtime failure before a complete report
- `2`: invalid target/config selection or cleanup options
- `3`: report emitted with operation or inspection errors, possibly partial work
- `130`: canceled by SIGINT/SIGTERM

Consumers must check both exit status and individual item actions. Incompatible
output changes increment `schema_version`; additive fields may retain it.

## Development

```sh
make fmt
make check                # formatting, vet, pinned lint, race tests
make build
make vuln
make release-snapshot     # requires GoReleaser; local only
```

CI tests Linux and macOS and cross-builds both CPU architectures. Tests use
isolated temporary Git repositories and fake remote responses. They never execute
the original scripts, mutate a user's registry, or clean actual user repositories.
No background service, editor integration, GitHub Enterprise support, or automatic
release installation is included.
