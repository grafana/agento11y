import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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
import { stablePiGenerationId } from "./lineage.js";
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
  // Host summarization calls (compaction, branch summary) go through the
  // synchronous path. Optional, because the fakes in turn-path tests leave it
  // off. Any test that expects a summary export needs it: FakePi.emit
  // swallows a missing handler and the plugin's handlers swallow throws into
  // logger.error, so a fake without startGeneration makes a broken export
  // look like a passing test.
  startGeneration?: (
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

// Pi's live model, as exposed on ctx.model. `id` (not `name`) is what
// assistant messages carry as their model, so the summary export path reads
// `id`.
const defaultModel = { provider: "anthropic", id: "claude-sonnet-4" };

const defaultCtx = {
  sessionManager: {
    getSessionFile: () => "session-1",
    getSessionId: () => "sess-default-id",
  },
  model: defaultModel,
};

function makeCtx({
  sessionFile,
  sessionId,
  model = defaultModel,
}: {
  sessionFile?: string | (() => string | undefined);
  sessionId: string | (() => string);
  model?: { provider: string; id: string };
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
    model,
  };
}

// A fake session entry as returned by ReadonlySessionManager.getBranch().
// Loose on purpose: message entries carry `message`, compaction and
// branch_summary entries carry summary/usage/timestamp instead.
interface FakeBranchEntry {
  type: string;
  id: string;
  parentId: string | null;
  message?: unknown;
  timestamp?: string;
  summary?: string;
  tokensBefore?: number;
  usage?: unknown;
  fromHook?: boolean;
}

// ctxWithBranch is a fake ReadonlySessionManager that returns a static
// session branch. Turn tests track which assistant entry each turn_end
// should hit by swapping a shared `currentMessage` reference between turns,
// mirroring how the real pi runtime appends entries to the tree. Pass
// `{ model: null }` to simulate a session with no resolvable model.
function ctxWithBranch(
  sessionId: string,
  branch: FakeBranchEntry[] | (() => FakeBranchEntry[]),
  opts?: { model?: { provider: string; id: string } | null },
) {
  const model = opts ? (opts.model ?? undefined) : defaultModel;
  const branchFn = typeof branch === "function" ? branch : () => branch;
  return {
    sessionManager: {
      getSessionFile: () => "pi-session.jsonl",
      getSessionId: () => sessionId,
      getBranch: branchFn,
    },
    model,
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

  // --- Host summarization calls (compaction, branch summary) ---
  //
  // These run outside pi's agent loop: no turn_*, message_*, or provider
  // events fire, so the only signals are session_before_compact /
  // session_compact and session_before_tree / session_tree.
  describe("host summarization export", () => {
    interface CapturedSeed {
      id?: string;
      conversationId?: string;
      conversationTitle?: string;
      agentName?: string;
      agentVersion?: string;
      operationName?: string;
      model?: { provider: string; name: string };
      startedAt?: Date;
      tags?: Record<string, string>;
      parentGenerationIds?: string[];
    }
    interface CapturedResult {
      usage?: Record<string, number>;
      metadata?: Record<string, unknown>;
      output?: Array<{ role: string; parts: Array<Record<string, unknown>> }>;
      completedAt?: Date;
      stopReason?: string;
      responseModel?: string;
    }

    function setupClient(opts?: { startGenerationError?: Error }) {
      const seeds: CapturedSeed[] = [];
      const results: CapturedResult[] = [];
      const turnSeeds: Array<Record<string, unknown>> = [];
      const turnResults: Array<Record<string, unknown>> = [];
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          turnSeeds.push(seed as Record<string, unknown>);
          await run({
            setResult: (value: unknown) => {
              turnResults.push(value as Record<string, unknown>);
            },
            setCallError: vi.fn(),
            setFirstTokenAt: vi.fn(),
          });
        }),
        startGeneration: vi.fn(async (seed, run) => {
          if (opts?.startGenerationError) throw opts.startGenerationError;
          seeds.push(seed as CapturedSeed);
          await run({
            setResult: (value: unknown) => {
              results.push(value as CapturedResult);
            },
            setCallError: vi.fn(),
          });
        }),
        startToolExecution: vi.fn(() => ({
          setResult: vi.fn(),
          setCallError: vi.fn(),
          end: vi.fn(),
          getError: vi.fn(),
        })),
        shutdown: vi.fn(async () => {}),
      };
      return { sigil, seeds, results, turnSeeds, turnResults };
    }

    function useConfig(
      sigil: Agento11yLike,
      contentCapture:
        | "metadata_only"
        | "full"
        | "no_tool_content"
        | "full_with_metadata_spans" = "full",
    ) {
      loadConfigMock.mockResolvedValue({
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        agentName: "pi",
        agentVersion: "0.82.1",
        contentCapture,
      });
      createAgento11yClientMock.mockReturnValue(sigil);
    }

    const COMPACTION_TS = "2025-01-01T00:00:05.000Z";
    const COMPACTION_AT = Date.parse(COMPACTION_TS);

    function compactionEntry(
      overrides?: Partial<FakeBranchEntry>,
    ): FakeBranchEntry {
      return {
        type: "compaction",
        id: "c1",
        parentId: "a1",
        timestamp: COMPACTION_TS,
        summary: "Earlier: we split the exporter and fixed two tests.",
        tokensBefore: 152000,
        usage: {
          input: 120000,
          output: 8000,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 128000,
          cost: { total: 2.5 },
        },
        ...overrides,
      };
    }

    // u1 -> a1 -> <summary entry>: the shape pi produces when compaction runs
    // right after an assistant turn.
    function branchWith(...tail: FakeBranchEntry[]): FakeBranchEntry[] {
      return [
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
          message: { role: "assistant" },
        },
        ...tail,
      ];
    }

    it("exports one generation for a threshold auto-compaction", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_compact", { reason: "threshold" }, ctx);
      await pi.emit(
        "session_compact",
        {
          compactionEntry: entry,
          fromExtension: false,
          reason: "threshold",
          willRetry: false,
        },
        ctx,
      );

      expect(sigil.startGeneration).toHaveBeenCalledTimes(1);
      // Not the streaming path: there is no token stream to time.
      expect(sigil.startStreamingGeneration).not.toHaveBeenCalled();
      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toMatch(/^pi-[a-f0-9]{24}$/);
      expect(seeds[0]!.id).toBe(stablePiGenerationId("pi-conv-1", "c1"));
      expect(seeds[0]!.tags?.["pi.call_kind"]).toBe("compaction");
      expect(seeds[0]!.conversationId).toBe("pi-conv-1");
      expect(seeds[0]!.agentName).toBe("pi");
      expect(seeds[0]!.model).toEqual({
        provider: "anthropic",
        name: "claude-sonnet-4",
      });
      // No explicit operationName: the SYNC path defaults it to generateText.
      expect(seeds[0]!.operationName).toBeUndefined();
      // Parent is the assistant turn that preceded the compaction.
      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId("pi-conv-1", "a1"),
      ]);
      expect(results[0]!.usage).toEqual({
        inputTokens: 120000,
        outputTokens: 8000,
        totalTokens: 128000,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      });
      expect(results[0]!.stopReason).toBe("end_turn");
      expect(results[0]!.completedAt).toEqual(new Date(COMPACTION_AT));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    // willRetry is only true on overflow recovery, so the reason and the retry
    // flag travel together.
    it.each([
      { reason: "manual", willRetry: false },
      { reason: "threshold", willRetry: false },
      { reason: "overflow", willRetry: true },
    ])("exports metadata for a $reason compaction", async (event) => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_compact", { reason: event.reason }, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false, ...event },
        ctx,
      );

      expect(sigil.startGeneration).toHaveBeenCalledTimes(1);
      expect(seeds[0]!.tags?.["pi.call_kind"]).toBe("compaction");
      expect(seeds[0]!.operationName).toBeUndefined();
      expect(results[0]!.metadata).toEqual({
        cost_usd: 2.5,
        "pi.tokens_before": 152000,
        "pi.compaction.reason": event.reason,
        "pi.compaction.will_retry": event.willRetry,
      });
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("records a zero cost, matching the turn path", async () => {
      const { sigil, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({
        usage: {
          input: 10,
          output: 2,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 12,
          cost: { total: 0 },
        },
      });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(results[0]!.metadata?.cost_usd).toBe(0);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("skips extension-supplied compaction (fromExtension)", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({ fromHook: true });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_compact", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: true },
        ctx,
      );

      expect(sigil.startGeneration).not.toHaveBeenCalled();
      expect(
        loggerMock.debug.mock.calls.map(([m]) => String(m)),
      ).toContainEqual(expect.stringContaining("supplied by an extension"));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("skips a compaction entry marked fromHook even when fromExtension is false", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({ fromHook: true });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(sigil.startGeneration).not.toHaveBeenCalled();
      expect(
        loggerMock.debug.mock.calls.map(([m]) => String(m)),
      ).toContainEqual(expect.stringContaining("came from an extension"));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("exports nothing for plain tree navigation without a summary", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const ctx = ctxWithBranch("pi-conv-1", branchWith());

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_tree", {}, ctx);
      await pi.emit(
        "session_tree",
        { newLeafId: "a1", oldLeafId: "a1", summaryEntry: undefined },
        ctx,
      );

      expect(sigil.startGeneration).not.toHaveBeenCalled();
      expect(loggerMock.error).not.toHaveBeenCalled();
      // Plain navigation is the common case and stays out of the log.
      expect(
        loggerMock.debug.mock.calls.map(([m]) => String(m)),
      ).not.toContainEqual(expect.stringContaining("branch_summary"));
    });

    it("exports a branch_summary generation for a summarizing tree navigation", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry: FakeBranchEntry = {
        type: "branch_summary",
        id: "b1",
        parentId: "a1",
        timestamp: COMPACTION_TS,
        summary: "The abandoned branch explored a caching layer.",
        usage: {
          input: 40000,
          output: 900,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 40900,
          cost: { total: 0.4 },
        },
      };
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_tree", {}, ctx);
      await pi.emit(
        "session_tree",
        { newLeafId: "b1", oldLeafId: "a1", summaryEntry: entry },
        ctx,
      );

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.operationName).toBeUndefined();
      expect(seeds[0]!.tags?.["pi.call_kind"]).toBe("branch_summary");
      expect(seeds[0]!.id).toBe(stablePiGenerationId("pi-conv-1", "b1"));
      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId("pi-conv-1", "a1"),
      ]);
      // tokensBefore only exists on compaction entries.
      expect(results[0]!.metadata).toEqual({ cost_usd: 0.4 });
      expect(results[0]!.output?.[0]?.parts[0]).toEqual({
        type: "text",
        text: "The abandoned branch explored a caching layer.",
      });
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("exports the entry session_tree carried, not the newest one on the branch", async () => {
      // session_tree resolves its entry with getEntry(summaryId), an exact id
      // lookup, so the event's entry is authoritative. The positional branch
      // scan exists only to correct session_compact's find-by-summary-text.
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const eventEntry: FakeBranchEntry = {
        type: "branch_summary",
        id: "b1",
        parentId: "a1",
        timestamp: COMPACTION_TS,
        summary: "Explored a caching layer.",
      };
      const laterEntry: FakeBranchEntry = {
        ...eventEntry,
        id: "b9",
        parentId: "b1",
      };
      const ctx = ctxWithBranch(
        "pi-conv-1",
        branchWith(eventEntry, laterEntry),
      );

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_tree",
        { newLeafId: "b9", oldLeafId: "a1", summaryEntry: eventEntry },
        ctx,
      );

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBe(stablePiGenerationId("pi-conv-1", "b1"));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("does not re-export the same summary entry twice", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      const event = { compactionEntry: entry, fromExtension: false };
      await pi.emit("session_compact", event, ctx);
      await pi.emit("session_compact", event, ctx);

      expect(sigil.startGeneration).toHaveBeenCalledTimes(1);
      expect(
        loggerMock.debug.mock.calls.map(([m]) => String(m)),
      ).toContainEqual(expect.stringContaining("already exported"));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("resolves the newest branch entry when the event hands back an older duplicate", async () => {
      // session_compact finds its entry by summary text, first match in the
      // whole session file, so two byte-identical summaries make it carry the
      // older entry. Resolving positionally from the branch avoids re-sending
      // an already-exported generation id.
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const first = compactionEntry({ id: "c1", parentId: "a1" });
      const second = compactionEntry({ id: "c2", parentId: "a2" });
      let branch = branchWith(first);
      const ctx = ctxWithBranch("pi-conv-1", () => branch);

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: first, fromExtension: false },
        ctx,
      );

      branch = [
        ...branch,
        {
          type: "message",
          id: "u2",
          parentId: "c1",
          message: { role: "user" },
        },
        {
          type: "message",
          id: "a2",
          parentId: "u2",
          message: { role: "assistant" },
        },
        second,
      ];
      // Pi hands us `first` again because the summaries are identical.
      await pi.emit(
        "session_compact",
        { compactionEntry: first, fromExtension: false },
        ctx,
      );

      expect(seeds.map((s) => s.id)).toEqual([
        stablePiGenerationId("pi-conv-1", "c1"),
        stablePiGenerationId("pi-conv-1", "c2"),
      ]);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("is a silent no-op before session_start and after session_shutdown", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));
      const event = { compactionEntry: entry, fromExtension: false };

      const pi = new FakePi();
      registerExtension(pi as any);

      await pi.emit("session_compact", event, ctx);
      expect(sigil.startGeneration).not.toHaveBeenCalled();

      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_shutdown", {}, ctx);
      await pi.emit("session_compact", event, ctx);

      expect(sigil.startGeneration).not.toHaveBeenCalled();
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("prefers ctx.model over the model cached from the last assistant message", async () => {
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "message_end",
        {
          message: {
            ...assistantMessage(),
            provider: "openai",
            model: "gpt-5",
          },
        },
        ctx,
      );
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(seeds[0]!.model).toEqual({
        provider: "anthropic",
        name: "claude-sonnet-4",
      });
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("falls back to the last seen model when ctx.model is unavailable", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry), {
        model: null,
      });

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "message_end",
        {
          message: {
            ...assistantMessage(),
            provider: "openai",
            model: "gpt-5",
          },
        },
        ctx,
      );
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(seeds[0]!.model).toEqual({ provider: "openai", name: "gpt-5" });
      expect(results[0]!.responseModel).toBe("gpt-5");
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("skips the export when no model can be resolved", async () => {
      const { sigil } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry), {
        model: null,
      });

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(sigil.startGeneration).not.toHaveBeenCalled();
      expect(
        loggerMock.debug.mock.calls.map(([m]) => String(m)),
      ).toContainEqual(expect.stringContaining("no model could be resolved"));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("drops the summary text in metadata_only but keeps usage, tags and metadata", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil, "metadata_only");
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(results[0]!.output).toBeUndefined();
      expect(results[0]!.usage?.inputTokens).toBe(120000);
      expect(seeds[0]!.tags?.["pi.call_kind"]).toBe("compaction");
      expect(results[0]!.metadata?.["pi.tokens_before"]).toBe(152000);
      // The summary must not leak through metadata or tags, which content
      // capture never strips.
      expect(JSON.stringify([seeds[0], results[0]])).not.toContain(
        "split the exporter",
      );
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    // The summary is assistant text, not a tool body, so the no_tool_content
    // split does not apply to it: every mode except metadata_only keeps it.
    it.each([
      "full",
      "no_tool_content",
      "full_with_metadata_spans",
    ] as const)("keeps the summary text in %s", async (contentCapture) => {
      const { sigil, results } = setupClient();
      useConfig(sigil, contentCapture);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(results[0]!.output).toEqual([
        {
          role: "assistant",
          parts: [
            {
              type: "text",
              text: "Earlier: we split the exporter and fixed two tests.",
            },
          ],
        },
      ]);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("exports without a usage block when the host reports no usage", async () => {
      const { sigil, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({ usage: undefined });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(results).toHaveLength(1);
      expect(results[0]!.usage).toBeUndefined();
      // The pre-compaction context estimate is still reported; it is not a
      // substitute for the missing input token count.
      expect(results[0]!.metadata?.["pi.tokens_before"]).toBe(152000);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it.each([
      {
        name: "a usage object with a cost but no token counts",
        usage: { cost: { total: 1.75 } },
        cost: 1.75,
      },
      {
        name: "a usage object with nothing mappable",
        usage: { input: "lots", cost: { total: "free" } },
        cost: undefined,
      },
    ])("omits the usage block for $name", async ({ usage, cost }) => {
      // Zeros would read as "this call used no tokens"; the truth is that
      // the host did not report any. A cost still survives on its own.
      const { sigil, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({ usage });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(results).toHaveLength(1);
      expect(results[0]!.usage).toBeUndefined();
      expect(results[0]!.metadata?.cost_usd).toBe(cost);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it.each([
      { name: "missing", timestamp: undefined },
      { name: "not a parseable date", timestamp: "whenever" },
    ])("falls back to the current time when the entry timestamp is $name", async ({
      timestamp,
    }) => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry({ timestamp });
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));
      const now = Date.parse("2025-02-02T03:04:05.000Z");
      const nowSpy = vi.spyOn(Date, "now").mockReturnValue(now);

      try {
        const pi = new FakePi();
        registerExtension(pi as any);
        await pi.emit("session_start", {}, ctx);
        await pi.emit(
          "session_compact",
          { compactionEntry: entry, fromExtension: false },
          ctx,
        );
      } finally {
        nowSpy.mockRestore();
      }

      expect(results).toHaveLength(1);
      expect(results[0]!.completedAt).toEqual(new Date(now));
      // No start signal was seen, so startedAt collapses onto completedAt
      // rather than inverting the window.
      expect(seeds[0]!.startedAt).toEqual(new Date(now));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("takes startedAt from session_before_compact", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));
      const startedAt = COMPACTION_AT - 5_000;
      const nowSpy = vi.spyOn(Date, "now").mockReturnValue(startedAt);

      try {
        const pi = new FakePi();
        registerExtension(pi as any);
        await pi.emit("session_start", {}, ctx);
        await pi.emit("session_before_compact", {}, ctx);
        await pi.emit(
          "session_compact",
          { compactionEntry: entry, fromExtension: false },
          ctx,
        );
      } finally {
        nowSpy.mockRestore();
      }

      expect(seeds[0]!.startedAt).toEqual(new Date(startedAt));
      expect(results[0]!.completedAt).toEqual(new Date(COMPACTION_AT));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("falls back to the entry timestamp when no start signal was seen", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(seeds[0]!.startedAt).toEqual(new Date(COMPACTION_AT));
      expect(results[0]!.completedAt).toEqual(new Date(COMPACTION_AT));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("overwrites an orphaned start from an aborted compaction", async () => {
      // An aborted or failed compaction emits no session_compact at all, so
      // its start timestamp must not be reused by the next attempt.
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));
      const orphaned = COMPACTION_AT - 60_000;
      const real = COMPACTION_AT - 3_000;
      const nowSpy = vi.spyOn(Date, "now").mockReturnValue(orphaned);

      try {
        const pi = new FakePi();
        registerExtension(pi as any);
        await pi.emit("session_start", {}, ctx);
        await pi.emit("session_before_compact", {}, ctx);
        // ... aborted, no session_compact ...
        nowSpy.mockReturnValue(real);
        await pi.emit("session_before_compact", {}, ctx);
        await pi.emit(
          "session_compact",
          { compactionEntry: entry, fromExtension: false },
          ctx,
        );
      } finally {
        nowSpy.mockRestore();
      }

      expect(seeds[0]!.startedAt).toEqual(new Date(real));
      expect(seeds[0]!.startedAt).not.toEqual(new Date(orphaned));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("does not reuse a consumed start for a later compaction", async () => {
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const first = compactionEntry({ id: "c1" });
      const second = compactionEntry({
        id: "c2",
        parentId: "c1",
        timestamp: "2025-01-01T00:10:05.000Z",
      });
      let branch = branchWith(first);
      const ctx = ctxWithBranch("pi-conv-1", () => branch);
      const firstStart = COMPACTION_AT - 5_000;
      const nowSpy = vi.spyOn(Date, "now").mockReturnValue(firstStart);

      try {
        const pi = new FakePi();
        registerExtension(pi as any);
        await pi.emit("session_start", {}, ctx);
        await pi.emit("session_before_compact", {}, ctx);
        await pi.emit(
          "session_compact",
          { compactionEntry: first, fromExtension: false },
          ctx,
        );
        branch = branchWith(first, second);
        await pi.emit(
          "session_compact",
          { compactionEntry: second, fromExtension: false },
          ctx,
        );
      } finally {
        nowSpy.mockRestore();
      }

      expect(seeds[0]!.startedAt).toEqual(new Date(firstStart));
      // Second export has no start of its own, so it falls back to its own
      // entry timestamp instead of inheriting the first one's start.
      expect(seeds[1]!.startedAt).toEqual(
        new Date(Date.parse("2025-01-01T00:10:05.000Z")),
      );
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("clamps startedAt so it never exceeds completedAt", async () => {
      const { sigil, seeds, results } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));
      // Clock jumped forward after the entry was written (or the entry came
      // from a machine with a different clock).
      const nowSpy = vi
        .spyOn(Date, "now")
        .mockReturnValue(COMPACTION_AT + 60_000);

      try {
        const pi = new FakePi();
        registerExtension(pi as any);
        await pi.emit("session_start", {}, ctx);
        await pi.emit("session_before_compact", {}, ctx);
        await pi.emit(
          "session_compact",
          { compactionEntry: entry, fromExtension: false },
          ctx,
        );
      } finally {
        nowSpy.mockRestore();
      }

      expect(seeds[0]!.startedAt).toEqual(new Date(COMPACTION_AT));
      expect(results[0]!.completedAt).toEqual(new Date(COMPACTION_AT));
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("discards the turn state that a manual mid-stream /compact abandons", async () => {
      // Manual /compact disconnects from the agent before aborting, so the
      // in-flight turn's turn_end never arrives. Its buffered prompt must not
      // be attributed to the next turn.
      const { sigil, turnResults } = setupClient();
      useConfig(sigil, "full");
      const msg = assistantMessage();
      const ctx = ctxWithBranch("pi-conv-1", branchWith());

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit(
        "message_end",
        {
          message: {
            role: "user",
            content: "abandoned prompt",
            timestamp: Date.now(),
          },
        },
        ctx,
      );
      await pi.emit("session_before_compact", { reason: "manual" }, ctx);

      // Next turn, with a fresh prompt.
      await pi.emit("turn_start", {}, ctx);
      await pi.emit(
        "message_end",
        {
          message: {
            role: "user",
            content: "fresh prompt",
            timestamp: Date.now(),
          },
        },
        ctx,
      );
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      const input = turnResults[0]?.input as
        | Array<{ parts: Array<{ text?: string }> }>
        | undefined;
      expect(input).toHaveLength(1);
      expect(input?.[0]?.parts[0]?.text).toBe("fresh prompt");
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("swallows an export failure into logger.error", async () => {
      const { sigil } = setupClient({
        startGenerationError: new Error("queue closed"),
      });
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      // The log names the entry: this catch is the only place an export
      // failure surfaces, and the two summary kinds interleave in the log.
      expect(loggerMock.error).toHaveBeenCalledWith(
        "session_compact failed, entry=c1",
        expect.any(Error),
      );
    });

    it("retries a previously failed entry on the next event", async () => {
      // The entry id is only recorded once the recorder accepted it, so a
      // transient export failure does not permanently drop the generation.
      let fail = true;
      const seeds: CapturedSeed[] = [];
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async () => {}),
        startGeneration: vi.fn(async (seed, run) => {
          if (fail) {
            fail = false;
            throw new Error("transient");
          }
          seeds.push(seed as CapturedSeed);
          await run({ setResult: vi.fn(), setCallError: vi.fn() });
        }),
        startToolExecution: vi.fn(),
        shutdown: vi.fn(async () => {}),
      };
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      const event = { compactionEntry: entry, fromExtension: false };
      await pi.emit("session_compact", event, ctx);
      await pi.emit("session_compact", event, ctx);

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBe(stablePiGenerationId("pi-conv-1", "c1"));
    });

    it("clears summary state across sessions", async () => {
      // A new session_start must not inherit the previous session's exported
      // entry ids or pending starts.
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = ctxWithBranch("pi-conv-1", branchWith(entry));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.id).toBe(seeds[1]!.id);
      expect(loggerMock.error).not.toHaveBeenCalled();
    });

    it("falls back to the event entry when getBranch is unavailable", async () => {
      const { sigil, seeds } = setupClient();
      useConfig(sigil);
      const entry = compactionEntry();
      const ctx = {
        sessionManager: {
          getSessionFile: () => "pi-session.jsonl",
          getSessionId: () => "pi-conv-1",
        },
        model: defaultModel,
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: entry, fromExtension: false },
        ctx,
      );

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBe(stablePiGenerationId("pi-conv-1", "c1"));
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(loggerMock.error).not.toHaveBeenCalled();
    });
  });

  describe("generation lineage on a forked session", () => {
    // These tests drive the real header read in sessionOrigin.ts against
    // session files on disk, so they cover the wiring, not just the pure
    // lineage function.
    const TRUNK_ID = "0199aaaa-1111-7000-8000-aaaaaaaaaaaa";
    const FORK_ID = "0199bbbb-2222-7000-8000-bbbbbbbbbbbb";
    const TRUNK_STARTED_AT = "2020-01-01T00:00:00.000Z";
    const BEFORE_FORK = "2020-01-01T00:00:30.000Z";
    const FORKED_AT = "2020-01-01T00:01:00.000Z";
    const AFTER_FORK = "2020-01-01T00:02:00.000Z";

    let dir: string;
    let trunkFile: string;
    let forkFile: string;

    beforeEach(() => {
      dir = mkdtempSync(join(tmpdir(), "pi-fork-lineage-"));
      trunkFile = join(dir, "trunk.jsonl");
      forkFile = join(dir, "fork.jsonl");
      writeSessionFile(trunkFile, trunkHeader());
      writeSessionFile(forkFile, forkHeader());
    });

    afterEach(() => {
      rmSync(dir, { recursive: true, force: true });
    });

    function writeSessionFile(path: string, header: Record<string, unknown>) {
      writeFileSync(path, `${JSON.stringify(header)}\n`);
    }

    function trunkHeader(): Record<string, unknown> {
      return {
        type: "session",
        version: 3,
        id: TRUNK_ID,
        timestamp: TRUNK_STARTED_AT,
        cwd: "/fixture/worktree",
      };
    }

    function forkHeader(
      overrides: Record<string, unknown> = {},
    ): Record<string, unknown> {
      return {
        type: "session",
        version: 3,
        id: FORK_ID,
        timestamp: FORKED_AT,
        cwd: "/fixture/worktree",
        parentSession: trunkFile,
        ...overrides,
      };
    }

    interface ForkSeed {
      id?: string;
      parentGenerationIds?: string[];
      metadata?: Record<string, unknown>;
    }

    function setupForkClient() {
      const seeds: ForkSeed[] = [];
      const recorder = {
        setResult: vi.fn(),
        setCallError: vi.fn(),
        setFirstTokenAt: vi.fn(),
      };
      const sigil: Agento11yLike = {
        startStreamingGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as ForkSeed);
          await run(recorder);
        }),
        startGeneration: vi.fn(async (seed, run) => {
          seeds.push(seed as ForkSeed);
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
      return { sigil, seeds };
    }

    function messageEntry(
      id: string,
      parentId: string | null,
      role: string,
      timestamp: string,
      message: unknown = { role },
    ) {
      return { type: "message", id, parentId, timestamp, message };
    }

    /**
     * The fork's branch: `a1` was copied from the trunk with its id and
     * timestamp intact, `u2` and `a2` are the fork's own first turn.
     */
    function forkBranch(assistantMsg: unknown) {
      return [
        messageEntry("u1", null, "user", BEFORE_FORK),
        messageEntry("a1", "u1", "assistant", BEFORE_FORK),
        messageEntry("u2", "a1", "user", AFTER_FORK),
        messageEntry("a2", "u2", "assistant", AFTER_FORK, assistantMsg),
      ];
    }

    /** The fork's second turn, appended to `forkBranch`. */
    function forkBranchTurnTwo(first: unknown, second: unknown) {
      return [
        ...forkBranch(first),
        messageEntry("u3", "a2", "user", "2020-01-01T00:03:00.000Z"),
        messageEntry(
          "a3",
          "u3",
          "assistant",
          "2020-01-01T00:03:01.000Z",
          second,
        ),
      ];
    }

    function ctxForSession(
      sessionFile: string,
      sessionId: string,
      branch: unknown[],
    ) {
      return {
        sessionManager: {
          getSessionFile: () => sessionFile,
          getSessionId: () => sessionId,
          getBranch: () => branch,
        },
      };
    }

    it("suppresses the parent edge and records the trunk link as metadata", async () => {
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const ctx = ctxForSession(forkFile, FORK_ID, forkBranch(msg));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBe(stablePiGenerationId(FORK_ID, "a2"));
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toEqual({
        "pi.fork.parent_session_id": TRUNK_ID,
        "pi.fork.parent_generation_id": stablePiGenerationId(TRUNK_ID, "a1"),
      });
    });

    it("links the fork's second turn to its first", async () => {
      const { seeds } = setupForkClient();
      const first = assistantMessage();
      const second = assistantMessage();
      let branch: unknown[] = forkBranch(first);
      const ctx = {
        sessionManager: {
          getSessionFile: () => forkFile,
          getSessionId: () => FORK_ID,
          getBranch: () => branch,
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: first, toolResults: [] }, ctx);

      branch = forkBranchTurnTwo(first, second);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: second, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(2);
      expect(seeds[1]!.id).toBe(stablePiGenerationId(FORK_ID, "a3"));
      expect(seeds[1]!.parentGenerationIds).toEqual([
        stablePiGenerationId(FORK_ID, "a2"),
      ]);
      expect(seeds[1]!.metadata).toBeUndefined();
    });

    it("links a resumed fork's turn to the fork's own earlier turn", async () => {
      // `pi -c` on a forked session: the header still says fork, and this
      // plugin instance has exported nothing yet, but the parent turn was
      // added after the fork so its generation exists under FORK_ID.
      const { seeds } = setupForkClient();
      const second = assistantMessage();
      const ctx = ctxForSession(
        forkFile,
        FORK_ID,
        forkBranchTurnTwo(assistantMessage(), second),
      );

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: second, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId(FORK_ID, "a2"),
      ]);
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("keeps the edge on a --session continuation of the trunk", async () => {
      // Same branch shape, but the header has no parentSession. A fresh
      // process resuming the trunk has exported nothing yet, and the
      // parent generation exists under this very conversation id.
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const ctx = ctxForSession(trunkFile, TRUNK_ID, forkBranch(msg));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId(TRUNK_ID, "a1"),
      ]);
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("re-resolves fork state when the session is cloned mid-process", async () => {
      // `/clone` emits session_start with reason "fork" and hands the
      // extension a session manager pointing at the new file
      // (agent-session-runtime.js). The trunk's answer must not be reused.
      const { seeds } = setupForkClient();
      const trunkMsg = assistantMessage();
      const forkMsg = assistantMessage();
      let sessionFile = trunkFile;
      let sessionId = TRUNK_ID;
      let branch: unknown[] = forkBranch(trunkMsg);
      const ctx = {
        sessionManager: {
          getSessionFile: () => sessionFile,
          getSessionId: () => sessionId,
          getBranch: () => branch,
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: trunkMsg, toolResults: [] }, ctx);

      sessionFile = forkFile;
      sessionId = FORK_ID;
      branch = forkBranch(forkMsg);
      await pi.emit(
        "session_start",
        { reason: "fork", previousSessionFile: trunkFile },
        ctx,
      );
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: forkMsg, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId(TRUNK_ID, "a1"),
      ]);
      expect(seeds[1]!.parentGenerationIds).toBeUndefined();
      expect(seeds[1]!.metadata).toEqual({
        "pi.fork.parent_session_id": TRUNK_ID,
        "pi.fork.parent_generation_id": stablePiGenerationId(TRUNK_ID, "a1"),
      });
    });

    it("suppresses the edge without metadata when the trunk file is unreadable", async () => {
      writeSessionFile(
        forkFile,
        forkHeader({ parentSession: join(dir, "deleted-trunk.jsonl") }),
      );
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const ctx = ctxForSession(forkFile, FORK_ID, forkBranch(msg));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("suppresses the edge without metadata when the fork header has no timestamp", async () => {
      // Nothing places the parent on either side of the fork, so neither
      // an edge nor a trunk pointer can be trusted.
      const { seeds } = setupForkClient();
      const header = forkHeader();
      delete header.timestamp;
      writeSessionFile(forkFile, header);
      const msg = assistantMessage();
      const ctx = ctxForSession(forkFile, FORK_ID, forkBranch(msg));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("uses getHeader when the runtime exposes it, without touching the file", async () => {
      // The production path: a real SessionManager answers getHeader() from
      // memory, so only the trunk file is ever read.
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const getSessionFile = vi.fn(() => forkFile);
      const ctx = {
        sessionManager: {
          getHeader: () => forkHeader(),
          getSessionFile,
          getSessionId: () => FORK_ID,
          getBranch: () => forkBranch(msg),
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toEqual({
        "pi.fork.parent_session_id": TRUNK_ID,
        "pi.fork.parent_generation_id": stablePiGenerationId(TRUNK_ID, "a1"),
      });
      expect(getSessionFile).not.toHaveBeenCalled();
    });

    it("reads the fork header once per conversation", async () => {
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const getSessionFile = vi.fn(() => forkFile);
      const ctx = {
        sessionManager: {
          getSessionFile,
          getSessionId: () => FORK_ID,
          getBranch: () => forkBranch(msg),
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      for (let i = 0; i < 3; i++) {
        await pi.emit("turn_start", {}, ctx);
        await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);
      }

      expect(seeds).toHaveLength(3);
      expect(getSessionFile).toHaveBeenCalledTimes(1);
    });

    it("omits the trunk generation id when the parent predates the trunk session", async () => {
      // A fork of a fork: the trunk copied `a1` in from an older session and
      // never exported it, so no generation for it exists under TRUNK_ID
      // either. The edge stays suppressed, the metadata goes away.
      writeSessionFile(trunkFile, {
        ...trunkHeader(),
        timestamp: "2020-01-01T00:00:45.000Z",
      });
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const ctx = ctxForSession(forkFile, FORK_ID, forkBranch(msg));

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("suppresses the edge on an in-memory fork, whose header names no trunk", async () => {
      // `pi --no-session` plus `/fork`: createBranchedSession writes no
      // `parentSession` when the manager does not persist, and there is no
      // session file either, so the session_start reason and the trunk the
      // plugin was already on are all it has to work from.
      const { seeds } = setupForkClient();
      const trunkMsg = assistantMessage();
      const forkMsg = assistantMessage();
      // An in-memory manager answers getHeader() from memory. The header
      // carries the new id and the fork instant, but no parentSession.
      let header: Record<string, unknown> = {
        type: "session",
        id: TRUNK_ID,
        timestamp: TRUNK_STARTED_AT,
      };
      let branch: unknown[] = forkBranch(trunkMsg);
      const ctx = {
        sessionManager: {
          getHeader: () => header,
          getSessionFile: () => undefined,
          getSessionId: () => header.id as string,
          getBranch: () => branch,
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", { reason: "startup" }, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: trunkMsg, toolResults: [] }, ctx);

      header = { type: "session", id: FORK_ID, timestamp: FORKED_AT };
      branch = forkBranch(forkMsg);
      await pi.emit("session_start", { reason: "fork" }, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: forkMsg, toolResults: [] }, ctx);

      expect(seeds).toHaveLength(2);
      expect(seeds[0]!.parentGenerationIds).toEqual([
        stablePiGenerationId(TRUNK_ID, "a1"),
      ]);
      // The defect this guards against: hashing the copied entry with the
      // fork's conversation id names a generation nobody exported.
      expect(seeds[1]!.id).toBe(stablePiGenerationId(FORK_ID, "a2"));
      expect(seeds[1]!.parentGenerationIds).toBeUndefined();
      expect(seeds[1]!.metadata).toEqual({
        "pi.fork.parent_session_id": TRUNK_ID,
        "pi.fork.parent_generation_id": stablePiGenerationId(TRUNK_ID, "a1"),
      });
    });

    it("suppresses the edge on an in-memory fork with no trunk to name", async () => {
      // Same path, but the plugin never saw the trunk conversation, so it
      // has no trunk id. The edge must still go.
      const { seeds } = setupForkClient();
      const msg = assistantMessage();
      const ctx = {
        sessionManager: {
          getHeader: () => ({
            type: "session",
            id: FORK_ID,
            timestamp: FORKED_AT,
          }),
          getSessionFile: () => undefined,
          getSessionId: () => FORK_ID,
          getBranch: () => forkBranch(msg),
        },
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", { reason: "fork" }, ctx);
      await pi.emit("turn_start", {}, ctx);
      await pi.emit("turn_end", { message: msg, toolResults: [] }, ctx);

      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toBeUndefined();
    });

    it("suppresses the edge for a compaction taken before the fork's first turn", async () => {
      // The summary's parent is the inherited assistant entry, so it sits on
      // the trunk side of the fork just as a turn's parent would.
      const { seeds } = setupForkClient();
      const compaction = {
        type: "compaction",
        id: "c1",
        parentId: "a1",
        timestamp: AFTER_FORK,
        summary: "Earlier: we forked and compacted straight away.",
      };
      const ctx = {
        sessionManager: {
          getSessionFile: () => forkFile,
          getSessionId: () => FORK_ID,
          getBranch: () => [
            messageEntry("u1", null, "user", BEFORE_FORK),
            messageEntry("a1", "u1", "assistant", BEFORE_FORK),
            compaction,
          ],
        },
        model: defaultModel,
      };

      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, ctx);
      await pi.emit("session_before_compact", { reason: "manual" }, ctx);
      await pi.emit(
        "session_compact",
        { compactionEntry: compaction, fromExtension: false },
        ctx,
      );

      expect(seeds).toHaveLength(1);
      expect(seeds[0]!.id).toBe(stablePiGenerationId(FORK_ID, "c1"));
      expect(seeds[0]!.parentGenerationIds).toBeUndefined();
      expect(seeds[0]!.metadata).toEqual({
        "pi.fork.parent_session_id": TRUNK_ID,
        "pi.fork.parent_generation_id": stablePiGenerationId(TRUNK_ID, "a1"),
      });
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
