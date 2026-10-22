import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import type { ContentCaptureMode } from "@grafana/agento11y";
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

const { telemetryFactory } = vi.hoisted(() => ({ telemetryFactory: vi.fn() }));
// Only provider construction is replaced. Spans, metrics, recorders, config,
// sanitization, and generation export use the actual SDK implementations.
vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: telemetryFactory,
}));

import registerExtension from "./index.js";
import type { PiAssistantMessage, PiToolResult } from "./mappers.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";

class SyntheticPi {
  handlers = new Map<string, (event: any, ctx: any) => any>();
  getAllTools() {
    return [{ name: "read" }, { name: "bash" }];
  }
  getActiveTools() {
    return ["read", "bash"];
  }
  on(event: string, handler: (event: any, ctx: any) => any) {
    this.handlers.set(event, handler);
  }
  async emit(event: string, payload: any, ctx: any) {
    return this.handlers.get(event)?.(payload, ctx);
  }
}

const MODES: ContentCaptureMode[] = [
  "full",
  "full_with_metadata_spans",
  "no_tool_content",
  "metadata_only",
];

describe("pi skill executions: actual SDK telemetry", () => {
  let savedEnv: Record<string, string | undefined>;
  let cwd: string;
  let server: Server;
  let endpoint: string;
  let generations: any[];
  let spans: InMemorySpanExporter;
  let metrics: InMemoryMetricExporter;
  let tracerProvider: BasicTracerProvider;
  let meterProvider: MeterProvider;
  let pi: SyntheticPi;
  let ctx: any;

  beforeEach(async () => {
    savedEnv = snapshotAndClearTestEnv();
    cwd = mkdtempSync(join(tmpdir(), "pi-skill-otlp-"));
    generations = [];
    server = createServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => {
        raw += chunk;
      });
      req.on("end", () => {
        const body = JSON.parse(raw);
        if (req.url === "/api/v1/hooks:evaluate") {
          const call = body.input.output[0].parts[0].tool_call;
          const path =
            call.id === "transform-out" || call.id === "blocked-later-in"
              ? "ordinary.md"
              : "missing.md";
          res.setHeader("Content-Type", "application/json");
          res.end(
            JSON.stringify(
              call.id === "blocked"
                ? { action: "deny", reason: "blocked", evaluations: [] }
                : {
                    action: "allow",
                    evaluations: [],
                    transformed_input: {
                      output: [
                        {
                          role: "assistant",
                          parts: [
                            {
                              type: "tool_call",
                              toolCall: {
                                id: call.id,
                                name: "read",
                                input_json: JSON.stringify({ path }),
                              },
                            },
                          ],
                        },
                      ],
                    },
                  },
            ),
          );
          return;
        }
        expect(req.url).toBe("/api/v1/generations:export");
        generations.push(...body.generations);
        res.setHeader("Content-Type", "application/json");
        res.end(
          JSON.stringify({
            results: body.generations.map((g: any) => ({
              generation_id: g.id,
              accepted: true,
            })),
          }),
        );
      });
    });
    await new Promise<void>((resolve) =>
      server.listen(0, "127.0.0.1", resolve),
    );
    const address = server.address();
    if (!address || typeof address === "string")
      throw new Error("missing loopback address");
    endpoint = `http://127.0.0.1:${address.port}`;
    Object.assign(process.env, {
      HOME: cwd,
      USERPROFILE: cwd,
      XDG_CONFIG_HOME: join(cwd, ".config"),
      AGENTO11Y_ENDPOINT: endpoint,
      AGENTO11Y_AUTH_TENANT_ID: "tenant",
      AGENTO11Y_AUTH_TOKEN: "test-token",
      AGENTO11Y_AGENT_NAME: "pi",
      AGENTO11Y_AGENT_VERSION: "test-version",
      AGENTO11Y_GUARDS_ENABLED: "false",
      AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT: endpoint,
    });
    spans = new InMemorySpanExporter();
    metrics = new InMemoryMetricExporter(AggregationTemporality.DELTA);
    tracerProvider = new BasicTracerProvider({
      spanProcessors: [new SimpleSpanProcessor(spans)],
    });
    meterProvider = new MeterProvider({
      readers: [
        new PeriodicExportingMetricReader({
          exporter: metrics,
          exportIntervalMillis: 60_000,
        }),
      ],
    });
    telemetryFactory.mockReturnValue({
      tracer: tracerProvider.getTracer("pi-skill-test"),
      meter: meterProvider.getMeter("pi-skill-test"),
      forceFlush: async () => {
        await Promise.all([
          tracerProvider.forceFlush(),
          meterProvider.forceFlush(),
        ]);
      },
      // Keep exporters alive for assertions after session_shutdown flushes the
      // SDK's generation queue. afterEach owns provider shutdown.
      shutdown: async () => {
        await Promise.all([
          tracerProvider.forceFlush(),
          meterProvider.forceFlush(),
        ]);
      },
    });
    pi = new SyntheticPi();
    registerExtension(pi as any);
    ctx = { cwd, sessionManager: { getSessionId: () => "skill-conversation" } };
  });

  afterEach(async () => {
    await pi.emit("session_shutdown", {}, ctx);
    await Promise.all([tracerProvider.shutdown(), meterProvider.shutdown()]);
    await new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    );
    rmSync(cwd, { recursive: true, force: true });
    restoreEnv(savedEnv);
  });

  it.each(
    MODES,
  )("retains failed skill loads and their generation parent in %s", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    await pi.emit("session_start", {}, ctx);
    const secret = "glc_abcdefghijklmnopqrstuvwxyz";
    const description = "private catalog description must not escape";
    const sourceURI = "https://private.invalid/catalog/source";
    const options = {
      skills: [
        {
          name: "wrong-before-later-handler",
          filePath: join(cwd, "old.md"),
          description,
          sourceURI,
        },
      ],
    };
    await pi.emit("before_agent_start", { systemPromptOptions: options }, ctx);
    options.skills = [
      {
        name: " org:review ",
        filePath: join(cwd, "missing.md"),
        description,
        sourceURI,
      },
      {
        name: "standalone",
        filePath: join(cwd, "guide.md"),
        description,
        sourceURI,
      },
      {
        name: secret,
        filePath: join(cwd, "secret.md"),
        description,
        sourceURI,
      },
    ];
    await pi.emit("agent_start", {}, ctx);
    options.skills[0]!.name = "wrong-after-snapshot";
    await pi.emit("turn_start", {}, ctx);
    const calls = [
      { id: "failed-load", name: "read", arguments: { path: "missing.md" } },
      { id: "standalone-load", name: "read", arguments: { path: "guide.md" } },
      { id: "ordinary-read", name: "read", arguments: { path: "ordinary.md" } },
      { id: "secret-name", name: "read", arguments: { path: "secret.md" } },
      {
        id: "resource-read",
        name: "read",
        arguments: { path: "guide.md/resource.md" },
      },
      {
        id: "command",
        name: "bash",
        arguments: { command: "echo missing.md" },
      },
      { id: "missing-start", name: "read", arguments: { path: "missing.md" } },
      { id: "missing-end", name: "read", arguments: { path: "missing.md" } },
    ];
    const message: PiAssistantMessage = {
      role: "assistant",
      content: calls.map((call) => ({ type: "toolCall", ...call })),
      provider: "anthropic",
      model: "test-model",
      stopReason: "toolUse",
      timestamp: Date.now(),
      usage: {
        input: 10,
        output: 20,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 30,
      },
    };
    await pi.emit("message_end", { message }, ctx);
    for (const call of calls) {
      if (call.id !== "missing-start")
        await pi.emit(
          "tool_execution_start",
          { toolCallId: call.id, toolName: call.name, args: call.arguments },
          ctx,
        );
      await pi.emit(
        "tool_call",
        { toolCallId: call.id, toolName: call.name, input: call.arguments },
        ctx,
      );
    }
    const results: PiToolResult[] = [];
    // Sibling completions may reverse the assistant's call order.
    for (const call of [...calls].reverse()) {
      const isError = call.id === "failed-load";
      const result: PiToolResult = {
        role: "toolResult",
        toolCallId: call.id,
        toolName: call.name,
        content: [{ type: "text", text: isError ? "ENOENT" : "ok" }],
        isError,
        timestamp: Date.now(),
      };
      results.push(result);
      await pi.emit("tool_result", { ...result, input: call.arguments }, ctx);
      if (call.id !== "missing-end")
        await pi.emit("tool_execution_end", result, ctx);
    }
    await pi.emit("turn_end", { message, toolResults: results }, ctx);
    await pi.emit("session_shutdown", {}, ctx);
    await Promise.all([
      tracerProvider.forceFlush(),
      meterProvider.forceFlush(),
    ]);

    const exported = spans.getFinishedSpans();
    expect(exported).toHaveLength(7);
    const generation = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "streamText",
    )!;
    expect(generation).toBeDefined();
    const tools = exported.filter(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(6);
    expect(
      tools.map((span) => span.attributes["gen_ai.tool.call.id"]).sort(),
    ).toEqual(
      calls
        .slice(0, 6)
        .map((call) => call.id)
        .sort(),
    );
    for (const span of tools) {
      expect(span.spanContext().traceId).toBe(generation.spanContext().traceId);
      expect(span.parentSpanContext?.spanId).toBe(
        generation.spanContext().spanId,
      );
      const call = calls.find(
        (call) => call.id === span.attributes["gen_ai.tool.call.id"],
      )!;
      expect(span.attributes["gen_ai.tool.name"]).toBe(call.name);
      const skill =
        call.id === "failed-load"
          ? "org:review"
          : call.id === "standalone-load"
            ? "standalone"
            : undefined;
      expect(span.attributes["gen_ai.skill.name"]).toBe(skill);
      expect(span.name).toBe(
        `execute_tool ${call.name}${skill ? ` ${skill}` : ""}`,
      );
      expect(span.status.code).toBe(call.id === "failed-load" ? 2 : 1);
      expect(span.attributes["gen_ai.tool.call.arguments"] !== undefined).toBe(
        mode === "full",
      );
      expect(span.attributes["gen_ai.tool.call.result"] !== undefined).toBe(
        mode === "full",
      );
      expect(
        Object.keys(span.attributes).filter((key) =>
          key.startsWith("gen_ai.skill."),
        ),
      ).toEqual(skill ? ["gen_ai.skill.name"] : []);
    }
    const structuralTelemetry = JSON.stringify(
      exported.map((span) => ({
        name: span.name,
        attributes: span.attributes,
      })),
    );
    for (const privateValue of [
      secret,
      description,
      sourceURI,
      join(cwd, "missing.md"),
      join(cwd, "guide.md"),
      "wrong-after-snapshot",
      "wrong-before-later-handler",
    ]) {
      expect(structuralTelemetry).not.toContain(privateValue);
    }
    const metricBatches = metrics.getMetrics();
    expect(metricBatches.length).toBeGreaterThan(0);
    const dimensions = metricBatches.flatMap((batch) =>
      batch.scopeMetrics.flatMap((scope) =>
        scope.metrics.flatMap((metric) =>
          metric.dataPoints.map((point) => point.attributes),
        ),
      ),
    );
    expect(dimensions.length).toBeGreaterThan(0);
    for (const attrs of dimensions) {
      expect(
        Object.keys(attrs).some(
          (key) => key.includes("skill") || key.includes("execution_kind"),
        ),
      ).toBe(false);
      expect(JSON.stringify(attrs)).not.toMatch(
        /org:review|standalone|missing\.md|guide\.md|private\.invalid/,
      );
    }
    expect(generations).toHaveLength(1);
    expect(generations[0].trace_id).toBe(generation.spanContext().traceId);
    expect(generations[0].span_id).toBe(generation.spanContext().spanId);
    const serializedParts = generations[0].output.flatMap(
      (output: any) => output.parts,
    );
    const toolCalls = serializedParts
      .filter((part: any) => part.tool_call)
      .map((part: any) => part.tool_call);
    const toolResults = serializedParts
      .filter((part: any) => part.tool_result)
      .map((part: any) => part.tool_result);
    expect(toolCalls.map((call: any) => [call.id, call.name])).toEqual(
      calls.map((call) => [call.id, call.name]),
    );
    expect(
      toolResults.map((result: any) => [result.tool_call_id, result.name]),
    ).toEqual(results.map((result) => [result.toolCallId, result.toolName]));
  });

  it("does not reuse temporary skills in a native custom-message run after settlement", async () => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "metadata_only";
    writeFileSync(join(cwd, "guide.md"), "instructions");
    writeFileSync(join(cwd, "ordinary.md"), "ordinary");
    const pin = realpathSync(
      fileURLToPath(
        new URL(
          "../node_modules/@mariozechner/pi-coding-agent",
          import.meta.url,
        ),
      ),
    );
    const native = (path: string) =>
      import(/* @vite-ignore */ pathToFileURL(join(pin, path)).href);
    const { AgentSession } = await native("dist/core/agent-session.js");
    const { Agent } = await native("../pi-agent-core/dist/agent.js");
    const { SessionManager } = await native("dist/core/session-manager.js");
    const { SettingsManager } = await native("dist/core/settings-manager.js");
    const { createEventBus } = await native("dist/core/event-bus.js");
    const { createExtensionRuntime, loadExtensionFromFactory } = await native(
      "dist/core/extensions/loader.js",
    );
    const { getCurrentSystemMessage } = await native("../pi-ai/dist/index.js");
    const runtime = createExtensionRuntime();
    const bus = createEventBus();
    let beforeStarts = 0;
    const telemetry = await loadExtensionFromFactory(
      registerExtension,
      cwd,
      bus,
      runtime,
      "telemetry",
    );
    const injector = await loadExtensionFromFactory(
      (api: any) => {
        api.on("before_agent_start", (event: any) => {
          beforeStarts++;
          event.systemPromptOptions.skills = [
            {
              name: "temporary",
              description: "temporary instructions",
              filePath: join(cwd, "guide.md"),
              baseDir: cwd,
            },
          ];
        });
      },
      cwd,
      bus,
      runtime,
      "injector",
    );
    const resourceLoader = {
      getExtensions: () => ({
        extensions: [telemetry, injector],
        errors: [],
        runtime,
      }),
      getSkills: () => ({ skills: [], diagnostics: [] }),
      getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }),
      getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => undefined,
      getAppendSystemPrompt: () => [],
      extendResources: () => {},
      reload: async () => {},
    };
    const model = {
      id: "local",
      provider: "synthetic",
      api: "synthetic",
      name: "local",
      input: ["text"],
      reasoning: false,
      contextWindow: 100_000,
      maxTokens: 1000,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    };
    const requests: any[] = [];
    const agent = new Agent({
      initialState: { model, thinkingLevel: "off", tools: [], messages: [] },
      streamFn: async (_model: unknown, context: any) => {
        requests.push(getCurrentSystemMessage(context.messages)?.sections);
        const turn = requests.length;
        const call = turn === 2 || turn === 3;
        const message = {
          role: "assistant",
          provider: "synthetic",
          model: "local",
          api: "synthetic",
          timestamp: Date.now(),
          stopReason: call ? "toolUse" : "stop",
          content: call
            ? [
                {
                  type: "toolCall",
                  id: turn === 2 ? "ordinary" : "stale-skill",
                  name: "read",
                  arguments: { path: turn === 2 ? "ordinary.md" : "guide.md" },
                },
              ]
            : [{ type: "text", text: "done" }],
          usage: {
            input: 1,
            output: 1,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 2,
            cost: {
              input: 0,
              output: 0,
              cacheRead: 0,
              cacheWrite: 0,
              total: 0,
            },
          },
        };
        return {
          async *[Symbol.asyncIterator]() {
            yield { type: "done", reason: message.stopReason, message };
          },
          async result() {
            return message;
          },
        };
      },
    });
    const session = new AgentSession({
      agent,
      cwd,
      resourceLoader,
      sessionManager: SessionManager.inMemory(cwd),
      settingsManager: SettingsManager.inMemory({
        compaction: { enabled: false },
        retry: { enabled: false },
        cacheWarming: "off",
      }),
      modelRuntime: { hasConfiguredAuth: () => true, getModel: () => model },
      initialActiveToolNames: ["read"],
    });
    try {
      await session.bindExtensions({
        mode: "print",
        onError: (error: unknown) => {
          throw new Error(JSON.stringify(error));
        },
      });
      await session.prompt("temporary skill run");
      expect(session.isIdle).toBe(true);
      await session.sendCustomMessage(
        {
          customType: "later",
          content: "read ordinary then guide",
          display: false,
        },
        { triggerTurn: true },
      );
      expect(beforeStarts).toBe(1);
      expect(requests).toHaveLength(4);
      expect(JSON.stringify(requests[0].skills)).toContain("temporary");
      expect(requests[2].skills).toBeUndefined();
    } finally {
      await session._extensionRunner.emit({
        type: "session_shutdown",
        reason: "exit",
      });
      session.dispose();
    }
    const exported = spans.getFinishedSpans();
    const tools = exported.filter(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(2);
    const tool = tools.find(
      (span) => span.attributes["gen_ai.tool.call.id"] === "stale-skill",
    )!;
    expect(tool.status.code).toBe(1);
    expect(tool.attributes["gen_ai.skill.name"]).toBeUndefined();
    expect(tool.name).toBe("execute_tool read");
    expect(
      exported.some(
        (span) => span.spanContext().spanId === tool.parentSpanContext?.spanId,
      ),
    ).toBe(true);
    expect(generations).toHaveLength(4);
  });

  it.each(
    MODES.flatMap((mode) =>
      ["earlier", "later"].flatMap((order) => [
        {
          mode,
          order,
          initial: "guide.md",
          latest: "ordinary.md",
          skill: undefined,
        },
        {
          mode,
          order,
          initial: "ordinary.md",
          latest: "guide.md",
          skill: order === "later" ? "review" : undefined,
        },
      ]),
    ),
  )("observes native $order-handler blocked rewrites $initial -> $latest in $mode", async ({
    mode,
    order,
    initial,
    latest,
    skill,
  }) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    writeFileSync(join(cwd, "guide.md"), "review instructions");
    writeFileSync(join(cwd, "ordinary.md"), "ordinary text");
    const pin = realpathSync(
      fileURLToPath(
        new URL(
          "../node_modules/@mariozechner/pi-coding-agent",
          import.meta.url,
        ),
      ),
    );
    const native = (path: string) =>
      import(/* @vite-ignore */ pathToFileURL(join(pin, path)).href);
    const { ExtensionRunner } = await native("dist/core/extensions/runner.js");
    const { AgentSession } = await native("dist/core/agent-session.js");
    const { createReadTool } = await native("dist/core/tools/read.js");
    const { runAgentLoop } = await native(
      "../pi-agent-core/dist/agent-loop.js",
    );
    let resultEvents = 0;
    const extension = {
      path: "telemetry",
      handlers: new Map(
        Array.from(pi.handlers, ([name, handler]) => [name, [handler]]),
      ),
    };
    const blocker = {
      path: "blocking-handler",
      handlers: new Map([
        [
          "tool_call",
          [
            async (event: any) => {
              await new Promise<void>((resolve) => setTimeout(resolve, 0));
              event.input.path = latest;
              return { block: true, reason: "blocked after rewrite" };
            },
          ],
        ],
        [
          "tool_result",
          [
            () => {
              resultEvents++;
            },
          ],
        ],
      ]),
    };
    const runner = new ExtensionRunner(
      order === "earlier" ? [blocker, extension] : [extension, blocker],
      {},
      cwd,
      ctx.sessionManager,
      {},
    );
    runner.onError((error: unknown) => {
      throw new Error(JSON.stringify(error));
    });
    const session = { agent: {}, _extensionRunner: runner } as any;
    AgentSession.prototype._installAgentToolHooks.call(session);
    await runner.emit({ type: "session_start" });
    await runner.emitBeforeAgentStart("review", undefined, {
      cwd,
      skills: [{ name: "review", filePath: join(cwd, "guide.md") }],
    });
    const message = {
      role: "assistant",
      provider: "synthetic",
      model: "local-model",
      api: "test",
      timestamp: Date.now(),
      stopReason: "toolUse",
      content: [
        {
          type: "toolCall",
          id: "native-blocked",
          name: "read",
          arguments: { path: initial },
        },
      ],
      usage: {
        input: 1,
        output: 1,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 2,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    };
    await runAgentLoop(
      [{ role: "user", content: "review", timestamp: Date.now() }],
      { messages: [], tools: [createReadTool(cwd)] },
      {
        model: { id: "local-model", provider: "synthetic" },
        convertToLlm: (messages: unknown) => messages,
        beforeToolCall: session.agent.beforeToolCall,
        afterToolCall: session.agent.afterToolCall,
        finishTurn: () => ({ action: "end" }),
      },
      (event: unknown) => runner.emit(event),
      undefined,
      async () => ({
        async *[Symbol.asyncIterator]() {
          yield { type: "done", reason: "toolUse", message };
        },
        async result() {
          return message;
        },
      }),
    );
    await runner.emit({ type: "session_shutdown" });
    const exported = spans.getFinishedSpans();
    expect(exported).toHaveLength(2);
    const generation = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "streamText",
    )!;
    const tool = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    )!;
    expect(message.content[0]!.arguments.path).toBe(initial);
    expect(tool.attributes["gen_ai.skill.name"]).toBe(skill);
    expect(tool.name).toBe(`execute_tool read${skill ? ` ${skill}` : ""}`);
    expect(tool.status.code).toBe(2);
    expect(tool.spanContext().traceId).toBe(generation.spanContext().traceId);
    expect(tool.parentSpanContext?.spanId).toBe(
      generation.spanContext().spanId,
    );
    expect(resultEvents).toBe(0);
    expect(generations).toHaveLength(1);
  });

  it.each(
    MODES,
  )("uses effective guarded inputs without losing blocked timings in %s", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    process.env.AGENTO11Y_GUARDS_ENABLED = "true";
    await pi.emit("session_start", {}, ctx);
    await pi.emit(
      "before_agent_start",
      {
        systemPromptOptions: {
          skills: [{ name: "review", filePath: join(cwd, "missing.md") }],
        },
      },
      ctx,
    );
    await pi.emit("agent_start", {}, ctx);
    await pi.emit("turn_start", {}, ctx);
    const calls = [
      { id: "transform-in", name: "read", arguments: { path: "ordinary.md" } },
      { id: "transform-out", name: "read", arguments: { path: "missing.md" } },
      {
        id: "result-overrides",
        name: "read",
        arguments: { path: "ordinary.md" },
      },
      { id: "blocked", name: "read", arguments: { path: "missing.md" } },
      {
        id: "blocked-later-in",
        name: "read",
        arguments: { path: "ordinary.md" },
      },
      {
        id: "blocked-later-out",
        name: "read",
        arguments: { path: "missing.md" },
      },
    ];
    const message: PiAssistantMessage = {
      role: "assistant",
      content: calls.map((call) => ({ type: "toolCall", ...call })),
      provider: "anthropic",
      model: "test-model",
      stopReason: "toolUse",
      timestamp: Date.now(),
      usage: {
        input: 10,
        output: 20,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 30,
      },
    };
    await pi.emit("message_end", { message }, ctx);
    for (const call of calls) {
      const input = { ...call.arguments };
      await pi.emit(
        "tool_execution_start",
        { toolCallId: call.id, toolName: call.name, args: input },
        ctx,
      );
      const verdict = await pi.emit(
        "tool_call",
        { toolCallId: call.id, toolName: call.name, input },
        ctx,
      );
      expect(input.path).toBe(
        call.id === "transform-out" || call.id === "blocked-later-in"
          ? "ordinary.md"
          : "missing.md",
      );
      expect(verdict?.block).toBe(call.id === "blocked" ? true : undefined);
      // A later tool_call handler may override our own transform.
      if (call.id === "result-overrides") input.path = "ordinary.md";
      if (call.id === "blocked-later-in") input.path = "missing.md";
      if (call.id === "blocked-later-out") input.path = "ordinary.md";
      const blocked = call.id.startsWith("blocked");
      // Pi 0.87.1 blocked calls end without invoking tool_result.
      if (!blocked) {
        await pi.emit(
          "tool_result",
          { toolCallId: call.id, toolName: call.name, input },
          ctx,
        );
      }
      await pi.emit(
        "tool_execution_end",
        {
          toolCallId: call.id,
          toolName: call.name,
          isError: blocked,
        },
        ctx,
      );
    }
    await pi.emit("turn_end", { message, toolResults: [] }, ctx);
    await pi.emit("session_shutdown", {}, ctx);
    const exported = spans.getFinishedSpans();
    expect(exported).toHaveLength(7);
    const generation = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "streamText",
    )!;
    const tools = exported.filter(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(6);
    for (const span of tools) {
      const id = span.attributes["gen_ai.tool.call.id"];
      expect(span.attributes["gen_ai.skill.name"]).toBe(
        id === "transform-in" || id === "blocked" || id === "blocked-later-in"
          ? "review"
          : undefined,
      );
      expect(span.status.code).toBe(String(id).startsWith("blocked") ? 2 : 1);
      expect(span.spanContext().traceId).toBe(generation.spanContext().traceId);
      expect(span.parentSpanContext?.spanId).toBe(
        generation.spanContext().spanId,
      );
    }
    expect(generations).toHaveLength(1);
  });
});
