# @grafana/agento11y-pi

[Pi](https://github.com/earendil-works/pi) agent extension that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/agento11y/scripts/install.sh | sh
agento11y pi
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y pi
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y pi
```

The script installs `agento11y` to `~/.local/bin`; `go install` uses `go env GOPATH`/bin (or `GOBIN`). Make sure that directory is on your `PATH`. See the [`agento11y` binary README](../agento11y/README.md#install) for all install options. The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y pi` installs the `@grafana/agento11y-pi` extension on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, and then launches pi.

<details>
<summary>Manual extension registration</summary>

```sh
pi install npm:@grafana/agento11y-pi
agento11y login
```

The extension reads the same `~/.config/agento11y/config.env` file whether you start pi with `agento11y pi` or plain `pi`. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

</details>

## 2. Credentials

When `agento11y pi` or `agento11y login` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

You need values from three Grafana Cloud pages:

1. **AI Observability → Configuration**
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
AGENTO11Y_ENDPOINT=https://sigil-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

</details>

When `AGENTO11Y_AUTH_TENANT_ID` and `AGENTO11Y_AUTH_TOKEN` are set, the extension uses them for Sigil and OTLP auth. If the OpenTelemetry card shows a different Instance ID, set `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(otlp-id:glc_token)>`.

To include conversation text (with automatic secret redaction), add this to your `config.env`:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run one pi turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env`, run another turn, and check the debug log at `~/.local/state/sigil/logs/sigil.log` (honors `XDG_STATE_HOME`).

## Tagging sessions

Launch with `--tag key=value` (repeatable) to attach tags to every generation pi exports:

```sh
agento11y pi --tag project=hackathon --tag team=ai
# forward args to pi after `--`
agento11y pi --tag team=ai -- --resume
```

`--tag` is shorthand for `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment or `~/.config/agento11y/config.env`. The merge happens in the SDK, so user tags reach every generation without the plugin reparsing them.

The plugin always attaches two built-in tags to every generation:

- `git.branch` — current branch from the working directory, or a 12-char short SHA on detached HEAD. Omitted when not inside a git checkout.
- `cwd` — the process working directory.

Built-in tags win collisions with user tags, matching the claude-code and cursor launchers.

## Redaction

Before any generation leaves the process, the SDK scrubs known token formats, PEM private keys, database URLs, `KEY=value` pairs, bearer tokens, and email addresses. Matches become `[REDACTED:<id>]`. User input messages are redacted by default; set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to leave them unchanged.

## Guards

Guards do two things when enabled: block tool calls that match a deny rule, and apply Transform rules to redact sensitive content. They're off by default:

```sh
AGENTO11Y_GUARDS_ENABLED=true agento11y pi
```

By default, transport errors and timeouts let the request through. Set `AGENTO11Y_GUARDS_FAIL_OPEN=false` to block tool calls on errors instead. Raise or lower `AGENTO11Y_GUARDS_TIMEOUT_MS` (default `1500`) to trade latency against tolerance for slow evaluators.

The same three variables are honored by the [Claude Code plugin](../claude-code/README.md); both plugins read them from `~/.config/agento11y/config.env`.

### Transform guards (redaction)

When guards are enabled, pi also applies [Transform guards](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/guides/guards/) — regex redaction rules you configure in Grafana — in two places:

- **Preflight (message redaction).** Before each model call, the outgoing conversation is sent to Sigil; redacted text replaces the original, so the placeholder (e.g. `[REDACTED]`) reaches the model instead of the secret.
- **Postflight (tool-argument redaction).** Before a tool runs, its arguments are sent to Sigil; if a Transform rule matches, the redacted arguments replace what the tool receives.

Limits:

- Each guarded model call adds one synchronous hook round-trip (`AGENTO11Y_GUARDS_TIMEOUT_MS`, default `1500`). Transform redaction always fails open: on a transport error or timeout the original messages or tool arguments pass through unchanged.
- A preflight deny verdict cannot stop the model call, only the transform output is applied. Enforced blocking happens at the tool-call (postflight) level.
- Redaction rewrites text content only. `thinking` blocks on assistant messages are left unchanged so multi-turn continuity is preserved.

## All options

`~/.config/agento11y/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENTO11Y_ENDPOINT` | — | Sigil URL (find it at `/plugins/grafana-sigil-app`) |
| `AGENTO11Y_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. Combined with `AGENTO11Y_AUTH_TOKEN` becomes Basic auth for Sigil and OTLP. |
| `AGENTO11Y_AUTH_TOKEN` | — | Cloud access policy token (`glc_…`). |
| `AGENTO11Y_AGENT_NAME` | `pi` | Agent name reported to Sigil. |
| `AGENTO11Y_AGENT_VERSION` | — | Optional version string reported with the agent. |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | `metadata_only` | One of `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans`. `default` is accepted as an alias for `metadata_only`. |
| `AGENTO11Y_DEBUG` | `false` | Write lifecycle events to `~/.local/state/sigil/logs/sigil.log` (honors `XDG_STATE_HOME`). Never written to the terminal, to avoid corrupting pi's TUI. |
| `AGENTO11Y_REDACT_INPUT_MESSAGES` | `true` | Redact known secret patterns in user input messages before export. |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP HTTP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `AGENTO11Y_OTEL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` | Override the OTLP password. |
| `AGENTO11Y_GUARDS_ENABLED` | `false` | Evaluate `tool_call` requests against Sigil policy. |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout for guard requests, in milliseconds. |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | `true` | Allow tools through when the guard call fails. Set `false` for strict mode. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only the following keys are honoured — anything else (including stray `PATH=…` lines) is ignored: any `AGENTO11Y_*` or `SIGIL_*` key plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME`.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.
