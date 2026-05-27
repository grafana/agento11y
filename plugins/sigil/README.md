# sigil

The launcher binary behind the [Claude Code](../claude-code), [Codex](../codex), [Copilot](../copilot), [Cursor](../cursor), [OpenCode](../opencode), and [pi](../pi) plugins for [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

## Install

```sh
brew install grafana/grafana/sigil
```

## Configure

All hosts read the same config file at `~/.config/sigil/config.env`. The first run of `sigil claude`, `sigil opencode`, or `sigil pi` prompts for your endpoint, tenant ID, token, and OTLP endpoint and writes them there; run `sigil login` to re-enter them later.

To preconfigure without the prompt, create the file:

```dotenv
SIGIL_ENDPOINT=https://sigil-prod-<region>.grafana.net
SIGIL_AUTH_TENANT_ID=<instance-id>
SIGIL_AUTH_TOKEN=glc_...
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Find these values in Grafana Cloud at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`.

Then follow your agent's quickstart:

- [Claude Code](../claude-code/README.md)
- [Codex](../codex/README.md)
- [Copilot](../copilot/README.md)
- [Cursor](../cursor/README.md)
- [OpenCode](../opencode/README.md)
- [pi](../pi/README.md)

## Content capture

The shared `sigil` binary defaults to `metadata_only`: only model, tokens, tool names, timing, and cost ship to Grafana AI Observability. Prompts, responses, and tool I/O stay on the local machine. To opt into sending content, set `SIGIL_CONTENT_CAPTURE_MODE` in `~/.config/sigil/config.env`. The shared binary parser accepts every mode the SDKs support:

```dotenv
# valid values: full | no_tool_content | metadata_only | full_with_metadata_spans
SIGIL_CONTENT_CAPTURE_MODE=full
```

Unknown values fall back to `metadata_only` with a warning. `default` is accepted as an alias for `metadata_only` so the shared binary matches the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`. The Pi (`@grafana/sigil-pi`) and OpenCode (`@grafana/sigil-opencode`) plugins ship their own parsers but accept the same set of values.

A plugin can only export fields the host agent passes through to it, so individual plugins may capture less than the SDK matrix shows. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the SDK-level behavior matrix and plugin defaults.

## Auto-update

`sigil claude`, `sigil codex`, `sigil copilot`, and `sigil opencode` refresh the installed host plugin automatically. Set `SIGIL_AUTO_UPDATE=false` to opt out.

## Troubleshooting

Hooks always exit 0, so problems only show up in the debug log. Set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env` and tail `~/.local/state/sigil/logs/sigil.log`.
