# Guard pack fixtures

`coding-agent-packs.json` exports the local coding agent guard packs for Grafana
Cloud. Sigil keeps a byte-for-byte copy and builds its coding agent guard templates
from it, so a Cloud guard denies the calls the local pack denies.

| Key | Contents |
|-----|----------|
| `packs` | Git safety, destructive commands, permissions, disk wipe, and File safety: each pack's title, description, priority, `shell_command` regex patterns, and the message a denial shows. File safety also has `blocked_names`, a tool filter for file tools. |
| `cases` | Tool calls with the verdict both runtimes must give. A case without `tool` is a Bash call running `command`; any other case passes `input` as the tool's arguments. |

## File safety is narrower in Cloud

The local File safety filter matches `.env` between character classes such as
`[/"' <>|;&()]`. Cloud's tool filter has only `*` and `?`, so `blocked_names` matches
a `.env` path as file tools send it: after a `/` or a quote, and before a quote or a
dot, plus the `apply_patch` header form. A `.env` after a space or a shell character
elsewhere in the arguments, such as in a file's content, is denied only locally.
The cases mark those calls `local_only`, and sigil expects Cloud to allow them.

## Changing a pack

Change the pack in `plugins/agento11y/internal/local/guardpacks.go`, then regenerate
the file from `plugins/agento11y`:

```sh
UPDATE_GOLDENS=1 go test -run TestCloudPacksFixture ./internal/local/
```

The test checks every case against the local engine before it writes. Copy the
result to sigil's `sigil/internal/eval/predefined/coding_agent_packs.json` and
`apps/plugin/src/evaluation/codingAgentPacks.json`.
