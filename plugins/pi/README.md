# @grafana/sigil-pi

[Pi](https://github.com/badlogic/pi) agent extension that records LLM generations to Grafana AI observability.

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `contentCapture` to `full` or `no_tool_content` to include message content.

## Install

**Directly via pi**:

```bash
pi install npm:@grafana/sigil-pi
```

**Via the [sigil launcher](../sigil/README.md)** (auto-bootstraps on first run):

```bash
sigil pi
```

The launcher installs Sigil plugin once if the extension isn't registered yet. Subsequent runs skip the install.

## Grafana Cloud credentials

Skip this if you're not using Grafana Cloud — see [Auth modes](#auth-modes) for `tenant` / `bearer` / `none`.

The same Instance ID and access token are used for both Sigil and OTLP. OTLP is required for the AI Observability UI's latency, tool-call, and throughput panels.

1. **API URL and Instance ID** — in **Observability → AI Observability → Configuration**:
   - **API URL** → `endpoint` (e.g. `https://sigil-prod-<region>.grafana.net`)
   - **Instance ID** → `auth.user` and `otlp.basicUser` (numeric stack ID)
2. **Access policy token** — in **Administration → Cloud access policies**, create a policy with scopes `metrics:write`, `traces:write`, and `sigil:write`, then add a token. The `glc_…` value is shown once — goes into `auth.password` and `otlp.basicPassword`.
3. **OTLP endpoint** — Cloud Portal → your stack → **OpenTelemetry** card. Copy the OTLP endpoint URL → `otlp.endpoint` (e.g. `https://otlp-gateway-prod-<region>.grafana.net/otlp`).

## Configure

Create `~/.config/sigil-pi/config.json`:

```json
{
  "endpoint": "https://sigil.example.com",
  "auth": {
    "mode": "basic",
    "user": "123456",
    "password": "${SIGIL_AUTH_TOKEN}"
  },
  "otlp": {
    "endpoint": "https://otlp-gateway.grafana.net/otlp",
    "basicUser": "123456",
    "basicPassword": "${SIGIL_AUTH_TOKEN}"
  }
}
```

String values support `${ENV_VAR}` interpolation.

### Auth modes

- **basic** — HTTP Basic + `X-Scope-OrgID` (Grafana Cloud): `{ "mode": "basic", "user": "<instance-id>", "password": "${SIGIL_AUTH_TOKEN}" }`
- **tenant** — `X-Scope-OrgID` only: `{ "mode": "tenant", "tenantId": "my-tenant" }`
- **bearer** — `Authorization: Bearer`: `{ "mode": "bearer", "bearerToken": "${SIGIL_TOKEN}" }`
- **none** — no auth (default)

### Redaction

Generations are scrubbed for secrets before they leave the process; matches become `[REDACTED:<id>]`. The sanitizer covers Grafana Cloud / cloud provider / SaaS tokens, PEM-encoded private keys, database connection strings, `KEY=value` env-style pairs, bearer tokens, and email addresses.

User-role messages are scrubbed too — set `redaction.redactInputMessages: false` to leave them alone, or `redaction.enabled: false` to disable redaction entirely.

### Options

| Field | Default | Description |
|-------|---------|-------------|
| `endpoint` | — | Sigil URL |
| `auth.mode` | `"none"` | `basic`, `tenant`, `bearer`, or `none` |
| `auth.user` / `auth.password` | — | Basic auth credentials |
| `auth.tenantId` | `auth.user` in basic mode | `X-Scope-OrgID` header (required in `tenant` mode) |
| `auth.bearerToken` | — | Required in `bearer` mode |
| `agentName` | `"pi"` | Agent name reported to Sigil |
| `agentVersion` | auto-detected | Pi agent version |
| `contentCapture` | `"metadata_only"` | `full`, `no_tool_content`, or `metadata_only` |
| `debug` | `false` | Log lifecycle events to stderr |
| `otlp.endpoint` | — | OTLP HTTP endpoint |
| `otlp.basicUser` / `otlp.basicPassword` | — | OTLP Basic auth |
| `otlp.bearerToken` | — | OTLP Bearer token |
| `otlp.headers` | — | Custom OTLP headers |
| `redaction.enabled` | `true` | Master switch for redaction |
| `redaction.redactInputMessages` | `true` | Scrub user-role content too |
| `redaction.redactEmailAddresses` | `true` | Scrub generic email addresses |

### Environment variables

Any field can be overridden via env var. When launched via `sigil pi`, vars in `~/.config/sigil/config.env` are loaded automatically.

| Variable | Sets |
|----------|------|
| `SIGIL_ENDPOINT` | `endpoint` |
| `SIGIL_AUTH_TENANT_ID` + `SIGIL_AUTH_TOKEN` | Basic auth for Sigil and OTLP (Grafana Cloud pattern) |
| `SIGIL_AGENT_NAME` / `SIGIL_AGENT_VERSION` | `agentName` / `agentVersion` |
| `SIGIL_CONTENT_CAPTURE_MODE` | `contentCapture` |
| `SIGIL_DEBUG` | `debug` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `otlp.endpoint` |
| `SIGIL_OTEL_AUTH_TOKEN` | OTLP basic-auth password (falls back to `SIGIL_AUTH_TOKEN`) |

Explicit `auth.mode` in the config file always wins over env-derived auth.

## What gets sent

Every generation carries model, token usage (input/output/cache), cost, stop reason, tool names and durations, conversation ID, and turn timing. Message content depends on `contentCapture`:

| Mode | Adds |
|------|------|
| `metadata_only` (default) | nothing — content is stripped by the SDK |
| `no_tool_content` | assistant text and thinking |
| `full` | assistant text, thinking, tool call arguments, tool results |

When `otlp` is configured, the SDK also exports `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, and `gen_ai.client.tool_calls_per_operation` histograms (labelled by provider/model/agent) plus one trace span per generation. Resource `service.name` is `sigil-pi`.
