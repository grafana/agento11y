import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Context } from "@deepseek-ai/cordis";
import {
  type ContentBlock,
  createAssistantMessage,
  createSystemMessage,
  createToolResultMessage,
  createUserMessage,
  type GenerateOptions,
  LlmAdapter,
  LlmError,
  LlmRuntime,
  type StreamChunk,
  ToolCallId,
} from "@deepseek-ai/dsh-llm";
import { MeterProvider } from "@opentelemetry/sdk-metrics";
import { BasicTracerProvider } from "@opentelemetry/sdk-trace-base";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Agento11yClient } from "../../../js/src/client.js";
import { HTTPGenerationExporter } from "../../../js/src/exporters/http.js";
import type { DshGenerateOptions, DshStreamChunk } from "./dsh.js";
import * as plugin from "./index.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";

// Resolve the real SDK from source so these tests do not need a workspace build.
vi.mock("@grafana/agento11y", () => import("../../../js/src/core.js"));

interface ToolDispatch {
  callId: string;
  name: string;
  arguments: unknown;
}

interface ToolResult {
  isError: boolean;
  content: readonly ContentBlock[];
  error?: { code: string; message: string };
}

// dsh-tools is not installed. Only its event is synthetic; Cordis owns dispatch and cleanup.
declare module "@deepseek-ai/cordis" {
  interface Events {
    "tools/execute"(
      exec: ToolDispatch,
      next: () => Promise<ToolResult>,
    ): Promise<ToolResult>;
  }
}

function mirrorOptions(options: GenerateOptions): DshGenerateOptions {
  return options;
}

function mirrorChunk(chunk: StreamChunk): DshStreamChunk {
  return chunk;
}

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function turnChunks(): StreamChunk[] {
  return [
    { type: "block-start", index: 0, blockType: "reasoning" },
    { type: "reasoning-delta", index: 0, text: "Consider" },
    {
      type: "block-end",
      index: 0,
      block: { type: "reasoning", text: "Consider" },
    },
    { type: "block-start", index: 1, blockType: "text" },
    { type: "text-delta", index: 1, text: "Hel" },
    { type: "text-delta", index: 1, text: "lo" },
    { type: "block-end", index: 1, block: { type: "text", text: "Hello" } },
    { type: "block-start", index: 2, blockType: "tool-call" },
    {
      type: "tool-call-delta",
      index: 2,
      id: ToolCallId("next-call"),
      name: "read_file",
      argumentsDelta: '{"path":"next.txt"}',
    },
    {
      type: "block-end",
      index: 2,
      block: {
        type: "tool-call",
        id: ToolCallId("next-call"),
        name: "read_file",
        arguments: '{"path":"next.txt"}',
      },
    },
    {
      type: "usage",
      usage: {
        inputTokens: 10,
        outputTokens: 4,
        cacheReadTokens: 3,
        cacheWriteTokens: 2,
        reasoningTokens: 1,
        totalTokens: 19,
      },
    },
    { type: "finish", reason: { kind: "tool-calls" } },
  ];
}

function request(): GenerateOptions {
  return {
    provider: "scripted",
    model: "host-contract",
    system: "one-shot prefix",
    messages: [
      createSystemMessage("host system prefix"),
      createUserMessage({
        content: [{ type: "text", text: "Read the file" }],
        source: { kind: "user" },
      }),
      createAssistantMessage({
        content: [
          {
            type: "tool-call",
            id: ToolCallId("prior-call"),
            name: "read_file",
            arguments: '{"path":"prior.txt"}',
          },
        ],
        source: { provider: "scripted", model: "host-contract" },
      }),
      createToolResultMessage({
        callId: ToolCallId("prior-call"),
        content: [{ type: "text", text: "file unavailable" }],
        isError: true,
      }),
    ],
    tools: [
      {
        name: "read_file",
        description: "Read a file",
        parameters: { type: "object" },
      },
    ],
  };
}

class ScriptedAdapter extends LlmAdapter {
  readonly requests: GenerateOptions[] = [];

  constructor(
    private readonly script: (attempt: number) => AsyncIterable<StreamChunk>,
  ) {
    super();
  }

  override stream(options: GenerateOptions): AsyncIterable<StreamChunk> {
    this.requests.push(options);
    return this.script(this.requests.length - 1);
  }
}

async function* emit(
  chunks: readonly StreamChunk[],
): AsyncIterable<StreamChunk> {
  yield* chunks;
}

async function drain(
  stream: AsyncIterable<StreamChunk>,
): Promise<StreamChunk[]> {
  const chunks: StreamChunk[] = [];
  for await (const chunk of stream) chunks.push(chunk);
  return chunks;
}

interface WireGeneration {
  id: string;
  operation_name: string;
  system_prompt: string;
  stop_reason: string;
  call_error: string;
  usage: Record<string, string>;
  input: Array<{
    role: string;
    parts: Array<{
      text?: string;
      tool_result?: {
        tool_call_id: string;
        is_error: boolean;
        content: string;
      };
    }>;
  }>;
  output: Array<{
    parts: Array<{ text?: string; tool_call?: { name: string } }>;
  }>;
}

interface WireSpan {
  name: string;
  status?: { code?: number; message?: string };
  attributes: Array<{ key: string; value: { stringValue?: string } }>;
}

async function receiver() {
  const requests: Array<{ path: string; body: string }> = [];
  const generations: WireGeneration[] = [];
  const held = new Set<string>();
  const pending: Array<{ path: string; acknowledge: () => void }> = [];
  const server = createServer((req, res) => {
    const buffers: Buffer[] = [];
    req.on("data", (chunk: Buffer) => buffers.push(chunk));
    req.on("end", () => {
      const path = req.url ?? "";
      const body = Buffer.concat(buffers).toString("utf8");
      requests.push({ path, body });
      let reply = "{}";
      if (path === "/api/v1/generations:export") {
        const payload: { generations: WireGeneration[] } = JSON.parse(body);
        generations.push(...payload.generations);
        reply = JSON.stringify({
          results: payload.generations.map((generation) => ({
            generation_id: generation.id,
            accepted: true,
          })),
        });
      }
      const acknowledge = () => {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(reply);
      };
      if (held.has(path)) pending.push({ path, acknowledge });
      else acknowledge();
    });
  });
  server.on("clientError", (_error, socket) => socket.destroy());
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string")
    throw new Error("expected a loopback TCP listener");

  function release(path: string) {
    held.delete(path);
    for (const item of pending.filter((item) => item.path === path))
      item.acknowledge();
    for (let index = pending.length - 1; index >= 0; index--) {
      if (pending[index]?.path === path) pending.splice(index, 1);
    }
  }

  return {
    endpoint: `http://127.0.0.1:${address.port}`,
    requests,
    generations,
    held,
    release,
    spans(): WireSpan[] {
      return requests
        .filter((item) => item.path === "/v1/traces")
        .flatMap((item) => {
          const payload: {
            resourceSpans?: Array<{
              scopeSpans?: Array<{ spans?: WireSpan[] }>;
            }>;
          } = JSON.parse(item.body);
          return (payload.resourceSpans ?? []).flatMap((resource) =>
            (resource.scopeSpans ?? []).flatMap((scope) => scope.spans ?? []),
          );
        });
    },
    async close() {
      for (const path of [...held]) release(path);
      await new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
        server.closeAllConnections();
      });
    },
  };
}

function attributes(span: WireSpan): Record<string, string | undefined> {
  return Object.fromEntries(
    span.attributes.map((attribute) => [
      attribute.key,
      attribute.value.stringValue,
    ]),
  );
}

const GENERATIONS = "/api/v1/generations:export";
const TRACES = "/v1/traces";
const METRICS = "/v1/metrics";

function spyOnShutdown() {
  return {
    client: vi.spyOn(Agento11yClient.prototype, "shutdown"),
    exporter: vi.spyOn(HTTPGenerationExporter.prototype, "shutdown"),
    tracer: vi.spyOn(BasicTracerProvider.prototype, "shutdown"),
    meter: vi.spyOn(MeterProvider.prototype, "shutdown"),
  };
}

async function expectCompletedShutdown(
  spies: ReturnType<typeof spyOnShutdown>,
) {
  expect(spies.exporter).toHaveBeenCalledTimes(1);
  for (const spy of [spies.client, spies.tracer, spies.meter]) {
    expect(spy).toHaveBeenCalledTimes(1);
    await expect(spy.mock.results[0]?.value).resolves.toBeUndefined();
  }
}

async function checkpoint() {
  await new Promise<void>((resolve) => setTimeout(resolve, 25));
}

describe("installed Cordis and LlmRuntime compatibility", () => {
  let saved: Record<string, string | undefined>;
  let home: string;
  let local: Awaited<ReturnType<typeof receiver>>;
  let ctx: Context;
  const releases: Array<() => void> = [];

  beforeEach(async () => {
    saved = snapshotAndClearTestEnv();
    home = mkdtempSync(join(tmpdir(), "agento11y-dsh-host-"));
    process.env.HOME = home;
    process.env.USERPROFILE = home;
    process.env.XDG_CONFIG_HOME = join(home, ".config");
    local = await receiver();
    process.env.AGENTO11Y_ENDPOINT = local.endpoint;
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT = local.endpoint;
    process.env.AGENTO11Y_AUTH_TENANT_ID = "test-tenant";
    process.env.AGENTO11Y_AUTH_TOKEN = "test-token";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "false";
    ctx = new Context();
    await ctx.plugin(LlmRuntime);
  });

  afterEach(async () => {
    try {
      for (const release of releases.splice(0)) release();
      for (const path of [...local.held]) local.release(path);
      await ctx.fiber.dispose();
    } finally {
      vi.restoreAllMocks();
      await local.close();
      restoreEnv(saved);
      rmSync(home, { recursive: true, force: true });
    }
  });

  it("assigns upstream requests and every chunk variant directly to local mirrors", () => {
    const upstream: GenerateOptions = request();
    expect(mirrorOptions(upstream)).toBe(upstream);
    expect(Object.isFrozen(upstream.messages[0])).toBe(true);
    for (const chunk of turnChunks()) expect(mirrorChunk(chunk)).toBe(chunk);
  });

  it.each([
    "ordinary",
    "prepared",
  ] as const)("%s streams preserve identity and export exactly one generation per attempt", async (mode) => {
    const chunks = turnChunks();
    const failure = new LlmError(
      "scripted adapter exploded",
      "SCRIPTED_FAILURE",
    );
    const adapter = new ScriptedAdapter(async function* (attempt) {
      yield* chunks.slice(0, -1);
      if (attempt === 0) throw failure;
      yield chunks[chunks.length - 1]!;
    });
    ctx.llm.registerAdapter(["scripted"], adapter);
    await ctx.plugin(plugin);
    const boundary: StreamChunk[][] = [];
    ctx.on("llm/stream", async function* (_options, next) {
      const seen: StreamChunk[] = [];
      boundary.push(seen);
      for await (const chunk of next()) {
        seen.push(chunk);
        yield chunk;
      }
    });

    for (let attempt = 0; attempt < 2; attempt++) {
      const options = request();
      const prepared =
        mode === "prepared" ? await ctx.llm.prepareCall(options) : undefined;
      const stream = prepared
        ? prepared.stream({ ...options, ...prepared.config })
        : ctx.llm.stream(options);
      const got = await drain(stream);
      expect(got).toHaveLength(chunks.length);
      for (let index = 0; index < chunks.length - 1; index++)
        expect(got[index]).toBe(chunks[index]);
      expect(boundary[attempt]).toHaveLength(got.length);
      for (const [index, chunk] of got.entries())
        expect(chunk).toBe(boundary[attempt]?.[index]);
      if (attempt === 0) {
        expect(got.at(-1)).toMatchObject({
          type: "finish",
          reason: {
            kind: "error",
            failure: { message: failure.message, code: "SCRIPTED_FAILURE" },
          },
        });
      } else expect(got.at(-1)).toBe(chunks.at(-1));
    }

    await ctx.fiber.dispose();
    expect(adapter.requests).toHaveLength(2);
    expect(local.generations).toHaveLength(2);
    expect(
      new Set(local.generations.map((generation) => generation.id)).size,
    ).toBe(2);
    expect(
      local.generations.map((generation) => generation.stop_reason),
    ).toEqual(["error", "tool-calls"]);
    expect(
      local.generations.map((generation) => generation.call_error),
    ).toEqual([failure.message, ""]);
    for (const generation of local.generations) {
      expect(generation.operation_name).toBe("streamText");
      expect(generation.system_prompt).toBe(
        "one-shot prefix\n\nhost system prefix",
      );
      expect(generation.usage).toMatchObject({
        input_tokens: "10",
        output_tokens: "4",
        cache_read_input_tokens: "3",
        cache_write_input_tokens: "2",
        reasoning_tokens: "1",
        total_tokens: "19",
      });
      expect(generation.input.map((message) => message.role)).toEqual([
        "MESSAGE_ROLE_USER",
        "MESSAGE_ROLE_ASSISTANT",
        "MESSAGE_ROLE_TOOL",
      ]);
      expect(generation.input.at(-1)?.parts[0]?.tool_result).toMatchObject({
        tool_call_id: "prior-call",
        is_error: true,
        content: "file unavailable",
      });
      expect(generation.output[0]?.parts).toEqual(
        expect.arrayContaining([
          expect.objectContaining({ thinking: "Consider" }),
          expect.objectContaining({ text: "Hello" }),
        ]),
      );
      const toolCalls = generation.output
        .flatMap((message) => message.parts)
        .filter((part) => part.tool_call);
      expect(toolCalls).toHaveLength(
        generation.stop_reason === "error" ? 0 : 1,
      );
      if (generation.stop_reason !== "error") {
        expect(toolCalls[0]?.tool_call?.name).toBe("read_file");
      }
    }
  });

  it("preserves synthetic tool result, returned error, and thrown error identity through the real waterfall", async () => {
    await ctx.plugin(plugin);
    const results: ToolResult[] = [
      { isError: false, content: [{ type: "text", text: "file contents" }] },
      {
        isError: true,
        content: [{ type: "text", text: "cannot read file" }],
        error: { code: "READ", message: "read failed" },
      },
    ];
    const thrown = new Error("tool dispatch exploded");
    let calls = 0;
    for (const [index, result] of results.entries()) {
      expect(
        await ctx.waterfall(
          "tools/execute",
          {
            callId: `tool-${index}`,
            name: "read_file",
            arguments: { path: "owned.txt" },
          },
          async () => {
            calls++;
            return result;
          },
        ),
      ).toBe(result);
    }
    await expect(
      ctx.waterfall(
        "tools/execute",
        {
          callId: "tool-thrown",
          name: "read_file",
          arguments: {},
        },
        async () => {
          calls++;
          throw thrown;
        },
      ),
    ).rejects.toBe(thrown);
    await ctx.fiber.dispose();
    expect(calls).toBe(3);
    const spans = local
      .spans()
      .filter((span) => span.name === "execute_tool read_file");
    expect(spans).toHaveLength(3);
    expect(
      spans.map((span) => attributes(span)["gen_ai.tool.call.id"]),
    ).toEqual(["tool-0", "tool-1", "tool-thrown"]);
    expect(attributes(spans[0]!)["gen_ai.tool.call.result"]).toBe(
      JSON.stringify("file contents"),
    );
    expect(spans[1]?.status).toMatchObject({ code: 2, message: "read failed" });
    expect(spans[2]?.status).toMatchObject({
      code: 2,
      message: thrown.message,
    });
    expect(local.generations).toHaveLength(0);
  });

  it("removes listeners on unload and does not duplicate listeners or exports after reload", async () => {
    const chunks = turnChunks();
    const adapter = new ScriptedAdapter(() => emit(chunks));
    ctx.llm.registerAdapter(["scripted"], adapter);
    const execute = () =>
      ctx.waterfall(
        "tools/execute",
        {
          callId: "reload-tool",
          name: "read_file",
          arguments: {},
        },
        async () => ({ isError: false, content: [] }),
      );
    const listeners = () =>
      ["llm/stream", "tools/execute"].map(
        (event) => ctx.events._hooks[event]?.length ?? 0,
      );
    const baseline = listeners();
    const first = await ctx.plugin(plugin);
    expect(listeners()).toEqual(baseline.map((count) => count + 1));
    await drain(ctx.llm.stream(request()));
    await execute();
    await first.dispose();
    expect(listeners()).toEqual(baseline);
    expect(local.generations).toHaveLength(1);
    await drain(ctx.llm.stream(request()));
    await execute();
    await ctx.plugin(plugin);
    expect(listeners()).toEqual(baseline.map((count) => count + 1));
    await drain(ctx.llm.stream(request()));
    await execute();
    await ctx.fiber.dispose();
    expect(listeners()).toEqual([0, 0]);
    expect(adapter.requests).toHaveLength(3);
    expect(local.generations).toHaveLength(2);
    expect(
      local.spans().filter((span) => span.name === "execute_tool read_file"),
    ).toHaveLength(2);
  });

  it("root disposal drains an admitted stream and waits for OTLP before client shutdown", async () => {
    const shutdown = spyOnShutdown();
    local.held.add(GENERATIONS);
    local.held.add(TRACES);
    local.held.add(METRICS);
    const admitted = deferred();
    const finish = deferred();
    releases.push(finish.resolve);
    const adapter = new ScriptedAdapter(async function* () {
      admitted.resolve();
      await finish.promise;
      yield* turnChunks();
    });
    ctx.llm.registerAdapter(["scripted"], adapter);
    await ctx.plugin(plugin);
    const before = local.requests.length;
    const running = drain(ctx.llm.stream(request()));
    await admitted.promise;
    let disposed = false;
    const disposal = ctx.fiber.dispose().then(() => {
      disposed = true;
    });
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.client).not.toHaveBeenCalled();
    expect(shutdown.tracer).not.toHaveBeenCalled();
    expect(shutdown.meter).not.toHaveBeenCalled();

    finish.resolve();
    await running;
    await vi.waitFor(() => {
      for (const path of [TRACES, METRICS]) {
        expect(
          local.requests.slice(before).some((item) => item.path === path),
        ).toBe(true);
      }
    });
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.client).not.toHaveBeenCalled();
    local.release(METRICS);
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.client).not.toHaveBeenCalled();
    expect(shutdown.tracer).not.toHaveBeenCalled();
    expect(shutdown.meter).not.toHaveBeenCalled();
    local.release(TRACES);

    await vi.waitFor(() =>
      expect(local.requests.some((item) => item.path === GENERATIONS)).toBe(
        true,
      ),
    );
    await vi.waitFor(() => expect(shutdown.client).toHaveBeenCalledTimes(1));
    // Client shutdown flushes generations. Exporter and provider shutdown must wait for the acknowledgment.
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.exporter).not.toHaveBeenCalled();
    expect(shutdown.tracer).not.toHaveBeenCalled();
    expect(shutdown.meter).not.toHaveBeenCalled();

    local.release(GENERATIONS);
    await vi.waitFor(() => {
      expect(shutdown.exporter).toHaveBeenCalledTimes(1);
      expect(shutdown.tracer).toHaveBeenCalledTimes(1);
      expect(shutdown.meter).toHaveBeenCalledTimes(1);
    });
    await disposal;
    expect(disposed).toBe(true);
    await expectCompletedShutdown(shutdown);
    expect(local.generations).toHaveLength(1);
    expect(
      local.spans().filter((span) => span.name === "execute_tool read_file"),
    ).toHaveLength(0);
  });

  it("root disposal drains an admitted tool and waits for OTLP during provider shutdown", async () => {
    const shutdown = spyOnShutdown();
    const tracerFlush = vi.spyOn(BasicTracerProvider.prototype, "forceFlush");
    const meterFlush = vi.spyOn(MeterProvider.prototype, "forceFlush");
    local.held.add(GENERATIONS);
    const adapter = new ScriptedAdapter(() => emit(turnChunks()));
    ctx.llm.registerAdapter(["scripted"], adapter);
    await ctx.plugin(plugin);
    // Tool spans have no generation export. Keep one real generation queued for shutdown.
    await drain(ctx.llm.stream(request()));
    await vi.waitFor(() => {
      expect(tracerFlush).toHaveBeenCalledTimes(1);
      expect(meterFlush).toHaveBeenCalledTimes(1);
    });
    await tracerFlush.mock.results[0]?.value;
    await meterFlush.mock.results[0]?.value;

    local.held.add(TRACES);
    local.held.add(METRICS);
    const before = local.requests.length;
    const admitted = deferred();
    const finish = deferred();
    releases.push(finish.resolve);
    const outcome: ToolResult = {
      isError: false,
      content: [{ type: "text", text: "done" }],
    };
    const running = ctx.waterfall(
      "tools/execute",
      {
        callId: "delayed-tool",
        name: "read_file",
        arguments: {},
      },
      async () => {
        admitted.resolve();
        await finish.promise;
        return outcome;
      },
    );
    await admitted.promise;
    let disposed = false;
    const disposal = ctx.fiber.dispose().then(() => {
      disposed = true;
    });
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.client).not.toHaveBeenCalled();
    expect(shutdown.tracer).not.toHaveBeenCalled();
    expect(shutdown.meter).not.toHaveBeenCalled();

    finish.resolve();
    expect(await running).toBe(outcome);
    await vi.waitFor(() =>
      expect(local.requests.some((item) => item.path === GENERATIONS)).toBe(
        true,
      ),
    );
    await vi.waitFor(() => expect(shutdown.client).toHaveBeenCalledTimes(1));
    await checkpoint();
    expect(disposed).toBe(false);
    expect(shutdown.exporter).not.toHaveBeenCalled();
    expect(shutdown.tracer).not.toHaveBeenCalled();
    expect(shutdown.meter).not.toHaveBeenCalled();

    local.release(GENERATIONS);
    await vi.waitFor(() => {
      expect(shutdown.exporter).toHaveBeenCalledTimes(1);
      expect(shutdown.tracer).toHaveBeenCalledTimes(1);
      expect(shutdown.meter).toHaveBeenCalledTimes(1);
    });
    await vi.waitFor(() => {
      for (const path of [TRACES, METRICS]) {
        expect(
          local.requests.slice(before).some((item) => item.path === path),
        ).toBe(true);
      }
    });
    await checkpoint();
    expect(disposed).toBe(false);
    local.release(METRICS);
    await checkpoint();
    expect(disposed).toBe(false);
    local.release(TRACES);

    await disposal;
    expect(disposed).toBe(true);
    await expectCompletedShutdown(shutdown);
    expect(local.generations).toHaveLength(1);
    expect(
      local.spans().filter((span) => span.name === "execute_tool read_file"),
    ).toHaveLength(1);
  });
});
