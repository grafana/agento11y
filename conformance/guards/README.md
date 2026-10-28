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

## How both runtimes read shell calls

Both read every value under the `command`, `cmd` and `script` keys, in any letter
case, use the same shell tool names, and share one shell reader, kept byte for byte
the same in `plugins/agento11y/internal/guardeval/shell.go` and sigil's
`sigil/internal/eval/hooks/shell.go`. After joining a backslash-newline, each
command is judged as written, with its quotes and backslashes removed, and as each
simple command in it, split where the shell splits commands, both as written and
from its name on, without assignments, wrappers with their option values, and
redirections. Quoted text and quoted heredoc bodies stay data; the code in
`$(...)`, backticks (also in an unquoted heredoc body), `sh -c`, `su -c`, `env -S`,
`script -c`, `eval`, `watch`, `trap`, `find -exec` and a heredoc fed to a shell is
read as commands, three levels deep, and code at the last level is judged as
written. Every pack pattern matches where a command starts. So `git "reset" --hard`,
`cd repo && git reset --hard` and `sudo -u deploy bash -c 'git reset --hard'` are
denied, and a commit message that mentions `git reset --hard` is not. A shell
command longer than 64 KiB is not read, and both runtimes fail it closed.

A shell call with no command key is judged by the string values in its arguments.
Where the arguments cannot be read at all (not one JSON object, or a command key
holding something other than a string or a list of strings), Cloud fails the call
with `blocked by tool_filter: shell tool arguments have no readable command`, while
the local engine judges the string values, or the text of arguments that are not
JSON. Cloud also reads command keys inside nested objects and every copy of a
repeated key. The local engine also judges an argv list joined with plain spaces,
as earlier versions did, so users' own `shell_command` patterns keep matching.

## What the patterns cannot see

The patterns judge the command as the shell reads it, not as it runs. A command
built when it runs gets past them, locally and in Cloud: a variable
(`g=git; $g reset --hard`), an alias, a script file, text piped into a shell
(`echo "..." | bash`), or a command an interpreter such as `python -c` runs. So
does `git checkout <path>` with no `--`, since a path cannot be told from a branch
name; a git config change that turns a safety off, such as
`git -c clean.requireForce=false clean`; and disk writes the disk pack does not
name, such as `> /dev/disk2` or `diskutil`. The packs stop an agent's mistakes, not
a determined attempt to get round them.

## Changing a pack

Change the pack in `plugins/agento11y/internal/local/guardpacks.go`, then regenerate
the file from `plugins/agento11y`:

```sh
UPDATE_GOLDENS=1 go test -run TestCloudPacksFixture ./internal/local/
```

The test checks every case against the local engine before it writes. Copy the
result byte for byte to sigil's
`sigil/internal/eval/hooks/testdata/coding_agent_packs.json`, where
`TestCodingAgentPacksMatchLocalCases` runs every case through each pack's Cloud
rule, and, once the Guards page change (grafana/sigil#2069) is in, to
`apps/plugin/src/evaluation/codingAgentPacks.json`, the pack list the Guards page
turns on.
