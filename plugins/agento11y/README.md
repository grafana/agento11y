# agento11y

The launcher binary behind the [Claude Code](../claude-code), [Codex](../codex), [Copilot](../copilot), [Cursor](../cursor), [OpenCode](../opencode), [pi](../pi), and [Vibe](../vibe) plugins for [Grafana Agent Observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/).

The command was renamed from `sigil`. Every install method also installs a `sigil` alias, which will be removed in a future release.

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

## Configure

All hosts read the same config file at `~/.config/agento11y/config.env`. If you only have the old `~/.config/sigil/config.env`, that file is read and updated instead. The first run of `agento11y claude`, `agento11y opencode`, or `agento11y pi` prompts for your endpoint, tenant ID, token, and OTLP endpoint and writes them there; run `agento11y login` to re-enter them later. After the connection details, `agento11y login` shows an optional preferences step for content capture mode, session tags, and guards — leave it at the defaults to keep the current behaviour. Cursor has no launcher, so wire it once with `agento11y cursor install` (which also prompts on first run) and remove it with `agento11y cursor uninstall`.

Before anything is written, login sends one request to the configured endpoint with the credentials you gave it. If the endpoint rejects them, login prints why and asks whether to save anyway.

The connection values can also come from flags, which is what a script or a devcontainer wants. The preferences have no flags; set them in the config file or answer the prompt.

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

Passing `--endpoint`, `--tenant`, and a token together skips the value prompts, so login works over SSH, in a devcontainer, and from a script. One prompt can still appear: if the credential check fails and stdin is a terminal, login asks whether to save anyway. Pass `--yes` (or `--no-verify`) so a script never stops there. Keeping a token out of your shell history:

```sh
printf %s "$TOKEN" | agento11y login --endpoint https://agento11y-prod-<region>.grafana.net --tenant <instance-id> --token-stdin
```

To preconfigure without the prompt, create the file:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Find these values in Grafana Cloud at `https://<your-grafana>.grafana.net/plugins/grafana-agento11y-app`.

Then follow your agent's quickstart:

- [Claude Code](../claude-code/README.md)
- [Codex](../codex/README.md)
- [Copilot](../copilot/README.md)
- [Cursor](../cursor/README.md)
- [OpenCode](../opencode/README.md)
- [pi](../pi/README.md)
- [Vibe](../vibe/README.md)

## Tagging sessions

Add `--tag key=value` (repeatable) before any `--` to attach tags to every generation the launched session produces. This is shorthand for setting `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment.

```sh
agento11y claude --tag project=hackathon --tag team=ai
# forward args to the underlying CLI after `--`
agento11y claude --tag project=hackathon -- --resume
```

The same flag works for every launcher (`claude`, `codex`, `copilot`, `opencode`, `pi`, `vibe`) and combines with `--local`.

## Content capture

The shared `agento11y` binary defaults to `metadata_only`: only model, tokens, tool names, timing, and cost ship to Grafana Agent Observability. Prompts, responses, and tool I/O stay on the local machine. To opt into sending content, set `AGENTO11Y_CONTENT_CAPTURE_MODE` in `~/.config/agento11y/config.env`. The shared binary parser accepts every mode the SDKs support:

```dotenv
# valid values: full | no_tool_content | metadata_only | full_with_metadata_spans
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Unknown values fall back to `metadata_only` with a warning. `default` is accepted as an alias for `metadata_only` so the shared binary matches the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`. The Pi (`@grafana/agento11y-pi`) and OpenCode (`@grafana/agento11y-opencode`) plugins ship their own parsers but accept the same set of values.

A plugin can only export fields the host agent passes through to it, so individual plugins may capture less than the SDK matrix shows. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the SDK-level behavior matrix and plugin defaults.

## Local mode and history import

`agento11y <agent> --local` records the session to a JSONL store on this machine and opens a viewer at `http://127.0.0.1:8765`. Start and stop the daemon by hand with `agento11y local start|status|stop`.

The viewer starts empty: it holds only the sessions captured after you installed agento11y. `agento11y history import` backfills the ones an agent already wrote to disk.

```sh
# See what would be imported. Nothing is decoded, exported, or stored.
agento11y history import claude-code --dry-run

# Import into the local store on this machine.
agento11y history import claude-code --local

# Import into Grafana Cloud, the default without --local.
agento11y history import claude-code
```

Supported agents are `claude-code` and `codex`. `agento11y history import` with no agent lists them.

`--local` picks the endpoint. Without it, the import exports to the configured Grafana Cloud endpoint, exactly as a live session does. With it, the import exports to the local daemon on this machine.

A dry run reads up to 1 MiB of each session file (a head and a tail window) to count turns and read session metadata. Nothing from those bytes is decoded into prompt or response text, shown, exported, or stored.

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

## Troubleshooting

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
