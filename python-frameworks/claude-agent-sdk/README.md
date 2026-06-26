# Sigil Python Framework Module: Claude Agent SDK

`sigil-sdk-claude-agent-sdk` records Claude Agent SDK sessions as Sigil generations and maps Claude tool hooks to Sigil tool spans.

## Installation

```bash
pip install sigil-sdk sigil-sdk-claude-agent-sdk
pip install claude-agent-sdk
```

The Claude Agent SDK runs the Claude Code CLI. Authenticate and configure Claude Code the same way you would for a normal Claude Agent SDK application.

## Quickstart

```python
import asyncio

from claude_agent_sdk import ClaudeAgentOptions
from sigil_sdk import Client
from sigil_sdk_claude_agent import sigil_query


async def main():
    client = Client()
    try:
        async for message in sigil_query(
            prompt="List the files in this directory.",
            options=ClaudeAgentOptions(
                permission_mode="default",
                model="claude-sonnet-4-5",
            ),
            client=client,
            conversation_id="demo-claude-agent-sdk",
            agent_name="claude-agent-demo",
        ):
            print(message)
    finally:
        client.shutdown()


asyncio.run(main())
```

## Existing ClaudeSDKClient Usage

For bidirectional sessions, attach hooks to your `ClaudeAgentOptions` and pass every stream message to the handler:

```python
from claude_agent_sdk import ClaudeAgentOptions
from sigil_sdk import Client
from sigil_sdk_claude_agent import SigilClaudeSDKClient

sigil = Client()
options = ClaudeAgentOptions(permission_mode="default")

async with SigilClaudeSDKClient(
    client=sigil,
    options=options,
    conversation_id="customer-42",
    agent_name="support-agent",
) as claude:
    await claude.query("Help me inspect this project.")
    async for message in claude.receive_response():
        print(message)

    await claude.set_permission_mode("acceptEdits")

sigil.shutdown()
```

`SigilClaudeSDKClient` forwards `query()`, `receive_response()`, `receive_messages()`, `set_permission_mode()`,
`rewind_files()`, `interrupt()`, and `disconnect()` to the wrapped Claude client. Use `sigil_query()` for simple
single-query scripts and `SigilClaudeSDKClient` when you need Claude SDK session control such as permission mode changes,
resume/checkpoint flows, or multiple queries in one client session.

## Guards

Sigil guards run through Claude Agent SDK hooks:

- `UserPromptSubmit` evaluates the submitted prompt before Claude proceeds. A Sigil deny returns `continue_=False`, stopping the run.
- `PreToolUse` evaluates tool requests before execution. A Sigil deny maps to Claude's `permissionDecision="deny"`.

Enable guards on the Sigil client:

```python
from sigil_sdk import Client, ClientConfig, HooksConfig

client = Client(ClientConfig(hooks=HooksConfig(enabled=True)))
```

`HooksConfig` keeps the core SDK defaults: preflight phase, 15 second timeout, and fail-open transport behavior unless configured otherwise.

## Conversation Mapping

Conversation ID precedence:

1. Explicit `conversation_id` passed to the Sigil handler or `sigil_query`
2. `ClaudeAgentOptions.session_id`
3. `ClaudeAgentOptions.resume`
4. unique fallback `sigil:framework:claude-agent-sdk:<run_id>`

`SigilClaudeSDKClient` uses the same explicit `conversation_id`, `session_id`, and `resume` precedence. If none are
set, it creates one client-level fallback conversation ID and reuses it for every query in that client session so
multi-query sessions stay grouped.

When Claude returns a session ID in the stream, the handler also records it in generation metadata as `sigil.framework.session_id`.

## Metadata

Required framework tags:

- `sigil.framework.name=claude-agent-sdk`
- `sigil.framework.source=hooks`
- `sigil.framework.language=python`

Metadata includes:

- `sigil.framework.run_id`
- `sigil.framework.run_type=agent`
- `sigil.framework.session_id` when Claude returns one
- `sigil.claude_agent.permission_mode` when configured
- `sigil.claude_agent.cwd` when configured
- `sigil.claude_agent.total_cost_usd` when Claude returns cost data

## Claude Native OpenTelemetry

This package records Sigil generations and tool spans through the Sigil Python SDK. The Claude Agent SDK can also make the Claude Code CLI export its native OpenTelemetry spans, metrics, and logs directly to your OTLP collector. Configure those variables in the parent process before calling `sigil_query`; the Python Claude Agent SDK merges `options.env` on top of the inherited environment.

For Grafana Cloud, the important Claude variables are:

```dotenv
CLAUDE_CODE_ENABLE_TELEMETRY=1
CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1
OTEL_TRACES_EXPORTER=otlp
OTEL_METRICS_EXPORTER=otlp
OTEL_LOGS_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64 of OTLP_INSTANCE_ID:SIGIL_AUTH_TOKEN>
```

Do not use the `console` OTel exporter with the Claude Agent SDK; stdout is part of its message channel.

## Local Validation

From the repository root:

```bash
mise run test:py:sdk-claude-agent-sdk
```

To run only this package manually:

```bash
uv run --python "$PYTHON_BIN" \
  --with './python[dev]' \
  --with-editable './python-frameworks/claude-agent-sdk[dev]' \
  pytest python-frameworks/claude-agent-sdk/tests
```

## Troubleshooting

- If generations are fragmented, pass a stable `conversation_id`.
- If no Claude native traces appear, verify `CLAUDE_CODE_ENABLE_TELEMETRY=1`, an OTLP exporter is selected, and the endpoint/header values are visible to the Python process.
- If guards do not run, make sure the Sigil client was created with `ClientConfig(hooks=HooksConfig(enabled=True))`.
- Always call `client.shutdown()` during teardown.
