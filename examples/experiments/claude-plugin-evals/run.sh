#!/usr/bin/env bash
# Run the documented example and import even when its quality gate fails.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p results
output=$(mktemp -d "$PWD/results/run.XXXXXX")
exporter=${AGENTO11Y_BIN:-agento11y}

if claude plugin eval ./commit-messages \
  --trust-plugin --runs 3 \
  --model claude-sonnet-4-6 --judge-model claude-haiku-4-5 \
  --max-cost-usd 5 --no-publish --keep-temp \
  --output-dir "$output" --json "$output/results.json" "$@"; then
  eval_status=0
else
  eval_status=$?
fi

# Cancellation is not a quality-gate failure: do not start an upload.
if [[ "$eval_status" -ge 128 ]]; then exit "$eval_status"; fi

import_status=0
if [[ -f "$output/results.json" ]]; then
  # Claude currently retains sandboxes under /tmp even when TMPDIR is set.
  import_args=(--trace-root "${AGENTO11Y_EVAL_TRACE_ROOT:-/tmp}")
  if [[ "${AGENTO11Y_EVAL_INCLUDE_CONTENT:-0}" == 1 ]]; then import_args+=(--include-content); fi
  if [[ -n "${AGENTO11Y_EVAL_EXPECT_TENANT:-}" ]]; then import_args+=(--expect-tenant "$AGENTO11Y_EVAL_EXPECT_TENANT"); fi
  if "$exporter" claude eval import "$output/results.json" "${import_args[@]}"; then
    printf 'Results: %s\n' "$output"
  else
    import_status=$?
  fi
else
  printf 'No result document written; nothing to import.\n' >&2
  import_status=1
fi

# Never turn a failing Claude quality gate into a green CI job.
if [[ "$eval_status" -ne 0 ]]; then exit "$eval_status"; fi
exit "$import_status"
