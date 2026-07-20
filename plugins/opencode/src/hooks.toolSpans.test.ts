import { createServer, type Server } from "node:http";
import type { Agento11yClient } from "@grafana/agento11y";
import type { Part } from "@opencode-ai/sdk";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import {
  _peekToolExecutionState,
  _resetHookState,
  _resetToolExecutionState,
  createAgento11yHooks,
  drainActiveToolExecutions,
  emitToolSpans,
  mergeToolSpanRecords,
  type ToolExecutionRecord,
  toolSpansFromParts,
} from "./hooks.js";
import { Redactor } from "./redact.js";

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

function makeRecord(
  overrides?: Partial<ToolExecutionRecord>,
): ToolExecutionRecord {
  return {
    sessionID: "sess-1",
    toolName: "Bash",
    toolCallId: "call-1",
    startedAt: 1_700_000_001_000,
    completedAt: 1_700_000_002_000,
    input: { command: "ls" },
    output: "ok",
    ...overrides,
  };
}

const defaultOpts = () => ({
  conversationId: "sess-1",
  agentName: "opencode:build",
  agentVersion: "test-version",
  requestProvider: "anthropic",
  requestModel: "claude-sonnet-4-opencode",
  redactor: new Redactor(),
  debugLog: () => {},
});

describe("emitToolSpans", () => {
  it("does nothing when no records", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, [], {
      ...defaultOpts(),
      contentCapture: "metadata_only",
    });
    expect(recorders).toHaveLength(0);
  });

  it("creates a span per completed record with full context", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      [
        makeRecord({ toolCallId: "c1", toolName: "Bash" }),
        makeRecord({ toolCallId: "c2", toolName: "Read" }),
      ],
      { ...defaultOpts(), contentCapture: "metadata_only" },
    );

    expect(recorders).toHaveLength(2);
    expect(recorders[0]!.start).toMatchObject({
      toolName: "Bash",
      toolCallId: "c1",
      toolType: "function",
      conversationId: "sess-1",
      agentName: "opencode:build",
      agentVersion: "test-version",
      requestProvider: "anthropic",
      requestModel: "claude-sonnet-4-opencode",
    });
    expect(recorders[1]!.start).toMatchObject({
      toolName: "Read",
      toolCallId: "c2",
    });
    expect(recorders.every((r) => r.ended)).toBe(true);
  });

  it("uses real start/end times", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      [makeRecord({ startedAt: 1000, completedAt: 5000 })],
      { ...defaultOpts(), contentCapture: "metadata_only" },
    );

    expect(recorders[0]!.start).toMatchObject({ startedAt: new Date(1000) });
    expect(recorders[0]!.result?.completedAt).toEqual(new Date(5000));
  });

  it("omits arguments and result in metadata_only", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, [makeRecord()], {
      ...defaultOpts(),
      contentCapture: "metadata_only",
    });

    expect(recorders[0]!.result?.arguments).toBeUndefined();
    expect(recorders[0]!.result?.result).toBeUndefined();
  });

  it("omits arguments and result in no_tool_content", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, [makeRecord()], {
      ...defaultOpts(),
      contentCapture: "no_tool_content",
    });

    expect(recorders[0]!.result?.arguments).toBeUndefined();
    expect(recorders[0]!.result?.result).toBeUndefined();
  });

  it("includes redacted arguments and result in full", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(
      client,
      [
        makeRecord({
          input: { command: "echo glc_abcdefghijklmnopqrstuv" },
          output: "leaked glc_abcdefghijklmnopqrstuv done",
        }),
      ],
      { ...defaultOpts(), contentCapture: "full" },
    );

    expect(recorders[0]!.result?.arguments).toBe(
      '{"command":"echo [REDACTED:grafana-cloud-token]"}',
    );
    expect(recorders[0]!.result?.result).toBe(
      "leaked [REDACTED:grafana-cloud-token] done",
    );
  });

  it("marks error records with setCallError", () => {
    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, [makeRecord({ isError: true, error: "boom" })], {
      ...defaultOpts(),
      contentCapture: "metadata_only",
    });

    expect(recorders[0]!.callError).toBeInstanceOf(Error);
    expect((recorders[0]!.callError as Error).message).toBe("boom");
  });

  it("swallows SDK errors so a span failure does not break the plugin", () => {
    const failing = {
      startToolExecution: () => {
        throw new Error("nope");
      },
    } as unknown as Agento11yClient;
    expect(() =>
      emitToolSpans(failing, [makeRecord()], {
        ...defaultOpts(),
        contentCapture: "metadata_only",
      }),
    ).not.toThrow();
  });
});

describe("toolSpansFromParts", () => {
  it("returns completed and error records with terminal timing", () => {
    const parts: Part[] = [
      {
        id: "p1",
        sessionID: "sess-1",
        messageID: "m1",
        type: "tool",
        callID: "c1",
        tool: "Bash",
        state: {
          status: "completed",
          input: { command: "ls" },
          output: "main.go",
          title: "ls",
          metadata: {},
          time: { start: 100, end: 200 },
        },
      } as Part,
      {
        id: "p2",
        sessionID: "sess-1",
        messageID: "m1",
        type: "tool",
        callID: "c2",
        tool: "Read",
        state: {
          status: "error",
          input: { path: "/missing" },
          error: "ENOENT",
          time: { start: 300, end: 400 },
        },
      } as Part,
      {
        id: "p3",
        sessionID: "sess-1",
        messageID: "m1",
        type: "tool",
        callID: "c3",
        tool: "Pending",
        state: { status: "pending", input: {}, raw: "" },
      } as Part,
      {
        id: "p4",
        sessionID: "sess-1",
        messageID: "m1",
        type: "text",
        text: "hello",
      } as Part,
    ];

    const records = toolSpansFromParts("sess-1", parts);
    expect(records).toEqual([
      {
        sessionID: "sess-1",
        toolName: "Bash",
        toolCallId: "c1",
        startedAt: 100,
        completedAt: 200,
        input: { command: "ls" },
        output: "main.go",
      },
      {
        sessionID: "sess-1",
        toolName: "Read",
        toolCallId: "c2",
        startedAt: 300,
        completedAt: 400,
        input: { path: "/missing" },
        isError: true,
        error: "ENOENT",
      },
    ]);
  });
});

describe("mergeToolSpanRecords", () => {
  it("prefers terminal records, keeps unique hook records", () => {
    const term = [
      makeRecord({ toolCallId: "c1", startedAt: 1, completedAt: 2 }),
    ];
    const hook = [
      makeRecord({ toolCallId: "c1", startedAt: 10, completedAt: 20 }),
      makeRecord({ toolCallId: "c2", startedAt: 30, completedAt: 40 }),
    ];

    const merged = mergeToolSpanRecords(term, hook);
    expect(merged).toHaveLength(2);
    expect(merged[0]).toMatchObject({
      toolCallId: "c1",
      startedAt: 1,
      completedAt: 2,
    });
    expect(merged[1]).toMatchObject({
      toolCallId: "c2",
      startedAt: 30,
      completedAt: 40,
    });
  });

  it("scopes the dedup key by session", () => {
    const term = [makeRecord({ sessionID: "sess-a", toolCallId: "shared" })];
    const hook = [
      makeRecord({ sessionID: "sess-b", toolCallId: "shared", startedAt: 99 }),
    ];

    const merged = mergeToolSpanRecords(term, hook);
    expect(merged).toHaveLength(2);
  });

  it("keeps a term error record over a drained record with the same key", () => {
    const term = [
      makeRecord({
        toolCallId: "c1",
        startedAt: 1,
        completedAt: 2,
        isError: true,
        error: "ENOENT",
      }),
    ];
    const drained = [
      makeRecord({
        toolCallId: "c1",
        startedAt: 100,
        completedAt: 200,
        isError: true,
        error: "tool did not complete (errored, denied, or interrupted)",
      }),
    ];

    const merged = mergeToolSpanRecords(term, drained);
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({
      toolCallId: "c1",
      startedAt: 1,
      completedAt: 2,
      error: "ENOENT",
    });
  });

  it("keeps a drained record whose key is not covered by term records", () => {
    const term = [makeRecord({ toolCallId: "c1" })];
    const drained = [
      makeRecord({
        toolCallId: "c2",
        startedAt: 100,
        completedAt: 200,
        isError: true,
        error: "tool did not complete (errored, denied, or interrupted)",
      }),
    ];

    const merged = mergeToolSpanRecords(term, drained);
    expect(merged).toHaveLength(2);
    expect(merged[1]).toMatchObject({ toolCallId: "c2", isError: true });
  });
});

function metadataOnlyConfig(): Agento11yOpencodeConfig {
  return {
    endpoint: "http://127.0.0.1:1",
    auth: { mode: "none" },
    agentName: "opencode",
    agentVersion: "test-version",
    contentCapture: "metadata_only",
    debug: false,
  };
}

async function makeHooks(config: Agento11yOpencodeConfig) {
  const hooks = await createAgento11yHooks(config, {
    session: { message: async () => ({ data: { parts: [] } }) },
  } as never);
  if (!hooks) throw new Error("expected hooks");
  return hooks;
}

describe("drainActiveToolExecutions", () => {
  beforeEach(() => _resetToolExecutionState());
  afterEach(() => _resetToolExecutionState());

  it("turns active entries into error records, removes them, scoped by session", async () => {
    const hooks = await makeHooks(metadataOnlyConfig());

    await hooks.toolExecuteBefore(
      { sessionID: "s1", callID: "c1", tool: "bash" },
      { args: { command: "false" } },
    );
    await hooks.toolExecuteBefore(
      { sessionID: "s1", callID: "c2", tool: "read" },
      { args: { path: "/x" } },
    );
    await hooks.toolExecuteBefore(
      { sessionID: "s2", callID: "c3", tool: "grep" },
      { args: { pattern: "x" } },
    );

    const drained = drainActiveToolExecutions("s1");

    expect(drained).toHaveLength(2);
    for (const rec of drained) {
      expect(rec.isError).toBe(true);
      expect(rec.error).toBeTruthy();
      expect(rec.completedAt).toBeGreaterThan(0);
    }
    expect(drained.map((r) => r.toolCallId).sort()).toEqual(["c1", "c2"]);

    // Only s1 is drained; the unrelated session keeps its active entry.
    const active = _peekToolExecutionState().active;
    expect(active).toHaveLength(1);
    expect(active[0]).toMatchObject({ sessionID: "s2", toolCallId: "c3" });
  });

  it("returns nothing and mutates nothing when the session has no active entries", async () => {
    const hooks = await makeHooks(metadataOnlyConfig());
    await hooks.toolExecuteBefore(
      { sessionID: "s2", callID: "c3", tool: "grep" },
      { args: {} },
    );

    expect(drainActiveToolExecutions("s1")).toEqual([]);
    expect(_peekToolExecutionState().active).toHaveLength(1);
  });
});

describe("synthesized error spans for never-completed tools", () => {
  beforeEach(() => _resetToolExecutionState());
  afterEach(() => {
    _resetToolExecutionState();
    vi.useRealTimers();
  });

  it("emits exactly one error span with real startedAt in metadata_only", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(1000);

    const hooks = await makeHooks(metadataOnlyConfig());
    await hooks.toolExecuteBefore(
      { sessionID: "s1", callID: "c1", tool: "bash" },
      { args: { command: "false" } },
    );
    // The tool errored or was denied upstream: tool.execute.after never fires.
    vi.setSystemTime(5000);

    // Reproduce recordAssistantMessage's span assembly in metadata_only: parts
    // are not fetched, so termRecords is empty and the drained record is the
    // only source.
    const termRecords = toolSpansFromParts("s1", []);
    const drained = drainActiveToolExecutions("s1");
    const merged = mergeToolSpanRecords(termRecords, drained);

    const { client, recorders } = mockAgento11yClient();
    emitToolSpans(client, merged, {
      ...defaultOpts(),
      conversationId: "s1",
      contentCapture: "metadata_only",
    });

    expect(recorders).toHaveLength(1);
    expect(recorders[0]!.start).toMatchObject({
      toolName: "bash",
      toolCallId: "c1",
      startedAt: new Date(1000),
    });
    expect(recorders[0]!.callError).toBeInstanceOf(Error);
    expect(_peekToolExecutionState().active).toHaveLength(0);
  });
});

function startResponseServer(
  response: Record<string, unknown>,
): Promise<{ server: Server; baseUrl: string }> {
  return new Promise((resolve) => {
    const server = createServer((_req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(response));
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error("expected AddressInfo from server.address()");
      }
      resolve({ server, baseUrl: `http://127.0.0.1:${addr.port}` });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}

describe("hook lifecycle records and guard denial", () => {
  const servers: Server[] = [];

  beforeEach(() => {
    _resetHookState();
  });

  afterEach(async () => {
    await Promise.all(servers.splice(0).map(closeServer));
    _resetHookState();
  });

  it("toolExecuteBefore/After move an active record into completed", async () => {
    const config: Agento11yOpencodeConfig = {
      endpoint: "http://127.0.0.1:1",
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "full",
      debug: false,
      // guards disabled, so the before hook just records timing.
    };

    const hooks = await createAgento11yHooks(config, {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as never);
    if (!hooks) throw new Error("expected hooks");

    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "Bash" },
      { args: { command: "ls" } },
    );

    expect(_peekToolExecutionState().active).toHaveLength(1);

    hooks.toolExecuteAfter(
      {
        sessionID: "sess-1",
        callID: "call-1",
        tool: "Bash",
        args: { command: "ls" },
      },
      { title: "ls", output: "main.go", metadata: {} },
    );

    const peeked = _peekToolExecutionState();
    expect(peeked.active).toHaveLength(0);
    expect(peeked.completed).toHaveLength(1);
    expect(peeked.completed[0]).toMatchObject({
      sessionID: "sess-1",
      toolCallId: "call-1",
      toolName: "Bash",
      input: { command: "ls" },
      output: "main.go",
    });
    expect(peeked.completed[0]!.completedAt).toBeGreaterThan(0);
  });

  it("clears the active record when the guard denies the tool", async () => {
    const guardSrv = await startResponseServer({
      action: "deny",
      reason: "blocked",
      evaluations: [],
    });
    servers.push(guardSrv.server);

    const config: Agento11yOpencodeConfig = {
      endpoint: guardSrv.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "full",
      debug: false,
      guards: { enabled: true, timeoutMs: 1500, failOpen: false },
    };

    const hooks = await createAgento11yHooks(config, {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as never);
    if (!hooks) throw new Error("expected hooks");

    await expect(
      hooks.toolExecuteBefore(
        { sessionID: "sess-1", callID: "call-1", tool: "Bash" },
        { args: { command: "ls" } },
      ),
    ).rejects.toThrow("blocked");

    expect(_peekToolExecutionState().active).toHaveLength(0);

    // The after hook for a denied call should be a no-op since the active
    // record was already cleared.
    hooks.toolExecuteAfter(
      {
        sessionID: "sess-1",
        callID: "call-1",
        tool: "Bash",
        args: { command: "ls" },
      },
      { title: "ls", output: "ignored", metadata: {} },
    );
    expect(_peekToolExecutionState().completed).toHaveLength(0);

    await hooks.event({ event: { type: "global.disposed", properties: {} } });
  });

  it("drains an incomplete tool execution when the assistant message records (metadata_only)", async () => {
    const agento11ySrv = await startResponseServer({ results: [] });
    servers.push(agento11ySrv.server);

    const config: Agento11yOpencodeConfig = {
      endpoint: agento11ySrv.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "metadata_only",
      debug: false,
    };

    const hooks = await createAgento11yHooks(config, {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as never);
    if (!hooks) throw new Error("expected hooks");

    const sessionID = "sess-1";
    hooks.chatMessage(
      { sessionID },
      {
        message: {
          id: "user-1",
          sessionID,
          role: "user",
          time: { created: 1_700_000_000_000 },
          system: "sys",
          tools: { bash: true },
        } as never,
        parts: [] as never,
      },
    );

    // Tool starts but never completes (errored or denied upstream): opencode
    // skips tool.execute.after, so no after hook fires.
    await hooks.toolExecuteBefore(
      { sessionID, callID: "call-1", tool: "bash" },
      { args: { command: "false" } },
    );
    expect(_peekToolExecutionState().active).toHaveLength(1);

    await hooks.event({
      event: {
        type: "message.updated",
        properties: {
          info: {
            id: "msg-1",
            sessionID,
            role: "assistant",
            time: {
              created: 1_700_000_001_000,
              completed: 1_700_000_002_000,
            },
            modelID: "claude-sonnet-4-opencode",
            providerID: "anthropic",
            mode: "build",
            tokens: {
              input: 10,
              output: 5,
              reasoning: 0,
              cache: { read: 0, write: 0 },
            },
            finish: "end_turn",
          },
        },
      },
    });

    expect(_peekToolExecutionState().active).toHaveLength(0);

    await hooks.event({ event: { type: "global.disposed", properties: {} } });
  });

  it("removes the stranded active entry in full mode when the error part is present", async () => {
    const agento11ySrv = await startResponseServer({ results: [] });
    servers.push(agento11ySrv.server);

    const sessionID = "sess-full";
    const messageID = "msg-full";
    const errorPart = {
      id: "p-err",
      sessionID,
      messageID,
      type: "tool",
      callID: "call-1",
      tool: "bash",
      state: {
        status: "error",
        input: { command: "false" },
        error: "exit status 1",
        time: { start: 1_700_000_001_200, end: 1_700_000_001_400 },
      },
    };

    const config: Agento11yOpencodeConfig = {
      endpoint: agento11ySrv.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "full",
      debug: false,
    };

    const hooks = await createAgento11yHooks(config, {
      session: { message: async () => ({ data: { parts: [errorPart] } }) },
    } as never);
    if (!hooks) throw new Error("expected hooks");

    hooks.chatMessage(
      { sessionID },
      {
        message: {
          id: "user-full",
          sessionID,
          role: "user",
          time: { created: 1_700_000_000_000 },
          system: "sys",
          tools: { bash: true },
        } as never,
        parts: [] as never,
      },
    );

    // Native tool errored: tool.execute.after never fires, leaving an active
    // entry, but the error is captured in the fetched parts.
    await hooks.toolExecuteBefore(
      { sessionID, callID: "call-1", tool: "bash" },
      { args: { command: "false" } },
    );
    expect(_peekToolExecutionState().active).toHaveLength(1);

    await hooks.event({
      event: {
        type: "message.updated",
        properties: {
          info: {
            id: messageID,
            sessionID,
            role: "assistant",
            time: {
              created: 1_700_000_001_000,
              completed: 1_700_000_002_000,
            },
            modelID: "claude-sonnet-4-opencode",
            providerID: "anthropic",
            mode: "build",
            tokens: {
              input: 10,
              output: 5,
              reasoning: 0,
              cache: { read: 0, write: 0 },
            },
            finish: "end_turn",
          },
        },
      },
    });

    // Drained in full mode too, so the entry never leaks. The merge keeps the
    // accurate terminal record over the synthesized drained one; that
    // precedence is pinned by the mergeToolSpanRecords cases above.
    expect(_peekToolExecutionState().active).toHaveLength(0);

    await hooks.event({ event: { type: "global.disposed", properties: {} } });
  });

  it("clears active and completed records on session.deleted", async () => {
    const config: Agento11yOpencodeConfig = {
      endpoint: "http://127.0.0.1:1",
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "full",
      debug: false,
    };

    const hooks = await createAgento11yHooks(config, {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as never);
    if (!hooks) throw new Error("expected hooks");

    // Active record for sess-1.
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "Bash" },
      { args: { command: "ls" } },
    );
    // Completed record for sess-1.
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-2", tool: "Read" },
      { args: { path: "/x" } },
    );
    hooks.toolExecuteAfter(
      {
        sessionID: "sess-1",
        callID: "call-2",
        tool: "Read",
        args: { path: "/x" },
      },
      { title: "/x", output: "ok", metadata: {} },
    );
    // Active record for an unrelated session that must survive cleanup.
    await hooks.toolExecuteBefore(
      { sessionID: "sess-2", callID: "call-3", tool: "Bash" },
      { args: { command: "pwd" } },
    );

    expect(_peekToolExecutionState().active).toHaveLength(2);
    expect(_peekToolExecutionState().completed).toHaveLength(1);

    await hooks.event({
      event: {
        type: "session.deleted",
        properties: { info: { id: "sess-1" } },
      },
    });

    const peeked = _peekToolExecutionState();
    expect(peeked.completed).toHaveLength(0);
    expect(peeked.active).toHaveLength(1);
    expect(peeked.active[0]).toMatchObject({
      sessionID: "sess-2",
      toolCallId: "call-3",
    });
  });
});
