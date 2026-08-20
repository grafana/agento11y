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

A cold run downloads that commit and the upstream semantic-conventions release
its `versions.env` names.

## Two runners

`Start` runs Weaver as an OTLP receiver and returns a `Report` when you call
`End`. Use it to check telemetry an SDK emits live.

`LiveCheck` runs Weaver over recorded spans passed as `Sample` values and
returns `Finding`s. Use it to check fixtures with no SDK in the loop. `Sample`
values come from `SampleFromSpan`, which converts an OTLP `tracepb.Span`.

Both need the `weaver` binary on `PATH` and report `ErrNotInstalled` when it is
missing. Pin the version: this package is developed against Weaver 0.25.1.

## Policies

`policies/` and `weaver.toml` are embedded, so a consumer needs no files of its
own. `Setup` stores them in a content-addressed directory beside the cached
registry. Later calls do not replace files while an existing Weaver process
reads them.

`weaver.toml` filters OpenTelemetry SDK resource attributes such as
`service.name`, which describe the emitting process and do not belong to the
GenAI registry. It also promotes `undefined_enum_variant` to a violation, so an
operation name outside the registry fails the check.

The policies add what the registry cannot express on its own:

- `genai_span_validation.rego`: span names, span kinds, operation-specific
  attribute sets, operation-name values, span status, and `error.type`.
- `genai_content_validation.rego`: captured content against the JSON schemas in
  the GenAI registry.

The two policy files start from
`open-telemetry/opentelemetry-python-genai` at commit
`8d11494c5417d13a1007f1546f1f16d5cae558df`. This copy drops that repository's
handwritten operation allowlist, because Weaver checks operation values against
the pinned registry instead.
