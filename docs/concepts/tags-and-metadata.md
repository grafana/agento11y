# Tags and Metadata

The SDK lets you attach custom key/value data (team, project, environment, request ID, end-user id) to what you record. Where each piece of data shows up depends on how you attach it. There are three independent mechanisms, and only one of them reaches OTel metrics.

Each SDK README links here for the language-specific config fields.

## The three mechanisms

| Mechanism | Set where | Cardinality | Generation export (Agent Observability UI) | OTel spans (traces) | OTel metrics |
| --- | --- | --- | --- | --- | --- |
| **Client tags** (`AGENTO11Y_TAGS` / config `tags`) | Once, on the client | Keep low | Yes, merged into every generation | Yes, as `agento11y.tag.<key>` | Yes, as `agento11y.tag.<key>` |
| **Per-generation `tags`** | Per `startGeneration` call | Any | Yes | No | No |
| **`metadata`** (struct/dict) | Per `startGeneration` call | Any | Yes | No | No |

There is also a dedicated **`user_id`** field (`AGENTO11Y_USER_ID` / config / per-call / context). It is recorded on the generation export and on the generation span as the `user.id` attribute (all SDKs), but it is **not** a metric label.

## Cross-SDK parity

`user.id` is emitted on the generation span by all five SDKs (Go, Python, JS, Java, .NET).

All five SDKs merge client tags into the generation export and emit them as `agento11y.tag.<key>` attributes on OTel spans and metrics.

Client tags become OTel metric attributes, which become Prometheus label values: one time series per distinct value.

## Setting them

### Client tags and default user id (apply to every generation)

Set client tags with the `AGENTO11Y_TAGS` env var (CSV: `key=value,key=value`) and `AGENTO11Y_USER_ID`. The SDK reads them when you construct the client with no explicit values. To set them in code:

**Go**

```go
cfg := agento11y.DefaultConfig()
cfg.Tags = map[string]string{"team": "checkout", "env": "prod"}
cfg.UserID = "u-1234" // default; per-call UserID and context still win
client := agento11y.NewClient(cfg)
```

**Python**

```python
client = Client(ClientConfig(
    tags={"team": "checkout", "env": "prod"},
    user_id="u-1234",
    generation_export=...,
))
```

**TypeScript / JavaScript**

```ts
const agento11y = createAgento11yClient({
  tags: { team: "checkout", env: "prod" },
  userId: "u-1234",
  generationExport: { /* ... */ },
});
```

### Per-generation tags, metadata, and user id

Per-call values win over client-level values on key conflict. Per-call `tags` and `metadata` are export-only; they do not appear on spans or metrics.

**Go**

```go
ctx, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
    Model:    agento11y.ModelRef{Provider: "openai", Name: "gpt-4.1-mini"},
    UserID:   "u-1234",                                  // -> user.id span attribute + export
    Tags:     map[string]string{"feature": "summarize"}, // export only
    Metadata: map[string]any{"prompt_version": "v2"},   // export only
})
defer rec.End()
```

**Python**

```python
with client.start_generation(GenerationStart(
    model=ModelRef(provider="openai", name="gpt-4.1-mini"),
    user_id="u-1234",                  # -> user.id span attribute + export
    tags={"feature": "summarize"},     # export only
    metadata={"prompt_version": "v2"} # export only
)) as rec:
    ...
```

**TypeScript / JavaScript**

```ts
await agento11y.startGeneration(
  {
    model: { provider: "openai", name: "gpt-4.1-mini" },
    userId: "u-1234",                  // -> user.id span attribute + export
    tags: { feature: "summarize" },    // export only
    metadata: { promptVersion: "v2" }, // export only
  },
  (rec) => { /* rec.setResult(...) */ },
);
```

## Built-in tags from the agent launchers

The coding-agent plugins (claude-code, codex, copilot, cursor, opencode, pi, vibe) add tags of their own to every generation, on top of whatever you set in `AGENTO11Y_TAGS`. A built-in key wins if you set the same key yourself. A tag is left off when the launcher has no value for it, so a missing key means "unknown" rather than "false".

| Tag | Value | Emitted by |
| --- | --- | --- |
| `cwd` | Working directory of the session. For pi, the directory pi was launched from, which can be a subdirectory of the repo. | all launchers |
| `git.branch` | Branch checked out in that directory, or a short commit SHA when HEAD is detached. Omitted outside a git checkout. | all launchers |
| `subagent` | `"true"` on generations from a subagent run. Absent otherwise, never `"false"`. | claude-code, codex, cursor, opencode |
| `pi.call_kind` | The host made this model call outside a user turn: `compaction` for context compaction, `branch_summary` for a tree-navigation summary. Absent on ordinary turns. | pi |

Launchers also set a few keys specific to one host, so this table is not the full list of what arrives on a generation.

## Opt-in automatic tags (`AGENTO11Y_AUTO_CODING_AGENT_TAGS`)

The built-in tags above are per-generation tags, so they reach the Agent Observability UI but never become metric labels. `AGENTO11Y_AUTO_CODING_AGENT_TAGS` resolves the same kind of session facts and attaches them as **client tags** instead, which is the one mechanism that does reach OTel metrics. That is what lets the Usage and Cost view filter and break down by user, repository, or branch. It is a coding-agent-plugin feature; the SDKs have nothing like it.

The switch is off by default and takes a boolean. With the switch alone, every supported value is tagged:

```sh
AGENTO11Y_AUTO_CODING_AGENT_TAGS=true
```

`AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES` narrows that to the names you list. It defaults to all of them:

```sh
AGENTO11Y_AUTO_CODING_AGENT_TAGS=true
AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo   # `all` is also accepted
```

| Name | Tag key | Value |
| --- | --- | --- |
| `user` | `user` | `AGENTO11Y_USER_ID` if set, then the identity the host agent knows (the signed-in Claude Code or Cursor account), then the operating-system account name. |
| `repo` | `repo` | `owner/name` from the `origin` remote of the checkout. A nested namespace stays whole (`group/subgroup/name`). Without an origin remote, the name of the checkout directory. |
| `branch` | `git.branch` | Branch checked out in the session's directory, or a short commit SHA on detached HEAD. |

Rules that apply to every name:

- Names are case-insensitive. An unsupported name is logged and skipped; the recognized names still apply. A list that names nothing supported attaches nothing.
- The allowlist does nothing on its own. With `AGENTO11Y_AUTO_CODING_AGENT_TAGS` unset or false, no automatic tag is attached and the launcher logs that the list was ignored.
- A key you set yourself in `AGENTO11Y_TAGS` wins. With `AGENTO11Y_TAGS=repo=monorepo` and the `repo` name enabled, the exported tag stays `repo=monorepo`.
- A value that does not resolve leaves its key off. Outside a git checkout, `repo` and `branch` are simply absent.
- Values are trimmed and capped at 128 characters.
- `branch` uses the same `git.branch` key as the per-generation built-in above. In the generation export the per-generation value wins, so the export still shows the branch of that generation; the client tag is what supplies the metric label.

In Prometheus the tags arrive as `agento11y_tag_user`, `agento11y_tag_repo`, and `agento11y_tag_git_branch`.

`agento11y login` asks about this too. Choose On under "Automatic tags" and it opens a checklist of the three values, then writes your answers to `config.env`:

- A first run ticks all three. A rerun ticks the names the saved allowlist holds.
- If the saved allowlist names nothing supported, login ticks nothing, because that config attaches no tags today. The checklist refuses an empty answer, so tick the names you want or go back and choose Off.
- Ticking all three writes no allowlist, so a name added in a later version applies too.
- Choosing Off sets the switch to false and deletes the allowlist. No unusable key stays behind.

Run `agento11y doctor` to see the enabled names, the variables that set them, and the value each one resolves to in the current directory before it leaves the machine. `doctor` runs outside a session, so it cannot read the account a host agent signs in to: with `AGENTO11Y_USER_ID` unset it reports `user` as `<depends on the agent>` rather than guessing. Set `AGENTO11Y_USER_ID` to see the exact value.

### Cardinality and personal data

Enabling these names is a deliberate trade. Read this first:

- The `user` value is commonly a work email address. It becomes a Prometheus label and is kept for the metric retention period, which is longer than most log retention. Enable it only where storing that address in metrics is acceptable.
- Every distinct combination of values is one more time series. Users multiply by repositories multiply by branches, and branch names are the fastest-growing of the three: a team that creates a branch per ticket adds a series per ticket, forever.
- `repo` and `user` are usually bounded per organization. `branch` is not. Set `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo` first and add `branch` only if you need per-branch cost.
- In the pi and opencode plugins the client is built once per session, so their metric labels freeze at session start. A checkout that changes mid-session keeps the label it started with. The hook-based agents (claude-code, codex, copilot, cursor, vibe) build a client per invocation and follow the checkout.

## Built-in metadata from the agent launchers

Metadata is exported but never turned into a metric label, so launchers use it for numbers and for keys with too many distinct values to be a tag.

Codex and copilot also add their own `codex.*` and `copilot.*` keys, so this table is not the full list. A launcher's own docs are the place to look for them.

| Key | Value | Emitted by |
| --- | --- | --- |
| `cost_usd` | Cost the coding agent itself reported, in US dollars. | pi, opencode, vibe |
| `pi.tokens_before` | Pi's estimate of the context size before a compaction, in tokens. Compaction generations only. The estimate covers the whole conversation, not this call's input tokens. | pi |
| `pi.compaction.reason` | What triggered the compaction: `manual` (`/compact` or `ctx.compact()`), `threshold` (context limit), or `overflow` (context-overflow recovery). | pi |
| `pi.compaction.will_retry` | `true` when pi retries the turn it aborted to run this compaction. Only overflow recovery retries. | pi |
| `pi.fork.parent_session_id` | Conversation id of the session a fork was taken from. On the fork's first generation only. | pi |
| `pi.fork.parent_generation_id` | Generation id of the trunk turn the fork continues from. Ships as metadata rather than a parent edge, because the trunk only holds that generation if it ran instrumented. | pi |
| `opencode.parent_session_id` | Session id of the run that spawned this subagent session. On every subagent generation, including one whose parent turn could not be named. | opencode |
| `opencode.child_session_id` | Subagent's own session id. Present when its turns were reparented onto the spawning conversation, where `conversation_id` names the root session of the subagent chain instead. | opencode |

## See also

- [Content Capture Modes](content-capture-modes.md) — which content fields ship. Content capture does not strip `tags` or `metadata`; both are always exported.
- Per-language SDK READMEs for the full config surface and env-var mapping.
