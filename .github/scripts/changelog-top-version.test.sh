#!/usr/bin/env bash
# Tests for changelog-top-version.sh. Exits non-zero on any failure.

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
TOP_VERSION="${DIR}/changelog-top-version.sh"
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

: > "$FILE"
assert_eq 'empty file prints nothing' '' "$("$TOP_VERSION" "$FILE")"
assert_eq 'empty file still exits 0' 0 "$("$TOP_VERSION" "$FILE" >/dev/null; echo $?)"

printf '# Changelog\n\nNothing released yet.\n' > "$FILE"
assert_eq 'no version heading prints nothing' '' "$("$TOP_VERSION" "$FILE")"

printf '# Changelog\n\n## [1.2.3] - 2026-01-02\n\n### Features\n\n- a\n\n## [1.2.2] - 2026-01-01\n\n- b\n' > "$FILE"
assert_eq 'first of several sections wins' '1.2.3' "$("$TOP_VERSION" "$FILE")"

printf '# Changelog\n\n## [Unreleased]\n\n## [0.9.0] - 2026-01-01\n\n- c\n' > "$FILE"
assert_eq 'non-semver heading skipped' '0.9.0' "$("$TOP_VERSION" "$FILE")"

assert_eq 'missing file prints nothing' '' "$("$TOP_VERSION" "${TMP}/absent.md" 2>/dev/null)"
assert_eq 'missing file still exits 0' 0 "$("$TOP_VERSION" "${TMP}/absent.md" >/dev/null 2>&1; echo $?)"

assert_eq 'no argument exits 64' 64 "$("$TOP_VERSION" >/dev/null 2>&1; echo $?)"

echo "passed: ${pass}, failed: ${fail}"
[[ $fail -eq 0 ]]
