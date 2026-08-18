# Jamf deployment for Claude Code and Cursor

This package gives a Jamf administrator a repeatable, per-user installation of
Grafana Agent Observability for Claude Code and Cursor. It is intentionally
split into a machine-level binary deployment and a user-level bootstrap: both
Claude's plugin store and Cursor's `hooks.json` are under the developer's home
directory.

## Policy sequence

1. Package a released macOS `agento11y` binary for the matching architecture
   and install it at `/usr/local/bin/agento11y` with root ownership and mode
   `0755`. Use the release checksum before creating the package.
2. In a Jamf **user-context login policy**, create
   `<console-user-home>/.config/agento11y/config.env`, owned by that user and
   mode `0600`, with the values shown in [`config.env.example`](config.env.example).
   Retrieve the access-policy token from your Jamf secret/parameter mechanism at
   policy execution time; do not put it in the script, package, policy name, or
   logs. Run the bootstrap from this policy after writing the file. The
   LaunchAgent below handles later logins; if it runs before config delivery, it
   reports `deferred_missing_config` and leaves a receipt rather than failing.
3. Install [`bootstrap.sh`](bootstrap.sh) as
   `/usr/local/libexec/agento11y/jamf-bootstrap.sh`, then install
   [`com.grafana.agento11y.reconcile.plist`](com.grafana.agento11y.reconcile.plist)
   in `/Library/LaunchAgents` with root ownership and mode `0644`. The
   LaunchAgent runs the bootstrap once in each developer's login context. Jamf
   normally runs policy scripts as
   root; the script resolves the console user, then re-executes the installer in
   that user's launchd context. It invokes `agento11y agents reconcile --agents
   claude,cursor --json`: Claude is skipped with `missing_host` when its CLI is
   not installed; Cursor hooks are merged idempotently into
   `~/.cursor/hooks.json`. It writes the same secret-free receipt to
   `~/Library/Application Support/agento11y/jamf-reconcile.json`.
4. Record that receipt in a Jamf extension attribute, taking care not to log
   `config.env`. The stable fields are `schema_version`, overall `status`,
   `agento11y.version`, `config.revision`, and per-agent results. Deferred
   receipts retain those fields with `null` for unavailable version/revision.
   Status is
   `converged`, `deferred_missing_config`, `deferred_missing_host`, or `error`;
   the root policy emits `deferred_no_user` when no user is logged in. A later
   policy can run
   `agento11y doctor --json` in the same user context to verify configuration
   and hook installation.

The bootstrap never launches Claude Code or Cursor and never prompts. Re-run it
after upgrades; the installers are idempotent and preserve other Cursor hook
entries. Set `AGENTO11Y_MANAGED_CONFIG_REVISION` in the managed config and
increment it for a policy or credential change; it is inventory metadata only,
not a security control or a replacement for token rotation.

## Rollout test

The repository test exercises the user-context bootstrap contract on every
platform without a real Jamf tenant or credentials:

```sh
./deploy/jamf/test-bootstrap.sh
```

It verifies missing-config deferral, stable receipt persistence, the
LaunchAgent user PATH, and a repeat reconcile through a fake `agento11y`
binary. It cannot exercise the
macOS-specific root-to-console-user handoff (`launchctl asuser`), so run the
following pilot once before fleet rollout.

Before a fleet rollout, use a disposable macOS test account with Claude Code
and Cursor installed:

```sh
install -d -m 700 "$HOME/.config/agento11y"
install -m 600 config.env.example "$HOME/.config/agento11y/config.env"
AGENTO11Y_BIN=/usr/local/bin/agento11y ./bootstrap.sh
agento11y doctor --json
```

Confirm that the bootstrap result has `status: "converged"` and reports
`installed` or `already_installed`
for each present host; open Cursor once and make a short Claude session, then
verify their data appears in Agent Observability. Test the policy again under
the same account to confirm it is idempotent and inspect `~/.cursor/hooks.json`
to ensure pre-existing hooks remain intact.

For a user who does not yet have Claude Code installed, `missing_host` is an
expected successful result. Run the bootstrap again after Claude Code is
available for that user.
