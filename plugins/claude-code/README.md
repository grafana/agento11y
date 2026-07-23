# Agent Observability for Claude Code

Sends every Claude Code turn to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/): model, tokens, tools, timing, and optionally the conversation content.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y claude
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y claude
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y claude
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y claude` registers the `sigil-cc` plugin on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, and then launches Claude Code.

<details>
<summary>Manual plugin registration</summary>

```
/plugin marketplace add grafana/agento11y
/plugin install sigil-cc@grafana-sigil
```

</details>

## 2. Credentials

When `agento11y claude` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure Agent Observability is enabled on your stack — an administrator opens **Observability → Agent Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **Agent Observability → Configuration**
   - **API URL** → `AGENTO11Y_ENDPOINT`
   - **Instance ID** → `AGENTO11Y_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `AGENTO11Y_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`

Run `agento11y login` later to update saved credentials.

<details>
<summary>Non-interactive config.env</summary>

Create or update `~/.config/agento11y/config.env` (if you already have the old `~/.config/sigil/config.env`, edit that one instead):

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

</details>

To also send the conversation text (with automatic secret redaction), add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run any turn in Claude Code, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up:

```sh
AGENTO11Y_DEBUG=true agento11y claude  # one turn
tail -f ~/.local/state/agento11y/logs/agento11y.log
```

Common culprits: `agento11y --version` doesn't work (binary not on `PATH`), a missing token, or a token without the `sigil:write` scope.

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-sigil-app`. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `agento11y.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). |
| `AGENTO11Y_USER_ID` | from `~/.claude.json` | Override the user id. |
| `AGENTO11Y_USER_ID_SOURCE` | `email` | Which field to read from `~/.claude.json`: `email` or `accountUuid`. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/agento11y/logs/agento11y.log`. |
| `AGENTO11Y_AUTO_UPDATE` | `true` | Refresh the `sigil-cc` plugin automatically. Set `false` to pin the installed version. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Claude Code `PreToolUse` hook calls the Agent Observability `/api/v1/hooks:evaluate` API and blocks tool calls denied by guard rules. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your Agent Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
