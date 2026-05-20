# @grafana/sigil-pi

[Pi](https://github.com/badlogic/pi) agent extension that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Flip `contentCapture` to `full` or `no_tool_content` to include message content.

## 1. Install

```sh
pi install npm:@grafana/sigil-pi
```

Or use the [sigil launcher](../sigil/README.md) which installs the extension on first run:

```sh
brew install grafana/grafana/sigil
sigil pi
```

## 2. Add your Grafana Cloud credentials

All Sigil connection details live at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Skip this section if you're running against a self-hosted Sigil — see [Auth modes](#auth-modes) below.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `endpoint`
   - **Instance ID** → `auth.user` and `otlp.basicUser`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `auth.password` and `otlp.basicPassword`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `otlp.endpoint`

## 3. Configure

Create `~/.config/sigil-pi/config.json`:

```json
{
  "endpoint": "https://sigil-prod-<region>.grafana.net",
  "auth": {
    "mode": "basic",
    "user": "<instance-id>",
    "password": "${SIGIL_AUTH_TOKEN}"
  },
  "otlp": {
    "endpoint": "https://otlp-gateway-prod-<region>.grafana.net/otlp",
    "basicUser": "<instance-id>",
    "basicPassword": "${SIGIL_AUTH_TOKEN}"
  }
}
```

String values support `${ENV_VAR}` interpolation, so the token stays out of the file.

To include conversation text (with automatic secret redaction), add `"contentCapture": "full"` to the config.

### Auth modes

- `basic` — Grafana Cloud: `{ "mode": "basic", "user": "<instance-id>", "password": "${SIGIL_AUTH_TOKEN}" }`
- `tenant` — `X-Scope-OrgID` only: `{ "mode": "tenant", "tenantId": "my-tenant" }`
- `bearer` — `{ "mode": "bearer", "bearerToken": "${SIGIL_TOKEN}" }`
- `none` — no auth (default)

## Redaction

Before any generation leaves the process, the SDK scrubs known token formats, PEM private keys, database URLs, `KEY=value` pairs, bearer tokens, and email addresses. Matches become `[REDACTED:<id>]`.

User-role messages are scrubbed too. Set `redaction.redactInputMessages: false` to leave them alone, or `redaction.enabled: false` to disable redaction entirely.

## Guards

Guards block tool calls before they execute (e.g. refuse a `bash` invocation matching a deny rule). They're off by default:

```sh
SIGIL_GUARDS_ENABLED=true pi
```

By default, transport errors and timeouts let the tool through. Set `SIGIL_GUARDS_FAIL_OPEN=false` to block on errors instead.

## All options

| Field | Default | Description |
|-------|---------|-------------|
| `endpoint` | — | Sigil URL (find it at `/plugins/grafana-sigil-app`) |
| `auth.mode` | `"none"` | `basic`, `tenant`, `bearer`, or `none` |
| `auth.user` / `auth.password` | — | Basic auth credentials |
| `auth.tenantId` | `auth.user` in basic mode | `X-Scope-OrgID` header |
| `auth.bearerToken` | — | Bearer token |
| `agentName` | `"pi"` | Agent name reported to Sigil |
| `contentCapture` | `"metadata_only"` | `full`, `no_tool_content`, or `metadata_only` |
| `debug` | `false` | Log lifecycle events to stderr |
| `otlp.endpoint` | — | OTLP HTTP endpoint |
| `otlp.basicUser` / `otlp.basicPassword` | — | OTLP Basic auth |
| `otlp.bearerToken` | — | OTLP Bearer token |
| `redaction.enabled` | `true` | Master switch for redaction |
| `redaction.redactInputMessages` | `true` | Scrub user-role content too |
| `guards.enabled` | `false` | Evaluate `tool_call` requests against Sigil policy |
| `guards.timeoutMs` | `1500` | Per-call timeout for guard requests |
| `guards.failOpen` | `true` | Allow tools through when guard checks fail |

Every field can be overridden via env var. When launched via `sigil pi`, vars in `~/.config/sigil/config.env` are loaded automatically.

| Variable | Sets |
|----------|------|
| `SIGIL_ENDPOINT` | `endpoint` |
| `SIGIL_AUTH_TENANT_ID` + `SIGIL_AUTH_TOKEN` | Basic auth for Sigil and OTLP |
| `SIGIL_CONTENT_CAPTURE_MODE` | `contentCapture` |
| `SIGIL_DEBUG` | `debug` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `otlp.endpoint` |
| `SIGIL_GUARDS_ENABLED` | `guards.enabled` |
