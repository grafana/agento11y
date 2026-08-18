# @grafana/agento11y-pi

[Pi](https://github.com/earendil-works/pi) agent extension that records LLM generations. With Grafana Cloud credentials, they go to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/). Without them, they stay on your machine.

Cloud exports contain only metadata by default (token counts, cost, model, tool names, durations); set `AGENTO11Y_CONTENT_CAPTURE_MODE` to change that. Local capture always records full content. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the accepted values.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y pi
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y pi
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y pi
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y pi` installs the `@grafana/agento11y-pi` extension on first run and then launches pi. With Grafana Cloud credentials, the session goes to Cloud; without them, the launcher captures it locally and prints the viewer URL. Use `--local` or `--no-local` to pick one; see [Local mode](../agento11y/README.md#local-mode).

<details>
<summary>Manual extension registration</summary>

```sh
pi install npm:@grafana/agento11y-pi
agento11y login
```

The extension reads the same `~/.config/agento11y/config.env` file whether you start pi with `agento11y pi` or plain `pi`. If you only have the old `~/.config/sigil/config.env`, that file is used instead. Plain `pi` does not capture locally; use the launcher for that.

</details>

## 2. Credentials

Credentials are optional on macOS and Linux: without them the session stays on this machine. Run `agento11y login` to enter them. The prompt asks which Grafana stack you are on, then prints that stack's coding-agent setup page (`https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`) and tries to open it in a browser. Copy the environment block that page hands out, paste it into the next prompt, and the endpoint, instance ID, token, and OTLP endpoint are all filled from it. The stack is saved, so a later run offers it back and you press Enter. Make sure Agent Observability is enabled on your stack. An administrator opens **Observability → Agent Observability** once and accepts the terms.

To type the values instead, press Enter on the empty paste box. They come from three Grafana Cloud pages:

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

When `AGENTO11Y_AUTH_TENANT_ID` and `AGENTO11Y_AUTH_TOKEN` are set, the extension uses them for Agent Observability and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To send conversation text to Grafana Cloud (with automatic secret redaction), add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run one pi turn. If the launcher printed a local viewer URL, open it and check Sessions. Otherwise, open **Agent Observability → Conversations** in Grafana Cloud.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env`, run another turn, and check the debug log at `~/.local/state/agento11y/logs/agento11y.log` (honors `XDG_STATE_HOME`).

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation pi exports:

```sh
agento11y pi --tag project=hackathon --tag team=ai
# forward args to pi after `--`
agento11y pi --tag team=ai -- --resume
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment or `~/.config/agento11y/config.env`. The merge happens in the SDK, so user tags reach every generation without the plugin reparsing them.

The plugin always attaches two built-in tags to every generation:

- `git.branch` — current branch from the working directory, or a 12-char short SHA on detached HEAD. Omitted when not inside a git checkout.
- `cwd` — the process working directory.

One more tag appears on generations pi produces outside a user turn:

- `pi.call_kind` — `compaction` or `branch_summary`. Absent on ordinary turns. See [Compaction and summarization](#compaction-and-summarization).

Built-in tags win collisions with user tags, matching the claude-code and cursor launchers. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#built-in-tags-from-the-agent-launchers) for the tags the launchers share and the metadata keys pi adds.

## Compaction and summarization

Pi makes model calls of its own outside the agent loop. It compacts the context when the conversation approaches the model's window, when you run `/compact`, and while recovering from a context overflow. It summarizes the abandoned branch when you navigate the session tree with summarization on. A compaction on a large-window model can be a six-figure-token request at full price, because pi sends it with cache retention off.

The plugin exports each of those calls as its own generation so they show up in the session's token and cost totals:

- The tag `pi.call_kind` is `compaction` or `branch_summary`. That is the marker to filter on.
- `operation_name` is `generateText` rather than the `streamText` a turn gets, because there is no token stream to time.
- The parent generation is the nearest assistant turn above the entry in pi's session tree. For a compaction that is the turn it followed. For a branch summary it is the turn above the navigation target, which can sit earlier in the session than the branch that was summarized.
- Metadata carries `cost_usd` whenever pi priced the call, on the same rule as a turn. Compactions add `pi.tokens_before`, `pi.compaction.reason`, and `pi.compaction.will_retry`. Branch summaries carry none of those three, because pi records no pre-summary context estimate and no trigger reason for them.
- The summary text is exported as assistant output, so a Cloud export with `AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only` drops it while keeping tokens, cost, and timing. The request side is not exported: these generations carry no input messages and no system prompt, so input tokens are reported without the text they came from.

Two cases export nothing, because no model call happened: an extension supplied the compaction, or you navigated the tree without asking for a summary. Older pi versions record no usage on the entry; those still export a generation, with timing and metadata but no token counts.

## Forked sessions

`pi --fork` and the in-TUI `/fork` and `/clone` start a new conversation that carries a copy of the trunk's history. The fork's first generation gets no `parent_generation_ids`, because the trunk turn it continues from was only exported if the trunk itself ran instrumented, and a fork can be taken from a session recorded before this plugin was installed. The link to the trunk ships as metadata instead:

- `pi.fork.parent_session_id` — conversation id of the trunk.
- `pi.fork.parent_generation_id` — generation id of the trunk turn the fork continues from.

The fork's own turns link to each other as usual. Both keys are omitted when the plugin cannot name the trunk turn: the trunk session file is unreadable, the fork header carries no usable timestamp, or the trunk inherited the turn from an older session instead of running it (a fork of a fork). If `pi --fork` starts on a session file whose header cannot be read, the plugin cannot tell the fork from a resume and links turns as usual; a fork taken inside the session is recognized either way, because pi reports it.

## Redaction

Before any generation leaves the process, the SDK scrubs known token formats, PEM private keys, database URLs, `KEY=value` pairs, bearer tokens, and email addresses. Matches become `[REDACTED:<id>]`. User input messages are redacted by default; set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to leave them unchanged.

## Guards

Guards do two things when enabled: block tool calls that match a deny rule, and apply Transform rules to redact sensitive content. They're off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y pi
```

By default, transport errors and timeouts let the request through. Set `AGENTO11Y_GUARDS_FAIL_OPEN=false` to block tool calls on errors instead. Raise or lower `AGENTO11Y_GUARDS_TIMEOUT_MS` (default `1500`) to trade latency against tolerance for slow evaluators.

The same three variables are honored by the [Claude Code plugin](../claude-code/README.md); both plugins read them from `~/.config/agento11y/config.env`.

### Transform guards (redaction)

When guards are enabled, pi also applies [Transform guards](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) — regex redaction rules you configure in Grafana — in two places:

- **Preflight (message redaction).** Before each model call, the outgoing conversation is sent to Agent Observability; redacted text replaces the original, so the placeholder (e.g. `[REDACTED]`) reaches the model instead of the secret.
- **Postflight (tool-argument redaction).** Before a tool runs, its arguments are sent to Agent Observability; if a Transform rule matches, the redacted arguments replace what the tool receives.

Limits:

- Each guarded model call adds one synchronous hook round-trip (`AGENTO11Y_GUARDS_TIMEOUT_MS`, default `1500`). Transform redaction always fails open: on a transport error or timeout the original messages or tool arguments pass through unchanged.
- A preflight deny verdict cannot stop the model call, only the transform output is applied. Enforced blocking happens at the tool-call (postflight) level.
- Redaction rewrites text content only. `thinking` blocks on assistant messages are left unchanged so multi-turn continuity is preserved.

## All options

`~/.config/agento11y/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL (find it at `/plugins/grafana-agento11y-app`) |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. Combined with `AGENTO11Y_AUTH_TOKEN` becomes Basic auth for Agent Observability and OTLP. |
| `AGENTO11Y_AUTH_TOKEN` | — | Cloud access policy token (`glc_…`). |
| `AGENTO11Y_AGENT_NAME` | `pi` | Agent name reported to Agent Observability. |
| `AGENTO11Y_AGENT_VERSION` | — | Optional version string reported with the agent. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags resolved for the session: the user, the repository, and the branch. These reach OTel metrics as `agento11y_tag_*` labels, unlike the per-generation built-ins. The plugin builds one client per session, so the values freeze at session start. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#opt-in-automatic-tags-agento11y_auto_coding_agent_tags) for the cardinality and personal-data trade-offs. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. Applies to Cloud exports; local capture keeps full content. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md). |
| `AGENTO11Y_DEBUG` | `false` | Write lifecycle events to `~/.local/state/agento11y/logs/agento11y.log` (honors `XDG_STATE_HOME`). Never written to the terminal, to avoid corrupting pi's TUI. |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret patterns in user input messages before export. |
| `AGENTO11Y_EXPORT_TIMEOUT_MS` | `30000` | Timeout for each generation export request. Use a base-10 integer from `1` through `2147483647` milliseconds. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP HTTP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Evaluate `tool_call` requests against Agent Observability policy. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout for guard requests, in milliseconds. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | Allow tools through when the guard call fails. Set `false` for strict mode. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only the following keys are honoured — anything else (including stray `PATH=…` lines) is ignored: any `AGENTO11Y_*` or `SIGIL_*` key plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME`.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.
