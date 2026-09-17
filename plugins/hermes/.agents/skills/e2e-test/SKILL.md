---
name: e2e-test
description: Optional credential-free Hermes integration checks using an explicit loopback model provider and local telemetry receivers. Inspect the imported scripts before running them; their defaults are not safe evidence of a local-only run.
---

# Optional Hermes end-to-end checks

Unit tests stub the SDK and invent hook payloads. These optional checks inspect
real Hermes hooks and exported OTel data. They are not part of normal CI.

Run from `plugins/hermes`. Read each script before execution. Do not run Cloud
verification, install packages, start servers, or invoke Hermes without approval.
The recipe below is documentation, not a claim that the import passed e2e.

## Safety boundary

A local telemetry sink does not make the model provider local. A mock provider
does not make telemetry local either. Explicitly configure all three destinations:

| Destination | Credential-free choice |
| --- | --- |
| Model provider | LOOPBACK: `openai-api`, `mock-model`, `http://127.0.0.1:8799/v1`, dummy key |
| Generation export | Disabled: `AGENTO11Y_PROTOCOL=none`, `AGENTO11Y_AUTH_MODE=none`, no token or tenant |
| Traces and metrics | LOOPBACK: `http://127.0.0.1:8801`, no auth headers |

Use a new temporary HOME and HERMES_HOME, not the user's real config. Clear the
inherited environment so provider keys, telemetry aliases, per-signal endpoints,
proxy settings, and `AGENTO11Y_ENV_FILE` cannot redirect the test.
Hermes's `.env` overrides process exports: never reuse an old test home.
Use synthetic content only. Hook-probe logs contain payloads before redaction.

## Imported wrapper limitations

The scripts were inspected during documentation migration but were not run:

- `setup.sh` installs packages and defaults its config to Anthropic. Its warning
  requesting an Anthropic key is not applicable to the loopback recipe.
- `run-hermes.sh sink` redirects OTel only. Its provider defaults to Anthropic,
  so `sink` alone is neither credential-free nor a local-only test.
- `run-hermes.sh full` leaves capture mode unset. After migration, it tests
  `metadata_only`, despite its name. The wrapper does not pass through capture
  mode, redaction, or automatic-tag environment switches.
- `run-mock.sh` chooses a loopback provider but reads generation and OTLP
  destinations from the environment or `AGENTO11Y_ENV_FILE`. It forces basic
  auth and uses broad `pkill` matching. Do not use it for this recipe.
- `verify-backend.sh` uses Cloud queries. It is outside this credential-free flow.
- `otlp-sink.py` decodes spans and metrics, but logs only attribute names.
  It does not validate generation ingestion or secret-redaction values.

## Isolated setup

Prerequisites: `uv`, Python 3.11+, and permission to download dependencies.
The setup step can access package registries; the model and telemetry steps
below use loopback. Run the steps in the same shell.

```sh
S="$PWD/.agents/skills/e2e-test/scripts"
E2E_DIR=$(mktemp -d)
mkdir -p "$E2E_DIR/user"
env -i PATH="$PATH" HOME="$E2E_DIR/user" E2E_DIR="$E2E_DIR" PY_VERSION=3.13 "$S/setup.sh" 0.19.0
```

`setup.sh` resolves the package root relative to its own location, so the nested
monorepo location still installs `plugins/hermes`. Confirm it reports the
`agento11y` entry point and registered hooks. Keep the fresh home free of `.env`
files. No provider key is needed for the next step.

## Start loopback receivers and run Hermes directly

Bypass the old wrappers so privacy switches reach the Hermes process.
The mock and OTLP server both bind to `127.0.0.1`. Choose unused ports; if either
server exits on startup, stop rather than connecting to an unknown listener.

```sh
env -i PATH="$PATH" HOME="$E2E_DIR/user" E2E_DIR="$E2E_DIR" MOCK_SCRIPT=tool,ok "$E2E_DIR/.venv/bin/python" "$S/mock-provider.py" 8799 >"$E2E_DIR/mock.out" 2>&1 &
mock_pid=$!
env -i PATH="$PATH" HOME="$E2E_DIR/user" E2E_DIR="$E2E_DIR" "$E2E_DIR/.venv/bin/python" "$S/otlp-sink.py" 8801 >"$E2E_DIR/sink.out" 2>&1 &
sink_pid=$!
trap 'kill "$mock_pid" "$sink_pid" 2>/dev/null || true' EXIT
sleep 1
kill -0 "$mock_pid" "$sink_pid" || exit 1

env -i PATH="$PATH" HOME="$E2E_DIR/user" TERM=dumb E2E_DIR="$E2E_DIR" HERMES_HOME="$E2E_DIR/home" OPENAI_API_KEY=mock-key OPENAI_BASE_URL=http://127.0.0.1:8799/v1 AGENTO11Y_PROTOCOL=none AGENTO11Y_AUTH_MODE=none AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only AGENTO11Y_AUTO_CODING_AGENT_TAGS=false OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:8801 OTEL_EXPORTER_OTLP_INSECURE=true "$E2E_DIR/.venv/bin/hermes" -m mock-model --provider openai-api -z 'List the available skills, then reply OK.'
```

This checks only the OTel channel. The disabled generation channel is deliberate,
not evidence that generation export works. To test neither channel, omit the
OTLP variables and set `AGENTO11Y_HERMES_OTEL_AUTO=false`.

To test generation export, first provide a loopback receiver that implements
`/api/v1/generations:export` and validates the SDK request. Explicitly set its
loopback endpoint and HTTP protocol. The plugin's channel activation also needs
a supported non-`none` auth mode; use dummy local credentials only. Keep OTel
pointed to the local sink or disable it. The imported sink does not implement
this ingest protocol; do not treat its generic HTTP 200 response as validation.

## What to inspect

Read `mock.log`, `hooks.jsonl`, and `otlp-sink.log` under the temporary directory.
The mock should show a tool request followed by completion. The sink should show
generation/tool spans and metrics. Missing spans can indicate a flush or hook
problem; no Cloud sampling is involved here.

Check these cases with explicit settings on the direct Hermes invocation:

- Capture mode unset, `default`, and invalid: all must resolve to `metadata_only`.
- Explicit `full`: exercise content and shared redaction with synthetic secrets.
  Use a receiver that inspects values; this sink's attribute-name log is insufficient.
- `AGENTO11Y_REDACT_INPUT_MESSAGES=false`: only prompt redaction turns off.
- Automatic tags off: no automatic user/repo/branch and no `cwd`.
  Enable `user,repo`, then `branch`, and check metric labels and explicit-tag precedence.
- Sampling rate zero: no generation or tool telemetry.
- Retry scripts `429,429,ok`, `empty`, `401`, and `scratchpad`: restart the mock
  with the chosen `MOCK_SCRIPT` and inspect actual hooks, not assumed attempt counts.
- Tool spans parent to the requesting generation, even though that parent ended.
- Request clipping and cache reuse: vary `HERMES_PLUGIN_PAYLOAD_MAX_CHARS`.
  Reused sampling parameters must come from the same model.

One-shot mode disables logging and bypasses atexit, so missing plugin logs are
expected. Some early-return paths can omit finalization and lose open records.
Use interactive Hermes when diagnosing logging or exit hooks.

Repeat on the supported floor and proposed Hermes upgrades. The PyPI release's
hook call sites are the contract; upstream HEAD can contain unreleased kwargs.

## Cleanup

Stop only the PIDs started above. Review the synthetic logs, then remove only the
temporary directory printed by `printf '%s\n' "$E2E_DIR"`. Do not use broad `pkill`
or delete a reused path. No Cloud data should have been written.
