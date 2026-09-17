# Changelog

## Unreleased

These are monorepo migration changes, not part of the historical PyPI `0.10.0` release.

- Move the plugin to https://github.com/grafana/agento11y/tree/main/plugins/hermes with independent packaging and checks. Release automation remains inactive.
- Upgrade to Python SDK 0.17.x and default to `metadata_only`. Shared secret redaction protects captured content and tool-execution spans.
- Make automatic user/repo/branch tags opt-in and remove unconditional `cwd` tagging.
- Keep the package name, Hermes entry point, and supported configuration. Shared launcher integration remains separate work.

## [0.10.0] - 2026-08-16

Historical notes restored from the original GitHub release:

- Wrap every hook handler in a fail-open guard (b9ec134)

## [0.9.0] - 2026-08-16

- Update the end-to-end tests for the new capture and fix false passes
- Record max_tokens, temperature, top_p and tool_choice from request body
- implement system prompt capture mode for hook payloads
- Tag tool spans and link them to the generation that requested them
- Document content capture mode
- Add end-to-end tests and update the hermes notes from them
- update hermes version compatibility
- Update Dependabot
- Update README.md
- Update README.md

## [0.6.0] - 2026-08-15

- Publish releases to PyPI
- Point setup docs at the Agent Observability setup page
- Switch to the agento11y SDK and rename to grafana-agento11y-hermes

## [0.5.0] - 2026-08-15

- Manage the project with uv and take the version from git tags

## [0.4.0] - 2026-06-07

- bump sigil-sdk to 0.8.0
- Send plugin User-Agent on generation export
- fix Grafana Cloud path
- add screenshot

## [0.3.0] - 2026-06-07

- Derive OTLP auth headers from Sigil creds when unset

## [0.2.0] - 2026-06-07

- Let the SDK derive tool-execution content capture
- Add SIGIL_HERMES_AGENT_VERSION
- sigil-sdk 0.5.0
- Rename project from hermes-plugin-sigil to sigil-hermes
- llms.txt: point the token step at the in-stack setup URL
- Make the install verify recipe actually work
- Change URL pattern for AI Observability
- Clarify Grafana AI Observability plugin instructions (#2)
- Update README to correct plugin description
- Add llms.txt
- hooks: scope recorder→assistant pairing to this turn

## [0.1.0] - 2026-05-01

- otel: drop SIGIL_OTEL_* schema, use standard OTEL_* envs
- config: adopt canonical SIGIL_* env-var schema
- hooks: move all tool work to post_tool_call
- hooks: bound generation span and duration to the LLM call window
- hooks: drop redundant set_result(input=...) at pre-hook time
- hooks: catch prep errors when closing pending generation recorders
- hooks: thread cfg.max_chars into _redact.safe_value
- redact: cap before materialization in safe_value
- initial commit
