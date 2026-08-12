# Agent Observability plugins for coding agents

Send conversations from your coding agent to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/) — model, tokens, tool calls, timing, and optionally the conversation content.

Full docs: [Instrument coding agents](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/instrument-coding-agents/).

## Install

On macOS use Homebrew; on Linux and Windows (or any platform with Go 1.25+) use `go install`.

**macOS** — Homebrew:

```sh
brew install grafana/grafana/agento11y
```

Upgrade later with `brew upgrade grafana/grafana/agento11y`.

**Linux and Windows** — `go install` (also works on macOS):

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
```

This installs `agento11y` to `go env GOPATH`/bin (or `GOBIN`); make sure that directory is on your `PATH`. Re-run the same command to upgrade.

Verify the install with `agento11y --version`.

The command was renamed from `sigil`; the old name still works but will be removed in a future release.

## Launch your agent

Launch with `agento11y <agent>`, where `<agent>` is `claude`, `codex`, `copilot`, `opencode`, `pi`, or `vibe`. On first run it installs the agent plugin or extension, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, and then launches the agent.

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

## Content and redaction

Plugins send metadata only by default. `AGENTO11Y_CONTENT_CAPTURE_MODE=full` adds conversation content; see [Content Capture Modes](../docs/concepts/content-capture-modes.md).

When a plugin exports content, it redacts known secret formats first. That covers user prompts, system prompts, assistant text, thinking, conversation titles, error messages, tool arguments, and tool results, on the generation and on the tool-execution span. Set `AGENTO11Y_REDACT_INPUT_MESSAGES=false` to send prompts without redaction; everything else stays redacted. The strength differs per field, and prose fields are deliberately treated more gently than pasted content; [Content Capture Modes](../docs/concepts/content-capture-modes.md#strength-per-field) has the table.

## Configuration

Plugins backed by the `agento11y` launcher share one config file at `~/.config/agento11y/config.env`. If you only have the old `~/.config/sigil/config.env`, that file is read and updated instead. The launcher creates or updates it on first run; `agento11y login` re-runs the same prompt later. The prompt asks for your Grafana stack, prints that stack's coding-agent setup page, and tries to open it in a browser. The credentials are then filled from the environment block you paste back. The stack is saved, so a later run offers it back and you press Enter. The same values can be passed as `--endpoint`, `--tenant`, and `--token` (or `--token-stdin`), which always outrank the prompt and also let login run where there is no terminal to prompt on. Cursor has no launcher, so register its plugin in-app and run `agento11y login` once for the shared config.
