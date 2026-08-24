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

These fixtures pin the on-disk shape expected by the Go reader and mapper. They
do not replay OpenCode's event stream and cannot detect an upstream schema
change.

## Differences from live capture

The history path cannot recover the composed system prompt, host version,
configured agent-name prefix, capture-time Git branch, or time to first token.
It uses the persisted message `cwd`, while live capture uses the plugin's
project directory. The final database title applies to every imported root
turn; live capture only adds a title to turns recorded after the title event.

The importer builds its tool catalog from completed and failed parts. It cannot
recover legacy tool overrides, incomplete tool executions, or standalone tool
spans. It uses the persisted `parent_id` chain even when live capture started
too late to link a child. Historical generation IDs use the shared history ID
scheme, not the OpenCode plugin's live ID scheme.

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
3. Apply the dry run's time bounds and five-minute active-session exclusion.
4. Compare the SQL totals with the planned session and turn counts.

The fixture cannot detect a table or JSON field rename in a newer OpenCode
release.
