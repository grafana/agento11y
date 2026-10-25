# weavertest

A Go test harness that checks GenAI telemetry against the pinned OpenTelemetry
[GenAI semantic-convention registry](https://github.com/open-telemetry/semantic-conventions-genai)
with [Weaver](https://github.com/open-telemetry/weaver).

```sh
go get github.com/grafana/agento11y/go/otelgenai/weavertest@<commit>
```

## What it does

`Setup` prepares the registry for one `semantic-conventions-genai` commit and
returns the paths Weaver needs:

```go
assets, err := weavertest.Setup(ctx, os.Getenv("SEMCONV_GENAI_REF"))
```

A cold run downloads that commit and its upstream semantic-conventions
release. Older revisions name the release in `versions.env`. Newer revisions
use a Git dependency in `model/manifest.yaml`. `Setup` accepts both layouts and
rewrites the dependency to the local filtered registry. When tool spans reference
core process attributes, `Setup` imports the core process definitions too.

## Two runners

`Start` runs Weaver as an OTLP receiver and returns a `Report` when you call
`End`. Use it to check telemetry an SDK emits live.

`LiveCheck` runs Weaver over recorded spans passed as `Sample` values and
returns `Finding`s. Use it for fixtures or SDK spans recorded in memory. `Sample`
values come from `SampleFromSpan`, which converts an OTLP `tracepb.Span`.
`Finding.Target` is empty for a span and uses these exact forms elsewhere:

- span attribute: `<attribute>`
- event: `event[N]:<event-name>`
- event attribute: `event[N]:<event-name>/<attribute>`
- link: `link[N]`
- link attribute: `link[N]/<attribute>`

Both need the `weaver` binary on `PATH` and report `ErrNotInstalled` when it is
missing. The repository runs two pinned combinations:

| Task | GenAI registry commit | Weaver | Signals |
| --- | --- | --- | --- |
| `test:go:conformance` | `1d85c963ea51e9c7d24cc330ff67057f6e90e6c5` | 0.25.1 | Existing spans, metrics, and events |
| `test:go:tool-execution-kinds-conformance` | `771e3210f7518694666834f7243573b8018c7a4f` | 0.26.1 | Tool spans only |

The second task enables `conformance,tool_execution_kinds` and selects only
`TestToolExecutionKindsConformance`. The first task does not enable the new test.
Each task selects its own Weaver version. The newer registry has unrelated metric
changes, so the focused task never sends legacy metrics to it.

## Policies

`policies/` and `weaver.toml` are embedded, so a consumer needs no files of its
own. `Setup` stores them in a content-addressed directory beside the cached
registry. Later calls do not replace files while an existing Weaver process
reads them.

`weaver.toml` filters OpenTelemetry SDK resource attributes such as
`service.name`, which describe the emitting process and do not belong to the
GenAI registry. It also promotes `undefined_enum_variant` to a violation, so an
operation name outside the registry fails the check.

`Setup` generates `coverage-model.json` from the pinned registry with the
pinned Weaver binary. Both runners load that model with the registry's content
schemas as advice data. The model keeps the three selected tool execution kinds in
`tool_execution_kinds`, keyed by registry ID, separate from base `spans`.
Skill/process attributes do not become requirements for generic tools.

Exported tool spans have no execution-kind discriminator. The name policy accepts
the standard forms without inferring an execution kind from attributes or tool names.
The focused SDK test checks exact names and attribute omissions against the
shared cases in `conformance/tool-execution-kinds/cases.json`. An unavailable
recommended agent name remains absent; the harness does not invent one.

The policies add what the registry cannot express on its own:

- `genai_span_validation.rego`: registry-derived attribute and span-kind
  expectations, plus explicit operation classification and span-name rules.
- `span_validation.rego`: span status and `error.type` invariants.
- `genai_content_validation.rego`: string-valued captured content against the
  GenAI registry's JSON schemas.

Weaver's OTLP receiver does not retain original structured log values in its
report. Tests that need to compare those values must use an in-memory log
recorder beside the Weaver exporter.

The two GenAI-specific policy files start from
`open-telemetry/opentelemetry-python-genai` at commit
`8d11494c5417d13a1007f1546f1f16d5cae558df`. This copy drops that repository's
handwritten operation allowlist, because Weaver checks operation values against
the pinned registry instead.
