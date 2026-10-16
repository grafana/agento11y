# Warp (history import)

Capture [Warp](https://www.warp.dev) agent conversations with the shared
`agento11y` binary. Warp has no hook or plugin API for agent lifecycle events
yet ([warpdotdev/warp#7834](https://github.com/warpdotdev/warp/issues/7834)),
so this integration reads the conversations Warp already stores on disk. There
is no launcher and nothing to install into Warp.

```sh
# See what would be imported. Nothing is decoded, exported, or stored.
agento11y history import warp --dry-run

# Import into the local store on this machine.
agento11y history import warp --local

# Import into Grafana Cloud.
agento11y history import warp
```

The local Agent Observability app also offers Warp under Settings → History.

## Keeping Warp in sync

Re-running an import only sends turns the destination does not have yet, so a
scheduled import works as an incremental sync:

```sh
agento11y history import warp --all --yes --since 1d
```

Conversations written to in the last five minutes are skipped as still active
and picked up by the next run. Run it from cron, a launchd agent, or a systemd
timer at whatever interval suits you.

## Sources

The importer reads `warp.sqlite` for the Stable and Preview channels, for both
the Warp app and the Warp Agent CLI (`tui/warp.sqlite`). The databases are
opened read-only.

| Platform | Location |
|----------|----------|
| macOS | `~/Library/Group Containers/2BBY89MBSN.dev.warp/Library/Application Support/dev.warp.Warp-{Stable,Preview}/` and the older `~/Library/Application Support/dev.warp.Warp-{Stable,Preview}/` |
| Linux | `$XDG_STATE_HOME/warp-terminal{,-preview}/` (default `~/.local/state/…`) |

Windows is not supported yet.

## Captured data and limits

- One generation per Warp agent request: the round trip that sends a user
  query or tool results and receives the model's reasoning, text, and tool
  calls. Requests chain through `parent_generation_ids`. Subagent tasks that
  share a request are merged into it.
- The model comes from Warp's model record for the request, such as
  `claude-4-6-sonnet-high`. The provider is inferred from the model name and
  falls back to `warp`.
- Token usage and cost (`cost_usd`) come from the per-request billing record.
  Older Warp versions did not write it. Turns without one are marked
  `approx_usage` and carry no tokens.
- Tool names cover every Warp tool. Tool arguments are decoded for shell
  commands, file reads, grep, file globs, codebase search, file edits (paths
  only), and MCP calls. Tool results are decoded for shell commands and MCP
  calls; other results carry only their name and error state.
- The conversation title is the root task's description. The `cwd` tag is the
  most recent working directory Warp recorded for the conversation.

History cannot recover the system prompt, the agent profile, time to first
token, or separate tool spans. Content passes through the shared history
sanitizer, and `AGENTO11Y_CONTENT_CAPTURE_MODE` applies as for every other
import.

Warp's storage format is internal and may change without notice. The reader
decodes Warp's task protobufs directly, following the public schema in
[warp-proto-apis](https://github.com/warpdotdev/warp-proto-apis), and skips
fields it does not recognize.

## Checking against a real database

Run discovery and a dry-run import without printing conversation content:

```sh
cd plugins/agento11y
GOWORK=off go run ./cmd/agento11y history import warp --dry-run --all --yes --since 365d
```

The planned turn count should match the number of distinct request IDs in the
`agent_tasks` payloads of the non-active conversations in range.
