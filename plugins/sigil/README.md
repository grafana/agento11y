# agento11y

The launcher binary behind the [Claude Code](../claude-code), [Codex](../codex), [Copilot](../copilot), [Cursor](../cursor), [OpenCode](../opencode), and [pi](../pi) plugins for [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

The command was renamed from `sigil`. Every install method also installs a `sigil` alias, which will be removed in a future release.

## Install

**Quick install (Linux/macOS):**

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | sh
```

The script downloads the latest [release](https://github.com/grafana/sigil-sdk/releases) for your OS and architecture, verifies its SHA-256 checksum, and installs the binary to `~/.local/bin`. Re-run it to upgrade. Set `INSTALL_DIR` to change the directory and `VERSION` to pin a release:

```sh
curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | INSTALL_DIR=/usr/local/bin sh
```

**Homebrew (macOS):**

```sh
brew install grafana/grafana/agento11y
```

Upgrade later with `brew upgrade grafana/grafana/agento11y`.

**Prebuilt binary (Windows):** download the `windows_amd64` or `windows_arm64` zip from the [releases page](https://github.com/grafana/sigil-sdk/releases), extract `agento11y.exe`, and put it on your `PATH`.

**Go install (any platform with Go 1.25+):**

```sh
go install github.com/grafana/sigil-sdk/plugins/sigil/cmd/agento11y@latest
```

This installs the binary to `go env GOPATH`/bin (or `GOBIN` if set); make sure that directory is on your `PATH`. Re-run the same command to upgrade.

Verify the install with `agento11y --version`.

## Configure

All hosts read the same config file at `~/.config/sigil/config.env`. The first run of `agento11y claude`, `agento11y opencode`, or `agento11y pi` prompts for your endpoint, tenant ID, token, and OTLP endpoint and writes them there; run `agento11y login` to re-enter them later. After the connection details, `agento11y login` shows an optional preferences step for content capture mode, session tags, and guards — leave it at the defaults to keep the current behaviour. Cursor has no launcher, so wire it once with `agento11y cursor install` (which also prompts on first run) and remove it with `agento11y cursor uninstall`.

To preconfigure without the prompt, create the file:

```dotenv
AGENTO11Y_ENDPOINT=https://sigil-prod-<region>.grafana.net
AGENTO11Y_AUTH_TENANT_ID=<instance-id>
AGENTO11Y_AUTH_TOKEN=glc_...
AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
```

Find these values in Grafana Cloud at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`.

Then follow your agent's quickstart:

- [Claude Code](../claude-code/README.md)
- [Codex](../codex/README.md)
- [Copilot](../copilot/README.md)
- [Cursor](../cursor/README.md)
- [OpenCode](../opencode/README.md)
- [pi](../pi/README.md)

## Tagging sessions

Add `--tag key=value` (repeatable) before any `--` to attach tags to every generation the launched session produces. This is shorthand for setting `AGENTO11Y_TAGS`; flag tags merge onto (and override) any `AGENTO11Y_TAGS` already in the environment.

```sh
agento11y claude --tag project=hackathon --tag team=ai
# forward args to the underlying CLI after `--`
agento11y claude --tag project=hackathon -- --resume
```

The same flag works for every launcher (`claude`, `codex`, `copilot`, `opencode`, `pi`) and combines with `--local`.

## Content capture

The shared `agento11y` binary defaults to `metadata_only`: only model, tokens, tool names, timing, and cost ship to Grafana AI Observability. Prompts, responses, and tool I/O stay on the local machine. To opt into sending content, set `AGENTO11Y_CONTENT_CAPTURE_MODE` in `~/.config/sigil/config.env`. The shared binary parser accepts every mode the SDKs support:

```dotenv
# valid values: full | no_tool_content | metadata_only | full_with_metadata_spans
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Unknown values fall back to `metadata_only` with a warning. `default` is accepted as an alias for `metadata_only` so the shared binary matches the Go envconfig resolver rather than the JS SDK's client-level default of `no_tool_content`. The Pi (`@grafana/sigil-pi`) and OpenCode (`@grafana/sigil-opencode`) plugins ship their own parsers but accept the same set of values.

A plugin can only export fields the host agent passes through to it, so individual plugins may capture less than the SDK matrix shows. See [Content Capture Modes](../../docs/concepts/content-capture-modes.md) for the SDK-level behavior matrix and plugin defaults.

## Auto-update

`agento11y claude`, `agento11y codex`, `agento11y copilot`, and `agento11y opencode` refresh the installed host plugin automatically. Set `AGENTO11Y_AUTO_UPDATE=false` to opt out.

## Troubleshooting

Run `agento11y doctor` first. It's a read-only diagnostic that reports both export pipelines, config, and installed host-agent plugins in one place:

```sh
agento11y doctor
```

The two pipelines are independent and fail independently:

- **Conversations** (the chat transcripts) export over `AGENTO11Y_ENDPOINT` + `AGENTO11Y_AUTH_TENANT_ID` + `AGENTO11Y_AUTH_TOKEN`. The token needs the `sigil:write` scope.
- **Analytics** (the AI Observability metrics and traces) export over `AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_ENDPOINT`). The token needs `metrics:write` and `traces:write`.

The common failure is conversations showing up while the analytics page stays empty: the OTLP endpoint is unset, or the token lacks the metrics/traces scopes. `agento11y doctor` flags that case explicitly and exits non-zero when a pipeline is broken.

Add `--probe` to send a lightweight request to each endpoint and report the HTTP status (a 401/403 on the OTLP path means the token is missing `metrics:write`/`traces:write`):

```sh
agento11y doctor --probe
```

For support, capture the machine-readable report — it never includes the auth token value:

```sh
agento11y doctor --json
```

If you need lower-level detail, hooks always exit 0, so problems only show up in the debug log. Set `AGENTO11Y_DEBUG=true` in `~/.config/sigil/config.env` and tail `~/.local/state/sigil/logs/sigil.log`.
