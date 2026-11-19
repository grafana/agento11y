# Agent Observability Python Framework Module: Strands Agents

`agento11y-strands` provides a Strands `HookProvider` bridge that maps agent, model, and tool lifecycle events into agento11y generation and tool recording.

## Installation

```bash
pip install agento11y agento11y-strands
pip install strands-agents
```

Requires `strands-agents>=1.36.0`. That is the first release to attach token usage to the assistant message, which is the only place `AfterModelCallEvent` exposes it. On older versions generations are still recorded, but always without token counts or cost.

## Quickstart

```python
from agento11y import Client
from agento11y_strands import with_agento11y_strands_hooks
from strands import Agent

# Client() exports generations. Configure an OTel MeterProvider as shown below
# when you also want Usage/Cost and latency metrics.
client = Client()
agent_config = with_agento11y_strands_hooks(
    {"name": "support-agent"},
    client=client,
    provider_resolver="auto",
)

agent = Agent(**agent_config)
agent(
    "Explain what LLM observability is in one sentence.",
    invocation_state={"conversation_id": "demo-strands"},
)

client.shutdown()
```

## Generation Export vs. OTLP Metrics

Generation export and OpenTelemetry metrics are separate paths. Generation export
makes conversations and generations visible; OTLP metrics power token usage, cost,
latency, and related dashboards. Register a `MeterProvider` and pass its meter to
the client:

```python
from agento11y import ClientConfig
from opentelemetry import metrics
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource

meter_provider = MeterProvider(
    resource=Resource.create({"service.name": "my-strands-agent"}),
    metric_readers=[
        PeriodicExportingMetricReader(
            OTLPMetricExporter(
                endpoint="https://otlp-gateway-prod-<region>.grafana.net/otlp/v1/metrics",
                # Copy the complete Authorization header from the Cloud setup page.
                headers={"Authorization": "Basic <base64(instance-id:otlp-token)>"},
            )
        )
    ],
)
metrics.set_meter_provider(meter_provider)
client = Client(ClientConfig(meter=meter_provider.get_meter("my-strands-agent")))

try:
    # Create and run the instrumented Strands agent here.
    pass
finally:
    client.shutdown()
    meter_provider.shutdown()
```

`Client()` now warns when no meter provider is registered and no meter is passed.
This is not fatal because generation export can still be useful, but metrics will
otherwise be lost to OpenTelemetry's no-op provider.

## Existing Agents

```python
from agento11y import Client
from agento11y_strands import with_agento11y_strands_hooks

client = Client()
with_agento11y_strands_hooks(agent, client=client, provider_resolver="auto")
```

## Conversation Mapping

Conversation ID precedence:

1. `conversation_id` / `session_id` / `group_id` from Strands `invocation_state`
2. `thread_id` from Strands `invocation_state`
3. deterministic fallback `agento11y:framework:strands:<run_id>`

Pass a stable value per user conversation:

```python
agent("Remember my timezone is UTC+1.", invocation_state={"conversation_id": "customer-42"})
agent("What timezone did I give you?", invocation_state={"conversation_id": "customer-42"})
```

A sub-agent called from a tool starts its own invocation, usually with an empty `invocation_state`.
When it uses the same hook provider and has no conversation key, it joins the conversation of the
active invocation. If several invocations with different keys are active on the hook provider, the
sub-agent gets its own conversation instead. Pass the outer `conversation_id` to the sub-agent to
keep it in the outer conversation.

## Metadata and Lineage

Required framework tags:

- `agento11y.framework.name=strands`
- `agento11y.framework.source=hooks`
- `agento11y.framework.language=python`

Metadata includes:

- required: `agento11y.framework.run_type`
- optional: `agento11y.framework.run_id`, `agento11y.framework.thread_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.component_name`, `agento11y.framework.event_id`

## Streaming Mode

Generations are recorded as `STREAM` or `SYNC` from the model's own configuration. Strands spells the flag three ways, and all of them are read:

- `params={"stream": False}` for OpenAI, LiteLLM, Anthropic and Writer models
- `stream=False` for Mistral and SageMaker models
- `streaming=False` for Bedrock models

Strands defaults every provider to streaming, so a model that sets none of these is recorded as `STREAM`.

## Provider Resolver

Resolver order: explicit provider option -> Strands model config metadata -> model prefix inference -> `custom`.

`BedrockModel` exposes the model ID but usually does not expose a provider field. For
standard Bedrock model IDs and inference-profile ARNs, the adapter infers the
underlying vendor (`anthropic`, `meta`, `mistral`, etc.) from the ID. This preserves
model-catalog and cost lookup when the backend has pricing for that model. The exact
model ID and token usage must still be present in the generation.

For custom aliases or IDs that do not contain a recognizable vendor, pass an explicit
provider when creating the hook provider, for example
`provider="anthropic"`. Use the underlying model vendor rather than `provider="bedrock"`
when you want vendor model-catalog matching.

## Multi-agent Runs

The adapter registers Strands multi-agent and node lifecycle hooks. Model calls made
inside an active node are linked beneath that node, while all generations from the
same invocation can be grouped with a shared `conversation_id`:

```python
state = {"conversation_id": "customer-42"}
root_agent("Delegate this request", invocation_state=state)
```

This requires Strands' multi-agent APIs to emit their standard node hooks. Agents
invoked as ordinary application code need to be instrumented separately.

## Agents Called from Tools

When a tool invokes another agent, Strands starts that agent as a new invocation, usually
with an empty `invocation_state`. If both agents use the same hook provider and
`capture_workflow_steps` is on (the default), the adapter records the invoked agent below
the agent that called the tool:

- Its generations get the caller's `conversation_id`, also when its own `invocation_state`
  names another conversation. Its tool executions keep the conversation of its own
  `invocation_state`. When that state names no conversation, each tool execution gets a
  fallback conversation. Pass the caller's `conversation_id` in the invoked agent's
  `invocation_state` to keep the tool executions in the caller's conversation too.
- Its first generation lists the caller's generation that called the tool in
  `parent_generation_ids`. Agents invoked by other tool calls of the same turn do not list
  each other, and the caller's next generation does not list the invoked agent's generations.
- It is not recorded as a workflow step of the caller. When the caller is a multi-agent node,
  its generations are linked to that node's workflow step.

A multi-agent run that a tool starts gets the caller's `conversation_id`. It starts its own
lineage, so its first node generation does not list the caller's generation that called the
tool.

With `capture_workflow_steps` off, the invoked agent's span is a child of the caller's span,
and its generations keep their own conversation.

Create one hook provider and pass it to every agent. `with_agento11y_strands_hooks` creates a
new provider on each call, so agents instrumented with separate calls are not linked:

```python
hooks = create_agento11y_strands_hook_provider(client=client)
research_agent = Agent(name="research", hooks=[hooks])


@tool
def research(question: str) -> str:
    """Research a question."""
    return str(research_agent(question))


root_agent = Agent(name="root", tools=[research], hooks=[hooks])
root_agent("Delegate this request", invocation_state={"conversation_id": "customer-42"})
```

The adapter finds the caller through the context that Strands gives the tool. Sync and async
tools, `Agent.__call__`, `invoke_async` and `stream_async` keep that context. These cases are
not linked:

- An agent with its own hook provider is a top-level run with its own conversation. Pass the
  caller's `conversation_id` in its `invocation_state` to keep it in the same conversation.
- A tool that runs the agent in its own thread pool without copying the context.

Background tools are not tested. Strands can start a background tool outside the context of
the tool call, so an agent that a background tool invokes can be recorded as a top-level run.

With `SequentialToolExecutor`, the tool runs in the task that iterates the caller's events. An
agent that application code starts in that task while the tool runs is recorded below the
caller too.

## Troubleshooting

- If conversations are fragmented, pass stable `conversation_id` or `session_id` in `invocation_state`.
- If generations carry no token usage or cost, check the installed `strands-agents` version. Usage is only reachable from the model hook on 1.36.0 and later.
- If provider is inferred as `custom`, set `provider="openai"` / `provider="anthropic"` / `provider="gemini"` on hook creation.
- If cost is zero, verify the exact model ID, underlying vendor provider, and input/output token usage in the generation.
- Always call `client.shutdown()` during teardown.
