# Agent Observability for Mistral Vibe

[Mistral Vibe](https://github.com/mistralai/mistral-vibe) is sent to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/) by registering hooks in Mistral Vibe's `hooks.toml` that forward each turn to the `agento11y` binary. `post_agent` exports one generation per turn. `pre_tool` enforces Agent Observability guard policy when guards are enabled. `post_tool` records per-tool timing for tool spans.

Mistral Vibe 2.21.0 renamed all three hook types: `post_agent_turn` became `post_agent`, `before_tool` became `pre_tool`, and `after_tool` became `post_tool`. Each version accepts only its own names and skips an entry that carries the others. `agento11y vibe` reads `vibe --version` and writes the names that version accepts, so both work. This page uses the current names.

> Status: **Experimental.** Hooks were an experimental feature of Mistral Vibe until 2.21.0, and 2.21.0 renamed every hook type. Expect this integration to need an update when the hook contract changes again.

By default only metadata is sent (token counts, model, tool names). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

Exported content is redacted for known secret formats first. That includes the user prompt; set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to export prompt text without redaction. Assistant text, reasoning, and tool results stay redacted either way. Tool-call arguments and the conversation title are not redacted yet.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y vibe
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y vibe
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y vibe
```

The command was renamed from `sigil`; the old name still works but will be removed in a future release.

On first run, `agento11y vibe` asks where sessions go, saves the answer to `~/.config/agento11y/config.env`, then resolves the `vibe` binary on `PATH`, upserts the three agento11y-owned `[[hooks]]` entries, and launches Vibe. **Grafana Cloud** asks for the credentials below. **Local only** sets `AGENTO11Y_LOCAL=true` and starts the local receiver for that launch. The question needs macOS or Linux and a terminal; see [Configure](../agento11y/README.md#configure) for the full rules.

The hooks file is `~/.vibe/hooks.toml`, or `$VIBE_HOME/hooks.toml` when `VIBE_HOME` is set. Repeated runs update entries named `agento11y`, `agento11y-before-tool`, and `agento11y-after-tool` instead of adding duplicates. The command replaces entries under the old `sigil*` names and preserves other hooks.

The entry names are the same for every Mistral Vibe version. Only the `type` values differ. If you upgrade Mistral Vibe across 2.21.0, the next `agento11y vibe` run rewrites the three types in place.

The launcher always sets `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in Mistral Vibe's environment. Before 2.21.0, Mistral Vibe read `hooks.toml` only behind that flag. 2.21.0 removed the flag and loads declared hooks unconditionally. Newer versions ignore unknown `VIBE_*` variables, so the launcher can keep setting it for the older ones.

<details>
<summary>Manual hook registration</summary>

Add these blocks to `~/.vibe/hooks.toml`:

```toml
[[hooks]]
name = "agento11y"
type = "post_agent"
command = "agento11y vibe hook"
timeout = 30

[[hooks]]
name = "agento11y-before-tool"
type = "pre_tool"
command = "agento11y vibe hook"
timeout = 30
match = "*"

[[hooks]]
name = "agento11y-after-tool"
type = "post_tool"
command = "agento11y vibe hook"
timeout = 30
match = "*"
```

Run `vibe --version` first. If it reports a version below 2.21.0, use the old type names instead: `post_agent_turn`, `before_tool`, and `after_tool`. For those versions, also export `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in the shell where you run `vibe`. Without it, they do not read `hooks.toml` at all.

Run `agento11y login` once for credentials.

</details>

## 2. Credentials

Credentials are shared with every other `agento11y` launcher; see [`pi/README.md`](../pi/README.md#2-credentials) for the field-by-field walkthrough of the Grafana Cloud path. Once `~/.config/agento11y/config.env` exists, every launcher (and the Mistral Vibe hook) picks it up. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

## 3. Verify

Run one agent turn, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds, labelled with agent `mistral-vibe` and conversation id equal to the Mistral Vibe `session_id`.

Hooks always exit 0, so a failed export never interrupts the session and never prints an error. If nothing shows up, turn on the debug log:

```sh
AGENTO11Y_DEBUG=true agento11y vibe   # one turn
tail -f ~/.local/state/agento11y/logs/agento11y.log
```

Each fire logs a `dispatch: event=… session=…` line. A successful turn export logs `post_agent: export id=…` followed by `post_agent: done`. The log labels do not change with the Mistral Vibe version.

Run `agento11y doctor` to check the hook install and both export pipelines. Mistral Vibe shows as `not configured` until all three `[[hooks]]` entries are in `hooks.toml` with the `type` values the installed Mistral Vibe accepts.

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation the session exports:

```sh
agento11y vibe --tag project=hackathon --tag team=ai
# forward args to vibe after `--`
agento11y vibe --tag team=ai -- --workdir ~/src/app
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment or `~/.config/agento11y/config.env`. The launcher merges the flags into `AGENTO11Y_TAGS` before it starts Mistral Vibe, and the SDK applies that value to every generation, so the hook never reparses them.

The hook always attaches these built-in tags:

- `entrypoint=vibe`
- `vibe.turn_seq` — the turn number within the session.
- `cwd` — the working directory Mistral Vibe reports for the session.
- `git.branch` — current branch for that directory, or a 12-char short SHA on detached HEAD. Omitted outside a git checkout.

Subagent turns are not tagged `subagent`. Mistral Vibe only exposes a session-level parent link, so the hook records `vibe.parent_session_id` on the child, and `vibe.child_session_id` when the child is reparented onto the parent conversation.

## Guards

Guards apply [Agent Observability rules](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) to submitted messages and tool calls.

`pre_tool` evaluates each tool call against Agent Observability guard policy. Guards are **off by default**; enable them with `AGENTO11Y_GUARDS_ENABLED=true` (tune with `AGENTO11Y_GUARDS_TIMEOUT_MS` and `AGENTO11Y_GUARDS_FAIL_OPEN`). When enabled, a policy can **deny** a tool call (Mistral Vibe blocks it and shows the reason to the model) or **rewrite** its arguments (e.g. redact a secret before the tool runs). With guards disabled, `pre_tool` is a pass-through that writes nothing. Evaluation runs synchronously before the tool, so a policy should be fast or local; on timeout or transport error the call follows `AGENTO11Y_GUARDS_FAIL_OPEN` (open by default).

Mistral Vibe does not support blocking messages with `preflight` guards. 

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-agento11y-app`. Without it the turn is not exported. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. With neither set, tool spans and metrics are dropped and the `post_tool` hook records nothing. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `agento11y.tag.<key>` on OTel spans/metrics. Same as `--tag`. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags resolved for the session: the user, the repository, and the branch. Unlike the built-ins, these reach OTel metrics as `agento11y_tag_*` labels. See [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#opt-in-automatic-tags-agento11y_auto_coding_agent_tags) for the cardinality and personal-data trade-offs. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_USER_ID` | — | Override the user id. |
| `AGENTO11Y_AGENT_NAME` | `mistral-vibe` | Override the exported `agent_name`. Avoid a `/` in the name: a slash marks a subagent generation, so every turn of the run is counted as one. Guard rules and dashboards that filter on `mistral-vibe` no longer match the generations this run exports. |
| `AGENTO11Y_LOCAL` | `false` | Send Vibe hook captures to the local viewer at `http://127.0.0.1:8765` instead of Grafana Cloud. Local mode always stores full content. Cloud forwarding also requires `AGENTO11Y_LOCAL_FORWARD`. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/agento11y/logs/agento11y.log`. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Evaluate every `pre_tool` fire against guard policy. Denied calls are blocked; Transform rules rewrite the tool arguments. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | On timeout, network error, or 5xx, run the tool anyway. Set `false` for strict mode. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call guard timeout. Every guarded tool call pays this latency at worst. |

Mistral Vibe has no auto-update step: each `agento11y vibe` run re-upserts the hook entries, so `AGENTO11Y_AUTO_UPDATE` does not apply.

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your Agent Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

## Troubleshooting

| Symptom | Try |
|---|---|
| Command not found | Reinstall `agento11y` (see step 1). Check `agento11y --version` and that its install dir is on `PATH`. |
| Hooks never fire, and `vibe` warns that `type` should be one of three other names | The `hooks.toml` entries are spelled for the other side of the 2.21.0 rename. Re-run `agento11y vibe`, which rewrites the three types for the installed version. |
| Hooks never fire on Mistral Vibe below 2.21.0 | Those versions read `hooks.toml` only behind a flag. `agento11y vibe` sets `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` for you; when you start `vibe` yourself, export that variable or set `enable_experimental_hooks = true` in `~/.vibe/config.toml`. |
| No `[[hooks]]` entries in `hooks.toml` | Re-run `agento11y vibe` (it upserts them before exec) and check `agento11y doctor`, which reads the same file. |
| Hooks fire but nothing appears in Agent Observability | Check `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TENANT_ID`, and `AGENTO11Y_AUTH_TOKEN`. Without all three the hook logs `not exporting: missing …` and skips the turn. |
| No latency or tool-call charts | Set `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` (or the standard `OTEL_EXPORTER_OTLP_ENDPOINT`). Tool spans only leave the process through the OTel exporter. |
| Prompt or tool content is missing | Check `AGENTO11Y_CONTENT_CAPTURE_MODE`. The default is `metadata_only`. |
| A tool call was blocked unexpectedly | Guards are enabled and a deny rule matched, or `AGENTO11Y_GUARDS_FAIL_OPEN=false` and the guard call itself failed (missing credentials, timeout, transport error). The reason is shown to the model and logged; turn guards off with `AGENTO11Y_GUARDS_ENABLED=false`. |
