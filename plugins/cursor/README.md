# Agent Observability for Cursor

Sends Cursor agent generations to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/): prompts, replies, tool calls, and token usage.

## 1. Install the shared binary

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
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

It registers `agento11y cursor hook` for the Cursor events agento11y captures in `~/.cursor/hooks.json`, merging with any hooks other tools already added. Re-running is safe: it updates agento11y's entry in place instead of adding a duplicate. On first run it also runs the same setup prompt as `agento11y login`, which asks where sessions go. **Local only** is saved for later hooks, and the local receiver starts on the next Cursor hook, not during the install. **Grafana Cloud** continues to the credential questions. The destination question needs macOS or Linux, a terminal, and no destination and no credentials saved yet; Windows cannot run the local receiver, so its flow starts at the Cloud questions. The install asks nothing when local mode is already on (`AGENTO11Y_LOCAL=true`) or when stdin is not a terminal.

To undo the wiring later, run `agento11y cursor uninstall` — it removes only agento11y's entries and leaves other tools' hooks alone.

<details>
<summary>Alternative: register the plugin inside Cursor</summary>

Instead of `agento11y cursor install`, you can register the plugin from Cursor's command palette:

```
/add-plugin grafana/agento11y
```

Do not use both. `/add-plugin` and `agento11y cursor install` write to the same `~/.cursor/hooks.json`, so running both captures every turn twice. Pick one.

</details>

## 3. Add your credentials

`agento11y cursor install` already prompts for these on first run; run `agento11y login` from a terminal to enter or change them later. After you pick Grafana Cloud, the prompt asks which Grafana stack you are on, then prints that stack's coding-agent setup page (`https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`) and tries to open it in a browser. Copy the environment block that page hands out, paste it into the next prompt, and the endpoint, instance ID, token, and OTLP endpoint are all filled from it. The stack is saved, so a later run offers it back and you press Enter. A rerun of `agento11y login` goes straight to the Cloud questions, and asks where sessions go only when neither that answer nor credentials are saved. Make sure Agent Observability is enabled on your stack: an administrator opens **Observability → Agent Observability** once and accepts the terms.

To type the values instead, press Enter on the empty paste box. They come from three Grafana Cloud pages:

1. **Agent Observability → Configuration**
   - **API URL** → `AGENTO11Y_ENDPOINT`
   - **Instance ID** → `AGENTO11Y_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `AGENTO11Y_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`

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

Captured prompt, assistant, and tool content is redacted before export.

## 4. Verify

Use Cursor's agent for one turn, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, add `AGENTO11Y_DEBUG=true` to `~/.config/agento11y/config.env` (Cursor launches from the GUI, so a shell env var won't reach the hooks) and tail the log:

```sh
tail -f ~/.local/state/agento11y/logs/agento11y.log
```

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-agento11y-app`. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `agento11y.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). Built-ins (`git.branch`, `cwd`, `subagent`) win on generation-export tag collision. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags resolved for the session: the user, the repository, and the branch. Unlike the built-ins, these reach OTel metrics as `agento11y_tag_*` labels. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#opt-in-automatic-tags-agento11y_auto_coding_agent_tags) for the cardinality and personal-data trade-offs. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_USER_ID` | from Cursor | Override the user id. |
| `AGENTO11Y_AGENT_NAME` | `cursor` | Override the exported `agent_name`. Avoid a `/` in the name: a slash marks a subagent generation, so every turn of the run is counted as one. Guard rules and dashboards that filter on `cursor` no longer match the generations this run exports. |
| `AGENTO11Y_LOCAL` | `false` | Send Cursor hook captures to the local viewer at `http://127.0.0.1:8765` instead of Grafana Cloud. Local mode always stores full content. Cloud forwarding also requires `AGENTO11Y_LOCAL_FORWARD`. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/agento11y/logs/agento11y.log`. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Cursor `preToolUse` hook is evaluated against Agent Observability: tool calls denied by guard rules are blocked, and Transform rules rewrite the tool arguments before execution. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |
| `AGENTO11Y_BIN` | auto | Override the binary path if you installed `agento11y` (or the legacy `sigil`) somewhere unusual. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your Agent Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.
