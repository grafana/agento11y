# Changelog

## [0.16.0] - 2026-08-05

### Features

- **providers**: normalize provider usage to the inclusive contract (phase 1: providers) (#533)
- **redaction**: generate secret patterns from one table and pin cross-SDK parity (#539)
- **core**: carry TokenInputSemantics through models and mark telemetry (phase 1: core) (#532)
- **proto**: TokenInputSemantics marker on TokenUsage (OTel-inclusive contract, phase 0) (#529)
- **sdk**: gate cloud trial evaluation behind an experimental opt-in (#496)
- **sdk/go**: add cloud evaluation support for experiments (#488)
- **experiments**: add Go tracking parity (#454)
- **sdk**: send new X-Agento11y-* request headers (#469)
- correlate guard decisions with conversations (#466)

### Bug Fixes

- **security/unknown/go**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#463)
- **security/unknown/go-providers/openai**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#462)
- **security/unknown/go-providers/gemini**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#461)
- **security/unknown/go-providers/anthropic**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#460)
- **security/unknown/go-frameworks/google-adk**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#459)
- **sdk/go**: detach retained metric label strings (#504)
- **sdk/go**: release exported batch references (#503)
- **sdk**: derive SDK versions from build metadata instead of constants (#445)

## [0.15.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)

## [0.14.1] - 2026-07-16

### Bug Fixes

- **go**: clone media message parts (#391)

## [0.14.0] - 2026-07-15

### Features

- add media message parts (#386)

## [0.13.0] - 2026-07-15

### Features

- **sdk-go**: add WithTag/WithTags for per-request dimensional tags (#384)

## [0.12.0] - 2026-07-14

### Features

- **go,js,python**: workflow-step ingest with cross-language parity (#361)

### Bug Fixes

- **cc**:  Treat duplicate generation export results as idempotent success (#370)
- **sdk-go**: skip empty thinking blocks in anthropic generation mapping (#369)

## [0.11.1] - 2026-07-01

### Features

- **go**: adding Experiments support

## [0.11.0] - 2026-06-12

### Features

- **sdk**: promote SIGIL_TAGS onto OTel spans and metrics (#284)

## [0.10.0] - 2026-06-03

### Features

- **experiments**: go SDK support for experiments API (#277)
- **sdk**: add SIGIL_REDACT_INPUT_MESSAGES env support (#257)

## [0.9.0] - 2026-06-01

### Features

- **sdk**: add User-Agent to generation traffic (#266)

## [0.8.0] - 2026-05-28

### Features

- **sdk/go**: expose importable generation packages (#188)

### Bug Fixes

- **security/unknown**: update module golang.org/x/crypto to v0.52.0 [security] (#236)
- **security/unknown**: update module golang.org/x/net to v0.55.0 [security] (#237)
- **security/unknown**: update module golang.org/x/sys to v0.44.0 [security] (#238)

### Documentation

- clarify content capture modes across SDKs and plugins (#229)

## [0.7.0] - 2026-05-27

### Features

- **sdks**: add full_with_metadata_spans content capture mode (#168)
- **sdk/go**: validate export responses and surface rejected generations (#164)

### Bug Fixes

- **sdk/go**: surface async export failures through Flush() (#171)
- **sdk**: add agent version metric labels (#170)

## [0.6.0] - 2026-05-20

### Features

- **sdk**: cache diagnostics metadata helpers (#150)
- **sdk**: record metrics while span is active for exemplar support (#162)

## [0.5.0] - 2026-05-15

### Features

- **sdk-go**: built-in secret redaction sanitizer (#141)

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)

## [0.4.0] - 2026-05-11

### Features

- auto-append /api/v1/generations:export to HTTP endpoint (#131)
- high-level executeToolCalls API (issue #127) (#135)
- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)
- **proto**: regenerate protos with effective_version (#120)
- **hooks**: support transformed_input from hooks:evaluate (#108)

### Bug Fixes

- **proto**: add WorkflowStep schema to protobuf  (#132)
- **deps**: update module github.com/grafana/sigil-sdk/go to v0.2.0 (#104)

## [0.3.0] - 2026-05-01

### Features

- **sdk**: canonical SIGIL_* env-var schema in Go/JS/Python clients (#103)
- **sdk**: synchronous hook evaluation (TS / Go / Python) (#83)
- apply OTel GenAI token usage histogram buckets across SDKs (#93)
- apply OTel GenAI duration histogram buckets across SDKs (#85)
- add parent_generation_ids field across all SDKs (#41)
- **python, go**: strip conversation_title in MetadataOnly content capture mode (#40)
- **go**: add ContentCaptureMode for SDK-level content stripping (#21)
- **plugins**: add Claude Code support (#16)

### Bug Fixes

- **sdk-go**: resolve auth headers in EvaluateHook (#100)

## [0.2.0] - 2026-04-03

### Breaking Changes

- **sdks**: remove SDK-managed trace transport config

### Features

- **eval**: update llm judge prompt context (#361)
- **plugin**: show sigil conversation titles in explore header (#287)
- **go-sdk**: preserve tool search variant provider types (#309)
- **conversations**: add user name to conversation search and detail (#262)
- add conversation title attribute across SDK, query, and plugin (#253)
- **go**: track deferred tools in sdk ingest and agent version hashing (#243)
- **sdks-go**: add basic auth mode to exporter (#197)
- **sdks-go**: add basic auth mode to exporter
- **sdks-go**: add sigil probe for grpc/http path checks (#193)
- **sdks**: add framework-native integrations for agents and ADK
- **sdks**: add embedding call observability across SDKs (#75)
- **sdks**: stamp sdk identity and remove devex runtime language attr
- **query**: ship conversation query path and plugin search UX
- add conversation ratings and annotations end-to-end
- deliver strict sdk parity and collector-first telemetry
- **sdk**: land strict openai parity and parity-gap execution plan
- **devex**: add core sdk traffic mega-emitter service
- add generation request controls across SDKs
- enforce tenant boundary and add per-export SDK auth
- **genai**: add optional agent name/version identity fields
- **go-sdk**: align genai otel conventions and add tool spans
- **go-sdk**: split generation input/output and add streaming start lifecycle
- **sdk-go**: auto-link generation records to active trace span
- bootstrap sigil monorepo with typed go sdk and provider helpers

### Bug Fixes

- **deps**: update module github.com/grafana/sigil/sdks/go to v0.1.2 (#655)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/anthropic to v0.1.2 (#656)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/openai to v0.1.2 (#659)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/gemini to v0.1.2 (#657)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/anthropic to v0.1.1 (#645)
- **deps**: update module github.com/grafana/sigil/sdks/go to v0.1.1 (#644)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/gemini to v0.1.1 (#646)
- **deps**: update module google.golang.org/genai to v1.51.0 (#648)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/openai to v0.1.1 (#635)
- **deps**: update module github.com/openai/openai-go/v3 to v3.29.0 (#637)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/anthropic to v0.1.1 (#633)
- **deps**: update module github.com/grafana/sigil/sdks/go-providers/gemini to v0.1.1 (#634)
- **deps**: update module github.com/grafana/sigil/sdks/go to v0.1.1 (#632)
- **deps**: update module github.com/anthropics/anthropic-sdk-go to v1.27.1 (#626)
- **deps**: update module github.com/openai/openai-go/v3 to v3.28.0 (#584)
- **deps**: update module google.golang.org/grpc to v1.79.3 [security] (#595)
- **deps**: update module google.golang.org/genai to v1.50.0 (#575)
- **tool-analytics**: separate tool labels from request models (#539)
- **google-adk**: populate RequestModel and RequestProvider in tool execution metrics (#507)
- **sdks**: stop leaking tool names into gen_ai.request.model metric (#502)
- **go**: complete tool call support across providers and framework
- **plugin,sigil**: show created dates for global templates and clean up eval UI (#435)
- **deps**: update module github.com/openai/openai-go/v3 to v3.26.0 (#384)
- **deps**: update opentelemetry-go monorepo to v1.42.0 (#404)
- **eval**: move validation to api layer and align error handling (#393)
- **deps**: update module google.golang.org/grpc to v1.79.2 (#376)
- **deps**: update module google.golang.org/genai to v1.49.0 (#360)
- **deps**: update opentelemetry-go monorepo to v1.41.0 (#306)
- restore conversation titles from storage metadata (#345)
- **deps**: update module google.golang.org/genai to v1.48.0 (#178)
- **sdks-go**: merge BasicUser and BasicPassword in auth config (#198)
- **anthropic**: accumulate content_block_delta events in stream mapper (#175)
- **deps**: update module github.com/openai/openai-go/v3 to v3.24.0
- **deps**: update module github.com/openai/openai-go/v3 to v3.23.0 (#129)
- **deps**: update module github.com/anthropics/anthropic-sdk-go to v1.26.0 (#108)
- **deps**: update module google.golang.org/genai to v1.47.0 (#115)
- **deps**: update module github.com/anthropics/anthropic-sdk-go to v1.25.0 (#103)
- **deps**: update module github.com/openai/openai-go/v3 to v3.22.0
- restore dashboard metrics visibility and filtering
- **deps**: update module google.golang.org/grpc to v1.79.1 (#63)
- **deps**: update module github.com/openai/openai-go/v3 to v3.21.0 (#62)
- **ci**: restore main and refresh SDK quick examples
- **openai**: support openai-go v3 tool union types
- **deps**: update module github.com/openai/openai-go to v3

### Documentation

- improve user onboarding and sdk licensing
- **go-providers**: add package godoc and runnable mapper examples
