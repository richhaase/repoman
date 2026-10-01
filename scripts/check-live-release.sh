#!/usr/bin/env bash
# Read current metadata for the exact configured publication source; never infer
# public visibility from a caller-provided flag, a fork, or stale event payload.
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
REPOMAN_REPOSITORY_PRIVATE="$(GH_TOKEN="${GITHUB_TOKEN:-}" gh api --hostname github.com repos/richhaase/repoman --jq .private)"
export REPOMAN_REPOSITORY_PRIVATE
bash "$root/scripts/check-release-preconditions.sh"
