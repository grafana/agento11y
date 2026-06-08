# Plugin feature parity report - 2026-06-08

Issue title:

```text
Plugin feature parity report - 2026-06-08
```

Issue body:

## Plugin Feature Parity Report

Source audited: `origin/main` after `git fetch origin main`.

Scope:

- Claude Code: `plugins/claude-code/` plus `plugins/sigil/internal/agents/claudecode/`
- Codex: `plugins/codex/` plus `plugins/sigil/internal/agents/codex/`
- Cursor: `plugins/cursor/` plus `plugins/sigil/internal/agents/cursor/`
- OpenCode: `plugins/opencode/` plus `plugins/sigil/internal/agents/opencode/launch.go`
- Pi: `plugins/pi/` plus `plugins/sigil/internal/agents/pi/launch.go`

Deduplication:

- Automation memories contain no previously created GitHub issue for these gaps.
- `gh issue list --repo grafana/sigil-sdk --state open --label enhancement --limit 200` returned no open enhancement issues.

Note: this run could not create the GitHub issue directly because the automation environment exposes no write-capable issue-creation tool, and repository instructions restrict `gh` to read-only operations.

### Parity matrix

| Category | Feature | Claude Code | Codex | Cursor | OpenCode | Pi |
|----------|---------|:-----------:|:-----:|:------:|:--------:|:--:|
| Packaging | Host-appropriate integration entrypoint | ✅ | ✅ | ✅ | ✅ | ✅ |
| Packaging | Launcher auto-install | ✅ | ✅ | ➖ | ✅ | ✅ |
| Packaging | Launcher auto-update / refresh | ✅ | ✅ | ➖ | ✅ | ➖ |
| Packaging | Local receiver launch env | ✅ | ✅ | ➖ | ✅ | ✅ |
| Config | Loads `~/.config/sigil/config.env` / XDG config | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | `SIGIL_CONTENT_CAPTURE_MODE` modes | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Metadata-only default capture posture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Config | Normalizes `SIGIL_ENDPOINT` if user pasted `/api/v1/generations:export` | ❌ | ❌ | ❌ | ✅ | ✅ |
| Auth | Basic auth with tenant ID + token | ✅ | ✅ | ✅ | ✅ | ✅ |
| Auth | Tenant-only export auth mode | ❌ | ❌ | ❌ | ✅ | ❌ |
| Telemetry | HTTP generation export | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Optional OTLP traces/metrics | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Per-tool execution spans | ✅ | ✅ | ✅ | ✅ | ✅ |
| Telemetry | Real tool execution timing when host exposes it | ➖ | ✅ | ✅ | ✅ | ✅ |
| Capture | User and assistant message capture when capture mode allows | ✅ | ✅ | ✅ | ✅ | ✅ |
| Capture | System prompt capture when host exposes it | ➖ | ➖ | ➖ | ✅ | ✅ |
| Capture | Thinking/reasoning presence flag | ✅ | ✅ | ✅ | ✅ | ✅ |
| Capture | Thinking/reasoning content capture when host exposes text | ❌ | ➖ | ➖ | ✅ | ✅ |
| Usage | Input/output/cache token usage | ✅ | ✅ | ✅ | ✅ | ✅ |
| Usage | Reasoning token usage when host exposes counts | ➖ | ✅ | ➖ | ✅ | ➖ |
| Usage | Cost metadata when host exposes cost | ➖ | ➖ | ➖ | ✅ | ✅ |
| Identity | Stable generation IDs | ✅ | ✅ | ✅ | ❌ | ✅ |
| Identity | Provider/host response ID mapping when host exposes an ID | ✅ | ➖ | ✅ | ❌ | ✅ |
| Identity | Conversation title | ✅ | ❌ | ✅ | ❌ | ✅ |
| Identity | Parent/subagent generation linking where host exposes linkage | ✅ | ✅ | ➖ | ➖ | ✅ |
| Tooling | Tool catalog / definitions | ✅ | ✅ | ✅ | ✅ | ✅ |
| Tooling | Tool status and error capture | ✅ | ✅ | ✅ | ✅ | ✅ |
| Tooling | Tool argument/result content gated by capture mode | ✅ | ✅ | ✅ | ✅ | ✅ |
| Tooling | Tool argument/result bounding or truncation | ✅ | ❌ | ❌ | ❌ | ❌ |
| Guards | Tool-call guard evaluation / blocking where host supports it | ✅ | ✅ | ➖ | ✅ | ✅ |
| Guards | Preflight context transform where host supports it | ➖ | ➖ | ➖ | ➖ | ✅ |
| Redaction | Full-content secret redaction before export | ✅ | ✅ | ❌ | ❌ | ✅ |
| Redaction | User input redaction when content capture allows input export | ✅ | ✅ | ❌ | ❌ | ✅ |
| Redaction | Structured JSON sensitive-key redaction | ✅ | ✅ | ❌ | ❌ | ❌ |
| Tags | Built-in `git.branch` tag in metadata modes | ✅ | ❌ | ✅ | ❌ | ❌ |
| Tags | Built-in cwd/source/entrypoint-style tags | ✅ | ✅ | ❌ | ❌ | ❌ |
| Reliability | Failed-export retry without data loss | ✅ | ✅ | ✅ | ➖ | ➖ |
| Reliability | Stale persisted state cleanup | ❌ | ✅ | ✅ | ➖ | ➖ |
| Operations | Debug logging option | ✅ | ✅ | ✅ | ✅ | ✅ |

Legend: ✅ implemented, ❌ missing and actionable, ➖ not relevant because of host-agent or architectural constraints.

### Gaps by plugin

#### Claude Code

- [ ] **Endpoint normalization** - `SIGIL_ENDPOINT` is concatenated directly with `/api/v1/generations:export`, unlike OpenCode and Pi, which strip an accidentally pasted export path. Present in: OpenCode, Pi. Reference: `plugins/sigil/internal/agents/claudecode/hook.go`, `plugins/opencode/src/config.ts`, `plugins/pi/src/config.ts`
- [ ] **Tenant-only export auth mode** - only Basic tenant+token auth is supported in the Go hook path, while OpenCode supports tenant-only auth. Present in: OpenCode. Reference: `plugins/sigil/internal/agents/claudecode/hook.go`, `plugins/opencode/src/config.ts`
- [ ] **Thinking content capture in full mode** - Claude transcript blocks expose `thinking` content, but the mapper always exports only `"[thinking block omitted]"`. Present in: OpenCode, Pi. Reference: `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/opencode/src/mappers.ts`, `plugins/pi/src/mappers.ts`
- [ ] **Stale persisted state cleanup** - Claude Code keeps per-session offset/title/model state but does not have Codex/Cursor-style stale cleanup. Present in: Codex, Cursor. Reference: `plugins/sigil/internal/agents/claudecode/state/state.go`, `plugins/sigil/internal/agents/codex/fragment/fragment.go`, `plugins/sigil/internal/agents/cursor/hook/sessionend.go`

#### Codex

- [ ] **Endpoint normalization** - `SIGIL_ENDPOINT` is concatenated directly with `/api/v1/generations:export`, unlike OpenCode and Pi. Present in: OpenCode, Pi. Reference: `plugins/sigil/internal/agents/codex/hook/handlers.go`, `plugins/opencode/src/config.ts`, `plugins/pi/src/config.ts`
- [ ] **Tenant-only export auth mode** - only Basic tenant+token auth is supported in the Go hook path, while OpenCode supports tenant-only auth. Present in: OpenCode. Reference: `plugins/sigil/internal/agents/codex/hook/handlers.go`, `plugins/opencode/src/config.ts`
- [ ] **Conversation title** - Codex exports conversation IDs and turn data but does not derive a conversation title from the first prompt or host session name. Present in: Claude Code, Cursor, Pi. Reference: `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/pi/src/mappers.ts`
- [ ] **Built-in `git.branch` tag** - Codex records `cwd`, source, subagent tags, and token metadata, but does not resolve `git.branch` from the working directory. Present in: Claude Code, Cursor, Pi. Reference: `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`, `plugins/pi/src/git.ts`
- [ ] **Tool argument/result bounding** - Codex can persist and export full tool JSON in full mode without a plugin-level truncation cap. Present in: Claude Code. Reference: `plugins/sigil/internal/agents/codex/hook/handlers.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`

#### Cursor

- [ ] **Endpoint normalization** - `SIGIL_ENDPOINT` is concatenated directly with `/api/v1/generations:export`, unlike OpenCode and Pi. Present in: OpenCode, Pi. Reference: `plugins/sigil/internal/agents/cursor/hook/emit.go`, `plugins/opencode/src/config.ts`, `plugins/pi/src/config.ts`
- [ ] **Tenant-only export auth mode** - only Basic tenant+token auth is supported in the Go hook path, while OpenCode supports tenant-only auth. Present in: OpenCode. Reference: `plugins/sigil/internal/agents/cursor/hook/emit.go`, `plugins/opencode/src/config.ts`
- [ ] **Full-content secret redaction** - Cursor explicitly does not pass captured content through the shared redactor before export. Present in: Claude Code, Codex, OpenCode, Pi. Reference: `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/sigil/internal/redact/redact.go`, `plugins/opencode/src/redact.ts`, `plugins/pi/src/client.ts`
- [ ] **User input redaction** - Cursor exports prompt content verbatim in content modes. Present in: Claude Code, Codex, Pi. Reference: `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/pi/src/client.ts`
- [ ] **Structured JSON sensitive-key redaction** - Cursor has no redaction pass, so JSON fields such as `password`, `token`, or `api_key` are not masked. Present in: Claude Code, Codex. Reference: `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/sigil/internal/redact/redact.go`
- [ ] **Tool argument/result bounding** - Cursor can persist and export full tool I/O in full mode without a plugin-level truncation cap. Present in: Claude Code. Reference: `plugins/sigil/internal/agents/cursor/hook/posttooluse.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`
- [ ] **Built-in entrypoint/source tag** - Cursor records `cwd`, `git.branch`, and background-agent/subagent status, but does not set an entrypoint/source tag. Present in: Claude Code, Codex. Reference: `plugins/sigil/internal/agents/cursor/tags/tags.go`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`

#### OpenCode

- [ ] **Stable generation and response IDs** - OpenCode deduplicates by message ID internally but does not pass a deterministic generation ID or host response ID into exported generations. Present in: Claude Code, Codex, Cursor, Pi. Reference: `plugins/opencode/src/hooks.ts`, `plugins/opencode/src/testdata/golden/opencode-full-message.golden.json`, `plugins/pi/src/lineage.ts`
- [ ] **Conversation title** - OpenCode exports conversation/session IDs but does not derive a title from session metadata or first user prompt. Present in: Claude Code, Cursor, Pi. Reference: `plugins/opencode/src/hooks.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/mapper/mapper.go`, `plugins/pi/src/mappers.ts`
- [ ] **Built-in context tags** - OpenCode exports with empty plugin-built tags and does not add `git.branch`, `cwd`, source, or entrypoint-style tags. Present in: Claude Code, Codex, Cursor, Pi. Reference: `plugins/opencode/src/hooks.ts`, `plugins/opencode/src/testdata/golden/opencode-full-message.golden.json`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`, `plugins/pi/src/index.ts`
- [ ] **User input redaction** - OpenCode deliberately does not redact user prompts before export in content modes. Present in: Claude Code, Codex, Pi. Reference: `plugins/opencode/src/mappers.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/pi/src/client.ts`
- [ ] **Structured JSON sensitive-key redaction** - OpenCode redaction is regex-pattern based and lacks the Go shared redactor's JSON sensitive-key pattern. Present in: Claude Code, Codex. Reference: `plugins/opencode/src/redact.ts`, `plugins/sigil/internal/redact/redact.go`
- [ ] **Tool argument/result bounding** - OpenCode can export full tool args/results without a plugin-level truncation cap. Present in: Claude Code. Reference: `plugins/opencode/src/mappers.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`

#### Pi

- [ ] **Tenant-only export auth mode** - Pi supports Basic tenant+token auth or no auth, while OpenCode supports tenant-only auth. Present in: OpenCode. Reference: `plugins/pi/src/config.ts`, `plugins/opencode/src/config.ts`
- [ ] **Built-in `git.branch` tag in metadata modes** - Pi only adds `git.branch` when content capture is `full`, while Claude Code and Cursor include branch metadata independently of content capture. Present in: Claude Code, Cursor. Reference: `plugins/pi/src/index.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`
- [ ] **Broader built-in context tags** - Pi does not add cwd/source/entrypoint-style tags. Present in: Claude Code, Codex, Cursor. Reference: `plugins/pi/src/index.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`, `plugins/sigil/internal/agents/codex/mapper/mapper.go`, `plugins/sigil/internal/agents/cursor/tags/tags.go`
- [ ] **Structured JSON sensitive-key redaction** - Pi uses SDK secret redaction, but the plugin does not implement the Go shared redactor's JSON sensitive-key pattern for plugin-level captured payloads. Present in: Claude Code, Codex. Reference: `plugins/pi/src/client.ts`, `plugins/sigil/internal/redact/redact.go`
- [ ] **Tool argument/result bounding** - Pi can export full tool args/results without a plugin-level truncation cap. Present in: Claude Code. Reference: `plugins/pi/src/mappers.ts`, `plugins/sigil/internal/agents/claudecode/mapper/mapper.go`

### Already tracked

None. The open enhancement issue query returned no results.

### Context

Created by the automated plugin feature parity audit.

