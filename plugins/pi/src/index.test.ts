import { beforeEach, describe, expect, it, vi } from "vitest";

const {
  loadConfigMock,
  createAgento11yClientMock,
  createTelemetryProvidersMock,
  resolveGitBranchMock,
  loggerMock,
} = vi.hoisted(() => ({
  loadConfigMock: vi.fn(),
  createAgento11yClientMock: vi.fn(),
  createTelemetryProvidersMock: vi.fn(),
  resolveGitBranchMock: vi.fn(),
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

vi.mock("./config.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./config.js")>();
  return {
    ...actual,
    loadConfig: loadConfigMock,
  };
});

vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));

vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: createTelemetryProvidersMock,
}));

vi.mock("./git.js", () => ({
  resolveGitBranch: resolveGitBranchMock,
}));

import type { Agento11yClient } from "@grafana/agento11y";
import registerExtension, { emitToolSpans } from "./index.js";
import type {
  PiAssistantMessage,
  PiToolResult,
  ToolTiming,
} from "./mappers.js";

interface RecorderLike {
  setResult: (value: unknown) => void;
  setCallError: (error: Error) => void;
  setFirstTokenAt?: (firstTokenAt: Date) => void;
}

interface ToolRecorderLike {
  setResult: ReturnType<typeof vi.fn>;
  setCallError: ReturnType<typeof vi.fn>;
  end: ReturnType<typeof vi.fn>;
  getError: ReturnType<typeof vi.fn>;
}

interface Agento11yLike {
  startStreamingGeneration: (
    seed: unknown,
    run: (recorder: RecorderLike) => Promise<void>,
  ) => Promise<void>;
  startToolExecution: ReturnType<typeof vi.fn>;
  shutdown: () => Promise<void>;
}

function assistantMessageUpdate(
  overrides?: Partial<{ type: string; delta: string }>,
) {
  return {
    message: {
      role: "assistant",
      content: [{ type: "text", text: "h" }],
      provider: "anthropic",
      model: "claude-sonnet-4",
      usage: {
        input: 0,
        output: 0,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 0,
      },
      stopReason: "stop",
      timestamp: Date.now(),
    },
    assistantMessageEvent: {
      type: overrides?.type ?? "text_delta",
      contentIndex: 0,
      delta: overrides?.delta ?? "h",
      partial: {},
    },
  };
}

class FakePi {
  handlers = new Map<string, (event: any, ctx: any) => Promise<void> | void>();
  getAllTools?: () => unknown;
  getActiveTools?: () => string[];

  on(event: string, handler: (event: any, ctx: any) => Promise<void> | void) {
    this.handlers.set(event, handler);
  }

  async emit(event: string, payload: any = {}, ctx: any = defaultCtx) {
    const handler = this.handlers.get(event);
    if (!handler) return;
    await handler(payload, ctx);
  }
}

const defaultCtx = {
  sessionManager: {
    getSessionFile: () => "session-1",
    getSessionId: () => "sess-default-id",
  },
};

function makeCtx({
  sessionFile,
  sessionId,
}: {
  sessionFile?: string | (() => string | undefined);
  sessionId: string | (() => string);
}) {
  const fileFn =
    typeof sessionFile === "function"
      ? sessionFile
      : () => sessionFile ?? "session-1";
  const idFn = typeof sessionId === "function" ? sessionId : () => sessionId;
  return {
    sessionManager: {
      getSessionFile: fileFn,
      getSessionId: idFn,
    },
  };
}

function assistantMessage() {
  return {
    role: "assistant",
    content: [{ type: "text", text: "hello" }],
    provider: "anthropic",
    model: "claude-sonnet-4",
    usage: {
      input: 10,
      output: 20,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 30,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
    stopReason: "stop",
    timestamp: Date.now(),
  };
}

describe("extension lifecycle", () => {
  beforeEach(() => {
    loadConfigMock.mockReset();
    createAgento11yClientMock.mockReset();
    createTelemetryProvidersMock.mockReset();
    // Default: no git repo. Individual tests opt into a branch by overriding.
    resolveGitBranchMock.mockReset();
    resolveGitBranchMock.mockReturnValue(undefined);
    loggerMock.debug.mockReset();
    loggerMock.warn.mockReset();
    loggerMock.error.mockReset();
  });

  it("uses assistant message_end timestamp as completedAt, not msg.timestamp", async () => {
    // `msg.timestamp` is set by pi providers when constructing the
    // AssistantMessage object — i.e. before the HTTP request — so it sits
    // near turn_start, not at stream completion. The plugin must instead
    // pick up Date.now() from the assistant `message_end` event, which
    // fires immediately after the provider stream's done/error event.
    let capturedSeed: { startedAt: Date } | undefined;
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        capturedSeed = seed as { startedAt: Date };
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    const msg = assistantMessage();
    // Deliberately ancient timestamp; if the plugin still uses
    // msg.timestamp, the assertion below will catch it.
    msg.timestamp = 1700000000000;

    await pi.emit("session_start");
    await pi.emit("turn_start");

    const beforeMessageEnd = Date.now();
    await pi.emit("message_end", { message: { role: "assistant" } });
    const afterMessageEnd = Date.now();

    await pi.emit("turn_end", { message: msg, toolResults: [] });

    expect(recorder.setResult).toHaveBeenCalledTimes(1);
    const result = recorder.setResult.mock.calls[0]![0] as {
      completedAt: Date;
    };
    const completedAt = result.completedAt.getTime();
    expect(completedAt).toBeGreaterThanOrEqual(beforeMessageEnd);
    expect(completedAt).toBeLessThanOrEqual(afterMessageEnd);
    expect(completedAt).not.toBe(msg.timestamp);

    // Sanity: startedAt is from turn_start and predates completedAt, so
    // duration is positive (not the ~0 we got from msg.timestamp before).
    expect(capturedSeed!.startedAt.getTime()).toBeLessThanOrEqual(completedAt);
  });

  it("falls back to msg.timestamp when no assistant message_end is observed", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    const msg = assistantMessage();
    msg.timestamp = 1700000005000;

    await pi.emit("session_start");
    await pi.emit("turn_start");
    // No assistant message_end — simulates extension-stripped events or
    // older pi versions. Plugin should fall back to msg.timestamp.
    await pi.emit("turn_end", { message: msg, toolResults: [] });

    const result = recorder.setResult.mock.calls[0]![0] as {
      completedAt: Date;
    };
    expect(result.completedAt.getTime()).toBe(msg.timestamp);
  });

  it("keeps firstTokenAt within [startedAt, completedAt] when streaming", async () => {
    // Smoke check that the TTFT, startedAt and completedAt timestamps are
    // mutually consistent: with streaming + assistant message_end, TTFT
    // must not exceed the generation duration.
    let capturedSeed: { startedAt: Date } | undefined;
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        capturedSeed = seed as { startedAt: Date };
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("message_update", assistantMessageUpdate());
    await pi.emit("message_end", { message: { role: "assistant" } });
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(recorder.setFirstTokenAt).toHaveBeenCalledTimes(1);
    const firstTokenAt = (
      recorder.setFirstTokenAt.mock.calls[0]![0] as Date
    ).getTime();
    const startedAt = capturedSeed!.startedAt.getTime();
    const completedAt = (
      recorder.setResult.mock.calls[0]![0] as { completedAt: Date }
    ).completedAt.getTime();

    expect(startedAt).toBeLessThanOrEqual(firstTokenAt);
    expect(firstTokenAt).toBeLessThanOrEqual(completedAt);
  });

  it("records streaming generations and first-token time from message_update", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil = {
      startGeneration: vi.fn(),
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    // Pi emits message_update events for each streamed chunk; only the first
    // one should be captured as the time-to-first-token.
    await pi.emit("message_update", assistantMessageUpdate({ delta: "he" }));
    await pi.emit("message_update", assistantMessageUpdate({ delta: "llo" }));
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(sigil.startStreamingGeneration).toHaveBeenCalledTimes(1);
    expect(sigil.startGeneration).not.toHaveBeenCalled();
    expect(recorder.setFirstTokenAt).toHaveBeenCalledTimes(1);
    const firstTokenAt = recorder.setFirstTokenAt.mock.calls[0]![0] as Date;
    expect(firstTokenAt).toBeInstanceOf(Date);
    expect(Number.isNaN(firstTokenAt.getTime())).toBe(false);
  });

  it("does not call setFirstTokenAt when no message_update fires", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(sigil.startStreamingGeneration).toHaveBeenCalledTimes(1);
    expect(recorder.setFirstTokenAt).not.toHaveBeenCalled();
  });

  it("ignores message_update for non-assistant roles", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    // Defensive: pi only emits message_update for assistant streaming, but
    // ignore any other roles regardless to avoid mis-attributing TTFT.
    await pi.emit("message_update", {
      message: { role: "user", content: "hey", timestamp: Date.now() },
      assistantMessageEvent: { type: "text_delta", delta: "x" },
    });
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(recorder.setFirstTokenAt).not.toHaveBeenCalled();
  });

  it("handles the happy path and exports one generation with user input", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "full",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("message_end", {
      message: { role: "user", content: "hey", timestamp: Date.now() },
    });
    await pi.emit("tool_execution_start", {
      toolCallId: "c1",
      toolName: "read",
    });
    await pi.emit("tool_execution_end", { toolCallId: "c1", isError: false });
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(sigil.startStreamingGeneration).toHaveBeenCalledTimes(1);
    expect(recorder.setResult).toHaveBeenCalledTimes(1);
    expect(recorder.setCallError).not.toHaveBeenCalled();

    const result = recorder.setResult.mock.calls[0]![0] as {
      input?: Array<{
        role: string;
        parts?: Array<{ type: string; text?: string }>;
      }>;
    };
    expect(result.input).toBeDefined();
    expect(result.input).toHaveLength(1);
    expect(result.input?.[0]?.role).toBe("user");
    expect(result.input?.[0]?.parts?.[0]).toEqual({
      type: "text",
      text: "hey",
    });
  });

  it("force flushes telemetry after exporting a turn", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };
    const telemetry = {
      tracer: { tracer: true },
      meter: { meter: true },
      forceFlush: vi.fn(async () => {}),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
      otlp: { endpoint: "http://localhost:4318", headers: {} },
    });
    createTelemetryProvidersMock.mockReturnValue(telemetry);
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(createTelemetryProvidersMock).toHaveBeenCalledWith(
      {
        endpoint: "http://localhost:4318",
        headers: {},
      },
      "sess-default-id",
    );
    expect(createAgento11yClientMock).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        tracer: telemetry.tracer,
        meter: telemetry.meter,
      }),
    );
    expect(telemetry.forceFlush).toHaveBeenCalledTimes(1);
  });

  it("does not print telemetry flush failures", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };
    const telemetry = {
      tracer: { tracer: true },
      meter: { meter: true },
      forceFlush: vi.fn(async () => {
        throw new Error("flush timeout");
      }),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
      otlp: { endpoint: "http://localhost:4318", headers: {} },
    });
    createTelemetryProvidersMock.mockReturnValue(telemetry);
    createAgento11yClientMock.mockReturnValue(sigil);

    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });
      await Promise.resolve();

      expect(telemetry.forceFlush).toHaveBeenCalledTimes(1);
      expect(warn).not.toHaveBeenCalled();
      expect(error).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
      error.mockRestore();
    }
  });

  it("leaves input empty on a tool-loop continuation with no user message_end", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "full",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");

    // Turn 1: user types, assistant calls a tool.
    await pi.emit("turn_start");
    await pi.emit("message_end", {
      message: { role: "user", content: "hey", timestamp: Date.now() },
    });
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    // Turn 2: agent loop continues with no new user input.
    await pi.emit("turn_start");
    await pi.emit("turn_end", { message: assistantMessage(), toolResults: [] });

    expect(sigil.startStreamingGeneration).toHaveBeenCalledTimes(2);
    expect(recorder.setResult).toHaveBeenCalledTimes(2);

    const turn1 = recorder.setResult.mock.calls[0]![0] as { input?: unknown[] };
    const turn2 = recorder.setResult.mock.calls[1]![0] as { input?: unknown[] };
    expect(turn1.input).toBeDefined();
    expect(turn1.input).toHaveLength(1);
    expect(turn2.input).toBeUndefined();
  });

  it("clamps startedAt when msg.timestamp precedes turnStartTime", async () => {
    let capturedSeed: any;
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        capturedSeed = seed;
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");

    // Simulate msg.timestamp that is earlier than turnStartTime
    // (can happen with clock adjustments)
    const msg = assistantMessage();
    msg.timestamp = Date.now() - 5000;

    await pi.emit("turn_end", { message: msg, toolResults: [] });

    expect(sigil.startStreamingGeneration).toHaveBeenCalledTimes(1);
    // startedAt must be <= completedAt
    const startedAt = capturedSeed.startedAt.getTime();
    const completedAt = msg.timestamp;
    expect(startedAt).toBeLessThanOrEqual(completedAt);
  });

  it("emits tool execution spans on turn_end", async () => {
    const toolRecorders: ToolRecorderLike[] = [];
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };

    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => {
        const tr: ToolRecorderLike = {
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        };
        toolRecorders.push(tr);
        return tr;
      }),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("tool_execution_start", {
      toolCallId: "c1",
      toolName: "read",
    });
    await pi.emit("tool_execution_end", { toolCallId: "c1", isError: false });
    await pi.emit("tool_execution_start", {
      toolCallId: "c2",
      toolName: "write",
    });
    await pi.emit("tool_execution_end", { toolCallId: "c2", isError: true });

    const msg = assistantMessage();
    (msg as any).content = [
      { type: "toolCall", id: "c1", name: "read", arguments: { path: "a.go" } },
      {
        type: "toolCall",
        id: "c2",
        name: "write",
        arguments: { path: "b.go" },
      },
    ];

    await pi.emit("turn_end", { message: msg, toolResults: [] });

    expect(sigil.startToolExecution).toHaveBeenCalledTimes(2);
    expect(toolRecorders).toHaveLength(2);
    expect(toolRecorders[0]!.end).toHaveBeenCalled();
    expect(toolRecorders[1]!.end).toHaveBeenCalled();
    expect(toolRecorders[1]!.setCallError).toHaveBeenCalled();
  });

  it("swallows sigil failures instead of throwing", async () => {
    const sigil = {
      startStreamingGeneration: vi.fn(async () => {
        throw new Error("transport down");
      }),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");

      await expect(
        pi.emit("turn_end", { message: assistantMessage(), toolResults: [] }),
      ).resolves.toBeUndefined();

      // Never touch the terminal: it would corrupt pi's TUI frame.
      expect(warn).not.toHaveBeenCalled();
      expect(error).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
      error.mockRestore();
    }
  });

  it("logs export failures to the debug log, not the terminal", async () => {
    const sigil = {
      startStreamingGeneration: vi.fn(async () => {
        throw new Error("transport down");
      }),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(loggerMock.debug).toHaveBeenCalledWith(
        "generation export failed",
        expect.any(Error),
      );
      expect(
        loggerMock.debug.mock.calls.map(([message]) => message),
      ).not.toEqual(
        expect.arrayContaining([expect.stringContaining("generation queued")]),
      );
      // The export failure must never reach the terminal.
      expect(error).not.toHaveBeenCalled();
    } finally {
      error.mockRestore();
    }
  });

  it("warns and skips when assistant message shape is invalid", async () => {
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
    };
    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (_seed, run) => {
        await run(recorder);
      }),
      startToolExecution: vi.fn(),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    // Missing required fields (e.g. usage, content) — should fail validation.
    await pi.emit("turn_end", {
      message: { role: "assistant" },
      toolResults: [],
    });

    expect(sigil.startStreamingGeneration).not.toHaveBeenCalled();
    expect(loggerMock.warn).toHaveBeenCalledWith(
      expect.stringContaining("did not validate"),
    );
  });

  it("uses sessionId, not file basename, as conversationId", async () => {
    // Two distinct pi sessions whose session files share a basename
    // (e.g. extensions that spawn child sessions under <root>/run-N/session.jsonl)
    // must emit distinct conversationIds.
    const seeds: Array<{ conversationId?: string }> = [];
    const recorder = { setResult: vi.fn(), setCallError: vi.fn() };
    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        seeds.push(seed as { conversationId?: string });
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    // Session 1: filename basename === "session.jsonl", uuid AAA
    const ctxA = makeCtx({
      sessionFile: "/tmp/runs/run-0/session.jsonl",
      sessionId: "019dd89e-ffad-76ae-9f80-454acd646039",
    });
    await pi.emit("session_start", {}, ctxA);
    await pi.emit("turn_start", {}, ctxA);
    await pi.emit(
      "turn_end",
      {
        message: assistantMessage(),
        toolResults: [],
      },
      ctxA,
    );
    await pi.emit("session_shutdown", {}, ctxA);

    // Session 2: same basename, different uuid BBB
    const ctxB = makeCtx({
      sessionFile: "/tmp/runs/run-1/session.jsonl",
      sessionId: "019de579-98b4-7619-9157-8a6a4f61d487",
    });
    await pi.emit("session_start", {}, ctxB);
    await pi.emit("turn_start", {}, ctxB);
    await pi.emit(
      "turn_end",
      {
        message: assistantMessage(),
        toolResults: [],
      },
      ctxB,
    );

    expect(seeds).toHaveLength(2);
    expect(seeds[0]!.conversationId).toBe(
      "019dd89e-ffad-76ae-9f80-454acd646039",
    );
    expect(seeds[1]!.conversationId).toBe(
      "019de579-98b4-7619-9157-8a6a4f61d487",
    );
    expect(seeds[0]!.conversationId).not.toBe(seeds[1]!.conversationId);
  });

  it("refreshes conversationId per turn when sessionId changes mid-life", async () => {
    // SessionManager reassigns this.sessionId on fork/branch
    // (session-manager.js:927,961). The plugin must observe the current
    // sessionId at every turn_end, not just at session_start.
    const seeds: Array<{ conversationId?: string }> = [];
    const recorder = { setResult: vi.fn(), setCallError: vi.fn() };
    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        seeds.push(seed as { conversationId?: string });
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    let currentId = "id-before-fork";
    const ctx = makeCtx({
      sessionFile: "/tmp/sess/session.jsonl",
      sessionId: () => currentId,
    });

    await pi.emit("session_start", {}, ctx);
    await pi.emit("turn_start", {}, ctx);
    await pi.emit(
      "turn_end",
      {
        message: assistantMessage(),
        toolResults: [],
      },
      ctx,
    );

    // Simulate fork/branch: sessionManager swaps sessionId.
    currentId = "id-after-fork";

    await pi.emit("turn_start", {}, ctx);
    await pi.emit(
      "turn_end",
      {
        message: assistantMessage(),
        toolResults: [],
      },
      ctx,
    );

    expect(seeds).toHaveLength(2);
    expect(seeds[0]!.conversationId).toBe("id-before-fork");
    expect(seeds[1]!.conversationId).toBe("id-after-fork");
  });

  it("yields undefined conversationId when sessionId is empty (no-session mode)", async () => {
    // session-manager.js:430 initializes sessionId to "" before newSession()
    // runs, and --no-session never assigns one. We must not emit a literal
    // empty string as the conversationId.
    let capturedSeed: { conversationId?: string } | undefined;
    const recorder = { setResult: vi.fn(), setCallError: vi.fn() };
    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        capturedSeed = seed as { conversationId?: string };
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    const ctx = makeCtx({ sessionFile: undefined, sessionId: "" });

    await pi.emit("session_start", {}, ctx);
    await pi.emit("turn_start", {}, ctx);
    await pi.emit(
      "turn_end",
      {
        message: assistantMessage(),
        toolResults: [],
      },
      ctx,
    );

    expect(capturedSeed).toBeDefined();
    expect(capturedSeed!.conversationId).toBeUndefined();
  });

  describe("guards (tool_call wiring)", () => {
    function makeRecorder() {
      return {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
    }

    function makeAgento11yLike(
      evaluateHook?: ReturnType<typeof vi.fn>,
    ): Agento11yLike & { evaluateHook?: ReturnType<typeof vi.fn> } {
      const recorder = makeRecorder();
      return {
        startStreamingGeneration: vi.fn(async (_seed, run) => {
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
        ...(evaluateHook ? { evaluateHook } : {}),
      } as Agento11yLike & { evaluateHook?: ReturnType<typeof vi.fn> };
    }

    it("does not call evaluateHook when guards are disabled", async () => {
      const evaluateHook = vi.fn();
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: false, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      const result = await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "ls" } },
        defaultCtx,
      );

      expect(evaluateHook).not.toHaveBeenCalled();
      expect(result).toBeUndefined();
    });

    it("returns undefined (allow) when guards allow the tool call", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      const result = await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "ls" } },
        defaultCtx,
      );

      expect(evaluateHook).toHaveBeenCalledTimes(1);
      expect(result).toBeUndefined();
    });

    it("returns { block, reason } when guards deny the tool call", async () => {
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "deny",
        reason: "blocked rm -rf",
        evaluations: [],
      });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      const result = await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "rm -rf /" } },
        defaultCtx,
      );

      expect(result).toMatchObject({ block: true });
      const reason = (result as unknown as { reason: string }).reason;
      expect(reason).toContain("blocked rm -rf");
      expect(reason).toContain("A Grafana Agent Observability policy");
      expect(reason).toContain('"bash"');
      expect(reason).toContain("Stop and tell the user");
    });

    it("forwards the model cached from the current assistant message_end", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      await fakePi.emit("message_end", { message: assistantMessage() });
      const handler = fakePi.handlers.get("tool_call")!;
      await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "ls" } },
        defaultCtx,
      );

      const req = evaluateHook.mock.calls[0]![0] as {
        context: { model: { provider: string; name: string } };
      };
      expect(req.context.model).toEqual({
        provider: "anthropic",
        name: "claude-sonnet-4",
      });
    });

    it("falls back to unknown model when no assistant message has ended yet", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "ls" } },
        defaultCtx,
      );

      const req = evaluateHook.mock.calls[0]![0] as {
        context: { model: { provider: string; name: string } };
      };
      expect(req.context.model).toEqual({
        provider: "unknown",
        name: "unknown",
      });
    });

    it("clears the cached model on session_shutdown", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      await fakePi.emit("message_end", { message: assistantMessage() });
      await fakePi.emit("session_shutdown");

      // Re-init session and immediately try a tool_call (no assistant message yet).
      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      await handler(
        { toolCallId: "c1", toolName: "bash", input: { command: "ls" } },
        defaultCtx,
      );

      const req = evaluateHook.mock.calls[0]![0] as {
        context: { model: { provider: string; name: string } };
      };
      expect(req.context.model).toEqual({
        provider: "unknown",
        name: "unknown",
      });
    });

    it("applies transformedInput tool args to event.input via in-place mutation", async () => {
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "c1",
                    name: "bash",
                    inputJSON: '{"command":"echo [REDACTED]"}',
                  },
                },
              ],
            },
          ],
        },
      });
      const sigil = makeAgento11yLike(evaluateHook);

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      await fakePi.emit("turn_start");
      const handler = fakePi.handlers.get("tool_call")!;
      const event = {
        toolCallId: "c1",
        toolName: "bash",
        input: { command: "echo sk-real-secret" },
      };
      const result = await handler(event, defaultCtx);

      // Allow the call (no block) but mutate input in place.
      expect(result).toBeUndefined();
      expect(event.input).toEqual({ command: "echo [REDACTED]" });
    });
  });

  describe("guards (context wiring — preflight transform)", () => {
    function makeRecorder() {
      return {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
    }

    function makeAgento11yLike(
      evaluateHook?: ReturnType<typeof vi.fn>,
    ): Agento11yLike & { evaluateHook?: ReturnType<typeof vi.fn> } {
      const recorder = makeRecorder();
      return {
        startStreamingGeneration: vi.fn(async (_seed, run) => {
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
        ...(evaluateHook ? { evaluateHook } : {}),
      } as Agento11yLike & { evaluateHook?: ReturnType<typeof vi.fn> };
    }

    function preflightConfig() {
      return {
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" as const },
        agentName: "pi",
        contentCapture: "metadata_only" as const,
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      };
    }

    it("does not call evaluateHook when guards are disabled", async () => {
      const evaluateHook = vi.fn();
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue({
        ...preflightConfig(),
        guards: { enabled: false, timeoutMs: 1500, failOpen: true },
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const result = await handler(
        {
          messages: [{ role: "user", content: "hello", timestamp: 1 }],
        },
        defaultCtx,
      );

      expect(evaluateHook).not.toHaveBeenCalled();
      expect(result).toBeUndefined();
    });

    it("replaces outgoing messages with redacted text from transformedInput", async () => {
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "allow",
        evaluations: [],
        transformedInput: {
          messages: [
            {
              role: "user",
              parts: [
                {
                  type: "text",
                  text: "my email is [REDACTED_EMAIL]",
                },
              ],
            },
          ],
        },
      });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [
        {
          role: "user",
          content: "my email is leak@example.com",
          timestamp: 1,
        },
      ];
      const result = await handler({ messages: piMessages }, defaultCtx);

      expect(evaluateHook).toHaveBeenCalledTimes(1);
      const [_req, override] = evaluateHook.mock.calls[0]!;
      expect(override).toEqual({
        enabled: true,
        phases: ["preflight"],
      });
      expect(result).toEqual({ messages: piMessages });
      expect(piMessages[0]!.content).toBe("my email is [REDACTED_EMAIL]");
    });

    it("returns undefined when the server emits no transformedInput", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [{ role: "user", content: "hello", timestamp: 1 }];
      const result = await handler({ messages: piMessages }, defaultCtx);
      expect(result).toBeUndefined();
      expect(piMessages[0]!.content).toBe("hello");
    });

    it("fails open (no transform) when evaluateHook throws", async () => {
      const evaluateHook = vi.fn().mockRejectedValue(new Error("timeout"));
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [
        { role: "user", content: "hello secret@example.com", timestamp: 1 },
      ];
      const result = await handler({ messages: piMessages }, defaultCtx);
      expect(result).toBeUndefined();
      expect(piMessages[0]!.content).toBe("hello secret@example.com");
    });

    it("passes through unchanged when redacted message count diverges", async () => {
      // Only one outgoing message but the server returned two. Pi keeps
      // the originals: we refuse to apply a misaligned redaction.
      loggerMock.debug.mockReset();
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "allow",
        evaluations: [],
        transformedInput: {
          messages: [
            { role: "user", parts: [{ type: "text", text: "a" }] },
            { role: "user", parts: [{ type: "text", text: "b" }] },
          ],
        },
      });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [{ role: "user", content: "original", timestamp: 1 }];
      const result = await handler({ messages: piMessages }, defaultCtx);
      expect(result).toBeUndefined();
      expect(piMessages[0]!.content).toBe("original");
      expect(loggerMock.debug).toHaveBeenCalledWith(
        expect.stringContaining("preflight transform dropped"),
      );
    });

    it("keeps thinking parts on assistant messages untouched during redaction", async () => {
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "allow",
        evaluations: [],
        transformedInput: {
          messages: [
            { role: "user", parts: [{ type: "text", text: "hi [REDACTED]" }] },
            {
              role: "assistant",
              parts: [{ type: "text", text: "answer" }],
            },
          ],
        },
      });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [
        { role: "user", content: "hi secret@example.com", timestamp: 1 },
        {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "opaque-sig" },
            { type: "text", text: "original" },
          ],
          provider: "anthropic",
          model: "claude-sonnet-4",
          usage: {
            input: 0,
            output: 0,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 0,
          },
          stopReason: "stop",
          timestamp: 2,
        },
      ];
      await handler({ messages: piMessages }, defaultCtx);

      // User text overwritten.
      expect(piMessages[0]!.content).toBe("hi [REDACTED]");
      // Thinking part preserved unchanged on the assistant message.
      const asst = piMessages[1] as unknown as {
        content: Array<{ type: string; text?: string; thinking?: string }>;
      };
      expect(asst.content[0]).toEqual({
        type: "thinking",
        thinking: "opaque-sig",
      });
      expect(asst.content[1]).toMatchObject({
        type: "text",
        text: "answer",
      });
    });

    it("sends provider/name = unknown on the first preflight (no assistant turn yet)", async () => {
      const evaluateHook = vi
        .fn()
        .mockResolvedValue({ action: "allow", evaluations: [] });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      await handler(
        {
          messages: [{ role: "user", content: "hello", timestamp: 1 }],
        },
        defaultCtx,
      );
      const req = evaluateHook.mock.calls[0]![0] as {
        context: { model: { provider: string; name: string } };
      };
      expect(req.context.model).toEqual({
        provider: "unknown",
        name: "unknown",
      });
    });

    it("applies redacted tool-result content from the server's tool_result part", async () => {
      // Regression for the bug where the plugin only walked text parts and
      // silently dropped the server's redacted `tool_result.content`. The
      // server transforms ToolResult.Content in place and returns it on the
      // same tool_result part, not as a synthetic text part.
      const evaluateHook = vi.fn().mockResolvedValue({
        action: "allow",
        evaluations: [],
        transformedInput: {
          messages: [
            { role: "user", parts: [{ type: "text", text: "run it" }] },
            {
              role: "tool",
              parts: [
                {
                  type: "tool_result",
                  toolResult: {
                    toolCallId: "c1",
                    name: "bash",
                    content: "token=[REDACTED_API_KEY]",
                    isError: false,
                  },
                },
              ],
            },
          ],
        },
      });
      const sigil = makeAgento11yLike(evaluateHook);
      loadConfigMock.mockResolvedValue(preflightConfig());
      createAgento11yClientMock.mockReturnValue(sigil);

      const fakePi = new FakePi();
      registerExtension(fakePi as any);

      await fakePi.emit("session_start");
      const handler = fakePi.handlers.get("context")!;
      const piMessages = [
        { role: "user", content: "run it", timestamp: 1 },
        {
          role: "toolResult",
          toolCallId: "c1",
          toolName: "bash",
          content: [{ type: "text", text: "token=sk-LEAKED" }],
          isError: false,
          timestamp: 2,
        },
      ];
      const result = await handler({ messages: piMessages }, defaultCtx);
      expect(result).toEqual({ messages: piMessages });
      const tr = piMessages[1] as unknown as {
        content: Array<{ type: string; text?: string }>;
      };
      expect(tr.content[0]).toMatchObject({
        type: "text",
        text: "token=[REDACTED_API_KEY]",
      });
    });
  });

  it("emits git.branch and cwd tags regardless of content capture mode", async () => {
    // git.branch + cwd are low-cardinality session metadata, not message
    // content; they ship in every content-capture mode (matches
    // claude-code/cursor).
    resolveGitBranchMock.mockReturnValue("feature-x");

    for (const mode of [
      "full",
      "metadata_only",
      "no_tool_content",
      "full_with_metadata_spans",
    ] as const) {
      resolveGitBranchMock.mockClear();

      let capturedSeed: { tags?: Record<string, string> } | undefined;
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          capturedSeed = seed as { tags?: Record<string, string> };
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };

      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: mode,
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(resolveGitBranchMock, `mode=${mode}`).toHaveBeenCalledTimes(1);
      expect(capturedSeed!.tags, `mode=${mode}`).toEqual({
        "git.branch": "feature-x",
        cwd: process.cwd(),
      });
    }
  });

  it("emits cwd tag without git.branch when not in a git repo", async () => {
    resolveGitBranchMock.mockReturnValue(undefined);

    let capturedSeed: { tags?: Record<string, string> } | undefined;
    const recorder = {
      setResult: vi.fn(),
      setCallError: vi.fn(),
      setFirstTokenAt: vi.fn(),
    };
    const sigil: Agento11yLike = {
      startStreamingGeneration: vi.fn(async (seed, run) => {
        capturedSeed = seed as { tags?: Record<string, string> };
        await run(recorder);
      }),
      startToolExecution: vi.fn(() => ({
        setResult: vi.fn(),
        setCallError: vi.fn(),
        end: vi.fn(),
        getError: vi.fn(),
      })),
      shutdown: vi.fn(async () => {}),
    };

    loadConfigMock.mockResolvedValue({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "none" },
      agentName: "pi",
      contentCapture: "full",
    });
    createAgento11yClientMock.mockReturnValue(sigil);

    const pi = new FakePi();
    registerExtension(pi as any);

    await pi.emit("session_start");
    await pi.emit("turn_start");
    await pi.emit("turn_end", {
      message: assistantMessage(),
      toolResults: [],
    });

    expect(resolveGitBranchMock).toHaveBeenCalledTimes(1);
    expect(capturedSeed!.tags).toEqual({ cwd: process.cwd() });
  });

  describe("systemPrompt capture", () => {
    function setupClient() {
      const seeds: Array<{ systemPrompt?: string }> = [];
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as { systemPrompt?: string });
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };
      return { sigil, seeds, recorder };
    }

    it("attaches systemPrompt to every turn_end during the agent loop under full mode", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("before_agent_start", {
        systemPrompt: "You are a helpful agent.",
      });
      // Turn 1
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });
      // Turn 2 (tool-loop continuation)
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.systemPrompt).toBe("You are a helpful agent.");
      expect(seeds[1]!.systemPrompt).toBe("You are a helpful agent.");
    });

    it("caches the prompt under no_tool_content but strips it under metadata_only", async () => {
      for (const mode of ["no_tool_content", "metadata_only"] as const) {
        const { sigil, seeds } = setupClient();
        loadConfigMock.mockResolvedValue({
          endpoint: "http://localhost:8080/api/v1/generations:export",
          auth: { mode: "none" },
          agentName: "pi",
          contentCapture: mode,
        });
        createAgento11yClientMock.mockReturnValue(sigil);

        const pi = new FakePi();
        registerExtension(pi as any);

        await pi.emit("session_start");
        await pi.emit("before_agent_start", {
          systemPrompt: "You are a helpful agent.",
        });
        await pi.emit("turn_start");
        await pi.emit("turn_end", {
          message: assistantMessage(),
          toolResults: [],
        });

        if (mode === "no_tool_content") {
          expect(seeds[0]!.systemPrompt, `mode=${mode}`).toBe(
            "You are a helpful agent.",
          );
        } else {
          expect(seeds[0]!.systemPrompt, `mode=${mode}`).toBeUndefined();
        }
      }
    });

    it("clears the cached prompt on agent_end so the next loop starts empty", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("before_agent_start", { systemPrompt: "first prompt" });
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });
      await pi.emit("agent_end", { messages: [] });

      // No new before_agent_start — second loop must not reuse the prior prompt.
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.systemPrompt).toBe("first prompt");
      expect(seeds[1]!.systemPrompt).toBeUndefined();
    });

    it("clears the cached prompt on session_shutdown", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("before_agent_start", { systemPrompt: "first prompt" });
      await pi.emit("session_shutdown");

      // Fresh session, no new before_agent_start.
      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.systemPrompt).toBeUndefined();
    });
  });

  describe("tool catalog capture", () => {
    function setupClient() {
      const seeds: Array<{ tools?: unknown[] }> = [];
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as { tools?: unknown[] });
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };
      return { sigil, seeds };
    }

    const bashTool = {
      name: "bash",
      description: "Run a shell command",
      parameters: {
        type: "object",
        properties: { command: { type: "string" } },
      },
    };
    const readTool = {
      name: "read",
      description: "Read a file",
      parameters: {
        type: "object",
        properties: { path: { type: "string" } },
      },
    };

    it("emits description and inputSchemaJSON for active tools under full mode", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      pi.getAllTools = () => [bashTool, readTool];
      pi.getActiveTools = () => ["bash", "read"];
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      const tools = seeds[0]!.tools as Array<{
        name: string;
        description?: string;
        inputSchemaJSON?: string;
      }>;
      expect(tools).toHaveLength(2);
      expect(tools[0]).toEqual({
        name: "bash",
        description: "Run a shell command",
        inputSchemaJSON: JSON.stringify(bashTool.parameters),
      });
      expect(tools[1]?.inputSchemaJSON).toBe(
        JSON.stringify(readTool.parameters),
      );
    });

    it("strips description and inputSchemaJSON under metadata_only and no_tool_content", async () => {
      for (const mode of ["metadata_only", "no_tool_content"] as const) {
        const { sigil, seeds } = setupClient();
        loadConfigMock.mockResolvedValue({
          endpoint: "http://localhost:8080/api/v1/generations:export",
          auth: { mode: "none" },
          agentName: "pi",
          contentCapture: mode,
        });
        createAgento11yClientMock.mockReturnValue(sigil);

        const pi = new FakePi();
        pi.getAllTools = () => [bashTool, readTool];
        pi.getActiveTools = () => ["bash", "read"];
        registerExtension(pi as any);

        await pi.emit("session_start");
        await pi.emit("turn_start");
        await pi.emit("turn_end", {
          message: assistantMessage(),
          toolResults: [],
        });

        const tools = seeds[0]!.tools as Array<{
          name: string;
          description?: string;
          inputSchemaJSON?: string;
        }>;
        expect(tools, `mode=${mode}`).toEqual([
          { name: "bash" },
          { name: "read" },
        ]);
      }
    });

    it("emits the offered (active) catalog even when no tool is called this turn", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      pi.getAllTools = () => [bashTool, readTool];
      pi.getActiveTools = () => ["bash", "read"];
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      // No tool_execution_* events — model offered tools but didn't call any.
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      const tools = seeds[0]!.tools as Array<{ name: string }>;
      expect(tools.map((t) => t.name)).toEqual(["bash", "read"]);
    });

    it("degrades to empty tools without crashing when getAllTools throws", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      pi.getAllTools = () => {
        throw new Error("registry unavailable");
      };
      pi.getActiveTools = () => [];
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.tools).toBeUndefined();
    });

    it("synthesizes name-only tools from getActiveTools when getAllTools throws", async () => {
      // Registry lookup fails (signature drift, transient error) but the
      // active-set API still reports the names pi offered the model.
      // Without the fallback, mapTools would filter an empty catalog and
      // the seed would silently omit the tool list.
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "full",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      pi.getAllTools = () => {
        throw new Error("registry unavailable");
      };
      pi.getActiveTools = () => ["bash", "read"];
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(1);
      const tools = seeds[0]!.tools as Array<{
        name: string;
        description?: string;
        inputSchemaJSON?: string;
      }>;
      expect(tools.map((t) => t.name).sort()).toEqual(["bash", "read"]);
      // Catalog was unavailable, so we have no description/schema to emit.
      for (const t of tools) {
        expect(t.description).toBeUndefined();
        expect(t.inputSchemaJSON).toBeUndefined();
      }
    });

    it("falls back to called tool names when getActiveTools is unavailable", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      // Neither hook present — emulates older pi versions.
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("tool_execution_start", {
        toolCallId: "c1",
        toolName: "bash",
      });
      await pi.emit("tool_execution_end", { toolCallId: "c1", isError: false });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      const tools = seeds[0]!.tools as Array<{ name: string }>;
      expect(tools).toEqual([{ name: "bash" }]);
    });

    it("emits no tools when getActiveTools explicitly returns []", async () => {
      // The registry is populated but the user disabled every tool via
      // setActiveTools([]); the seed must NOT report the full registry.
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      pi.getAllTools = () => [bashTool, readTool];
      pi.getActiveTools = () => [];
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.tools).toBeUndefined();
    });
  });

  describe("request controls capture", () => {
    function setupClient() {
      const seeds: Array<Record<string, unknown>> = [];
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as Record<string, unknown>);
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };
      return { sigil, seeds };
    }

    it("populates the seed for the matching turn_end", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: {
          max_tokens: 4096,
          temperature: 0.2,
          top_p: 0.9,
          tool_choice: { type: "auto" },
          thinking: { type: "enabled", budget_tokens: 2048 },
        },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      const seed = seeds[0]!;
      expect(seed.maxTokens).toBe(4096);
      expect(seed.temperature).toBe(0.2);
      expect(seed.topP).toBe(0.9);
      expect(seed.toolChoice).toBe("auto");
      expect(seed.metadata).toEqual({
        "agento11y.gen_ai.request.thinking.budget_tokens": 2048,
      });
    });

    it("clears between turns so an empty turn 2 does not inherit turn 1's values", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { max_tokens: 1024, temperature: 0.5 },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      // Turn 2: no before_provider_request fires — values must clear.
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.maxTokens).toBe(1024);
      expect(seeds[0]!.temperature).toBe(0.5);
      expect(seeds[1]!.maxTokens).toBeUndefined();
      expect(seeds[1]!.temperature).toBeUndefined();
    });

    it("clears on agent_end so the next agent loop does not inherit controls", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("before_agent_start", { systemPrompt: "sp" });
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { max_tokens: 1024, temperature: 0.5 },
      });
      // Agent loop ends without a matching turn_end — turn_end's finally
      // never runs, so agent_end is the last line of defense against stale
      // request controls leaking into the next agent loop.
      await pi.emit("agent_end", { messages: [] });

      // Next agent loop: no before_provider_request fires before turn_end.
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.maxTokens).toBeUndefined();
      expect(seeds[0]!.temperature).toBeUndefined();
    });

    it("refreshes per turn when consecutive before_provider_request events fire", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { max_tokens: 1024, temperature: 0.5, top_p: 0.8 },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      // Tool-loop continuation: a new request payload with fewer fields.
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { max_tokens: 2048 },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.maxTokens).toBe(1024);
      expect(seeds[0]!.topP).toBe(0.8);
      expect(seeds[1]!.maxTokens).toBe(2048);
      expect(seeds[1]!.temperature).toBeUndefined();
      expect(seeds[1]!.topP).toBeUndefined();
    });

    it("emits controls under metadata_only too", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { temperature: 0.1, max_tokens: 256 },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.temperature).toBe(0.1);
      expect(seeds[0]!.maxTokens).toBe(256);
    });

    it("extracts controls from Gemini-shaped payloads", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: {
          model: "gemini-2.0-flash",
          contents: [],
          config: {
            temperature: 0.4,
            topP: 0.95,
            maxOutputTokens: 8192,
            toolConfig: { functionCallingConfig: { mode: "ANY" } },
            thinkingConfig: { thinkingBudget: 2048 },
          },
        },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.maxTokens).toBe(8192);
      expect(seeds[0]!.temperature).toBe(0.4);
      expect(seeds[0]!.topP).toBe(0.95);
      expect(seeds[0]!.toolChoice).toBe("ANY");
      expect(seeds[0]!.metadata).toEqual({
        "agento11y.gen_ai.request.thinking.budget_tokens": 2048,
      });
    });

    it("preserves the forced tool name in Anthropic tool_choice", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("before_provider_request", {
        payload: { tool_choice: { type: "tool", name: "search" } },
      });
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds[0]!.toolChoice).toBe("tool:search");
    });
  });

  describe("generation lineage", () => {
    function setupClient() {
      const seeds: Array<{
        id?: string;
        parentGenerationIds?: string[];
      }> = [];
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as { id?: string; parentGenerationIds?: string[] });
          await run(recorder);
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };
      return { sigil, seeds };
    }

    // ctxWithBranch is a fake ReadonlySessionManager that returns a static
    // session branch. We track which assistant entry each turn_end should
    // hit by swapping a shared `currentMessage` reference between turns,
    // mirroring how the real pi runtime appends entries to the tree.
    function ctxWithBranch(
      sessionId: string,
      branch: Array<{
        type: string;
        id: string;
        parentId: string | null;
        message?: { role: string } | null;
      }>,
    ) {
      return {
        sessionManager: {
          getSessionFile: () => "pi-session.jsonl",
          getSessionId: () => sessionId,
          getBranch: () => branch,
        },
      };
    }

    it("emits deterministic pi-* generation id when branch data is available", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const msg = assistantMessage();
      const ctx = ctxWithBranch("pi-conv-1", [
        {
          type: "message",
          id: "u1",
          parentId: null,
          message: { role: "user" },
        },
        {
          type: "message",
          id: "a1",
          parentId: "u1",
          message: msg,
        },
      ]);

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toMatch(/^pi-[a-f0-9]{24}$/);
      // First assistant turn — no parent.
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
    });

    it("is stable across re-exports of the same conversationId + session entry", async () => {
      // Re-running the export pipeline against the same session state
      // (same conversationId, same assistant entry id) must produce the
      // same generation id. This is what makes the dependency graph robust
      // to retries.
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const msg = assistantMessage();
      const branch = [
        {
          type: "message",
          id: "u1",
          parentId: null,
          message: { role: "user" },
        },
        {
          type: "message",
          id: "a1",
          parentId: "u1",
          message: msg,
        },
      ];

      const piA = new FakePi();
      registerExtension(piA as any);
      const ctxA = ctxWithBranch("pi-conv-1", branch);
      await piA.emit("session_start", {}, ctxA);
      await piA.emit("turn_start", {}, ctxA);
      await piA.emit("turn_end", { message: msg, toolResults: [] }, ctxA);

      const piB = new FakePi();
      registerExtension(piB as any);
      const ctxB = ctxWithBranch("pi-conv-1", branch);
      await piB.emit("session_start", {}, ctxB);
      await piB.emit("turn_start", {}, ctxB);
      await piB.emit("turn_end", { message: msg, toolResults: [] }, ctxB);

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.id).toBe(seeds[1]!.id);
    });

    it("links a second assistant turn to the first one on the same branch", async () => {
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const msg1 = assistantMessage();
      const msg2 = assistantMessage();
      // Branch grows as turns proceed: turn 1 sees only [u1, a1]; turn 2
      // sees [u1, a1, u2, a2]. Encode this with a closure-backed branch.
      let branch: Array<{
        type: string;
        id: string;
        parentId: string | null;
        message?: { role: string } | null;
      }> = [
        {
          type: "message",
          id: "u1",
          parentId: null,
          message: { role: "user" },
        },
        { type: "message", id: "a1", parentId: "u1", message: msg1 },
      ];
      const ctx = {
        sessionManager: {
          getSessionFile: () => "pi-session.jsonl",
          getSessionId: () => "pi-conv-2",
          getBranch: () => branch,
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg1, toolResults: [] }, ctx);

      // Pi appends the user message and assistant response to the tree as
      // turn 2 progresses.
      branch = [
        ...branch,
        {
          type: "message",
          id: "u2",
          parentId: "a1",
          message: { role: "user" },
        },
        { type: "message", id: "a2", parentId: "u2", message: msg2 },
      ];
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg2, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.id).toMatch(/^pi-[a-f0-9]{24}$/);
      expect(seeds[1]!.id).toMatch(/^pi-[a-f0-9]{24}$/);
      expect(seeds[0]!.id).not.toBe(seeds[1]!.id);

      // Turn 1 is the first assistant on the branch — no parent.
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      // Turn 2 points back to turn 1.
      expect(seeds[1]!.parentGenerationIds).toEqual([seeds[0]!.id]);
    });

    it("omits lineage fields when getBranch is unavailable (older pi)", async () => {
      // Older pi runtimes do not expose getBranch on ReadonlySessionManager;
      // the plugin must not set id or parentGenerationIds so the SDK keeps
      // its random `gen-*` fallback behavior.
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const pi = new FakePi();
      registerExtension(pi as any);
      // defaultCtx has no getBranch.
      await pi.emit("session_start");
      await pi.emit("turn_start");
      await pi.emit("turn_end", {
        message: assistantMessage(),
        toolResults: [],
      });

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBeUndefined();
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
    });

    it("omits lineage when conversationId is empty", async () => {
      // No conversationId (no-session mode): we cannot hash a stable id.
      const { sigil, seeds } = setupClient();
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        contentCapture: "metadata_only",
      });
      createAgento11yClientMock.mockReturnValue(sigil);

      const msg = assistantMessage();
      const ctx = ctxWithBranch("", [
        { type: "message", id: "a1", parentId: null, message: msg },
      ]);

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.id).toBeUndefined();
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
    });
  });
});

// --- Unit tests for emitToolSpans ---

function makePiMsg(
  overrides?: Partial<PiAssistantMessage>,
): PiAssistantMessage {
  return {
    role: "assistant",
    content: [{ type: "text", text: "Hello" }],
    provider: "anthropic",
    model: "claude-sonnet-4-20250514",
    usage: {
      input: 100,
      output: 50,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 150,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
    stopReason: "toolUse",
    timestamp: 1700000001000,
    ...overrides,
  };
}

function makePiTiming(overrides?: Partial<ToolTiming>): ToolTiming {
  return {
    toolCallId: "call-1",
    toolName: "bash",
    startedAt: 1700000000500,
    completedAt: 1700000001500,
    isError: false,
    ...overrides,
  };
}

function makePiToolResult(overrides?: Partial<PiToolResult>): PiToolResult {
  return {
    role: "toolResult",
    toolCallId: "call-1",
    toolName: "bash",
    content: [{ type: "text", text: "output" }],
    isError: false,
    timestamp: 1700000002000,
    ...overrides,
  };
}

function mockAgento11yClient() {
  const recorders: Array<{
    start: Record<string, unknown>;
    result: Record<string, unknown> | undefined;
    callError: unknown;
    ended: boolean;
  }> = [];

  const client = {
    startToolExecution: vi.fn((start: Record<string, unknown>) => {
      const rec = {
        start,
        result: undefined as Record<string, unknown> | undefined,
        callError: undefined as unknown,
        ended: false,
      };
      recorders.push(rec);
      return {
        setResult: vi.fn((r: Record<string, unknown>) => {
          rec.result = r;
        }),
        setCallError: vi.fn((e: unknown) => {
          rec.callError = e;
        }),
        end: vi.fn(() => {
          rec.ended = true;
        }),
        getError: vi.fn(() => undefined),
      };
    }),
  } as unknown as Agento11yClient;

  return { client, recorders };
}

describe("emitToolSpans", () => {
  it("does nothing when no timings", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, makePiMsg(), [], [], {
      agentName: "pi",
      contentCapture: "metadata_only",
    });
    expect(recorders).toHaveLength(0);
  });

  it("creates a span per tool timing", () => {
    const { client, recorders } = mockAgento11yClient();
    const msg = makePiMsg({
      content: [
        { type: "toolCall", id: "c1", name: "bash", arguments: { cmd: "ls" } },
        {
          type: "toolCall",
          id: "c2",
          name: "read",
          arguments: { path: "a.go" },
        },
      ],
    });

    emitToolSpans(
      client,
      msg,
      [],
      [
        makePiTiming({ toolCallId: "c1", toolName: "bash" }),
        makePiTiming({ toolCallId: "c2", toolName: "read" }),
      ],
      { agentName: "pi", contentCapture: "metadata_only" },
    );

    expect(recorders).toHaveLength(2);
    expect(recorders[0]!.start).toMatchObject({
      toolName: "bash",
      toolCallId: "c1",
      toolType: "function",
    });
    expect(recorders[1]!.start).toMatchObject({
      toolName: "read",
      toolCallId: "c2",
      toolType: "function",
    });
    expect(recorders.every((r) => r.ended)).toBe(true);
  });

  it("passes model and agent context", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      makePiMsg(),
      [],
      [makePiTiming({ toolCallId: "c1" })],
      {
        conversationId: "conv-42",
        agentName: "pi",
        agentVersion: "2.0.0",
        contentCapture: "metadata_only",
      },
    );

    expect(recorders[0]!.start).toMatchObject({
      conversationId: "conv-42",
      agentName: "pi",
      agentVersion: "2.0.0",
      requestModel: "claude-sonnet-4-20250514",
      requestProvider: "anthropic",
    });
  });

  it("includes arguments and results with content capture", () => {
    const { client, recorders } = mockAgento11yClient();
    const msg = makePiMsg({
      content: [
        { type: "toolCall", id: "c1", name: "bash", arguments: { cmd: "ls" } },
      ],
    });
    const toolResults = [
      makePiToolResult({
        toolCallId: "c1",
        content: [{ type: "text", text: "file.txt" }],
      }),
    ];

    emitToolSpans(
      client,
      msg,
      toolResults,
      [makePiTiming({ toolCallId: "c1" })],
      {
        agentName: "pi",
        contentCapture: "full",
      },
    );

    expect(recorders[0]!.result?.arguments).toBe('{"cmd":"ls"}');
    expect(recorders[0]!.result?.result).toBe("file.txt");
  });

  it("omits content when contentCapture is off", () => {
    const { client, recorders } = mockAgento11yClient();
    const msg = makePiMsg({
      content: [
        { type: "toolCall", id: "c1", name: "bash", arguments: { cmd: "ls" } },
      ],
    });

    emitToolSpans(
      client,
      msg,
      [makePiToolResult({ toolCallId: "c1" })],
      [makePiTiming({ toolCallId: "c1" })],
      {
        agentName: "pi",
        contentCapture: "metadata_only",
      },
    );

    expect(recorders[0]!.result?.arguments).toBeUndefined();
    expect(recorders[0]!.result?.result).toBeUndefined();
  });

  it("marks error tool executions", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      makePiMsg(),
      [],
      [makePiTiming({ toolCallId: "c1", isError: true })],
      { agentName: "pi", contentCapture: "metadata_only" },
    );

    expect(recorders[0]!.callError).toBeInstanceOf(Error);
  });

  it("uses real start/end times", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      makePiMsg(),
      [],
      [makePiTiming({ startedAt: 1000, completedAt: 5000 })],
      { agentName: "pi", contentCapture: "metadata_only" },
    );

    expect(recorders[0]!.start).toMatchObject({ startedAt: new Date(1000) });
    expect(recorders[0]!.result?.completedAt).toEqual(new Date(5000));
  });
});
