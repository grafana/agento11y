# agento11y

`agento11y` records coding-agent sessions in [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/) or stores them on your machine for the local viewer.

Refer to [Instrument coding agents](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/instrument-coding-agents/) in the Grafana Cloud documentation.

## Install

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
```

The script downloads the latest [release](https://github.com/grafana/agento11y/releases) for your OS and architecture, verifies its SHA-256 checksum, and installs the binary to `~/.local/bin`. Re-run it to upgrade. Set `INSTALL_DIR` to change the directory and `VERSION` to pin a release:

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | INSTALL_DIR=/usr/local/bin sh
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

Upgrade later with `brew upgrade grafana/grafana/agento11y`.

**Prebuilt binary (Windows):** download the `windows_amd64` or `windows_arm64` zip from the [releases page](https://github.com/grafana/agento11y/releases), extract `agento11y.exe`, and put it on your `PATH`.

**Go install (any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
```

This installs the binary to `go env GOPATH`/bin (or `GOBIN` if set); make sure that directory is on your `PATH`. Re-run the same command to upgrade.

Verify the install with `agento11y --version`.

## Launch a coding agent

Run `agento11y <agent>` with your coding agent's command name:

```sh
agento11y claude
```

Cursor has no launcher. Run `agento11y cursor install` once, then start Cursor normally. Remove its hooks with `agento11y cursor uninstall`.

## Configure

Run `agento11y login` to connect to Grafana Cloud. It prints your stack's coding-agent setup page and asks you to paste the connection block from that page.

If no Grafana Cloud connection is saved, `agento11y <agent>` and `agento11y cursor install` start the same flow. Local mode skips it. Run `agento11y login` again to change the connection, content capture, tags, or guard settings.

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

## Tagging sessions

Add `--tag key=value` (repeatable) before any `--` to attach tags to every generation the launched session produces. This is shorthand for setting `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment.

```sh
agento11y claude --tag project=hackathon --tag team=ai
# forward args to the underlying CLI after `--`
agento11y claude --tag project=hackathon -- --resume
```

The same flag works for every launcher (`claude`, `codex`, `copilot`, `opencode`, `pi`, `vibe`) and combines with `--local`.

### Automatic tags

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

## Content capture

The shared `agento11y` binary defaults to `metadata_only`: only model, tokens, tool names, timing, and cost ship to Grafana Agent Observability. Prompts, responses, and tool I/O stay on the local machine. To opt into sending content, set `AGENTO11Y_CONTENT_CAPTURE_MODE` in `~/.config/agento11y/config.env`. The shared binary parser accepts every mode the SDKs support:

```dotenv
# valid values: full | no_tool_content | metadata_only | full_with_metadata_spans
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Unknown values fall back to `metadata_only` with a warning. `default` is accepted as an alias for `metadata_only` so the shared binary matches the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`. The Pi (`@grafana/agento11y-pi`) and OpenCode (`@grafana/agento11y-opencode`) plugins ship their own parsers but accept the same set of values.

A plugin can only export fields the host agent passes through to it, so individual plugins may capture less than the SDK matrix shows. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the SDK-level behavior matrix and plugin defaults.

## Local mode

`agento11y <agent> --local` records the session to a JSONL store and starts the local viewer. The command prints the viewer URL. It first tries `http://127.0.0.1:8765` and uses a higher port when that port is unavailable.

`AGENTO11Y_LOCAL=true` in the shell or `config.env` enables local mode for every launch and installed hook. Use `--no-local` to override this setting for one launcher session.

Local mode stores full session content. Manage the viewer separately with `agento11y local start|status|stop|restart`.

## History import

The viewer starts empty: it holds only the sessions captured after you installed agento11y. `agento11y history import` backfills the ones an agent already wrote to disk.

```sh
# See what would be imported. Nothing is decoded, exported, or stored.
agento11y history import claude-code --dry-run

# Import into the local store on this machine.
agento11y history import claude-code --local

# Import into Grafana Cloud, the default without --local.
agento11y history import claude-code
```

Supported agents are `claude-code`, `codex`, `cursor`, and `pi`. `agento11y history import` with no agent lists them.

A `pi` import reads pi's session logs under `$PI_CODING_AGENT_DIR/sessions` (by default `~/.pi/agent/sessions`) and produces one generation per assistant turn, with its prompt, thinking, tool calls, matched tool results, model, token usage, cost, both timestamps, and parent turn. Three things live capture records are not in the session log, so an imported pi session is thinner than a captured one:

- No compaction or branch-summary generations. Live exports each summarization call as its own generation. pi's `compaction` entry carries the summary text, the token count it compacted away, and, on recent pi versions, the call's `usage` and cost, but never the model or the provider, so the call cannot be reconstructed as a generation. An imported session therefore holds fewer generations than a captured one, and the missing ones are the expensive calls.
- No system prompt, request controls (`max_tokens`, `temperature`, `top_p`, tool choice, thinking budget), or time to first token. pi records none of them.
- Tool definitions are name-only, and only for the tools a turn called: the descriptions, schemas, and the list of tools that were offered but unused live in pi's runtime. Session tags (`cwd`, `git.branch`) are absent for the same reason.

Subagent runs are in neither: the nested `run-N/session.jsonl` logs come from the third-party `pi-subagents` package, which live capture ignores too, so importing them would exceed live fidelity rather than match it.

A `cursor` import reads Cursor's session databases under `~/.cursor/chats` and produces one generation per prompt, with that prompt, the assistant's reply, its tool calls and matched results, the workspace, and the session's model. Cursor records less about a turn than the other three sources do, so an imported Cursor session is the thinnest of them:

- No token usage. Cursor keeps no per-turn counts, so every turn reports no usage and is marked approximate. A dashboard shows no cost for an imported Cursor turn.
- Approximate times. Cursor stamps no message with a time. Some sessions can be dated to the second, and in the rest the turns are spread across the session's span, which orders them and measures nothing. Every turn is marked approximate either way.
- A session the import cannot date is exported as ending where it started, rather than at a later time nothing in the session supports.
- One model name per session, and only when the session recorded one. A turn from a session that names none is marked as having no model.
- No system prompt and no turn IDs. Cursor's own system prompt is dropped, because a live capture exports none either. An imported turn is numbered by its position. The workspace and git-status block Cursor puts in front of a prompt stays there, because the model saw it.

Reading a Cursor session can add a `store.db-shm` file next to the session database. SQLite needs that file to read the newest part of a session. The import writes nothing else in `~/.cursor`, and a dry run is no different, because it opens the same databases.

Cursor publishes no schema for this format and stamps no version into it, so a Cursor release can change it.

A forked pi session imports only the turns the fork itself ran. The trunk holds the entries a fork copied from it and exports those turns under its own import, and, when the trunk exported the fork's parent turn, the fork's first turn carries `pi.fork.parent_session_id` and `pi.fork.parent_generation_id` metadata instead of a parent edge. A fork of a fork carries neither key, because no trunk generation exists to name.

`--local` picks the endpoint. Without it, the import exports to the configured Grafana Cloud endpoint, exactly as a live session does. With it, the import exports to the local daemon on this machine.

A dry run reads up to 1 MiB of each session file (a head and a tail window) to count turns and read session metadata. A `cursor` session is a SQLite database rather than a log file, so the dry run queries it for the same counts instead, and no message body leaves the database. Nothing from those bytes is decoded into prompt or response text, shown, exported, or stored.

Without `--since`, an import covers the last 90 days. The local store is a linear scan over JSONL files, so an unbounded first import makes the viewer slow before you have used it. Pass `--since 365d`, `--since 2026-01-01T00:00:00Z`, or any duration to widen the window, and `--until` to bound the other end.

The rest of the flags:

- `--source` restricts the import to one discovered path (repeatable). It filters the paths discovery already found; it cannot add a path outside the agent's roots.
- `--workspace` keeps only sessions whose workspace path contains the given text.
- `--max-sessions` and `--max-turns` cap how many sessions the run imports and how many turns it takes from each.
- `--all` skips the picker, `--yes` skips the confirmation.
- `--dry-run` prints the plan and imports nothing.
- `--force` re-exports turns the ledger already records.
- `--local` targets the local daemon instead of Grafana Cloud.

Without a terminal there is no picker and no confirmation, so the command prints the plan and imports nothing. Pass `--all --yes` to import from a script.

The viewer offers the same import in two places: a banner on the Sessions page, and Settings, then History, for the full form. An import there runs in the background and reports progress live; you can cancel it, and a cancelled run keeps what it already imported.

A local import never leaves this machine. Whether an import stays local follows from the endpoint, not from the flag. An import that exports to a loopback endpoint sets the daemon's forwarding marker on every request, and captures full content, matching live local capture. The daemon stores a marked backfill without relaying it to Grafana Cloud, whatever `AGENTO11Y_LOCAL_FORWARD` and `AGENTO11Y_CONTENT_CAPTURE_MODE` are set to. An import that reaches `127.0.0.1` is therefore marked even when it was started without `--local`. An import without `--local` and with a Cloud endpoint configured does leave the machine, which is the point of it.

Each imported turn is recorded in a per-agent ledger under `~/.local/state/agento11y/history/ledger/`. The ledger holds hashes, statuses, and counts, never paths or content. Re-running an import skips the turns the ledger already records, so an import is safe to repeat and a cancelled or failed run resumes where it stopped. `--force` re-exports those turns under the same generation IDs, so the export replaces the stored copy rather than adding a second one.

## Auto-update

`agento11y claude`, `agento11y codex`, and `agento11y opencode` refresh the installed host plugin automatically. Set `AGENTO11Y_AUTO_UPDATE=false` to opt out.

`AGENTO11Y_AUTO_UPDATE` does not apply to the other launchers. `agento11y copilot` rewrites its own `agento11y.json` hooks file, and `agento11y vibe` re-upserts its three entries into vibe's `hooks.toml`, so both always point at the installed binary. `agento11y pi` leaves upgrades to pi's own installer.

## Advanced configuration

### Config file

All integrations read `~/.config/agento11y/config.env`. If you only have the old `~/.config/sigil/config.env`, that file is read and updated instead.

The stack is saved as `AGENTO11Y_STACK_URL`. It is used only to build links; the ingest endpoint is a different host. Login never saves the stack as `AGENTO11Y_ENDPOINT`.

To configure the connection without the prompt, create the file:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Find these values at `https://<your-grafana>.grafana.net/a/grafana-agento11y-app/setup-coding-agent`.

`AGENTO11Y_EXPORT_TIMEOUT_MS` bounds each generation export request. It defaults to `30000` milliseconds and accepts base-10 integers from `1` through `2147483647`.

### Login flags

Scripts and devcontainers can pass the connection values as flags. Preferences have no flags; set them in the config file or answer the prompt.

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
