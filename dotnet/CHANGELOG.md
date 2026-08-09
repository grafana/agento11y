# Changelog

## [0.4.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)
- **sdk**: emit client tags on OTel spans and metrics in Python, Java, and .NET (#385)
- **dotnet**: add secrets redaction sanitizer (#323)

## [0.3.0] - 2026-06-12

_No user-facing changes._

## [0.2.0] - 2026-06-01

### Features

- **sdk**: add User-Agent to generation traffic (#266)
- **sdks**: add full_with_metadata_spans content capture mode (#168)
- **sdk**: cache diagnostics metadata helpers (#150)
- **sdk**: record metrics while span is active for exemplar support (#162)

### Bug Fixes

- **sdk**: add agent version metric labels (#170)

## [0.1.0] - 2026-05-15

### Breaking Changes

- **sdks**: remove SDK-managed trace transport config

### Features

- high-level executeToolCalls API (issue #127) (#135)
- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)
- **sdk**: canonical SIGIL_* env-var schema in Java and .NET clients (#107)
- apply OTel GenAI token usage histogram buckets across SDKs (#93)
- apply OTel GenAI duration histogram buckets across SDKs (#85)
- **dotnet**: add ContentCaptureMode for SDK-level content stripping (#45)
- add parent_generation_ids field across all SDKs (#41)
- **sdks**: add langchain/langgraph framework integrations
- **sdks**: add embedding call observability across SDKs (#75)
- **sdks**: stamp sdk identity and remove devex runtime language attr
- add conversation ratings and annotations end-to-end
- deliver strict sdk parity and collector-first telemetry
- **sdk**: land strict openai parity and parity-gap execution plan
- **devex**: add core sdk traffic mega-emitter service
- add generation request controls across SDKs
- **dotnet-sdk**: add .NET SDK parity runtime and provider wrappers

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)
- **dotnet**: set required Container member in DevExEmitter (#20)
- **deps**: update protobuf monorepo (#661)
- **tool-analytics**: separate tool labels from request models (#539)
- **sdks**: stop leaking tool names into gen_ai.request.model metric (#502)
- **deps**: update protobuf monorepo (#179)
- **deps**: update protobuf monorepo (#66)
