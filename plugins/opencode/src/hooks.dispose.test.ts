// Teardown behavior: opencode's `Hooks.dispose` finalizer, the
// `server.instance.disposed` event, and the `beforeExit` fallback all have to
// reach one idempotent shutdown path.

import { beforeEach, describe, expect, it, vi } from "vitest";

const { createAgento11yClientMock, createTelemetryProvidersMock } = vi.hoisted(
  () => ({
    createAgento11yClientMock: vi.fn(),
    createTelemetryProvidersMock: vi.fn(),
  }),
);

vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));
vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: createTelemetryProvidersMock,
}));

import type { Agento11yOpencodeConfig } from "./config.js";
import {
  _peekToolExecutionState,
  _resetHookState,
  createAgento11yHooks,
} from "./hooks.js";
import {
  baseConfig,
  emitEvent,
  emitServerInstanceDisposed,
  makeAgento11yMock,
  makeOpencodeClient,
  type TestHooks,
} from "./hooks.testutil.js";

type TelemetryMock = {
  telemetry: {
    tracer: unknown;
    meter: unknown;
    forceFlush: ReturnType<typeof vi.fn>;
    shutdown: ReturnType<typeof vi.fn>;
  };
};

function makeTelemetryMock(calls: string[]): TelemetryMock {
  return {
    telemetry: {
      tracer: {},
      meter: {},
      forceFlush: vi.fn(async () => {}),
      shutdown: vi.fn(async () => {
        calls.push("telemetry.shutdown");
      }),
    },
  };
}

function otlpConfig(): Agento11yOpencodeConfig {
  return baseConfig({
    otlp: { endpoint: "http://127.0.0.1:1/otlp", headers: {} },
  });
}

/**
 * Builds a hooks instance with its own agento11y and telemetry mocks, plus a
 * shared ordered log of shutdown calls.
 */
async function makeInstance(): Promise<{
  hooks: TestHooks;
  sigil: { shutdown: ReturnType<typeof vi.fn> };
  telemetry: TelemetryMock["telemetry"];
  calls: string[];
}> {
  const calls: string[] = [];
  const { sigil } = makeAgento11yMock();
  sigil.shutdown = vi.fn(async () => {
    calls.push("sigil.shutdown");
  });
  const { telemetry } = makeTelemetryMock(calls);
  createAgento11yClientMock.mockReturnValue(sigil);
  createTelemetryProvidersMock.mockReturnValue(telemetry);

  const hooks = await createAgento11yHooks(otlpConfig(), makeOpencodeClient());
  if (!hooks) throw new Error("expected hooks");
  return { hooks, sigil, telemetry, calls };
}

/** Leaves an active tool execution behind so state clearing is observable. */
async function seedHookState(hooks: TestHooks): Promise<void> {
  await hooks.toolExecuteBefore(
    { sessionID: "sess-dispose", callID: "call-dispose", tool: "Bash" },
    { args: { command: "ls" } },
  );
  expect(_peekToolExecutionState().active).toHaveLength(1);
}

describe("opencode plugin teardown", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("shuts down agento11y before OTel and clears hook state on dispose", async () => {
    const { hooks, sigil, telemetry, calls } = await makeInstance();
    await seedHookState(hooks);

    await hooks.dispose();

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
    expect(calls).toEqual(["sigil.shutdown", "telemetry.shutdown"]);
    expect(_peekToolExecutionState().active).toHaveLength(0);
  });

  it("still shuts down OTel and clears state when agento11y shutdown rejects", async () => {
    const { hooks, sigil, telemetry } = await makeInstance();
    // Swapped in after construction: shutdownOnce reads the property at call
    // time, so this is the mock it invokes.
    sigil.shutdown = vi.fn(async () => {
      throw new Error("test shutdown failure");
    });
    await seedHookState(hooks);

    await expect(hooks.dispose()).resolves.toBeUndefined();

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
    expect(_peekToolExecutionState().active).toHaveLength(0);
  });

  it("keeps the idempotency guard per plugin instance", async () => {
    const first = await makeInstance();
    await first.hooks.dispose();
    expect(first.sigil.shutdown).toHaveBeenCalledTimes(1);

    const second = await makeInstance();
    await second.hooks.dispose();

    expect(second.sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(second.telemetry.shutdown).toHaveBeenCalledTimes(1);
    // The first instance did not shut down a second time.
    expect(first.sigil.shutdown).toHaveBeenCalledTimes(1);
  });

  it("keeps module state while another instance is still live", async () => {
    const first = await makeInstance();
    const second = await makeInstance();
    // Hook state is module-scoped and shared by both instances. One opencode
    // server hosts an instance per directory, so a sibling can be mid-turn.
    await seedHookState(second.hooks);

    await first.hooks.dispose();

    expect(first.sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(_peekToolExecutionState().active).toHaveLength(1);

    await second.hooks.dispose();

    expect(_peekToolExecutionState().active).toHaveLength(0);
  });

  it("shuts down on the server.instance.disposed event", async () => {
    const { hooks, sigil, telemetry, calls } = await makeInstance();
    await seedHookState(hooks);

    await emitServerInstanceDisposed(hooks);

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
    expect(calls).toEqual(["sigil.shutdown", "telemetry.shutdown"]);
    expect(_peekToolExecutionState().active).toHaveLength(0);
  });

  it("shuts down once when dispose and the disposal event both fire", async () => {
    const { hooks, sigil, telemetry } = await makeInstance();

    await hooks.dispose();
    await emitServerInstanceDisposed(hooks);
    await expect(hooks.dispose()).resolves.toBeUndefined();

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
  });

  it("shuts down once when triggers overlap before the first shutdown settles", async () => {
    const calls: string[] = [];
    const { sigil } = makeAgento11yMock();
    let releaseSigilShutdown: (() => void) | undefined;
    const sigilShutdownStarted = new Promise<void>((startResolve) => {
      sigil.shutdown = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            calls.push("sigil.shutdown");
            releaseSigilShutdown = resolve;
            startResolve();
          }),
      );
    });
    const { telemetry } = makeTelemetryMock(calls);
    createAgento11yClientMock.mockReturnValue(sigil);
    createTelemetryProvidersMock.mockReturnValue(telemetry);
    const hooks = await createAgento11yHooks(
      otlpConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    const disposing = hooks.dispose();
    await sigilShutdownStarted;
    // Second trigger arrives while the first shutdown is still in flight.
    let eventTeardownSettled = false;
    const eventTeardown = emitServerInstanceDisposed(hooks).then(() => {
      eventTeardownSettled = true;
    });
    // setImmediate runs after the microtask queue drains, so a boolean
    // "already shutting down" guard would have resolved the second trigger by
    // now, while the exporters are still draining. The memoized promise makes
    // it wait.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(eventTeardownSettled).toBe(false);

    releaseSigilShutdown?.();
    await Promise.all([disposing, eventTeardown]);

    expect(eventTeardownSettled).toBe(true);
    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
    expect(calls).toEqual(["sigil.shutdown", "telemetry.shutdown"]);
  });

  it("accepts only event names from the opencode Event union", async () => {
    const { hooks, sigil, telemetry } = await makeInstance();

    // The name the disposal branch used to be gated on. opencode never delivers
    // it to the `event` hook and it is not in the `Event` union.
    // @ts-expect-error not a member of the opencode Event union
    await emitEvent(hooks, "global.disposed");

    // Nothing in the plugin reacts to that name.
    expect(sigil.shutdown).not.toHaveBeenCalled();
    expect(telemetry.shutdown).not.toHaveBeenCalled();

    await hooks.dispose();
  });

  it("delegates the beforeExit fallback to the same shutdown path", async () => {
    const listenersBeforeCreate = process.listenerCount("beforeExit");
    const { hooks, sigil, telemetry } = await makeInstance();
    expect(process.listenerCount("beforeExit")).toBe(listenersBeforeCreate + 1);

    process.emit("beforeExit", 0);
    // A beforeExit listener cannot be awaited. Both shutdown mocks resolve
    // immediately, so one macrotask is enough for the shutdown it started to
    // run to completion.
    await new Promise<void>((resolve) => setImmediate(resolve));

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
    // The listener deregisters itself once shutdown starts, so a disposed
    // instance stops holding a process listener and a later beforeExit is a
    // no-op. A later dispose is stopped by the memoized promise instead.
    expect(process.listenerCount("beforeExit")).toBe(listenersBeforeCreate);

    process.emit("beforeExit", 0);
    await hooks.dispose();
    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
    expect(telemetry.shutdown).toHaveBeenCalledTimes(1);
  });
});
