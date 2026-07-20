# Sigil for Mistral Vibe

[Mistral Vibe](https://github.com/mistralai/vibe) is sent to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/) by registering hooks in Mistral Vibe's `hooks.toml` that forward each turn to the `agento11y` binary. `post_agent_turn` exports one generation per turn; `before_tool` enforces Sigil guard policy (when enabled); `after_tool` records per-tool timing for tool spans.

> Status: **Experimental.** Mistral Vibe's hook contract is itself marked experimental and may change between releases; the launcher pins to the shape verified at build time.

By default only metadata is sent (token counts, model, tool names). Set `AGENTO11Y_CONTENT_CAPTURE_MODE` to `full`, `no_tool_content`, `metadata_only`, or `full_with_metadata_spans` to control what is sent. `default` is accepted as an alias for `metadata_only`. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the full reference.

## 1. Install and launch

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/agento11y/scripts/install.sh | sh
agento11y vibe
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
agento11y vibe
```

**Go install (Windows, or any platform with Go 1.25+):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
agento11y vibe
```

The command was renamed from `sigil`; the old name still works but will be removed in a future release.

`agento11y vibe` resolves the `vibe` binary on `PATH`, upserts the three agento11y-owned `[[hooks]]` entries into `~/.vibe/hooks.toml` (or `$VIBE_HOME/hooks.toml`) on first run, prompts for missing Grafana Cloud credentials, writes `~/.config/agento11y/config.env`, and then execs it. Repeated runs are no-ops: each entry is matched by name (`agento11y`, `agento11y-before-tool`, `agento11y-after-tool`); entries under the pre-rename `sigil*` names are replaced and any hand-authored hooks in the same file are preserved.

The launcher always sets `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in Mistral Vibe's environment because these events are gated behind that flag.

<details>
<summary>Manual hook registration</summary>

Add these blocks to `~/.vibe/hooks.toml`:

```toml
[[hooks]]
name = "agento11y"
type = "post_agent_turn"
command = "agento11y vibe hook"
timeout = 30

[[hooks]]
name = "agento11y-before-tool"
type = "before_tool"
command = "agento11y vibe hook"
timeout = 30
match = "*"

[[hooks]]
name = "agento11y-after-tool"
type = "after_tool"
command = "agento11y vibe hook"
timeout = 30
match = "*"
```

Then export `VIBE_ENABLE_EXPERIMENTAL_HOOKS=true` in the shell where you run `vibe`, and run `agento11y login` once for credentials.

</details>

## 2. Credentials

Credentials are shared with every other `agento11y` launcher; see [`pi/README.md`](../pi/README.md#2-credentials) for the field-by-field walkthrough. Once `~/.config/agento11y/config.env` exists, every launcher (and the Mistral Vibe hook) picks it up. If you only have the old `~/.config/sigil/config.env`, that file is used instead.

## 3. Verify

Run one agent turn, then open **AI Observability → Conversations** in Grafana Cloud. A new generation should appear within a few seconds, labelled with agent `mistral-vibe` and conversation id equal to the Mistral Vibe `session_id`.

## Guards

`before_tool` evaluates each tool call against Sigil guard policy. Guards are **off by default**; enable them with `AGENTO11Y_GUARDS_ENABLED=true` (tune with `AGENTO11Y_GUARDS_TIMEOUT_MS` and `AGENTO11Y_GUARDS_FAIL_OPEN`). When enabled, a policy can **deny** a tool call (Mistral Vibe blocks it and shows the reason to the model) or **rewrite** its arguments (e.g. redact a secret before the tool runs). With guards disabled, `before_tool` is a pass-through that writes nothing. Evaluation runs synchronously before the tool, so a policy should be fast or local; on timeout or transport error the call follows `AGENTO11Y_GUARDS_FAIL_OPEN` (open by default).
