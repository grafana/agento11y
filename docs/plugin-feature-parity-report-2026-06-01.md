# Plugin Feature Parity Report - 2026-06-01

Automated audit of Claude Code, Codex, Cursor, OpenCode, and Pi plugin parity.

## Method

- Fetched `origin/main`.
- Listed plugin files with `git ls-tree -r --name-only origin/main -- plugins/<name>/`.
- Read the plugin package files plus delegated shared-binary agent implementations from `origin/main` with `git show origin/main:<path>`.
- Verified no audited plugin paths differ from `origin/main` on branch `cursor/plugin-feature-parity-audit-bf6c`.
- Checked open enhancement issues with `gh issue list --repo grafana/sigil-sdk --state open --label enhancement --limit 200`; no open enhancement issues were returned.

This environment does not expose a write-capable GitHub issue tool, and repository instructions restrict `gh` to read-only operations. The report below is formatted as the issue body that should be filed.

## Plugin Feature Parity Report

### Parity matrix

| Category | Feature | Claude Code | Codex | Cursor | OpenCode | Pi |
|----------|---------|:-----------:|:-----:|:------:|:--------:|:--:|
| Packaging | Thin host plugin delegates to shared `sigil` launcher/binary | ✅ | ✅ | ✅ | ➖ | ➖ |
| Packaging | In-process TypeScript plugin/extension | ➖ | ➖ | ➖ | ✅ | ✅ |
| Launcher | `sigil <agent>` launcher | ✅ | ✅ | ➖ | ✅ | ✅ |
| Launcher | Auto-install host plugin | ✅ | ✅ | ➖ | ✅ | ✅ |
| Launcher | Periodic auto-update/refresh | ✅ | ✅ | ➖ | ✅ | ➖ |
| Launcher | `--local` endpoint/OTLP bootstrap with placeholder auth | ✅ | ✅ | ➖ | ✅ | ✅ |
| Config | Shared `~/.config/sigil/config.env` loading | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | OS environment wins over dotenv | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | `SIGIL_CONTENT_CAPTURE_MODE` | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Capture modes: `metadata_only`, `full`, `no_tool_content`, `full_with_metadata_spans` | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | `metadata_only` default | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | `SIGIL_TAGS` custom tags | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | `SIGIL_USER_ID` override | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Basic export auth | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Tenant-only or unauthenticated export auth for non-local endpoints | ❌ | ❌ | ❌ | ✅ | ❌ |
| Config | Prefix/trailing-slash-safe export endpoint normalization | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Debug logging option | ✅ | ✅ | ✅ | ✅ | ✅ |
| Lifecycle | Transcript replay export | ✅ | ➖ | ➖ | ➖ | ➖ |
| Lifecycle | Fragment/event assembly across hook invocations | ➖ | ✅ | ✅ | ➖ | ➖ |
| Lifecycle | In-memory event assembly | ➖ | ➖ | ➖ | ✅ | ✅ |
| Lifecycle | Stop/terminal-message generation export | ✅ | ✅ | ✅ | ✅ | ✅ |
| Lifecycle | Session-end or idle sweep for stranded work | ✅ | ➖ | ✅ | ✅ | ➖ |
| Lifecycle | Pending retry preservation after export/flush failure | ✅ | ✅ | ✅ | ➖ | ➖ |
| Lifecycle | Stale on-disk state cleanup | ❌ | ✅ | ❌ | ➖ | ➖ |
| Telemetry | Generation export to `/api/v1/generations:export` | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | OpenTelemetry traces/metrics provider setup | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Per-tool `execute_tool` spans | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Streaming generation mode / TTFT metric | ➖ | ➖ | ➖ | ❌ | ✅ |
| Telemetry | Token usage totals | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Cache read token usage | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Cache write token usage | ✅ | ➖ | ✅ | ✅ | ✅ |
| Telemetry | Reasoning token usage | ➖ | ✅ | ➖ | ✅ | ✅ |
| Telemetry | Cost metadata | ➖ | ➖ | ➖ | ✅ | ✅ |
| Mapping | Deterministic generation IDs from host IDs | ✅ | ✅ | ✅ | ❌ | ✅ |
| Mapping | Provider response ID when host exposes one | ❌ | ➖ | ➖ | ➖ | ✅ |
| Mapping | Conversation title | ✅ | ❌ | ✅ | ❌ | ✅ |
| Mapping | Provider/model inference or mapping | ✅ | ✅ | ✅ | ✅ | ✅ |
| Mapping | Stop reason/status mapping | ✅ | ✅ | ✅ | ✅ | ✅ |
| Mapping | Structured LLM call error classification | ❌ | ❌ | ✅ | ✅ | ✅ |
| Mapping | Tool definitions/catalog | ✅ | ✅ | ✅ | ✅ | ✅ |
| Mapping | Rich tool definitions with descriptions/schemas when host exposes them | ➖ | ➖ | ➖ | ➖ | ✅ |
| Mapping | Request controls (`temperature`, `top_p`, `tool_choice`, thinking budget) | ➖ | ➖ | ➖ | ➖ | ✅ |
| Messages | User prompt capture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Messages | Assistant text capture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Messages | System prompt capture when host exposes it | ➖ | ➖ | ➖ | ✅ | ✅ |
| Messages | Thinking/reasoning content capture when host exposes it and capture mode allows it | ❌ | ➖ | ➖ | ✅ | ✅ |
| Messages | Tool call/result capture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Messages | `no_tool_content` hides tool bodies while preserving tool structure | ✅ | ✅ | ✅ | ✅ | ✅ |
| Privacy | User input redaction in full-content mode | ✅ | ✅ | ❌ | ❌ | ✅ |
| Privacy | Assistant/tool redaction in full-content mode | ✅ | ✅ | ❌ | ✅ | ✅ |
| Privacy | Structured JSON sensitive-key redaction | ✅ | ✅ | ❌ | ✅ | ✅ |
| Privacy | Tool argument/result truncation or bounding | ✅ | ✅ | ❌ | ❌ | ➖ |
| Guards | Tool-call guard evaluation | ✅ | ✅ | ➖ | ✅ | ✅ |
| Guards | Host-specific deny response/blocking behavior | ✅ | ✅ | ➖ | ✅ | ✅ |
| Guards | Multiple permission/guard surfaces | ➖ | ➖ | ➖ | ✅ | ❌ |
| Identity | Host-derived user identity | ✅ | ➖ | ✅ | ➖ | ➖ |
| Tags | Built-in `git.branch` tag | ✅ | ❌ | ✅ | ❌ | ❌ |
| Tags | Built-in `cwd` / workspace context tag | ✅ | ✅ | ✅ | ❌ | ❌ |
| Tags | Entrypoint/source tag | ✅ | ✅ | ❌ | ❌ | ❌ |
| Subagents | Parent generation linking | ✅ | ✅ | ➖ | ➖ | ✅ |
| Subagents | Background/subagent tag when parent link is unavailable | ✅ | ❌ | ✅ | ➖ | ➖ |
| Resilience | Missing credentials fail safely without crashing host | ✅ | ✅ | ✅ | ✅ | ✅ |
| Resilience | Hook/plugin errors swallowed so host agent continues | ✅ | ✅ | ✅ | ✅ | ✅ |
| Testing | Golden export fixture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Testing | Subagent/lineage golden or dedicated lineage tests | ✅ | ✅ | ❌ | ❌ | ✅ |
| Testing | Guard integration tests | ✅ | ✅ | ➖ | ✅ | ✅ |
| Testing | OTLP/tool-span integration tests | ✅ | ✅ | ✅ | ✅ | ✅ |

### Gaps by plugin

#### Claude Code

- [ ] **Non-basic export auth modes** - Claude Code hard-codes basic auth for generation export. OpenCode supports tenant-only and unauthenticated export modes. Present in: OpenCode. Reference: `plugins/opencode/src/config.ts`.
- [ ] **Provider response ID mapping** - Claude Code transcript assistant messages include provider/message identifiers, but exports do not populate `response_id`. Present in: Pi. Reference: `plugins/pi/src/testdata/golden/pi-full-turn.golden.json`.
- [ ] **Thinking/reasoning content capture** - Claude Code emits a placeholder for thinking blocks even in full capture mode, while TS plugins export host-exposed thinking/reasoning content subject to capture mode and sanitization. Present in: OpenCode, Pi. Reference: `plugins/opencode/src/mappers.ts`, `plugins/pi/src/mappers.ts`.
- [ ] **Structured LLM call error classification** - Claude Code filters some socket-error transcript markers but does not map provider/API/auth/abort errors into structured call errors. Present in: Cursor, OpenCode, Pi. Reference: `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/opencode/src/mappers.ts`, `plugins/pi/src/mappers.ts`.
- [ ] **Stale state cleanup** - Claude Code persists per-session transcript offsets without time-based cleanup. Present in: Codex. Reference: `plugins/sigil/internal/agents/codex/fragment/fragment.go`.

#### Codex

- [ ] **Non-basic export auth modes** - Codex hard-codes basic auth for generation export. OpenCode supports tenant-only and unauthenticated export modes. Present in: OpenCode. Reference: `plugins/opencode/src/config.ts`.
- [ ] **Conversation title** - Codex captures the user prompt but does not derive or export a conversation title. Present in: Claude Code, Cursor, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/hook/beforesubmit.go`, `plugins/pi/src/mappers.ts`.
- [ ] **Built-in `git.branch` tag** - Codex has cwd/session context but does not add a git branch tag. Present in: Claude Code, Cursor, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`, `plugins/pi/src/git.ts`.
- [ ] **Structured LLM call error classification** - Codex maps stop status and token usage but does not classify provider/API/auth/abort call errors. Present in: Cursor, OpenCode, Pi. Reference: `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/opencode/src/mappers.ts`, `plugins/pi/src/mappers.ts`.
- [ ] **Background/subagent tag fallback** - Codex implements parent linking when spawn metadata resolves but does not tag unresolved child/background turns as subagent work. Present in: Claude Code, Cursor. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`.

#### Cursor

- [ ] **Non-basic export auth modes** - Cursor forces basic auth via SDK config and requires endpoint/tenant/token credentials. OpenCode supports tenant-only and unauthenticated export modes. Present in: OpenCode. Reference: `plugins/opencode/src/config.ts`.
- [ ] **Full-content redaction** - Cursor documents that full capture is exported verbatim; it lacks user, assistant, tool, and structured JSON sensitive-key redaction. Present in: Claude Code, Codex, OpenCode, Pi. Reference: `plugins/sigil/internal/redact/redact.go`, `plugins/opencode/src/redact.ts`, `plugins/pi/src/client.ts`.
- [ ] **Tool argument/result bounding** - Cursor forwards captured tool input/output without adapter-level truncation or bounding. Present in: Claude Code, Codex. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/hook/handlers.go`.
- [ ] **Entrypoint/source tag** - Cursor has the shared tag builder but does not set an entrypoint/source tag on exported generations. Present in: Claude Code, Codex. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`.
- [ ] **Subagent/lineage test coverage** - Cursor tags background agents but lacks a golden or focused lineage/subagent test. Present in: Claude Code, Codex, Pi. Reference: `plugins/sigil/cmd/sigil/testdata/golden/claude-code-subagent`, `plugins/pi/src/lineage.test.ts`.

#### OpenCode

- [ ] **Streaming generation mode / TTFT** - OpenCode exports generations with synchronous SDK calls and does not record first-token timing. Pi uses streaming generation and sets first-token time. Present in: Pi. Reference: `plugins/pi/src/index.ts`.
- [ ] **Deterministic generation IDs** - OpenCode relies on SDK-generated IDs instead of deriving stable IDs from host session/message IDs. Present in: Claude Code, Codex, Cursor, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/pi/src/lineage.ts`.
- [ ] **Conversation title** - OpenCode does not set `conversationTitle` from session metadata or first prompt. Present in: Claude Code, Cursor, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/hook/beforesubmit.go`, `plugins/pi/src/mappers.ts`.
- [ ] **User input redaction** - OpenCode redacts assistant/tool content but intentionally sends user prompt text as-is when capture mode allows it. Present in: Claude Code, Codex, Pi. Reference: `plugins/sigil/internal/redact/redact.go`, `plugins/pi/src/client.ts`.
- [ ] **Built-in context tags** - OpenCode does not add built-in `git.branch`, `cwd`, or entrypoint/source tags. Present in: Claude Code, Codex, Cursor, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`, `plugins/pi/src/git.ts`.
- [ ] **Request controls** - OpenCode does not capture provider request controls such as `temperature`, `top_p`, `tool_choice`, or thinking budget. Present in: Pi. Reference: `plugins/pi/src/mappers.ts`.
- [ ] **Tool argument/result bounding** - OpenCode redacts tool arguments/results but does not apply explicit size bounds before export/span attributes. Present in: Claude Code, Codex. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/hook/handlers.go`.
- [ ] **Lineage/subagent test coverage** - OpenCode lacks a lineage-focused test or golden. Present in: Claude Code, Codex, Pi. Reference: `plugins/sigil/cmd/sigil/testdata/golden/claude-code-subagent`, `plugins/pi/src/lineage.test.ts`.

#### Pi

- [ ] **Non-basic export auth modes** - Pi supports basic or no auth, but not OpenCode's tenant-only mode. Present in: OpenCode. Reference: `plugins/opencode/src/config.ts`.
- [ ] **Built-in context tags beyond `git.branch`** - Pi only adds `git.branch`, and only in full capture mode; it lacks `cwd`, entrypoint/source, and comparable broad built-in context tags. Present in: Claude Code, Codex, Cursor. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`.
- [ ] **Multiple permission/guard surfaces** - Pi evaluates guards at `tool_call`, but OpenCode also evaluates `permission.ask`. Present in: OpenCode. Reference: `plugins/opencode/src/hooks.ts`.

### Already tracked

No open `enhancement` issues were returned by:

```sh
gh issue list --repo grafana/sigil-sdk --state open --label enhancement --limit 200
```

No previously created issue was found in automation memory.

### Context

Created by the automated plugin feature parity audit.
