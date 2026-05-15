# sigil-cc: Claude Code hooks for Sigil

A Claude Code plugin that forwards each session's transcript to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/). Backed by the consolidated [`sigil`](../sigil/) binary; this plugin only ships hook manifest + wrapper plumbing.

## Install

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
```

Then, from inside Claude Code:

```
/plugin marketplace add grafana/sigil-sdk
/plugin install sigil-cc@grafana-sigil
```

The plugin registers the hooks for you; future hook updates ship with the plugin so you don't re-edit `settings.json`.

> `go install` drops the binary in `$GOBIN` (usually `~/go/bin`). The hook manifest invokes `sigil claude-code hook` directly, so make sure `$GOBIN` is on `PATH` for the shell that launches Claude Code. Verify with `sigil --version`.

## Install manually

If you'd rather not use the plugin marketplace, install the binary the same way and wire the hook yourself in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "sigil claude-code hook",
            "timeout": 10000
          }
        ]
      }
    ]
  }
}
```

## Get your credentials from Grafana Cloud

You need four values from your Grafana Cloud stack: the Sigil API URL, an OTLP endpoint, an instance ID, and an access policy token.

### Sigil API URL and Instance ID

In **Observability → AI Observability → Configuration** (`https://<stack>.grafana.net/plugins/grafana-sigil-app`), copy:

- **API URL** → `SIGIL_ENDPOINT`. Looks like `https://sigil-prod-<region>.grafana.net`.
- **Instance ID** → `SIGIL_AUTH_TENANT_ID`. Numeric stack ID. Used as Basic-auth username and the `X-Scope-OrgID` header.

### Access policy token

In **Administration → Users and access → Cloud access policies** (`https://<stack>.grafana.net/a/grafana-auth-app`), click **Create access policy**. One token covers both the generations channel and OTel:

- **Scopes**: tick `metrics: Write` and `traces: Write`. Use **Add scope** to add `sigil:write`.
- Click **Create**, then **Add token** on the new policy. Copy the `glc_…` token once — you can't view it again.

This token → `SIGIL_AUTH_TOKEN`. The same value is reused for OTel auth.

### OTLP endpoint

The AI Observability UI relies on traces and metrics for latency charts, tool call breakdowns, and other panels. Without OTel configured, half the UI is empty — treat this as required.

Open the **Grafana Cloud Portal**, click into your stack, and find the **OpenTelemetry** card. Copy:

- **OTLP Endpoint URL** → `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`. Looks like `https://otlp-gateway-prod-<region>.grafana.net/otlp`.

## Configure

The binary reads its config from the environment. The hook fires inside Claude Code's process, so anything in Claude Code's environment when it starts works.

### Shell environment (recommended)

Keeps tokens out of any global config file.

```sh
export SIGIL_ENDPOINT=https://sigil-prod-us-central-0.grafana.net
export SIGIL_AUTH_TENANT_ID=123456
export SIGIL_AUTH_TOKEN=glc_...
export SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-us-central-0.grafana.net/otlp
export SIGIL_CONTENT_CAPTURE_MODE=full
claude
```

OTel auth defaults to `SIGIL_AUTH_TENANT_ID` (Basic-auth user) and `SIGIL_AUTH_TOKEN` (Basic-auth password). Override the password with `SIGIL_OTEL_AUTH_TOKEN` if you want a separate token.

### `~/.claude/settings.json` `env` block

Persistent across all Claude Code sessions.

```json
{
  "env": {
    "SIGIL_ENDPOINT": "https://sigil-prod-us-central-0.grafana.net",
    "SIGIL_AUTH_TENANT_ID": "123456",
    "SIGIL_AUTH_TOKEN": "glc_...",
    "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp-gateway-prod-us-central-0.grafana.net/otlp",
    "SIGIL_CONTENT_CAPTURE_MODE": "full"
  }
}
```

### Dotenv fallback

When Claude Code launches `sigil` under a stripped environment, the binary reads `$XDG_CONFIG_HOME/sigil/config.env` (default `~/.config/sigil/config.env`) and applies any unset values. Only `SIGIL_*` and a small set of `OTEL_*` keys are honoured.

## Verify it worked

Run any single turn:

```sh
claude  # ask it anything, then exit
```

Then either:

- Check the Sigil UI under **Observability → AI Observability → Conversations**. A new generation should appear within seconds.
- Or enable debug logging and tail the log:

  ```sh
  export SIGIL_DEBUG=true
  claude  # one turn
  tail -f "${XDG_STATE_HOME:-$HOME/.local/state}/sigil/logs/sigil.log"
  ```

If nothing appears, the most common causes are: `sigil` not on `$PATH` (run `sigil --version`), missing `SIGIL_ENDPOINT` / `SIGIL_AUTH_TENANT_ID` / `SIGIL_AUTH_TOKEN`, or a token without the `sigil:write` scope.

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SIGIL_ENDPOINT` | yes | Sigil API URL from AI Observability → Configuration. |
| `SIGIL_AUTH_TENANT_ID` | yes | Grafana Cloud stack/instance ID. Sent as Basic-auth username and `X-Scope-OrgID` header. |
| `SIGIL_AUTH_TOKEN` | yes | `glc_…` access policy token with `sigil:write` (and `metrics:write` / `traces:write` if using OTel). [Access Policies docs](https://grafana.com/docs/grafana-cloud/account-management/authentication-and-permissions/access-policies/). |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | yes | OTLP HTTP endpoint for metrics and traces. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. The plugin runs without it, but the AI Observability UI depends on these signals — half the panels are empty. |
| `SIGIL_OTEL_AUTH_TOKEN` | no | OTel Basic-auth password. Defaults to `SIGIL_AUTH_TOKEN`. The OTel Basic-auth username is always `SIGIL_AUTH_TENANT_ID`. |
| `SIGIL_OTEL_EXPORTER_OTLP_INSECURE` | no | `true` to disable TLS. Falls back to `OTEL_EXPORTER_OTLP_INSECURE`. Default: TLS enabled. |
| `SIGIL_CONTENT_CAPTURE_MODE` | no | Content capture mode: `full`, `metadata_only`, `no_tool_content` (default: `metadata_only`). |
| `SIGIL_TAGS` | no | Comma-separated `key=value` tags added to every generation. Built-in tags (`git.branch`, `cwd`, `entrypoint`, `subagent`) take precedence on collision. |
| `SIGIL_USER_ID` | no | Explicit override for the per-generation user id. When set, it wins over `~/.claude.json` and ignores `SIGIL_USER_ID_SOURCE`. |
| `SIGIL_USER_ID_SOURCE` | no | Field to read from `~/.claude.json` when `SIGIL_USER_ID` is unset: `email` (default) or `accountUuid`. |
| `SIGIL_DEBUG` | no | Set to `true` to log to `$XDG_STATE_HOME/sigil/logs/sigil.log`. |

## How It Works

1. Claude Code fires `SessionStart` and persists the session model (used by tool hooks).
2. Claude Code fires the Stop hook after each turn, passing `{session_id, transcript_path}` on stdin to `sigil claude-code hook`.
3. The agent adapter loads the byte offset from the last run, reads new JSONL lines, maps assistant messages with token usage to Sigil Generation records, and exports via the sigil-sdk HTTP client.
4. On successful flush, the new offset is persisted for the next invocation.

The plugin also registers a `PreToolUse` hook that evaluates Sigil **postflight** guards against the tool call name. If a guard denies, the hook returns a Claude Code `permissionDecision: "deny"` and the tool call is blocked before execution.

Each assistant API response becomes one Generation with model, tokens, tools, timestamps, tags, and conversation title. The hook always exits 0; telemetry is best-effort.

## Content Capture

Content capture is controlled by `SIGIL_CONTENT_CAPTURE_MODE` (default: `metadata_only`):

| Mode | What's sent |
|------|-------------|
| `metadata_only` | Model, tokens, tool names, timestamps, tags. All text content stripped by the SDK. |
| `full` | Full conversation content with automatic secret redaction (see below). |
| `no_tool_content` | Full generation content but tool execution arguments/results excluded from spans. |

When content is included (`full` or `no_tool_content`), automatic redaction is applied:

- User prompts: Tier 1 redaction (known token formats)
- Assistant text: Tier 1 redaction (known token formats)
- Tool inputs/outputs: Tier 1 + Tier 2 redaction (tokens + env-file heuristics)
- Thinking blocks: omitted (noted in metadata)

## Development

The Go code lives in [`../sigil/`](../sigil/). Use the root [`mise`](../../mise.toml) tasks for lint/format/test, or:

```sh
cd plugins/sigil
GOWORK=off go test ./internal/agents/claudecode/...
GOWORK=off go build ./cmd/sigil
```

Manual test:

```sh
echo '{"session_id":"test","transcript_path":"/path/to/session.jsonl"}' | \
  SIGIL_ENDPOINT=https://sigil-prod-us-central-0.grafana.net \
  SIGIL_AUTH_TENANT_ID=123 SIGIL_AUTH_TOKEN=glc_... \
  sigil claude-code hook
```
