# Changelog

## [0.31.0] - 2026-08-18

### Features

- **plugins/agento11y**: add local viewer filtering and grouping (#597)

### Documentation

- update coding agents o11y documentation (#598)

## [0.30.0] - 2026-08-18

### Features

- **plugins/agento11y**: show commit hash for dev versions (#593)
- **plugins/agento11y**: add CSP to local viewer (#595)
- **plugins/agento11y**: require JSON media types for local writes (#591)
- **plugins/agento11y**: route hook captures to local mode (#588)
- **plugins/agento11y**: refresh local viewer design (#589)
- **plugins/agento11y**: add noninteractive managed install (#570)

### Bug Fixes

- **plugins/agento11y**: fix Cursor conversation titles (#590)

## [0.29.0] - 2026-08-17

### Features

- **plugins/agento11y**: update login form in the local viewer (#581)

## [0.28.0] - 2026-08-17

### Breaking Changes

- bump default timeout to 30s and support overriding via env var (#566)

### Features

- **plugins/agento11y**: redesign local conversation viewer (#569)
- **plugins/agento11y**: import Cursor chat history (#568)
- **plugins/agento11y**: bundle a setup skill in the binary (#546)
- **plugins**: honour AGENTO11Y_REDACT_INPUT_MESSAGES in every coding agent plugin (#563)

### Bug Fixes

- **plugins/agento11y**: add prompt redaction row to test expectation (#567)

## [0.27.0] - 2026-08-11

### Features

- **plugins/agento11y**: accept credentials block in login (#554)

## [0.26.0] - 2026-08-10

### Features

- **plugins/agento11y**: export agent reported cost as cost_usd everywhere (#550)

### Bug Fixes

- **plugins/agento11y**: stamp local conversation files with their last activity (#556)

### Performance

- **plugins/agento11y**: cache per-file summaries for the local viewer (#555)

## [0.25.0] - 2026-08-06

### Features

- **plugins**: add opt-in AGENTO11Y_AUTO_TAGS for user, repo, and branch (#543)
- **plugins/agento11y**: honour AGENTO11Y_AGENT_NAME in coding agent plugins (#544)
- **redaction**: generate secret patterns from one table and pin cross-SDK parity (#539)
- **plugins/agento11y**: import pi session history (#535)

## [0.24.0] - 2026-08-04

### Features

- **plugins/agento11y**: check credentials and accept flags in login (#530)
- **plugins/agento11y**: import Claude Code and Codex session history (#531)

### Bug Fixes

- **plugins/agento11y**: improve doctor endpoint checks (#520)
- **plugins/agento11y**: separate 401 and 403 in doctor --probe (#508)
- **plugins/agento11y**: give Claude Code spans real time windows (#519)

### Performance

- **plugins/agento11y**: stop decoding all files on each local viewer request (#521)

### Documentation

- add Vibe to agent list and fix auto-update docs (#507)

## [0.23.0] - 2026-08-03

### Features

- **plugins/agento11y**: add a vibe probe to doctor (#500)
- **plugins/agento11y**: redact secrets in Cursor content (#501)
- **plugins/agento11y**: enable local mode with AGENTO11Y_LOCAL env var (#499)
- **plugins/agento11y**: support Cloud guards in --local mode (#487)
- **plugins/agento11y**: opt-in Cloud forwarding for local sessions (#484)

### Bug Fixes

- **security/unknown/plugins/agento11y**: update module go.opentelemetry.io/otel to v1.44.0 [security] (#464)
- **plugins/agento11y**: correct doctor version, alignment, and agent install state (#505)

## [0.22.0] - 2026-07-28

### Features

- **plugins**: local viewer UI improvements (#475)

## [0.21.0] - 2026-07-28

### Features

- **plugins**: migrate legacy sigil plugin installs to agento11y (#447)

### Bug Fixes

- **security/high/plugins/agento11y**: update module google.golang.org/grpc to v1.82.1 [security] (#442)

### Documentation

- AI Observability -> Agent Observability rename (#450)
- sigil -> agento11y rename in docs, examples and errors (#415)

## [0.20.0] - 2026-07-21

### Breaking Changes

- rename public SDK APIs from sigil to agento11y (#403)

### Features

- **plugins**: rename launcher state folder to agento11y (#404)
- **plugins**: move CLI config to ~/.config/agento11y/config.env (#401)
- **sdk**: rename attributes from sigil.* to agento11y.* (#392)
- rename package identities from sigil-sdk to agento11y (#397)

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
