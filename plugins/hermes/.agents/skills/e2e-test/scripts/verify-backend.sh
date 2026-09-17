#!/usr/bin/env bash
# Pull what the backend stored for a run and check it field by field.
#
# usage: verify-backend.sh [conversation-id]
#   With no argument, picks the newest conversation whose id looks like a hermes
#   session id (YYYYMMDD_HHMMSS_hex).
#
# env
#   GCX_CONTEXT   required: the gcx context pointing at the stack you exported to
#   AGENT_NAME    agent name used by the run, default hermes-e2e
#   E2E_DIR       where the JSON dumps are written, default /tmp/agento11y-hermes-e2e
#
# Needs the gcx CLI. Exporting is asynchronous, so allow a few seconds after a
# run before expecting records.
set -uo pipefail

: "${GCX_CONTEXT:?set GCX_CONTEXT to the gcx context for your stack}"
E2E_DIR="${E2E_DIR:-/tmp/agento11y-hermes-e2e}"
AGENT_NAME="${AGENT_NAME:-hermes-e2e}"
SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GCX=(gcx --context "$GCX_CONTEXT")

CONV="${1:-}"
if [ -z "$CONV" ]; then
  CONV=$("${GCX[@]}" agento11y conversations list --limit 25 2>/dev/null \
    | grep -oE '^[0-9]{8}_[0-9]{6}_[0-9a-f]+' | head -1)
fi
if [ -z "$CONV" ]; then
  echo "verify-backend: no hermes-shaped conversation found; wait a few seconds and retry" >&2
  exit 1
fi

echo "== conversation $CONV"
"${GCX[@]}" agento11y conversations get "$CONV" -o json > "$E2E_DIR/conversation.json" || exit 1
python3 "$SKILL_DIR/scripts/check-generations.py" "$E2E_DIR/conversation.json"

echo
echo "== agent catalog entry ($AGENT_NAME)"
"${GCX[@]}" agento11y agents get "$AGENT_NAME" -o json 2>/dev/null \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({k: d.get(k) for k in ("declared_version_latest","generation_count","tool_count","system_prompt_prefix","token_estimate","models")}, indent=1))'

# Every metric query looks back an hour instead of asking for the value now. An
# instant query only sees a series that got a sample inside the lookback window,
# so a run that finished ten minutes ago reads as "No data" and looks like a
# plugin that exports no metrics.
WINDOW="${METRICS_WINDOW:-1h}"
echo
echo "== metrics (client-side, emitted by the plugin, last $WINDOW)"
"${GCX[@]}" metrics query "count by (__name__) (last_over_time({__name__=~\"gen_ai_client.*\", gen_ai_agent_name=\"$AGENT_NAME\"}[$WINDOW]))" 2>&1 | head -14
echo
echo "== token usage by type (last $WINDOW)"
"${GCX[@]}" metrics query "sum by (gen_ai_token_type) (last_over_time(gen_ai_client_token_usage_sum{gen_ai_agent_name=\"$AGENT_NAME\"}[$WINDOW]))" 2>&1 | head -10
echo
echo "== cost (computed backend-side from model + tokens, last $WINDOW)"
"${GCX[@]}" metrics query "sum by (gen_ai_request_model) (last_over_time(agento11y_generation_cost_usd_total{gen_ai_agent_name=\"$AGENT_NAME\"}[$WINDOW]))" 2>&1 | head -10

echo
echo "== tool execution spans"
"${GCX[@]}" traces query "{resource.service.name=\"hermes\" && name=~\"execute_tool.*\"}" --since 1h --limit 10 2>&1 | head -8
echo "(fewer spans here than tool calls usually means sampling on the receiving end;"
echo " confirm the exporter with the sink mode instead of assuming a plugin bug)"

echo
echo "next: fetch a trace and inspect attributes"
echo "  gcx --context $GCX_CONTEXT traces get <trace-id> -o json > $E2E_DIR/trace.json"
echo "  python3 $SKILL_DIR/scripts/show-spans.py $E2E_DIR/trace.json"
