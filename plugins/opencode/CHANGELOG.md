# Changelog

## [0.14.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **plugins**: rename launcher state folder to agento11y (#404)
- **plugins**: move CLI config to ~/.config/agento11y/config.env (#401)
- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)
- **plugins/sigil**: rename sigil CLI to agento11y (#393)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)

## [0.13.0] - 2026-07-14

### Features

- **plugins/opencode**: capture system prompt (#382)
- **plugins**: add git.branch and cwd built-in tags to pi, opencode, codex, copilot (#328)

### Bug Fixes

- **plugins/opencode**: apply guard tool-call argument transforms (#379)
- **plugins/opencode**: link subagent sessions to the spawning parent (#378)
- **sigil-sdk**: address fixable npm CVEs (#366)
- **plugins/opencode**: record error spans for tools that never complete (#317)

## [0.12.0] - 2026-06-16

### Features

- **plugins/sigil**: add install script for prebuilt binaries (#298)

### Bug Fixes

- **plugins/opencode**: stop re-exporting session history as new generations (#315)

### Documentation

- **plugins**: add go install path for Linux and Windows (#289)

## [0.11.0] - 2026-06-01

### Features

- **plugins**: send plugin User-Agent on generation export (#273)

## [0.10.0] - 2026-05-29

### Features

- **plugins**: wrap guard deny messages with source, tool, and behavior hint (#260)
- **plugins/opencode**: emit tool execution spans (#252)

### Documentation

- clarify content capture modes across SDKs and plugins (#229)

## [0.9.0] - 2026-05-28

_No user-facing changes._

## [0.8.0] - 2026-05-28

### Features

- **plugin**: adding opencode guard support (#219)
- **plugins/opencode**: export OTel traces and metrics via OTLP (#227)
- **plugins/sigil**: add opencode launcher (#224)

### Bug Fixes

- **plugins**: accept all ContentCaptureMode values in opencode and pi (#230)

## [0.7.0] - 2026-05-27

### Features

- **plugins/opencode**: read shared ~/.config/sigil/config.env (#221)

### Documentation

- **plugins**: switch install instructions to brew and simplify (#174)

## [0.6.0] - 2026-05-16

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)

### Documentation

- align SDK READMEs and examples to SIGIL_* env vars (#133)

## [0.5.0] - 2026-05-08

### Features

- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)

## [0.4.0] - 2026-05-01

_No user-facing changes._

## [0.3.1] - 2026-04-29

_No user-facing changes._

## [0.3.0] - 2026-04-29

_No user-facing changes._

## [0.2.0] - 2026-04-29

### Features

- **plugins**: publish pi and opencode to npm under @grafana scope (#86)
- add opencode plugin (#519)

### Documentation

- add OTel setup guidance, Cloud OTLP options, and telemetry to examples (#78)
- add parent_generation_ids to OpenCode instrumentation skill (#43)

