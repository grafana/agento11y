// Behavior tests use a fake Cordis context. Export tests send records from the
// real SDK to a local HTTP server.

import { mkdtempSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Agento11yClient } from "@grafana/agento11y";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type {
  DshContext,
  DshGenerateOptions,
  DshSession,
  DshStreamChunk,
  DshToolDispatchExecution,
  DshToolExecutionResult,
} from "./dsh.js";
import { apply } from "./index.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";

type StreamListener = (
  request: DshGenerateOptions,
  next: () => AsyncIterable<DshStreamChunk>,
) => AsyncIterable<DshStreamChunk>;
type ToolListener = (
  exec: DshToolDispatchExecution,
  next: () => Promise<DshToolExecutionResult>,
) => Promise<DshToolExecutionResult>;

class FakeDsh implements DshContext {
  stream?: StreamListener;
  tool?: ToolListener;
  disposers: Array<() => void | Promise<void>> = [];
  readonly store = new Map<string, DshSession>();
  readonly titles = new Map<string, string>();

  get(service: string): unknown {
    if (service === "sessions") {
      return {
        get: (id: string): DshSession | undefined => this.store.get(id),
      };
    }
    if (service === "sessionTitle") {
      return {
        get: (session: DshSession) => {
          const title = this.titles.get(session.id);
          return title ? { title } : undefined;
        },
      };
    }
    return undefined;
  }

  on(event: string, listener: unknown): unknown {
    if (event === "llm/stream") this.stream = listener as StreamListener;
    if (event === "tools/execute") this.tool = listener as ToolListener;
    return () => {};
  }

  effect(setup: () => () => void | Promise<void>): unknown {
    this.disposers.push(setup());
    return () => {};
  }
}

function chunksFrom(
  chunks: DshStreamChunk[],
): () => AsyncIterable<DshStreamChunk> {
  return async function* () {
    for (const chunk of chunks) yield chunk;
  };
}

async function drain(
  iterable: AsyncIterable<DshStreamChunk>,
): Promise<DshStreamChunk[]> {
  const out: DshStreamChunk[] = [];
  for await (const chunk of iterable) out.push(chunk);
  return out;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const TURN_CHUNKS: DshStreamChunk[] = [
  { type: "block-start", index: 0, blockType: "text" },
  { type: "text-delta", index: 0, text: "Hel" },
  { type: "text-delta", index: 0, text: "lo" },
  { type: "block-end", index: 0, block: { type: "text", text: "Hello" } },
  { type: "usage", usage: { inputTokens: 10, outputTokens: 2 } },
  { type: "finish", reason: { kind: "stop" } },
];

const TURN_REQUEST: DshGenerateOptions = {
  provider: "deepseek",
  model: "deepseek-chat",
  sessionId: "session-1",
  messages: [{ role: "user", content: [{ type: "text", text: "hi" }] }],
};

describe("dsh plugin: transparency with no endpoint configured", () => {
  let saved: Record<string, string | undefined> = {};

  beforeEach(() => {
    saved = snapshotAndClearTestEnv();
    const home = mkdtempSync(join(tmpdir(), "agento11y-dsh-"));
    process.env.HOME = home;
    process.env.USERPROFILE = home;
    process.env.XDG_CONFIG_HOME = join(home, ".config");
  });

  afterEach(() => {
    restoreEnv(saved);
    saved = {};
  });

  it("yields every chunk unchanged and in order", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const chunks: DshStreamChunk[] = [
      { type: "text-delta", index: 0, text: "Hel" },
      { type: "text-delta", index: 0, text: "lo" },
      { type: "finish", reason: { kind: "stop" } },
    ];

    const got = await drain(ctx.stream!(TURN_REQUEST, chunksFrom(chunks)));

    expect(got).toHaveLength(3);
    for (const [i, chunk] of chunks.entries()) {
      expect(got[i]).toBe(chunk);
    }
  });

  it("rethrows the stream failure after yielding what came before it", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const delta: DshStreamChunk = {
      type: "text-delta",
      index: 0,
      text: "partial",
    };
    const streamErr = new Error("adapter exploded");
    const next = async function* (): AsyncIterable<DshStreamChunk> {
      yield delta;
      throw streamErr;
    };

    const seen: DshStreamChunk[] = [];
    let thrown: unknown;
    try {
      for await (const chunk of ctx.stream!(TURN_REQUEST, next)) {
        seen.push(chunk);
      }
    } catch (err) {
      thrown = err;
    }

    expect(seen).toEqual([delta]);
    expect(thrown).toBe(streamErr);
  });

  it("returns the dispatch outcome unchanged", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const toolResult: DshToolExecutionResult = {
      isError: false,
      content: [{ type: "text", text: "# README" }],
    };

    const got = await ctx.tool!(
      {
        callId: "call-1",
        name: "read_file",
        arguments: { path: "README.md" },
      },
      async () => toolResult,
    );

    expect(got).toBe(toolResult);
  });

  it("propagates a dispatch failure", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const dispatchErr = new Error("tool blew up");

    await expect(
      ctx.tool!({ callId: "call-1", name: "read_file", arguments: {} }, () =>
        Promise.reject(dispatchErr),
      ),
    ).rejects.toBe(dispatchErr);
  });

  it("waits for an active stream before disposal completes", async () => {
    const finish = deferred<void>();
    const ctx = new FakeDsh();
    apply(ctx);
    let started = false;
    const running = drain(
      ctx.stream!(TURN_REQUEST, async function* () {
        started = true;
        yield { type: "text-delta", index: 0, text: "partial" };
        await finish.promise;
        yield { type: "finish", reason: { kind: "stop" } };
      }),
    );
    await vi.waitFor(() => expect(started).toBe(true));

    let disposed = false;
    const disposal = Promise.resolve(ctx.disposers[0]!()).then(() => {
      disposed = true;
    });
    await Promise.resolve();
    expect(disposed).toBe(false);

    finish.resolve(undefined);
    await running;
    await disposal;
    expect(disposed).toBe(true);
  });

  it("waits for an active tool before disposal completes", async () => {
    const finish = deferred<DshToolExecutionResult>();
    const ctx = new FakeDsh();
    apply(ctx);
    const running = ctx.tool!(
      { callId: "call-1", name: "read_file", arguments: {} },
      () => finish.promise,
    );

    let disposed = false;
    const disposal = Promise.resolve(ctx.disposers[0]!()).then(() => {
      disposed = true;
    });
    await Promise.resolve();
    expect(disposed).toBe(false);

    finish.resolve({ isError: false, content: [] });
    await running;
    await disposal;
    expect(disposed).toBe(true);
  });

  it("returns one shutdown promise and delegates later calls", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const first = ctx.disposers[0]!();
    const second = ctx.disposers[0]!();
    expect(first).toBe(second);
    await first;

    const chunks = await drain(
      ctx.stream!(TURN_REQUEST, chunksFrom(TURN_CHUNKS)),
    );
    const outcome = { isError: false, content: [] };
    const tool = await ctx.tool!(
      { callId: "call-1", name: "read_file", arguments: {} },
      async () => outcome,
    );

    expect(chunks).toEqual(TURN_CHUNKS);
    expect(tool).toBe(outcome);
  });
});

interface CapturedExport {
  path: string;
  generations: Array<Record<string, any>>;
}

function startExportServer(): Promise<{
  server: Server;
  baseUrl: string;
  captures: CapturedExport[];
}> {
  const captures: CapturedExport[] = [];
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        let parsed: { generations?: Array<{ id?: string }> } = {};
        try {
          parsed = JSON.parse(body);
        } catch {
          /* recorded below as an empty generation list */
        }
        captures.push({
          path: req.url ?? "",
          generations: (parsed.generations ?? []) as Array<Record<string, any>>,
        });
        res.setHeader("Content-Type", "application/json");
        res.end(
          JSON.stringify({
            results: (parsed.generations ?? []).map((g) => ({
              generation_id: g?.id ?? "",
              accepted: true,
            })),
          }),
        );
      });
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error("expected AddressInfo from server.address()");
      }
      resolve({ server, baseUrl: `http://127.0.0.1:${addr.port}`, captures });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}

describe("dsh plugin: export against a local endpoint", () => {
  let env: Awaited<ReturnType<typeof startExportServer>>;
  let saved: Record<string, string | undefined> = {};
  let firstTokenAts: Date[] = [];
  // conversationTitle exists on the OTel span but not the export proto.
  let seeds: Array<Record<string, any>> = [];

  beforeEach(async () => {
    env = await startExportServer();
    saved = snapshotAndClearTestEnv();
    const home = mkdtempSync(join(tmpdir(), "agento11y-dsh-export-"));
    process.env.HOME = home;
    process.env.USERPROFILE = home;
    process.env.XDG_CONFIG_HOME = join(home, ".config");
    process.env.AGENTO11Y_ENDPOINT = env.baseUrl;
    process.env.AGENTO11Y_AUTH_TENANT_ID = "tenant";
    process.env.AGENTO11Y_AUTH_TOKEN = "token";
    process.env.AGENTO11Y_AGENT_VERSION = "test-version";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";

    firstTokenAts = [];
    seeds = [];
    for (const method of [
      "startStreamingGeneration",
      "startGeneration",
    ] as const) {
      const original = Agento11yClient.prototype[method];
      vi.spyOn(Agento11yClient.prototype, method).mockImplementation(function (
        this: Agento11yClient,
        seed: any,
        callback?: any,
      ) {
        seeds.push(seed);
        if (!callback) return (original as any).call(this, seed);
        return (original as any).call(this, seed, async (recorder: any) => {
          const set = recorder.setFirstTokenAt.bind(recorder);
          recorder.setFirstTokenAt = (at: Date) => {
            firstTokenAts.push(at);
            set(at);
          };
          return callback(recorder);
        });
      } as any);
    }
  });

  afterEach(async () => {
    vi.restoreAllMocks();
    await closeServer(env.server);
    restoreEnv(saved);
    saved = {};
  });

  async function runCall(
    ctx: FakeDsh,
    request: DshGenerateOptions,
    chunks: DshStreamChunk[],
  ): Promise<Record<string, any>> {
    await drain(ctx.stream!(request, chunksFrom(chunks)));
    // The SDK batches exports, so wait for the server instead of the iterator.
    await vi.waitFor(
      () => {
        expect(env.captures.length).toBeGreaterThan(0);
        expect(env.captures[0]?.generations.length).toBeGreaterThan(0);
      },
      { timeout: 5_000, interval: 25 },
    );
    return env.captures[0]!.generations[0]!;
  }

  it("exports a conversation turn as a streamText generation", async () => {
    const ctx = new FakeDsh();
    ctx.store.set("session-1", { id: "session-1" });
    ctx.titles.set("session-1", "Fix the parser");
    apply(ctx);

    const generation = await runCall(ctx, TURN_REQUEST, TURN_CHUNKS);

    expect(generation.model).toEqual({
      provider: "deepseek",
      name: "deepseek-chat",
    });
    expect(generation.conversation_id).toBe("session-1");
    expect(seeds[0]?.conversationTitle).toBe("Fix the parser");
    expect(generation.agent_name).toBe("dsh");
    expect(generation.operation_name).toBe("streamText");
    expect(generation.stop_reason).toBe("stop");
    // JSON encodes int64 fields as strings.
    expect(generation.usage).toMatchObject({
      input_tokens: "10",
      output_tokens: "2",
      total_tokens: "12",
    });
    expect(generation.output?.[0]?.parts?.[0]?.text).toBe("Hello");
    expect(firstTokenAts).toHaveLength(1);
  });

  it("exports an auxiliary call as a generateText generation", async () => {
    const ctx = new FakeDsh();
    apply(ctx);

    const generation = await runCall(
      ctx,
      { ...TURN_REQUEST, purpose: "compaction" },
      TURN_CHUNKS,
    );

    expect(generation.operation_name).toBe("generateText");
    expect(firstTokenAts).toHaveLength(0);
  });

  it("names a subagent session dsh/subagent", async () => {
    const ctx = new FakeDsh();
    ctx.store.set("session-2", {
      id: "session-2",
      header: { id: "session-2", origin: "subagent" },
    });
    apply(ctx);

    const generation = await runCall(
      ctx,
      { ...TURN_REQUEST, sessionId: "session-2" },
      TURN_CHUNKS,
    );

    expect(generation.agent_name).toBe("dsh/subagent");
  });

  it("retains session enrichment until a delayed export starts", async () => {
    const ctx = new FakeDsh();
    const session = {
      id: "session-2",
      header: { id: "session-2", origin: "subagent", cwd: "/tmp/project" },
    };
    ctx.store.set(session.id, session);
    ctx.titles.set(session.id, "Retained title");
    apply(ctx);

    const stream = ctx.stream!(
      { ...TURN_REQUEST, sessionId: session.id },
      async function* () {
        yield { type: "text-delta", index: 0, text: "answer" };
        ctx.store.delete(session.id);
        ctx.titles.delete(session.id);
        yield { type: "finish", reason: { kind: "stop" } };
      },
    );
    await drain(stream);
    await vi.waitFor(() => expect(env.captures.length).toBeGreaterThan(0), {
      timeout: 5_000,
      interval: 25,
    });

    const generation = env.captures[0]!.generations[0]!;
    expect(generation.agent_name).toBe("dsh/subagent");
    expect(seeds[0]?.conversationTitle).toBe("Retained title");
  });

  it("records one tool execution per dispatch", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const spy = vi.spyOn(Agento11yClient.prototype, "startToolExecution");
    const toolResult: DshToolExecutionResult = {
      isError: false,
      content: [{ type: "text", text: "# README" }],
    };

    const got = await ctx.tool!(
      {
        callId: "call-1",
        name: "read_file",
        arguments: { path: "README.md" },
      },
      async () => toolResult,
    );

    expect(got).toBe(toolResult);
    // Dispatch must not wait for the plugin's export.
    await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
    expect(spy.mock.calls[0]?.[0]).toMatchObject({
      toolName: "read_file",
      toolCallId: "call-1",
      agentName: "dsh",
    });
  });

  it("ties nested tool spans to the execution's owning session", async () => {
    const ctx = new FakeDsh();
    const session = {
      id: "session-2",
      header: { id: "session-2", origin: "subagent" },
    };
    ctx.store.set("session-2", session);
    ctx.titles.set("session-2", "Fix the parser");
    apply(ctx);
    const spy = vi.spyOn(Agento11yClient.prototype, "startToolExecution");

    await ctx.tool!(
      {
        callId: "call-7:code:1",
        rootCallId: "call-7",
        name: "read_file",
        arguments: { path: "README.md" },
        agent: { id: "session-2", session },
      },
      async () => ({ isError: false, content: [] }),
    );

    await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
    expect(spy.mock.calls[0]?.[0]).toMatchObject({
      toolCallId: "call-7:code:1",
      conversationId: "session-2",
      conversationTitle: "Fix the parser",
      agentName: "dsh/subagent",
    });
  });

  it("attributes equal tool call ids through their owning agents", async () => {
    const ctx = new FakeDsh();
    const first = { id: "session-a", header: { id: "session-a" } };
    const second = {
      id: "session-b",
      header: { id: "session-b", origin: "subagent" },
    };
    apply(ctx);
    const spy = vi.spyOn(Agento11yClient.prototype, "startToolExecution");

    for (const session of [first, second]) {
      await ctx.tool!(
        {
          callId: "same-call",
          name: "read_file",
          arguments: {},
          agent: { id: session.id, session },
        },
        async () => ({ isError: false, content: [] }),
      );
    }

    await vi.waitFor(() => expect(spy).toHaveBeenCalledTimes(2));
    expect(spy.mock.calls.map((call) => call[0].conversationId)).toEqual([
      "session-a",
      "session-b",
    ]);
    expect(spy.mock.calls.map((call) => call[0].agentName)).toEqual([
      "dsh",
      "dsh/subagent",
    ]);
  });

  it("keeps message structure in metadata_only", async () => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "metadata_only";
    const ctx = new FakeDsh();
    apply(ctx);

    const generation = await runCall(ctx, TURN_REQUEST, TURN_CHUNKS);

    expect(generation.input?.[0]?.role).toBe("MESSAGE_ROLE_USER");
    expect(generation.input?.[0]?.parts?.[0]).toHaveProperty("text", "");
    expect(generation.output?.[0]?.role).toBe("MESSAGE_ROLE_ASSISTANT");
    expect(generation.output?.[0]?.parts?.[0]).toHaveProperty("text", "");
  });

  it("keeps generation tool content in no_tool_content", async () => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "no_tool_content";
    const ctx = new FakeDsh();
    apply(ctx);
    const request: DshGenerateOptions = {
      ...TURN_REQUEST,
      tools: [
        {
          name: "read_file",
          description: "Read a file",
          parameters: { type: "object" },
        },
      ],
      messages: [
        {
          role: "assistant",
          content: [
            {
              type: "tool-call",
              id: "old-call",
              name: "read_file",
              arguments: '{"path":"before.md"}',
            },
          ],
        },
        {
          role: "user",
          source: { kind: "tool" },
          content: [
            {
              type: "tool-result",
              toolCallId: "history-call",
              content: [{ type: "text", text: "history body" }],
            },
          ],
        },
      ],
    };

    const generation = await runCall(ctx, request, [
      { type: "block-start", index: 0, blockType: "tool-call" },
      {
        type: "block-end",
        index: 0,
        block: {
          type: "tool-call",
          id: "output-call",
          name: "read_file",
          arguments: '{"path":"after.md"}',
        },
      },
      { type: "finish", reason: { kind: "tool-calls" } },
    ]);

    expect(generation.tools?.[0]?.description).toBe("Read a file");
    expect(generation.tools?.[0]?.input_schema_json).toBeTruthy();
    expect(
      generation.input?.[0]?.parts?.[0]?.tool_call?.input_json,
    ).toBeTruthy();
    expect(generation.input?.[1]?.parts?.[0]?.tool_result?.content).toBe(
      "history body",
    );
    expect(
      generation.output?.[0]?.parts?.[0]?.tool_call?.input_json,
    ).toBeTruthy();
  });

  it("keeps system history in place as a valid exported message", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const generation = await runCall(
      ctx,
      {
        ...TURN_REQUEST,
        system: "base",
        messages: [
          { role: "system", content: [{ type: "text", text: "history" }] },
          { role: "user", content: [{ type: "text", text: "question" }] },
        ],
      },
      TURN_CHUNKS,
    );

    expect(generation.system_prompt).toBe("base");
    expect(generation.input?.map((message: any) => message.role)).toEqual([
      "MESSAGE_ROLE_USER",
      "MESSAGE_ROLE_USER",
    ]);
    expect(generation.input?.[0]?.parts?.[0]?.text).toBe("history");
  });

  it("records cancellation before a terminal finish", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const controller = new AbortController();
    const secret =
      "TokenStore = " + "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";
    const chunks = ctx.stream!(
      { ...TURN_REQUEST, signal: controller.signal },
      async function* () {
        yield { type: "block-start", index: 0, blockType: "text" };
        yield { type: "text-delta", index: 0, text: "partial" };
        yield {
          type: "block-end",
          index: 0,
          block: { type: "text", text: "partial" },
        };
        yield { type: "usage", usage: { inputTokens: 1, outputTokens: 1 } };
        yield {
          type: "finish",
          reason: { kind: "aborted", failure: { message: secret } },
        };
      },
    );

    for await (const chunk of chunks) {
      if (chunk.type === "usage") {
        controller.abort(new Error(secret));
        break;
      }
    }

    await vi.waitFor(() => expect(env.captures.length).toBeGreaterThan(0), {
      timeout: 5_000,
      interval: 25,
    });
    const generation = env.captures[0]!.generations[0]!;
    expect(generation.stop_reason).toBe("aborted");
    expect(generation.call_error).not.toContain(secret);
    expect(generation.output?.[0]?.parts?.[0]?.text).toBe("partial");
  });

  it("timestamps tool completion before asynchronous recording", async () => {
    const now = vi
      .spyOn(Date, "now")
      .mockReturnValueOnce(1_000)
      .mockReturnValueOnce(1_005)
      .mockReturnValue(9_999);
    const ctx = new FakeDsh();
    apply(ctx);
    const starts: Array<Record<string, any>> = [];
    const results: Array<Record<string, any>> = [];
    vi.spyOn(
      Agento11yClient.prototype,
      "startToolExecution",
    ).mockImplementation((start: any) => {
      starts.push(start);
      return {
        setCallError: () => {},
        setResult: (result: any) => results.push(result),
        end: () => {},
      } as any;
    });

    await ctx.tool!(
      { callId: "call-time", name: "read_file", arguments: {} },
      async () => ({ isError: false, content: [] }),
    );

    await vi.waitFor(() => expect(results).toHaveLength(1));
    expect(starts[0]?.startedAt).toEqual(new Date(1_000));
    expect(results[0]?.completedAt).toEqual(new Date(1_005));
    now.mockRestore();
  });

  it("redacts tool titles and error copies without changing the outcome", async () => {
    const secret = "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";
    const ctx = new FakeDsh();
    const session = { id: "session-secret", header: { id: "session-secret" } };
    ctx.titles.set(session.id, `Debug ${secret}`);
    apply(ctx);
    const starts: Array<Record<string, any>> = [];
    const errors: Error[] = [];
    vi.spyOn(
      Agento11yClient.prototype,
      "startToolExecution",
    ).mockImplementation((start: any) => {
      starts.push(start);
      return {
        setCallError: (error: Error) => errors.push(error),
        setResult: () => {},
        end: () => {},
      } as any;
    });
    const outcome = {
      isError: true,
      content: [],
      error: { message: `failed with ${secret}` },
    };

    const got = await ctx.tool!(
      {
        callId: "call-secret",
        name: "read_file",
        arguments: {},
        agent: { id: session.id, session },
      },
      async () => outcome,
    );

    expect(got).toBe(outcome);
    await vi.waitFor(() => expect(errors).toHaveLength(1));
    expect(starts[0]?.conversationTitle).not.toContain(secret);
    expect(errors[0]?.message).not.toContain(secret);
    expect(errors[0]?.stack).not.toContain(secret);
  });

  it("redacts secrets in tool arguments and tool output", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const results: Array<Record<string, any>> = [];
    vi.spyOn(
      Agento11yClient.prototype,
      "startToolExecution",
    ).mockImplementation(
      () =>
        ({
          setCallError: () => {},
          setResult: (r: Record<string, any>) => results.push(r),
          end: () => {},
        }) as any,
    );
    const token = "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";

    await ctx.tool!(
      {
        callId: "call-1",
        name: "bash",
        arguments: { command: `echo ${token}` },
      },
      async () => ({
        isError: false,
        content: [{ type: "text", text: `wrote ${token}` }],
      }),
    );

    await vi.waitFor(() => expect(results).toHaveLength(1));
    expect(String(results[0]?.arguments)).not.toContain(token);
    expect(String(results[0]?.result)).not.toContain(token);
  });

  it("drops blank and unnamed parts without dropping the generation", async () => {
    const ctx = new FakeDsh();
    apply(ctx);

    const generation = await runCall(ctx, TURN_REQUEST, [
      { type: "text-delta", index: 0, text: "  " },
      { type: "reasoning-delta", index: 1, text: "" },
      {
        type: "tool-call-delta",
        index: 2,
        id: "unnamed",
        argumentsDelta: "{}",
      },
      { type: "usage", usage: { inputTokens: 3, outputTokens: 0 } },
      { type: "finish", reason: { kind: "stop" } },
    ]);

    expect(generation.output).toEqual([]);
    expect(generation.usage?.total_tokens).toBe("3");
  });

  it("exports the model's tool calls as output parts", async () => {
    const ctx = new FakeDsh();
    apply(ctx);

    const generation = await runCall(ctx, TURN_REQUEST, [
      { type: "block-start", index: 0, blockType: "tool-call" },
      {
        type: "block-end",
        index: 0,
        block: {
          type: "tool-call",
          id: "call-7",
          name: "read_file",
          arguments: '{"path":"README.md"}',
        },
      },
      { type: "finish", reason: { kind: "tool-calls" } },
    ]);

    expect(generation.stop_reason).toBe("tool-calls");
    const call = generation.output?.[0]?.parts?.[0]?.tool_call;
    expect(call).toMatchObject({ id: "call-7", name: "read_file" });
    // JSON encodes the input_json bytes as base64.
    expect(Buffer.from(call.input_json, "base64").toString()).toBe(
      '{"path":"README.md"}',
    );
  });

  it("reads a restored title without waiting for a new event", async () => {
    const ctx = new FakeDsh();
    ctx.store.set("session-1", { id: "session-1" });
    ctx.titles.set("session-1", "Restored title");
    apply(ctx);

    await runCall(ctx, TURN_REQUEST, TURN_CHUNKS);

    expect(seeds[0]?.conversationTitle).toBe("Restored title");
  });
});
