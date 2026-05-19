# sigil-codex: Codex plugin for Grafana Sigil

Forwards completed Codex generations, supported hook-visible tool calls, timing,
best-effort subagent relationships, and optional content to
[Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).
Backed by the consolidated [`sigil`](../sigil/) binary; this plugin only ships
the hook manifest.

## Status

This plugin is experimental while Codex hooks and plugin lifecycle config remain
feature-flagged. The current public
[Codex hooks documentation](https://developers.openai.com/codex/hooks) names
the hook flag `codex_hooks`; some local Codex builds expose split `hooks` and
`plugin_hooks` flags instead. Use `codex features list` to confirm the flag
names for your installed Codex version.

## Install

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
```

`go install` drops the binary in `$GOBIN` (usually `~/go/bin`). The hook
manifest invokes `sigil codex hook` directly, so make sure `$GOBIN` is on
`PATH` for the shell that launches Codex. Verify with:

```sh
sigil --version
```

Then add the plugin marketplace:

```sh
codex plugin marketplace add grafana/sigil-sdk
```

Enable the plugin in `~/.codex/config.toml`:

```toml
[plugins."sigil-codex@grafana-sigil"]
enabled = true
```

If your Codex build exposes a plugin UI, you can enable
`sigil-codex@grafana-sigil` there instead. The success check is the same:
restart Codex, open `/hooks`, and confirm the hook source is
`Plugin - sigil-codex@grafana-sigil`.

The plugin registers Codex lifecycle hooks for you; no manual `~/.codex/hooks.json`
file is needed.

## Enable Codex plugin hooks

Codex hook flags live in `~/.codex/config.toml`. Current
[public docs](https://developers.openai.com/codex/hooks) show:

```toml
[features]
codex_hooks = true
```

Some Codex builds expose the equivalent capability as:

```toml
[features]
hooks = true
plugin_hooks = true
```

Run `codex features list` and enable the hook/plugin hook flags your build
reports. If Codex marks a hook feature as under development, you can hide that
startup warning after intentionally enabling it with this top-level key:

```toml
suppress_unstable_features_warning = true
```

Restart Codex after changing `~/.codex/config.toml`.

## Get your credentials from Grafana Cloud

Generation export needs three values from your Grafana Cloud stack: the Sigil
API URL, an instance ID, and an access policy token. For the full AI
Observability UI, also configure the OTLP endpoint so traces, latency charts,
and tool-call panels populate.

### Sigil API URL and Instance ID

In **Observability -> AI Observability -> Configuration**
(`https://<stack>.grafana.net/plugins/grafana-sigil-app`), copy:

- **API URL** -> `SIGIL_ENDPOINT`. Looks like
  `https://sigil-prod-<region>.grafana.net`.
- **Instance ID** -> `SIGIL_AUTH_TENANT_ID`. Numeric stack ID. Used as the
  Basic-auth username and tenant header.

### Access policy token

In **Administration -> Users and access -> Cloud access policies**
(`https://<stack>.grafana.net/a/grafana-auth-app`), click **Create access
policy**. One token can cover both the generations channel and OTel:

- **Scopes**: add `sigil:write` and write scopes for traces, metrics and logs.
- Click **Create**, then **Add token** on the new policy. Copy the `glc_...`
  token once; Grafana Cloud will not show it again.

This token -> `SIGIL_AUTH_TOKEN`. The same value is reused for OTel auth unless
you set `SIGIL_OTEL_AUTH_TOKEN` yourself.

### OTLP endpoint

Generation export works without OTel, but the AI Observability UI relies on
traces and metrics for latency charts, tool-call breakdowns, and other panels.
Treat OTel as part of normal setup when you want the complete UI.

Open the **Grafana Cloud Portal**, click into your stack, and find the
**OpenTelemetry** card. Copy:

- **OTLP Endpoint URL** -> `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`. Looks like
  `https://otlp-gateway-prod-<region>.grafana.net/otlp`.

## Configure

Configuration is read from environment variables and from a dotenv file at
`${XDG_CONFIG_HOME:-$HOME/.config}/sigil/config.env`. OS env wins per-key; the
file fills in unset keys. Codex hook subprocesses may not inherit your shell
profile, so the file is the reliable place to put credentials. Keep
`~/.codex/config.toml` for Codex feature flags and plugin enablement, not for
Sigil credentials.

The dotenv loader only imports `SIGIL_*` keys and the supported OTel keys
documented below. It intentionally ignores unrelated process settings such as
`PATH`.

Create the file:

```sh
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/sigil"
chmod 700 "${XDG_CONFIG_HOME:-$HOME/.config}/sigil"
cat > "${XDG_CONFIG_HOME:-$HOME/.config}/sigil/config.env" <<'EOF'
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<stack-instance-id>
SIGIL_AUTH_TOKEN=<grafana-cloud-access-policy-token>
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
EOF
chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/sigil/config.env"
```

For local debugging or explicit content capture, add these lines to
`config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
SIGIL_DEBUG=true
```

For normal use, omit `SIGIL_CONTENT_CAPTURE_MODE` so the plugin defaults to
`metadata_only`. Use `full` when you intentionally want prompt, assistant,
and tool content exported.

OTel auth defaults to `SIGIL_AUTH_TENANT_ID` as the Basic-auth username and
`SIGIL_AUTH_TOKEN` as the Basic-auth password. Override the password with
`SIGIL_OTEL_AUTH_TOKEN` if you want a separate token for traces and metrics.
When `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` is set, the plugin passes that endpoint
and synthesised Sigil auth directly to the OTel exporters, so inherited
signal-specific OTel variables such as `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` do
not override the Sigil destination.

| Variable | Required | Description |
| --- | --- | --- |
| `SIGIL_ENDPOINT` | yes | Sigil API URL. The plugin appends `/api/v1/generations:export`. |
| `SIGIL_AUTH_TENANT_ID` | yes | Grafana Cloud stack/instance ID. Used as Basic-auth username and tenant header. |
| `SIGIL_AUTH_TOKEN` | yes | Grafana Cloud token with `sigil:write`; also used to synthesise OTel Basic auth when needed. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | for full UI | Base OTLP HTTP endpoint for traces and metrics. The plugin appends `/v1/traces` and `/v1/metrics`. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `SIGIL_OTEL_AUTH_TOKEN` | no | OTel Basic-auth password. Defaults to `SIGIL_AUTH_TOKEN`. |
| `SIGIL_OTEL_EXPORTER_OTLP_INSECURE` | no | `true` to disable TLS. Falls back to `OTEL_EXPORTER_OTLP_INSECURE`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | Plain OTel endpoint fallback if the Sigil-prefixed endpoint is unset. |
| `OTEL_EXPORTER_OTLP_HEADERS` | no | Plain OTel headers. If no authorization header is present, the plugin synthesises Basic auth from Sigil credentials. |
| `OTEL_EXPORTER_OTLP_INSECURE` | no | Plain OTel TLS toggle fallback. |
| `OTEL_SERVICE_NAME` | no | OTel service name. Defaults to `sigil` when unset. |
| `SIGIL_CONTENT_CAPTURE_MODE` | no | `metadata_only`, `full`, or `no_tool_content`. Default: `metadata_only`. |
| `SIGIL_TAGS` | no | Comma-separated `key=value` tags added by the SDK. |
| `SIGIL_USER_ID` | no | User id override read by the SDK. |
| `SIGIL_DEBUG` | no | `true` writes logs under `$XDG_STATE_HOME/sigil/logs/sigil.log`. |

Without `SIGIL_ENDPOINT`, `SIGIL_AUTH_TENANT_ID`, or `SIGIL_AUTH_TOKEN`, hooks
still run and discard the current fragment without emitting telemetry.

## Content Capture

| Mode | User prompt | Assistant text | Tool args/results | Reasoning |
| --- | --- | --- | --- | --- |
| `metadata_only` | stripped | stripped | tool names/status only; raw args/results dropped | omitted |
| `no_tool_content` | included | included | tool names/status only; raw args/results dropped | omitted |
| `full` | included with redaction | included with redaction | included with redaction | omitted |

In `full` and `no_tool_content`, allowed prompt and assistant text can be stored
raw in local fragment files until the turn exports or stale cleanup removes the
fragment. Files are written with `0600` permissions. Redaction happens before
data is sent to Sigil.

## How It Works

Codex invokes `SessionStart`, `UserPromptSubmit`, `PostToolUse`, and `Stop`
hooks. The agent adapter stores a lightweight per-turn fragment under
`$XDG_STATE_HOME/sigil/codex` (default `~/.local/state/sigil/codex`), then
flushes one generation on `Stop`. Relative `XDG_CONFIG_HOME` and
`XDG_STATE_HOME` values are ignored; config and state paths are always
absolute. Session and turn ids are sanitised and hashed before becoming
filenames so similar ids cannot collide on disk.

When Codex spawns a subagent, current hook payloads do not provide a stable
parent id. The plugin therefore performs a metadata-only lookup in Codex's
local transcripts. If the child transcript says it is a subagent and the parent
transcript contains the matching `spawn_agent` output, Sigil receives the child
as `agent_name=codex/subagent` with a parent-generation edge. If only the child
side can be proven, the generation is still labeled `codex/subagent` but
remains in the child conversation with partial-link metadata. If the transcript
shape is missing or changes, the plugin exports the turn normally rather than
failing the Codex session.

`Stop` is the completed-turn boundary. If you exit after a turn finishes, the
generation should already be exported. If you exit or interrupt while Codex is
still thinking, Codex may not send `Stop`, and that incomplete turn is not
exported by this plugin. The exported generation includes a
`codex.stop_hook_active` tag so reviewers can tell whether the `Stop` hook fired
while Codex was already continuing from another Stop hook.

Do not combine this plugin with another `Stop` hook that continues the same
turn with `decision: "block"` or `continue: false` behaviour. Codex launches
matching command hooks concurrently, so the hook cannot delay export after
another Stop hook decides to keep the turn going. That setup can create partial
or duplicate Sigil generations.

Interrupted turns can leave local state under
`${XDG_STATE_HOME:-$HOME/.local/state}/sigil/codex`. The `turns/` directory can
contain incomplete turn fragments; in `full` or `no_tool_content` mode, those
fragments can contain the content allowed by that mode. The `sessions/`
directory contains session metadata such as cwd, model, source, and transcript
path. The `subagents/` directory contains metadata-only child-to-parent link
files. The plugin performs best-effort cleanup of session, turn, and subagent
link files older than 24 hours on later hook invocations. It does not retry or
replay interrupted turns.

The hook always exits successfully. Sigil export failures after credentials are
present keep the current fragment only until stale cleanup; missing credentials
are treated as local configuration absence and discard the current fragment.
Stop export is bounded below the Codex hook timeout and uses a reduced retry
budget so a down Sigil endpoint fails cleanly instead of leaving Codex waiting
on long SDK backoff.

Codex hook payloads do not place token counts directly in hook stdin, but they
can include `transcript_path`. On `Stop`, the plugin reads the Codex rollout
JSONL at that path and looks for `event_msg` records whose payload type is
`token_count`. It attributes those records by the active `turn_context.turn_id`
and computes Sigil generation usage from the completed turn's cumulative
`total_token_usage` delta, not from `last_token_usage`. That avoids
undercounting turns that make more than one model sampling call before `Stop`.
If Codex emits a `token_count` immediately after `turn_context` but before any
assistant model activity, the plugin treats it as the resumed-thread baseline
rather than usage for the new turn. If the rollout is missing, malformed,
oversized, or cannot be attributed unambiguously, the generation still exports
without usage.

When usage is available, the plugin sets Sigil `Generation.Usage` fields for
input, output, total, cache-read, and reasoning tokens. It stores final
cumulative totals and context window as generation metadata, not tags, to avoid
high-cardinality tag values. The plugin does not calculate monetary cost; cost
should be derived downstream from the provider, model, and token counts when
pricing metadata is available.

Current documented payloads do not provide a generic tool status or duration
field [OpenAI Codex hooks docs](https://developers.openai.com/codex/hooks).
Tool status is marked as an error only when Codex provides an explicit
error/status field or a known response shape such as a non-zero `exit_code`;
otherwise the status is left unknown. Codex `PostToolUse` currently covers
supported hook tools such as Bash, `apply_patch`, and MCP calls; it does not
cover every shell execution path, WebSearch, or other non-shell, non-MCP tools.

## Verify it worked

Start a fresh Codex session after changing plugin or config files:

```sh
codex
```

Open `/hooks`. A first install should show four hooks with source:

```text
Plugin - sigil-codex@grafana-sigil
```

Review and trust the `SessionStart`, `UserPromptSubmit`, `PostToolUse`, and
`Stop` hooks. That trust prompt is expected. Hook trust is hash-based, so Codex
will ask again if the plugin hook manifest changes.

Run one turn that uses a shell command, then let the turn finish normally so
the `Stop` hook can flush. It is fine to `/exit` after the assistant has
completed the response. Check **Observability -> AI Observability ->
Conversations** in Grafana Cloud; a new Codex generation should appear within
seconds.

To verify subagent linkage, run a prompt that asks Codex to spawn and wait for
a small subagent. When the parent/child transcript metadata is available, Sigil
should show a parent `codex` generation and a child `codex/subagent` generation
connected by a parent-generation edge.

If `SIGIL_DEBUG=true` is set, you can also verify local hook activity with:

```sh
tail -n 50 "${XDG_STATE_HOME:-$HOME/.local/state}/sigil/logs/sigil.log"
```

Expected log evidence includes `dispatch: event=UserPromptSubmit`,
`dispatch: event=PostToolUse`, `dispatch: event=Stop`, and
`stop: emitted session=...`.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `/hooks` is empty | Run `codex features list`, enable the hook/plugin hook flags for your Codex build, enable `plugins."sigil-codex@grafana-sigil"`, then restart Codex. |
| Hooks are installed but inactive | Open `/hooks`, review each hook, and trust it. First-run review is expected. |
| Hook command cannot run | Install the binary with `go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest`, and confirm `sigil --version` works in a fresh terminal. |
| No Sigil data appears | Let a turn finish normally so `Stop` runs, check the debug log, and verify `SIGIL_ENDPOINT`, `SIGIL_AUTH_TENANT_ID`, and `SIGIL_AUTH_TOKEN` are set in `${XDG_CONFIG_HOME:-$HOME/.config}/sigil/config.env` or process env. |
| Data appears for completed turns but not interrupted ones | This is expected: Codex does not send `Stop` for a turn interrupted before completion. Stale local state is removed after 24 hours by later hook invocations. |
| A subagent appears as a normal Codex turn | The plugin could not prove the child -> parent relationship from local transcript metadata. The turn still exports; check debug logs for `subagent:` messages if `SIGIL_DEBUG=true`. |
| A subagent appears as `codex/subagent` but is not connected to the parent | The child transcript proved it was a subagent, but the parent turn or `spawn_agent` output was unavailable. The generation remains visible with partial-link metadata. |
| Under-development feature warning appears | Set top-level `suppress_unstable_features_warning = true` after intentionally enabling `plugin_hooks`. |
