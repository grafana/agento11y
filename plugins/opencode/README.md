# @grafana/agento11y-opencode

[OpenCode](https://opencode.ai) plugin that sends LLM generations to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
agento11y opencode
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y opencode
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y opencode
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y opencode` installs `@grafana/agento11y-opencode` into OpenCode on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, and then launches OpenCode. Pass arguments to OpenCode after `--`, e.g. `agento11y opencode -- run "say hi"`.

<details>
<summary>Manual plugin registration</summary>

```sh
opencode plugin @grafana/agento11y-opencode --global
agento11y login
```

The plugin reads `~/.config/agento11y/config.env` on every session start, whether you start OpenCode with `agento11y opencode` or plain `opencode`. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

</details>

## 2. Credentials

When `agento11y opencode` or `agento11y login` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure Agent Observability is enabled on your stack — an administrator opens **Observability → Agent Observability** once and accepts the terms.

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

When `AGENTO11Y_AUTH_TENANT_ID` and `AGENTO11Y_AUTH_TOKEN` are set, the plugin uses them for Agent Observability and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To include conversation text, add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

OpenCode redacts assistant and tool content before export. User prompt text is sent as-is when content capture allows it.

## 3. Verify

Run one OpenCode turn, then open **Agent Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env`, run another turn, and check OpenCode stderr.

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation the plugin exports:

```sh
agento11y opencode --tag project=hackathon --tag team=ai
# forward args to opencode after `--`
agento11y opencode --tag team=ai -- run "say hi"
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment or `~/.config/agento11y/config.env`. The merge happens in the SDK, so user tags reach every generation without the plugin reparsing them.

The plugin always attaches two built-in tags to every generation:

- `git.branch` — current branch from the opencode project directory, or a 12-char short SHA on detached HEAD. Omitted when not inside a git checkout.
- `cwd` — the opencode project directory (from `PluginInput.directory`).

Built-in tags win collisions with user tags, matching the claude-code and cursor launchers.

## All options

`~/.config/agento11y/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|---|---|---|
| `AGENTO11Y_ENDPOINT` | — | Agent Observability API URL. Find it at `/plugins/grafana-sigil-app`. Empty value disables the plugin. |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. |
| `AGENTO11Y_AUTH_TOKEN` | — | `glc_…` Cloud Access Policy Token. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP endpoint. Without it, the Agent Observability latency and tool-call panels stay empty. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Evaluate OpenCode tool calls against Agent Observability guards before execution. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-evaluation guard timeout in milliseconds. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | Allow tool calls if guard evaluation fails. Set to `false` to fail closed. |
| `AGENTO11Y_AGENT_NAME` | `opencode` | Agent name reported to Agent Observability. The plugin appends `:<mode>` for OpenCode's UI mode, such as `build` or `plan`. |
| `AGENTO11Y_AGENT_VERSION` | OpenCode version | Version string reported with the agent. |
| `AGENTO11Y_DEBUG` | `false` | Log lifecycle events to stderr. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only `AGENTO11Y_*` and `SIGIL_*` keys plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME` are honored — anything else (including stray `PATH=…` lines) is ignored.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.

## Development

```bash
pnpm install
pnpm --filter @grafana/agento11y-opencode build
pnpm --filter @grafana/agento11y-opencode test
```

The `@grafana/agento11y` dependency resolves via pnpm workspace linking to `js/`.
