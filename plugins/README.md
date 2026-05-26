# Sigil plugins for coding agents

Send conversations from your coding agent to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/) — model, tokens, tool calls, timing, and optionally the conversation content.

> AI Observability is in [public preview](https://grafana.com/docs/release-life-cycle/).

## Fastest start (Claude Code, Codex, Copilot, or pi)

```sh
brew install grafana/grafana/sigil
sigil claude     # for Claude Code
sigil codex      # for Codex
sigil copilot    # for Copilot CLI
sigil pi         # for pi
```

Use the `sigil <agent>` launcher for setup and daily use. On first run it installs the agent plugin or extension, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches the agent.

## All plugins

| Agent | Plugin | Status |
|-------|--------|--------|
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | [`claude-code/`](claude-code/) | Available |
| [Codex](https://developers.openai.com/codex) | [`codex/`](codex/) | Experimental |
| [Copilot CLI](https://docs.github.com/en/copilot/github-copilot-in-the-cli/using-github-copilot-in-the-cli) | [`copilot/`](copilot/) | Experimental |
| [Cursor](https://cursor.com) | [`cursor/`](cursor/) | Available |
| [OpenCode](https://opencode.ai) | [`opencode/`](opencode/) | Available |
| [Pi](https://github.com/badlogic/pi) | [`pi/`](pi/) | Available |

Plugins backed by the `sigil` launcher share one config file at `~/.config/sigil/config.env`. The launcher creates or updates it on first run; `sigil login` re-runs the same prompt later. Cursor has no launcher, so register its plugin in-app and run `sigil login` once for the shared config. OpenCode uses its own JSON config.
