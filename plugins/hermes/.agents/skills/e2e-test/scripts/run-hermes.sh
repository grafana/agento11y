#!/usr/bin/env bash
# Run one hermes one-shot turn against the test install, with a deliberately
# built environment.
#
# usage: run-hermes.sh <mode> <prompt> [extra hermes args...]
#
# modes
#   full        generations + OTel, content capture left at the plugin default
#   metadata    same, with AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only
#   legacy-env  retired SIGIL_* names only, plus the branded OTLP alias, so the
#               compat shim and the endpoint override both get exercised
#   gen-only    generations only, no OTLP endpoint (expect a null trace_id)
#   otel-only   OTel only, no generation credentials (expect spans, no records)
#   sink        OTel only, pointed at the local OTLP sink: no Grafana account
#               needed, and it shows exactly which spans and metrics are emitted
#   none        no telemetry env at all (expect one warning and no data)
#
# env
#   E2E_DIR             where setup.sh installed everything, default
#                       /tmp/agento11y-hermes-e2e
#   AGENTO11Y_ENV_FILE  KEY=VALUE file with the AGENTO11Y_* / OTEL_* block from
#                       your stack's Agent Observability setup page. Falls back
#                       to the AGENTO11Y_*/OTEL_* values already in the shell.
#   MODEL / PROVIDER    default claude-haiku-4-5-20251001 on anthropic
#   AGENT_NAME          default hermes-e2e; the mode becomes the agent version,
#                       so runs are trivially separable in queries
#
# The environment is built with env -i, so a knob set in the calling shell does
# not reach hermes unless it is named in PASSTHROUGH below. Each inherited value
# is echoed, because a silently dropped knob makes a run look like a pass:
#   AGENTO11Y_HERMES_SAMPLE_RATE      0 records nothing
#   AGENTO11Y_HERMES_MAX_CHARS        per-string cap on tool args and results
#   AGENTO11Y_HERMES_OTEL_AUTO        false leaves provider installation alone
#   HERMES_PLUGIN_PAYLOAD_MAX_CHARS   hermes's own cap, which decides how much
#                                     of the system prompt and the tool schemas
#                                     reach the hooks at all
set -uo pipefail

MODE="${1:?usage: run-hermes.sh <mode> <prompt> [args...]}"; shift
PROMPT="${1:?usage: run-hermes.sh <mode> <prompt> [args...]}"; shift

E2E_DIR="${E2E_DIR:-/tmp/agento11y-hermes-e2e}"
MODEL="${MODEL:-claude-haiku-4-5-20251001}"
PROVIDER="${PROVIDER:-anthropic}"
AGENT_NAME="${AGENT_NAME:-hermes-e2e}"
SINK_PORT="${SINK_PORT:-8801}"

read_setting() {
  # $1 = variable name. The env file wins over the ambient shell, because the
  # ambient shell often carries a mix of current and retired names.
  local name="$1" value=""
  if [ -n "${AGENTO11Y_ENV_FILE:-}" ] && [ -f "$AGENTO11Y_ENV_FILE" ]; then
    value=$(grep -E "^${name}=" "$AGENTO11Y_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"')
  fi
  [ -z "$value" ] && value="$(printenv "$name" 2>/dev/null || true)"
  printf '%s' "$value"
}

GEN_ENDPOINT=$(read_setting AGENTO11Y_ENDPOINT)
TENANT=$(read_setting AGENTO11Y_AUTH_TENANT_ID)
TOKEN=$(read_setting AGENTO11Y_AUTH_TOKEN)
OTLP_ENDPOINT=$(read_setting OTEL_EXPORTER_OTLP_ENDPOINT)
[ -z "$OTLP_ENDPOINT" ] && OTLP_ENDPOINT=$(read_setting AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT)
OTLP_HEADERS=$(read_setting OTEL_EXPORTER_OTLP_HEADERS)

GENERATIONS=(
  "AGENTO11Y_ENDPOINT=$GEN_ENDPOINT"
  "AGENTO11Y_PROTOCOL=http"
  "AGENTO11Y_AUTH_MODE=basic"
  "AGENTO11Y_AUTH_TENANT_ID=$TENANT"
  "AGENTO11Y_AUTH_TOKEN=$TOKEN"
)
OTEL=(
  "OTEL_EXPORTER_OTLP_ENDPOINT=$OTLP_ENDPOINT"
  "OTEL_EXPORTER_OTLP_HEADERS=$OTLP_HEADERS"
)

COMMON=(
  "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin"
  "HOME=$HOME"
  "TERM=dumb"
  "E2E_DIR=$E2E_DIR"
  "HERMES_HOME=$E2E_DIR/home"
  "AGENTO11Y_DEBUG=true"
  "AGENTO11Y_AGENT_NAME=$AGENT_NAME"
  "AGENTO11Y_AGENT_VERSION=$MODE"
)
PASSTHROUGH=(
  AGENTO11Y_HERMES_SAMPLE_RATE
  AGENTO11Y_HERMES_MAX_CHARS
  AGENTO11Y_HERMES_OTEL_AUTO
  HERMES_PLUGIN_PAYLOAD_MAX_CHARS
)
for name in "${PASSTHROUGH[@]}"; do
  value="$(printenv "$name" 2>/dev/null || true)"
  if [ -n "$value" ]; then
    COMMON+=("$name=$value")
    echo "run-hermes: inherited $name=$value" >&2
  fi
done

case "$MODE" in
  full)       ENVV=("${COMMON[@]}" "${GENERATIONS[@]}" "${OTEL[@]}") ;;
  metadata)   ENVV=("${COMMON[@]}" "${GENERATIONS[@]}" "${OTEL[@]}" "AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only") ;;
  legacy-env) ENVV=("${COMMON[@]}"
                "SIGIL_ENDPOINT=$GEN_ENDPOINT" "SIGIL_PROTOCOL=http" "SIGIL_AUTH_MODE=basic"
                "SIGIL_TENANT_ID=$TENANT" "SIGIL_AUTH_TOKEN=$TOKEN"
                "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=$OTLP_ENDPOINT") ;;
  gen-only)   ENVV=("${COMMON[@]}" "${GENERATIONS[@]}") ;;
  otel-only)  ENVV=("${COMMON[@]}" "${OTEL[@]}") ;;
  sink)       ENVV=("${COMMON[@]}"
                "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:$SINK_PORT"
                "OTEL_EXPORTER_OTLP_INSECURE=true") ;;
  none)       ENVV=("${COMMON[@]}") ;;
  *) echo "run-hermes: unknown mode '$MODE'" >&2; exit 2 ;;
esac

rm -f "$E2E_DIR/hooks.jsonl"
exec env -i "${ENVV[@]}" \
  "$E2E_DIR/.venv/bin/hermes" -m "$MODEL" --provider "$PROVIDER" -z "$PROMPT" "$@"
