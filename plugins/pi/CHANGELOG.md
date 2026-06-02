# Changelog

## [0.16.0] - 2026-06-01

### Features

- **plugins**: send plugin User-Agent on generation export (#273)

## [0.15.0] - 2026-05-29

### Features

- **plugins/pi**: set a conversation title on exported generations (#262)

## [0.14.0] - 2026-05-29

### Features

- **plugins**: wrap guard deny messages with source, tool, and behavior hint (#260)

## [0.13.0] - 2026-05-28

### Bug Fixes

- **plugins/pi**: write diagnostics to the debug log file, not the terminal (#256)

## [0.12.0] - 2026-05-28

### Features

- **plugins/pi**: emit deterministic generation IDs and parent links (#254)

### Documentation

- clarify content capture modes across SDKs and plugins (#229)

## [0.11.0] - 2026-05-27

### Features

- **plugins/opencode**: export OTel traces and metrics via OTLP (#227)

### Bug Fixes

- **plugins**: accept all ContentCaptureMode values in opencode and pi (#230)

## [0.10.0] - 2026-05-27

### Features

- **plugins/pi**: capture systemPrompt and tool schemas (#192)
- **plugins/claude**: support SIGIL_GUARDS_* env vars in Claude Code plugin (#178)
- **plugins/pi**: read config.env on startup (#173)
- **plugins/pi**: add Sigil guards support (#161)
- **plugins/sigil**: add pi launcher subcommand (#166)

### Bug Fixes

- **plugins/pi**: report host pi version, not stale peer copy (#193)

### Documentation

- **plugins**: lead with sigil launcher, hide manual install (#210)
- **plugins**: switch install instructions to brew and simplify (#174)

## [0.9.0] - 2026-05-16

### Features

- **plugins/pi**: tag generations with git.branch (#148)

### Bug Fixes

- use cache_write_input_tokens instead of cache_creation_input_tokens (#151)

### Documentation

- align SDK READMEs and examples to SIGIL_* env vars (#133)

## [0.8.0] - 2026-05-12

### Bug Fixes

- **plugins/pi**: tag OTel resource with per-session service.instance.id (#144)
- **plugins/pi**: record LLM turns as streaming generations (#136)

## [0.7.0] - 2026-05-08

### Features

- **sdk**: plumb effective_version through SDKs and coding agent plugins (#124)

### Bug Fixes

- **plugins/pi**: silence telemetry flush failures (#119)

### Documentation

- **plugins**: fix claude-code env vars and add cloud setup walkthrough (#112)

## [0.6.0] - 2026-05-05

### Bug Fixes

- **plugins/pi**: fix sending OTel telemetry (#109)

## [0.5.1] - 2026-05-05

### Bug Fixes

- **plugins/pi**: use sessionId as conversation id (#106)

## [0.5.0] - 2026-05-01

### Features

- **plugins/pi**: add PII redaction via SDK secret redaction sanitizer (#98)

## [0.4.0] - 2026-04-30

### Features

- **plugins/pi**: capture user input in Sigil generations (#96)

## [0.3.1] - 2026-04-29

_No user-facing changes._

## [0.3.0] - 2026-04-29

_No user-facing changes._

## [0.2.0] - 2026-04-29

### Features

- **plugins**: publish pi and opencode to npm under @grafana scope (#86)
- **plugins**: add pi plugin (#76)

