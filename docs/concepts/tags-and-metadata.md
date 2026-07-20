# Tags and Metadata

The SDK lets you attach custom key/value data (team, project, environment, request ID, end-user id) to what you record. Where each piece of data shows up depends on how you attach it. There are three independent mechanisms, and only one of them reaches OTel metrics.

Each SDK README links here for the language-specific config fields.

## The three mechanisms

| Mechanism | Set where | Cardinality | Generation export (Sigil UI) | OTel spans (traces) | OTel metrics |
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

## See also

- [Content Capture Modes](content-capture-modes.md) — which content fields ship. Content capture does not strip `tags` or `metadata`; both are always exported.
- Per-language SDK READMEs for the full config surface and env-var mapping.
