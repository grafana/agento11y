# Sigil for Cursor

Sends Cursor agent generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/): prompts, replies, tool calls, and token usage.

## 1. Install the shared binary

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/agento11y/scripts/install.sh | sh
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

Cursor is a GUI app with no `agento11y cursor` launcher, so after installing the binary you wire the hooks once with `agento11y cursor install` (next step), then add credentials.

## 2. Wire the hooks

Run this once from a terminal:

```sh
agento11y cursor install
```

It registers `agento11y cursor hook` for the Cursor events Sigil captures in `~/.cursor/hooks.json`, merging with any hooks other tools already added. Re-running is safe: it updates Sigil's entry in place instead of adding a duplicate. On first run it also prompts for credentials (the same prompt as `agento11y login`).

To undo the wiring later, run `agento11y cursor uninstall` — it removes only Sigil's entries and leaves other tools' hooks alone.

<details>
<summary>Alternative: register the plugin inside Cursor</summary>

Instead of `agento11y cursor install`, you can register the plugin from Cursor's command palette:

```
/add-plugin grafana/sigil-sdk
```

Do not use both. `/add-plugin` and `agento11y cursor install` write to the same `~/.cursor/hooks.json`, so running both captures every turn twice. Pick one.

</details>

## 3. Add your credentials

`agento11y cursor install` already prompts for these on first run; run `agento11y login` from a terminal to enter or change them later. The prompt asks for values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `AGENTO11Y_ENDPOINT`
   - **Instance ID** → `AGENTO11Y_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `AGENTO11Y_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`

<details>
<summary>Non-interactive config.env</summary>

Create or update `~/.config/sigil/config.env`:

```dotenv
AGENTO11Y_ENDPOINT=https://sigil-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

</details>

To also send the conversation text, add this to `~/.config/sigil/config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Cursor content is not passed through the shared redactor before export. Avoid `full` when prompts, replies, or tool output may contain secrets.

## 4. Verify

Use Cursor's agent for one turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, add `AGENTO11Y_DEBUG=true` to `~/.config/sigil/config.env` (Cursor launches from the GUI, so a shell env var won't reach the hooks) and tail the log:

```sh
tail -f ~/.local/state/sigil/logs/sigil.log
```

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Sigil API URL. Find it at `/plugins/grafana-sigil-app`. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the AI Observability latency and tool-call panels stay empty. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `sigil.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). Built-ins (`git.branch`, `cwd`, `subagent`) win on generation-export tag collision. |
| `AGENTO11Y_USER_ID` | from Cursor | Override the user id. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Cursor `preToolUse` hook is evaluated against Sigil: tool calls denied by guard rules are blocked, and Transform rules rewrite the tool arguments before execution. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |
| `AGENTO11Y_BIN` | auto | Override the binary path if you installed `agento11y` (or the legacy `sigil`) somewhere unusual. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
