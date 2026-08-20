# Coding Agent Observability

Monitor the coding agents you already use — Cursor, Claude Code, Codex, Copilot CLI, OpenCode, Pi, Vibe, and others. Observe usage, cost, tokens, and tools across all of them in one place. Keep sessions on your machine with the local Agent Observability app, or send them to [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

<p align="center">
  <video src="https://github.com/user-attachments/assets/391a1e09-0f2e-4d96-9682-3c5e29079e55" controls muted autoplay loop playsinline width="100%"></video>
</p>

## Quick start

1. [Install](#install) `agento11y`.
2. [Configure](#configure) with `agento11y login` (use local Agent Observability app or Grafana Cloud Agent Observability).
3. [Launch a coding agent](#launch-a-coding-agent) with `agento11y <agent>` (for example `agento11y claude`).
4. If something looks wrong, run [`agento11y doctor`](#troubleshooting).

Or hand setup to a coding agent already in your terminal — see [Skills](#skills).

## Install

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
```

Installs to `~/.local/bin`. Put that directory on your `PATH` if it is not already.

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
```

Installs to `$(go env GOPATH)/bin` (or `GOBIN`). Put that directory on your `PATH`.

**Windows (prebuilt binary):** download the `windows_amd64` or `windows_arm64` zip from the [releases page](https://github.com/grafana/agento11y/releases), extract `agento11y.exe`, and put it on your `PATH`.

Verify with `agento11y --version`.

> **Note:** The command was renamed from `sigil`; the old name still works but will be removed.

## Configure

Run `agento11y login` to configure capture. On first run it asks where sessions go: **Local only**, or **Grafana Cloud**. That choice appears only on macOS and Linux when nothing is configured yet. Windows has no local receiver, so login goes straight to the Cloud credential questions. The Cloud path prints your stack's coding-agent setup page and asks you to paste the connection block from that page.

Run `agento11y login` again to change the Cloud connection, content capture, tags, or guard settings. A rerun goes straight to the Cloud questions, and asks where sessions go only when neither that answer nor credentials are saved.

For scripts and unattended rollout (register an agent without prompts), see [Noninteractive agent setup](#noninteractive-agent-setup) and [Fleet reconciliation](#fleet-reconciliation).

## Launch a coding agent

Run `agento11y <agent>` with your coding agent's command name:

```sh
agento11y claude
```

| Agent | How to run |
|-------|------------|
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | `agento11y claude` |
| [Codex](https://developers.openai.com/codex) | `agento11y codex` |
| [Copilot CLI](https://docs.github.com/en/copilot/github-copilot-in-the-cli/using-github-copilot-in-the-cli) | `agento11y copilot` |
| [Cursor](https://cursor.com) | `agento11y cursor install`, then start Cursor |
| [OpenCode](https://opencode.ai) | `agento11y opencode` |
| [Pi](https://github.com/earendil-works/pi) | `agento11y pi` |
| [Vibe](https://github.com/mistralai/vibe) | `agento11y vibe` |

Cursor has no launcher. Run `agento11y cursor install` once, then start Cursor normally. Remove its hooks with `agento11y cursor uninstall`. See also [`cursor/README.md`](../cursor/README.md). Per-agent notes and glue live under [`plugins/`](../).

## Skills

The binary carries agent skills: markdown workflows a coding agent reads and follows. They ship inside the binary, so there is nothing to fetch and no second CLI to install. Upgrading `agento11y` upgrades them.

```sh
agento11y skills list                          # name and one-line description
agento11y skills show setup-coding-agent       # the raw SKILL.md on stdout
```

`get` is accepted as an alias for `show`, matching `gcx agent skills get`.

`setup-coding-agent` walks a coding agent through the whole setup: reading `agento11y doctor --json`, installing the binary, saving credentials, wiring the host agent, verifying one session, and diagnosing a broken pipeline. To hand setup to the agent already open in your terminal, paste this:

```text
Run `agento11y skills show setup-coding-agent` and follow it to set up Grafana Agent observability for my coding agent.
```

`agento11y doctor` and `agento11y login` both name that command when they finish.

The skills for instrumenting your own application code are separate and ship with [`gcx`](https://github.com/grafana/gcx) instead: `gcx agent skills install agento11y-instrument`.

## Local mode

`agento11y <agent> --local` records the session to a JSONL store and starts the local Agent Observability app. The command prints the app URL (tries `http://127.0.0.1:8765`, then a higher port if needed).

`AGENTO11Y_LOCAL=true` in the shell or `config.env` enables local mode for every launch and installed hook. Choosing **Local only** at first-run login writes that setting (and `SIGIL_LOCAL=true`) and later runs do not ask again. Local mode is available on macOS and Linux only; Windows has no local receiver. Use `--no-local` to override local mode for one launcher session.

Local mode stores full session content. Manage the app with `agento11y local start|status|stop|restart`.

### History import

The local Agent Observability app starts empty: it only has sessions captured after you installed agento11y. To backfill earlier sessions, prefer the app — a banner on the Sessions page, or Settings → History. Imports run in the background with live progress; you can cancel them, and a cancelled run keeps what it already imported.

You can also use the CLI. Supported agents are `claude-code`, `codex`, `cursor`, and `pi` (`agento11y history import` with no agent lists them):

```sh
# See what would be imported. Nothing is decoded, exported, or stored.
agento11y history import claude-code --dry-run

# Import into the local store on this machine.
agento11y history import claude-code --local

# Import into Grafana Cloud, the default without --local.
agento11y history import claude-code
```

Imported sessions are thinner than live capture — host logs omit fields that live hooks see:

- **pi** — reads `$PI_CODING_AGENT_DIR/sessions` (default `~/.pi/agent/sessions`). One generation per assistant turn. Missing vs live: compaction/branch-summary generations, system prompt and request controls, full tool schemas, and `git.branch`. Forks import only the fork's own turns; subagent logs are skipped.
- **cursor** — reads agent-transcript JSONL under `~/.cursor/projects/…/agent-transcripts/` and the older `store.db` under `~/.cursor/chats/…`. Missing vs live: token usage/cost (turns are marked approximate), reliable timestamps on `store.db` sessions, models on transcripts, and tool results on transcripts. Cursor formats can change without notice.

Without `--since`, an import covers the last 90 days. Pass `--since 365d` (or a timestamp) to widen the window, and `--until` to bound the other end. Other useful flags: `--workspace`, `--max-sessions`, `--max-turns`, `--all`, `--yes`, `--force`. Without a terminal, pass `--all --yes` to import from a script.

Re-running an import is safe: a per-agent ledger under `~/.local/state/agento11y/history/ledger/` skips turns already recorded. `--force` re-exports those turns under the same generation IDs.

## Grafana Agent Observability

Send sessions to Grafana Cloud Agent Observability. Choose **Grafana Cloud** during [`agento11y login`](#configure), or write Cloud credentials into `config.env` (see below).

### Credentials

To configure the connection without the prompt, set these in `~/.config/agento11y/config.env`:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Find these values at `https://<your-grafana>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`.

### Content capture

The shared `agento11y` binary defaults to `metadata_only`: only model, tokens, tool names, timing, and cost ship to Grafana Agent Observability. Prompts, responses, and tool I/O stay on the local machine. To opt into sending content, set `AGENTO11Y_CONTENT_CAPTURE_MODE` in `~/.config/agento11y/config.env`. The shared binary parser accepts every mode the SDKs support:

```dotenv
# valid values: full | no_tool_content | metadata_only | full_with_metadata_spans
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Unknown values fall back to `metadata_only` with a warning. `default` is accepted as an alias for `metadata_only` so the shared binary matches the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`. The Pi (`@grafana/agento11y-pi`) and OpenCode (`@grafana/agento11y-opencode`) plugins ship their own parsers but accept the same set of values.

A plugin can only export fields the host agent passes through to it, so individual plugins may capture less than the SDK matrix shows. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the SDK-level behavior matrix and plugin defaults.

### Login flags

Scripts and devcontainers can pass the Cloud connection values as flags. Preferences have no flags; set them in the config file or answer the prompt.

```sh
agento11y login --endpoint https://agento11y-prod-<region>.grafana.net --tenant <instance-id> --token glc_...
```

| Flag | Meaning |
|------|---------|
| `--endpoint url` | conversations API URL |
| `--tenant id` | instance ID |
| `--token value` | access-policy token with the `sigil:write` scope |
| `--token-stdin` | read the token from stdin; requires `--endpoint` and `--tenant` |
| `--otlp-endpoint url` | OTLP endpoint for SDK traces and metrics |
| `--no-verify` | write the file without checking the credentials |
| `--yes` | save even when the check fails |

Passing `--endpoint`, `--tenant`, and a token together skips the value prompts. Login verifies the credentials before it writes the file. If verification fails and stdin is a terminal, login asks whether to save anyway. Pass `--yes` or `--no-verify` so a script never stops there.

Keep the token out of your shell history with `--token-stdin`:

```sh
printf %s "$TOKEN" | agento11y login --endpoint https://agento11y-prod-<region>.grafana.net --tenant <instance-id> --token-stdin
```

## Settings

Shared options that apply whether you use local mode or Grafana Cloud.

### Tagging sessions

Add `--tag key=value` (repeatable) before any `--` to attach tags to every generation the launched session produces. This is shorthand for setting `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment.

```sh
agento11y claude --tag project=hackathon --tag team=ai
# forward args to the underlying CLI after `--`
agento11y claude --tag project=hackathon -- --resume
```

The same flag works for every launcher (`claude`, `codex`, `copilot`, `opencode`, `pi`, `vibe`) and combines with `--local`.

#### Automatic tags

`AGENTO11Y_AUTO_CODING_AGENT_TAGS` resolves the session's user, repository, and branch and attaches them as client tags, which are the tags that also become OTel metric labels. Use it to break usage and cost down by person, repository, or branch. The switch is off by default, and on its own it enables every name:

```dotenv
# ~/.config/agento11y/config.env
AGENTO11Y_AUTO_CODING_AGENT_TAGS=true
# Optional: narrow it to some of the names. Defaults to all of them.
AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo
```

| Name | Tag key | Resolved from |
| --- | --- | --- |
| `user` | `user` | `AGENTO11Y_USER_ID`, then the identity the host agent knows, then the OS account name |
| `repo` | `repo` | `owner/name` of the checkout's `origin` remote, or the checkout directory name |
| `branch` | `git.branch` | Branch checked out in the session's directory |

A key already set in `AGENTO11Y_TAGS` wins, an unresolved value leaves its key off, and an unsupported name is logged and skipped. The allowlist does nothing while the switch is off. Because these are client tags, they arrive in Prometheus as `agento11y_tag_user`, `agento11y_tag_repo`, and `agento11y_tag_git_branch`.

`agento11y login` asks the same question. Answer Yes to "Automatic tags" and it opens a checklist of the three values, ticked to match the saved allowlist. A first run ticks all three. Login then writes the switch and the allowlist for you. Answering No sets the switch to false and deletes the allowlist.

The setting works whether the session starts through `agento11y <agent>` or through the host agent directly: the installed hooks read it when they build their client.

Before enabling it, read the cardinality and personal-data notes in [Tags and Metadata](../../docs/concepts/tags-and-metadata.md#cardinality-and-personal-data). The `user` value is commonly an email address, and it is kept for the metric retention period. Run `agento11y doctor` to see the exact values first.

### Config file

All integrations read `~/.config/agento11y/config.env`. If you only have the old `~/.config/sigil/config.env`, that file is read and updated instead.

The stack is saved as `AGENTO11Y_STACK_URL`. It is used only to build links; the ingest endpoint is a different host. Login never saves the stack as `AGENTO11Y_ENDPOINT`.

Cloud credentials in this file are documented under [Grafana Agent Observability](#grafana-agent-observability).

`AGENTO11Y_EXPORT_TIMEOUT_MS` bounds each generation export request. It defaults to `30000` milliseconds and accepts base-10 integers from `1` through `2147483647`.

### Noninteractive agent setup

After writing the current user's `config.env`, a script can register an agent integration without launching the host or opening the credential prompt:

```sh
agento11y copilot install --json
agento11y opencode install --json
agento11y pi install --json
```

Each command prints one secret-free result with `installed`, `already_installed`, `missing_host`, or `error`. Copilot writes its shared user hook file and does not require the Copilot CLI on `PATH`; that file is also read by Copilot Chat in VS Code. OpenCode and pi require their respective CLI to be on the current user's `PATH`; `missing_host` is a successful deferral, so a later run can configure a host installed after the script.

Claude Code provides the same command as `agento11y claude install --json`.

#### Fleet reconciliation

For MDM, configuration management, and other unattended rollout tools, reconcile those installers in one shot and receive one JSON result per agent:

```sh
agento11y agents reconcile --agents all --json
```

`all` includes every noninteractive installer registered in the installed binary, including an agent added by a future release. To target a fixed allowlist, pass names instead: `--agents claude,cursor`. The command never launches a coding agent or opens a login prompt. Its receipt reports `installed`, `already_installed`, `missing_host`, or a per-agent descriptive error; it exits non-zero only when an installer fails. This command contains no MDM-vendor, credential, or device-policy assumptions.

### Auto-update

`agento11y claude`, `agento11y codex`, and `agento11y opencode` refresh the installed host plugin automatically. Set `AGENTO11Y_AUTO_UPDATE=false` to opt out.

`AGENTO11Y_AUTO_UPDATE` does not apply to the other launchers. `agento11y copilot` rewrites its own `agento11y.json` hooks file, and `agento11y vibe` re-upserts its three entries into vibe's `hooks.toml`, so both always point at the installed binary. `agento11y pi` leaves upgrades to pi's own installer.

## Troubleshooting

`agento11y help` (or `--help`, or `-h`) prints the full command list on stdout and exits 0. An unknown subcommand, or a command given the wrong number of arguments, prints a one-line usage form on stderr and exits 2.

Run `agento11y doctor` first. It's a read-only diagnostic that reports both export pipelines, config, and installed host-agent plugins in one place. It sends a lightweight request to each endpoint and reports the HTTP status, so a wrong endpoint or a token missing a scope shows up as a broken pipeline:

```sh
agento11y doctor
```

The two pipelines are independent and fail independently:

- **Conversations** (the chat transcripts) export over `AGENTO11Y_ENDPOINT` + `AGENTO11Y_AUTH_TENANT_ID` + `AGENTO11Y_AUTH_TOKEN`. The token needs the `sigil:write` scope.
- **Analytics** (the Agent Observability metrics and traces) export over `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_ENDPOINT`). The token needs `metrics:write` and `traces:write`.

The common failure is conversations showing up while the analytics page stays empty: the OTLP endpoint is unset, or the token lacks the metrics/traces scopes. `agento11y doctor` flags that case explicitly and exits non-zero when a pipeline is broken.

A 403 means the token is missing a write scope. A 401 means the endpoint refused the credentials without saying why: the token may be invalid or expired, or `AGENTO11Y_AUTH_TENANT_ID` may be wrong.

For support, capture the machine-readable report — it never includes the auth token value:

```sh
agento11y doctor --json
```

If you need lower-level detail, hooks always exit 0, so problems only show up in the debug log. Set `AGENTO11Y_DEBUG=true` in `~/.config/agento11y/config.env` and tail `~/.local/state/agento11y/logs/agento11y.log`. Installs that still have the pre-rename `~/.local/state/sigil` directory keep using it (with the new `agento11y.log` file name) until it is removed.

## Further reading

This page is the setup guide for the `agento11y` CLI. Product docs: [Instrument coding agents](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/guides/instrument-coding-agents/).
