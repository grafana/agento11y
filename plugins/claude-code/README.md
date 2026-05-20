# Sigil for Claude Code

Sends every Claude Code turn to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): model, tokens, tools, timing, and optionally the conversation content.

## 1. Install

```sh
brew install grafana/grafana/sigil
```

## 2. Register the plugin

Either run:

```sh
sigil claude
```

…which installs the `sigil-cc` plugin on first run and launches Claude Code. Or from inside Claude Code:

```
/plugin marketplace add grafana/sigil-sdk
/plugin install sigil-cc@grafana-sigil
```

## 3. Add your credentials

All Sigil connection details live at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `SIGIL_ENDPOINT`
   - **Instance ID** → `SIGIL_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `SIGIL_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`

Save them to `~/.config/sigil/config.env` (shared by all three host plugins):

```dotenv
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<instance-id>
SIGIL_AUTH_TOKEN=glc_...
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

To also send the conversation text (with automatic secret redaction), add:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

## 4. Verify

Run any turn in Claude Code, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up:

```sh
SIGIL_DEBUG=true claude  # one turn
tail -f ~/.local/state/sigil/logs/sigil.log
```

Common culprits: `sigil --version` doesn't work (binary not on `PATH`), a missing token, or a token without the `sigil:write` scope.

## All options

| Variable | Default | Description |
|---|---|---|
| `SIGIL_ENDPOINT` | — | Sigil API URL. Find it at `/plugins/grafana-sigil-app`. |
| `SIGIL_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `SIGIL_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the AI Observability latency and tool-call panels stay empty. |
| `SIGIL_OTEL_AUTH_TOKEN` | `SIGIL_AUTH_TOKEN` | Override the OTel password. |
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, or `full`. |
| `SIGIL_TAGS` | — | `key=value,key=value` tags added to every generation. |
| `SIGIL_USER_ID` | from `~/.claude.json` | Override the user id. |
| `SIGIL_USER_ID_SOURCE` | `email` | Which field to read from `~/.claude.json`: `email` or `accountUuid`. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>` manually instead of relying on the auto-synthesised auth.
