#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
home=$(mktemp -d)
trap 'rm -rf "$home"' EXIT
uv_bin=$(command -v uv)
cache=$(uv --no-config cache dir)
pythons=$(uv --no-config python dir)

# Provider keys, telemetry credentials, user config, and PYTHONPATH must not reach checks.
# Expand the script's variables only in the clean child shell.
# shellcheck disable=SC2016
env -i PATH="$PATH" HOME="$home" TMPDIR="$home" UV_CACHE_DIR="$cache" UV_PYTHON_INSTALL_DIR="$pythons" \
  bash -eu -o pipefail -c '
    uv_bin=$1
    mode=$2
    python=$3
    run() { "$uv_bin" --no-config run --locked --isolated --no-env-file --python "$python" "$@"; }
    case "$mode" in
      format) run ruff format .; run ruff check --fix . ;;
      lint) run ruff format --check .; run ruff check . ;;
      typecheck) run ty check ;;
      test) run python -m pytest --cov ;;
      build) run python scripts/check-package.py ;;
      *) echo "Unknown check: $mode" >&2; exit 2 ;;
    esac
  ' bash "$uv_bin" "${1:?expected format, lint, typecheck, test, or build}" "${2:-3.11}"
