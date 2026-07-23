# Agent Observability for GitHub Copilot CLI

Forwards completed GitHub Copilot turns, hook-visible tool calls, error
metadata, subagent lifecycle metadata, and optional prompt/tool content to
[Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).
Powered by the shared `agento11y` binary and driven by a single hooks file at
`~/.copilot/hooks/agento11y.json`, which is read by **both** the GitHub Copilot CLI
and Copilot Chat in **VS Code**. Each exported turn is tagged with the host it
came from (`hook.surface` = `copilot-cli` or `vscode`).

> Experimental. GitHub Copilot CLI plugin support is still evolving, and the
> current documented hook payloads do not expose final assistant response text,
> reliable full token usage, or stable native turn IDs for completed turns. This
> plugin therefore exports one generation per completed turn using a local
> synthetic turn ID at the hook layer, then enriches the completed turn from
> Copilot CLI's local `events.jsonl` transcript when that artifact is present.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y copilot -- <copilot args>
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y copilot -- <copilot args>
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y copilot -- <copilot args>
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y copilot` writes the shared hooks file to `~/.copilot/hooks/agento11y.json`, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, removes any legacy `sigil-copilot` plugin left by older versions, and then launches Copilot CLI.

For VS Code, no launch wrapper is needed — once `~/.copilot/hooks/agento11y.json` exists, add `~/.copilot/hooks` to the `chat.hookFilesLocations` setting and Copilot Chat picks it up.

> The integration deliberately does **not** register a Copilot CLI plugin. The
> CLI runs hooks from the plugin store *and* `~/.copilot/hooks`, so a plugin
> alongside the shared file would fire every hook (and export every turn)
> twice. The single shared file covers both the CLI and VS Code; the hook
> dispatcher infers the host at runtime.

## 2. Credentials

When `agento11y copilot` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure Agent Observability is enabled on your stack — an administrator opens **Observability → Agent Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

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

To also send the conversation text (with automatic secret redaction), add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Start a Copilot CLI session in a repository and give it a prompt that triggers
at least one tool call. The plugin only exports completed turns at `agentStop`.
Then open **Agent Observability → Conversations** in Grafana Cloud and look for
generations with `agent_name=copilot`.

If nothing shows up:

```sh
AGENTO11Y_DEBUG=true agento11y copilot   # one turn
tail -f ~/.local/state/agento11y/logs/agento11y.log
```

## Supported Hook Events

The shared hooks file wires these GitHub Copilot hook triggers:

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
| `full_with_metadata_spans` | included on generation export with redaction; stripped on OTel spans | included on generation export with redaction; stripped on OTel spans | tool names/status only | stripped |

Captured prompt, assistant, and tool content is redacted before export. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the cross-SDK reference.

## Guards

Guards do two things when enabled: block tool calls that match a deny rule, and apply Transform rules to redact sensitive tool arguments. They're off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y copilot
```

By default, transport errors and timeouts let the tool call through. Set `AGENTO11Y_GUARDS_FAIL_OPEN=false` to block tool calls on errors instead. Raise or lower `AGENTO11Y_GUARDS_TIMEOUT_MS` (default `1500`) to trade latency against tolerance for slow evaluators.

### Transform guards (redaction)

When guards are enabled and a [Transform rule](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/guards/) matches a tool call, the redacted arguments replace what the tool receives.

Limits:

- Transforms apply in `copilot-cli`. Copilot Chat in VS Code expects a different hook response shape for argument rewrites that has not been verified end to end, so no rewrite is sent there and VS Code tool calls run with their original arguments. Deny rules work on both surfaces.
- Each guarded tool call adds one synchronous hook round-trip (`AGENTO11Y_GUARDS_TIMEOUT_MS`). Transform redaction fails open: when the transform cannot be applied, the original arguments pass through unchanged. Transport errors follow `AGENTO11Y_GUARDS_FAIL_OPEN`.
- When arguments are redacted, the exported tool record carries the redacted arguments the tool actually ran with.

## All options

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-sigil-app`. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTel password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | `metadata_only`, `no_tool_content`, `full`, or `full_with_metadata_spans`. |
| `AGENTO11Y_TAGS` | — | `key=value,key=value` tags on every generation and as `agento11y.tag.<key>` on OTel spans/metrics (e.g. `project=my-app`). |
| `AGENTO11Y_USER_ID` | — | Override the user id. |
| `AGENTO11Y_DEBUG` | `false` | Log to `~/.local/state/agento11y/logs/agento11y.log`. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Enable tool-call guards. When on, each Copilot `preToolUse` hook is evaluated against Agent Observability: tool calls denied by guard rules are blocked, and Transform rules redact tool arguments in `copilot-cli`. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | When the guard call fails (timeout, network, 5xx), proceed with the tool call. Set `false` for strict mode. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout. Lower = less added latency on every tool call, higher = better tolerance for slow `llm_judge` evaluators. |
| `AGENTO11Y_COPILOT_HOOK_SURFACE` | _(auto)_ | Override the detected host surface (`copilot-cli` or `vscode`). Normally inferred at runtime; set explicitly only when driving capture through a custom hooks config. |

If your OTLP **Instance ID** (on the OpenTelemetry card) differs from your Agent Observability Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

## What Gets Exported

When the documented Copilot hook payloads provide it, the plugin exports:

- session id as the Agent Observability conversation id
- a local synthetic turn id in metadata
- user prompt text
- assistant response text from the local Copilot transcript when available
- model, Copilot CLI version, reasoning effort, and request/message identifiers from the local Copilot transcript when available
- output token counts from the local Copilot transcript when available
- tool names and tool call order
- tool arguments and results when `AGENTO11Y_CONTENT_CAPTURE_MODE=full`
- tool status from `PostToolUse` and `PostToolUseFailure`
- error metadata from `ErrorOccurred`
- subagent lifecycle metadata from `SubagentStart` and `SubagentStop`
- timestamps and turn duration

Exported generations are always tagged with:

- `entrypoint=copilot`
- `hook.surface` — `copilot-cli` or `vscode`, inferred at runtime (see below)
- `cwd` when Copilot includes it
- `hook.source` when Copilot includes it

### Surface detection (`copilot-cli` vs `vscode`)

The Copilot hook payload carries no host identifier, and one shared
`~/.copilot/hooks/agento11y.json` serves both hosts, so the surface is resolved at
runtime:

1. An explicit `AGENTO11Y_COPILOT_HOOK_SURFACE` env var wins (the in-repo
   `plugins/copilot/hooks.json` sets `copilot-cli` for anyone driving capture
   through a plugin instead of the shared file).
2. Otherwise, if a `copilot` process is an ancestor of the hook, the surface is
   `copilot-cli` — this holds even when the CLI runs inside a VS Code
   integrated terminal.
3. Otherwise the surface is `vscode` (Copilot Chat's extension host spawns the
   hook, with no `copilot` ancestor).

The resolved surface is also written to the `copilot.hook_surface` metadata
field and the `AGENTO11Y_DEBUG` log line (`dispatch: event=… surface=…`).

## Limitations / Known Gaps

- The current documented Copilot hook payloads still do not carry final assistant response text, model, or usage directly. This plugin recovers those fields from the local Copilot CLI `events.jsonl` transcript on `agentStop`.
- Current observed Copilot CLI transcripts expose assistant text, model, request ids, message ids, native turn ids, reasoning effort, and output token counts. They do not appear to expose input token counts, cache token counts, or reasoning token counts for completed turns, so usage and cost can still be partial.
- Copilot does not document a stable native turn ID in these hook payloads. This plugin creates a local monotonic turn ID per session and hashes it into the Agent Observability generation id.
- Subagent hooks do not currently expose enough durable identity to synthesize separate child generations safely, so subagent activity is exported as parent-turn metadata only.
- This package targets the local Copilot CLI and Copilot Chat in VS Code via the shared `~/.copilot/hooks/agento11y.json`. Copilot cloud agent uses repository-level `.github/hooks/*.json` instead, and GitHub documents cloud-agent outbound network access as restricted by the firewall by default.

## Troubleshooting

| Symptom | Try |
|---|---|
| Hooks file missing at `~/.copilot/hooks/agento11y.json` | Re-run `agento11y copilot -- <args>` (it writes the file before launching). For VS Code, also add `~/.copilot/hooks` to `chat.hookFilesLocations`. |
| Turns appear twice in Agent Observability | A leftover `sigil-copilot` plugin is firing alongside the shared file. Remove it: `copilot plugin uninstall sigil-copilot` (newer `agento11y copilot` runs do this automatically). |
| Command not found | Reinstall `agento11y` (see step 1). Check `agento11y --version` and that its install dir is on `PATH`. |
| Hooks run but nothing appears in Agent Observability | Check `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TENANT_ID`, and `AGENTO11Y_AUTH_TOKEN`. Without all three, the plugin discards the completed fragment. |
| No latency/tool charts in Agent Observability | Set `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` so the plugin can emit traces and metrics. |
| Prompt or tool content is missing | Check `AGENTO11Y_CONTENT_CAPTURE_MODE`. The default is `metadata_only`. |
| Assistant response text is missing | Check that `agentStop` included a readable `transcriptPath` and that the local `events.jsonl` transcript still exists under `~/.copilot/session-state/<session-id>/`. |
| Model or output tokens are still missing | The local Copilot transcript for that turn did not include those fields. This plugin can only export the fields Copilot recorded locally. |
| Cloud agent cannot reach Agent Observability | Expected unless your admin allows the agento11y endpoint through the cloud-agent firewall. This plugin is documented for Copilot CLI first. |
