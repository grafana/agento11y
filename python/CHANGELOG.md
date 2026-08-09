# Changelog

## [0.13.0] - 2026-08-05

### Features

- **providers**: normalize provider usage to the inclusive contract (phase 1: providers) (#533)
- **redaction**: generate secret patterns from one table and pin cross-SDK parity (#539)
- **python-frameworks/litellm**: apply hook transforms to the request (#515)
- **core**: carry TokenInputSemantics through models and mark telemetry (phase 1: core) (#532)
- **proto**: TokenInputSemantics marker on TokenUsage (OTel-inclusive contract, phase 0) (#529)
- **sdk/js**: add experiments with cloud evaluation support (#518)
- **python-frameworks/litellm**: support postflight guards (#498)
- **sdk**: gate cloud trial evaluation behind an experimental opt-in (#496)
- **sdk/python**: add cloud evaluation support for experiments (#485)
- **python-frameworks/litellm**: support preflight guards (#486)
- **python-frameworks/litellm**: support /v1/responses and /v1/messages routes (#489)

### Bug Fixes

- **python-frameworks/litellm**: correct agent identity, output, and grouping (#534)
- **security/high/python-frameworks/litellm**: update dependency aiohttp to v3.14.3 [security] (#526)
- **python-frameworks/litellm**: stop logging unevaluated requests as guards success (#497)

## [0.12.0] - 2026-07-27

### Breaking Changes

- **python-frameworks/litellm**: resolve agent_name from LiteLLM's agent_id (#470)

### Features

- **sdk**: send new X-Agento11y-* request headers (#469)
- correlate guard decisions with conversations (#466)
- **experiments**: python test suite support and general hardening (#453)

### Documentation

- sigil -> agento11y rename in docs, examples and errors (#415)

## [0.10.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)
- **skills**: rename eval-starter and experiments skills to agento11y (#394)
- **sdk**: emit client tags on OTel spans and metrics in Python, Java, and .NET (#385)
- sigil-eval-starter skill + agent_version propagation fix (#367)

### Bug Fixes

- **security/high/python**: update dependency setuptools to v83 [security] (#381)
- **deps**: require aiohttp for the LiteLLM framework (#402)
- **sdk**: non-canonical provider hint no longer blocks Bedrock model inference (#377)

## [0.9.3] - 2026-07-13

### Features

- **go,js,python**: workflow-step ingest with cross-language parity (#361)

### Bug Fixes

- **python**: backfill Bedrock inference-profile model onto generation (#371)

## [0.9.2] - 2026-07-07

_No user-facing changes._

## [0.9.1] - 2026-07-07

### Features

- **python**: experiments v1 support (#357)
- **python**: adding claude agent SDK support (#353)

### Bug Fixes

- **python**: nest executor spans under the execute_tool span (#362)

## [0.9.0] - 2026-06-12

### Features

- datasets from conversation collections prompt only (#290)
- **sdk**: add SIGIL_REDACT_INPUT_MESSAGES env support (#257)

### Bug Fixes

- **python-frameworks/google-adk**: fix compatibility (#278)

## [0.8.0] - 2026-06-01

### Features

- **experiments**: python SDK support for experiments API (#268)

## [0.7.0] - 2026-06-01

### Features

- **frameworks/litellm**: capture reasoning (#265)

## [0.6.0] - 2026-06-01

### Features

- **sdk**: add User-Agent to generation traffic (#266)
- **litellm**: record embeddings as OTel spans (#264)
- **sdk/go**: expose importable generation packages (#188)

## [0.5.0] - 2026-05-27

### Features

- **sdks**: add full_with_metadata_spans content capture mode (#168)
- **sdk**: cache diagnostics metadata helpers (#150)
- **sdk**: record metrics while span is active for exemplar support (#162)

### Bug Fixes

- pin transitive dependencies (#208)
- **sdk**: add agent version metric labels (#170)

## [0.4.0] - 2026-05-16

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)
- **sdk-py**: wire ToolDefinition.deferred through hooks and generation export (#153)

### Documentation

- align SDK READMEs and examples to SIGIL_* env vars (#133)

## [0.3.0] - 2026-05-11

### Features

- auto-append /api/v1/generations:export to HTTP endpoint (#131)

## [0.2.1] - 2026-05-11

### Features

- high-level executeToolCalls API (issue #127) (#135)
- **sdk-python**: add workflow step ingest pipeline (#117)
- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)
- **proto**: regenerate protos with effective_version (#120)
- **hooks**: support transformed_input from hooks:evaluate (#108)

### Documentation

- **examples**: add dependency graph getting-started example (#134)

## [0.2.0] - 2026-05-01

### Features

- **sdk**: canonical SIGIL_* env-var schema in Go/JS/Python clients (#103)
- **sdk**: synchronous hook evaluation (TS / Go / Python) (#83)

## [0.1.5] - 2026-04-29

### Features

- apply OTel GenAI token usage histogram buckets across SDKs (#93)
- Strands SDK python support (#88)
- apply OTel GenAI duration histogram buckets across SDKs (#85)
- add pii redaction gitleaks shared util for python sdks (#49)

### Bug Fixes

- Gemini langchain SDK tool calling properly recorded (#89)

## [0.1.4] - 2026-04-29

### Features

- **python**: Add Pydantic AI framework integration (#74)

### Bug Fixes

- **ci**: use GitHub App token for Python SDK publish push (#63)

## [0.1.3] - 2026-04-17

### Features

- add parent_generation_ids field across all SDKs (#41)
- **python, go**: strip conversation_title in MetadataOnly content capture mode (#40)
- **python**: add ContentCaptureMode for SDK-level content stripping (#22)
- **python**: add ruff linter and formatter (#23)
- **sdk**: add LiteLLM integration package (#15)

### Bug Fixes

- **sigil-sdk-openai**: relax openai dependency to >=1.66.0 (#62)
- **deps**: update dependency pytest to v9.0.3 [security] (#37)
- **langgraph**: unwrap ToolMessage before serialization in on_tool_end (#46)

## [0.1.2] - 2026-03-24

### Bug Fixes

- **sdk-python**: lowercase gRPC metadata keys to avoid illegal header errors (#614)

## [0.1.1] - 2026-03-20

### Breaking Changes

- **sdks**: remove SDK-managed trace transport config

### Features

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
- **sdk-python**: deliver python sdk parity runtime and providers
- bootstrap sigil monorepo with typed go sdk and provider helpers

### Bug Fixes

- **langchain**: extract ToolMessage content before passing to base SDK (#596)
- **sdks**: fall back to Anthropic-style keys in LangChain framework handler (#546)
- **tool-analytics**: separate tool labels from request models (#539)
- **sdks**: stop leaking tool names into gen_ai.request.model metric (#502)
- preserve SDK tool-result parity and expose embeddings support
- **sdk**: harden framework TTFT and share python runtime

### Documentation

- improve user onboarding and sdk licensing
