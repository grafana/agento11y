# Sigil plugins for coding agents

Send conversations from your coding agent to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/) — model, tokens, tool calls, timing, and optionally the conversation content.

> AI Observability is in [public preview](https://grafana.com/docs/release-life-cycle/).

## Install

On macOS use Homebrew; on Linux and Windows (or any platform with Go 1.25+) use `go install`.

**macOS** — Homebrew:

```sh
brew install grafana/grafana/agento11y
```

Upgrade later with `brew upgrade grafana/grafana/agento11y`.

**Linux and Windows** — `go install` (also works on macOS):

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/agento11y@latest
```

This installs `agento11y` to `go env GOPATH`/bin (or `GOBIN`); make sure that directory is on your `PATH`. Re-run the same command to upgrade.

Verify the install with `agento11y --version`.

The command was renamed from `sigil`; the old name still works but will be removed in a future release.

## Launch your agent

Launch with `agento11y <agent>`, where `<agent>` is `claude`, `codex`, `copilot`, `opencode`, `pi`, or `vibe`. On first run it installs the agent plugin or extension, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then launches the agent.

Cursor has no launcher; see [`cursor/README.md`](cursor/README.md) for setup.

## All plugins

| Agent | Plugin | Status |
|-------|--------|--------|
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | [`claude-code/`](claude-code/) | Available |
| [Codex](https://developers.openai.com/codex) | [`codex/`](codex/) | Experimental |
| [Copilot CLI](https://docs.github.com/en/copilot/github-copilot-in-the-cli/using-github-copilot-in-the-cli) | [`copilot/`](copilot/) | Experimental |
| [Cursor](https://cursor.com) | [`cursor/`](cursor/) | Available |
| [OpenCode](https://opencode.ai) | [`opencode/`](opencode/) | Available |
| [Pi](https://github.com/earendil-works/pi) | [`pi/`](pi/) | Available |
| [Vibe](https://github.com/mistralai/vibe) | [`vibe/`](vibe/) | Experimental |

Plugins backed by the `agento11y` launcher share one config file at `~/.config/sigil/config.env`. The launcher creates or updates it on first run; `agento11y login` re-runs the same prompt later. Cursor has no launcher, so register its plugin in-app and run `agento11y login` once for the shared config.
