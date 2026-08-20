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

Grafana Agent observability records model calls, tokens, timing, tool names, and
reported or estimated cost. Conversation content is opt-in. The `agento11y`
binary launches agents and hosts hook-based integrations; pi and OpenCode export
in process. This path installs and configures the integration. It never changes
the user's application code.

Do the steps the machine's current state calls for, not all five. Step 1 says
where to start: a machine that needs one credential re-saved does not need a
reinstall, and a working setup the user asked a question about needs no changes
at all.

## Rules

1. **Never mint, generate, or guess a credential.** The token is a Grafana Cloud
   access-policy token that only the user can create. Ask for it, or tell the
   user where to click. A made-up token produces a 401.
2. **Never invent an endpoint URL.** The API URL and the OTLP endpoint come from
   the setup page of the user's own stack (Step 3). Do not assemble one from the
   stack name; regions differ.
3. **Never print or echo a token, and never ask for the connection block in
   chat.** The block the setup page hands out carries a live token, and this
   conversation can be exported. The user pastes it into login's own prompt. A
   token you have to pass yourself goes in over stdin with `--token-stdin`.
4. **Do not edit application code.** Adding SDK calls to the user's app is a
   different job ("Path B"). If the user wants their own app or agent
   instrumented, stop and hand off to the `agento11y-instrument` skill, which
   ships with the `gcx` CLI (`gcx agent skills install agento11y-instrument`).
5. **Do not enable full content capture or guards without asking.** The default,
   `metadata_only`, excludes prompts, responses, and tool I/O from generation
   exports. `full` sends that content to Grafana Cloud. Guards send the content
   they evaluate to `AGENTO11Y_ENDPOINT` regardless of capture mode. Local
   mode is separate: it always captures full content in the local store.

## Step 1: Read the current state

Run this first, every time, before changing anything:

```sh
agento11y doctor --json
```

If the command is not found, the binary is not installed: go to Step 2.

Branch on these fields. The JSON also reports auto-update state:

| Field | Meaning | Where to go |
| --- | --- | --- |
| `agento11y.version` | The installed build. `dev` is an unstamped build, including one installed with `go install`. Always present when the command runs. | command not found: Step 2 |
| `config.exists` | Whether the resolved config file exists. | `false`: Step 3 |
| `conversations.status` | The transcript pipeline: `ok`, `warning`, `error`. | `error`: Troubleshooting |
| `analytics.status` | The OTLP metrics and traces pipeline. | `error`: Troubleshooting |
| `agents[]` | One entry per coding agent, with `install_state` and `on_path`. | target agent `not_installed`: Step 4 |

Cursor is hook-based. `install_state: installed` means every agento11y Cursor
hook is present in `~/.cursor/hooks.json`; `not_installed` means one or more is
absent. Run `agento11y cursor install` when the user wants Cursor wired;
re-running it is safe.

`agento11y doctor` exits 1 when conversations, analytics, or config is in
`error`. The agent list never fails the command: an agent the user does not have
is reported, not treated as a problem.

Run `agento11y doctor` without `--json` when you want the human report, and
share `--json` output when the user asks for something to send to support. The
JSON never contains the token value. It can contain a short token-scheme prefix,
such as `glc_`.

## Step 2: Install the binary

Pick one method that matches the platform and how the user installs other tools.

**Quick install (Linux and macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/agento11y/main/plugins/agento11y/scripts/install.sh | sh
```

The script downloads the latest release for the OS and architecture and installs
to `~/.local/bin`. It verifies the SHA-256 when `sha256sum` or `shasum` is
available; otherwise it warns and continues. Re-run it to upgrade. `INSTALL_DIR`
changes the directory and `VERSION` pins a release.

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

Upgrade with `brew upgrade grafana/grafana/agento11y`.

**Go install (any platform with Go 1.25.7+):**

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

The command was renamed from `sigil`. The quick installer and Homebrew also
install a `sigil` alias. `go install` installs only the binary you name. The old
name will be removed in a future release, so do not write new instructions
against `sigil`.

## Step 3: Choose a destination

A first `agento11y login` asks where sessions go: **Local only**, or **Grafana
Cloud**. Windows has no local receiver, so its flow starts at the Cloud
questions.

**Local only** writes `AGENTO11Y_LOCAL=true` and `SIGIL_LOCAL=true` to
`$XDG_CONFIG_HOME/agento11y/config.env`, or `~/.config/agento11y/config.env` when
`XDG_CONFIG_HOME` is unset. **Grafana Cloud** continues to the credential flow,
which writes credentials there too.

### The paste flow

Tell the user to run `agento11y login` in their own terminal, then wait. You
cannot drive it yourself: it needs a terminal, and the block it asks for holds a
live token that must not pass through this conversation. The destination question
appears only on macOS and Linux, and only when no destination and no credentials
are saved. The Cloud questions after it run on every interactive `agento11y login`
without complete credential flags, including a rerun with saved credentials.

1. **Where should sessions go?**: a two-option list. **Local only** saves local
   mode and ends the flow. **Grafana Cloud** continues with the steps below.
2. **Your Grafana Cloud URL**: the Grafana they open in a browser, for example
   `https://mystack.grafana.net`. The field arrives pre-filled from the stack an
   earlier run saved or from a gcx configuration, and becomes a list when more
   than one is known. It only builds the links below; it is never the ingest
   endpoint.
3. Login prints `https://<stack>/a/grafana-agento11y-app/setup-coding-agent` and
   tries to open it in a browser. That page has three steps: create an API token,
   copy the connection settings, and paste them back in the terminal. The page
   creates the token when the user has permission and the stack supports it.
   Otherwise it links to Cloud Access Policies and accepts an existing token.
4. **Paste from Grafana**: the whole block goes into this one masked field, which
   fills the endpoint, instance ID, token, and OTLP settings. Login then asks
   only for what the block did not carry. Pasting is optional: Enter on the empty
   box types the values field by field instead.
5. **Preferences**: content capture mode, session tags, guards and their timeout,
   and automatic tags. Enter keeps the current behavior, which is the answer
   unless the user asks otherwise. Read the privacy warning in Rule 5 before
   enabling full capture or guards. Read **Automatic tags** below before enabling
   automatic tags; Yes opens a second checklist for the values to attach.

The copied block has concrete values in this shape:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_PROTOCOL=http
AGENTO11Y_AUTH_MODE=basic
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS='Authorization=Basic <base64-value>'
```

A stack with no OTLP gateway has neither `OTEL_*` line, so conversations work
but analytics does not. Login rejects placeholders in values it takes from the
block; a flag can replace a block slot. It ignores pasted protocol and auth-mode
values because the launcher hardcodes HTTP and Basic. Existing config keys stay.

For both pipelines, the token needs `sigil:write`, `metrics:write`, and
`traces:write`. One token covers them. A generated policy can carry extra scopes;
the integrations use only these three.

### The flags

`--endpoint`, `--tenant`, and a token together skip the destination and value
prompts, so this form works over SSH, in a devcontainer, and from a script. A
failed credential check can still prompt on an interactive terminal. Use this
form for values the user has already given you.

```sh
printf %s "$TOKEN" | agento11y login \
  --endpoint https://agento11y-prod-<region>.grafana.net \
  --tenant <instance-id> \
  --token-stdin \
  --otlp-endpoint https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Before writing the file, login sends one request to the endpoint with the
credentials and reports what came back. If that check fails, what happens next
depends on the terminal. Interactively, login asks whether to save anyway. In a
script the question is never asked, because a piped stdin is not a terminal:
login writes nothing and exits 1. Pass `--yes` to save the values regardless, or
`--no-verify` to skip the check. The preferences have no flags.

Login writes to the resolved config path described above. An install that still
has only the old `sigil/config.env` under the same config root keeps using that
file. You can write it by hand instead:

```dotenv
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

A hand-written file needs no OTLP auth header. When
`OTEL_EXPORTER_OTLP_HEADERS` has no `Authorization` entry, the launcher builds
Basic authorization from the tenant ID and the first available OTLP or ingest
token.

## Step 4: Wire the coding agent

Six agents launch through `agento11y <agent>`. A first run asks where sessions go
as Step 3 describes, and **Local only** there starts the receiver for that launch.
The command then installs the host integration and replaces itself with the agent.
It asks nothing when local mode is already on (`--local` or `AGENTO11Y_LOCAL=true`)
or when stdin is not a terminal. Existing integrations follow the **Auto-update**
rules in the Reference.

| Agent | Command | Mechanism |
| --- | --- | --- |
| Claude Code | `agento11y claude` | shared Go binary, plugin `agento11y-claude-code` |
| Codex | `agento11y codex` | shared Go binary, plugin `agento11y-codex` |
| Copilot CLI | `agento11y copilot` | hooks shared with Copilot Chat in VS Code |
| OpenCode | `agento11y opencode` | installs `@grafana/agento11y-opencode` |
| pi | `agento11y pi` | installs `@grafana/agento11y-pi` |
| Vibe | `agento11y vibe` | shared Go binary via `hooks.toml` |

After the first `agento11y codex` launch, open `/hooks` inside Codex and trust
the agento11y hooks. Codex does not export turns until the user completes this
manual step, and doctor cannot detect whether they did.

Vibe hooks are experimental. The launcher enables them in the child process, so
launch Vibe through `agento11y vibe` for capture.

**Cursor is the exception.** It is a GUI application with no launcher, so wire
it directly:

```sh
agento11y cursor install     # merges the hook into ~/.cursor/hooks.json
agento11y cursor uninstall   # removes it
```

On macOS and Linux, re-run `agento11y cursor install` after moving the binary;
Cursor's hook stores its absolute path.

For managed macOS deployment, after the administrator has placed the connection file for the target user, use `agento11y claude install --json` and `agento11y cursor install`; neither launches a host. `missing_host` for Claude means rerun after Claude Code is installed for that user.

Arguments after `--` go to the underlying CLI unchanged:

```sh
agento11y claude -- --resume
```

Suggest the user alias the prefix (for example `alias claude='agento11y claude'`)
only if they ask; silently shadowing their agent command is surprising.

## Step 5: Verify

1. Run the agent for one turn. Any prompt will do.
2. Open Grafana Cloud, then Agent Observability, then **Conversations**. It
   usually appears within a few seconds; if it does not, continue to step 3.
3. If nothing appears, run `agento11y doctor`. For Codex, also confirm that the
   user trusted the hooks from `/hooks`. Read launcher stderr for lines starting
   with `agento11y:`.

Confirm both pipelines. Conversations and analytics fail independently. If
conversations arrive while **Analytics** stays empty, check the OTLP pipeline.

## Troubleshooting

`agento11y doctor` is the first command for every problem. It is read-only. When
the required settings exist, it sends one conversations probe plus separate
metrics and traces probes, then reports their HTTP statuses. Some non-success
responses are warnings rather than errors, so read each probe message as well as
the section status.

### The two pipelines

They are independent and fail independently.

- **Conversations** (the chat transcripts) export over `AGENTO11Y_ENDPOINT`,
  `AGENTO11Y_AUTH_TENANT_ID`, and `AGENTO11Y_AUTH_TOKEN`. The token needs the
  `sigil:write` scope.
- **Analytics** (the metrics and traces) export over
  `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`, or the vendor-neutral
  `OTEL_EXPORTER_OTLP_ENDPOINT`. The token needs `metrics:write` and
  `traces:write`.

If conversations work while analytics stays empty, the OTLP endpoint may be
unset, the token may lack the metrics or traces scope, or the OTLP export may be
failing for another reason. Doctor names the missing-endpoint case and probes
both signals when an endpoint is set.

### Reading the report row by row

- **endpoint** and **tenant id**: the resolved value plus the variable it came
  from. `not set` means neither spelling was found. Doctor names the winning
  spelling because both `AGENTO11Y_*` and the older `SIGIL_*` are accepted.
- **auth token**: `set` plus a short prefix. The value is never printed.
- **probe**: the HTTP status and URL. Analytics has separate **metrics probe**
  and **traces probe** rows. `no response` includes DNS, connection, timeout, TLS,
  and blocked-network failures.
- **file**: the config path and whether it exists.
- **content capture**: the effective mode and where it came from. An invalid
  value falls back to `metadata_only` and the section message names the variable
  to fix.
- **guards**: `disabled`, or `enabled` with the timeout and fail-open/fail-closed
  mode. With local forwarding, **local guard checks** says whether guard content
  reaches Grafana Cloud.
- **Coding agents**: one row per agent. `not found on PATH` means doctor cannot
  find that CLI. `on PATH, plugin not installed` describes current state; the
  integration may never have been installed, may have been removed, or may be
  installed for another scope. `install state unknown` means the read-only probe
  could not determine the state. Cursor has no install probe. Its human row says
  `detected <version>` when Cursor is on PATH; only JSON reports it as unknown.

### Status codes

- **401 on conversations**: the token may be invalid, expired, or missing
  `sigil:write`; the tenant ID may also be wrong. Sigil reports all of these as
  rejected credentials.
- **401 on analytics**: the OTLP credentials were rejected.
- **403**: check the scope named in the probe message. Grafana's OTLP gateway can
  use this for missing `metrics:write` or `traces:write`; doctor also handles it
  defensively for conversations.
- **Any redirect on the conversations probe**: `AGENTO11Y_ENDPOINT` is not an
  API URL. A bare stack URL can redirect to login. Doctor rejects a Grafana app
  page before probing it. Re-run `agento11y login` and paste the block again.

### Debug logging

Once a valid hook command is dispatched, hook failures and panics are swallowed
so they do not crash the host agent. An invalid command line can still exit 2.
Turn on the log for hook detail:

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

## Reference

### Config keys

All hosts read the resolved config path. The default is
`~/.config/agento11y/config.env`; `$XDG_CONFIG_HOME` changes the config root.

| Key | Meaning |
| --- | --- |
| `AGENTO11Y_ENDPOINT` | Conversations API URL |
| `AGENTO11Y_AUTH_TENANT_ID` | Instance ID |
| `AGENTO11Y_AUTH_TOKEN` | Access-policy token |
| `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP endpoint for metrics and traces |
| `AGENTO11Y_STACK_URL` | The stack login asks about; builds links only, never ingest |
| `AGENTO11Y_CONTENT_CAPTURE_MODE` | Which content fields ship (see below) |
| `AGENTO11Y_TAGS` | `key=value,key=value` attached to every generation |
| `AGENTO11Y_AUTO_CODING_AGENT_TAGS` | Opt-in automatic user, repo, and branch tags |
| `AGENTO11Y_GUARDS_ENABLED` | Send supported preflight and tool calls for guard evaluation |
| `AGENTO11Y_GUARDS_TIMEOUT_MS` | Guard evaluation timeout in milliseconds |
| `AGENTO11Y_GUARDS_FAIL_OPEN` | Allow the operation when guard evaluation fails |
| `AGENTO11Y_LOCAL` | A true value routes `agento11y <agent>` launches, agento11y hooks, and history imports to the local daemon |
| `AGENTO11Y_LOCAL_FORWARD` | Forward local-mode captures to Grafana Cloud |
| `AGENTO11Y_AUTO_UPDATE` | A false value opts out of host-plugin refresh |
| `AGENTO11Y_DEBUG` | A true value writes the debug log |

All branded keys except `AGENTO11Y_STACK_URL` have an older `SIGIL_*` spelling.
**Local only** writes both `AGENTO11Y_LOCAL=true` and `SIGIL_LOCAL=true`, so later
launchers and `agento11y cursor install` ask nothing. An explicit `agento11y login`
still runs the Cloud questions, and does not ask where sessions go again. Doctor
reports provenance only for settings in its report.

### Tagging sessions

`--tag key=value` is repeatable and goes before any `--`. It attaches tags to
every generation the launched session produces, and merges onto (and overrides)
`AGENTO11Y_TAGS`:

```sh
agento11y claude --tag project=hackathon --tag team=ai -- --resume
```

Every CLI launcher accepts it. Cursor has no launcher; set `AGENTO11Y_TAGS` in
the config file for Cursor sessions.

### Automatic tags

`AGENTO11Y_AUTO_CODING_AGENT_TAGS=true` resolves the session's user, repository,
and branch and attaches them as **client** tags, which are the tags that also
become OTel metric labels. That is what lets the Usage and Cost view break down
by person, repository, or branch. The switch is off by default, and on its own
it enables all three names.

| Name | Tag key | Resolved from |
| --- | --- | --- |
| `user` | `user` | `AGENTO11Y_USER_ID`, then the signed-in Claude Code or Cursor identity when available, then the OS account name |
| `repo` | `repo` | Full namespace from the checkout's `origin` remote, or the checkout directory name |
| `branch` | `git.branch` | Checked-out branch, or a short commit SHA on detached HEAD |

Narrow the list with `AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES=user,repo`. A key
already in `AGENTO11Y_TAGS` wins, an unresolved value leaves its key off, and an
unsupported name is logged and skipped. In Prometheus these arrive as
`agento11y_tag_user`, `agento11y_tag_repo`, and `agento11y_tag_git_branch`.

Warn the user before enabling this. For Claude Code and Cursor, `user` can be a
work email address retained as a Prometheus label. Each value combination creates
a time series, and branch names are unbounded. Start with `user,repo`. Doctor
shows repo and branch but cannot read a host's signed-in identity; set
`AGENTO11Y_USER_ID` when the user value must be known before launch.

### Content capture

For coding agents, use only these modes:

| Mode | What ships in generation exports |
| --- | --- |
| `metadata_only` | Model, tokens, timing, IDs, tool names, and cost when available. Prompts, responses, and tool I/O are omitted. |
| `full` | Conversation content, including tool arguments and results. Host APIs can omit or truncate fields they do not expose in full. |

Set the mode with `AGENTO11Y_CONTENT_CAPTURE_MODE`. It defaults to
`metadata_only`. Unknown values fall back to `metadata_only` with a warning.
Every plugin redacts known secret formats from captured content, but capture mode
chooses which fields ship, not whether the shipped fields are clean. Treat
`full` as opt-in on shared machines.

### Local mode

`agento11y <agent> --local`, or **Local only** at the first-run question, routes
launcher runs and agento11y hooks to a JSONL store on the machine. It also
overrides the capture mode with full content. The launcher prints the viewer URL.
Manage the daemon with `agento11y local start|status|stop|restart`. Local mode
runs on macOS and Linux only. **Local only** at a launcher starts the receiver for
that launch; after `agento11y login` or `agento11y cursor install` the receiver
starts on the next launch or hook. `--no-local` runs one session against Cloud
while the saved answer stays set. Never describe local mode as a switch that keeps
all data on the machine: with `AGENTO11Y_LOCAL_FORWARD` and valid Cloud settings
it forwards to Grafana Cloud too. Doctor prints that correction only for a
configured `AGENTO11Y_LOCAL`.

### History import

Until an import runs, the viewer and Grafana Cloud hold only sessions captured after installation.
`agento11y history import` backfills sessions `claude-code`, `codex`, `cursor`, or `pi` already wrote to disk.

```sh
agento11y history import claude-code --dry-run   # plan only; no setup or export
agento11y history import claude-code --local     # import into the local store
agento11y history import claude-code --no-local  # import into Grafana Cloud
```

The import resolves the destination in this order: `--no-local`, `--local`, a true
`AGENTO11Y_LOCAL`/`SIGIL_LOCAL` value from the shell or `config.env`, then saved Cloud credentials.
With none set, an interactive run asks where sessions go. `--no-local` or any valid LOCAL value
skips that question; missing Cloud credentials still starts Cloud setup. A noninteractive run never
asks. It imports only with both `--all` and `--yes`; otherwise it prints a dry-run plan and exits 0.

Without `--since`, an import selects sessions active during the last 90 days. Each imported turn goes into a per-agent ledger that omits the destination.
To send the same turns to another destination, add `--force` to the second import. If saved local mode would keep that import local, add `--no-local` too.
A cancelled or failed run resumes from turns not marked exported.

### Auto-update

Claude, Codex, and OpenCode refresh after a binary version change and, after
the first post-install refresh, at most once a day. A relaunch inside that period
does not pick up a plugin fix. `AGENTO11Y_AUTO_UPDATE=0|false|no|off` opts out.
Copilot and Vibe reconcile hooks on each launch. Pi leaves upgrades to pi's
installer.

## Further reading

- Product docs: <https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/>
- Source: <https://github.com/grafana/agento11y>
