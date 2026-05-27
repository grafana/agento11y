# @grafana/sigil-opencode

[OpenCode](https://opencode.ai) plugin that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `SIGIL_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

```sh
brew install grafana/grafana/sigil
sigil opencode
```

`sigil opencode` installs `@grafana/sigil-opencode` into OpenCode on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches OpenCode. Pass arguments to OpenCode after `--`, e.g. `sigil opencode -- run "say hi"`.

<details>
<summary>Manual plugin registration</summary>

```sh
opencode plugin @grafana/sigil-opencode --global
sigil login
```

The plugin reads `~/.config/sigil/config.env` on every session start, whether you start OpenCode with `sigil opencode` or plain `opencode`.

</details>

## 2. Credentials

When `sigil opencode` or `sigil login` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

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

When `SIGIL_AUTH_TENANT_ID` and `SIGIL_AUTH_TOKEN` are set, the plugin uses them for Sigil and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To include conversation text, add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

OpenCode redacts assistant and tool content before export. User prompt text is sent as-is when content capture allows it.

## 3. Verify

Run one OpenCode turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env`, run another turn, and check OpenCode stderr.

## All options

`~/.config/sigil/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|---|---|---|
| `SIGIL_ENDPOINT` | — | Sigil API URL. Find it at `/plugins/grafana-sigil-app`. Empty value disables the plugin. |
| `SIGIL_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `SIGIL_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the AI Observability latency and tool-call panels stay empty. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `SIGIL_OTEL_AUTH_TOKEN` | `SIGIL_AUTH_TOKEN` | Override the OTLP password. |
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `SIGIL_AGENT_NAME` | `opencode` | Agent name reported to Sigil. The plugin appends `:<mode>` for OpenCode's UI mode, such as `build` or `plan`. |
| `SIGIL_AGENT_VERSION` | — | Optional version string reported with the agent. |
| `SIGIL_DEBUG` | `false` | Log lifecycle events to stderr. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only `SIGIL_*` keys plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME` are honored — anything else (including stray `PATH=…` lines) is ignored.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.

## Development

```bash
pnpm install
pnpm --filter @grafana/sigil-opencode build
pnpm --filter @grafana/sigil-opencode test
```

The `@grafana/sigil-sdk-js` dependency resolves via pnpm workspace linking to `js/`.
