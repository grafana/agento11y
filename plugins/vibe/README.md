# @grafana/sigil-vibe

[Mistral Vibe](https://github.com/mistralai/vibe) is sent to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/) by registering hooks in Mistral Vibe's `hooks.toml` that forward each turn to the `sigil` binary. `post_agent_turn` exports one generation per turn; `before_tool` enforces Sigil guard policy (when enabled); `after_tool` records per-tool timing for tool spans.

> Status: **Experimental.** Mistral Vibe's hook contract is itself marked experimental and may change between releases; the launcher pins to the shape verified at build time.

By default only metadata is sent (token counts, model, tool names). Set `SIGIL_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | sh
sigil vibe
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/sigil
sigil vibe
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/sigil@latest
sigil vibe
```

`sigil vibe` resolves the `vibe` binary on `PATH`, upserts the three sigil-owned `[[hooks]]` entries into `~/.vibe/hooks.toml` (or `$VIBE_HOME/hooks.toml`) on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/sigil/config.env`, and then execs it. Repeated runs are no-ops: each entry is matched by name (`sigil`, `sigil-before-tool`, `sigil-after-tool`) and any hand-authored hooks in the same file are preserved.

The launcher always sets `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in Mistral Vibe's environment because these events are gated behind that flag.

<details>
<summary>Manual hook registration</summary>

Add these blocks to `~/.vibe/hooks.toml`:

```toml
[[hooks]]
name = "sigil"
type = "post_agent_turn"
command = "sigil vibe hook"
timeout = 30

[[hooks]]
name = "sigil-before-tool"
type = "before_tool"
command = "sigil vibe hook"
timeout = 30
match = "*"

[[hooks]]
name = "sigil-after-tool"
type = "after_tool"
command = "sigil vibe hook"
timeout = 30
match = "*"
```

Then export `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in the shell where you run `vibe`, and run `sigil login` once for credentials.

</details>

## 2. Credentials

Credentials are shared with every other `sigil` launcher; see [`pi/README.md`](../pi/README.md#2-credentials) for the field-by-field walkthrough. Once `~/.config/sigil/config.env` exists, every launcher (and the Mistral Vibe hook) picks it up.

## 3. Verify

Run one agent turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds, labelled with agent `mistral-vibe` and conversation id equal to the Mistral Vibe `session_id`.

## Guards

`before_tool` evaluates each tool call against Sigil guard policy. Guards are **off by default**; enable them with `SIGIL_GUARDS_ENABLED=true` (tune with `SIGIL_GUARDS_TIMEOUT_MS` and `SIGIL_GUARDS_FAIL_OPEN`). When enabled, a policy can **deny** a tool call (Mistral Vibe blocks it and shows the reason to the model) or **rewrite** its arguments (e.g. redact a secret before the tool runs). With guards disabled, `before_tool` is a pass-through that writes nothing. Evaluation runs synchronously before the tool, so a policy should be fast or local; on timeout or transport error the call follows `SIGIL_GUARDS_FAIL_OPEN` (open by default).
