# Guard pack fixtures

`coding-agent-packs.json` exports the local coding agent guard packs for Grafana
Cloud. Sigil keeps byte-for-byte copies and turns each pack into one guard rule
whose tool filter carries it, so a Cloud guard denies the calls the local pack
denies.

| Key | Contents |
|-----|----------|
| `packs` | Git safety, destructive commands, permissions, disk wipe, and File safety: each pack's title, description, priority, the regex patterns of its local `shell_command` evaluator, which Cloud runs as `tool_filter.shell_patterns`, and the message a denial shows. File safety also has `blocked_names`, glob patterns for file and content search tools (`Grep`, `grep_search`), which sit in the same Cloud rule. |
| `cases` | Tool calls with the verdict both runtimes must give. A case without `tool` is a Bash call running `command`; any other case passes `input` as the tool's arguments. |

## File safety is narrower in Cloud

The local File safety filter matches `.env` between character classes such as
`[/"' <>|;&()]`. Cloud's tool filter has only `*` and `?`, so `blocked_names` matches
a `.env` path as file tools send it: after a `/` or a quote, and before a quote or a
dot, plus the `apply_patch` header form. A `.env` after a space or a shell character
elsewhere in the arguments, such as in a file's content, is denied only locally.
The cases mark those calls `local_only`, and sigil expects Cloud to allow them.

Content search tools (`Grep`, `grep_search`) take a glob, so a `*` can sit on either
side of `.env`: `.env*`, `**/.env*`, `*.env`. Locally their filter adds `*` to both
character classes. Cloud cannot name a literal `*`, so it puts `?` in that position,
which also blocks a glob with one ordinary character there, such as `a.env` or
`.env1`.

## Cloud is stricter on shell arguments

The local engine judges one command per shell call: the first command key it
finds. Cloud's `shell_patterns` judge every value under every command key,
including repeated keys and other letter cases, and fail a shell call whose
arguments are not one JSON object or whose command is neither a string nor a list
of strings. So Cloud also denies `{"command":"ls","script":"rm -rf x"}`, a repeated
`command` key whose last copy is harmless, and unreadable arguments, which the local
engine allows. Cloud also reads command keys inside a nested object when the top
level has none. Both use the same shell tool names and command keys, and both join
a backslash-newline before matching, as the shell does.

## Changing a pack

Change the pack in `plugins/agento11y/internal/local/guardpacks.go`, then regenerate
the file from `plugins/agento11y`:

```sh
UPDATE_GOLDENS=1 go test -run TestCloudPacksFixture ./internal/local/
```

The test checks every case against the local engine before it writes. Copy the
result to sigil's `sigil/internal/eval/hooks/testdata/coding_agent_packs.json` and
`apps/plugin/src/evaluation/codingAgentPacks.json`, the two copies the comment in
sigil's `sigil/internal/eval/hooks/coding_agent_packs_test.go` names.
