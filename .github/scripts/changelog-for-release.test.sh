#!/usr/bin/env bash
# Tests for changelog-for-release.sh. Exits non-zero on any failure.

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
CHANGELOG="${DIR}/changelog-for-release.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail=0
pass=0

assert_contains() {
  local desc="$1" needle="$2" haystack="$3"
  if grep -Fq -- "$needle" <<<"$haystack"; then
    pass=$((pass + 1))
  else
    echo "FAIL ${desc}: missing '${needle}'"
    echo "--- output ---"
    printf '%s\n' "$haystack"
    echo "--------------"
    fail=$((fail + 1))
  fi
}

assert_not_contains() {
  local desc="$1" needle="$2" haystack="$3"
  if grep -Fq -- "$needle" <<<"$haystack"; then
    echo "FAIL ${desc}: unexpected '${needle}'"
    echo "--- output ---"
    printf '%s\n' "$haystack"
    echo "--------------"
    fail=$((fail + 1))
  else
    pass=$((pass + 1))
  fi
}

assert_count() {
  local desc="$1" needle="$2" want="$3" haystack="$4" got
  got=$(grep -F -c -- "$needle" <<<"$haystack")
  if [[ "$got" == "$want" ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL ${desc}: '${needle}' appeared ${got} times, want ${want}"
    echo "--- output ---"
    printf '%s\n' "$haystack"
    echo "--------------"
    fail=$((fail + 1))
  fi
}

commit_plugin() {
  local msg="$1"
  printf '%s\n' "$msg" >> plugins/agento11y/file.txt
  git add plugins/agento11y/file.txt
  git commit -q -m "$msg"
}

cd "$TMP" || exit 1
git init -q
git config user.name 'Test User'
git config user.email 'test@example.com'

mkdir -p plugins/agento11y plugins/pi
printf 'seed\n' > plugins/agento11y/file.txt
git add plugins/agento11y/file.txt
git commit -q -m 'feat(plugins/agento11y): initial release'
git tag plugins/agento11y/v0.1.0

commit_plugin 'feat!: rename config key'
commit_plugin 'chore!: drop old flag'
commit_plugin 'fix(plugins/agento11y): repair login'
commit_plugin 'docs: update sigil docs'
printf 'pi\n' > plugins/pi/file.txt
git add plugins/pi/file.txt
git commit -q -m 'feat(plugins/pi): unrelated plugin change'

out=$("$CHANGELOG" 0.2.0 plugins/agento11y plugins/agento11y)
assert_contains 'breaking section exists' '### Breaking Changes' "$out"
assert_contains 'breaking feat listed' '- rename config key' "$out"
assert_contains 'breaking chore listed' '- drop old flag' "$out"
assert_count 'breaking feat is not duplicated' '- rename config key' 1 "$out"
assert_count 'breaking chore is not contradicted by fallback' '- drop old flag' 1 "$out"
assert_not_contains 'breaking feat excluded from feature section' '### Features' "$out"
assert_not_contains 'breaking changes count as user-facing' '_No user-facing changes._' "$out"
assert_contains 'scoped fix keeps scope' '- **plugins/agento11y**: repair login' "$out"
assert_contains 'docs section exists' '### Documentation' "$out"
assert_contains 'docs entry listed' '- update sigil docs' "$out"
assert_not_contains 'other plugin path excluded' 'unrelated plugin change' "$out"

git tag plugins/agento11y/v0.2.0
commit_plugin 'chore: internal cleanup'

out=$("$CHANGELOG" 0.3.0 plugins/agento11y plugins/agento11y)
assert_contains 'non-user-facing fallback' '_No user-facing changes._' "$out"
assert_not_contains 'non-breaking chore omitted' 'internal cleanup' "$out"

# Multi-path SDK: tag prefix differs from the source paths, and a release
# spans several directories (mirrors the Python/JS layouts).
mkdir -p python python-providers/openai python-frameworks/langchain other
printf 'seed\n' > python/file.txt
git add python/file.txt
git commit -q -m 'feat(sdk-python): seed core'
git tag sdk-python/v0.1.0

printf 'core\n' >> python/file.txt
git add python/file.txt
git commit -q -m 'feat(sdk-python): core export change'
printf 'prov\n' > python-providers/openai/file.txt
git add python-providers/openai/file.txt
git commit -q -m 'fix(providers/openai): patch wrapper'
printf 'fw\n' > python-frameworks/langchain/file.txt
git add python-frameworks/langchain/file.txt
git commit -q -m 'feat(frameworks/langchain): new adapter'
printf 'unrelated\n' > other/file.txt
git add other/file.txt
git commit -q -m 'feat(other): outside the python tree'

out=$("$CHANGELOG" 0.2.0 sdk-python python python-providers python-frameworks)
assert_contains 'core path included' '- **sdk-python**: core export change' "$out"
assert_contains 'providers path included' '- **providers/openai**: patch wrapper' "$out"
assert_contains 'frameworks path included' '- **frameworks/langchain**: new adapter' "$out"
assert_not_contains 'path outside the SDK excluded' 'outside the python tree' "$out"
assert_not_contains 'seed before previous tag excluded' 'seed core' "$out"

echo "passed: ${pass}, failed: ${fail}"
[[ $fail -eq 0 ]]
