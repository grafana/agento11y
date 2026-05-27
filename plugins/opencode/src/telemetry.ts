/**
 * Sets up OTel SDK providers for metrics and traces, exporting via OTLP/HTTP.
 * Returns a tracer and meter to pass into SigilClient so its internal
 * instruments actually export data.
 */

import type { Meter, Tracer } from "@opentelemetry/api";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-http";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import {
  defaultResource,
  resourceFromAttributes,
} from "@opentelemetry/resources";
import {
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BasicTracerProvider,
  BatchSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import type { OtlpConfig } from "./config.js";

const INSTRUMENTATION_SCOPE = "sigil-opencode";
const SERVICE_NAME = "sigil-opencode";

export interface TelemetryProviders {
  tracer: Tracer;
  meter: Meter;
  forceFlush: () => Promise<void>;
  shutdown: () => Promise<void>;
}

export function createTelemetryProviders(
  otlp: OtlpConfig,
  instanceId: string,
): TelemetryProviders {
  const base = otlp.endpoint.replace(/\/+$/, "");
  // service.instance.id MUST disambiguate concurrent opencode processes on the
  // same host. Unlike pi, opencode runs many sessions per process, so the id
  // is generated once per plugin instance rather than per session.
  const resource = defaultResource().merge(
    resourceFromAttributes({
      "service.name": SERVICE_NAME,
      "service.instance.id": instanceId,
    }),
  );

  const metricExporter = new OTLPMetricExporter({
    url: `${base}/v1/metrics`,
    headers: otlp.headers,
  });
  const metricReader = new PeriodicExportingMetricReader({
    exporter: metricExporter,
    exportIntervalMillis: 15_000,
    exportTimeoutMillis: 15_000,
  });
  const meterProvider = new MeterProvider({
    resource,
    readers: [metricReader],
  });

  const traceExporter = new OTLPTraceExporter({
    url: `${base}/v1/traces`,
    headers: otlp.headers,
  });
  const tracerProvider = new BasicTracerProvider({
    resource,
    spanProcessors: [new BatchSpanProcessor(traceExporter)],
  });

  return {
    tracer: tracerProvider.getTracer(INSTRUMENTATION_SCOPE),
    meter: meterProvider.getMeter(INSTRUMENTATION_SCOPE),
    forceFlush: async () => {
      await settleOrThrow(
        [meterProvider.forceFlush(), tracerProvider.forceFlush()],
        "telemetry force flush failed",
      );
    },
    shutdown: async () => {
      await settleOrThrow(
        [meterProvider.shutdown(), tracerProvider.shutdown()],
        "telemetry shutdown failed",
      );
    },
  };
}

async function settleOrThrow(promises: Promise<void>[], message: string) {
  // Both providers must attempt the operation even if one fails, so we use
  // allSettled and aggregate errors afterwards rather than short-circuiting.
  const results = await Promise.allSettled(promises);
  const reasons = results
    .filter((r): r is PromiseRejectedResult => r.status === "rejected")
    .map((r) => r.reason);
  if (reasons.length > 0) {
    throw new AggregateError(reasons, message);
  }
}
