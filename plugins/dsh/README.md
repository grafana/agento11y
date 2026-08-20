# @grafana/agento11y-dsh

This [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh`) plugin sends model calls and tool executions to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

By default, the plugin sends only metadata: token counts, model, tool names, and durations. Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is an alias for `metadata_only`. Read [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for details.

## 1. Install and launch

The `agento11y dsh` launcher is supported on macOS and Linux only.

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y dsh -- web
```

**Go install (Linux or macOS with Go 1.26.9+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y dsh -- web
```

`agento11y dsh` uses npm to install this package in agento11y's state directory. The default path is `~/.local/state/agento11y/dsh`, or the corresponding path under `XDG_STATE_HOME`. The launcher passes the built bundle to dsh as a `--patch` overlay with an absolute path. It does not modify your dsh profiles, `$DSH_HOME`, or sessions.

- This plugin captures sessions only when launched through `agento11y dsh`. It does not capture plain `dsh` sessions or sessions started by another launcher.
- `dsh plugin list` does not show the plugin because the plugin is not installed in a profile. Use `agento11y doctor` to inspect it.

Arguments after `--` go to dsh. Profile shorthand such as `headless task` or `rescue --from-default-profile web task` keeps the selected profile. The launcher adds its overlay before your `--patch`, so your patch can override or disable the `agento11y` row. It omits the overlay for an initial `plugin` command and launcher-level `--dump-default-config`. Flags after app input starts belong to the app and do not suppress capture.

The extra-separator form `agento11y dsh -- -- web` remains a wrapper convenience. Raw `dsh -- web` fails because it selects no profile; use `agento11y dsh -- web`.

## 2. Credentials

`agento11y login` writes `~/.config/agento11y/config.env`. All agento11y plugins read this file. The [pi plugin README](../pi/README.md#2-credentials) has the credential steps, which are the same for dsh.

## 3. Verify

Run one dsh turn, then open **Conversations** in Grafana Cloud Agent Observability. A new generation should appear within a few seconds.

`agento11y doctor` reports the installed plugin version, whether or not `dsh` is on your `PATH`.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env` and run another turn. Check `~/.local/state/agento11y/logs/agento11y.log`, or the corresponding path under `XDG_STATE_HOME`.

## What is recorded

- One generation per intercepted `llm/stream` call, including prepared calls. A conversation turn uses `streamText`. A call marked for compaction or a session title uses `generateText` and sets the `dsh.purpose` metadata key.
- The system prompt combines `request.system` with all leading system messages. Text blocks within each system message concatenate without separators; nonempty contributions join with `\n\n`. Leading system messages do not also appear in input.
- Later system/developer text keeps its position using the SDK's `user` role, with full redaction even when human-input redaction is disabled. The SDK has no system-history or developer role.
- Current tool-role messages keep their call ID and error state, even with empty or image-only results. Legacy nested tool results also map.
- Image, file, tool-addition, tool-removal, and extension blocks are omitted individually without dropping adjacent text. Empty system updates cannot be represented, so prompt-clearing semantics are lost.
- One `execute_tool` span per tool dispatch, using the dsh session's conversation ID and title. Its arguments and result reflect `tools/execute` before post-execute and final transforms.
- Token counts. dsh reports uncached input, cache reads, cache writes, and an exact total when the provider supplies one. The plugin derives `total_tokens` from the four component counts only when that total is absent. `input_tokens` is only the uncached part, not the billed input.
- The conversation title dsh derives for the session.
- The stop reason as dsh reports it (`stop`, `tool-calls`, `max-tokens`, `aborted`, `error`), without translation.
- A subagent session records under the agent name `dsh/subagent`.

Capture describes the request at `llm/stream`, not the exact provider payload. Provider adapters can later replace system snapshots or project file, image, and tool history differently.

Direct DeepSeek/Perplexity search-model calls that bypass `llm/stream` are not captured. For auxiliary calls, the plugin uses any upstream session identity and purpose provided. It does not infer missing values. Tool capture covers dispatch, not final `tools/result` outcomes or pre-dispatch denials. History import, guards, and turn-level capture are not supported.

In `no_tool_content` mode, generation messages keep tool-call arguments and tool-result content, but `execute_tool` span bodies are absent.

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation:

```sh
agento11y dsh --tag project=hackathon -- web
```

Every generation includes `cwd`. If that directory is in a Git checkout, the generation also includes `git.branch`. On a detached HEAD, `git.branch` is a 12-character commit SHA. Under `dsh web`, the plugin resolves both values from the directory where the session was created, not the server's directory.

## Redaction

The plugin redacts conversation titles, error messages, and tool payload fields at dsh boundaries. The SDK generation sanitizer handles messages and system prompts.

For titles, errors, assistant text, and reasoning, the lightweight pass redacts known token formats, private keys, database URLs, bearer tokens, and email addresses. System prompts and tool payloads also scrub `KEY=value` pairs. User prompts use this full pass by default. Matches become `[REDACTED:<id>]`.

If redaction would make a valid tool JSON payload invalid, the exported copy becomes `"[REDACTED:json]"`. dsh still receives the original value. Set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to leave user prompts unchanged.

After redaction, tool arguments use a 4 KiB content limit and tool results use 1 MiB, plus truncation markers and JSON encoding. Clipped arguments become JSON strings containing the retained prefix and `[truncated]`. These limits apply per payload; a generation with many tool results can still exceed the SDK's export limit.

The plugin does not scrub tags, metadata, user IDs, or cwd values. Do not put secrets in them.

`AGENTO11Y_GUARDS_ENABLED` has no effect for dsh.

## Common options

The plugin reads `~/.config/agento11y/config.env`. If only the legacy file exists, it reads `~/.config/sigil/config.env` instead. Set options with environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENTO11Y_ENDPOINT` | Not set | Agent Observability API URL (find it at `/plugins/grafana-agento11y-app`) |
| `AGENTO11Y_AUTH_TENANT_ID` | Not set | Grafana Cloud instance ID. Combined with `AGENTO11Y_AUTH_TOKEN` becomes Basic auth for Agent Observability and OTLP. |
| `AGENTO11Y_AUTH_TOKEN` | Not set | Cloud access policy token (`glc_...`). |
| `AGENTO11Y_AGENT_NAME` | `dsh` | Agent name reported to Agent Observability. |
| `AGENTO11Y_AGENT_VERSION` | Not set | Optional version string reported with the agent. |
| `AGENTO11Y_TAGS` | None | Comma-separated `key=value` tags attached to every generation. |
| `AGENTO11Y_USER_ID` | None | User ID attached to every generation and used by the automatic `user` tag. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags for the user, the repository, and the branch. These reach OTel metrics as `agento11y_tag_*` labels. Each plugin instance loads config and sets these tags at its first model or tool call. The values stay cached with that config and describe the process's directory. For `dsh web`, that is the server's directory. |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` | all names | Narrows the switch above to a comma-separated subset of `user`, `repo`, `branch` (`all` is also accepted). Does nothing while the switch is off. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `AGENTO11Y_DEBUG` | `false` | Write lifecycle events to the agento11y log under the state directory. The plugin does not write them to the terminal because that can corrupt dsh's TUI. |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret patterns in user input messages before export. |
| `AGENTO11Y_EXPORT_TIMEOUT_MS` | `30000` | Timeout for each generation export request. Use a base-10 integer from `1` through `2147483647` milliseconds. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | Not set | OTLP HTTP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_LOCAL` | `false` | Record to the local receiver on this machine instead of Grafana Cloud. `agento11y dsh --local` sets it for one launch. |
| `AGENTO11Y_AUTO_UPDATE` | `true` | Set to `false` to stop the launcher from refreshing the managed dsh plugin. |

File format: one `KEY=value` per line, `#` line comments, an optional `export ` prefix, and optional matching single or double quotes around the value. The plugin accepts only `AGENTO11Y_*` and `SIGIL_*` keys plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME`.

A non-empty operating-system environment value always wins over the file. An empty or whitespace-only value is unset and gets its value from `config.env`. A missing file produces no warning.

## Compatibility

Tested against DeepSeek Harness `0.2.1-rc.2`, commit `687f282e8625c6b8d11ff58211a90524be2e4317`. Automated tests use published `@deepseek-ai/cordis@4.0.5` and `@deepseek-ai/dsh-llm@0.2.1-rc.2`, synthetic adapters, and loopback telemetry receivers. The build check mounts the built module namespace in Cordis without dispatching calls. These checks do not cover the full DSH Loader, native runtime, or tests of the packed npm package in consumer projects.

The plugin wraps `llm/stream` and `tools/execute` and reads optional `sessions` and `sessionTitle` services. [`src/dsh.ts`](src/dsh.ts) names the upstream declarations mirrored locally. The plugin does not reject hosts based on version. Compatibility with other prereleases or future releases is not guaranteed.

Shutdown waits for calls already accepted by the plugin and their exports, but delivery is best effort. DSH allows five seconds for process shutdown. The default export timeout is thirty seconds and can exceed that deadline.

Fresh managed installs cannot capture until `@grafana/agento11y-dsh` is published to npm. Building and testing this source does not publish the package.
