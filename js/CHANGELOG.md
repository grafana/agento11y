# Changelog

## [0.12.0] - 2026-08-17

### Breaking Changes

- bump default timeout to 30s and support overriding via env var (#566)

### Features

- **plugins**: honour AGENTO11Y_REDACT_INPUT_MESSAGES in every coding agent plugin (#563)

### Bug Fixes

- **sdk**: restore legacy environment fallback (#573)

## [0.11.0] - 2026-08-11

### Features

- **frameworks**: add conversation_id option and document OTel provider setup (#559)

### Bug Fixes

- **frameworks**: record chat-start request params in the JS handler (#560)
- **frameworks**: record system prompts from LangChain/LangGraph chat messages (#558)

## [0.10.0] - 2026-08-05

### Features

- **providers**: normalize provider usage to the inclusive contract (phase 1: providers) (#533)
- **redaction**: generate secret patterns from one table and pin cross-SDK parity (#539)
- **core**: carry TokenInputSemantics through models and mark telemetry (phase 1: core) (#532)
- **proto**: TokenInputSemantics marker on TokenUsage (OTel-inclusive contract, phase 0) (#529)
- **sdk/js**: add experiments with cloud evaluation support (#518)
- **sdk**: send new X-Agento11y-* request headers (#469)
- correlate guard decisions with conversations (#466)

### Bug Fixes

- **deps**: update dependency @google/adk to ^0.6.0 (#248)
- **sdk**: derive SDK versions from build metadata instead of constants (#445)

### Documentation

- AI Observability -> Agent Observability rename (#450)
- sigil -> agento11y rename in docs, examples and errors (#415)

## [0.9.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)

## [0.8.2] - 2026-07-16

### Bug Fixes

- **sdk-js**: support Vercel AI SDK v6 prepareStep (#388)
- **sdk**: non-canonical provider hint no longer blocks Bedrock model inference (#377)

## [0.8.1] - 2026-07-13

### Features

- **go,js,python**: workflow-step ingest with cross-language parity (#361)

### Bug Fixes

- **js**: backfill Bedrock inference-profile model onto generation (#375)
- **sigil-sdk**: address fixable npm CVEs (#366)

### Documentation

- **examples**: reorganize and add TypeScript guards example (#337)

## [0.8.0] - 2026-06-12

### Features

- **sdk**: promote SIGIL_TAGS onto OTel spans and metrics (#284)
- **sdk**: add SIGIL_REDACT_INPUT_MESSAGES env support (#257)

## [0.7.0] - 2026-06-01

### Features

- **sdk**: add User-Agent to generation traffic (#266)
- **sdk/go**: expose importable generation packages (#188)

### Documentation

- add hooks/guards SDK docs and Go/Python examples (#259)

## [0.6.0] - 2026-05-27

### Features

- **js**: add slim core SDK package (#226)
- **sdk-js**: capture Output.object schema into Generation.tools (#179)
- **sdks**: add full_with_metadata_spans content capture mode (#168)
- **plugins/pi**: add Sigil guards support (#161)
- **sdk**: cache diagnostics metadata helpers (#150)
- **sdk**: record metrics while span is active for exemplar support (#162)

### Bug Fixes

- pin transitive dependencies (#208)
- **sdk**: add agent version metric labels (#170)

## [0.5.0] - 2026-05-16

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)

## [0.4.0] - 2026-05-11

### Features

- auto-append /api/v1/generations:export to HTTP endpoint (#131)
- high-level executeToolCalls API (issue #127) (#135)
- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)

### Bug Fixes

- **proto**: add WorkflowStep schema to protobuf  (#132)

## [0.3.2] - 2026-05-08

### Features

- **proto**: regenerate protos with effective_version (#120)

### Bug Fixes

- **js**: bump package.json version to 0.3.1 (#128)

## [0.3.1] - 2026-05-07

### Features

- **hooks**: support transformed_input from hooks:evaluate (#108)

### Bug Fixes

- **js/ts**: cloudflare workers runtime compatibility (#118)

## [0.3.0] - 2026-05-01

_No user-facing changes._

## [0.2.1] - 2026-05-01

### Features

- **sdk**: canonical SIGIL_* env-var schema in Go/JS/Python clients (#103)
- **sdk**: synchronous hook evaluation (TS / Go / Python) (#83)

## [0.2.0] - 2026-04-29

### Breaking Changes

- **sdks**: remove SDK-managed trace transport config

### Features

- strands sdk typescript support (#94)
- apply OTel GenAI token usage histogram buckets across SDKs (#93)
- add pii redaction based on gitleaks shared util for js sdks (#47)
- apply OTel GenAI duration histogram buckets across SDKs (#85)
- **js**: add content capture mode (full / no_tool_content / metadata_only) (#38)
- add parent_generation_ids field across all SDKs (#41)
- **js**: add biome linter and formatter (#24)
- TreeView (#229)
- **sdks-js**: add Vercel AI SDK integration (#118)
- **sdks**: add framework-native integrations for agents and ADK
- **sdk**: enrich framework callback context and spans
- **sdk**: propagate framework thread ids into conversations and spans
- **sdks**: add langchain/langgraph framework integrations
- **sdks**: add embedding call observability across SDKs (#75)
- **sdks**: stamp sdk identity and remove devex runtime language attr
- **query**: ship conversation query path and plugin search UX
- add conversation ratings and annotations end-to-end
- deliver strict sdk parity and collector-first telemetry
- **sdk**: land strict openai parity and parity-gap execution plan
- **devex**: add core sdk traffic mega-emitter service
- add generation request controls across SDKs
- enforce tenant boundary and add per-export SDK auth
- **sdk**: Add typescript/js (#23)
- bootstrap sigil monorepo with typed go sdk and provider helpers

### Bug Fixes

- **deps**: update dependency @anthropic-ai/sdk to ^0.81.0 [security] (#13)
- **js**: add explicit rootDir for TypeScript 6 compatibility (#18)
- **ci**: resolve build failures in TS, Java, and .NET jobs
- **deps**: update dependency @openai/agents to ^0.8.0 (#679)
- **deps**: update dependency @anthropic-ai/sdk to ^0.80.0 (#630)
- **deps**: update dependency @anthropic-ai/sdk to ^0.79.0 (#610)
- **deps**: update dependency @openai/agents to ^0.7.0 (#560)
- **tool-analytics**: separate tool labels from request models (#539)
- **deps**: update dependency @google/adk to ^0.5.0 (#525)
- **sdks**: stop leaking tool names into gen_ai.request.model metric (#502)
- preserve SDK tool-result parity and expose embeddings support
- **deps**: update dependency @openai/agents to ^0.6.0 (#450)
- **deps**: update opentelemetry-js monorepo (#353)
- **deps**: update dependency @google/adk to ^0.4.0 (#171)
- **sdks-js**: avoid deprecated langgraph-sdk resolution (#167)
- **deps**: update dependency @openai/agents to ^0.5.0 (#133)
- **deps**: update dependency @anthropic-ai/sdk to ^0.78.0 (#111)
- **deps**: update dependency @langchain/langgraph to v1 (#100)
- **deps**: update dependency @anthropic-ai/sdk to ^0.77.0 (#102)
- **deps**: update dependency @langchain/core to v1 (#99)
- **sdk**: harden framework TTFT and share python runtime
- **deps**: update opentelemetry-js monorepo (#65)

### Documentation

- improve user onboarding and sdk licensing
