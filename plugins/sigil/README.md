# sigil — single binary for the Claude Code, Codex, and Cursor agent plugins

`sigil` is one Go binary that backs all three agent plugins (`plugins/claude-code`, `plugins/codex`, `plugins/cursor`) in this repository. The dispatcher accepts:

```
sigil <agent> hook        # process a JSON hook payload on stdin for <agent>
sigil --version           # print the build version
```

where `<agent>` is `claude-code`, `codex`, or `cursor`.

## Install

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
```

The binary lands in `$GOBIN`, or `$(go env GOPATH)/bin` if `GOBIN` is unset — typically `$HOME/go/bin/sigil`. Make sure that directory is on `PATH`; the Claude Code and Codex hook manifests invoke `sigil <agent> hook` directly. The Cursor wrapper at `plugins/cursor/scripts/run.sh` probes common install paths because Cursor's GUI launch strips `PATH` — set `SIGIL_BIN` there to override.

## Configuration

The binary reads its configuration from environment variables. When the agent launches hooks under a stripped environment (notably Cursor and Codex headless mode), the binary loads the dotenv file at `$XDG_CONFIG_HOME/sigil/config.env` and applies any keys that are unset in the OS env.

### Required for generation export

```
SIGIL_ENDPOINT          # e.g. https://sigil.example.com
SIGIL_AUTH_TENANT_ID    # tenant identifier
SIGIL_AUTH_TOKEN        # bearer/basic password for export
```

### Optional

```
SIGIL_CONTENT_CAPTURE_MODE   # default metadata_only; set to "full" to include content (with redaction)
SIGIL_TAGS                   # k=v,k=v applied to every generation
SIGIL_USER_ID                # override auto-resolved user id
SIGIL_USER_ID_SOURCE         # claude-code only: "email" (default) or "accountUuid"
SIGIL_DEBUG                  # true|1|yes — write to $XDG_STATE_HOME/sigil/logs/sigil.log
```

### OTel trace + metric export

The binary configures OTLP exporters for the SDK's traces and metrics. Set one of:

```
SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT   # sigil-specific override
OTEL_EXPORTER_OTLP_ENDPOINT         # standard OTel env var (fallback)
```

When `OTEL_EXPORTER_OTLP_HEADERS` does not contain an `Authorization` entry, the binary synthesises `Authorization=Basic base64(SIGIL_AUTH_TENANT_ID:SIGIL_AUTH_TOKEN)` so users do not have to hand-encode it. `SIGIL_OTEL_AUTH_TOKEN` overrides the credential token for OTel only.

## How agents launch the binary

| Agent | Invocation |
|-------|------------|
| `claude-code` | `sigil claude-code hook` (resolved via `PATH`) |
| `codex` | `sigil codex hook` (resolved via `PATH`) |
| `cursor` | `plugins/cursor/scripts/run.sh` |

Claude Code and Codex run hooks under the user's shell environment, so the binary just has to be on `PATH`. Cursor launches hooks under a stripped environment (macOS GUI launches inherit launchd's `/usr/bin:/bin:/usr/sbin:/sbin`), which doesn't include `~/go/bin` or `/opt/homebrew/bin` — the wrapper probes `$SIGIL_BIN`, `~/go/bin/sigil`, `/opt/homebrew/bin/sigil`, `/usr/local/bin/sigil`, and `~/.local/bin/sigil`, then exec's `sigil cursor hook` on the first hit. If no binary is found it emits a permissive JSON response so `beforeSubmitPrompt` is never blocked and exits 0.

## Per-agent quirks

See the agent-plugin READMEs for agent-specific behaviour:

- [`plugins/claude-code/README.md`](../claude-code/README.md)
- [`plugins/codex/README.md`](../codex/README.md)
- [`plugins/cursor/README.md`](../cursor/README.md)

## Development

```sh
cd plugins/sigil
GOWORK=off go test ./...     # full suite, isolated from go.work
GOWORK=off go build ./...    # compile-check
make build                   # produces ./sigil
```

The repo-wide tasks `mise run lint:go` and `mise run format:go` cover this module via `find . -name go.mod`.

## Troubleshooting

Set `SIGIL_DEBUG=true` to enable logging to `$XDG_STATE_HOME/sigil/logs/sigil.log`. The wrappers always exit 0, so failures only show up in that log; nothing reaches the agent's stderr.

If an agent plugin appears to do nothing:

1. Run `sigil --version` from the user's shell. For Claude Code and Codex the manifest invokes `sigil` directly, so the binary must be on `PATH`. For Cursor the wrapper additionally probes `~/go/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, and `~/.local/bin`.
2. Confirm credentials are loaded: `cat $XDG_CONFIG_HOME/sigil/config.env`. The dotenv loader only honours `SIGIL_*` keys and a small set of `OTEL_*` keys.
3. Check the debug log for `not exporting: missing …` lines, which indicate the agent adapter saw incomplete credentials.
