---
name: setup-coding-agent
description: >-
  Set up Grafana Agent observability for a coding agent (Claude Code, Codex,
  Copilot CLI, Cursor, OpenCode, pi, or Vibe) with the agento11y binary:
  install it, save credentials, wire the agent, verify a session reaches
  Grafana Cloud, and diagnose a broken pipeline. Use when the user says "set
  up agento11y", "monitor my coding agent", "agento11y doctor says something
  is wrong", "my sessions do not show up in Grafana", or asks why
  conversations arrive but analytics stay empty.
---

# Set up a coding agent with Grafana Agent observability

Grafana Agent observability records what a coding agent does: each model call,
its tokens, its cost, and (when the user opts in) the conversation content. The
`agento11y` binary is the launcher and hook host that sends that data to Grafana
Cloud. Your job in this skill is to get one machine from nothing to a session
visible in Grafana Cloud.

This is the coding-agent path. It installs a binary and writes a config file. It
never changes the user's application code.

## Rules

Follow all five. Each one exists because breaking it produces a setup that looks
finished and is not.

1. **Never mint, generate, or guess a credential.** The token is a Grafana Cloud
   access-policy token that only the user can create. Ask for it, or tell the
   user where to click. A made-up token produces a 401 and a wasted hour.
2. **Never invent an endpoint URL.** The API URL and the OTLP endpoint come from
   the Connection page of the user's own stack. Do not assemble one from the
   stack name; regions differ.
3. **Never print or echo a token.** Read it from stdin with `--token-stdin`, or
   let `agento11y login` prompt for it.
4. **Do not edit application code.** Adding SDK calls to the user's app is a
   different job ("Path B"). If the user wants their own app or agent
   instrumented, stop and hand off to the `agento11y-instrument` skill, which
   ships with the `gcx` CLI (`gcx agent skills install agento11y-instrument`).
5. **Do not turn on content capture without asking.** The default,
   `metadata_only`, keeps prompts, responses, and tool I/O on the machine.
   Changing it sends conversation text to Grafana Cloud.

## Step 1: Read the current state

Run this first, every time, before changing anything:

```sh
agento11y doctor --json
```

If the command is not found, the binary is not installed: go to Step 2.

The report has five parts. Branch on them:

| Field | Meaning | Where to go |
| --- | --- | --- |
| `agento11y.version` | The installed build. `dev` means a local build, not a release. Always present when the command runs at all. | command not found: Step 2 |
| `config.exists` | Whether `~/.config/agento11y/config.env` is there. | `false`: Step 3 |
| `conversations.status` | The transcript pipeline: `ok`, `warning`, `error`. | `error`: Troubleshooting |
| `analytics.status` | The OTLP metrics and traces pipeline. | `error`: Troubleshooting |
| `agents[]` | One entry per coding agent, with `install_state` and `on_path`. | target agent not `installed`: Step 4 |

`agento11y doctor` exits 1 when conversations, analytics, or config is in
`error`. The agent list never fails the command: an agent the user does not have
is reported, not treated as a problem.

Run `agento11y doctor` without `--json` when you want the human report, and
share `--json` output when the user asks for something to send to support. The
JSON never contains the token value, only its first few characters.

## Step 2: Install the binary

Three methods. Pick by platform, and prefer the one that matches how the user
installs other tools.

**Quick install (Linux and macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
```

The script downloads the latest release for the OS and architecture, checks its
SHA-256, and installs to `~/.local/bin`. Re-run it to upgrade. `INSTALL_DIR`
changes the directory and `VERSION` pins a release.

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

Upgrade with `brew upgrade grafana/grafana/agento11y`.

**Go install (any platform with Go 1.25+, and the usual choice on Windows):**

```sh
go install github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y@latest
```

This writes to `go env GOPATH`/bin, or `GOBIN` when set. On Windows the other
option is the `windows_amd64` or `windows_arm64` zip from the releases page:
extract `agento11y.exe` and put it on `PATH`.

Whichever method you use, the install directory has to be on `PATH`. Check the
result:

```sh
agento11y --version
```

The command was renamed from `sigil`. The quick-install script also creates a
`sigil` symlink next to the binary, so the old name keeps working there.
`go install` installs only the binary you name. The old name will be removed in
a future release, so do not write new instructions against `sigil`.

## Step 3: Save credentials

Four values are needed. All four are on the **Connection** tab of the Agent
Observability app in the user's own stack:

```
https://<your-stack>.grafana.net/plugins/grafana-agento11y-app
```

| Value | Config key | What it is |
| --- | --- | --- |
| API URL | `AGENTO11Y_ENDPOINT` | The conversations ingest URL, e.g. `https://agento11y-prod-<region>.grafana.net` |
| Instance ID | `AGENTO11Y_AUTH_TENANT_ID` | The stack's numeric instance ID |
| API token | `AGENTO11Y_AUTH_TOKEN` | An access-policy token, `glc_...` |
| OTLP endpoint | `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | The OTLP gateway URL for metrics and traces |

The token needs four scopes: `sigil:write`, `metrics:write`, `traces:write`, and
`logs:write`. One token covers both pipelines. The Connection page has a *Create
a token in Cloud Access Policies* link; ask the user to follow it. Do not create
a token on their behalf.

The OTLP endpoint has its own link on the same page, *Set
`OTEL_EXPORTER_OTLP_ENDPOINT` for traces and metrics*, which opens the Grafana
Cloud OpenTelemetry tile with the values ready to copy.

Once the user has the four values, save them without putting the token in shell
history:

```sh
printf %s "$TOKEN" | agento11y login \
  --endpoint https://agento11y-prod-<region>.grafana.net \
  --tenant <instance-id> \
  --token-stdin \
  --otlp-endpoint https://otlp-gateway-prod-<region>.grafana.net/otlp
```

`--endpoint`, `--tenant`, and a token together need no terminal, so this form
works over SSH, in a devcontainer, and from a script. Before writing the file,
login sends one request to the endpoint with the credentials and reports what
came back.

If that check fails, what happens next depends on the terminal. Interactively,
login asks whether to save anyway. In a script the question is never asked,
because a piped stdin is not a terminal: login writes nothing and exits 1. Pass
`--yes` to save the values regardless, or `--no-verify` to skip the check.

With no flags, `agento11y login` prompts for all four values, then offers an
optional preferences step: content capture mode, session tags, guards and their
timeout, and automatic tags. Answering yes to automatic tags unfolds a checklist
of which values to attach. Leave the preferences at their defaults unless the
user asks otherwise, and stop at the automatic-tags question: read the warning
under **Automatic tags** below before answering yes.

Login writes the file to `~/.config/agento11y/config.env`. An install that still
has only the old `~/.config/sigil/config.env` keeps using that file. You can
write it by hand instead:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

## Step 4: Wire the coding agent

Six agents launch through `agento11y <agent>`. The launcher installs or refreshes
the host plugin, prompts for credentials on a first run, and then replaces itself
with the agent, so the user's normal workflow is unchanged apart from the prefix.

| Agent | Command | Mechanism |
| --- | --- | --- |
| Claude Code | `agento11y claude` | shared Go binary, plugin `agento11y-claude-code` |
| Codex | `agento11y codex` | shared Go binary via hooks |
| Copilot CLI | `agento11y copilot` | shared Go binary via hooks |
| OpenCode | `agento11y opencode` | installs `@grafana/agento11y-opencode` |
| pi | `agento11y pi` | installs `@grafana/agento11y-pi` |
| Vibe | `agento11y vibe` | shared Go binary via `hooks.toml` |

**Cursor is the exception.** It is a GUI application with no launcher, so it is
wired once and stays wired:

```sh
agento11y cursor install     # merges the hook into ~/.cursor/hooks.json
agento11y cursor uninstall   # removes it
```

Arguments after `--` go to the underlying CLI unchanged:

```sh
agento11y claude -- --resume
```

Suggest the user alias the prefix (for example `alias claude='agento11y claude'`)
only if they ask; silently shadowing their agent command is surprising.

## Step 5: Verify

1. Run the agent for one turn. Any prompt will do.
2. Open Grafana Cloud, then Agent Observability, then **Conversations**. A new
   conversation appears within a few seconds.
3. If nothing appears, run `agento11y doctor` and read the launcher's stderr for
   lines starting with `agento11y:`.

Confirm both pipelines, not just one. Conversations and analytics fail
independently, and the most common half-broken setup is conversations arriving
while the Usage and Cost views stay empty.

## Troubleshooting

`agento11y doctor` is the first command for every problem. It is read-only, it
sends one lightweight request to each endpoint, and it reports the HTTP status,
so a wrong URL or a token missing a scope shows up as a broken pipeline rather
than as silence.

### The two pipelines

They are independent and fail independently.

- **Conversations** (the chat transcripts) export over `AGENTO11Y_ENDPOINT`,
  `AGENTO11Y_AUTH_TENANT_ID`, and `AGENTO11Y_AUTH_TOKEN`. The token needs the
  `sigil:write` scope.
- **Analytics** (the metrics and traces) export over
  `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`, or the vendor-neutral
  `OTEL_EXPORTER_OTLP_ENDPOINT`. The token needs `metrics:write` and
  `traces:write`.

Conversations working while analytics stay empty means the OTLP endpoint is
unset or the token lacks the metrics and traces scopes. Doctor flags that case
by name.

### Reading the report row by row

- **endpoint** and **tenant id**: the resolved value plus the variable it came
  from. `not set` means neither spelling was found. Doctor names the winning
  spelling because both `AGENTO11Y_*` and the older `SIGIL_*` are accepted.
- **auth token**: `set` plus a short prefix. The value is never printed.
- **probe**: the HTTP status the endpoint returned for a test request, and the
  URL it was sent to. `no response` means nothing answered: wrong host, DNS
  failure, or a blocked network.
- **file**: the config path and whether it exists.
- **content capture**: the effective mode and where it came from. An invalid
  value falls back to `metadata_only` and the section message names the variable
  to fix.
- **Coding agents**: one row per agent. `not found on PATH` means the CLI is not
  installed. `on PATH, plugin not installed` means the agent is there but has
  never been launched through `agento11y`. `install state unknown` means the
  probe itself failed, usually an unreadable hook file.

### Status codes

- **401**: the endpoint refused the credentials without saying why. The token
  may be invalid or expired, or `AGENTO11Y_AUTH_TENANT_ID` may be wrong.
- **403**: the token is valid but missing a write scope. Check `sigil:write` for
  conversations and `metrics:write` plus `traces:write` for analytics.
- **A redirect (301, 302) on the conversations probe**: `AGENTO11Y_ENDPOINT` is
  not an API URL. A stack URL or an app-page URL redirects to a login page. Copy
  the API URL from the Connection page.

### Debug logging

Hooks always exit 0 so a failure can never crash the host agent, which means a
hook problem is invisible on the terminal. Turn on the log:

```dotenv
# ~/.config/agento11y/config.env
AGENTO11Y_DEBUG=true
```

Then read it:

```sh
tail -f ~/.local/state/agento11y/logs/agento11y.log
```

An install that still has the pre-rename `~/.local/state/sigil` directory keeps
using it, with the new `agento11y.log` file name.

For a support ticket, capture `agento11y doctor --json`. It never includes the
token value.

## Reference

### Config keys

All hosts read `~/.config/agento11y/config.env`.

| Key | Meaning |
| --- | --- |
| `AGENTO11Y_ENDPOINT` | Conversations API URL |
| `AGENTO11Y_AUTH_TENANT_ID` | Instance ID |
| `AGENTO11Y_AUTH_TOKEN` | Access-policy token |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP endpoint for metrics and traces |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | Which content fields ship (see below) |
| `AGENTO11Y_TAGS` | `key=value,key=value` attached to every generation |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | Opt-in automatic user, repo, and branch tags |
| `AGENTO11Y_LOCAL` | Route `agento11y <agent>` launches to the local daemon |
| `AGENTO11Y_AUTO_UPDATE` | `false` opts out of host-plugin refresh |
| `AGENTO11Y_DEBUG` | `true` writes the debug log |

Every key also has an older `SIGIL_*` spelling. Doctor reports which one won.

### Tagging sessions

`--tag key=value` is repeatable and goes before any `--`. It attaches tags to
every generation the launched session produces, and merges onto (and overrides)
`AGENTO11Y_TAGS`:

```sh
agento11y claude --tag project=hackathon --tag team=ai -- --resume
```

Every launcher accepts it.

### Automatic tags

`AGENTO11Y_AUTO_CODING_AGENT_TAGS=true` resolves the session's user, repository,
and branch and attaches them as **client** tags, which are the tags that also
become OTel metric labels. That is what lets the Usage and Cost view break down
by person, repository, or branch. The switch is off by default, and on its own
it enables all three names.

| Name | Tag key | Resolved from |
| --- | --- | --- |
| `user` | `user` | `AGENTO11Y_USER_ID`, then the identity the host agent knows, then the OS account name |
| `repo` | `repo` | `owner/name` of the checkout's `origin` remote, or the checkout directory name |
| `branch` | `git.branch` | Branch checked out in the session's directory |

Narrow the list with `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo`. A key
already in `AGENTO11Y_TAGS` wins, an unresolved value leaves its key off, and an
unsupported name is logged and skipped. In Prometheus these arrive as
`agento11y_tag_user`, `agento11y_tag_repo`, and `agento11y_tag_git_branch`.

Warn the user before turning this on. The `user` value is commonly a work email
address, it becomes a Prometheus label, and it is kept for the metric retention
period. Every distinct combination of values is one more time series, and branch
names grow without bound: start with `user,repo` and add `branch` only for
per-branch cost. Run `agento11y doctor` to see the exact values first.

### Content capture modes

Coding-agent plugins default to `metadata_only`: model, tokens, tool names,
timing, and cost ship; prompts, responses, and tool I/O stay on the machine.

| Mode | What ships |
| --- | --- |
| `metadata_only` | Structure only: roles, tool names, model, tokens, timing, IDs |
| `no_tool_content` | Full conversation content, but no tool arguments or results |
| `full` | All content, including tool arguments and results |
| `full_with_metadata_spans` | Full content over the ingest API, no content on OTel spans |

Set it with `AGENTO11Y_CONTENT_CAPTURE_MODE`. Unknown values fall back to
`metadata_only` with a warning; `default` is accepted as an alias for
`metadata_only`. Every plugin redacts known secret formats from captured
content, but a capture mode chooses which fields ship, not whether the shipped
fields are clean. Treat content capture as opt-in on shared machines.

### Local mode

`agento11y <agent> --local` records the session to a JSONL store on this machine
and opens a viewer at `http://127.0.0.1:8765`. A local session reaches Grafana
Cloud only when `AGENTO11Y_LOCAL_FORWARD` is on. Manage the daemon by hand with
`agento11y local start|status|stop`.

Local mode covers launches that go through `agento11y <agent>`, and nothing
else. A session the host agent starts on its own, such as Cursor or a plain
`claude`, keeps exporting to the configured endpoint. Never describe local mode
to the user as a switch that keeps all data on the machine: `agento11y doctor`
prints that same correction whenever local mode is on.

`--no-local` runs one session against Cloud while `AGENTO11Y_LOCAL=true` stays
set.

### History import

The viewer and Grafana Cloud both start empty: they hold only sessions captured
after the install. `agento11y history import` backfills sessions an agent
already wrote to disk. Supported agents are `claude-code`, `codex`, and `pi`.

```sh
agento11y history import claude-code --dry-run   # plan only, nothing exported
agento11y history import claude-code --local     # into the local store
agento11y history import claude-code             # into Grafana Cloud
```

Without `--since`, an import covers the last 90 days. Each imported turn is
recorded in a ledger, so a repeated import skips what it already sent and a
cancelled run resumes. Without a terminal there is no picker and no
confirmation: pass `--all --yes` from a script.

### Auto-update

`agento11y claude`, `agento11y codex`, and `agento11y opencode` refresh the
installed host plugin at most once a day, and always on the first launch after
the `agento11y` binary itself changes version. A relaunch inside that window
changes nothing, so do not tell the user a restart picks up a plugin fix.
`AGENTO11Y_AUTO_UPDATE=false` opts out.

Auto-update does not apply to the other launchers: `copilot` and `vibe` always
rewrite their own hook entries, and `pi` leaves upgrades to pi's own installer.

## Further reading

- Product docs: <https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/>
- Source: <https://github.com/grafana/agento11y>
