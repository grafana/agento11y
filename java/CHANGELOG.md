# Changelog

## [0.7.0] - 2026-08-25

### Features

- **retries**: add configurations for java and .NET (#675)

### Bug Fixes

- **sdk**: change operation.duration  and client.token.usage description to match semconv (#648)

## [0.6.0] - 2026-08-17

### Breaking Changes

- bump default timeout to 30s and support overriding via env var (#566)

### Features

- **providers**: normalize provider usage to the inclusive contract (phase 1: providers) (#533)
- **core**: carry TokenInputSemantics through models and mark telemetry (phase 1: core) (#532)

### Bug Fixes

- **deps**: update dependency com.google.genai:google-genai to v1.63.0 (#250)
- **sdk**: derive SDK versions from build metadata instead of constants (#445)

## [0.5.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)
- **sdk**: emit client tags on OTel spans and metrics in Python, Java, and .NET (#385)

### Bug Fixes

- **security/high/java/gradle**: update dependency com.fasterxml.jackson.core:jackson-databind to v2.21.5 [security] (#351)

## [0.4.0] - 2026-06-12

_No user-facing changes._

## [0.3.0] - 2026-06-01

### Features

- **sdk**: add User-Agent to generation traffic (#266)

## [0.2.0] - 2026-05-27

### Features

- **sdks**: add full_with_metadata_spans content capture mode (#168)
- **sdk**: cache diagnostics metadata helpers (#150)
- **sdk**: record metrics while span is active for exemplar support (#162)

### Bug Fixes

- **sdk**: add agent version metric labels (#170)

## [0.1.0] - 2026-05-15

### Breaking Changes

- **sdks**: remove SDK-managed trace transport config

### Features

- auto-append /api/v1/generations:export to HTTP endpoint (#131)
- high-level executeToolCalls API (issue #127) (#135)
- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)
- **sdk**: canonical SIGIL_* env-var schema in Java and .NET clients (#107)
- apply OTel GenAI token usage histogram buckets across SDKs (#93)
- apply OTel GenAI duration histogram buckets across SDKs (#85)
- **java**: add ContentCaptureMode for SDK-level content stripping (#44)
- **java**: add Maven Central publishing via Central Portal (#73)
- add parent_generation_ids field across all SDKs (#41)
- **sdks**: add framework-native integrations for agents and ADK
- **sdks**: add embedding call observability across SDKs (#75)
- **sdks**: stamp sdk identity and remove devex runtime language attr
- add conversation ratings and annotations end-to-end
- deliver strict sdk parity and collector-first telemetry
- **sdk**: land strict openai parity and parity-gap execution plan
- **devex**: add core sdk traffic mega-emitter service
- add generation request controls across SDKs
- **sdks**: add Java SDK parity track with transport/runtime/provider coverage

### Bug Fixes

- **security/medium**: update dependency io.opentelemetry:opentelemetry-api to v1.62.0 [security] (#155)
- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)
- **java-sdk**: set gen_ai.operation.name on tool-execution spans (#80)
- **java**: include devex-emitter subproject in gradle settings (#19)
- **ci**: resolve build failures in TS, Java, and .NET jobs
- **deps**: update dependency com.openai:openai-java to v4.29.1 (#684)
- **deps**: update jackson monorepo to v2.21.2 (#654)
- **deps**: update protobuf monorepo (#661)
- **deps**: update dependency com.anthropic:anthropic-java to v2.18.0 (#631)
- **deps**: update dependency com.openai:openai-java to v4.29.0 (#625)
- **deps**: update dependency com.google.genai:google-genai to v1.44.0 (#624)
- **deps**: update dependency com.anthropic:anthropic-java to v2.17.0 (#611)
- **deps**: update grpc-java monorepo to v1.80.0 (#612)
- **deps**: update dependency com.openai:openai-java to v4.28.0 (#583)
- **deps**: update dependency com.google.genai:google-genai to v1.43.0 (#574)
- **deps**: update dependency com.anthropic:anthropic-java to v2.16.1 (#557)
- **tool-analytics**: separate tool labels from request models (#539)
- **sdks**: stop leaking tool names into gen_ai.request.model metric (#502)
- **deps**: update dependency com.anthropic:anthropic-java to v2.16.0 (#421)
- **deps**: update opentelemetry-java monorepo to v1.60.1 (#423)
- **deps**: update dependency com.openai:openai-java to v4.26.0 (#359)
- **deps**: update dependency com.google.genai:google-genai to v1.42.0 (#364)
- **deps**: update dependency com.google.genai:google-genai to v1.41.0 (#172)
- **deps**: update dependency com.openai:openai-java to v4.23.0 (#176)
- **deps**: update protobuf monorepo (#179)
- **deps**: update jackson monorepo to v2.21.1 (#159)
- stabilize sdk traffic ingest and make online eval opt-in (#151)
- **deps**: update dependency com.anthropic:anthropic-java to v2.15.0 (#112)
- **deps**: update dependency com.google.genai:google-genai to v1.40.0 (#113)
- **deps**: update dependency com.openai:openai-java to v4.22.0 (#114)
- **deps**: update dependency org.junit:junit-bom to v6
- **deps**: update dependency org.junit:junit-bom to v5.14.3 (#80)
- **deps**: update protobuf monorepo to v4
- **deps**: update dependency com.openai:openai-java to v4.21.0
- **deps**: update dependency com.squareup.okhttp3:mockwebserver to v5 (#74)
- **deps**: update protobuf monorepo (#66)
- **deps**: update opentelemetry-java monorepo to v1.59.0 (#64)
- **deps**: update jackson monorepo to v2.21.0 (#60)
- **deps**: update dependency com.openai:openai-java to v4.20.0 (#55)
- **deps**: update dependency com.google.genai:google-genai to v1.39.0 (#54)
- **deps**: update jackson monorepo (#58)
- **deps**: update grpc-java monorepo to v1.79.0 (#57)
- **deps**: update dependency org.junit:junit-bom to v5.14.2 (#56)
- **deps**: update dependency com.anthropic:anthropic-java to v2.14.0 (#53)
- **deps**: update dependency io.grpc:grpc-netty-shaded to v1.75.0 [security] (#30)
- **deps**: update dependency org.assertj:assertj-core to v3.27.7 [security] (#29)
