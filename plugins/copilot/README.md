# Sigil for GitHub Copilot CLI

Forwards completed GitHub Copilot CLI turns, hook-visible tool calls, error
metadata, subagent lifecycle metadata, and optional prompt/tool content to
[Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).
Ships as a GitHub Copilot CLI plugin powered by the shared `sigil` binary.

> Experimental. GitHub Copilot CLI plugin support is still evolving, and the
> current documented hook payloads do not expose final assistant response text,
> reliable full token usage, or stable native turn IDs for completed turns. This
> plugin therefore exports one generation per completed turn using a local
> synthetic turn ID at the hook layer, then enriches the completed turn from
> Copilot CLI's local `events.jsonl` transcript when that artifact is present.

## 1. Install and launch

```sh
brew install grafana/grafana/sigil
sigil copilot
```

`sigil copilot` registers `sigil-copilot` on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches Copilot CLI.

<details>
<summary>Manual plugin registration</summary>

The current
[GitHub Copilot CLI plugin reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-plugin-reference)
documents both local-path and GitHub-subdirectory install forms:

```sh
copilot plugin install ./plugins/copilot
```

or:

```sh
copilot plugin install grafana/sigil-sdk:plugins/copilot
```

Confirm the install:

```sh
copilot plugin list
```

</details>

## 2. Credentials

When `sigil copilot` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
   - **API URL** → `SIGIL_ENDPOINT`
   - **Instance ID** → `SIGIL_AUTH_TENANT_ID`

2. **Administration → Users and access → Cloud access policies**
   - Create a policy with scopes `sigil:write`, `metrics:write`, `traces:write`.
   - Add a token. The `glc_…` value is shown once → `SIGIL_AUTH_TOKEN`.

3. **Grafana Cloud Portal → your stack → OpenTelemetry card**
   - **OTLP endpoint URL** → `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT`

Run `sigil login` later to update saved credentials.

<details>
<summary>Non-interactive config.env</summary>

Create or update `~/.config/sigil/config.env`:

```dotenv
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<instance-id>
SIGIL_AUTH_TOKEN=glc_...
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

</details>

To also send the conversation text (with automatic secret redaction), add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Start a Copilot CLI session in a repository and give it a prompt that triggers
at least one tool call. The plugin only exports completed turns at `agentStop`.
Then open **AI Observability → Conversations** in Grafana Cloud and look for
generations with `agent_name=copilot`.

If nothing shows up:

```sh
SIGIL_DEBUG=true sigil copilot   # one turn
tail -f ~/.local/state/sigil/logs/sigil.log
```

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

`agentStop` is the completed-turn export boundary. `sessionEnd` performs cleanup
only. The handler accepts both the lower-camel and PascalCase event payload
shapes.

## Content Capture

| Mode | User prompt | Assistant text | Tool args/results | Error text |
| --- | --- | --- | --- | --- |
| `metadata_only` | stripped | stripped | tool names/status only | stripped |
| `no_tool_content` | included with redaction | included with redaction | tool names/status only | stripped |
| `full` | included with redaction | included with redaction | included with redaction | included with redaction |

Redaction happens before export.

## All options

| Variable | Default | Description |
|---|---|---|
| `SIGIL_ENDPOINT` | — | Sigil API URL. Find it at `/plugins/grafana-sigil-app`. |
| `SIGIL_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `SIGIL_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the AI Observability latency and tool-call panels stay empty. |
| `SIGIL_OTEL_AUTH_TOKEN` | `SIGIL_AUTH_TOKEN` | Override the OTel password. |
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, or `full`. |
| `SIGIL_TAGS` | — | `key=value,key=value` tags added to every generation. |
| `SIGIL_USER_ID` | — | Override the user id. |
| `SIGIL_DEBUG` | `false` | Log to `~/.local/state/sigil/logs/sigil.log`. |
| `SIGIL_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Copilot `preToolUse` hook is evaluated against Sigil and tool calls denied by guard rules are blocked. |
| `SIGIL_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `SIGIL_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |
| `SIGIL_AUTO_UPDATE` | `true` | Refresh the `sigil-copilot` plugin automatically. Set `false` to pin the installed version. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your AI Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

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

## Troubleshooting

| Symptom | Try |
|---|---|
| Plugin does not appear in `copilot plugin list` | Re-run `sigil copilot` or `copilot plugin install grafana/sigil-sdk:plugins/copilot`. Confirm `plugin.json` is at the plugin root. |
| Command not found | Reinstall: `brew install grafana/grafana/sigil`. Check `sigil --version`. |
| Hooks run but nothing appears in Sigil | Check `SIGIL_ENDPOINT`, `SIGIL_AUTH_TENANT_ID`, and `SIGIL_AUTH_TOKEN`. Without all three, the plugin discards the completed fragment. |
| No latency/tool charts in AI Observability | Set `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` so the plugin can emit traces and metrics. |
| Prompt or tool content is missing | Check `SIGIL_CONTENT_CAPTURE_MODE`. The default is `metadata_only`. |
| Assistant response text is missing | Check that `agentStop` included a readable `transcriptPath` and that the local `events.jsonl` transcript still exists under `~/.copilot/session-state/<session-id>/`. |
| Model or output tokens are still missing | The local Copilot transcript for that turn did not include those fields. This plugin can only export the fields Copilot recorded locally. |
| Cloud agent cannot reach Sigil | Expected unless your admin allows the Sigil endpoint through the cloud-agent firewall. This plugin is documented for Copilot CLI first. |
