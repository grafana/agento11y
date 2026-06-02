#!/usr/bin/env bash
# Tests for prepend-changelog.sh. Exits non-zero on any failure.

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
PREPEND="${DIR}/prepend-changelog.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail=0
pass=0

assert_eq() {
  local desc="$1" want="$2" got="$3"
  if [[ "$want" == "$got" ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL ${desc}"
    echo "--- want ---"; printf '%s\n' "$want"
    echo "--- got ----"; printf '%s\n' "$got"
    echo "------------"
    fail=$((fail + 1))
  fi
}

FILE="${TMP}/CHANGELOG.md"

# First release: file does not exist yet, gets created with a single header.
printf '## [0.1.0] - 2026-01-01\n\n### Features\n\n- initial\n' | "$PREPEND" "$FILE"
assert_eq 'creates file with one header' \
"# Changelog

## [0.1.0] - 2026-01-01

### Features

- initial" "$(cat "$FILE")"

# Second release: new section goes on top, old header is not duplicated.
printf '## [0.2.0] - 2026-02-02\n\n### Bug Fixes\n\n- fix it\n' | "$PREPEND" "$FILE"
assert_eq 'prepends without duplicating header' \
"# Changelog

## [0.2.0] - 2026-02-02

### Bug Fixes

- fix it

## [0.1.0] - 2026-01-01

### Features

- initial" "$(cat "$FILE")"

assert_eq 'exactly one top-level header' 1 "$(grep -c '^# Changelog' "$FILE")"

echo "passed: ${pass}, failed: ${fail}"
[[ $fail -eq 0 ]]
