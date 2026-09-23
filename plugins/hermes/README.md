# grafana-agento11y-hermes

[Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/) plugin for [Hermes Agent](https://github.com/NousResearch/hermes-agent).

## Install

The shared launcher does not support Hermes. `agento11y login`, its shared `config.env`, and its local mode do not configure this plugin.

Install into the Python environment that runs Hermes, not an unrelated system Python. Python 3.11 or newer is required. Hermes 0.16.0 is the supported floor; the plugin was tested against Hermes 0.19.0.

From the repository root, with Hermes's Python environment active:

```sh
python -m pip install ./plugins/hermes
```

The package is `grafana-agento11y-hermes`. Its `hermes_agent.plugins` entry point is `agento11y = grafana_agento11y_hermes`.

The privacy behavior documented below is unreleased. Published PyPI `0.10.0` defaults to full content and truncates payloads without shared secret redaction. Install from source to use metadata-only defaults and shared redaction.

Add `agento11y` to `plugins.enabled` in `~/.hermes/config.yaml`, preserving other entries:

```yaml
plugins:
  enabled:
    - agento11y
```

On Hermes 0.19.0, `hermes plugins enable` and `hermes plugins list` do not see pip-installed plugins. Enable through YAML and verify exported telemetry instead.

If upgrading from `hermes-plugin-sigil`, uninstall that distribution first to avoid registering two plugins. Replace the old `sigil` enabled key with `agento11y`. Retired `SIGIL_*` variables remain compatibility inputs where supported; use `AGENTO11Y_*` for new configuration.

For agent-assisted setup, use [llms.txt](llms.txt).

## Configure the two channels

Copy endpoints and credentials from your stack's Agent Observability setup page:

```text
https://<stack>.grafana.net/a/grafana-agento11y-app/setup
```

Create a token and copy the environment block. Do not guess endpoint regions or authorization headers. Both channels can use the setup token, but each has its own endpoint:

| Channel | Configuration | Data |
| --- | --- | --- |
| Generations | `AGENTO11Y_ENDPOINT`, `AGENTO11Y_PROTOCOL=http`, `AGENTO11Y_AUTH_MODE=basic`, `AGENTO11Y_AUTH_TENANT_ID`, `AGENTO11Y_AUTH_TOKEN` | One generation per LLM API call, including usage and timing |
| OpenTelemetry | `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` | Generation and tool-execution spans, plus metrics |

Tool executions have no separate generation-ingest record. Their arguments and results can also occur inside generation messages when content capture permits them.

The channels are independently optional. Generation export activates with a token or an explicit non-`none` auth mode. An endpoint alone does not activate it. OTel setup requires the base OTLP endpoint or its `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` alias. The standard endpoint wins. HTTP exporters append `/v1/traces` and `/v1/metrics`.

With neither channel configured, telemetry is disabled. To disable generations, remove generation credentials and set `AGENTO11Y_AUTH_MODE=none`. To disable plugin OTel setup, remove both base endpoint names and set `AGENTO11Y_HERMES_OTEL_AUTO=false`. Host-owned providers remain the host's responsibility.

Set variables in the environment that starts Hermes. Hermes loads `~/.hermes/.env` with override enabled, so values there beat shell exports. The plugin does not read the launcher's `~/.config/agento11y/config.env`.

## Privacy

The plugin uses Python SDK 0.17.x for content capture and secret redaction.

- `metadata_only` is the default. Message structure, tool names, model, usage, timing, IDs, and sampling parameters can leave the machine. Prompt text, responses, system prompts, tool schemas, tool I/O, and detailed error text do not.
- Set `AGENTO11Y_CONTENT_CAPTURE_MODE=full` only when you want content exported. `no_tool_content` still exports generation content. `full_with_metadata_spans` sends content only through generation ingest.
- `default`, an empty value, and unknown modes resolve to `metadata_only`; Hermes currently falls back without a warning.
- Shared secret redaction sanitizes exported content, including tool-execution spans. Structural truncation is not secret redaction. Pattern matching is not a guarantee that all secrets or personal data are removed.
- Prompt redaction defaults to on. `AGENTO11Y_REDACT_INPUT_MESSAGES=false` disables only user-prompt redaction; invalid values keep it on. Assistant text and errors use lightweight patterns. System prompts and tool payloads also use key/value patterns. Email addresses are redacted.
- No automatic `cwd` tag is emitted. Automatic user, repository, and branch tags are off by default.

Opt into automatic client tags with `AGENTO11Y_AUTO_CODING_AGENT_TAGS=true`. Narrow them with `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo`; accepted names are `user`, `repo`, `branch`, or `all`. The allowlist alone enables nothing. Explicit `AGENTO11Y_TAGS` values win over automatic values.

These tags reach generations, spans, and metric labels. User identities can be personal data, and branch names increase metric cardinality. Hermes caches its client for the process, so automatic values do not follow later directory or branch changes. Capture modes do not remove custom tags or metadata; never put secrets there.

See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) and [Tags and Metadata](../../docs/concepts/tags-and-metadata.md).

## Other settings

| Variable | Default | Purpose |
| --- | --- | --- |
| `AGENTO11Y_AGENT_NAME` | `hermes` | Agent identity |
| `OTEL_SERVICE_NAME` | `hermes` | OTel service identity |
| `AGENTO11Y_DEBUG` | `false` | Diagnostic logging |
| `AGENTO11Y_HERMES_SAMPLE_RATE` | `1.0` | Fraction of calls recorded, from `0.0` to `1.0`; not a privacy control |
| `AGENTO11Y_HERMES_MAX_CHARS` | `12000` | Per-string bound on tool payloads |
| `AGENTO11Y_HERMES_OTEL_AUTO` | `true` | Install missing OTel providers; never replace host-owned providers |
| `AGENTO11Y_HERMES_ERROR_FLUSH_TIMEOUT` | `2.0` | Maximum wait in seconds for the error-path flush |
| `AGENTO11Y_HEADERS` | unset | Extra generation-export headers |

## Verify and troubleshoot

With permission to send telemetry, start interactive Hermes with `AGENTO11Y_DEBUG=true`. Check `~/.hermes/logs/agent.log` for client initialization and installed OTel providers. A host-owned provider does not produce an installation message.

Run one turn and check generations in Agent Observability and traces/metrics in their respective data sources. One working channel does not prove the other works. `hermes -z` disables logging, so missing log lines in one-shot mode do not prove failure.

Hermes can clip request payloads before hooks run. The plugin reuses cached request facts where possible; `hermes.request_facts_reused` marks that reuse. A tool inventory may be stale after `tool_search`. Sampling parameters are not borrowed from a different model. `HERMES_PLUGIN_PAYLOAD_MAX_CHARS` can reduce envelope truncation, but cannot undo Hermes's per-string clipping.

Generations use synchronous mode because released hooks do not provide a reliable first-token signal. No time-to-first-token histogram is promised. Tool spans can end after their already-ended parent generation span.

## Development

From the repository root:

```sh
mise run format:py:plugin-hermes
mise run lint:py:plugin-hermes
mise run typecheck:py:plugin-hermes
mise run test:py:plugin-hermes
mise run build:py:plugin-hermes
```

The build task checks the wheel and source distribution. Root `mise run check` includes this artifact check. CI tests Python 3.11, 3.12, 3.13, and 3.14 with branch coverage enabled and a 99% minimum.

Real-Hermes end-to-end testing is optional, not part of the normal checks. Read [.agents/skills/e2e-test/SKILL.md](.agents/skills/e2e-test/SKILL.md) for a credential-free loopback recipe. A local telemetry sink alone does not make the model provider local.

## License

Copyright 2026 Grafana Labs. [Apache-2.0](../../LICENSE).
