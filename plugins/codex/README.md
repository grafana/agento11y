# Sigil for Codex

Sends Codex turns to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): model, tokens, tools, timing, and optionally the conversation content.

> Experimental. Codex hooks and plugin lifecycle config are still feature-flagged.

## 1. Install

```sh
brew install grafana/grafana/sigil
```

## 2. Register the plugin

```sh
codex plugin marketplace add grafana/sigil-sdk
```

Enable it in `~/.codex/config.toml`:

```toml
[plugins."sigil-codex@grafana-sigil"]
enabled = true

[features]
codex_hooks = true
```

Older Codex builds use `hooks = true` and `plugin_hooks = true` instead of `codex_hooks`. Run `codex features list` to see which flag names your build accepts.

Restart Codex, open `/hooks`, and trust the four `sigil-codex@grafana-sigil` hooks (first-run review is expected).

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

Run one turn in Codex and let it finish — the plugin only exports completed turns, so `/exit` mid-turn means nothing is sent. Then open **AI Observability → Conversations** in Grafana Cloud.

If nothing shows up:

```sh
SIGIL_DEBUG=true codex  # one turn
tail -f ~/.local/state/sigil/logs/sigil.log
```

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
| `SIGIL_USER_ID` | — | Override the user id. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>` manually instead of relying on the auto-synthesised auth.

## Troubleshooting

| Symptom | Try |
|---|---|
| `/hooks` is empty | Enable the hook feature flags (`codex features list`), enable `plugins."sigil-codex@grafana-sigil"`, restart Codex. |
| Hooks listed but inactive | Open `/hooks` and trust each one. |
| Command not found | Reinstall: `brew install grafana/grafana/sigil`. Check `sigil --version`. |
| No data appears | Let turns finish (interrupted turns are not exported). Then check the debug log. |
| Subagent appears as a normal turn | Codex hook payloads don't always carry the parent link. Known limitation. |
