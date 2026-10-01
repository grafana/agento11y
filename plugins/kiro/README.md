# Kiro CLI (experimental)

Capture Kiro CLI 3 turns with the shared `agento11y` binary. This integration
uses Kiro's [workspace command hooks](https://kiro.dev/docs/hooks/), not a
separate npm package.

```sh
agento11y login
agento11y kiro -- chat
# Or use the local receiver:
agento11y kiro --local -- chat
```

The launcher requires CLI 3 or newer and installs `.kiro/hooks/agento11y.json`
in the current directory. Run it from the project root. Arguments after `--`
are passed to `kiro-cli` unchanged. Install without launching, or remove the
integration from this workspace:

```sh
agento11y kiro install
agento11y kiro install --json
agento11y kiro uninstall
```

After installation, launching Kiro directly also invokes the hooks. Saved
`agento11y login` settings apply; launcher-only flags apply only to that launch.
`agento11y doctor` checks installation in the current workspace. Other hook
files are preserved, and installation/removal refuses customized versions of
`agento11y.json`. The integration is workspace-scoped and is not part of fleet
reconciliation.

## Captured data and limits

- One generation per observed user turn, grouped by Kiro session ID.
- User prompts when supplied by `UserPromptSubmit`.
- Tool names, arguments, and responses from `PostToolUse`, including MCP tools.
- Generation and tool spans when OTLP is configured. Tool spans are point events
  at hook receipt; execution duration and tool error status are unavailable.
- Existing tags, agent-name overrides, content-capture modes, and redaction.
  Prompts are redacted by default; tool payloads are always redacted.

The documented hook contract does **not** supply assistant response text,
model identity, token counts, or cost. These are unavailable, not estimated;
model is `unknown`, provider is the product label `kiro`, and generation
metadata marks unavailable fields. Token/cost dashboards will be incomplete.
This adapter does not read undocumented Kiro session databases or transcripts.

`AGENTO11Y_CONTENT_CAPTURE_MODE` supports `full`, `no_tool_content`,
`metadata_only` (default), and `full_with_metadata_spans`. Content excluded by
these modes is also excluded from the adapter's private temporary turn files.
Successful exports delete those files; failed exports retain them under the
agento11y state directory for retry at the next Stop/SessionEnd. Retries reuse
generation IDs. Up to 128 pending turns are retained per session; a full queue
is reported to the debug log. Hooks always exit successfully and write nothing
to stdout, so capture errors do not inject messages into Kiro.

CLI 2.x, Kiro IDE, guards, historical session import, and subagent lineage are
not supported by this initial adapter. Validation uses hook fixtures and a
local HTTP receiver; live Kiro compatibility still needs a smoke test.

Contract references: [hook events](https://kiro.dev/docs/hooks/types/),
[CLI 3 migration](https://kiro.dev/docs/cli/v3/hooks-migration/), and
[Kiro's CLI hook reference](https://github.com/kirodotdev/KiroCrew/blob/main/docs/reference/kiro-cli/hooks.md).
