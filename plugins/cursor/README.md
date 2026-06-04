# Sigil for Cursor

Sends Cursor agent generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): prompts, replies, tool calls, and token usage.

## 1. Install the shared binary

```sh
brew install grafana/grafana/sigil
```

Cursor does not have a `sigil cursor` launcher. Install the binary, register the Cursor plugin, then use `sigil login` or `~/.config/sigil/config.env` for credentials.

## 2. Register the plugin

In Cursor:

```
/add-plugin grafana/sigil-sdk
```

## 3. Add your credentials

Run `sigil login` from a terminal. The prompt asks for values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `SIGIL_ENDPOINT`
   - **Instance ID** → `SIGIL_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `SIGIL_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`

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

To also send the conversation text, add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

Cursor content is not passed through the shared redactor before export. Avoid `full` when prompts, replies, or tool output may contain secrets.

## 4. Verify

Use Cursor's agent for one turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, add `SIGIL_DEBUG=true` to `~/.config/sigil/config.env` (Cursor launches from the GUI, so a shell env var won't reach the hooks) and tail the log:

```sh
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
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `SIGIL_TAGS` | — | `key=value,key=value` tags on every generation and as `sigil.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). Built-ins (`git.branch`, `cwd`, `subagent`) win on generation-export tag collision. |
| `SIGIL_USER_ID` | from Cursor | Override the user id. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |
| `SIGIL_BIN` | auto | Override the binary path if you installed `sigil` somewhere unusual. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
