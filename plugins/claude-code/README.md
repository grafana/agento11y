# Sigil for Claude Code

Sends every Claude Code turn to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): model, tokens, tools, timing, and optionally the conversation content.

## 1. Install and launch

```sh
brew install grafana/grafana/sigil
sigil claude
```

`sigil claude` registers the `sigil-cc` plugin on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches Claude Code.

<details>
<summary>Manual plugin registration</summary>

```
/plugin marketplace add grafana/sigil-sdk
/plugin install sigil-cc@grafana-sigil
```

</details>

## 2. Credentials

When `sigil claude` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `SIGIL_ENDPOINT`
   - **Instance ID** → `SIGIL_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `SIGIL_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`

Run `sigil login` later to update saved credentials.

<details>
<summary>Non-interactive config.env</summary>

Create or update `~/.config/sigil/config.env`:

```dotenv
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<instance-id>
SIGIL_AUTH_TOKEN=glc_...
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

</details>

To also send the conversation text (with automatic secret redaction), add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run any turn in Claude Code, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up:

```sh
SIGIL_DEBUG=true sigil claude  # one turn
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
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `SIGIL_TAGS` | — | `key=value,key=value` tags on every generation and as `sigil.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). |
| `SIGIL_USER_ID` | from `~/.claude.json` | Override the user id. |
| `SIGIL_USER_ID_SOURCE` | `email` | Which field to read from `~/.claude.json`: `email` or `accountUuid`. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |
| `SIGIL_AUTO_UPDATE` | `true` | Refresh the `sigil-cc` plugin automatically. Set `false` to pin the installed version. |
| `SIGIL_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Claude Code `PreToolUse` hook calls Sigil's `/api/v1/hooks:evaluate` and blocks tool calls denied by guard rules. |
| `SIGIL_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `SIGIL_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
