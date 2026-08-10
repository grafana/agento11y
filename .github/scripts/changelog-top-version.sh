#!/usr/bin/env bash
# Print the version of the newest release section in a CHANGELOG.md.
#
# Usage:
#   changelog-top-version.sh <changelog-file>
#
# Prints `0.25.0` for a file whose first `## [X.Y.Z]` heading is
# `## [0.25.0] - 2026-08-06`, and prints nothing when the file has no such
# heading or does not exist. Both cases exit 0: a release line whose
# changelog has not been written yet is a row to skip, not a failure.

set -euo pipefail

FILE="${1:-}"
if [[ -z "$FILE" ]]; then
  echo "usage: $0 <changelog-file>" >&2
  exit 64
fi

[[ -f "$FILE" ]] || exit 0

VERSION=$(grep -m1 -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "$FILE" || true)
VERSION=${VERSION#'## ['}
VERSION=${VERSION%']'}

[[ -n "$VERSION" ]] && printf '%s\n' "$VERSION"

exit 0
