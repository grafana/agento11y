# @grafana/sigil-opencode

[OpenCode](https://opencode.ai) plugin that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `SIGIL_CONTENT_CAPTURE_MODE=full` or `no_tool_content` to include message content.

## 1. Install

```sh
brew install grafana/grafana/sigil
sigil opencode
```

`sigil opencode` installs `@grafana/sigil-opencode` into OpenCode on first run, prompts for credentials when they're missing, writes them to `~/.config/sigil/config.env`, and then launches OpenCode. Pass arguments to OpenCode after `--`, e.g. `sigil opencode -- run "say hi"`.

To install the plugin manually, run `opencode plugin @grafana/sigil-opencode --global` and then `sigil login` to populate `~/.config/sigil/config.env`. The plugin reads that file on every session start.

## 2. Credentials

Run `sigil login` and copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from two Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `SIGIL_ENDPOINT`
   - **Instance ID** → `SIGIL_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scope `sigil:write`.
   - Add a token. The `glc_…` value is shown once → `SIGIL_AUTH_TOKEN`.

Run `sigil login` later to update saved credentials.

<details>
<summary>Non-interactive config.env</summary>

Create or update `~/.config/sigil/config.env`:

```dotenv
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<instance-id>
SIGIL_AUTH_TOKEN=glc_...
```

</details>

When `SIGIL_AUTH_TENANT_ID` and `SIGIL_AUTH_TOKEN` are both set, Sigil generation export authenticates with the synthesized Basic auth header (`Basic base64(tenant:token)`). With only `SIGIL_AUTH_TENANT_ID` set, the `X-Scope-OrgID` header is sent on its own (tenant mode). Without either, no auth header is sent.

To include conversation text (with automatic secret redaction), add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run one OpenCode turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env`, run another turn, and check OpenCode stderr plus any Sigil logs.

## All options

`~/.config/sigil/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGIL_ENDPOINT` | — | Sigil URL (find it at `/plugins/grafana-sigil-app`). Empty value disables the plugin. |
| `SIGIL_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. Combined with `SIGIL_AUTH_TOKEN` becomes Basic auth. |
| `SIGIL_AUTH_TOKEN` | — | Cloud access policy token (`glc_…`). |
| `SIGIL_AGENT_NAME` | `opencode` | Agent name reported to Sigil. The plugin appends `:<mode>` (OpenCode's UI mode, e.g. `build`, `plan`) to this. |
| `SIGIL_AGENT_VERSION` | — | Optional version string reported with the agent. |
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `full`, `no_tool_content`, or `metadata_only`. |
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
