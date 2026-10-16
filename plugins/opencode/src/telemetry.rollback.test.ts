import { beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  fail: "",
  created: new Map<string, { shutdown: ReturnType<typeof vi.fn> }>(),
}));
function fixtureConstructor(name: string) {
  return class {
    shutdown = vi.fn(async () => {});
    constructor() {
      if (state.fail === name) throw new Error(`fixture ${name}`);
      state.created.set(name, this);
    }
    getMeter() {
      return {};
    }
    getTracer() {
      return {};
    }
  };
}
vi.mock("@opentelemetry/exporter-metrics-otlp-http", () => ({
  OTLPMetricExporter: fixtureConstructor("metricExporter"),
}));
vi.mock("@opentelemetry/exporter-trace-otlp-http", () => ({
  OTLPTraceExporter: fixtureConstructor("traceExporter"),
}));
vi.mock("@opentelemetry/sdk-metrics", () => ({
  MeterProvider: fixtureConstructor("meterProvider"),
  PeriodicExportingMetricReader: fixtureConstructor("metricReader"),
}));
vi.mock("@opentelemetry/sdk-trace-base", () => ({
  BasicTracerProvider: fixtureConstructor("tracerProvider"),
  BatchSpanProcessor: fixtureConstructor("processor"),
}));

import { createOwnedTelemetryProviders } from "./telemetry.js";

describe("telemetry construction rollback", () => {
  beforeEach(() => {
    state.fail = "";
    state.created.clear();
  });

  it.each([
    ["metricExporter", []],
    ["metricReader", ["metricExporter"]],
    ["meterProvider", ["metricReader"]],
    ["traceExporter", ["meterProvider"]],
    ["processor", ["meterProvider", "traceExporter"]],
    ["tracerProvider", ["meterProvider", "processor"]],
  ] as const)("releases the existing owners when %s construction fails", async (fail, owners) => {
    state.fail = fail;
    await expect(
      createOwnedTelemetryProviders(
        { endpoint: "http://127.0.0.1:1", headers: {} },
        "fixture",
      ),
    ).rejects.toThrow(`fixture ${fail}`);
    for (const [name, resource] of state.created) {
      expect(resource.shutdown).toHaveBeenCalledTimes(
        owners.some((owner) => owner === name) ? 1 : 0,
      );
    }
  });
});
