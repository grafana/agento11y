# sigil-copilot: GitHub Copilot CLI plugin for Grafana Sigil

Forwards completed GitHub Copilot CLI turns, hook-visible tool calls, error
metadata, subagent lifecycle metadata, and optional prompt/tool content to
[Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).
Ships as a GitHub Copilot CLI plugin.

## Status

This plugin is experimental.

GitHub Copilot CLI plugin support is still evolving, and the current documented
hook payloads do not expose final assistant response text, reliable full token
usage, or stable native turn IDs for completed turns. This plugin therefore
exports one generation per completed turn using a local synthetic turn ID at
the hook layer, then enriches the completed turn from Copilot CLI's local
`events.jsonl` transcript when that artifact is present.

## Install

Install the binary first:

```bash
go install github.com/grafana/sigil-sdk/plugins/copilot/cmd/sigil-copilot@latest
```

Verify the binary is on the `PATH` visible to Copilot hook subprocesses:

```bash
which sigil-copilot
```

Then install the plugin into GitHub Copilot CLI. The current
[GitHub Copilot CLI plugin reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-plugin-reference)
documents both local-path and GitHub-subdirectory install forms:

```bash
copilot plugin install ./plugins/copilot
```

or:

```bash
copilot plugin install grafana/sigil-sdk:plugins/copilot
```

Confirm the install:

```bash
copilot plugin list
```

You should see `sigil-copilot` in the installed plugin list.

## Required Settings

No Copilot feature flag is required beyond a working GitHub Copilot CLI install.
This plugin relies on Copilot CLI's documented plugin and hook support.

## Get Your Credentials From Grafana Cloud

Generation export needs three values from your Grafana Cloud stack: the Sigil
API URL, an instance ID, and an access policy token. For the complete AI
Observability UI, also configure an OTLP endpoint so traces and metrics are
available.

### Sigil API URL and Instance ID

In **Observability -> AI Observability -> Configuration**, copy:

- **API URL** -> `SIGIL_ENDPOINT`
- **Instance ID** -> `SIGIL_AUTH_TENANT_ID`

### Access Policy Token

Create a Grafana Cloud access policy token with at least `sigil:write`.
If you also want traces and metrics, include write scopes for those signals too.

- Token -> `SIGIL_AUTH_TOKEN`

### OTLP Endpoint

In the Grafana Cloud portal for your stack, copy the **OTLP Endpoint URL**:

- OTLP endpoint -> `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`

## Configure

Configuration is read from environment variables and from a dotenv file at:

```text
${XDG_CONFIG_HOME:-$HOME/.config}/sigil-copilot/config.env
```

OS environment variables win per key. The config file is the reliable place to
store credentials for hook subprocesses.

Example:

```bash
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/sigil-copilot"
chmod 700 "${XDG_CONFIG_HOME:-$HOME/.config}/sigil-copilot"
cat > "${XDG_CONFIG_HOME:-$HOME/.config}/sigil-copilot/config.env" <<'EOF'
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<stack-instance-id>
SIGIL_AUTH_TOKEN=<grafana-cloud-access-policy-token>
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
EOF
chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/sigil-copilot/config.env"
```

Supported variables:

| Variable | Required | Description |
| --- | --- | --- |
| `SIGIL_ENDPOINT` | yes | Sigil API URL. The plugin appends `/api/v1/generations:export`. |
| `SIGIL_AUTH_TENANT_ID` | yes | Grafana Cloud instance ID. Used as Basic-auth username and tenant header. |
| `SIGIL_AUTH_TOKEN` | yes | Grafana Cloud access policy token with `sigil:write`. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | for full UI | OTLP HTTP endpoint for traces and metrics. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `SIGIL_OTEL_AUTH_TOKEN` | no | Optional OTel Basic-auth password override. Defaults to `SIGIL_AUTH_TOKEN`. |
| `SIGIL_OTEL_EXPORTER_OTLP_INSECURE` | no | `true` disables TLS. Falls back to `OTEL_EXPORTER_OTLP_INSECURE`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | Standard OTel endpoint fallback. |
| `OTEL_EXPORTER_OTLP_HEADERS` | no | Standard OTel headers. Basic auth is synthesized from Sigil credentials when `Authorization` is absent. |
| `OTEL_EXPORTER_OTLP_INSECURE` | no | Standard OTel TLS toggle fallback. |
| `OTEL_SERVICE_NAME` | no | OTel service name. Defaults to `sigil-copilot`. |
| `SIGIL_CONTENT_CAPTURE_MODE` | no | `metadata_only`, `full`, or `no_tool_content`. Default: `metadata_only`. |
| `SIGIL_TAGS` | no | Comma-separated `key=value` tags added by the SDK. |
| `SIGIL_USER_ID` | no | User id override. |
| `SIGIL_DEBUG` | no | `true` writes logs under the plugin state directory. |

## Supported Hook Events

The plugin registers these GitHub Copilot CLI hook triggers:

- `sessionStart`
- `sessionEnd`
- `userPromptSubmitted`
- `preToolUse`
- `postToolUse`
- `postToolUseFailure`
- `errorOccurred`
- `subagentStart`
- `subagentStop`
- `agentStop`

This plugin config uses the lower-camel event names shown in the GitHub Copilot
CLI docs. The handler also accepts the PascalCase / VS Code-compatible payload
shape for compatibility. `agentStop` is the completed-turn export boundary.
`sessionEnd` performs cleanup only.

## Content Capture

| Mode | User prompt | Assistant text | Tool args/results | Error text |
| --- | --- | --- | --- | --- |
| `metadata_only` | stripped | stripped | tool names/status only | stripped |
| `no_tool_content` | included with redaction | included with redaction | tool names/status only | stripped |
| `full` | included with redaction | included with redaction | included with redaction | included with redaction |

Redaction happens before export.

## What Gets Exported

When the documented Copilot hook payloads provide it, the plugin exports:

- session id as the Sigil conversation id
- a local synthetic turn id in metadata
- user prompt text
- assistant response text from the local Copilot transcript when available
- model, Copilot CLI version, reasoning effort, and request/message identifiers from the local Copilot transcript when available
- output token counts from the local Copilot transcript when available
- tool names and tool call order
- tool arguments and results when `SIGIL_CONTENT_CAPTURE_MODE=full`
- tool status from `PostToolUse` and `PostToolUseFailure`
- error metadata from `ErrorOccurred`
- subagent lifecycle metadata from `SubagentStart` and `SubagentStop`
- timestamps and turn duration

The plugin always tags exported generations with:

- `entrypoint=copilot`
- `cwd` when Copilot includes it
- `hook.source` when Copilot includes it

## Limitations / Known Gaps

- The current documented Copilot hook payloads still do not carry final assistant response text, model, or usage directly. This plugin recovers those fields from the local Copilot CLI `events.jsonl` transcript on `agentStop`.
- Current observed Copilot CLI transcripts expose assistant text, model, request ids, message ids, native turn ids, reasoning effort, and output token counts. They do not appear to expose input token counts, cache token counts, or reasoning token counts for completed turns, so usage and cost can still be partial.
- Copilot does not document a stable native turn ID in these hook payloads. This plugin creates a local monotonic turn ID per session and hashes it into the Sigil generation id.
- Subagent hooks do not currently expose enough durable identity to synthesize separate child generations safely, so subagent activity is exported as parent-turn metadata only.
- This package targets Copilot CLI plugin installation. Copilot cloud agent uses repository-level `.github/hooks/*.json` instead, and GitHub documents cloud-agent outbound network access as restricted by the firewall by default.

## Verification

1. Install the plugin and confirm it appears in `copilot plugin list`.
2. Set valid Sigil credentials in the config file above.
3. Start a Copilot CLI session in a repository and give it a prompt that triggers at least one tool call.
4. Open Grafana AI Observability and look for generations with `agent_name=copilot`.
5. Confirm the generation metadata includes `copilot.turn_id`, `copilot.native_turn_id`, and `copilot.request_id`.
6. Confirm the generation shows the Copilot model and any available output token count.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Plugin does not appear in `copilot plugin list` | Re-run `copilot plugin install ./plugins/copilot` or the GitHub subdir install form. Confirm `plugin.json` is at the plugin root. |
| Hooks run but nothing appears in Sigil | Check `SIGIL_ENDPOINT`, `SIGIL_AUTH_TENANT_ID`, and `SIGIL_AUTH_TOKEN`. Without all three, the plugin discards the completed fragment. |
| No latency/tool charts in AI Observability | Set `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` so the plugin can emit traces and metrics. |
| Prompt or tool content is missing | Check `SIGIL_CONTENT_CAPTURE_MODE`. The default is `metadata_only`. |
| Assistant response text is missing | Check that `agentStop` included a readable `transcriptPath` and that the local `events.jsonl` transcript still exists under `~/.copilot/session-state/<session-id>/`. |
| Model or output tokens are still missing | The local Copilot transcript for that turn did not include those fields. This plugin can only export the fields Copilot recorded locally. |
| Cloud agent cannot reach Sigil | Expected unless your admin allows the Sigil endpoint through the cloud-agent firewall. This plugin is documented for Copilot CLI first. |
