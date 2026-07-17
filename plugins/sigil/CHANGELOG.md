# Changelog

## [0.19.0] - 2026-07-17

### Features

- **plugins/sigil**: rename sigil CLI to agento11y (#393)
- add AGENTO11Y_* env var aliases with SIGIL_* fallback (#395)

## [0.18.0] - 2026-07-13

_No user-facing changes._

## [0.17.0] - 2026-06-23

### Features

- **plugins/sigil**: add Settings tab to the local viewer (#345)

### Bug Fixes

- **plugins/sigil**: stop local daemons started with go run (#344)

## [0.16.0] - 2026-06-19

### Features

- **plugins/sigil**: prompt for content capture, tags, and guards in login (#335)

## [0.15.0] - 2026-06-18

### Features

- **plugins/cursor**: add tool-call guards (block + transform) (#306)

### Bug Fixes

- **plugins/sigil**: scope the headline model count to the time window (#333)

## [0.14.0] - 2026-06-18

### Features

- **plugins/sigil**: add doctor diagnostic command (#330)
- **plugins/sigil**: add sigil cursor install command (#326)
- **plugins/sigil**: improve design of the local conversation detail view (#331)
- **plugins/sigil**: add stats cards to the conversations list view (#327)
- **plugins/mistral-vibe**: add mistral vibe support (#329)
- **plugins**: add git.branch and cwd built-in tags to pi, opencode, codex, copilot (#328)
- **plugins/copilot**: apply transform/redact guards (#311)
- **plugins**: log when a guard redaction transform is applied (#322)
- **plugins/sigil**: chart token usage in the local viewer (#303)

## [0.13.0] - 2026-06-16

### Features

- **plugins/codex**: apply transform/redact guards (#305)
- **plugins/sigil**: add install script for prebuilt binaries (#298)

## [0.12.0] - 2026-06-12

### Features

- **plugins/claude**: apply transform/redact guards (#297)

## [0.11.0] - 2026-06-06

### Bug Fixes

- **plugins/sigil**: fix compilation on Windows (#291)

### Documentation

- **plugins**: add go install path for Linux and Windows (#289)

## [0.10.0] - 2026-06-05

### Bug Fixes

- **plugins/claude**: capture final assistant turn lost to transcript flush race (#287)

## [0.9.0] - 2026-06-04

### Features

- **plugins/sigil**: add --tag flag to set SIGIL_TAGS on launchers (#285)
- **experiments**: go SDK support for experiments API (#277)
- **plugins/copilot**: VS Code support, surface tracking, guard deny fix (#280)

## [0.8.0] - 2026-06-01

### Features

- **plugins**: send plugin User-Agent on generation export (#273)
- **plugins/sigil**: surface conversation title for Cursor sessions (#223)
- **plugins/sigil**: use first prompt as conversation title for Claude Code (#228)
- **plugins**: wrap guard deny messages with source, tool, and behavior hint (#260)

### Bug Fixes

- **security/unknown**: update module golang.org/x/net to v0.55.0 [security] (#237)

### Documentation

- clarify content capture modes across SDKs and plugins (#229)

## [0.7.0] - 2026-05-27

### Features

- **plugins/sigil**: add opencode launcher (#224)
- **plugins**: auto-update sigil plugins (#185)
- **plugins/codex**: support tool call guards (#213)
- **plugins/copilot**: support tool call guards (#214)

### Bug Fixes

- **plugins/sigil**: set service.instance.id per agent session (#218)

### Documentation

- **plugins**: lead with sigil launcher, hide manual install (#210)

## [0.6.0] - 2026-05-26

### Features

- **plugins/sigil**: add local capture mode (#186)

### Bug Fixes

- pin transitive dependencies (#208)
- **plugins/claude**: advance offset only after assistant response (#187)

## [0.5.0] - 2026-05-21

### Features

- **plugins/copilot**: add sigil copilot launcher (#181)
- **plugins/claude**: support SIGIL_GUARDS_* env vars in Claude Code plugin (#178)
- **plugins/copilot**: move copilot plugin into sigil single binary (#176)
- **plugins/codex**: add sigil codex command (#177)

## [0.4.0] - 2026-05-20

### Features

- **plugins/claude**: preserve Claude Code offsets on empty batches (#175)
- **plugins**: add interactive login flow for sigil (#172)
- **plugins**: add copilot plugin (#164)

### Bug Fixes

- **sdk/go**: surface async export failures through Flush() (#171)

### Documentation

- **plugins**: switch install instructions to brew and simplify (#174)

## [0.3.0] - 2026-05-20

### Features

- **plugins/sigil**: add `sigil claude` launcher with plugin bootstrap (#167)
- **plugins/sigil**: add pi launcher subcommand (#166)

## [0.2.0] - 2026-05-19

### Features

- **plugins**: consolidate claude-code, codex, cursor plugin helpers into single binary (#163)
