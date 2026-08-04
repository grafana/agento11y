# pi session fixtures

These files pin the agreement between two implementations of one mapping.

`agento11y history import pi` backfills pi sessions from the JSONL logs pi writes
under `$PI_CODING_AGENT_DIR/sessions`. Live capture of the same sessions is the
TypeScript plugin `@grafana/agento11y-pi`. Every other importer proves it matches
live capture by calling the agent's own Go live mapper and comparing turn for
turn; pi has no Go live mapper, so `plugins/agento11y/internal/history/pi.go`
hand-ports the export path of `plugins/pi/src/mappers.ts` and
`plugins/pi/src/lineage.ts`. These fixtures are what stops the port drifting.

| File | Contents |
|------|----------|
| `sessions.json` | Session log inputs: the exact JSONL entries pi writes, one array per file. |
| `generations.json` | The normalized generation each input must produce, per case. |

Both sides read both files:

- Go: `plugins/agento11y/internal/history/pi_conformance_test.go` writes each
  case's files to a temporary directory, runs `piImporter.Turns`, normalizes, and
  structurally diffs against `generations.json`.
- TypeScript: `plugins/pi/src/piSessionConformance.test.ts` writes the same files,
  replays them as pi events through a faked pi host into the real JS SDK,
  normalizes the exported payload, and diffs against the same file.

`mise run test:pi-sessions-conformance` runs both halves.

## The inputs are real pi entries

An entry is copied from what pi's `SessionManager` writes, not invented:

- Line 1 is the session header: `{"type":"session","version":3,"id":…,"timestamp":…,"cwd":…}`,
  plus `parentSession` when the session is a fork.
- Every later line is a tree node with `id`, `parentId`, an ISO `timestamp`, and a
  `type`. Only `type: "message"` entries carry model traffic; `session_info` carries
  the user-set session name.
- A `message` entry holds a user message, an assistant message, or a tool result,
  told apart by `message.role` (`user`, `assistant`, `toolResult`).
- An assistant message carries `provider`, `model`, `responseId`, `stopReason`,
  `usage`, a unix-millisecond `timestamp`, and `content` blocks of type `text`,
  `thinking`, or `toolCall`.

Two encodings are worth stating because nothing in the proto describes them:

| Field | On disk | In the fixture |
|-------|---------|----------------|
| `message.timestamp` | unix milliseconds, stamped when the provider builds the message, before the request | not compared; see the placeholder table |
| tool call `arguments` | a JSON object | `output[].parts[].tool_call.arguments`, as a parsed object |

The tool call arguments are compared parsed, not as text, because the two sides
encode them differently on the wire: Go keeps embedded JSON, and the JS SDK
base64-encodes `input_json`. Each reader decodes before comparing.

`${DIR}` in an entry is replaced with the temporary directory the case runs in.
Only the fork case needs it: a fork's header names its trunk by absolute path, and
that path cannot be in a committed file.

## The placeholder rule

A `${NAME}` value in `generations.json` means: the field must be present and
non-empty, and its value cannot agree across the two implementations. Both
comparators enforce exactly that, and each has a test that asserts an empty
placeholder still fails, so a placeholder never becomes a hole.

| Placeholder | Why it cannot agree |
|-------------|---------------------|
| `${GENERATION_ID}` | Live IDs are `pi-sha256(conversationId\0entryId)[:24]` (`plugins/pi/src/lineage.ts`). Imported IDs are `histgen-…`, hashed from the source reference, and the framework guarantees a backfilled ID can never collide with a live one (`plugins/agento11y/internal/history/identity.go`). |
| `${PARENT_GENERATION_ID}` | The parent edge names a generation, so it follows the same two schemes. Its presence and count are compared; its value is not. |
| `${TRUNK_GENERATION_ID}` | Same, for the fork metadata's `pi.fork.parent_generation_id`. The sibling `pi.fork.parent_session_id` is a real session ID and is compared. |

One limit of the fork pointer is worth stating, because no case can cover it. An
imported generation ID hashes the path the session was read from, and a fork
names its trunk by whatever path string the user typed, so the import can only
reproduce the trunk's ID when the header spells the trunk the way discovery walks
it. Path noise is cleaned away; a trunk reachable under a genuinely different
path, through a symlinked home or a relative `PI_CODING_AGENT_DIR`, still reads
fine and still yields a pointer to an ID nothing ingested. Closing that needs the
framework to normalize `SourcePath` for every agent.
| `${STARTED_AT}`, `${COMPLETED_AT}` | Live reads its own clock: `startedAt` is `Date.now()` at `turn_start` and `completedAt` is `Date.now()` at the assistant `message_end`. The importer has neither instant and uses the two the log persists: `message.timestamp` for the start and the entry's ISO `timestamp` for the end. The importer's exact values are pinned in `pi_test.go` instead. |

## Fields that are not in the fixture at all

Each one is either live-only or import-only, so comparing it would fail for a
reason that is not drift.

| Field | Which side | Why |
|-------|------------|-----|
| `system_prompt` | live only | Read from pi's runtime at `turn_end`. The session log does not persist it. |
| `max_tokens`, `temperature`, `top_p`, `tool_choice`, thinking budget | live only | Read from the `before_provider_request` payload. Not persisted. |
| time to first token | live only | Observed from the first `message_update`. Not persisted. |
| `tools[].description`, `tools[].input_schema_json` | live only | Read from pi's tool registry. The log records only the calls a turn made, so an imported turn carries name-only definitions for those tools, and which tools were offered but unused is unrecoverable. |
| `tags` (`cwd`, `git.branch`) | live only | Resolved from `process.cwd()` per turn. The header's `cwd` is the session's start directory, not the turn's. |
| `agent_version`, `effective_version` | live only | The plugin's own version. Nothing in the log says which version wrote it. |
| `agent_name` | filled later | The importer leaves it empty and `Exporter.prepare` fills it from the agent ID, giving `pi`, which is what the plugin sends. |
| `agento11y.sdk.*` metadata | live only | Stamped by the SDK that exports. |
| `agento11y.import.*` metadata | import only | The backfill and quality markers (`plugins/agento11y/internal/history/export.go`). |
| quality flags | import only | `MissingModel`, `ApproxUsage` and the fork notes stay local to the import; they say what the log lacked, which live never has to ask. |

## What the cases cover

| Case | What it pins |
|------|--------------|
| `full-turn` | The base mapping: model, response ID, stop reason, usage, cost, one prompt in, one answer out. |
| `thinking-turn` | Thinking text, which pi persists and Claude Code does not, plus a redacted thinking block both sides drop. `usage.total_tokens` here is pi's own `input + output + cacheRead + cacheWrite`, deliberately not the Go launchers' `input + output`. |
| `tool-call-turn` | Two tool calls, both results matched to their call by ID, an error result, name-only tool definitions, and a second turn whose parent is the first one reached through the tool-result entries. The error result holds two text blocks, the first empty, which both sides drop before joining the rest with a newline. |
| `missing-usage` | A usage block with a cost and no token counts. Both sides map every count to zero rather than inventing one. |
| `fork` | A fork whose header names a trunk: the copied parent turn ships as `pi.fork.parent_session_id` metadata and no parent edge, because an edge would name a generation that does not exist under the fork's conversation ID. |

## One case has a different generation count than the file has turns

The `fork` input holds two assistant entries and both sides produce one
generation. A fork copies the trunk's entries into its own file; pi never fires
`turn_end` for a copied entry, so live never exported it under the fork's
conversation, and the importer skips it for the same reason. The trunk's own
import exports that turn. The copied prompts are skipped too, which is why the
fork's title and input come from the first prompt the fork itself sent.

## Turns the fixture cannot hold

Compaction and branch summaries exist live and cannot be imported, so no case
covers them. Live exports each host summarization call as its own generation. The
persisted `compaction` entry carries `summary`, `firstKeptEntryId`,
`tokensBefore`, `fromHook`, `details`, and on recent pi versions the call's
`usage` with its cost, but no model, no provider and no request timing, so the
call cannot be rebuilt as a generation. An imported session therefore holds fewer
generations than live capture produced, and the missing ones are the expensive
summarization calls, so comparing cost between imported and live data is not like
for like.

Subagent runs are missing from both sides rather than from the import alone. The
nested `<session>/<runId>/run-N/session.jsonl` files come from the third-party
`pi-subagents` package, and the plugin has no subagent code at all, so live
capture ignores them; importing them would exceed live fidelity rather than match
it.

## Changing a fixture

Neither `POST /api/v1/generations:export` field set nor the pi entry schema has a
generated stub tying these files to code, so the files are the contract. When a
mapping changes on purpose:

1. Change the mapping on both sides.
2. Update `generations.json`.
3. Run `mise run test:pi-sessions-conformance`, then `mise run test:ts:plugin-pi`
   to confirm the pi goldens in `plugins/pi/src/testdata/golden/` did not move for
   an unrelated reason.

Each side also has a comparator test that mutates a real generation one way the
fixture cannot accept and asserts the diff names the offending field. Keep them:
a permissive comparator keeps every case above green while checking nothing.
