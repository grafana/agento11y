# Agent Observability Python Framework Module: Strands Agents

`agento11y-strands` provides a Strands `HookProvider` bridge that maps agent, model, and tool lifecycle events into agento11y generation and tool recording.

## Installation

```bash
pip install agento11y agento11y-strands
pip install strands-agents
```

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

## Metadata and Lineage

Required framework tags:

- `agento11y.framework.name=strands`
- `agento11y.framework.source=hooks`
- `agento11y.framework.language=python`

Metadata includes:

- required: `agento11y.framework.run_type`
- optional: `agento11y.framework.run_id`, `agento11y.framework.thread_id`, `agento11y.framework.parent_run_id`, `agento11y.framework.component_name`, `agento11y.framework.event_id`

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
invoked as ordinary application code or tools need to be instrumented separately.

## Troubleshooting

- If conversations are fragmented, pass stable `conversation_id` or `session_id` in `invocation_state`.
- If provider is inferred as `custom`, set `provider="openai"` / `provider="anthropic"` / `provider="gemini"` on hook creation.
- If cost is zero, verify the exact model ID, underlying vendor provider, and input/output token usage in the generation.
- Always call `client.shutdown()` during teardown.
