# OpenTelemetry Setup

Agent Observability accepts generations from **OpenTelemetry GenAI spans or the Grafana SDKs**. Compatible spans can carry prompts, responses, tool calls, and token usage into conversations without a Grafana SDK or a custom generation exporter. Metrics alone do not carry conversation content.

## Choose an instrumentation path

- **Existing OpenTelemetry instrumentation:** keep your framework's instrumentation and export compatible GenAI spans to your Grafana Cloud OTLP endpoint. Follow [Instrument agents with OpenTelemetry](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/opentelemetry/) for setup and a runnable sample without a model provider.
- **Grafana SDK:** use a provider wrapper, framework adapter, or manual recorder to send normalized generations. The SDKs also provide APIs for features such as hooks and experiments. The provider examples below describe this path.

A framework missing from the Grafana SDK integration list can still work through compatible OpenTelemetry spans. An exporter labeled "OpenTelemetry" does not by itself guarantee that it sends the required GenAI attributes or message format. Check the exported spans before assuming compatibility.

### Send conversations through OpenTelemetry

1. Initialize your instrumentation's tracer provider and OTLP exporter. Use your stack's OTLP connection details and a Cloud Access Policy Token with `traces:write`. No `AGENTO11Y_ENDPOINT` is needed for this path.
2. Set `agento11y.record=true` as a resource attribute or on each GenAI span. Append it to existing `OTEL_RESOURCE_ATTRIBUTES` rather than replacing other attributes. A span-level `agento11y.record=false` excludes that span.
3. Include `gen_ai.operation.name` (`chat`, `generate_content`, `text_completion`, or `invoke_agent`), `gen_ai.provider.name`, and `gen_ai.request.model`. Agent spans also need provider/model information to appear as generations. Tool-execution spans do not create standalone generations.
4. Enable content capture in the instrumentation. Write `gen_ai.input.messages` and `gen_ai.output.messages` as span attributes in the OTel GenAI message format. Content sent only as OTLP logs does not populate conversations.
5. Reuse `gen_ai.conversation.id` across calls in one conversation. Without it, each trace forms a separate conversation. Set `gen_ai.agent.name` on inference spans; otherwise the resource's `service.name` identifies the agent.
6. Record token usage when available, using `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens`. Flush the exporter at shutdown.

Preserve these attributes through Alloy or your Collector. Spans dropped by application or collector sampling never reach Agent Observability. Remove secrets and sensitive content before export; Grafana SDK capture settings do not configure third-party instrumentation.

Do not opt in calls that the Grafana SDK already exports as generations. Recording the same call through both paths can create duplicates.

## Configure the Grafana SDK's telemetry

By default, the Grafana SDK sends generations directly and emits companion OTel spans and metrics. These are separate exports; configuring one does not configure the other.

| Pipeline | Carries | Configured by | Shows up as |
| --- | --- | --- | --- |
| **Generation and workflow-step export** | Prompts, responses, token usage, tool calls; workflow steps use a separate SDK ingest contract | `AGENTO11Y_ENDPOINT` and `AGENTO11Y_AUTH_*`; use HTTP and Basic auth for Grafana Cloud | Conversations and generations |
| **OTel spans and metrics** | `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, `gen_ai.client.time_to_first_token`, `gen_ai.client.tool_calls_per_operation`, and generation spans | A `TracerProvider` and `MeterProvider` **you** register | Client-metrics panels, including latency and tool activity, and trace drilldown |

**The SDK does not create OTel providers.** Without configured providers, companion spans and client metrics can be discarded by the default no-op implementations while direct generation export continues. Metrics-backed latency and tool panels can be missing even when conversations and generation-derived costs are available.

Agent Observability calculates generation cost at ingest from reported token usage and the applicable model price. It does not require client-emitted `gen_ai.client.token.usage` metrics for that calculation. Missing usage or an unknown model price can leave cost unavailable. Refer to [Configure your model prices](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/observe-and-troubleshoot/model-prices/).

## Rules

1. Register the providers **before** constructing the agento11y client. The client resolves its tracer and meter once, at construction.
2. Shut the providers down **after** `agento11y.shutdown()`, so the final flush has somewhere to go.
3. Set `agent_name` (and ideally `agent_version`) on the client or handler. They become metric labels, and analytics views group by them.

You can hand providers to the client explicitly via config (`tracer` / `meter`) instead of registering them globally. Explicit providers win over the globals.

## Delivery options

The OTel SDK exporters read `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS` from the environment, so the snippets below need no endpoint arguments.

**Direct to Grafana Cloud.** Point OTLP at the Grafana Cloud OTLP gateway. Authentication is Basic auth using your OTLP instance ID and a `glc_…` token. Use the OTLP instance ID from your stack's connection details, not an assumed copy of the Agent Observability instance ID. The same Cloud Access Policy Token can serve both exports when it has the required scopes: `sigil:write` for direct generation export, `traces:write` for traces, and `metrics:write` for metrics. Check exporter errors when delivery fails.

**Via Alloy or an OTel Collector.** Set `OTEL_EXPORTER_OTLP_ENDPOINT` to the collector and let it forward to Grafana Cloud. Preferred when you want centralized token management, retries, relabeling, or metadata enrichment.

## Provider setup

### Python

Requires `opentelemetry-sdk` and `opentelemetry-exporter-otlp-proto-http`.

```python
from opentelemetry import trace, metrics
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter

resource = Resource.create({"service.name": "my-app"})

tracer_provider = TracerProvider(resource=resource)
tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(tracer_provider)

meter_provider = MeterProvider(
    resource=resource,
    metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
)
metrics.set_meter_provider(meter_provider)
```

### JS/TS

This example uses OpenTelemetry JS SDK 2.x. Install the imported packages with compatible versions.

```typescript
import { metrics } from '@opentelemetry/api';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { MeterProvider, PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http';

const resource = resourceFromAttributes({ 'service.name': 'my-app' });
const tracerProvider = new NodeTracerProvider({
  resource,
  spanProcessors: [new BatchSpanProcessor(new OTLPTraceExporter())],
});
tracerProvider.register();

const meterProvider = new MeterProvider({
  resource,
  readers: [new PeriodicExportingMetricReader({ exporter: new OTLPMetricExporter() })],
});
metrics.setGlobalMeterProvider(meterProvider);
```

### Go

```go
traceExp, _ := otlptracehttp.New(ctx)
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
otel.SetTracerProvider(tp)
defer tp.Shutdown(ctx)

metricExp, _ := otlpmetrichttp.New(ctx)
mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))
otel.SetMeterProvider(mp)
defer mp.Shutdown(ctx)
```

## Verifying it works

Register an in-memory reader instead of an OTLP exporter and assert that instruments fired. In Python:

```python
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

reader = InMemoryMetricReader()
meter_provider = MeterProvider(metric_readers=[reader])
# ... run one generation ...
for resource_metrics in reader.get_metrics_data().resource_metrics:
    for scope_metrics in resource_metrics.scope_metrics:
        for metric in scope_metrics.metrics:
            print(metric.name)
```

`gen_ai.client.operation.duration` and `gen_ai.client.token.usage` should both appear. If the list is empty, the provider was registered after the client was constructed, or the client is using a different one.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| No conversation from third-party OTel instrumentation | Check exporter errors, `agento11y.record`, operation, provider/model attributes, and sampling |
| OTel conversation has no prompt or response | Enable content capture and check the message span attributes; metrics and OTLP logs alone do not supply messages |
| Calls split into separate conversations | Set the same `gen_ai.conversation.id` across traces for the conversation |
| Conversations appear, client-metrics panels are empty | Check the `MeterProvider`, exporter, and `metrics:write` scope |
| Generation cost is missing | Check reported token usage, provider/model identity, and the applicable model price |
| Analytics cannot be grouped by agent | `agent_name` is unset |
| Companion spans or metrics do not arrive | Check provider initialization, OTLP endpoint, token scopes, and exporter errors |
| Metrics stop just before exit | Providers shut down before `agento11y.shutdown()` |

For the shared coding-agent launcher, use `agento11y doctor` to check connection configuration. It does not validate a third-party framework's exported GenAI message format.
