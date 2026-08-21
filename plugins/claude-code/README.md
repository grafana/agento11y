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

On first run, `agento11y claude` asks where sessions go, saves the answer to `~/.config/agento11y/config.env`, then registers the `agento11y-claude-code` plugin and launches Claude Code. **Grafana Cloud** asks for the credentials below. **Local only** sets `AGENTO11Y_LOCAL=true` and starts the local receiver for that launch. The question needs macOS or Linux and a terminal; see [Configure](../agento11y/README.md#configure) for the full rules.

<details>
<summary>Manual plugin registration</summary>

```
/plugin marketplace add grafana/agento11y
/plugin install agento11y-claude-code@agento11y
```

</details>

## 2. Credentials

When `agento11y claude` prompts and you pick Grafana Cloud, it asks which Grafana stack you are on, then prints that stack's coding-agent setup page (`https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`) and tries to open it in a browser. Copy the environment block that page hands out, paste it into the next prompt, and the endpoint, instance ID, token, and OTLP endpoint are all filled from it. The stack is saved, so a later run offers it back and you press Enter. Make sure Agent Observability is enabled on your stack: an administrator opens **Observability → Agent Observability** once and accepts the terms.

To type the values instead, press Enter on the empty paste box. They come from three Grafana Cloud pages:

1. **Agent Observability → Configuration**
   - **API URL** → `AGENTO11Y_ENDPOINT`
   - **Instance ID** → `AGENTO11Y_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `AGENTO11Y_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`

Run `agento11y login` later to update saved credentials; a rerun asks the Cloud questions again.

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

## Guards

Guards apply [Agent Observability rules](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) to submitted messages and tool calls.

Guards are off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y claude
```

Guards can be

- `preflight`: stop the turn before the message reaches the model.
- `postflight`: block the tool call and tells the model why. A redact rule rewrites the tool arguments.

When enabled, guards send every submitted message and tool argument to `AGENTO11Y_ENDPOINT`, regardless of `AGENTO11Y_CONTENT_CAPTURE_MODE`.

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-agento11y-app`. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret formats out of user prompt text. Set to `false` to export prompts without redaction; assistant and tool content stay redacted. |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `agento11y.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags resolved for the session: the user, the repository, and the branch. Unlike the built-ins, these reach OTel metrics as `agento11y_tag_*` labels. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#opt-in-automatic-tags-agento11y_auto_coding_agent_tags) for the cardinality and personal-data trade-offs. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_USER_ID` | from `~/.claude.json` | Override the user id. |
| `AGENTO11Y_AGENT_NAME` | `claude-code` | Override the exported `agent_name`. Subagent turns become `<name>/<subagent type>`, or `<name>/subagent` when Claude Code names no type. Avoid a `/` in the name itself: a slash marks a subagent generation, so every turn of the run is counted as one. Guard rules and dashboards that filter on `claude-code` no longer match the generations this run exports. |
| `AGENTO11Y_USER_ID_SOURCE` | `email` | Which field to read from `~/.claude.json`: `email` or `accountUuid`. |
| `AGENTO11Y_LOCAL` | `false` | Send Claude Code captures (launches and installed hooks) to the local viewer at `http://127.0.0.1:8765` instead of Grafana Cloud. Local mode always stores full content. Cloud forwarding also requires `AGENTO11Y_LOCAL_FORWARD`. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/agento11y/logs/agento11y.log`. |
| `AGENTO11Y_AUTO_UPDATE` | `true` | Refresh the `agento11y-claude-code` plugin automatically. Set `false` to pin the installed version. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | See [Guards](#guards). |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | On timeout, network error, or 5xx, send the message or run the tool call. Set `false` to block the message or call. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Guard timeout per message or tool call. Raise it for slow `llm_judge` evaluators. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your Agent Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
