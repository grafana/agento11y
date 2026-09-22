#!/usr/bin/env bash
# Resolve a Java release version to its tagged commit for a publication retry.
set -euo pipefail

VERSION="${1:-}"
NUM='(0|[1-9][0-9]*)'
if ! [[ "$VERSION" =~ ^${NUM}\.${NUM}\.${NUM}$ ]]; then
  echo "invalid release version: ${VERSION} (expected X.Y.Z)" >&2
  exit 65
fi

git rev-parse --verify "refs/tags/sdk-java/v${VERSION}^{commit}"
