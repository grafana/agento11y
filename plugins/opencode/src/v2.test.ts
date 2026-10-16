import type { Plugin } from "@opencode/plugin";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-http";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { baseConfig } from "./hooks.testutil.js";
import { LocalReceiverError } from "./local.js";

const mocks = vi.hoisted(() => ({
  loadConfig: vi.fn(),
  createClient: vi.fn(),
  createTelemetry: vi.fn(),
  createCapture: vi.fn(),
  clientShutdown: vi.fn(async () => {}),
  telemetryShutdown: vi.fn(async () => {}),
  captureShutdown: vi.fn(async () => {}),
  handleEvent: vi.fn(),
  registerGuards: vi.fn(),
  registerFinalChecks: vi.fn(),
  finalChecksDispose: vi.fn(async () => {}),
  registerCopilot: vi.fn(),
  guardDispose: vi.fn(async () => {}),
  forgetIdentity: vi.fn(),
  copilotDispose: vi.fn(async () => {}),
  copilotRefresh: vi.fn(async () => {}),
}));
vi.mock("./config.js", () => ({
  loadConfig: mocks.loadConfig,
  resolveAutoTagValues: () => ({ project: "fixture" }),
}));
vi.mock("./client.js", () => ({ createAgento11yClient: mocks.createClient }));
vi.mock("./telemetry.js", () => ({
  createOwnedTelemetryProviders: mocks.createTelemetry,
}));
vi.mock("./v2-events.js", () => ({ createNativeCapture: mocks.createCapture }));
vi.mock("./v2-guards.js", () => ({
  createNativeGuards: () => ({
    register: mocks.registerGuards,
    forget: mocks.forgetIdentity,
  }),
}));
vi.mock("./v2-request-guards.js", () => ({
  createFinalCompactionGuards: (args: unknown) => ({
    register: () => mocks.registerFinalChecks(args),
  }),
}));
vi.mock("./v2-copilot.js", () => ({
  registerCopilotTitleProtection: mocks.registerCopilot,
}));

import { setupV2 } from "./v2.js";

type Context = Parameters<Plugin.Plugin["setup"]>[0];
function host() {
  const signals: AbortSignal[] = [];
  const subscribe = vi.fn(({ signal }: { signal: AbortSignal }) => {
    signals.push(signal);
    return (async function* () {
      await new Promise<void>((resolve) => {
        signal.addEventListener("abort", () => resolve(), { once: true });
      });
    })();
  });
  const context = {
    app: { name: "opencode", version: "2.0.29", channel: "latest" },
    location: { directory: "/repo", workspaceID: "workspace-1" },
    event: { subscribe },
  } as unknown as Context;
  return { context, subscribe, signals };
}

describe("native setup ownership", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    mocks.loadConfig.mockResolvedValue(
      baseConfig({ otlp: { endpoint: "http://127.0.0.1:1", headers: {} } }),
    );
    mocks.createClient.mockReturnValue({ shutdown: mocks.clientShutdown });
    mocks.createTelemetry.mockResolvedValue({
      tracer: {},
      meter: {},
      shutdown: mocks.telemetryShutdown,
    });
    mocks.createCapture.mockReturnValue({
      handleEvent: mocks.handleEvent,
      shutdown: mocks.captureShutdown,
    });
    mocks.registerGuards.mockResolvedValue(mocks.guardDispose);
    mocks.registerFinalChecks.mockResolvedValue(mocks.finalChecksDispose);
    mocks.registerCopilot.mockResolvedValue({
      dispose: mocks.copilotDispose,
      refresh: mocks.copilotRefresh,
    });
  });

  it("starts intake without waiting for host initialization and cleans up once", async () => {
    const { context, subscribe, signals } = host();
    const cleanup = await setupV2(context);
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(mocks.createCapture.mock.calls[0]?.[0]).toMatchObject({
      directory: "/repo",
      workspaceID: "workspace-1",
      hostVersion: "2.0.29",
    });
    expect(mocks.createClient.mock.calls[0]?.[0].autoTags).toEqual({
      project: "fixture",
    });
    if (!cleanup) throw new Error("missing cleanup");
    await Promise.all([cleanup(), cleanup()]);
    expect(signals[0]?.aborted).toBe(true);
    expect(mocks.captureShutdown).toHaveBeenCalledTimes(1);
    expect(mocks.clientShutdown).toHaveBeenCalledTimes(1);
    expect(mocks.telemetryShutdown).toHaveBeenCalledTimes(1);
    expect(mocks.captureShutdown.mock.invocationCallOrder[0]).toBeLessThan(
      mocks.clientShutdown.mock.invocationCallOrder[0] ?? 0,
    );
  });

  it.each([
    "client",
    "capture",
    "subscription",
  ])("rolls back resources if %s initialization fails before returning cleanup", async (stage) => {
    const { context, subscribe } = host();
    const failure = new Error("fixture initialization failure");
    if (stage === "client")
      mocks.createClient.mockImplementation(() => {
        throw failure;
      });
    if (stage === "capture")
      mocks.createCapture.mockImplementation(() => {
        throw failure;
      });
    if (stage === "subscription")
      subscribe.mockImplementation(() => {
        throw failure;
      });
    await expect(setupV2(context)).rejects.toBe(failure);
    expect(mocks.telemetryShutdown).toHaveBeenCalledOnce();
    expect(mocks.clientShutdown).toHaveBeenCalledTimes(
      stage === "client" ? 0 : 1,
    );
    expect(mocks.captureShutdown).toHaveBeenCalledTimes(
      stage === "subscription" ? 1 : 0,
    );
  });

  it("rolls back telemetry when the shared client factory returns null", async () => {
    mocks.createClient.mockReturnValue(null);
    await expect(setupV2(host().context)).resolves.toBeUndefined();
    expect(mocks.telemetryShutdown).toHaveBeenCalledOnce();
    expect(mocks.createCapture).not.toHaveBeenCalled();
  });

  it("preserves capture failure while attempting both failing export channels", async () => {
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      mocks.captureShutdown.mockRejectedValueOnce(
        new Error("settlement failed"),
      );
      mocks.clientShutdown.mockRejectedValueOnce(new Error("export failed"));
      mocks.telemetryShutdown.mockRejectedValueOnce(
        new Error("fixture secret"),
      );
      const cleanup = await setupV2(host().context);
      if (!cleanup) throw new Error("missing cleanup");
      await expect(cleanup()).rejects.toThrow("settlement failed");
      expect(mocks.clientShutdown).toHaveBeenCalledOnce();
      expect(mocks.telemetryShutdown).toHaveBeenCalledOnce();
      expect(warning.mock.calls).toEqual([
        ["[agento11y-opencode] final OTLP drain failed; telemetry may be lost"],
      ]);
    } finally {
      warning.mockRestore();
    }
  });

  it.each([
    "success",
    "client only",
    "OTLP async",
    "OTLP sync",
    "both",
  ])("reports final OTLP drain failures without payloads: %s", async (failure) => {
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const error = new Error("fixture payload and credentials");
      if (failure === "client only" || failure === "both")
        mocks.clientShutdown.mockRejectedValueOnce(error);
      if (failure === "OTLP async" || failure === "both")
        mocks.telemetryShutdown.mockRejectedValueOnce(error);
      if (failure === "OTLP sync")
        mocks.telemetryShutdown.mockImplementationOnce(() => {
          throw error;
        });
      const cleanup = await setupV2(host().context);
      if (!cleanup) throw new Error("missing cleanup");
      await Promise.all([cleanup(), cleanup()]);
      expect(mocks.clientShutdown).toHaveBeenCalledOnce();
      expect(mocks.telemetryShutdown).toHaveBeenCalledOnce();
      const otlpFailed = failure.startsWith("OTLP") || failure === "both";
      expect(warning.mock.calls).toEqual(
        otlpFailed
          ? [
              [
                "[agento11y-opencode] final OTLP drain failed; telemetry may be lost",
              ],
            ]
          : [],
      );
    } finally {
      warning.mockRestore();
    }
  });

  it("reports failure from a real final trace export and still drains metrics", async () => {
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    const traces = vi
      .spyOn(OTLPTraceExporter.prototype, "export")
      .mockImplementation((_spans, callback) => {
        callback({ code: 1, error: new Error("fixture secret trace payload") });
      });
    const metrics = vi
      .spyOn(OTLPMetricExporter.prototype, "export")
      .mockImplementation((_data, callback) => callback({ code: 0 }));
    try {
      const { createOwnedTelemetryProviders } =
        await vi.importActual<typeof import("./telemetry.js")>(
          "./telemetry.js",
        );
      mocks.createTelemetry.mockImplementation(async () => {
        const providers = await createOwnedTelemetryProviders(
          { endpoint: "http://127.0.0.1:1", headers: {} },
          "fixture",
        );
        providers.tracer.startSpan("fixture").end();
        providers.meter.createCounter("fixture").add(1);
        return providers;
      });
      const cleanup = await setupV2(host().context);
      if (!cleanup) throw new Error("missing cleanup");
      await cleanup();
      expect(traces).toHaveBeenCalledOnce();
      expect(metrics).toHaveBeenCalledOnce();
      expect(mocks.clientShutdown).toHaveBeenCalledOnce();
      expect(warning.mock.calls).toEqual([
        ["[agento11y-opencode] final OTLP drain failed; telemetry may be lost"],
      ]);
    } finally {
      warning.mockRestore();
      traces.mockRestore();
      metrics.mockRestore();
    }
  });

  it.each([
    "unconfigured",
    "local unavailable",
  ])("does not construct exporters when %s", async (kind) => {
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      if (kind === "unconfigured") mocks.loadConfig.mockResolvedValue(null);
      else
        mocks.loadConfig.mockRejectedValue(new LocalReceiverError("fixture"));
      await expect(setupV2(host().context)).resolves.toBeUndefined();
      expect(mocks.createTelemetry).not.toHaveBeenCalled();
      expect(mocks.createClient).not.toHaveBeenCalled();
    } finally {
      warning.mockRestore();
    }
  });

  it.each([
    "success",
    "guard registration failure",
    "subscription failure",
  ])("owns guarded registrations through %s", async (stage) => {
    mocks.loadConfig.mockResolvedValue(
      baseConfig({
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      }),
    );
    const { context, subscribe } = host();
    if (stage === "guard registration failure")
      mocks.registerGuards.mockRejectedValueOnce(
        new Error("fixture registration"),
      );
    if (stage === "subscription failure")
      subscribe.mockImplementation(() => {
        throw new Error("fixture subscription");
      });
    if (stage === "success") {
      const cleanup = await setupV2(context);
      if (!cleanup) throw new Error("missing cleanup");
      await cleanup();
    } else await expect(setupV2(context)).rejects.toThrow("fixture");
    expect(mocks.registerCopilot).toHaveBeenCalledOnce();
    expect(mocks.copilotDispose).toHaveBeenCalledOnce();
    expect(mocks.guardDispose).toHaveBeenCalledTimes(
      stage === "guard registration failure" ? 0 : 1,
    );
    expect(mocks.finalChecksDispose).toHaveBeenCalledTimes(
      stage === "guard registration failure" ? 0 : 1,
    );
    if (stage !== "guard registration failure")
      expect(mocks.registerFinalChecks.mock.calls[0]?.[0]).toMatchObject({
        agentVersion: "test-version",
      });
    expect(mocks.clientShutdown).toHaveBeenCalledOnce();
    expect(mocks.copilotDispose.mock.invocationCallOrder[0]).toBeLessThan(
      mocks.clientShutdown.mock.invocationCallOrder[0] ?? 0,
    );
  });

  it("does not remove the title agent with guards disabled", async () => {
    const cleanup = await setupV2(host().context);
    if (!cleanup) throw new Error("missing cleanup");
    await cleanup();
    expect(mocks.registerCopilot).not.toHaveBeenCalled();
  });

  it.each([
    true,
    false,
  ])("processes intake separately from export settlement with guards=%s", async (enabled) => {
    mocks.loadConfig.mockResolvedValue(
      baseConfig({ guards: { enabled, timeoutMs: 1500, failOpen: true } }),
    );
    const { context } = host();
    let release: (() => void) | undefined;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    mocks.handleEvent.mockReturnValue(pending);
    context.event.subscribe = vi.fn(() =>
      (async function* () {
        yield { type: "plugin.updated" } as never;
        yield {
          type: "session.deleted",
          data: { sessionID: "deleted" },
        } as never;
      })(),
    );
    const cleanup = await setupV2(context);
    await vi.waitFor(() => expect(mocks.handleEvent).toHaveBeenCalledTimes(2));
    // Plugin activation can reorder transforms; deletion ends a session identity.
    expect(mocks.copilotRefresh).toHaveBeenCalledTimes(enabled ? 1 : 0);
    expect(mocks.forgetIdentity.mock.calls).toEqual([["deleted"]]);
    release?.();
    if (!cleanup) throw new Error("missing cleanup");
    await cleanup();
  });
});
