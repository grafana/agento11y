---
name: setup-local-guards
description: >-
  Help choose, configure, and test local agento11y guard packs for coding-agent
  tool calls. Use when the user asks to enable safety packs, protect .env files,
  block dangerous commands, redact tool arguments, create custom local rules,
  or diagnose local guards.
  Covers local packs, not Grafana Cloud rule management or application SDK setup.
---

# Set up local guard packs

Inspect the installed configuration, explain the relevant packs, ask for approval,
then configure and test only the user's choices.
For a question about existing behavior, stop after explaining it; do not change settings.

## Safety rules

- Ask before enabling or disabling guards, changing packs, editing rules, or changing the session destination.
- Preserve custom rules, credentials, capture settings, and the user's Cloud forwarding choice.
- Never execute a dangerous command to test a guard. Pass quoted command text only to `agento11y guards test`.
- Never test with real secrets or print `config.env`; it can contain credentials.
- Explain that guards cover only tool calls submitted through the integration. They are not an operating-system sandbox.
- Do not claim live protection from an offline dry run. A matching rule and a working host hook are separate checks.

## 1. Inspect the installation

Run:

```sh
agento11y --version
agento11y guards test --help
agento11y local status
```

If the binary or local guard commands are missing, explain what is missing.
Offer `agento11y skills show setup-coding-agent` for installation or host integration setup.
Do not assume an older binary includes this skill or every pack.
Local mode requires macOS or Linux.

Identify the target coding agent, its installed agento11y integration, and whether its sessions use the local daemon or go directly to Cloud.
Opening the local app does not route a running agent through it.
For Codex, ask the user to open `/hooks` and trust the agento11y hooks after installation.

Use the running app's **Settings** > **Local** > **Guards** to inspect the saved enablement, rules path, pack selections, and compilation errors.
If the app is stopped, ask before running `agento11y local open` to start it.
Use the displayed rules path rather than assuming `~/.config/agento11y/guards.toml`.
Read an existing rules file before proposing edits; do not replace it with a template.

Use `agento11y doctor --json` if installation or routing needs further diagnosis.
Explain that doctor probes configured endpoints and obtain approval before those requests.

## 2. Help the user choose

Use the installed app's pack descriptions and **view** previews to explain available packs.
For blocked and allowed examples, refer to the [local guard pack guide](https://github.com/grafana/agento11y/blob/main/docs/local-guards.md).
The online guide can describe a different binary version; prefer the installed catalog and dry-run results when they differ.
Do not require a network lookup to proceed with the installed catalog.

Before recommending Secret redaction, check whether the target host applies argument rewrites.
Copilot CLI applies them; Copilot Chat in VS Code and unidentified Copilot surfaces drop them and run the original arguments.
Do not present an offline redaction result as tool-argument protection on those surfaces.

Explain the trade-offs relevant to the user's request:

- Secret redaction rewrites recognized secrets in tool arguments. It can change what the tool receives and break commands that need those values.
- File safety matches `.env` and `.env.*`, including `.env.example`. It does not protect every secret file.
- Git safety allows `--force-with-lease`; it does not block every history rewrite.
- Destructive commands blocks recursive force deletion even for temporary build output, but does not block every deletion.
- Permissions targets selected root and home spellings, not every directory.
- Disk wipe uses broad command-name patterns that can also block read-only inspection.

State that text matching can miss indirect operations and can block harmless mentions.
Unknown tools, unsupported hooks, and scripts that perform dangerous operations internally can escape these checks.

Explain any destination change before asking for approval.
Local mode stores full session content locally.
With Cloud forwarding, guard payloads can leave the machine even when generation capture is `metadata_only`.
Never enable forwarding just to make local guards work.

Ask which packs to enable or disable, then confirm the exact changes.
Do not select every pack by default.
Before changing packs in a hand-edited file, offer a backup: pack updates remove TOML comments and formatting.

## 3. Apply approved changes

Configure bundled packs in the app or custom rules in `guards.toml`.

### Bundled packs

Use **Settings** > **Local** > **Guards** to enable guards and toggle only the approved packs.
If you cannot operate the app, give the user those steps and wait for confirmation.
There is no `agento11y guards enable` or `agento11y guards install` command; do not invent one or reconstruct shipped pack regexes.

The switches save immediately: enablement goes in `config.env`, and pack rules go in the adjacent `guards.toml`.
Turning off the main switch removes all known pack rules when the file can be parsed.
Turning it back on does not restore the selection; custom rules remain.
Those custom rules still apply to requests from agents that have not picked up the disabled setting.
A broken file stays unchanged when the main switch is disabled.

Disabling a pack removes edits to that pack; re-enabling creates the shipped definition.
Stop on save or compilation errors rather than claiming the pack is active.

For launcher-based sessions, use `agento11y <agent> --local` after approval, replacing `<agent>` with the target launcher name.
For hook-based setup, follow `setup-coding-agent` rather than guessing host configuration.
Restart the coding agent after changing enablement or destination, and inspect explicit environment overrides if saved settings do not take effect.
Pack-rule edits apply on the next daemon check without a daemon restart.

### Custom rules

Custom rules are `[[rules]]` entries in `guards.toml`; preserve existing rules and pack definitions.
Refer to the [custom-rule example](https://github.com/grafana/agento11y/blob/main/docs/local-guards.md#create-a-custom-rule) for TOML syntax.

| Field | Meaning |
| --- | --- |
| `rule_id` | Unique identifier shown in results. Keep custom IDs outside `pack.*`. |
| `enabled` | Defaults to `true`; `false` disables the rule. Custom rules have no pack switch. |
| `phase` | `postflight` checks tool calls before execution and is the default. |
| `priority` | Lower values run first; defaults to `0`. |
| `action_on_fail` | `deny` blocks, `warn` continues, and `allow` ends local evaluation, skipping later rules. Defaults to `deny`. |
| `evaluators` | Checks attached to the rule, each written as `[[rules.evaluators]]`. |
| `kind` | `regex` runs locally. Cloud-only evaluator kinds do not. |
| `config.target` | `shell_command` selects decoded arguments from recognized shell tools. |
| `config.reject` | `true` fails on any pattern match; `false` (the default) fails when none match. |
| `config.patterns` | Go regular expressions. Use TOML single-quoted strings to preserve backslashes. |

Use `agento11y guards test '<command>'` for a local dry run.

## 4. Test the saved rules

Choose dry runs for the selected packs, including a denied case and an allowed case.
If your own tool calls pass through guards, ask the user to run the complete dry-run commands directly in a terminal.
The outer tool call can match the dangerous text inside the quoted argument and be blocked before `guards test` starts.
Do not disable guards or disguise the command to get past that check.
A host-hook rejection is not a completed dry run.
Remind the user to run the full `agento11y guards test` command, never the inner command alone.

```sh
agento11y guards test 'git reset --hard'
agento11y guards test 'git push --force-with-lease'
agento11y guards test 'rm -rf /tmp/agento11y-guard-example'
agento11y guards test --json 'cat .env'
```

With unmodified Git safety alone, the first command denies and the second allows.
Destructive commands denies the third; File safety denies the fourth.
Other rules can change those results.
Use `--rules` before the quoted command when testing an explicit file.
Use only synthetic, non-secret values for redaction tests.

The command executes none of the submitted text, contacts no endpoints, and changes no files.
It evaluates saved rules even when host guards are disabled.
Exit `0` means allow, `1` means deny, and `2` means an error, including compilation errors.
Read notices and errors: a missing default file or no enforceable rules can return allow.
`--tool` changes the synthetic tool name, not its `{"command": ...}` argument shape; it does not replay arbitrary file-tool calls.

For unexpected results, compare the rules path, enabled rules, tool name, and exact input before changing policy.
A malformed or unreadable file can leave no local rules enforcing.
`AGENTO11Y_GUARDS_FAIL_OPEN=false` does not make such a file fail closed.
Do not disable unrelated packs or broaden exceptions to make a test pass.

## 5. Report what is verified

List the resolved file path, selected packs, changes made, dry-run results, and any remaining errors.
Separate these conclusions:

- Saved policy: which examples the local evaluator allowed, denied, or redacted.
- Host integration: whether the target session uses local routing with guards enabled, and what evidence confirms it.

If no live host check was observed, say that host enforcement remains unverified.
Do not attempt a destructive live test to close that gap.
