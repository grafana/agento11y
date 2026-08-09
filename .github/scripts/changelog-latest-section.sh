#!/usr/bin/env bash
# Print the newest release section of a CHANGELOG.md, heading included.
#
# Usage:
#   changelog-latest-section.sh <changelog-file>
#
# The output is the release-notes body for the version that
# changelog-top-version.sh reports. Exits 1 when the file has no section:
# callers pass the result straight to `gh release create`, and publishing
# empty notes is worse than failing the run.

set -euo pipefail

FILE="${1:-}"
if [[ -z "$FILE" ]]; then
  echo "usage: $0 <changelog-file>" >&2
  exit 64
fi

if [[ ! -f "$FILE" ]]; then
  echo "no such changelog: ${FILE}" >&2
  exit 1
fi

# Print every line inside the first section and stop at the next heading. The
# pattern matches the same headings as changelog-top-version.sh, so a
# `## [Unreleased]` block above the newest release is skipped by both.
SECTION=$(awk '/^## \[[0-9]+\.[0-9]+\.[0-9]+\]/{n++} n==1{print} n==2{exit}' "$FILE")

if [[ -z "$SECTION" ]]; then
  echo "no '## [x.y.z]' section found in ${FILE}" >&2
  exit 1
fi

printf '%s\n' "$SECTION"
