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

`mise run test:pi-sessions-conformance` runs both halves. The supported live test baseline is Pi 1.4.0; these fixtures do not establish older-host compatibility.

## The inputs are real pi entries

An entry is copied from what pi's `SessionManager` writes, not invented:

- Line 1 is the session header: `{"type":"session","version":3,"id":…,"timestamp":…,"cwd":…}`,
  plus `parentSession` when the session is a fork.
- Every later line is a tree node with `id`, `parentId`, an ISO `timestamp`, and a
  `type`. Only `type: "message"` entries carry model traffic; `session_info` carries
  the user-set session name.
- A `message` entry holds a user message, an assistant message, or a tool result,
  told apart by `message.role` (`user`, `assistant`, `toolResult`).
- An assistant message includes `provider`, `model`, optional `responseModel`,
  `responseId`, `stopReason`, `usage`, a unix-millisecond `timestamp`, and
  `content` blocks of type `text`, `thinking`, or `toolCall`.
  `model` identifies the request. Both implementations map the response identity
  as `responseModel ?? model`; a missing or null response identity uses `model`.

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
| `${STARTED_AT}`, `${COMPLETED_AT}` | Live observes `turn_start` and assistant `message_end`. Import uses persisted `message.timestamp` and the entry's ISO `timestamp`. The importer's exact values are pinned in `pi_test.go`. |

One limit of the fork pointer is worth stating, because no case can cover it. An
imported generation ID hashes the path the session was read from, and a fork
names its trunk by whatever path string the user typed, so the import can only
reproduce the trunk's ID when the header spells the trunk the way discovery walks
it. Path noise is cleaned away; a trunk reachable under a genuinely different
path, through a symlinked home or a relative `PI_CODING_AGENT_DIR`, still reads
fine and still yields a pointer to an ID nothing ingested. Closing that needs the
framework to normalize `SourcePath` for every agent.

## Fields that are not in the fixture at all

Each field here is on one side only, or on both with values that cannot agree,
so comparing it would fail for a reason that is not drift.

| Field | Which side | Why |
|-------|------------|-----|
| `system_prompt` | omitted by both | Pi 1.4.0 exposes intermediate prompts, not a verified final provider prompt. The session log does not persist it. |
| `max_tokens`, `temperature`, `top_p`, `tool_choice`, thinking budget | omitted by both | Provider callbacks lack trustworthy request correlation. Live does not assign warming or background controls to foreground generations. |
| time to first token | live only | Observed from the first `message_update`. Not persisted. |
| `tools[].description`, `tools[].input_schema_json` | omitted by both | Both sides emit name-only definitions from assistant tool-call proposals. These names describe observed calls, not the complete offered catalog. |
| `tags` (`cwd`, `git.branch`) | both, not compared | Both sides emit `cwd`, but live resolves it per turn and import reads the session's start directory. Only live emits `git.branch`; the log does not record a branch. |
| `agent_version`, `effective_version` | live only | The plugin's own version. Nothing in the log says which version wrote it. |
| `agent_name` | filled later | The importer leaves it empty and `Exporter.prepare` fills it from the agent ID, giving `pi`, which is what the plugin sends. |
| `agento11y.sdk.*` metadata | live only | Stamped by the SDK that exports. |
| `pi.tool_usage.*` metadata | live only | Finite, nonnegative numeric usage and cost from finalized top-level tool results. Pi already aggregates nested usage there, so live counts it once. It does not change assistant usage, `cost_usd`, or backend generation totals. It is not attributed to the requesting model. |
| `pi.usage.cache_write_1h_input_tokens` metadata | live only | Assistant or summary one-hour cache writes, already included in cache-write counts. Tool usage retains its own non-additive detail under `pi.tool_usage.cache_write_1h_input_tokens`. |
| `agento11y.import.*` metadata | import only | The backfill and quality markers (`plugins/agento11y/internal/history/export.go`). |
| quality flags | import only | `MissingModel`, `ApproxUsage` and the fork notes stay local to the import and describe missing persisted evidence. |

## What the cases cover

| Case | What it pins |
|------|--------------|
| `full-turn` | Distinct request and response models, response ID, stop reason, usage, cost, one prompt in, one answer out. Other cases pin the fallback when `responseModel` is absent. |
| `thinking-turn` | Thinking text, which pi persists and Claude Code does not, plus a redacted thinking block both sides drop. `usage.total_tokens` here is pi's own `input + output + cacheRead + cacheWrite`, deliberately not the Go launchers' `input + output`. |
| `tool-call-turn` | Two tool calls, both results matched to their call by ID, an error result, name-only tool definitions, and a second turn whose parent is the first one reached through the tool-result entries. The error result holds two text blocks, the first empty, which both sides drop before joining the rest with a newline. The first turn reports 7 reasoning tokens; the second reports an explicit zero. Output, total tokens, cache counts, and cost stay unchanged. |
| `missing-usage` | A usage block with a cost and no token counts. Both sides map every count to zero rather than inventing one. |
| `fork` | A fork whose header names a trunk: the copied parent turn ships as `pi.fork.parent_session_id` metadata and no parent edge, because an edge would name a generation that does not exist under the fork's conversation ID. |

## One case has a different generation count than the file has turns

The `fork` input holds two assistant entries and both sides produce one
generation. A fork copies the trunk's entries into its own file; pi never fires
`turn_end` for a copied entry, so live never exported it under the fork's
conversation, and the importer skips it for the same reason. The trunk's own
import exports that turn. The copied prompts are skipped too, which is why the
fork's title and input come from the first prompt the fork itself sent.

## Live observations outside the fixture contract

The shared cases compare persisted turn mapping. They do not prove live guard coverage or final provider input. Live incremental input uses validated request observations and omits ambiguous bodies instead of restoring raw history.

Live tool state separates assistant proposals, guard-stage input, and post-execution `tool_result.input`. Transformed or uncertain proposal bodies are omitted. Permitted span input is labeled as a post-execution observation, never an execution-entry snapshot. Top-level result bodies and generation error flags use finalized transcript messages, including replacements. Nested span relationships and nested result bodies come from live events, not invented transcript entries. These distinctions need live tests; the unchanged proposal bodies in the shared cases cannot prove them.

Live preserves active-turn input and timing across request-time compaction. Failed assistant attempts remain separate from successful retries. Persisted usage does not establish accounting for every failed attempt.

Persisted fork evidence takes precedence over live observations. A reload-stable ephemeral registry retains origin facts only for the same public `SessionManager` object. It stores no clients or message bodies. Independent managers and process restarts cannot recover those ephemeral facts. Live suppresses uncertain ancestry instead of manufacturing a parent edge.

Guard transformations do not establish universal historical replay protection after reload or resume. The plugin does not rerun historical tool authorization indiscriminately. See [Pi guard limits](../../plugins/pi/README.md#guards).

## Turns the fixture cannot hold

Compaction and branch summaries exist live and cannot be imported, so no case
covers them. Live exports each successful built-in summary as an operation
aggregate. Split compaction can make several requests, and operation timing can
include retries and backoff. Saved usage does not account for every failed attempt. Failed or cancelled compaction clears pending state without creating a zero-usage generation.

Pi exposes no `session_tree_failed` event. Tree events, the plugin's own cancellation, and shutdown clear pending tree timing. The next tree preparation replaces it. Live cannot observe every other tree failure.

Summary entries lack physical model identity. Live uses `unknown` for the request
model and provider, omits the response model, and sets
`pi.summary.model_attribution: "unknown"` and
`pi.summary.operation_aggregate: true`. When available, the current selection is
recorded separately as `pi.summary.selected_model` and
`pi.summary.selected_provider`. A selected router is not a physical response model.

The persisted `compaction` entry includes `summary`, `firstKeptEntryId`,
`tokensBefore`, `fromHook`, `details`, and sometimes aggregate `usage` and cost.
It lacks model, provider, and request timing, so import skips it. Imported and live
sessions therefore have different generation counts and cost coverage.

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
