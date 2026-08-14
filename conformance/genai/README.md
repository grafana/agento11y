# GenAI semantic-convention live checks

The Go suite in `go/otelgenai/conformance` sends spans, metrics, and log records to `weaver registry live-check` over OpenTelemetry Protocol (OTLP) gRPC. Weaver compares the received telemetry with the pinned GenAI semantic-convention registry.

## Version pins

The repository root `versions.env` pins two inputs:

- `WEAVER_VERSION` is the Weaver release installed by CI. Local runs use the `weaver` executable on `PATH`; install this version to match CI.
- `SEMCONV_GENAI_REF` is a commit in `open-telemetry/semantic-conventions-genai`.

The GenAI registry depends on the upstream semantic-conventions version in its own `versions.env`. The test setup downloads both archives on a cold run. It caches the prepared registry under `$SEMCONV_CACHE`. When that variable is unset, it uses `otel-conformance/semconv` under the operating system user cache directory.

The prepared registry removes the `gen-ai`, `mcp`, and `openai` directories from the upstream dependency. It also removes the migrated `registry.aws.bedrock` group. These definitions already exist in the GenAI registry and would otherwise be loaded twice.

Weaver's Rego engine refuses to fetch the external draft-07 meta-schema. Setup replaces that reference in the cached tool-definition schema with `type: object`, so it checks that parameters are objects but does not validate the schema contents.

## Weaver configuration

`.weaver.toml` filters OpenTelemetry SDK resource attributes such as `service.name`. Those attributes describe the test process and do not belong to the GenAI registry. The file also promotes `undefined_enum_variant` findings to violations, so an unknown operation name fails the suite.

The policies add checks that the registry cannot express on its own:

- `genai_span_validation.rego` checks span names, span kinds, operation-specific attribute sets, operation-name values, span status, and `error.type`.
- `genai_content_validation.rego` checks captured content against the JSON schemas in the GenAI registry.

The policy files start from `open-telemetry/opentelemetry-python-genai` at commit `8d11494c5417d13a1007f1546f1f16d5cae558df`. This copy omits that repository's handwritten operation allowlist because Weaver checks operation values against the pinned registry. The Weaver configuration starts from the same commit and promotes that registry check to a violation.

## Running the suite

Install the Weaver version from `versions.env`, then run:

```sh
mise run test:go:conformance
```

The tests skip when `weaver` is not on `PATH`. Each scenario rejects violation-level findings unless its allowlist names the advice ID and part of the message. The agento11y scenario allows only `missing_attribute` findings for its `agento11y.*` extension attributes.
