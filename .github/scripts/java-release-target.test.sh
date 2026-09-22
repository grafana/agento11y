#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_REPO=$(mktemp -d)
trap 'rm -rf "$TEST_REPO"' EXIT
git init -q "$TEST_REPO"
cd "$TEST_REPO"
git -c user.name=Test -c user.email=test@example.com -c commit.gpgsign=false \
  commit -q --allow-empty -m 'Tagged release'
RELEASE_SHA=$(git rev-parse HEAD)
git -c user.name=Test -c user.email=test@example.com -c tag.gpgsign=false \
  tag -a sdk-java/v0.7.1 -m 'Java 0.7.1'
git -c user.name=Test -c user.email=test@example.com -c commit.gpgsign=false \
  commit -q --allow-empty -m 'Newer development'

# A retry must use the tagged commit, not the current branch or tag object.
[[ $("$DIR/java-release-target.sh" 0.7.1) == "$RELEASE_SHA" ]]

for version in '' 0.7.2 0.07.1 0.7 0.7.1-rc1 main '0.7.1^{commit}'; do
  if "$DIR/java-release-target.sh" "$version" >/dev/null 2>&1; then
    echo "FAIL: accepted invalid or missing release '${version}'"
    exit 1
  fi
done
echo 'Java release target tests passed'
