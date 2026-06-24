# Sigil for Cursor

Sends Cursor agent generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): prompts, replies, tool calls, and token usage.

## 1. Install the shared binary

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | sh
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/sigil
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
```

The script installs `sigil` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`sigil` binary README](../sigil/README.md#install) for all install options.

Cursor is a GUI app with no `sigil cursor` launcher, so after installing the binary you wire the hooks once with `sigil cursor install` (next step), then add credentials.

## 2. Wire the hooks

Run this once from a terminal:

```sh
sigil cursor install
```

It registers `sigil cursor hook` for the Cursor events Sigil captures in `~/.cursor/hooks.json`, merging with any hooks other tools already added. Re-running is safe: it updates Sigil's entry in place instead of adding a duplicate. On first run it also prompts for credentials (the same prompt as `sigil login`).

To undo the wiring later, run `sigil cursor uninstall` — it removes only Sigil's entries and leaves other tools' hooks alone.

<details>
<summary>Alternative: register the plugin inside Cursor</summary>

Instead of `sigil cursor install`, you can register the plugin from Cursor's command palette:

```
/add-plugin grafana/sigil-sdk
```

Do not use both. `/add-plugin` and `sigil cursor install` write to the same `~/.cursor/hooks.json`, so running both captures every turn twice. Pick one.

</details>

## 3. Add your credentials

`sigil cursor install` already prompts for these on first run; run `sigil login` from a terminal to enter or change them later. The prompt asks for values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

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
| `SIGIL_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Cursor `preToolUse` hook is evaluated against Sigil: tool calls denied by guard rules are blocked, and Transform rules rewrite the tool arguments before execution. |
| `SIGIL_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `SIGIL_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |
| `SIGIL_BIN` | auto | Override the binary path if you installed `sigil` somewhere unusual. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
