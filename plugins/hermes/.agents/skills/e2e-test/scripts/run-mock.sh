#!/usr/bin/env bash
# Drive one hermes turn against the scripted mock provider.
#
# usage: run-mock.sh <mock-script> <prompt> [extra hermes args...]
#   e.g. run-mock.sh "429,429,ok"        "reply with OK"
#        run-mock.sh "empty"             "reply with OK"   # retry exhaustion
#        run-mock.sh "scratchpad"        "reply with OK"   # thinking-budget path
#
# The mock needs its own HERMES_HOME because $HERMES_HOME/.env overrides the
# process env, so the real provider key would otherwise win.
set -uo pipefail

SCRIPT="${1:?usage: run-mock.sh <mock-script> <prompt> [args...]}"; shift
PROMPT="${1:?usage: run-mock.sh <mock-script> <prompt> [args...]}"; shift

E2E_DIR="${E2E_DIR:-/tmp/agento11y-hermes-e2e}"
MOCK_PORT="${MOCK_PORT:-8799}"
AGENT_NAME="${AGENT_NAME:-hermes-e2e}"
SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

read_setting() {
  local name="$1" value=""
  if [ -n "${AGENTO11Y_ENV_FILE:-}" ] && [ -f "$AGENTO11Y_ENV_FILE" ]; then
    value=$(grep -E "^${name}=" "$AGENTO11Y_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"')
  fi
  [ -z "$value" ] && value="$(printenv "$name" 2>/dev/null || true)"
  printf '%s' "$value"
}

# The setup page hands out the standard name, but a stored credential file may
# hold the branded alias instead. Without this fallback the mock runs get no
# OTLP endpoint, no provider is installed, and every record comes back with a
# null trace_id, which hides the span half of a failure.
OTLP_ENDPOINT=$(read_setting OTEL_EXPORTER_OTLP_ENDPOINT)
[ -z "$OTLP_ENDPOINT" ] && OTLP_ENDPOINT=$(read_setting AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT)

MOCK_HOME="$E2E_DIR/home-mock"
mkdir -p "$MOCK_HOME/plugins"
cp -R "$SKILL_DIR/scripts/probe-plugin" "$MOCK_HOME/plugins/hookdump"
cat > "$MOCK_HOME/config.yaml" <<'YAML'
model: mock-model
plugins:
  enabled:
    - agento11y
    - hookdump
YAML
printf 'OPENAI_API_KEY=mock-key\nOPENAI_BASE_URL=http://127.0.0.1:%s/v1\n' "$MOCK_PORT" > "$MOCK_HOME/.env"
chmod 600 "$MOCK_HOME/.env"

pkill -f "mock-provider.py $MOCK_PORT" 2>/dev/null
rm -f "$E2E_DIR/mock.log" "$E2E_DIR/hooks.jsonl"
E2E_DIR="$E2E_DIR" MOCK_SCRIPT="$SCRIPT" MOCK_LOG="$E2E_DIR/mock.log" \
  nohup "$E2E_DIR/.venv/bin/python" "$SKILL_DIR/scripts/mock-provider.py" "$MOCK_PORT" \
  >"$E2E_DIR/mock.out" 2>&1 &
sleep 1

env -i \
  PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin \
  HOME="$HOME" TERM=dumb \
  E2E_DIR="$E2E_DIR" HERMES_HOME="$MOCK_HOME" \
  AGENTO11Y_DEBUG=true \
  AGENTO11Y_AGENT_NAME="$AGENT_NAME" \
  AGENTO11Y_AGENT_VERSION="mock-${SCRIPT//,/ }" \
  AGENTO11Y_ENDPOINT="$(read_setting AGENTO11Y_ENDPOINT)" \
  AGENTO11Y_PROTOCOL=http AGENTO11Y_AUTH_MODE=basic \
  AGENTO11Y_AUTH_TENANT_ID="$(read_setting AGENTO11Y_AUTH_TENANT_ID)" \
  AGENTO11Y_AUTH_TOKEN="$(read_setting AGENTO11Y_AUTH_TOKEN)" \
  OTEL_EXPORTER_OTLP_ENDPOINT="$OTLP_ENDPOINT" \
  OTEL_EXPORTER_OTLP_HEADERS="$(read_setting OTEL_EXPORTER_OTLP_HEADERS)" \
  "$E2E_DIR/.venv/bin/hermes" -m mock-model --provider openai-api -z "$PROMPT" "$@"
rc=$?

pkill -f "mock-provider.py $MOCK_PORT" 2>/dev/null
echo "--- provider calls"
grep CALL "$E2E_DIR/mock.log" 2>/dev/null
exit $rc
