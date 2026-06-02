#!/usr/bin/env bash
# Prepend a changelog section (read from stdin) to a CHANGELOG.md, keeping
# a single top-level "# Changelog" header at the top of the file.
#
# Usage:
#   changelog-for-release.sh ... | prepend-changelog.sh <changelog-file>
#
# Creates the file if it does not exist yet (the first release for an SDK).

set -euo pipefail

FILE="${1:-}"
if [[ -z "$FILE" ]]; then
  echo "usage: $0 <changelog-file> < section" >&2
  exit 64
fi

SECTION="$(cat)"
TMP="$(mktemp)"

{
  printf '# Changelog\n\n'
  printf '%s\n' "$SECTION"
  # Append the existing body minus its own top-level header so we don't
  # duplicate it.
  if [[ -f "$FILE" ]]; then
    awk 'NR==1 && /^# /{next} {print}' "$FILE"
  fi
} > "$TMP"

mv "$TMP" "$FILE"
