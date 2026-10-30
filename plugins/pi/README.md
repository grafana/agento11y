# @grafana/agento11y-pi

[Pi](https://github.com/earendil-works/pi) agent extension that sends LLM generations to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

The supported test baseline is Pi 1.4.0. Older hosts are not verified. Upgrading from the older dotenv loader requires one process restart; see [configuration reload](#all-options).

By default only metadata is sent (token counts, cost, model, tool names, recognized skill names, durations). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

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

**Go install (Windows, or any platform with Go 1.26.9+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y pi
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

On first run, `agento11y pi` asks where sessions go, saves the answer to `~/.config/agento11y/config.env`, then installs the `@grafana/agento11y-pi` extension and launches pi. **Grafana Cloud** asks for the credentials below. **Local only** sets `AGENTO11Y_LOCAL=true` and starts the local receiver for that launch. The question needs macOS or Linux and a terminal; see [Configure](../agento11y/README.md#configure) for the full rules.

<details>
<summary>Manual extension registration</summary>

```sh
pi install npm:@grafana/agento11y-pi
agento11y login
```

The extension reads the same `~/.config/agento11y/config.env` file whether you start pi with `agento11y pi` or plain `pi`. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

</details>

## 2. Credentials

When `agento11y pi` or `agento11y login` prompts and you pick Grafana Cloud, it asks which Grafana stack you are on, then prints that stack's coding-agent setup page (`https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`) and tries to open it in a browser. Copy the environment block that page hands out, paste it into the next prompt, and the endpoint, instance ID, token, and OTLP endpoint are all filled from it. The stack is saved, so a later run offers it back and you press Enter. Make sure Agent Observability is enabled on your stack: an administrator opens **Observability → Agent Observability** once and accepts the terms.

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

When `AGENTO11Y_AUTH_TENANT_ID` and `AGENTO11Y_AUTH_TOKEN` are set, the extension uses them for Agent Observability and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To include conversation text (with automatic secret redaction), add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run one pi turn, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

The plugin reports observed export failures with a fixed warning, deduplicated per client, even when debug logging is disabled. Interactive sessions use Pi UI notifications; headless sessions use stderr, never stdout. If a notification sink throws, reporting attempts stderr without interrupting Pi.

Warnings cover observed generation-export failures, including rejection, queue overflow, exhausted retries, and shutdown loss. They do not detect every silent loss. Export warnings and their SDK debug diagnostics omit raw errors, payloads, endpoints, and credentials. Diagnostic filtering is independent of capture mode. There is no durable spool or recovery guarantee.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env`, run another turn, and check the debug log at `~/.local/state/agento11y/logs/agento11y.log` (honors `XDG_STATE_HOME`).

Live capture recognizes `read` attempts for an exact instruction file in Pi's finalized skill catalog, including standalone Markdown skills. The catalog comes from `systemPromptOptions` after all `before_agent_start` handlers finish and is cleared at `agent_settled`. Runs without a fresh catalog produce no skill match. Pi 1.4.0 custom-message runs emit `before_agent_start` too. Detection requires input observed through `tool_call` or `tool_result`; calls blocked before those handlers run stay generic. Reads without matching execution start/end events produce no tool span. Detection assumes built-in-compatible local `read` behavior; custom or remote overrides can associate a read with the wrong skill.

Recognized attempts mark existing tool-execution spans with `gen_ai.skill.name`, including failures. Pi retains the matched catalog name with its case and namespace, trimming only surrounding whitespace. Recognition does not prove successful loading or explain later actions. It excludes imported history, direct `/skill` prompt insertion, resource reads, scripts, and attribution of later activity. Validated names remain in reduced capture modes; secret-like names are omitted. Recognition adds no descriptions, paths, or URIs and changes no payload policy. Recognized attempts, including failures, also add `gen_ai.skill.name` and `agento11y.tool.execution.kind="load_skill"` to the existing `gen_ai.client.operation.duration` histogram. Names remain under metric retention even in reduced modes; use stable, nonsensitive names.

For local capture, open **Analytics > Skills**. See [Local skill analytics](../agento11y/README.md#local-skill-analytics) for counts, timing coverage, and session drilldowns. Local recording and analytics need no Tempo. Cloud skill trace visibility requires retained tool and generation spans in Tempo plus deployed Sigil consumer support; a Cloud conversation alone is insufficient.

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation pi exports:

```sh
agento11y pi --tag project=hackathon --tag team=ai
# forward args to pi after `--`
agento11y pi --tag team=ai -- --resume
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags override matching values from the environment or `~/.config/agento11y/config.env`. Guards use the same precedence as generation exports: automatic tags, configured tags, then built-in tags.

The plugin always attaches two built-in tags to every generation:

- `git.branch` — current branch from the working directory, or a 12-char short SHA on detached HEAD. Omitted when not inside a git checkout.
- `cwd` — the session working directory.

One more tag appears on generations pi produces outside a user turn:

- `pi.call_kind` — `compaction` or `branch_summary`. Absent on ordinary turns. See [Compaction and summarization](#compaction-and-summarization).

Built-in tags win collisions with user tags, matching the claude-code and cursor launchers. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#built-in-tags-from-the-agent-launchers) for the tags the launchers share and the metadata keys pi adds.

## Capture and attribution limits

Pi 1.4.0 does not expose a verified final provider request. The plugin omits system prompts, offered-tool descriptions and schemas, and request controls such as temperature or token limits. Uncorrelated provider callbacks cannot distinguish foreground requests from warming or background work.

`Generation.tools` contains unique, name-only definitions from finalized assistant tool-call proposals. These names describe proposed calls, not the offered catalog or proof of execution. Structural tool-call and tool-result parts remain available in reduced capture modes.

The plugin keeps three tool observations separate:

- The assistant's proposal supplies the generation's tool-call identity and arguments, when safe correspondence is proven.
- The `tool_call` hook supplies guard-stage input. Argument preparation and other handlers can change it.
- `tool_result.input` supplies post-execution input, explicitly labeled on tool spans. A tool can mutate its arguments, so this is not an execution-entry snapshot.

Transformed proposal bodies are omitted. Missing or ambiguous correspondence also omits bodies instead of substituting prepared input or raw history. Post-execution input is retained only when its safety checks pass; blocked or invalid calls do not acquire invented execution input. Top-level result bodies and generation error flags use finalized transcript results, including message replacements. Top-level spans omit result bodies without a unique matching finalized result. Nested result bodies use live completion events because nested calls have no transcript entry. Observed parent IDs determine nested tool spans; missing, ambiguous, or malformed relationships stay attached to the generation.

Tool spans retain the observed start. When Pi supplies a valid execute-only duration, the plugin places that duration at the observed start. This placement is an estimate: Pi does not expose the exact execution-entry timestamp, and its duration excludes guards and result hooks.

Ordinary responses use `responseModel ?? model`: a supplied response identity wins, and a missing or null identity falls back to the request model. Failed assistant attempts remain separate from successful retries. Request-time compaction preserves active-turn input and timing. Reported usage does not establish complete accounting for every failed attempt or background call.

Numeric `pi.tool_usage.*` metadata aggregates usage only from finalized top-level tool results, once per result; Pi already includes nested usage there. Tool usage and cost remain separate from assistant usage and `cost_usd`. This metadata does not change backend generation totals or attribute tool usage to the requesting model.

One-hour cache writes are already included in cache-write counts. Assistant and summary records expose this non-additive detail as `pi.usage.cache_write_1h_input_tokens`. Tool usage uses `pi.tool_usage.cache_write_1h_input_tokens`.

The plugin uses client-scoped OpenTelemetry providers. It does not replace global providers or the global context manager.

## Compaction and summarization

Pi makes model calls of its own outside the agent loop. It compacts the context when the conversation approaches the model's window, when you run `/compact`, and while recovering from a context overflow. It summarizes the abandoned branch when you navigate the session tree with summarization on. A compaction on a large-window model can be a six-figure-token request at full price, because pi sends it with cache retention off.

The plugin exports each successful built-in summary operation as a generation. Split compaction can make several requests; the record is an operation aggregate, not an individual physical call:

- The tag `pi.call_kind` is `compaction` or `branch_summary`. That is the marker to filter on.
- `operation_name` is `generateText` rather than the `streamText` a turn gets, because there is no token stream to time.
- When ancestry is known, the parent is the nearest local assistant turn above the summary entry. A branch summary follows the navigation target, which can precede the summarized branch.
- Physical model identity is unknown. The request model and provider are `unknown`, and the response model is omitted. Metadata sets `pi.summary.model_attribution="unknown"` and `pi.summary.operation_aggregate=true`. The current selection, when available, stays separate in `pi.summary.selected_model` and `pi.summary.selected_provider`.
- Metadata carries `cost_usd` whenever pi priced the call, on the same rule as a turn. Compactions add `pi.tokens_before`, `pi.compaction.reason`, and `pi.compaction.will_retry`. Branch summaries carry none of those three, because pi records no pre-summary context estimate and no trigger reason for them.
- The summary text is exported as assistant output, so `AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only` drops it while keeping tokens, cost, and timing. The request side is not exported: these generations carry no input messages and no system prompt, so input tokens are reported without the text they came from.

Extension-supplied summaries and tree navigation without summarization export no generation. Failed or cancelled compaction clears pending state without inventing a zero-usage generation. Pi exposes no `session_tree_failed` event. Tree events, the plugin's own cancellation, and shutdown clear pending tree timing. The next tree preparation replaces it. The plugin cannot observe every other tree failure.

Summary timing can include retries and backoff. Saved usage does not account for every failed attempt. A successful summary entry without token counts exports no usage block. History import skips summary entries because they lack model identity and request timing.

## Forked sessions

`pi --fork` and the in-TUI `/fork` and `/clone` start a new conversation that carries a copy of the trunk's history. The fork's first generation gets no `parent_generation_ids`, because the trunk turn it continues from was only exported if the trunk itself ran instrumented, and a fork can be taken from a session recorded before this plugin was installed. The link to the trunk ships as metadata instead:

- `pi.fork.parent_session_id` — conversation id of the trunk.
- `pi.fork.parent_generation_id` — generation id of the trunk turn the fork continues from.

The fork's own turns link when origin evidence is sufficient. Both metadata keys are omitted when the plugin cannot establish the trunk turn's origin. This includes unreadable trunk evidence, missing usable timestamps, and turns inherited by the trunk from an older session.

Persisted header evidence takes precedence. For an observed in-memory fork, a reload-stable ephemeral registry retains origin facts under the same public `SessionManager` object. It stores no clients or message bodies. It does not establish ancestry across independent managers or process restarts. If origin is unknown, the plugin suppresses uncertain parent edges rather than guessing.

## Redaction

Before any generation leaves the process, the SDK scrubs known token formats, PEM private keys, database URLs, `KEY=value` pairs, bearer tokens, and email addresses. Matches become `[REDACTED:<id>]`. User input messages are redacted by default; set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to leave them unchanged.

## Guards

Guards do two things when enabled: block tool calls that match a deny rule, and apply Transform rules to redact sensitive content. They're off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y pi
```

By default, transport errors and timeouts let the request through. Set `AGENTO11Y_GUARDS_FAIL_OPEN=false` to block tools and cancel supported summaries on evaluation failures. Raise or lower `AGENTO11Y_GUARDS_TIMEOUT_MS` (default `1500`) to trade latency against tolerance for slow evaluators.

Guard readiness is independent of capture readiness. If guard initialization fails, strict mode blocks tool calls as evaluation failures. Fail-open mode allows tools with a fixed warning. Disabled guards do not block tools when capture fails. An OTLP setup failure does not disable a working guard client. Failed local receiver discovery never selects Cloud as a fallback.

The same three variables are honored by the [Claude Code plugin](../claude-code/README.md); both plugins read them from `~/.config/agento11y/config.env`.

### Transform guards (redaction)

When guards are enabled, pi also applies [Transform guards](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) — regex redaction rules you configure in Grafana — in these paths:

- Preflight message redaction runs at Pi's `context_with_system` hook. Validated replacements update request-local copies, not stored history. Supported content includes user and assistant text, tool arguments and results, custom messages, included Bash executions, summaries, and visible system updates.
- Postflight tool-argument redaction runs before a tool executes. Matching Transform rules replace the arguments the tool receives. If Pi's input cannot be changed, `AGENTO11Y_GUARDS_FAIL_OPEN=false` blocks the call. Fail-open mode permits execution with original or partially changed arguments.
- Built-in summary guards evaluate compaction and branch-summary preparation. See [Summary guard coverage](#summary-guard-coverage).

Limits:

- Each ordinary request adds one synchronous hook round-trip (`AGENTO11Y_GUARDS_TIMEOUT_MS`, default `1500`). This hook cannot cancel a request. Denied, failed, or invalid evaluations leave the request unchanged, log content-free warnings only when debug logging is enabled, and omit incremental user-message bodies from `Generation.input`. System prompts, offered-tool bodies, and unverified request controls are omitted in every capture mode.
- Exported incremental user input and prompt-derived titles use only successfully observed content. Missing or ambiguous correspondence with finalized entries omits the body instead of reading raw history.
- Ordinary guards use the current physical model selection. Tool guards use the assistant's reported dispatch model. Unknown or virtual selections remain unknown.
- Pi substitutes forced prompts and filters hidden tools after this hook. Later extensions can change the request again. This is not a final provider boundary.
- Images, thinking blocks, signatures, and unrelated fields remain unchanged. Guards do not evaluate that opaque content.
- Tool postflight transforms do not establish universal historical replay protection after reload or resume. The plugin does not rerun historical tool authorization indiscriminately. Arbitrary extension-owned model calls remain outside coverage.

### Summary guard coverage

Each summary preparation adds one synchronous guard request, even when split compaction makes two model calls.

Compaction guards cover messages, split-turn prefixes, previous summary text, tracked file paths, and custom instructions. Approved replacements update preparation copies, leaving persisted entries unchanged. Pi has no consumed compaction-instruction override, so a required instruction rewrite cancels compaction.

Branch-summary guards replace prepared entries with sanitized clones, retaining the original array. They cover message and custom-message content, prior summaries, tracked file paths, and supported custom-instruction overrides. Tree navigation without summarization does not evaluate a summary guard.

A deny verdict or invalid required transformation cancels the summary, even with fail-open enabled. Evaluation failures continue with a fixed warning in fail-open mode; strict mode cancels. The plugin returns cancellation explicitly because Pi ignores exceptions from these hooks.

Cancellation does not undo earlier host changes. Manual compaction can already have aborted active work. Overflow-recovery edits remain. Cancelling threshold compaction can still allow the next foreground request. Cancelling a tree summary cancels that navigation.

These hooks do not protect virtual-router internal calls, opaque content, or later extension changes. Earlier tree-instruction overrides are not exposed as effective preparation, and later hook results can replace this plugin's instruction override. Summary guards are not a final provider boundary.

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
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `AGENTO11Y_DEBUG` | `false` | Write filtered diagnostics to `~/.local/state/agento11y/logs/agento11y.log` (honors `XDG_STATE_HOME`). Debug logs stay off the terminal. Observed export-failure warnings use Pi UI or headless stderr independently. |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret patterns in user input messages before export. |
| `AGENTO11Y_EXPORT_TIMEOUT_MS` | `30000` | Timeout for each generation export request. Use a base-10 integer from `1` through `2147483647` milliseconds. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP HTTP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Evaluate ordinary requests, tool calls, and supported summaries against Agent Observability policy. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout in milliseconds, from `1` through `2147483647`. Invalid values use `1500`. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | Allow tools and supported summaries on evaluation failure. Does not override policy denies or invalid summary transformations. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only the following keys are honoured — anything else (including stray `PATH=…` lines) is ignored: any `AGENTO11Y_*` or `SIGIL_*` key plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME`.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.

For recognized alias pairs, shell values take precedence across spellings: shell `AGENTO11Y_*`, shell `SIGIL_*`, file `AGENTO11Y_*`, then file `SIGIL_*`. This includes `QUEUE_SIZE`, `MAX_RETRIES`, and `MAX_BACKOFF_MS`. File edits and removals take effect at the next session start, including extension reload.

Restart the Pi process once after upgrading from the older dotenv loader; extension reload alone is insufficient. The older loader does not retain ownership across module replacement, so the new loader cannot distinguish its injected values from shell exports. Later reloads retain configuration ownership without retaining clients or session contexts.
