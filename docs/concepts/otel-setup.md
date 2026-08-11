# OpenTelemetry Setup

Agent Observability is fed by **two independent pipelines**. Configuring one does not configure the other, and the SDK cannot tell you when the second one is missing.

| Pipeline | Carries | Configured by | Shows up as |
| --- | --- | --- | --- |
| **Generation export** | Prompts, responses, token usage, tool calls, workflow steps | `AGENTO11Y_ENDPOINT` + `AGENTO11Y_API_KEY` (or `agento11y login`) | Conversations and generations |
| **OTel spans and metrics** | `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, `gen_ai.client.time_to_first_token`, `gen_ai.client.tool_calls_per_operation`, and generation spans | A `TracerProvider` and `MeterProvider` **you** register | Analytics dashboards, cost and latency panels, traces |

**The SDK does not create OTel providers.** If your application never registers them, the OTel API hands the SDK a no-op provider, every metric is discarded in-process, and nothing is logged. Conversations keep arriving normally, so the failure looks like "Agent Observability works but analytics is empty."

Cost is derived server-side from `gen_ai.client.token.usage` plus the model name. No token metrics means no cost data.

## Rules

1. Register the providers **before** constructing the agento11y client. The client resolves its tracer and meter once, at construction.
2. Shut the providers down **after** `agento11y.shutdown()`, so the final flush has somewhere to go.
3. Set `agent_name` (and ideally `agent_version`) on the client or handler. They become metric labels, and analytics views group by them.

You can hand providers to the client explicitly via config (`tracer` / `meter`) instead of registering them globally. Explicit providers win over the globals.

## Delivery options

The OTel SDK exporters read `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS` from the environment, so the snippets below need no endpoint arguments.

**Direct to Grafana Cloud.** Point OTLP at the Grafana Cloud OTLP gateway. Authentication is Basic auth using your OTLP instance ID and a `glc_…` token. Note this is a *different* credential from the generation-export API key, and a token missing the OTLP metrics scope fails silently from the application's point of view.

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

```typescript
import { metrics } from '@opentelemetry/api';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { MeterProvider, PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http';

const tracerProvider = new NodeTracerProvider({ resource });
tracerProvider.addSpanProcessor(new BatchSpanProcessor(new OTLPTraceExporter()));
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
| Conversations appear, analytics panels are empty | No `MeterProvider` registered, or registered after the client was built |
| Metrics exist but cost is missing | Token usage is not being recorded; check that the provider wrapper or handler sees usage on the response |
| Analytics cannot be grouped by agent | `agent_name` is unset |
| Nothing exports, no errors | OTLP token missing the metrics scope, or `OTEL_EXPORTER_OTLP_ENDPOINT` unset |
| Metrics stop just before exit | Providers shut down before `agento11y.shutdown()` |

For the coding-agent plugins, `agento11y doctor` reports these directly.
