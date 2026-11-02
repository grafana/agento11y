# Local guard packs

Local guard packs check coding-agent prompts and tool calls before they run.
Seven packs deny matching requests; **Secret redaction** rewrites matching secret values in tool arguments.
Use this guide to choose packs and understand their limits.

These checks apply only to calls that your coding agent submits to the local agento11y daemon.
They do not protect other terminal commands, inspect programs that a command launches, or replace an operating-system sandbox.
Local mode requires macOS or Linux.

## Set up packs

For agent-assisted setup, paste this into your coding agent:

```text
Run `agento11y skills show setup-local-guards` and follow it to help me choose, configure, and test local guard packs.
```

Before enabling guards, check whether the session forwards to Cloud.
Guard requests include evaluated content even when generation capture is `metadata_only`.
Local-only mode avoids that relay, but local mode still stores full session content on this machine.

Back up a hand-edited `guards.toml` before changing packs.
Pack updates preserve custom rules but remove TOML comments and formatting.

For manual setup:

1. Run `agento11y local open` to open the local app.
1. Open **Security** > **Guards**, then turn on **Enable guards**.
1. Turn on each pack you want. Use **view** beside a pack to inspect its examples or secret formats.
1. Start a fresh agent session through local mode, for example `agento11y claude --local` or `agento11y pi --local`.
1. Test the saved rules with the [offline checks](#test-without-running-a-dangerous-command).

Opening the local app alone does not route an agent through it.
The agent needs its agento11y integration, guards enabled, and a local destination.
For Codex, open `/hooks` inside the agent and trust the agento11y hooks after installation.
For hook installation and saved local destinations, refer to [Coding agent observability](../plugins/agento11y/README.md#local-mode).
A Cloud-only session does not evaluate this machine's `guards.toml`.

The switches save immediately.
The main switch saves `AGENTO11Y_GUARDS_ENABLED` in `config.env`; pack switches save rules in `guards.toml` beside it.
The usual path is `~/.config/agento11y/guards.toml`, but configuration discovery can select another directory.
Use the path displayed in the app rather than assuming the default.

Enabling guards does not select any packs for you.
Turning off the main switch removes all known pack rules, so turning it back on does not restore your selection.
Custom rules remain and still apply to requests from agents that have not picked up the disabled setting.
If the rules file cannot be parsed, disabling guards leaves that file unchanged.

Restart the coding agent after changing guard enablement or its destination.
Changes to pack rules take effect on the daemon's next check without a daemon restart.

## Choose packs

Examples describe each unmodified pack on its own.
Current coding-agent integrations use lexer-based regex matching. Offline tests can use Bash syntax-tree analysis; see [Enforcement limits](#enforcement-limits).
Another pack, a custom rule, or Cloud policy can change the final result.
Treat an allowed example as a matching limit, not a recommendation to run it.

### Secret redaction

This pack replaces recognized API keys, tokens, and private keys in tool arguments with redaction placeholders.
It allows the call unless another rule denies it.
It uses the shared [secret-pattern table](../redaction/README.md); **view** lists the formats included in your installed binary.
An unrecognized password or token can pass unchanged.

Hosts that apply transforms pass the rewritten arguments to the tool, which can break commands that need the original secret.
The Copilot integration applies argument rewrites in Copilot CLI, but drops them in Copilot Chat in VS Code and on unidentified surfaces.
Do not rely on this pack to redact arguments before tool execution on those surfaces.
The pack runs during the tool-call check, called `postflight` in the rules format; that check happens before tool execution.
It does not scrub existing files, earlier transcripts, or arbitrary tool results.

### File safety

The pack matches tool names and argument text, not filesystem access.
For example, it denies `cat .env`, `cat .env.local`, and recognized read, write, edit, or patch calls referencing those names.
Shell matching distinguishes quoted data from command words, so a quoted commit message mentioning `.env` can pass.
It allows `cat .envrc`, `cat .environment`, and `cat config.env` when no other rule blocks them.
A file named `.env.example` also matches, even if it contains no secrets.
Other filenames containing secrets are outside this pack's scope.

A different tool name, a symlink, or a program that opens a file internally can avoid these checks.
The pack does not resolve paths or inspect file contents.

### Git safety

The pack denies these command forms, including supported option-order and quoting variations, even for intentional cleanup:

- `git reset --hard`
- `git push --force`, `git push -f`, and force refspecs such as `git push origin +HEAD:main`
- `git clean -f`, except recognized dry runs
- `git checkout --` and whole-tree pathspecs such as `.`, `./`, `./*`, `*`, and `:/`
- `git checkout -f` or `--force`, and `git switch -f`, `--force`, or `--discard-changes`
- `git restore` into the working tree, including paths supplied through `--pathspec-from-file` or `xargs`
- `git stash drop` and `git stash clear`
- `git branch -D`

It allows `git push --force-with-lease`, `git push --force-if-includes`, ordinary pushes, and `git restore --staged` without a worktree mode.
Branch checkouts and individual paths without `--` also allow: the pack cannot distinguish `git checkout file.go` from a branch checkout.

Regex matching allows dry-run clusters such as `git clean -nf`, but still denies separate flags in `git clean -n -f`.
When parsing completes, the pack recognizes separate dry-run flags and option order, including `--no-dry-run` cancellation.
An option value does not count: `git clean -e -n -f` denies because `-n` is the exclusion value.
The parser treats option values and operands after `--` as data, not flags.

Supported Git global options, such as `git -P` and `git -C dir`, do not hide these commands.
Regex matching recognizes selected option abbreviations, such as `git reset --ha` and `git clean --fo`.
Unsupported parser options trigger fallback rather than complete analysis.
Other history rewrites, such as rebase, and configuration changes such as `git -c clean.requireForce=false clean` are outside this pack's checks.
Git aliases are not expanded.

### Destructive commands

The pack denies `rm -rf`, `rm -fr`, separate flags such as `rm -r -f`, and long forms such as `rm --recursive --force`.
The target does not matter: deleting a temporary build directory matches too.

It allows `rm -r`, `rm -f`, and plain `rm` because they do not combine recursive and force options.
Deletion through another program is outside this pack's list.

### Permissions

The pack checks recursive `chmod`/`chown`, and `chmod 777`, targeting `/`, `~`, or `$HOME`.
It denies examples such as `chmod -R 755 /`, `chown -R root ~`, and `chmod 777 "$HOME"`.
It recognizes selected root and home spellings, including `/.`, `${HOME}`, trailing slashes, `~/.`, `$HOME/.`, and some glob forms.

It allows `chmod -R 755 /tmp`, `chmod -R 755 "$HOME/project"`, and `chmod 755 /`.
It does not expand arbitrary variables or resolve a literal home path such as `/Users/alex` to `$HOME`.

### Disk wipe

The pack denies `dd` writes to recognized disk devices, including `/dev/sda`, `/dev/nvme0n1`, and `/dev/disk2`.
Supported quoting variations for `of=` also match.
It allows `dd if=/dev/sda of=/tmp/image` and `dd if=/dev/zero of=/tmp/image`.
Writes to non-disk devices such as `/dev/null`, `/dev/stdout`, and `/dev/fd/1` also allow.

The pack denies `mkfs`, `mkfs.*`, and `wipefs`, including inspection forms.
It allows `fdisk -l`, `parted -l`, and `parted /dev/sda print` with or without Bash metadata.
It denies interactive device operations such as `fdisk /dev/sda` and `parted /dev/sda`.
Mutations such as `parted -s /dev/sda mklabel gpt` also deny.
When parsing completes, additional `fdisk` list options allow: `-x`/`--list-details` and `-F`/`--list-free`.
An option value does not count as a flag: parsed `fdisk --output -l /dev/sda` still denies.

Other device-writing tools, such as `cat disk.img > /dev/disk2` or `diskutil eraseDisk`, are outside this pack's checks.
Do not rely on this pack as a complete disk-access policy.

### High-risk prompt triage

This pack denies explicit requests to decide, rank, select, approve, deny, or otherwise act on people in employment, credit, insurance, health care, benefits, law enforcement, migration, or justice contexts. For example, it denies a request to rank job candidates and choose whom to hire, or to decide whether to grant an applicant asylum.

It allows general educational requests such as explaining employment fairness or summarizing a health-care policy. The matching is a coarse regular-expression triage: it cannot determine whether a use is lawful, discriminatory, explainable, or subject to meaningful human oversight. It only runs in integrations that submit prompt preflight checks; it does not inspect prior conversation context or an assistant response.

### PHI-like data egress

This pack denies recognizable outbound tool calls and shell network commands when their arguments include either a labelled high-confidence identifier (such as an MRN, medical-record number, patient ID, or SSN) or a patient/member reference paired with diagnosis, treatment, medication, prescription, condition, or symptom information. It decodes JSON tool arguments before matching, recognizes common outbound tool names, treats named MCP tools as potentially outbound, and recognizes shell egress commands including `curl`, `wget`, `scp`, and `rsync`.

It allows health-policy text, local file writes, and outbound calls without those indicators. It cannot identify every outbound tool, inspect indirect data in files or variables, determine whether the recipient is authorized, or decide whether the use has a permitted purpose or meets a minimum-necessary standard. It is a narrow data-egress control, not a HIPAA compliance determination.

## Enforcement limits

### Runtime parser for stock command packs

Only unmodified Git safety, Destructive commands, Permissions, and Disk wipe packs support Bash parsing.
Pack enablement does not change this; editing any other field disables parsing for that rule.
Saved rules stay unchanged. Edited packs, custom regex rules, and older binaries use regex matching.

Current coding-agent integrations omit shell metadata and still use lexer-based regex matching.
Parsing requires separate host metadata identifying the final command as Bash after any host rewrites.
The tool must be named exactly `Bash` or `bash`, with a string `command` argument.
Metadata cannot activate parsing for generic tool names such as `shell`, `exec_command`, or `local_shell`.
A tool name, agent identity, or shell setting inside tool arguments is not enough.

The parser never executes shell input.
It handles supported static `env`, `sudo`, `command`, and `exec` wrappers, plus literal `bash`/`sh`/`dash` `-c` payloads.
It checks literal heredocs passed to those shells, command substitutions, and process substitutions, including substitutions in expanding heredocs.
When analysis completes, plain quoted mentions and data heredocs do not count as commands.
For example, `echo 'rm -rf /'` and a literal `cat` heredoc allow; `echo "$(rm -rf /)"` denies.
The parser does not resolve commands at runtime, expand aliases, inspect Python or other interpreter payloads, or read script files.
It cannot guarantee that arbitrary shell input is safe.

Parser limits are 64 KiB of cumulative source, 8192 syntax tree nodes, and four nested shell payloads.
Missing or unsupported metadata, unsupported syntax or executors, unresolved policy arguments, invalid syntax, or exceeded parser limits trigger regex fallback.
Separately, a shell command longer than 64 KiB fails a shell rule, so stock packs deny it with or without metadata.
Carriage return (CR) and NUL characters also trigger fallback.
Any parser-detected denial still applies; otherwise the regex result decides.

Git configuration overrides (`-c`, `--config-env`, or explicit `GIT_CONFIG_*` assignments) trigger fallback because they can select programs to execute.
Explicit `GIT_SSH` and `GIT_SSH_COMMAND` assignments also trigger fallback.
Even harmless configuration overrides use regex matching; their values are not interpreted.

Secondary Bash evaluation contexts, such as prompt expansion, arithmetic array references, and indirect expansion, can also trigger fallback.
Quoted payloads in those contexts or Git configuration can pass the lexer-based fallback despite executing dangerous commands at runtime.

Evaluation explanations report why analysis was incomplete, including in offline `--json` output.
Fallback can miss dangerous commands or block harmless text. Incomplete analysis alone does not deny a call.

### Lexer-based regex matching

The default and fallback paths use a shell lexer before applying regex patterns; they are not Bash syntax-tree analysis.
The lexer splits command boundaries, removes shell quoting and backslashes, decodes `$'...'` escapes, and drops redirections such as `2>&1`.
Stock patterns distinguish quoted data from commands: quoted `echo` arguments, comments, and ordinary heredoc bodies mentioning blocked commands generally allow.
Unquoted command names in arguments can still match: `echo rm -rf /` denies.
Matching command names after unquoted separators also catches wrappers such as `sudo`, `ssh host`, and `docker exec c`.

The lexer reads literal code in substitutions and shell `-c` arguments.
It also reads payloads for `su -c`, `env -S`, `script -c`, `eval`, `watch`, `trap`, and `find -exec`.
It reads heredocs supplied directly to shells and substitutions in expanding heredocs, but not data piped into a shell.
Nested code is read three levels deep; code at the last level is matched as written.
The lexer does not expand variables, follow aliases, read script files, or inspect arbitrary interpreter payloads.
Custom regexes also receive original command text and can match quoted mentions that stock patterns ignore.

Shell matching recognizes these tool names, ignoring case:
`Bash`, `shell`, `run_terminal_cmd`, `execute_command`, `run_command`, `terminal`, `powershell`, `pwsh`, `exec_command`, `local_shell`, `run_shell_command`, and `run_in_terminal`.
It reads every nonempty string or string array under `command`, `cmd`, and `script`, including nested keys such as `local_shell`'s `action.command`.
Arrays preserve argument boundaries through shell quoting; top-level arrays also retain plain-space joining for compatibility.
Without readable command keys, matching uses string values from the arguments; non-JSON arguments are matched as text.
Recognizing a PowerShell tool does not add patterns for every PowerShell command.
Unknown tools or argument shapes can pass without a shell or egress check.

Host hooks determine which calls reach guards and whether returned transforms can be applied.
For example, Codex guards only its supported Bash, patch, and MCP tool types.
For host-specific behavior, refer to the guard sections for [Claude Code](../plugins/claude-code/README.md#guards), [Codex](../plugins/codex/README.md#guards), [Copilot](../plugins/copilot/README.md#guards), [Cursor](../plugins/cursor/README.md#guards), [OpenCode](../plugins/opencode/README.md#guards), [pi](../plugins/pi/README.md#guards), and [Vibe](../plugins/vibe/README.md#guards).

## Local and Cloud decisions

The daemon evaluates local rules first.
A local deny stops the request without relaying it to Cloud.
If forwarding is configured and the local check allows the call, the daemon also asks Cloud for a decision.
Local redactions apply before that relay.

Failures occur at different layers:

| Failure | Behavior |
| --- | --- |
| Missing, unreadable, unparsable, or empty `guards.toml` | No local rules block calls. |
| A rule fails compilation | The daemon skips that rule and evaluates the valid rules. |
| A local deny is returned | Cloud fail-open settings do not override it. |
| Cloud relay fails | `AGENTO11Y_GUARDS_FAIL_OPEN` chooses allow on failure (`true`, the default) or deny (`false`). |
| The host cannot obtain a guard response | Host timeout and failure handling apply; refer to the host's guard documentation. |

Setting `AGENTO11Y_GUARDS_FAIL_OPEN=false` does not make a broken rules file fail closed.
The app displays compilation errors.
`agento11y doctor` reports the resolved file path, errors, and number of locally enforceable rules; it also probes configured endpoints.

## Test without running a dangerous command

Use `agento11y guards test` to submit command text to the local evaluator without executing it.

If you ask a guarded agent to run a dry run, its guard can match the dangerous text inside the quoted argument.
That can block the outer command before `guards test` starts.
Run the complete dry-run commands directly in your terminal instead; do not run the inner commands alone.
A host-hook rejection is not a dry-run result.

```sh
agento11y guards test 'rm -rf /tmp/agento11y-guard-example'
agento11y guards test 'git reset --hard'
agento11y guards test 'git push --force-with-lease'
agento11y guards test --json 'cat .env'
```

With only the relevant unmodified packs enabled, the first two calls deny, the third allows, and File safety denies the fourth.
Keep the command text quoted and pass it only to `guards test`.
Never run a destructive command to prove that a guard works.

The dry run contacts no endpoints, starts no daemon, and writes no files.
It evaluates the saved rules even if host guards are disabled.
A successful deny proves the local rule matched the synthetic request; it does not prove your running agent sends guard requests.

Tests use regex matching by default, even with the default tool name `Bash`.
Add `--shell bash` to test Bash parsing. Only `bash` is supported; `--tool` must remain `Bash` or `bash`.
The flag does not change `--tool` or verify the host shell.
Compare results with and without parsing:

```sh
agento11y guards test --json 'git clean -n -f'
agento11y guards test --shell bash --json 'git clean -n -f'
```

With Git safety alone, the default test denies separate flags; the parsed test allows this dry run.
Both paths allow the clustered form `git clean -nf`.
Check JSON evaluation explanations even when verdicts agree: they identify incomplete analysis and fallback.

Exit codes are `0` for allow, `1` for deny, and `2` for an input, file, compilation, or evaluation error.
For flags, JSON output, and synthetic-request limitations, refer to [Test local guards offline](../plugins/agento11y/README.md#test-local-guards-offline).
Use fake values for redaction tests: output can include the original command and rewritten arguments.

## Create a custom rule

Add `[[rules]]` entries to the `guards.toml` path shown in **Security** > **Guards**, preserving existing rules.
This example denies the literal command form `git push`:

```toml
[[rules]]
rule_id = "custom.block-git-push"
enabled = true
phase = "postflight"
priority = 5
action_on_fail = "deny"

  [[rules.evaluators]]
  kind = "regex"
  config.target = "shell_command"
  config.reject = true
  config.patterns = ['(?i)\bgit\s+push\b']
```

| Field | Meaning |
| --- | --- |
| `rule_id` | Rule identifier shown in results. Use a unique name outside the `pack.*` names used by bundled packs. |
| `enabled` | Whether the rule runs. Defaults to `true`; set `false` to disable it. Custom rules have no pack switch. |
| `phase` | When the rule runs. `postflight` checks the proposed tool call before execution; it is the default. |
| `priority` | Evaluation order, lowest first. Defaults to `0`. |
| `action_on_fail` | What happens when a check fails: `deny` blocks, `warn` records the failure and continues, and `allow` ends local evaluation without running later rules. Defaults to `deny`. |
| `evaluators` | Checks attached to this rule. Each `[[rules.evaluators]]` starts another check. |
| `kind` | Evaluator type. `regex` runs locally; Cloud-only kinds do not. |
| `config.target` | Text to inspect. `shell_command` selects arguments from recognized shell tools, including original text and lexer-derived commands. See [Lexer-based regex matching](#lexer-based-regex-matching). Commands longer than 64 KiB fail the check. |
| `config.reject` | With `true`, any pattern match fails the check. With `false` (the default), no match fails the check. |
| `config.patterns` | List of Go regular expressions. TOML single-quoted strings preserve backslashes. `(?i)` makes this example case-insensitive. |

An earlier `allow` rule can prevent this rule from running.
The regex can match quoted mentions and miss Git global options, aliases, or scripts; it is not a complete policy against pushes.
As with packs, enforcement requires guards enabled and local routing. Saved edits apply on the daemon's next check.

To check a command without executing it, use `agento11y guards test 'git push origin HEAD'`.

## Pack updates and upgrades

Packs are stored as rules named `pack.secrets`, `pack.files`, `pack.git`, `pack.destructive`, `pack.permissions`, `pack.disk`, `pack.high_risk_prompt_triage`, and `pack.phi_egress`.
Keep custom rules under different IDs.

An enabled switch identifies a stored rule; it does not prove that the rule compiled or still matches the shipped definition.

Binary upgrades can refresh unchanged older pack definitions in memory without rewriting the file.
Edited definitions remain yours; absent and disabled packs stay absent and disabled.
Turning a pack off removes its stored definition, including edits to that pack.
Turning it back on creates the definition shipped with the installed binary.
