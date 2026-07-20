#!/usr/bin/env bash
# Generate a markdown changelog section for a release.
#
# Usage:
#   changelog-for-release.sh <new-version> <tag-prefix> <path>...
#
# Examples:
#   .github/scripts/changelog-for-release.sh 0.8.0 plugins/agento11y plugins/agento11y
#   .github/scripts/changelog-for-release.sh 0.7.0 sdk-python python python-providers python-frameworks
#
# Finds the previous tag matching `<tag-prefix>/v*`, then walks
# `git log <last-tag>..HEAD -- <path>...` and groups commits by their
# conventional-commit type. Several SDKs tag under a different prefix than
# their source path (e.g. `sdk-python/v*` for code under `python/`), and
# some span multiple paths, so the tag prefix and commit paths are passed
# separately. Squash-merge subjects already carry the `(#PR)` suffix, so
# the output is link-ready on GitHub.
#
# Range and date default to "since the latest tag, dated today", which is
# what a release wants.
#   CHANGELOG_FROM  start ref (exclusive). Set but empty = walk from root.
#                   Unset = auto-detect the latest <tag-prefix>/v* tag.
#   CHANGELOG_TO    end ref (inclusive). Default HEAD.
#   CHANGELOG_DATE  section date (YYYY-MM-DD). Default today.
#
# Prints the section (one ## heading + body) to stdout. Does not touch
# any files. Caller is responsible for prepending the output to
# CHANGELOG.md (see prepend-changelog.sh).

set -euo pipefail

NEW="${1:-}"
TAG_PREFIX="${2:-}"

if [[ -z "$NEW" || -z "$TAG_PREFIX" || $# -lt 3 ]]; then
    echo "usage: $0 <new-version> <tag-prefix> <path>..." >&2
    exit 64
fi

shift 2
PATHS=("$@")

# Start of the range. An explicitly-set (even empty) CHANGELOG_FROM wins;
# otherwise fall back to the latest tag for this release line.
if [[ -n "${CHANGELOG_FROM+x}" ]]; then
    PREV="$CHANGELOG_FROM"
else
    PREV=$(git tag -l "${TAG_PREFIX}/v*" | sort -V | tail -1 || true)
fi

TO="${CHANGELOG_TO:-HEAD}"
DATE="${CHANGELOG_DATE:-$(date +%Y-%m-%d)}"

if [[ -n "$PREV" ]]; then
    RANGE="${PREV}..${TO}"
else
    RANGE="$TO"
fi

# Only commits that actually touched files under one of the paths.
COMMITS=$(git log --no-merges --pretty=format:'%s' "$RANGE" -- "${PATHS[@]}" || true)

emit_section() {
    local title="$1" pattern="$2" matched
    matched=$(grep -E "$pattern" <<<"$COMMITS" || true)
    [[ -z "$matched" ]] && return
    printf '### %s\n\n' "$title"
    # Drop the leading `type` but keep the scope as a bold prefix when present,
    # so `feat(plugins/copilot): foo` -> `- **plugins/copilot**: foo` and a
    # bare `feat: foo` -> `- foo`.
    printf '%s\n' "$matched" |
        sed -E 's/^[a-z]+\(([^)]+)\)!?: /- **\1**: /' |
        sed -E 's/^[a-z]+!?: /- /'
    printf '\n'
}

printf '## [%s] - %s\n\n' "$NEW" "$DATE"

# Breaking changes first (any type with `!:` suffix). Keep them out of
# type-specific sections so each commit appears once.
emit_section 'Breaking Changes' '^[a-z]+(\([^)]+\))?!: '
emit_section 'Features' '^feat(\([^)]+\))?: '
emit_section 'Bug Fixes' '^fix(\([^)]+\))?: '
emit_section 'Performance' '^perf(\([^)]+\))?: '
emit_section 'Documentation' '^docs(\([^)]+\))?: '

if ! grep -qE '^([a-z]+(\([^)]+\))?!: |(feat|fix|perf|docs)(\([^)]+\))?: )' <<<"$COMMITS"; then
    printf '_No user-facing changes._\n\n'
fi
