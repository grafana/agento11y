# sigil

The hook binary behind the [Claude Code](../claude-code), [Codex](../codex), [Copilot](../copilot), and [Cursor](../cursor) plugins for [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

## Install

```sh
brew install grafana/grafana/sigil
```

## Configure

All four hosts read the same config file at `~/.config/sigil/config.env`. The first run of `sigil claude` or `sigil pi` prompts for your endpoint, tenant ID, token, and OTLP endpoint and writes them there; run `sigil login` to re-enter them later.

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
- [pi](../pi/README.md) (separate JS plugin)

## Troubleshooting

Hooks always exit 0, so problems only show up in the debug log. Set `SIGIL_DEBUG=true` in `~/.config/sigil/config.env` and tail `~/.local/state/sigil/logs/sigil.log`.
