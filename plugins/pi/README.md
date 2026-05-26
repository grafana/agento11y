# @grafana/sigil-pi

[Pi](https://github.com/badlogic/pi) agent extension that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (token counts, cost, model, tool names, durations). Set `SIGIL_CONTENT_CAPTURE_MODE=full` or `no_tool_content` to include message content.

## 1. Install and launch

```sh
brew install grafana/grafana/sigil
sigil pi
```

`sigil pi` installs the `@grafana/sigil-pi` extension on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches pi.

<details>
<summary>Manual extension registration</summary>

```sh
pi install npm:@grafana/sigil-pi
sigil login
```

The extension reads the same `~/.config/sigil/config.env` file whether you start pi with `sigil pi` or plain `pi`.

</details>

## 2. Credentials

When `sigil pi` or `sigil login` prompts, copy values from `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`. Make sure AI Observability is enabled on your stack — an administrator opens **Observability → AI Observability** once and accepts the terms.

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

When `SIGIL_AUTH_TENANT_ID` and `SIGIL_AUTH_TOKEN` are both set, both Sigil generation export and the OTLP endpoint authenticate with the synthesized Basic auth header (`Basic base64(tenant:token)`). With only one of the two set, no auth header is sent.

To include conversation text (with automatic secret redaction), add this to `~/.config/sigil/config.env`:

```dotenv
SIGIL_CONTENT_CAPTURE_MODE=full
```

## 3. Verify

Run one pi turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds.

If nothing shows up, set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env`, run another turn, and check pi stderr plus any Sigil logs.

## Redaction

Before any generation leaves the process, the SDK scrubs known token formats, PEM private keys, database URLs, `KEY=value` pairs, bearer tokens, and email addresses. Matches become `[REDACTED:<id>]`. User input messages are redacted by default; set `SIGIL_REDACT_INPUT_MESSAGES=false` to leave them unchanged.

## Guards

Guards block tool calls before they execute (e.g. refuse a `bash` invocation matching a deny rule). They're off by default:

```sh
SIGIL_GUARDS_ENABLED=true sigil pi
```

By default, transport errors and timeouts let the tool through. Set `SIGIL_GUARDS_FAIL_OPEN=false` to block on errors instead. Raise or lower `SIGIL_GUARDS_TIMEOUT_MS` (default `1500`) to trade latency against tolerance for slow evaluators.

The same three variables are honored by the [Claude Code plugin](../claude-code/README.md); both plugins read them from `~/.config/sigil/config.env`.

## All options

`~/.config/sigil/config.env` is the only configuration file. Every option is set via env var.

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGIL_ENDPOINT` | — | Sigil URL (find it at `/plugins/grafana-sigil-app`) |
| `SIGIL_AUTH_TENANT_ID` | — | Grafana Cloud instance ID. Combined with `SIGIL_AUTH_TOKEN` becomes Basic auth for Sigil and OTLP. |
| `SIGIL_AUTH_TOKEN` | — | Cloud access policy token (`glc_…`). |
| `SIGIL_AGENT_NAME` | `pi` | Agent name reported to Sigil. |
| `SIGIL_AGENT_VERSION` | — | Optional version string reported with the agent. |
| `SIGIL_CONTENT_CAPTURE_MODE` | `metadata_only` | `full`, `no_tool_content`, or `metadata_only`. |
| `SIGIL_DEBUG` | `false` | Log lifecycle events to stderr. |
| `SIGIL_REDACT_INPUT_MESSAGES` | `true` | Redact known secret patterns in user input messages before export. |
| `SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP HTTP endpoint. Falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `SIGIL_OTEL_AUTH_TOKEN` | — | OTel-only auth token. Overrides `SIGIL_AUTH_TOKEN` when synthesising OTLP Basic auth. |
| `SIGIL_GUARDS_ENABLED` | `false` | Evaluate `tool_call` requests against Sigil policy. |
| `SIGIL_GUARDS_TIMEOUT_MS` | `1500` | Per-call timeout for guard requests, in milliseconds. |
| `SIGIL_GUARDS_FAIL_OPEN` | `true` | Allow tools through when the guard call fails. Set `false` for strict mode. |

File format: one `KEY=value` per line, `#` line comments, optional `export ` prefix, optional matching single or double quotes around the value. Only the following keys are honoured — anything else (including stray `PATH=…` lines) is ignored: any `SIGIL_*` key plus `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_SERVICE_NAME`.

A non-empty OS env value always wins over the file; an empty or whitespace-only OS value is treated as unset and gets filled from `config.env`. Missing files are silent.
