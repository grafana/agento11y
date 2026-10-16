# OpenCode history fixtures

OpenCode stores sessions, messages, and parts as SQLite rows. The JSON files in
this directory are readable descriptions of those rows. Tests load each file
into a temporary SQLite database. No database or conversation copied from a
real user is committed here.

`mapping.json` covers three root turns: two share one user message, and the
third spawns a child session through a `task` part. It also covers accumulated
step usage, text, reasoning, completed and failed tools, and an ignored running
tool. The shared user row carries a synthetic `summary.diffs` value. The
importer must never export that value.

`v2-mapping.json` uses the `session_message` format checked against upstream commit `13afea6a1a76360936bbaa7677cff600ad52d452` and released v2.0.22 (`527f0b931d1f9b3ebd34e106c51b31ce5db5b075`) and v2.0.29 (`042d0832f5f3a73eaa039a58a7d565776f6d4290`).
See `packages/schema/src/session-message.ts`, `packages/core/src/session/sql.ts`, `session/message-updater.ts` and `database/v1-migration.bun.ts` in [OpenCode](https://github.com/anomalyco/opencode).
These are synthetic contract tests, not a real-host compatibility test.

These fixtures pin the on-disk shape expected by the Go reader and mapper. They
do not replay OpenCode's event stream and cannot detect an upstream schema
change.

## Differences from live capture

The history path cannot recover the composed system prompt, host version,
configured agent-name prefix, capture-time Git branch, or time to first token.
V1 uses the persisted message `cwd`; v2 reconstructs location switches, omitting earlier directories when the first switch has no previous location.
V2 forks omit generation `cwd` because copied messages can predate the inherited directory.
Live capture uses the plugin's project directory. The final database title applies to every imported root
turn; live capture only adds a title to turns recorded after the title event.

V1 builds its tool catalog from completed and failed parts; v2 includes unfinished tool calls too.
Neither path recovers full tool schemas, legacy tool overrides or standalone tool spans. It uses the persisted `parent_id` chain even when live capture started
too late to link a child. Historical generation IDs use the shared history ID
scheme, not the OpenCode plugin's live ID scheme.

See [History import](../../../../README.md#history-import) for input, cost, migration and duplicate-counting limits.
Each selected v2 session holds a read-only snapshot, which can delay WAL cleanup until the session finishes or the import stops.

History content passes through the shared full sanitizer. Live capture uses
lightweight redaction for assistant text and reasoning. Stored prompts also
predate any preflight rewrite made by an Agent Observability rule. The importer
ignores `message.data.summary`, including its raw file diffs.

## Checking against a real database

Run discovery and a dry-run import against the database without printing
message or part data:

```sh
cd plugins/agento11y
GOWORK=off go run ./cmd/agento11y history import opencode --dry-run --all --yes --since 365d
```

Use read-only SQL:

1. Count assistant messages whose JSON has a numeric `time.completed` value, an
   `error` object, or both.
2. Count sessions that contain at least one of those messages.
3. Apply the dry run's time bounds, five-minute recency exclusion and, for v2, unresolved execution-claim exclusion.
4. Compare the SQL totals with the planned session and turn counts.

The fixture cannot detect a table or JSON field rename in a newer OpenCode
release.
