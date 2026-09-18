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
It allows `cat .envrc`, `cat .environment`, and `cat config.env` when no other rule blocks them.
A file named `.env.example` also matches, even if it contains no secrets.
Other filenames containing secrets are outside this pack's scope.

A different tool name, a symlink, or a program that opens a file internally can avoid these checks.
The pack does not resolve paths or inspect file contents.

### Git safety

The pack denies these command forms, including supported option-order and quoting variations, even for intentional cleanup:

- `git reset --hard`
- `git push --force` and `git push -f`
- `git clean -f`
- `git checkout --`
- `git stash drop` and `git stash clear`
- `git branch -D`

It allows `git push --force-with-lease`, `git push --force-if-includes`, and ordinary pushes.
Other commands that rewrite history, such as rebase, are outside this pack's list.
Git aliases are not expanded.

### Destructive commands

The pack denies `rm -rf`, `rm -fr`, separate flags such as `rm -r -f`, and long forms such as `rm --recursive --force`.
The target does not matter: deleting a temporary build directory matches too.

It allows `rm -r`, `rm -f`, and plain `rm` because they do not combine recursive and force options.
Deletion through another program is outside this pack's list.

### Permissions

The pack checks recursive `chmod`/`chown`, and `chmod 777`, targeting `/`, `~`, or `$HOME`.
It denies examples such as `chmod -R 755 /`, `chown -R root ~`, and `chmod 777 "$HOME"`.
It recognizes selected root and home spellings, including `${HOME}`, trailing slashes, and some glob forms.

It allows `chmod -R 755 /tmp`, `chmod -R 755 "$HOME/project"`, and `chmod 755 /`.
It does not expand arbitrary variables or resolve a literal home path such as `/Users/alex` to `$HOME`.

### Disk wipe

The pack denies `dd if=/dev/zero of=/dev/sda`, including supported quoting variations for `of=`.
It allows `dd if=/dev/sda of=/tmp/image`: the output is not a device path.
It also allows `dd if=/dev/zero of=/tmp/image`.

The other patterns match `mkfs`, `mkfs.*`, `wipefs`, `fdisk`, and `parted` without distinguishing destructive options from inspection options.
For example, `fdisk -l` matches even though it lists partitions.
Do not rely on this pack as a complete disk-access policy.

### High-risk prompt triage

This pack denies explicit requests to decide, rank, select, approve, deny, or otherwise act on people in employment, credit, insurance, health care, benefits, law enforcement, migration, or justice contexts. For example, it denies a request to rank job candidates and choose whom to hire, or to decide whether to grant an applicant asylum.

It allows general educational requests such as explaining employment fairness or summarizing a health-care policy. The matching is a coarse regular-expression triage: it cannot determine whether a use is lawful, discriminatory, explainable, or subject to meaningful human oversight. It only runs in integrations that submit prompt preflight checks; it does not inspect prior conversation context or an assistant response.

### PHI-like data egress

This pack denies recognizable outbound tool calls and shell network commands when their arguments include either a labelled high-confidence identifier (such as an MRN, medical-record number, patient ID, or SSN) or a patient/member reference paired with diagnosis, treatment, medication, prescription, condition, or symptom information. It decodes JSON tool arguments before matching, recognizes common outbound tool names, treats named MCP tools as potentially outbound, and recognizes shell egress commands including `curl`, `wget`, `scp`, and `rsync`.

It allows health-policy text, local file writes, and outbound calls without those indicators. It cannot identify every outbound tool, inspect indirect data in files or variables, determine whether the recipient is authorized, or decide whether the use has a permitted purpose or meets a minimum-necessary standard. It is a narrow data-egress control, not a HIPAA compliance determination.

## Enforcement limits

Prompt and shell rules match text with regular expressions.
They recognize selected syntax variations, but do not interpret the shell, expand arbitrary variables, follow aliases, or decode executable payloads.
Quoted text and heredoc bodies can match even when they only describe a command.
Commands hidden inside scripts or other programs can pass.

Shell matching recognizes `Bash`, `shell`, `run_terminal_cmd`, `execute_command`, `run_command`, `terminal`, `powershell`, and `pwsh`, ignoring case.
It reads a nonempty string or string array from `command`, `cmd`, or `script` arguments.
Arrays are joined with spaces without reconstructing quoting, so matching can differ from the tool's interpretation of the arguments.
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
| `config.target` | Text to inspect. `shell_command` selects decoded arguments from recognized shell tools. |
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
