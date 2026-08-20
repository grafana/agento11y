import { mkdtempSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Agento11yClient } from "@grafana/agento11y";
import { metrics, trace } from "@opentelemetry/api";
import {
  AggregationTemporality,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BasicTracerProvider,
  InMemorySpanExporter,
  SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as configModule from "./config.js";
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
    for (const dispose of ctx.disposers) await dispose();
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
    for (const dispose of ctx.disposers) await dispose();
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
    for (const dispose of ctx.disposers) await dispose();
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
    for (const dispose of ctx.disposers) await dispose();
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
  body: Record<string, any>;
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
        } catch {}
        captures.push({
          path: req.url ?? "",
          body: parsed,
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

describe("dsh plugin: OTel-only capture", () => {
  let env: Awaited<ReturnType<typeof startExportServer>>;
  let saved: Record<string, string | undefined>;
  let ctx: FakeDsh;

  beforeEach(async () => {
    saved = snapshotAndClearTestEnv();
    const home = mkdtempSync(join(tmpdir(), "agento11y-dsh-otel-"));
    process.env.HOME = home;
    process.env.USERPROFILE = home;
    process.env.XDG_CONFIG_HOME = join(home, ".config");
    env = await startExportServer();
    ctx = new FakeDsh();
  });

  afterEach(async () => {
    for (const dispose of ctx.disposers) await dispose();
    vi.restoreAllMocks();
    metrics.disable();
    trace.disable();
    await closeServer(env.server);
    restoreEnv(saved);
  });

  async function runCalls() {
    apply(ctx);
    expect(
      await drain(ctx.stream!(TURN_REQUEST, chunksFrom(TURN_CHUNKS))),
    ).toEqual(TURN_CHUNKS);
    const outcome = { isError: false, content: [] };
    expect(
      await ctx.tool!(
        { callId: "call-1", name: "read_file", arguments: {} },
        async () => outcome,
      ),
    ).toBe(outcome);
    for (const dispose of ctx.disposers) await dispose();
  }

  it.each([
    {
      label: "no generation settings",
      endpoint: false,
      tenant: false,
      token: false,
    },
    {
      label: "credentials without an endpoint",
      endpoint: false,
      tenant: true,
      token: true,
    },
    {
      label: "endpoint without credentials",
      endpoint: true,
      tenant: false,
      token: false,
    },
    { label: "missing token", endpoint: true, tenant: true, token: false },
    { label: "missing tenant", endpoint: true, tenant: false, token: true },
  ])("uses host providers with $label", async ({ endpoint, tenant, token }) => {
    if (endpoint) process.env.AGENTO11Y_ENDPOINT = env.baseUrl;
    if (tenant) process.env.AGENTO11Y_AUTH_TENANT_ID = "tenant";
    if (token) process.env.AGENTO11Y_AUTH_TOKEN = "token";

    const spans = new InMemorySpanExporter();
    const tracerProvider = new BasicTracerProvider({
      spanProcessors: [new SimpleSpanProcessor(spans)],
    });
    const metricExporter = new InMemoryMetricExporter(
      AggregationTemporality.CUMULATIVE,
    );
    const meterProvider = new MeterProvider({
      readers: [
        new PeriodicExportingMetricReader({ exporter: metricExporter }),
      ],
    });
    trace.setGlobalTracerProvider(tracerProvider);
    metrics.setGlobalMeterProvider(meterProvider);
    const tracerShutdown = vi.spyOn(tracerProvider, "shutdown");
    const meterShutdown = vi.spyOn(meterProvider, "shutdown");
    const tracerFlush = vi.spyOn(tracerProvider, "forceFlush");
    const meterFlush = vi.spyOn(meterProvider, "forceFlush");
    try {
      await runCalls();
      expect(tracerFlush).not.toHaveBeenCalled();
      expect(meterFlush).not.toHaveBeenCalled();
      await tracerProvider.forceFlush();
      await meterProvider.forceFlush();

      expect(
        spans
          .getFinishedSpans()
          .map((span) => span.attributes["gen_ai.operation.name"]),
      ).toEqual(expect.arrayContaining(["streamText", "execute_tool"]));
      const metricNames = metricExporter
        .getMetrics()
        .flatMap((resource) =>
          resource.scopeMetrics.flatMap((scope) =>
            scope.metrics.map((metric) => metric.descriptor.name),
          ),
        );
      expect(metricNames).toEqual(
        expect.arrayContaining([
          "gen_ai.client.operation.duration",
          "gen_ai.client.token.usage",
        ]),
      );
      expect(env.captures).toEqual([]);
      expect(tracerShutdown).not.toHaveBeenCalled();
      expect(meterShutdown).not.toHaveBeenCalled();
    } finally {
      await tracerProvider.shutdown();
      await meterProvider.shutdown();
    }
  });

  it.each([
    "OTEL_EXPORTER_OTLP_ENDPOINT",
    "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT",
    "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
  ])("exports traces and metrics with only %s", async (key) => {
    process.env[key] = `${env.baseUrl}/otlp`;
    await runCalls();

    expect(env.captures.map((capture) => capture.path)).toEqual(
      expect.arrayContaining(["/otlp/v1/traces", "/otlp/v1/metrics"]),
    );
    const spans = env.captures.flatMap((capture) =>
      (capture.body.resourceSpans ?? []).flatMap((resource: any) =>
        resource.scopeSpans.flatMap((scope: any) => scope.spans),
      ),
    );
    expect(spans).toHaveLength(2);
    const metricNames = env.captures.flatMap((capture) =>
      (capture.body.resourceMetrics ?? []).flatMap((resource: any) =>
        resource.scopeMetrics.flatMap((scope: any) =>
          scope.metrics.map((metric: any) => metric.name),
        ),
      ),
    );
    expect(metricNames).toContain("gen_ai.client.token.usage");
    expect(
      env.captures.every((capture) => capture.path.startsWith("/otlp/")),
    ).toBe(true);
  });
});

describe("dsh plugin: export against a local endpoint", () => {
  let env: Awaited<ReturnType<typeof startExportServer>>;
  let saved: Record<string, string | undefined> = {};
  let firstTokenAts: Date[] = [];
  // conversationTitle has no dedicated export-proto field;
  // the SDK includes it in span attributes and export metadata.
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
    // Protobuf JSON encodes int64 fields as strings.
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

  it("owns mapped request values before delayed initialization resumes", async () => {
    const initialize = deferred<void>();
    const loadConfig = configModule.loadConfig;
    vi.spyOn(configModule, "loadConfig").mockImplementation(async () => {
      await initialize.promise;
      return loadConfig();
    });
    const system = { type: "text", text: "leading policy" };
    const user = { type: "text", text: "original question" };
    const schema = { type: "object", properties: { path: { type: "string" } } };
    const tool = {
      name: "read_file",
      description: "Original description",
      parameters: schema,
    };
    const controller = new AbortController();
    const request = {
      ...TURN_REQUEST,
      provider: "original-provider",
      model: "original-model",
      system: "original policy",
      temperature: 0.25,
      maxTokens: 1234,
      tools: [tool],
      messages: [
        { role: "system", content: [system] },
        { role: "user", content: [user] },
      ],
      signal: controller.signal,
      replayMetadata: { uncloneable: () => {} },
    };
    const before = {
      ...request,
      tools: structuredClone(request.tools),
      messages: structuredClone(request.messages),
    };
    const ctx = new FakeDsh();
    apply(ctx);

    try {
      const seen = await drain(ctx.stream!(request, chunksFrom(TURN_CHUNKS)));
      for (const [index, chunk] of TURN_CHUNKS.entries()) {
        expect(seen[index]).toBe(chunk);
      }
      expect(request).toEqual(before);
      expect(seeds).toHaveLength(0);

      request.provider = "changed-provider";
      request.model = "changed-model";
      request.system = "changed policy";
      request.temperature = 0.9;
      request.maxTokens = 99;
      system.text = "changed leading policy";
      user.text = "changed question";
      request.messages.push({
        role: "user",
        content: [{ type: "text", text: "extra" }],
      });
      schema.properties.path.type = "number";
      tool.name = "changed_tool";
      tool.description = "Changed description";
      request.tools.push({
        name: "extra",
        description: "Extra",
        parameters: schema,
      });
    } finally {
      initialize.resolve(undefined);
      await ctx.disposers[0]!();
    }

    const generation = env.captures.flatMap(
      (capture) => capture.generations,
    )[0]!;
    expect(generation.model).toEqual({
      provider: "original-provider",
      name: "original-model",
    });
    expect(generation.response_model).toBe("original-model");
    expect(generation.system_prompt).toBe("original policy\n\nleading policy");
    expect(generation.input).toHaveLength(1);
    expect(generation.input[0].parts[0].text).toBe("original question");
    expect(seeds[0]).toMatchObject({ temperature: 0.25, maxTokens: 1234 });
    expect(seeds[0]?.tools).toHaveLength(1);
    expect(seeds[0]?.tools[0]).toMatchObject({
      name: "read_file",
      description: "Original description",
    });
    expect(JSON.parse(seeds[0]?.tools[0].inputSchemaJSON)).toEqual({
      type: "object",
      properties: { path: { type: "string" } },
    });
    expect(request.signal).toBe(controller.signal);
    expect(request.replayMetadata.uncloneable).toBe(
      before.replayMetadata.uncloneable,
    );
    expect(user.text).toBe("changed question");
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

  it("combines the leading system prefix without duplicating input", async () => {
    const ctx = new FakeDsh();
    apply(ctx);
    const generation = await runCall(
      ctx,
      {
        ...TURN_REQUEST,
        system: "base",
        messages: [
          { role: "system", content: [{ type: "text", text: "lead" }] },
          {
            role: "system",
            content: [
              { type: "text", text: "ne" },
              { type: "text", text: "xt" },
            ],
          },
          { role: "user", content: [{ type: "text", text: "question" }] },
        ],
      },
      TURN_CHUNKS,
    );

    expect(generation.system_prompt).toBe("base\n\nlead\n\nnext");
    expect(generation.input?.map((message: any) => message.role)).toEqual([
      "MESSAGE_ROLE_USER",
    ]);
    expect(generation.input?.[0]?.parts?.[0]?.text).toBe("question");
  });

  it.each([
    "full",
    "no_tool_content",
    "metadata_only",
  ])("preserves current tool results under %s capture", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    const secret = "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";
    const ctx = new FakeDsh();
    apply(ctx);
    const generation = await runCall(
      ctx,
      {
        ...TURN_REQUEST,
        messages: [
          {
            role: "tool",
            toolCallId: "call-1",
            isError: true,
            content: [{ type: "text", text: `failed ${secret}` }],
          },
        ],
      },
      TURN_CHUNKS,
    );

    expect(generation.input).toHaveLength(1);
    expect(generation.input[0].role).toBe("MESSAGE_ROLE_TOOL");
    const result = generation.input[0].parts[0].tool_result;
    expect(result).toMatchObject({ tool_call_id: "call-1", is_error: true });
    expect(result.content).not.toContain(secret);
    if (mode === "metadata_only") expect(result.content).toBe("");
    else expect(result.content).toContain("[REDACTED:");
    await ctx.disposers[0]!();
  });

  it("fully redacts later system and developer text even with input redaction disabled", async () => {
    process.env.AGENTO11Y_REDACT_INPUT_MESSAGES = "false";
    const ctx = new FakeDsh();
    apply(ctx);
    const generation = await runCall(
      ctx,
      {
        ...TURN_REQUEST,
        messages: [
          { role: "user", content: [{ type: "text", text: "TOKEN=human" }] },
          {
            role: "system",
            content: [
              { type: "text", text: "TOKEN=system" },
              { type: "tool-addition" },
              { type: "text", text: "after update" },
            ],
          },
          { role: "system", content: [] },
          {
            role: "developer",
            content: [
              { type: "tool-removal" },
              { type: "text", text: "TOKEN=developer" },
            ],
          },
        ],
      },
      TURN_CHUNKS,
    );

    expect(generation.input).toHaveLength(3);
    expect(generation.input[0].parts[0].text).toBe("TOKEN=human");
    expect(generation.input[1].parts.map((part: any) => part.text)).toEqual([
      "TOKEN=[REDACTED:env-secret-value]",
      "after update",
    ]);
    expect(generation.input[2].parts[0].text).toBe(
      "TOKEN=[REDACTED:env-secret-value]",
    );
    await ctx.disposers[0]!();
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

  it.each([
    "full",
    "no_tool_content",
  ])("exports oversized tool content under %s capture", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    const ctx = new FakeDsh();
    apply(ctx);
    const argumentsJSON = JSON.stringify({ text: "x".repeat(10_000) });
    const content = "\0".repeat(750_000);
    const call = {
      type: "tool-call",
      id: "call-large",
      name: "read_file",
      arguments: argumentsJSON,
    };
    const request = {
      ...TURN_REQUEST,
      messages: [
        { role: "assistant", content: [call] },
        {
          role: "tool",
          toolCallId: "call-large",
          content: [{ type: "text", text: content }],
        },
      ],
    };
    const generation = await runCall(ctx, request, [
      { type: "block-start", index: 0, blockType: "tool-call" },
      { type: "block-end", index: 0, block: call },
      { type: "finish", reason: { kind: "tool-calls" } },
    ]);
    const result = generation.input[1].parts[0].tool_result.content;
    expect(result).toContain("[truncated]");
    expect(Buffer.byteLength(JSON.stringify(result))).toBeLessThanOrEqual(
      (1 << 20) + '" [truncated]"'.length,
    );
    for (const message of [generation.input[0], generation.output[0]]) {
      const input = Buffer.from(
        message.parts[0].tool_call.input_json,
        "base64",
      ).toString("utf8");
      expect(() => JSON.parse(input)).not.toThrow();
      expect(input).toContain("[truncated]");
      expect(Buffer.byteLength(input)).toBeLessThan(5_000);
    }
    expect(request.messages[0]?.content[0]).toBe(call);
    expect(call.arguments).toBe(argumentsJSON);
    expect(request.messages[1]?.content[0]).toEqual({
      type: "text",
      text: content,
    });
    for (const dispose of ctx.disposers) await dispose();
  });

  it.each([
    false,
    true,
  ])("redacts tool bodies (oversized=%s)", async (oversized) => {
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

    const suffix = oversized ? "\0".repeat(750_000) : "";
    const args = { command: `echo ${token} ${suffix}` };
    const outcome = {
      isError: false,
      content: [{ type: "text", text: `wrote ${token} ${suffix}` }],
    };
    const got = await ctx.tool!(
      {
        callId: "call-1",
        name: "bash",
        arguments: args,
      },
      async () => outcome,
    );

    await vi.waitFor(() => expect(results).toHaveLength(1));
    expect(String(results[0]?.arguments)).not.toContain(token);
    expect(String(results[0]?.result)).not.toContain(token);
    expect(() => JSON.parse(results[0]?.arguments)).not.toThrow();
    expect(Buffer.byteLength(results[0]?.arguments)).toBeLessThan(5_000);
    expect(
      Buffer.byteLength(JSON.stringify(results[0]?.result)),
    ).toBeLessThanOrEqual((1 << 20) + '" [truncated]"'.length);
    expect(got).toBe(outcome);
    expect(args.command).toBe(`echo ${token} ${suffix}`);
    if (oversized) {
      expect(results[0]?.arguments).toContain("[truncated]");
      expect(results[0]?.result).toContain("[truncated]");
    }
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
    // Protobuf JSON encodes the input_json bytes as base64.
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
