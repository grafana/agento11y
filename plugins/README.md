# Sigil plugins for coding agents

Plugins that send generations from coding agents to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/). They capture model, tool calls, traces, optionally full conversation content, and tokens/cost where the host agent exposes reliable usage data.

Claude Code, Codex, and Cursor share one Go binary (`sigil`) built from [`sigil/`](sigil/) and invoked as `sigil <host> hook`. Each host directory ships the plugin manifest, hook config, and a wrapper script that locates the shared binary.

| Agent | Plugin | Backed by | Status |
|-------|--------|-----------|--------|
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | [`claude-code/`](claude-code/) | [`sigil/`](sigil/) | Available |
| [Codex](https://developers.openai.com/codex) | [`codex/`](codex/) | [`sigil/`](sigil/) | Experimental |
| [Cursor](https://cursor.com) | [`cursor/`](cursor/) | [`sigil/`](sigil/) | Available |
| [OpenCode](https://opencode.ai) | [`opencode/`](opencode/) | TypeScript | Available |
| [Pi](https://github.com/badlogic/pi) | [`pi/`](pi/) | TypeScript | Available |

Each plugin's README covers install, config, auth, and content-capture options. The shared binary's own [README](sigil/README.md) documents environment variables that apply to all three Go-backed hosts.

Codex support is experimental because Codex hooks and plugin-provided lifecycle
config remain feature-flagged.
