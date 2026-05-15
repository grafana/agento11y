# sigil-cursor — Cursor → Grafana Sigil plugin

Forwards Cursor agent generations (prompts, assistant replies, tool calls,
token usage) to Grafana Sigil for AI observability. Backed by the
consolidated [`sigil`](../sigil/) binary; this plugin only ships the hook
manifest and a wrapper script.

## Install

Cursor's hook runtime does not manage dependencies. Install the binary
yourself:

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
```

This places `sigil` in `$GOBIN` (default `$HOME/go/bin`). The wrapper at
[`plugins/cursor/scripts/run.sh`](scripts/run.sh)
probes `$SIGIL_BIN`, `$HOME/go/bin/sigil`, `/opt/homebrew/bin/sigil`,
`/usr/local/bin/sigil`, and `$HOME/.local/bin/sigil` to find it under
Cursor's stripped hook environment.

Then register the plugin in Cursor:

```
/add-plugin grafana/sigil-sdk
```

## Configure

Configuration is read from environment variables and from a dotenv file at
`${XDG_CONFIG_HOME:-$HOME/.config}/sigil/config.env`. OS env wins
per-key; the file fills in unset keys. Cursor runs hooks under a clean
process environment that does not inherit your shell profile, so the file
is the reliable place to put credentials.

The Sigil and OTel env-var schemas come from the SDKs, not this plugin:

- `SIGIL_*` — read by the Sigil Go SDK
  ([`go/sigil/env.go`](../../go/sigil/env.go)).
- `OTEL_EXPORTER_OTLP_*` — read by the OpenTelemetry Go SDK exporters.

| Variable | Required | Purpose |
|---|---|---|
| `SIGIL_ENDPOINT` | yes | Sigil API URL, for example `https://sigil-prod-<region>.grafana.net`. |
| `SIGIL_AUTH_TENANT_ID` | yes | Grafana Cloud stack/instance ID. Used as Basic-auth username and the `X-Scope-OrgID` header. |
| `SIGIL_AUTH_TOKEN` | yes | `glc_…` Cloud access policy token with the `sigil:write` scope. |
| `SIGIL_TAGS` | no | Comma-separated `k=v` pairs added to every generation. Built-ins (`git.branch`, `cwd`, `subagent`) win on collision. |
| `SIGIL_CONTENT_CAPTURE_MODE` | no | `full`, `no_tool_content`, or `metadata_only` (default). |
| `SIGIL_USER_ID` | no | Override the per-generation user id (default: `user_email` from Cursor's payload). |
| `SIGIL_DEBUG` | no | `true` writes a log to `$XDG_STATE_HOME/sigil/logs/sigil.log`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | yes | OTLP HTTP endpoint for traces and metrics. The plugin runs without it, but the AI Observability UI depends on these signals; half the panels are empty without them. |
| `OTEL_EXPORTER_OTLP_HEADERS` | no | Extra OTLP headers (CSV `k=v`). When `Authorization` is missing the plugin synthesises `Authorization=Basic base64(SIGIL_AUTH_TENANT_ID:SIGIL_AUTH_TOKEN)` so the OTel SDK exporter picks it up. |
| `OTEL_EXPORTER_OTLP_INSECURE` | no | Standard OTel toggle. `true` disables TLS for the OTLP endpoint. |
| `SIGIL_BIN` | no | Override the binary path used by the wrapper script. |

Without `SIGIL_ENDPOINT` / `SIGIL_AUTH_TENANT_ID` / `SIGIL_AUTH_TOKEN`,
hooks still run but nothing is emitted to Sigil.

### Where to find these values

- `SIGIL_ENDPOINT` — Grafana Cloud → AI Observability → Configuration,
  **API URL** field.
- `SIGIL_AUTH_TENANT_ID` — Grafana Cloud → AI Observability →
  Configuration, **Instance ID** field. Numeric Grafana Cloud stack id.
- `SIGIL_AUTH_TOKEN` — a Grafana Cloud access policy token (grafana.com →
  Security → Access Policies), with the realm set to the stack's region.
  Required scope: `sigil:write`. Add `metrics:write`, `metrics:import`,
  and `traces:write` when also setting `OTEL_EXPORTER_OTLP_ENDPOINT` —
  one token can cover both channels.
- `OTEL_EXPORTER_OTLP_ENDPOINT` — Grafana Cloud → your stack →
  OpenTelemetry card, **OTLP Endpoint URL**. The OTLP gateway region is
  tied to the stack's region, which can differ from the Sigil region.

## Content capture

| Mode | User prompt | Assistant text | Thinking | Tool args / results |
|---|---|---|---|---|
| `full` | included | included | presence-only | included |
| `no_tool_content` | included | included | presence-only | structure kept, content stripped |
| `metadata_only` (default) | stripped | stripped | presence-only | stripped |

Thinking content is never exported regardless of mode — only
`thinking_enabled: true` is set on the Generation.

## Built-in tags

The plugin sets these tags automatically; user values from `SIGIL_TAGS`
lose to built-ins on key collision (the SDK applies `SIGIL_TAGS` as the
client-level base layer; the plugin sets built-ins per-generation, which
override on the same key):

- `git.branch` — best-effort branch resolution. Walks up to 6 parent
  directories from the workspace root looking for a `.git` entry; follows
  `gitdir: <path>` indirection used by worktrees and submodules; reads
  HEAD from the resolved git directory. Returns the branch name on a
  symbolic ref, the first 12 hex chars on detached HEAD, or nothing on
  failure.
- `cwd` — first `cwd` observed in `postToolUse`, falling back to the first
  workspace root.
- `subagent` — `"true"` for background-agent runs.
- `entrypoint` — never auto-populated. Set via `SIGIL_TAGS` if you want
  a value here. Cursor's `composer_mode` is **not** auto-mapped; the
  equivalence has not been validated.

## How it works

Per-generation state is buffered on disk under
`$XDG_STATE_HOME/sigil/cursor/<conversation>/gen-<generation>.json` (mode
`0600`) until the `stop` event flushes it to Sigil. `sessionEnd` removes
any stranded fragments left by crashed turns.

## Development

The Go code lives in [`../sigil/`](../sigil/). Use the root [`mise`](../../mise.toml) tasks for lint/format/test, or:

```sh
cd plugins/sigil
GOWORK=off go test ./internal/agents/cursor/...
GOWORK=off go build ./cmd/sigil
```
