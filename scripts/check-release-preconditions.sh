#!/usr/bin/env bash
# This script never prints credential values or changes repository settings.
set -euo pipefail

if [[ "${REPOMAN_REPOSITORY_PRIVATE:-}" != "false" ]]; then
  printf '%s\n' '::error::Public Homebrew publishing is blocked while Repoman is private or repository visibility is unknown. Choose and authorize a distribution model first.' >&2
  exit 1
fi

missing=0
for name in GITHUB_TOKEN HOMEBREW_TAP_GITHUB_TOKEN QUILL_SIGN_P12 QUILL_SIGN_PASSWORD QUILL_NOTARY_KEY QUILL_NOTARY_KEY_ID QUILL_NOTARY_ISSUER; do
  if [[ -z "${!name:-}" ]]; then
    printf '::error::Missing release secret: %s\n' "$name" >&2
    missing=1
  fi
done
exit "$missing"
