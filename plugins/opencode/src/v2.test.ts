import { createRequire } from "node:module";
import type { Plugin } from "@opencode/plugin";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-http";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { baseConfig, makeAgento11yMock } from "./hooks.testutil.js";
import { LocalReceiverError } from "./local.js";
import { effectV2 } from "./v2-effect.js";
import { nativeFixture, nativeTranscript } from "./v2-testdata.js";

const mocks = vi.hoisted(() => ({
  loadConfig: vi.fn(),
  createClient: vi.fn(),
  createTelemetry: vi.fn(),
  createCapture: vi.fn(),
  clientShutdown: vi.fn(async () => {}),
  telemetryShutdown: vi.fn(async () => {}),
  captureShutdown: vi.fn(async () => {}),
  handleEvent: vi.fn(),
  observeToolExecution: vi.fn(),
  stopObservingTools: vi.fn(),
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
  const observerDispose = vi.fn(async () => {});
  const hook = vi.fn<Context["tool"]["hook"]>(async () => ({
    dispose: observerDispose,
  }));
  const context = {
    tool: { hook },
    skill: {
      list: vi.fn(async () => ({
        location: { directory: "/repo" },
        data: [
          {
            id: "opaque/id",
            name: "review",
            path: "/private/SKILL.md",
            content: "PRIVATE_CATALOG",
          },
        ],
      })),
    },
    app: { name: "opencode", version: "2.0.29", channel: "latest" },
    location: { directory: "/repo", workspaceID: "workspace-1" },
    event: { subscribe },
  } as unknown as Context;
  return { context, subscribe, signals, hook, observerDispose };
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
      observeToolExecution: mocks.observeToolExecution,
      stopObservingTools: mocks.stopObservingTools,
      shutdown: mocks.captureShutdown,
    });
    mocks.registerGuards.mockResolvedValue(mocks.guardDispose);
    mocks.registerFinalChecks.mockResolvedValue(mocks.finalChecksDispose);
    mocks.registerCopilot.mockResolvedValue({
      dispose: mocks.copilotDispose,
      refresh: mocks.copilotRefresh,
    });
  });

  it("verifies skills by default using only the supplied skill domain", async () => {
    mocks.loadConfig.mockResolvedValue(baseConfig());
    const { context } = host();
    const cleanup = await setupV2(context);
    const options = mocks.createCapture.mock.calls[0]?.[0];
    expect(options.config).not.toHaveProperty("experimentalToolMetrics");
    expect(mocks.createClient.mock.calls[0]?.[0]).not.toHaveProperty(
      "experimentalToolMetrics",
    );
    expect(context.skill.list).not.toHaveBeenCalled();
    await options.verificationReady;
    const abort = new AbortController();
    expect(await options.verifySkill("opaque/id", abort.signal)).toBe("review");
    expect(context.skill.list).toHaveBeenCalledExactlyOnceWith(
      { location: { directory: "/repo" } },
      { signal: abort.signal },
    );
    await cleanup?.();
  });

  it("never starts lookup during callbacks or incomplete setup registrations", async () => {
    vi.useFakeTimers();
    const { createNativeCapture } =
      await vi.importActual<typeof import("./v2-events.js")>("./v2-events.js");
    mocks.loadConfig.mockResolvedValue(baseConfig());
    const { context, hook } = host();
    let release!: (dispose: () => Promise<void>) => void;
    mocks.registerGuards.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    let capture!: ReturnType<typeof createNativeCapture>;
    mocks.createCapture.mockImplementation((options) => {
      capture = createNativeCapture(options);
      const started = nativeTranscript().find(
        (entry) => entry.type === "session.step.started",
      )!;
      capture.handleEvent({ ...started, location: context.location });
      return capture;
    });
    try {
      const setup = setupV2(context);
      await vi.waitFor(() =>
        expect(mocks.registerGuards).toHaveBeenCalledOnce(),
      );
      const started = nativeTranscript().find(
        (entry) => entry.type === "session.step.started",
      )!;
      if (!("assistantMessageID" in started.data))
        throw new Error("missing message");
      const observe = hook.mock.calls[0]?.[1];
      expect(
        observe?.({
          sessionID: started.data.sessionID,
          messageID: started.data.assistantMessageID,
          id: "c",
          agent: "build",
          tool: "skill",
          input: { id: "opaque/id" },
        } as never),
      ).toBeUndefined();
      await vi.advanceTimersByTimeAsync(0);
      expect(context.skill.list).not.toHaveBeenCalled();
      release(mocks.guardDispose);
      const cleanup = await setup;
      expect(context.skill.list).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(0);
      expect(context.skill.list).toHaveBeenCalledOnce();
      await cleanup?.();
    } finally {
      await capture?.shutdown();
      vi.useRealTimers();
    }
  });

  it.each([
    [undefined, undefined],
    ["true", undefined],
    ["false", undefined],
    [undefined, "true"],
  ])("ignores removed metric env=%s and legacy alias=%s", async (value, legacy) => {
    const keys = [
      "AGENTO11Y_EXPERIMENTAL_TOOL_METRICS",
      "SIGIL_EXPERIMENTAL_TOOL_METRICS",
    ];
    const saved = keys.map((key) => process.env[key]);
    try {
      for (const [index, setting] of [value, legacy].entries()) {
        const key = keys[index]!;
        if (setting === undefined) delete process.env[key];
        else process.env[key] = setting;
      }
      const { context } = host();
      const cleanup = await setupV2(context);
      const options = mocks.createCapture.mock.calls[0]?.[0];
      expect(options.config).not.toHaveProperty("experimentalToolMetrics");
      expect(mocks.createClient.mock.calls[0]?.[0]).not.toHaveProperty(
        "experimentalToolMetrics",
      );
      await options.verificationReady;
      expect(
        await options.verifySkill("opaque/id", new AbortController().signal),
      ).toBe("review");
      await cleanup?.();
    } finally {
      keys.forEach((key, index) => {
        if (saved[index] === undefined) delete process.env[key];
        else process.env[key] = saved[index];
      });
    }
  });

  it.each([
    "local",
    "tenant",
    "none",
  ] as const)("ignores unrelated SDK auth during %s capture setup", async (mode) => {
    vi.stubEnv("AGENTO11Y_AUTH_MODE", "basic");
    vi.stubEnv("AGENTO11Y_AUTH_TOKEN", undefined);
    mocks.loadConfig.mockResolvedValue(
      baseConfig({
        local: mode === "local",
        auth:
          mode === "local"
            ? {
                mode: "basic",
                basicUser: "local",
                basicPassword: "local",
                tenantId: "local",
              }
            : mode === "tenant"
              ? { mode: "tenant", tenantId: "fixture" }
              : { mode: "none" },
      }),
    );
    try {
      const cleanup = await setupV2(host().context);
      expect(mocks.createClient).toHaveBeenCalledOnce();
      expect(mocks.createClient.mock.calls[0]?.[0].auth).toEqual(
        (await mocks.loadConfig()).auth,
      );
      await cleanup?.();
    } finally {
      vi.unstubAllEnvs();
    }
  });

  it.each([
    "timeout",
    "replacement",
    "plugin cleanup",
  ])("cancels native catalog work after capture %s with the installed Promise adapter", async (kind) => {
    const { Effect, Stream } = createRequire(
      import.meta.resolve("@opencode/plugin"),
    )("effect");
    const { createNativeCapture } =
      await vi.importActual<typeof import("./v2-events.js")>("./v2-events.js");
    const { sigil } = makeAgento11yMock();
    mocks.createClient.mockReturnValue(sigil);
    mocks.loadConfig.mockResolvedValue(baseConfig());
    let capture!: ReturnType<typeof createNativeCapture>;
    mocks.createCapture.mockImplementation((options) => {
      capture = createNativeCapture(options);
      return capture;
    });
    let started = 0;
    let interrupted = 0;
    const location = { directory: "/repo", workspaceID: "w1" };
    const stub = new Proxy(() => Effect.void, {
      get: (_target, key) => (key === "then" ? undefined : stub),
    });
    const nativeHost = new Proxy(
      {},
      {
        get: (_target, key) => {
          if (key === "location") return location;
          if (key === "app") return { name: "opencode", version: "2.0.22" };
          if (key === "event") return { subscribe: () => Stream.never };
          if (key === "tool")
            return { hook: () => Effect.succeed({ dispose: Effect.void }) };
          if (key === "skill")
            return {
              list: () =>
                Effect.callback(() => {
                  started++;
                  return Effect.sync(() => {
                    interrupted++;
                  });
                }),
            };
          return stub;
        },
      },
    );
    await Effect.runPromise(
      Effect.scoped(
        Effect.andThen(
          effectV2(nativeHost),
          Effect.promise(async () => {
            const events = nativeTranscript().map((entry) => ({
              ...entry,
              location,
            }));
            for (const entry of events) {
              if (
                entry.type === "session.step.ended" ||
                entry.type === "session.execution.succeeded"
              )
                continue;
              if (entry.type === "session.tool.input.started") {
                capture.handleEvent({
                  ...entry,
                  data: { ...entry.data, name: "skill" },
                });
              } else capture.handleEvent(entry);
            }
            const observation = {
              sessionID: nativeFixture.sessionID,
              messageID: nativeFixture.assistantMessageID,
              id: nativeFixture.toolID,
              tool: "skill",
              agent: "build",
              skillName: "review",
              rawSkillID: "opaque/id",
            };
            capture.observeToolExecution(observation);
            await vi.waitFor(() => expect(started).toBe(1));
            if (kind === "replacement") {
              capture.observeToolExecution({
                ...observation,
                rawSkillID: "second/id",
              });
              await vi.waitFor(() => {
                expect(started).toBe(2);
                expect(interrupted).toBe(1);
              });
            } else if (kind === "timeout") {
              capture.handleEvent(
                events.find((entry) => entry.type === "session.step.ended")!,
              );
              await capture.drain();
              await vi.waitFor(() => expect(interrupted).toBe(1));
              expect(sigil.startToolExecution).toHaveBeenCalledOnce();
              expect(
                sigil.startToolExecution.mock.results[0].value.setResult.mock
                  .calls[0][0].verifiedSkillName,
              ).toBeUndefined();
            }
          }),
        ),
      ),
    );
    expect(interrupted).toBe(started);
    expect(sigil.shutdown).toHaveBeenCalledOnce();
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

  it.each([
    true,
    false,
  ])("observes final input independently of guards=%s", async (enabled) => {
    mocks.loadConfig.mockResolvedValue(
      baseConfig({ guards: { enabled, timeoutMs: 1, failOpen: false } }),
    );
    const { context, hook, observerDispose } = host();
    Object.defineProperty(context, "session", {
      get: () => {
        throw new Error("no session lookup");
      },
    });
    Object.defineProperty(context.tool, "list", {
      get: () => {
        throw new Error("no tool lookup");
      },
    });
    const cleanup = await setupV2(context);
    expect(hook).toHaveBeenCalledOnce();
    expect(hook.mock.calls[0]?.[0]).toBe("execute.after");
    const observe = (payload: unknown) =>
      hook.mock.calls[0]?.[1](payload as never);
    const ids = {
      tool: "skill",
      sessionID: "s",
      messageID: "a",
      id: "c",
      agent: "build",
    };
    for (const status of ["completed", "error"]) {
      const payload = Object.freeze({
        ...ids,
        status,
        input: Object.freeze({ id: "final-skill", name: "v1-name" }),
        get result() {
          throw new Error("display name is not input");
        },
        get error() {
          throw new Error("error is not input");
        },
      });
      expect(observe(payload)).toBeUndefined();
      expect(observe(payload)).toBeUndefined();
    }
    expect(mocks.observeToolExecution).toHaveBeenCalledTimes(4);
    expect(mocks.observeToolExecution).toHaveBeenLastCalledWith({
      ...ids,
      skillName: "final-skill",
      rawSkillID: "final-skill",
    });
    expect(
      Object.keys(mocks.observeToolExecution.mock.calls[0]?.[0]).sort(),
    ).toEqual(
      [
        "tool",
        "sessionID",
        "messageID",
        "id",
        "agent",
        "skillName",
        "rawSkillID",
      ].sort(),
    );
    for (const input of [
      null,
      {},
      { name: "v1-only" },
      { id: 42 },
      { id: "" },
    ]) {
      expect(() =>
        observe({ ...ids, status: "completed", input }),
      ).not.toThrow();
      expect(mocks.observeToolExecution).toHaveBeenLastCalledWith({
        ...ids,
        skillName: null,
        rawSkillID: typeof input?.id === "string" ? input.id : null,
      });
    }
    expect(() =>
      observe({
        ...ids,
        get input() {
          throw new Error("malformed");
        },
      }),
    ).not.toThrow();
    expect(mocks.observeToolExecution).toHaveBeenLastCalledWith({
      ...ids,
      skillName: null,
      rawSkillID: null,
    });
    for (const payload of [
      undefined,
      null,
      Object.freeze({}),
      new Proxy(
        {},
        {
          get() {
            throw new Error("malformed");
          },
        },
      ),
    ])
      expect(() => observe(payload)).not.toThrow();
    mocks.observeToolExecution.mockImplementationOnce(() => {
      throw new Error("capture failure");
    });
    expect(() => observe({ ...ids, input: { id: "valid" } })).not.toThrow();
    if (!cleanup) throw new Error("missing cleanup");
    await cleanup();
    expect(observerDispose).toHaveBeenCalledOnce();
  });

  it.each([
    "success",
    "disposal failure",
  ])("disables observation synchronously before intake/disposal: %s", async (stage) => {
    const { context, hook, observerDispose } = host();
    let finishIntake!: () => void;
    context.event.subscribe = vi.fn(() =>
      (async function* () {
        await new Promise<void>((resolve) => {
          finishIntake = resolve;
        });
      })(),
    );
    const cleanup = await setupV2(context);
    if (!cleanup) throw new Error("missing cleanup");
    if (stage === "disposal failure")
      observerDispose.mockRejectedValueOnce(new Error("dispose"));
    const pending = cleanup();
    expect(mocks.stopObservingTools).toHaveBeenCalledOnce();
    expect(observerDispose).not.toHaveBeenCalled();
    hook.mock.calls[0]?.[1]({
      tool: "skill",
      sessionID: "s",
      messageID: "a",
      id: "c",
      agent: "build",
      input: { id: "late" },
    } as never);
    expect(mocks.observeToolExecution).not.toHaveBeenCalled();
    finishIntake();
    await pending;
    expect(observerDispose).toHaveBeenCalledOnce();
    expect(mocks.captureShutdown).toHaveBeenCalledOnce();
    expect(mocks.clientShutdown).toHaveBeenCalledOnce();
  });

  it.each([
    "observer",
    "guards",
    "final checks",
    "subscription",
  ])("rolls back the passive observer after %s registration failure", async (stage) => {
    const { context, hook, subscribe, observerDispose } = host();
    const failure = new Error("registration failure");
    if (stage === "observer") hook.mockRejectedValueOnce(failure);
    if (stage === "guards") mocks.registerGuards.mockRejectedValueOnce(failure);
    if (stage === "final checks")
      mocks.registerFinalChecks.mockRejectedValueOnce(failure);
    if (stage === "subscription")
      subscribe.mockImplementationOnce(() => {
        throw failure;
      });
    observerDispose.mockRejectedValueOnce(new Error("disposal failure"));
    await expect(setupV2(context)).rejects.toBe(failure);
    expect(mocks.stopObservingTools).toHaveBeenCalledOnce();
    expect(observerDispose).toHaveBeenCalledTimes(stage === "observer" ? 0 : 1);
    expect(mocks.captureShutdown).toHaveBeenCalledOnce();
    expect(mocks.clientShutdown).toHaveBeenCalledOnce();
    expect(mocks.telemetryShutdown).toHaveBeenCalledOnce();
    hook.mock.calls[0]?.[1]({ tool: "skill", input: { id: "late" } } as never);
    expect(mocks.observeToolExecution).not.toHaveBeenCalled();
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
