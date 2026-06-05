# Sigil for Codex

Sends Codex turns to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): model, tokens, tools, timing, and optionally the conversation content.

> Experimental. Codex hooks and plugin lifecycle config are still feature-flagged.

## 1. Install and launch

**macOS** (Homebrew):

```sh
brew install grafana/grafana/sigil
sigil codex
```

**Linux and Windows** (or any platform with Go 1.25+):

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
sigil codex
```

`go install` puts `sigil` in `go env GOPATH`/bin (or `GOBIN`); add that to `PATH`. See the [`sigil` binary README](../sigil/README.md#install) for details.

`sigil codex` registers `sigil-codex@grafana-sigil` on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches Codex.

On first launch only, open `/hooks` inside Codex and trust each `sigil-codex@grafana-sigil` hook. Codex requires this manual review after plugin install.

<details>
<summary>Manual plugin registration</summary>

```sh
codex plugin marketplace add grafana/sigil-sdk
codex plugin add sigil-codex@grafana-sigil
```

On current Codex builds the `hooks` and `plugin_hooks` features are stable by default (`codex features list` confirms this), so no `config.toml` edits are needed. Older builds gated this on feature flags — if `/hooks` is empty after install, add the following to `~/.codex/config.toml`:

```toml
[plugins."sigil-codex@grafana-sigil"]
enabled = true

[features]
codex_hooks = true
```

Older Codex builds use `hooks = true` and `plugin_hooks = true` instead of `codex_hooks`. Run `codex features list` to see which flag names your build accepts.

Restart Codex, open `/hooks`, and trust the five `sigil-codex@grafana-sigil` hooks (first-run review is expected).

</details>

## 2. Credentials

When `sigil codex` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

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

Run one turn in Codex and let it finish — the plugin only exports completed turns, so `/exit` mid-turn means nothing is sent. Then open **AI Observability → Conversations** in Grafana Cloud.

If nothing shows up:

```sh
SIGIL_DEBUG=true sigil codex  # one turn
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
| `SIGIL_TAGS` | — | `key=value,key=value` tags on every generation and as `sigil.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). |
| `SIGIL_USER_ID` | — | Override the user id. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |
| `SIGIL_GUARDS_ENABLED` | `false` | Enable Codex `PreToolUse` guards against Sigil rules. |
| `SIGIL_GUARDS_FAIL_OPEN` | `true` | Allow the tool call when the guard request fails (set `false` for fail-closed). |
| `SIGIL_GUARDS_TIMEOUT_MS` | `1500` | Per-call guard timeout. |
| `SIGIL_AUTO_UPDATE` | `true` | Refresh the `sigil-codex` plugin automatically. Set `false` to pin the installed version. |

Guards only intercept tool calls that Codex routes through `PreToolUse` — Bash, the `apply_patch` variants, and MCP tools. See the [Codex hooks docs](https://developers.openai.com/codex/hooks) for the supported set.

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

## Troubleshooting

| Symptom | Try |
|---|---|
| `/hooks` is empty | Enable the hook feature flags (`codex features list`), enable `plugins."sigil-codex@grafana-sigil"`, restart Codex. |
| Hooks listed but inactive | Open `/hooks` and trust each one. |
| Command not found | Reinstall `sigil` (see step 1). Check `sigil --version` and that its install dir is on `PATH`. |
| No data appears | Let turns finish (interrupted turns are not exported). Then check the debug log. |
| Subagent appears as a normal turn | Codex hook payloads don't always carry the parent link. Known limitation. |
