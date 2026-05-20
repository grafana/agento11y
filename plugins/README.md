# Sigil plugins for coding agents

Send conversations from your coding agent to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/) — model, tokens, tool calls, timing, and optionally the conversation content.

> AI Observability is in [public preview](https://grafana.com/docs/release-life-cycle/).

## Fastest start (Claude Code or pi)

```sh
brew install grafana/grafana/sigil
sigil claude     # for Claude Code
sigil pi         # for pi
```

The launcher installs the plugin on first run. Add your credentials to `~/.config/sigil/config.env` — see any per-agent README for the 3-page Grafana Cloud walkthrough.

## All plugins

| Agent | Plugin | Status |
|-------|--------|--------|
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | [`claude-code/`](claude-code/) | Available |
| [Codex](https://developers.openai.com/codex) | [`codex/`](codex/) | Experimental |
| [Copilot CLI](https://docs.github.com/en/copilot/github-copilot-in-the-cli/using-github-copilot-in-the-cli) | [`copilot/`](copilot/) | Experimental |
| [Cursor](https://cursor.com) | [`cursor/`](cursor/) | Available |
| [OpenCode](https://opencode.ai) | [`opencode/`](opencode/) | Available |
| [Pi](https://github.com/badlogic/pi) | [`pi/`](pi/) | Available |

Claude Code, Codex, Copilot, and Cursor share the same Go binary (`brew install grafana/grafana/sigil`) and the same config file (`~/.config/sigil/config.env`). All Sigil connection details live at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`.
