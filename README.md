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

## Homebrew distribution

After an approved stable release has published its cask and assets:

```sh
brew install --cask richhaase/tap/repoman
brew upgrade --cask repoman
```

The generated cask installs the `repoman` binary and declares `git` and `gh`
dependencies. The publishing destination is the existing
[richhaase/tap](https://github.com/richhaase/homebrew-tap) repository; GoReleaser
updates its default branch directly, following the Bigboard/Plonk/ACR pattern.
There is no hand-maintained formula or tap pull-request publisher.

A snapshot or a merged setup PR is **not** an installable release. No version tag
or release is created by the validation commands below.

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
      "cleanup_level": "conservative",
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
original scripts, there is no force-sync mode, automatic inactive-clone deletion,
config mutation, or event-API fallback. Cleanup aggression is configured separately. Primary clones are inventoried and always
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

Cleanup always previews unless `--apply` is supplied. Choose a per-target
`cleanup_level` in JSON or override it for this invocation with `clean --level`.
The default is `conservative`; existing configurations retain their behavior.

- **conservative:** keep the existing policy: the worktree must be completely clean
  (including no untracked or ignored files), and GitHub must confirm a terminal PR
  for the exact current HEAD in the same repository, with no associated open PR or
  open PR on the current branch
- **balanced:** also remove completely clean worktrees without a terminal PR, but
  only after complete, successful GitHub checks establish no associated open PR or
  open PR on the current branch. This deliberately treats an unassociated clean
  checkout as disposable. The HEAD and all local branch commits must remain
  reachable from locally recorded remote refs; absence of a PR alone is insufficient
- **aggressive:** use balanced PR/commit eligibility, and also permit discarding
  tracked changes, untracked files, and ignored files. Applying this level requires
  the separate `--discard-local-changes` acknowledgement on every invocation, even
  if the current plan happens to contain only clean worktrees. Selecting aggressive
  in config does **not** authorize data loss

```sh
# Preview a stricter or looser policy without modifying anything
repoman clean --root ~/src --level balanced
repoman clean --root ~/src --level aggressive --json

# Apply a reviewed clean-worktree policy
repoman clean --root ~/src --level balanced --apply

# DANGER: permanently discard local file contents in eligible linked worktrees
repoman clean --root ~/src --level aggressive --apply --discard-local-changes

# Override a configured aggressive target back to the safe default
repoman clean ~/src --level conservative --apply
```

The discard flag is rejected with conservative or balanced levels. It cannot be
persisted in configuration. Human previews label the effective policy and flag
planned file loss; JSON includes the policy and a per-item destructive indicator.
A preview is not a saved plan: applying builds a fresh plan from current evidence.

**Protections that no level overrides:** primary checkouts, paths outside the
selected root, unknown Git or path identity, local-only committed work, stash,
hidden-index flags (assume-unchanged/skip-worktree), submodules, locks, active Git
operations, in-use checkouts/shared metadata, or incomplete process observation.
Nested repositories and unsafe filesystem content are protected before aggressive
removal. Branches are never deleted. A GitHub API failure, missing access,
unsupported origin, or incomplete response is unknown evidence, never “no PR.”
Remote tracking refs are not refreshed during cleanup.

`--apply` validates the complete inventory and GitHub evidence again before any
mutation and rechecks each candidate immediately before removal. Aggressive
candidates additionally receive content snapshots of the worktree and index so
changed local bytes or newly discovered unsafe content abort removal. Each
candidate is limited to 100,000 entries, 256 directory levels, and 256 MiB of
snapshot input; unreadable
content, larger candidates, special files, nested mounts, and nested repositories
are kept rather than bypassing inspection. Symlink targets are not followed. Non-destructive removal uses
`git worktree remove`; acknowledged destructive removal may use one `--force`.
Locks are never overridden with double force, and there is no shell deletion
fallback or metadata pruning.

Concurrent external Git/filesystem activity cannot be made atomic with a CLI
check; stop other writers before applying. A late change aborts further removals
and reports completed removals honestly. A failed attempted removal is marked
`failed`, not assumed untouched. No rollback is promised.

Process-use detection is scoped to the current OS user (Linux /proc, macOS lsof).
It does not claim visibility into other users or prevent new processes starting
after inspection. An in-use sibling checkout or shared Git metadata protects all
worktrees of that repository. No elevated privileges are needed or recommended.

## Output and exit codes

All three commands return one JSON object with `schema_version: 1`, `command`,
`dry_run`, `items` (always an array), and `errors` (always an array). Human and JSON
renderers consume the same results. Item actions explain planned, skipped, or
completed work; a skipped safety check is not a successful update/removal.
Cleanup additionally emits `cleanup_policies` for every selected root, including
empty roots, with `level` and the requested `discard_local_changes` flag. Each
cleanup item includes its effective level and `destructive` indicator. These are
additive schema-v1 fields; `dry_run` and item `action` distinguish plans from actual
removals. A requested discard flag alone never means data was deleted.

- `0`: completed inspection/action; intentional safety skips may remain
- `1`: command/usage/runtime failure before a complete report
- `2`: invalid target/config selection or cleanup options
- `3`: report emitted with operation or inspection errors (possibly partial work)
- `130`: canceled by SIGINT/SIGTERM

Consumers must check both exit status and individual item actions. Schema additions
may be backward-compatible; incompatible changes will increment `schema_version`.

## Release setup and privacy

The ordinary public-tap download path requires **public release assets**. This
workflow therefore refuses to publish while the Repoman repository is private or
its visibility cannot be verified. Both the workflow preflight and GoReleaser
before hook check current metadata for the exact Repoman repository before
creating a release, signing binaries, or writing the public cask. It never changes
repository visibility itself. Choose and explicitly authorize a public distribution
model before the first release. Keeping source private requires a different,
reviewed authenticated-download or separate-artifact-repository design; do not
remove the privacy guard just to work around an installation failure.

Before an authorized tag release, the repository needs these Actions secrets
(reference names only; never commit their values):

- `HOMEBREW_TAP_GITHUB_TOKEN`: an approved credential with access to update the tap
- `QUILL_SIGN_P12` and `QUILL_SIGN_PASSWORD`: the approved base64-encoded P12
  macOS signing identity and its password
- `QUILL_NOTARY_KEY`, `QUILL_NOTARY_KEY_ID`, and `QUILL_NOTARY_ISSUER`: approved
  Apple notarization credentials (`QUILL_NOTARY_KEY` is the base64-encoded P8 key)
- `GITHUB_TOKEN` is supplied by GitHub Actions for this repository's release

The default Actions token cannot update another repository's tap. Existing secret
names in a different repository do not automatically make them available here.
Creating credentials, expanding their access, or configuring them for this
repository is a separate authorized setup step. The preflight reports missing
names only and fails before publication; it never prints values.

The release workflow runs on explicitly pushed `v*` tags and uses pinned
GoReleaser 2.18.2. It builds Linux/macOS AMD64/ARM64 archives, signs and notarizes
macOS binaries, computes SHA-256 checksums, uploads GitHub release assets, and
pushes the generated cask directly to the tap. `skip_upload: auto` keeps
prereleases out of the stable Homebrew cask. Do not create an arbitrary version
just to test the workflow; choose the actual release version deliberately after
CI and the distribution/credential prerequisites are satisfied.

Safe validation requires GoReleaser 2.18.2 and Python 3:

```sh
make release-check       # synthetic preflight tests and GoReleaser schema check
make release-snapshot    # local build/package/cask generation; no publishing
```

CI performs the same snapshot checks with read-only repository permission and no
release/signing credentials. It verifies all four archive targets, packaged
binary/README/LICENSE, executable modes, cask URLs and matching checksums, and
Homebrew dependencies. Snapshots skip signing, notarization and publishing, and
are not uploaded as workflow artifacts. Passing these checks does not prove tap
write access, Apple credential validity, or installation from a real release.
Only run a real release after those prerequisites are explicitly approved. The
guarded workflow is the normal route; a live local GoReleaser invocation runs the
same current-visibility and credential-presence check instead of bypassing it.

References: [GoReleaser casks](https://goreleaser.com/customization/publish/homebrew_casks/)
and [snapshot behavior](https://goreleaser.com/customization/publish/snapshots/).

## Development and scope

```sh
make fmt
make check                # fmt-check, vet, pinned lint, race tests
make build
make vuln
make release-check        # release safeguards and configuration
make release-snapshot     # requires GoReleaser 2.18.2; local only
```

CI runs the race-test suite on Linux and macOS, and cross-builds both CPU
architectures. Tests create isolated temporary Git repositories and fake remote responses. They
do not touch a user's clone registry or invoke the original cleanup scripts.
No background service, task orchestration, editor integration, primary-clone
deletion or GitHub Enterprise support is included. Release installation is a
separate step after a real, authorized release has been published.
