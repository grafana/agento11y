# @grafana/agento11y-opencode

[OpenCode](https://opencode.ai) plugin that sends LLM generations to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y opencode
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y opencode
```

**Go install (Windows, or any platform with Go 1.26.9+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y opencode
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

On first run, `agento11y opencode` asks where sessions go, saves the answer to `~/.config/agento11y/config.env`, then installs `@grafana/agento11y-opencode` and launches OpenCode. **Grafana Cloud** asks for the credentials below. **Local only** sets `AGENTO11Y_LOCAL=true` and starts the local receiver for that launch. The question needs macOS or Linux and a terminal; see [Configure](../agento11y/README.md#configure) for the full rules. Pass arguments to OpenCode after `--`, for example, `agento11y opencode -- run "say hi"`. For supported releases and OpenCode 2 behavior, see [OpenCode 2](#opencode-2).

<details>
<summary>Manual plugin registration</summary>

```sh
# OpenCode 1
opencode plugin @grafana/agento11y-opencode --global
# OpenCode 2
opencode plugin add @grafana/agento11y-opencode
agento11y login
```

The plugin reads `~/.config/agento11y/config.env` on every session start, whether you start OpenCode with `agento11y opencode` or plain `opencode`. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

</details>

## 2. Credentials

When `agento11y opencode` or `agento11y login` prompts and you pick Grafana Cloud, it asks which Grafana stack you are on, then prints that stack's coding-agent setup page (`https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`) and tries to open it in a browser. Copy the environment block that page hands out, paste it into the next prompt, and the endpoint, instance ID, token, and OTLP endpoint are all filled from it. The stack is saved, so a later run offers it back and you press Enter. Make sure Agent Observability is enabled on your stack: an administrator opens **Observability → Agent Observability** once and accepts the terms.

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

When `AGENTO11Y_AUTH_TENANT_ID` and `AGENTO11Y_AUTH_TOKEN` are set, the plugin uses them for Agent Observability and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To include conversation text, add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

OpenCode redacts known secret formats out of assistant text, the system prompt, tool arguments, tool results, and user prompts before export. Set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to send prompt text without redaction; assistant and tool content stay redacted either way.

## 3. Verify

Run one OpenCode turn, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env`, run another turn, and check OpenCode stderr.

## OpenCode 2

`agento11y opencode` supports OpenCode 1.3.13 or a later 1.x release, and 2.0.22 or a later 2.x release. It runs `opencode --version` before anything else. For any other release, it prints an error and changes nothing; run `opencode` directly to use that release without capture. The plugin package also loads on OpenCode 1.2.16 when you register it by hand.

On OpenCode 2, `agento11y opencode`:

- Installs the plugin with `opencode plugin add @grafana/agento11y-opencode`.
- Adds `--standalone` to sessions: the TUI, `run`, and `mini`. OpenCode 2 normally attaches a session to a shared background service. That service keeps the environment it started with, so it does not get the launcher's endpoint, local mode, capture, guard, or tag settings. A standalone session runs a private server that does. `--server` and `--no-standalone` are refused.
- Runs every other command as typed, for example `agento11y opencode -- plugin list` or `agento11y opencode -- --version`. It skips the login prompt, the local receiver, the plugin install, and `--standalone`.
- Does not refresh the plugin, because `opencode plugin update` starts or contacts the background service. Run `opencode plugin update @grafana/agento11y-opencode` yourself.
- Leaves the plugin off when your OpenCode config turns it off with `-grafana.agento11y`.

Local mode runs on macOS and Linux only; Windows has no local receiver.

Sessions started with plain `opencode` use the background service. The plugin still reads `~/.config/agento11y/config.env` there, but launcher flags such as `--tag` and `--local` do not reach it.

`agento11y history import opencode` reads v1 and released v2 databases. See [History import](../agento11y/README.md#history-import) for limitations.

### Compaction records

On OpenCode 2, each observed compaction success or failure exports a separate `SYNC` generation with operation `generateText` and tag `opencode.call_kind=compaction`. Its parent is the active, previous, or spawning turn observed at compaction start. Ordinary turns keep their IDs and parent chain.

Tokens and cost cover one logical compaction, including retries and rejected summaries, rather than one provider call (`opencode.usage_scope=logical_compaction`). Missing usage or cost stays absent; explicit zeros stay zero. Cumulative session usage adds no generation.

Successful events supply the compaction model when available. Failed events name no model. Their generations use provider and model `unknown`, with `opencode.model_source=unavailable`. The plugin never substitutes the primary turn's model because compaction can use a different model.

Compaction input is the history observed at its start, not the final provider request. Without an observed start, input is empty, parent edges are absent, and start time equals completion time. Metadata `opencode.start_time_approximate` and `opencode.completion_time_approximate` record these timing limits. Compactions have no time-to-first-token sample or tool spans.

Summary output uses full secret redaction and follows `AGENTO11Y_CONTENT_CAPTURE_MODE`. In `metadata_only`, input and output are empty; usage and metadata remain. Summary output excludes retained recent context and opaque provider state. Success replaces history for later turns; failure preserves history and instructions.

OpenCode 1 capture is unchanged. History import has separate [compaction limitations](../agento11y/README.md#history-import).

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation the plugin exports:

```sh
agento11y opencode --tag project=hackathon --tag team=ai
# forward args to opencode after `--`
agento11y opencode --tag team=ai -- run "say hi"
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment or `~/.config/agento11y/config.env`. The merge happens in the SDK, so user tags reach every generation without the plugin reparsing them.

The plugin attaches built-in tags of its own:

- `git.branch` — current branch from the opencode project directory, or a 12-char short SHA on detached HEAD. Omitted when not inside a git checkout.
- `cwd` — the opencode project directory.
- `subagent` — `"true"` on generations from a subagent session. Omitted for ordinary sessions, and for a subagent that opencode started before the plugin loaded.
- `opencode.call_kind` is `compaction` on separate OpenCode 2 compaction generations. Absent on ordinary turns.

Built-in tags win collisions with user tags, matching the claude-code and cursor launchers. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#built-in-tags-from-the-agent-launchers) for the tags the launchers share.

## Subagent sessions

OpenCode runs a subagent (the `task` tool) in a session of its own. The plugin exports those turns into the conversation that spawned them, so the subagent's work shows up in the parent's timeline rather than as an unrelated conversation:

- `conversation_id` on every child turn is the spawning session's id. A subagent that spawns a subagent of its own flattens the same way: every turn in the tree lands in the root session's conversation, so a grandchild's `conversation_id` is the root rather than its immediate parent.
- The child's first turn carries `parent_generation_ids` naming the parent turn that made the `task` call. Later child turns chain to the child's own previous turn.
- `opencode.parent_session_id` metadata names the session that spawned this one, so a nested subagent still reports its immediate parent rather than the root. `opencode.child_session_id` names the child's own session id, which nothing else carries: `conversation_id` holds an ancestor's, so looking up the child session id finds no conversation.
- The child sends no conversation title. Agent Observability keeps the newest title per conversation, and the child's title names the subagent, so a child landing after the parent's last turn would rename the shared conversation.

If the plugin cannot name the spawning turn, the child keeps its own conversation id and exports no parent edge. That happens when the plugin loaded after OpenCode had already started the parent's turn. The `subagent` tag and `opencode.parent_session_id` are still attached, so the relationship is recorded as metadata instead of an edge.

## Guards

Guards check a request against rules you configure in Grafana and can stop or rewrite it before the model sees it. Refer to [Set up guards](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) to write the rules. This section covers what they do in OpenCode.

They are off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y opencode
```

With guards on, a rule can:

- **Refuse a turn.** A `preflight` deny stops the turn before it reaches the model, so nothing is sent and no tokens are spent. The reason appears in the session and in OpenCode's log.
- **Redact what the model sees.** A `preflight` redact rule replaces matching text in the conversation on its way to the provider. Your own message is not rewritten.
- **Block or rewrite a tool call.** A `postflight` deny stops the call and tells the model why. A redact rule rewrites the arguments the tool receives. OpenCode's permission prompts count as tool calls. On OpenCode 1, its permission API has no field for a reason, so a denied prompt reports none.

On OpenCode 2:

- Checks run when a prompt is submitted, before each model request (including compaction, title, and `session.generate` requests), before a tool runs, and when OpenCode evaluates a permission. A denied permission reports the rule's reason.
- Compaction is checked a second time, on the request OpenCode actually sends, because OpenCode rebuilds the compaction text from stored history after the first check. This check reads OpenAI Chat Completions, OpenAI Responses (HTTP and WebSocket), Anthropic Messages, Gemini, and Bedrock Converse requests. With guards on, compaction through any other protocol fails. A redact rule can rewrite only an unsigned HTTP request that carries no server-side state or signed reasoning. In every other case, compaction fails instead of sending the unredacted text.
- Automatic session titles stop while guards are on, unless you set `agent.title.model` in your OpenCode config. Without a title model, OpenCode's GitHub Copilot provider sends the conversation for a title before any plugin can check it, so the plugin removes OpenCode's title agent.
- Guards do not cover requests other plugins send themselves, OpenCode's global `generate` API, request changes made by plugins that run after this one, or raw provider body overrides on requests other than compaction.

## All options

`~/.config/agento11y/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-agento11y-app`. Empty value disables the plugin. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret formats out of user prompt text. Set to `false` to export prompts without redaction. |
| `AGENTO11Y_EXPORT_TIMEOUT_MS` | `30000` | Timeout for each generation export request. Use a base-10 integer from `1` through `2147483647` milliseconds. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Check your prompts, the conversation sent to the model, and OpenCode tool calls against Agent Observability guards. See [Guards](#guards). |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-evaluation guard timeout in milliseconds. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | Send the turn and allow tool calls when a guard evaluation fails. Set to `false` to refuse them instead. This covers evaluation failures at all three checks. A tool call is blocked under either setting when a guard returns redacted arguments that the plugin cannot write into the arguments OpenCode runs the tool with. |
| `AGENTO11Y_AGENT_NAME` | `opencode` | Agent name reported to Agent Observability. The plugin appends `:<mode>` for OpenCode's UI mode, such as `build` or `plan`. |
| `AGENTO11Y_AGENT_VERSION` | OpenCode version | Version string reported with the agent. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags resolved for the session: the user, the repository, and the branch. These reach OTel metrics as `agento11y_tag_*` labels, unlike the per-generation built-ins. The plugin builds one client per session, so the values freeze at session start. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#opt-in-automatic-tags-agento11y_auto_coding_agent_tags) for the cardinality and personal-data trade-offs. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_DEBUG` | `false` | Log lifecycle events to stderr. |
| `AGENTO11Y_AUTO_UPDATE` | `true` | Refresh the `@grafana/agento11y-opencode` plugin automatically on OpenCode 1. Set `false` to pin the installed version. OpenCode 2 is never refreshed automatically; see [OpenCode 2](#opencode-2). |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only `AGENTO11Y_*` and `SIGIL_*` keys plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME` are honored — anything else (including stray `PATH=…` lines) is ignored.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.

## Development

```bash
pnpm install
pnpm --filter @grafana/agento11y-opencode build
pnpm --filter @grafana/agento11y-opencode test
```

The `@grafana/agento11y` dependency resolves via pnpm workspace linking to `js/`.
