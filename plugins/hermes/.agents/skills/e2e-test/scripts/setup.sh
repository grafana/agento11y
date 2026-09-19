#!/usr/bin/env bash
# Build a throwaway hermes install with this plugin in it.
#
# The venv, isolated HERMES_HOME, and hook-probe plugin are created under
# $E2E_DIR (default /tmp/agento11y-hermes-e2e). Nothing touches ~/.hermes.
#
# usage: setup.sh [hermes-version]
set -euo pipefail

E2E_DIR="${E2E_DIR:-/tmp/agento11y-hermes-e2e}"
HERMES_VERSION="${1:-${HERMES_VERSION:-}}"
PY_VERSION="${PY_VERSION:-3.13}"

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$SKILL_DIR/../../.." && pwd)"

if ! command -v uv >/dev/null; then
  echo "setup: uv is required" >&2
  exit 1
fi

mkdir -p "$E2E_DIR"
cd "$E2E_DIR"

if [ ! -d .venv ]; then
  uv venv --python "$PY_VERSION"
fi

spec="hermes-agent[anthropic]"
[ -n "$HERMES_VERSION" ] && spec="hermes-agent[anthropic]==$HERMES_VERSION"

VIRTUAL_ENV="$E2E_DIR/.venv" uv pip install --quiet "$spec"
VIRTUAL_ENV="$E2E_DIR/.venv" uv pip install --quiet "$REPO_ROOT"

# Isolated hermes home. The plugin is an entry point, so only config.yaml
# decides whether it loads; `hermes plugins enable` never sees pip plugins.
HERMES_HOME="$E2E_DIR/home"
mkdir -p "$HERMES_HOME/plugins"
cp -R "$SKILL_DIR/scripts/probe-plugin" "$HERMES_HOME/plugins/hookdump"

cat > "$HERMES_HOME/config.yaml" <<'YAML'
model: claude-haiku-4-5-20251001
plugins:
  enabled:
    - agento11y
    - hookdump
YAML

# hermes reads $HERMES_HOME/.env and it OVERRIDES the process environment,
# so the provider key goes here and telemetry env stays on the command line.
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY" > "$HERMES_HOME/.env"
  chmod 600 "$HERMES_HOME/.env"
else
  echo "setup: ANTHROPIC_API_KEY is unset — write the provider key to $HERMES_HOME/.env before running" >&2
fi

echo "ready: $E2E_DIR"
# HERMES_HOME matters here: without it the plugin manager scans the real
# ~/.hermes, which is both wrong and often unreadable under a sandbox.
env -i PATH="$PATH" HOME="$HOME" HERMES_HOME="$HERMES_HOME" \
  "$E2E_DIR/.venv/bin/python" "$SKILL_DIR/scripts/check-install.py"
