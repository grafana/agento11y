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

**Go install (Linux or macOS with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y dsh -- web
```

`agento11y dsh` uses npm to install this package in agento11y's state directory. The default path is `~/.local/state/agento11y/dsh`, or the corresponding path under `XDG_STATE_HOME`. The launcher passes the built bundle to dsh as a `--patch` overlay with an absolute path. It does not modify your dsh profiles, `$DSH_HOME`, or sessions.

- Capture applies only to `agento11y dsh`. Plain `dsh`, or dsh started by another launcher, records nothing.
- `dsh plugin list` does not show the plugin because the plugin is not installed in a profile. Use `agento11y doctor` to inspect it.

Arguments after `--` go to dsh. The launcher adds its overlay before your `--patch`, so your patch can override or disable the `agento11y` row. DeepSeek Harness rejects `--patch` on `dsh plugin` and on a top-level dsh invocation that contains `--dump-default-config`. The launcher omits its overlay in those forms.

## 2. Credentials

`agento11y login` writes `~/.config/agento11y/config.env`. All agento11y plugins read this file. The [pi plugin README](../pi/README.md#2-credentials) has the credential steps, which are the same for dsh.

## 3. Verify

Run one dsh turn, then open **Conversations** in Grafana Cloud Agent Observability. A new generation should appear within a few seconds.

`agento11y doctor` reports the installed plugin version, whether or not `dsh` is on your `PATH`.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env` and run another turn. Check `~/.local/state/agento11y/logs/agento11y.log`, or the corresponding path under `XDG_STATE_HOME`.

## What is recorded

- One generation per model call. A conversation turn uses `streamText`. A call that dsh makes for compaction or a session title uses `generateText` and sets the `dsh.purpose` metadata key.
- Generation messages omit image, file, and extension blocks because the JavaScript SDK cannot represent them. A system-role message in dsh history stays in its original position but is exported with the `user` role. The SDK supports a leading system prompt but has no system-history role.
- One `execute_tool` span per tool dispatch, using the dsh session's conversation ID and title. Its arguments and result reflect `tools/execute` before post-execute and final transforms.
- Token counts. dsh reports uncached input, cache reads, cache writes, and an exact total when the provider supplies one. The plugin derives `total_tokens` from the four component counts only when that total is absent. `input_tokens` is only the uncached part, not the billed input.
- The conversation title dsh derives for the session.
- The stop reason as dsh reports it (`stop`, `tool-calls`, `max-tokens`, `aborted`, `error`), without translation.
- A subagent session records under the agent name `dsh/subagent`.

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

The plugin does not scrub tags, metadata, user IDs, or cwd values. Do not put secrets in them.

Guards are not implemented for dsh. `AGENTO11Y_GUARDS_ENABLED` does nothing here.

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
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | `false` | Opt in to client tags for the user, the repository, and the branch. These reach OTel metrics as `agento11y_tag_*` labels. The plugin loads config once per dsh process, so they freeze at the first model call and describe the process's own directory, which under `dsh web` is the server's. |
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

dsh is pre-1.0 and states that it will make compatibility-breaking changes. This plugin is tested against DeepSeek Harness commit `c389f96bf3` (`0.1.3-alpha.2`). It wraps `llm/stream` and `tools/execute` and reads the optional `sessions` and `sessionTitle` services. The structural type mirrors in [`src/dsh.ts`](src/dsh.ts) use the same names as their upstream declarations. A dsh release can require updates to these types and services.
