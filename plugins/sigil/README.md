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

## Auto-update

`sigil claude`, `sigil codex`, `sigil copilot`, and `sigil opencode` refresh the installed host plugin automatically. Set `SIGIL_AUTO_UPDATE=false` to opt out.

## Troubleshooting

Hooks always exit 0, so problems only show up in the debug log. Set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env` and tail `~/.local/state/sigil/logs/sigil.log`.
