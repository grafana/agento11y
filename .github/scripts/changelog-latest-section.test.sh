#!/usr/bin/env bash
# Tests for changelog-latest-section.sh. Exits non-zero on any failure.

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
SECTION="${DIR}/changelog-latest-section.sh"
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

printf '# Changelog\n\n## [0.1.0] - 2026-01-01\n\n### Features\n\n- initial\n' > "$FILE"
assert_eq 'single section' \
"## [0.1.0] - 2026-01-01

### Features

- initial" "$("$SECTION" "$FILE")"

printf '# Changelog\n\n## [0.2.0] - 2026-02-02\n\n### Bug Fixes\n\n- fix it\n\n## [0.1.0] - 2026-01-01\n\n### Features\n\n- initial\n' > "$FILE"
assert_eq 'newest of several sections' \
"## [0.2.0] - 2026-02-02

### Bug Fixes

- fix it" "$("$SECTION" "$FILE")"
assert_eq 'older section excluded' '' "$("$SECTION" "$FILE" | grep -F '0.1.0')"

printf '# Changelog\n\n## [Unreleased]\n\n- pending\n\n## [0.9.0] - 2026-01-01\n\n- c\n' > "$FILE"
assert_eq 'non-semver heading skipped' \
"## [0.9.0] - 2026-01-01

- c" "$("$SECTION" "$FILE")"

printf '# Changelog\n\nNothing released yet.\n' > "$FILE"
assert_eq 'no heading prints nothing' '' "$("$SECTION" "$FILE" 2>/dev/null)"
assert_eq 'no heading exits non-zero' 1 "$("$SECTION" "$FILE" >/dev/null 2>&1; echo $?)"

assert_eq 'missing file exits non-zero' 1 "$("$SECTION" "${TMP}/absent.md" >/dev/null 2>&1; echo $?)"

assert_eq 'no argument exits 64' 64 "$("$SECTION" >/dev/null 2>&1; echo $?)"

echo "passed: ${pass}, failed: ${fail}"
[[ $fail -eq 0 ]]
