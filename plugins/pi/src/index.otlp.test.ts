import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import type { ContentCaptureMode } from "@grafana/agento11y";
import {
  AggregationTemporality,
  DataPointType,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BasicTracerProvider,
  InMemorySpanExporter,
  SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  type MockInstance,
  vi,
} from "vitest";

const { telemetryFactory } = vi.hoisted(() => ({ telemetryFactory: vi.fn() }));
// Only provider construction is replaced. Spans, metrics, recorders, config,
// sanitization, and generation export use the actual SDK implementations.
vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: telemetryFactory,
}));

import registerExtension from "./index.js";
import type { PiAssistantMessage, PiToolResult } from "./mappers.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";
import {
  type Agento11yTestServer,
  closeServer,
  startAgento11yTestServer,
} from "./testHttp.js";

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

const DURATION_BUCKETS = [
  0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48,
  40.96, 81.92,
];

type ExpectedToolSeries = {
  ids: string[];
  tool?: string;
  skill?: string;
  error?: boolean;
};

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

  async function expectToolMetrics(
    series: ExpectedToolSeries[],
    provider = "anthropic",
    model = "test-model",
  ) {
    await meterProvider.forceFlush();
    await meterProvider.forceFlush();
    const exportedMetrics = metrics
      .getMetrics()
      .flatMap((batch) => batch.scopeMetrics.flatMap((scope) => scope.metrics));
    expect(
      [
        ...new Set(exportedMetrics.map((metric) => metric.descriptor.name)),
      ].sort(),
    ).toEqual([
      "gen_ai.client.operation.duration",
      "gen_ai.client.token.usage",
      "gen_ai.client.tool_calls_per_operation",
    ]);
    const allowedDimensions = new Set([
      "gen_ai.operation.name",
      "gen_ai.provider.name",
      "gen_ai.request.model",
      "gen_ai.agent.name",
      "gen_ai.agent.version",
      "gen_ai.tool.name",
      "gen_ai.skill.name",
      "agento11y.tool.execution.kind",
      "gen_ai.token.type",
      "error.type",
      "error.category",
    ]);
    for (const metric of exportedMetrics) {
      for (const point of metric.dataPoints) {
        expect(
          Object.keys(point.attributes).filter(
            (key) => !allowedDimensions.has(key),
          ),
        ).toEqual([]);
        if (
          metric.descriptor.name !== "gen_ai.client.operation.duration" ||
          point.attributes["gen_ai.operation.name"] !== "execute_tool"
        ) {
          expect(point.attributes).not.toHaveProperty("gen_ai.skill.name");
          expect(point.attributes).not.toHaveProperty(
            "agento11y.tool.execution.kind",
          );
        }
        expect(JSON.stringify(point.attributes)).not.toMatch(
          /glc_|private|\.md|wrong-|skill-conversation/,
        );
        expect(JSON.stringify(point.attributes)).not.toContain(cwd);
      }
    }
    const points = exportedMetrics.flatMap((metric) => {
      if (metric.descriptor.name !== "gen_ai.client.operation.duration")
        return [];
      expect(metric.descriptor.unit).toBe("s");
      expect(metric.dataPointType).toBe(DataPointType.HISTOGRAM);
      expect(metric.aggregationTemporality).toBe(AggregationTemporality.DELTA);
      return metric.dataPointType === DataPointType.HISTOGRAM
        ? metric.dataPoints.filter(
            (point) =>
              point.attributes["gen_ai.operation.name"] === "execute_tool",
          )
        : [];
    });
    const tools = spans
      .getFinishedSpans()
      .filter(
        (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
      );
    const expectedIds = series.flatMap((group) => group.ids);
    expect(new Set(expectedIds).size).toBe(expectedIds.length);
    expect(
      tools.map((span) => span.attributes["gen_ai.tool.call.id"]).sort(),
    ).toEqual([...expectedIds].sort());
    const duration = (span: (typeof tools)[number]) =>
      span.duration[0] + span.duration[1] / 1e9;
    const bucketCounts = (durations: number[]) => {
      const counts = Array<number>(DURATION_BUCKETS.length + 1).fill(0);
      for (const seconds of durations) {
        const index = DURATION_BUCKETS.findIndex((bound) => seconds <= bound);
        counts[index === -1 ? DURATION_BUCKETS.length : index]!++;
      }
      return counts;
    };
    const attributeKey = (attributes: Record<string, unknown>) =>
      JSON.stringify(
        Object.entries(attributes).sort(([left], [right]) =>
          left.localeCompare(right),
        ),
      );
    expect(
      new Set(points.map((point) => attributeKey(point.attributes))).size,
    ).toBe(series.length);
    for (const group of series) {
      const attributes = {
        "gen_ai.operation.name": "execute_tool",
        "gen_ai.tool.name": group.tool ?? "read",
        "gen_ai.provider.name": provider,
        "gen_ai.request.model": model,
        "gen_ai.agent.name": "pi",
        "gen_ai.agent.version": "test-version",
        "error.type": group.error ? "tool_execution_error" : "",
        "error.category": group.error ? "sdk_error" : "",
        ...(group.skill
          ? {
              "agento11y.tool.execution.kind": "load_skill",
              "gen_ai.skill.name": group.skill,
            }
          : {}),
      };
      const matching = points.filter(
        (point) => attributeKey(point.attributes) === attributeKey(attributes),
      );
      expect(matching.length).toBeGreaterThan(0);
      for (const point of matching) {
        expect(point.attributes).toEqual(attributes);
        expect(point.value.sum).toBeDefined();
        expect(point.value.buckets.boundaries).toEqual(DURATION_BUCKETS);
        expect(point.value.buckets.counts).toHaveLength(
          DURATION_BUCKETS.length + 1,
        );
        expect(
          point.value.buckets.counts.reduce((sum, count) => sum + count, 0),
        ).toBe(point.value.count);
      }
      const durations = tools
        .filter((span) =>
          group.ids.includes(String(span.attributes["gen_ai.tool.call.id"])),
        )
        .map(duration);
      // Native turns flush separately, so one label set can span delta batches.
      expect(matching.reduce((sum, point) => sum + point.value.count, 0)).toBe(
        group.ids.length,
      );
      expect(
        matching.reduce((sum, point) => sum + (point.value.sum ?? 0), 0),
      ).toBeCloseTo(
        durations.reduce((sum, value) => sum + value, 0),
        12,
      );
      expect(
        DURATION_BUCKETS.concat(Infinity).map((_, index) =>
          matching.reduce(
            (sum, point) => sum + point.value.buckets.counts[index]!,
            0,
          ),
        ),
      ).toEqual(bucketCounts(durations));
      expect(
        Math.min(...matching.map((point) => point.value.min!)),
      ).toBeCloseTo(Math.min(...durations), 12);
      expect(
        Math.max(...matching.map((point) => point.value.max!)),
      ).toBeCloseTo(Math.max(...durations), 12);
    }
    expect(points.reduce((sum, point) => sum + point.value.count, 0)).toBe(
      expectedIds.length,
    );
    expect(
      points.reduce((sum, point) => sum + (point.value.sum ?? 0), 0),
    ).toBeCloseTo(
      tools.reduce((sum, span) => sum + duration(span), 0),
      12,
    );
    expect(
      DURATION_BUCKETS.concat(Infinity).map((_, index) =>
        points.reduce(
          (sum, point) => sum + point.value.buckets.counts[index]!,
          0,
        ),
      ),
    ).toEqual(bucketCounts(tools.map(duration)));
  }

  afterEach(async () => {
    await pi.emit("session_shutdown", {}, ctx);
    await Promise.all([tracerProvider.shutdown(), meterProvider.shutdown()]);
    await new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    );
    rmSync(cwd, { recursive: true, force: true });
    restoreEnv(savedEnv);
    vi.restoreAllMocks();
  });

  it.each(
    MODES,
  )("retains finalized top-level results and occurrence-scoped nested observations in %s", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    await pi.emit("session_start", {}, ctx);
    const batches: Array<{ calls: any[]; input: Record<string, unknown> }> = [];
    for (let turn = 0; turn < 2; turn++) {
      await pi.emit("turn_start", {}, ctx);
      const proposed = { value: `proposal-${turn}` };
      const observed =
        turn === 0 ? proposed : { value: "prepared", extra: "post-execution" };
      const calls = [
        { toolCallId: "reused", toolName: "bash" },
        { toolCallId: "child-a", toolName: "bash", parentToolCallId: "reused" },
        { toolCallId: "child-b", toolName: "read", parentToolCallId: "reused" },
        {
          toolCallId: "grandchild",
          toolName: "bash",
          parentToolCallId: "child-a",
        },
        { toolCallId: "orphan", toolName: "bash", parentToolCallId: "missing" },
        { toolCallId: "reused/99", toolName: "bash" },
      ];
      batches.push({ calls, input: observed });
      const message: PiAssistantMessage = {
        role: "assistant",
        content: [
          { type: "toolCall", id: "reused", name: "bash", arguments: proposed },
        ],
        provider: "anthropic",
        model: "test-model",
        stopReason: "toolUse",
        timestamp: 1000 + turn,
        usage: {
          input: 1,
          output: 1,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 2,
        },
      };
      await pi.emit("message_end", { message }, ctx);
      for (const call of calls) {
        await pi.emit("tool_execution_start", { ...call, args: proposed }, ctx);
        await pi.emit("tool_call", { ...call, input: observed }, ctx);
      }
      for (const call of [...calls].reverse()) {
        await pi.emit(
          "tool_result",
          {
            ...call,
            input: observed,
            content: [{ type: "text", text: "early result must not win" }],
            isError: false,
          },
          ctx,
        );
        await pi.emit(
          "tool_execution_end",
          {
            ...call,
            isError:
              call.toolCallId === "grandchild" || call.toolCallId === "reused",
            durationMs: 12.5,
            result: {
              content: [
                { type: "text", text: `final-${turn}-${call.toolCallId}` },
              ],
            },
          },
          ctx,
        );
      }
      await pi.emit(
        "turn_end",
        {
          message,
          toolResults: [
            {
              role: "toolResult",
              toolCallId: "reused",
              toolName: "bash",
              content: [{ type: "text", text: `finalized-${turn}-reused` }],
              isError: false,
              timestamp: Date.now(),
            },
          ],
        },
        ctx,
      );
    }
    await pi.emit("session_shutdown", {}, ctx);
    await tracerProvider.forceFlush();
    expect(generations).toHaveLength(2);
    const exported = spans.getFinishedSpans();
    for (let turn = 0; turn < 2; turn++) {
      const generation = generations[turn];
      const tools = exported.filter(
        (span) =>
          span.spanContext().traceId === generation.trace_id &&
          span.attributes["gen_ai.operation.name"] === "execute_tool",
      );
      expect(tools).toHaveLength(6);
      for (const call of batches[turn]!.calls) {
        const span = tools.find(
          (span) => span.attributes["gen_ai.tool.call.id"] === call.toolCallId,
        )!;
        const parent = tools.find(
          (span) =>
            span.attributes["gen_ai.tool.call.id"] === call.parentToolCallId,
        );
        expect(span.parentSpanContext?.spanId).toBe(
          parent?.spanContext().spanId ?? generation.span_id,
        );
        expect(span.duration[0] + span.duration[1] / 1e9).toBeCloseTo(
          Math.round(12.5) / 1000,
          6,
        );
        expect(span.status.code).toBe(
          call.toolCallId === "grandchild" || call.toolCallId === "reused"
            ? 2
            : 1,
        );
        if (mode === "full") {
          expect(
            JSON.parse(span.attributes["gen_ai.tool.call.arguments"] as string),
          ).toEqual({
            observation: "tool_result.input",
            input: batches[turn]!.input,
          });
          const body = span.attributes["gen_ai.tool.call.result"];
          if (call.toolCallId === "reused") {
            expect(JSON.parse(body as string)).toBe(`finalized-${turn}-reused`);
          } else if (call.parentToolCallId) {
            expect(JSON.parse(body as string)).toBe(
              `final-${turn}-${call.toolCallId}`,
            );
          } else expect(body).toBeUndefined();
        } else {
          expect(span.attributes["gen_ai.tool.call.arguments"]).toBeUndefined();
          expect(span.attributes["gen_ai.tool.call.result"]).toBeUndefined();
        }
      }
      const parts = generation.output.flatMap((message: any) => message.parts);
      const call = parts.find((part: any) => part.tool_call).tool_call;
      const result = parts.find((part: any) => part.tool_result).tool_result;
      if (
        turn === 0 &&
        (mode === "full" || mode === "full_with_metadata_spans")
      ) {
        expect(
          JSON.parse(Buffer.from(call.input_json, "base64").toString("utf8")),
        ).toEqual({ value: "proposal-0" });
      } else expect(call.input_json ?? "").toBe("");
      expect(result.content ?? "").toBe(
        mode === "full" || mode === "full_with_metadata_spans"
          ? `finalized-${turn}-reused`
          : "",
      );
      expect(result.is_error ?? false).toBe(false);
    }
    expect(JSON.stringify(generations)).not.toContain("final-0-reused");
    expect(JSON.stringify(generations)).not.toContain("final-1-reused");
  });

  it.each(
    MODES,
  )("retains successful and failed skill loads and their generation parent in %s", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
    let now = 1_700_000_000_000;
    vi.spyOn(Date, "now").mockImplementation(() => now);
    const intervals = new Map<string, { start: number; end?: number }>();
    writeFileSync(join(cwd, "guide.md"), "instructions");
    await pi.emit("session_start", {}, ctx);
    const secret = "glc_abcdefghijklmnopqrstuvwxyz";
    const description = "private catalog description must not escape";
    const sourceURI = "https://private.invalid/catalog/source";
    const options = {
      skills: [
        {
          name: "wrong-before-later-handler" as string | undefined,
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
      ...[undefined, "", "private/name", "https:private"].map(
        (name, index) => ({
          name,
          filePath: join(cwd, `unsafe-${index}.md`),
          description,
          sourceURI,
        }),
      ),
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
      {
        id: "standalone-repeat",
        name: "read",
        arguments: { path: "guide.md" },
      },
      ...[0, 1, 2, 3].map((index) => ({
        id: `unsafe-${index}`,
        name: "read",
        arguments: { path: `unsafe-${index}.md` },
      })),
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
      now += 13;
      if (call.id !== "missing-start") {
        intervals.set(call.id, { start: now });
        await pi.emit(
          "tool_execution_start",
          { toolCallId: call.id, toolName: call.name, args: call.arguments },
          ctx,
        );
      }
      await pi.emit(
        "tool_call",
        { toolCallId: call.id, toolName: call.name, input: call.arguments },
        ctx,
      );
    }
    const results: PiToolResult[] = [];
    // Sibling completions may reverse the assistant's call order.
    for (const call of [...calls].reverse()) {
      now += 37;
      const interval = intervals.get(call.id);
      if (interval) interval.end = now;
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
        await pi.emit(
          "tool_execution_end",
          { ...result, result: { content: result.content } },
          ctx,
        );
    }
    await pi.emit("turn_end", { message, toolResults: results }, ctx);
    await pi.emit("session_shutdown", {}, ctx);
    await Promise.all([
      tracerProvider.forceFlush(),
      meterProvider.forceFlush(),
    ]);

    const exported = spans.getFinishedSpans();
    expect(exported).toHaveLength(calls.length - 1);
    const generation = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "streamText",
    )!;
    expect(generation).toBeDefined();
    const tools = exported.filter(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(calls.length - 2);
    expect(
      tools.map((span) => span.attributes["gen_ai.tool.call.id"]).sort(),
    ).toEqual(
      calls
        .slice(0, -2)
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
          : call.id.startsWith("standalone-")
            ? "standalone"
            : undefined;
      const interval = intervals.get(call.id)!;
      expect(span.duration[0] + span.duration[1] / 1e9).toBeCloseTo(
        (interval.end! - interval.start) / 1000,
        12,
      );
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
    await expectToolMetrics([
      { ids: ["failed-load"], skill: "org:review", error: true },
      { ids: ["standalone-load", "standalone-repeat"], skill: "standalone" },
      {
        ids: [
          "ordinary-read",
          "secret-name",
          "resource-read",
          "unsafe-0",
          "unsafe-1",
          "unsafe-2",
          "unsafe-3",
        ],
      },
      { ids: ["command"], tool: "bash" },
    ]);
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

  it.each(
    MODES,
  )("does not reuse temporary skills in a native custom-message run after settlement in %s", async (mode) => {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
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
          if (event.messageType !== "user") return;
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
        const call = turn === 1 || turn === 3 || turn === 4;
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
                  id:
                    turn === 1
                      ? "temporary-skill"
                      : turn === 3
                        ? "ordinary"
                        : "stale-skill",
                  name: "read",
                  arguments: { path: turn === 3 ? "ordinary.md" : "guide.md" },
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
      expect(beforeStarts).toBe(2);
      expect(requests).toHaveLength(5);
      expect(JSON.stringify(requests[0].skills)).toContain("temporary");
      expect(requests[3].skills).toBeUndefined();
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
    expect(tools).toHaveLength(3);
    const temporary = tools.find(
      (span) => span.attributes["gen_ai.tool.call.id"] === "temporary-skill",
    )!;
    expect(temporary.attributes["gen_ai.skill.name"]).toBe("temporary");
    expect(temporary.status.code).toBe(1);
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
    expect(generations).toHaveLength(5);
    await expectToolMetrics(
      [
        { ids: ["temporary-skill"], skill: "temporary" },
        { ids: ["ordinary", "stale-skill"] },
      ],
      "synthetic",
      "local",
    );
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
    let blockedCalls = 0;
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
              blockedCalls++;
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
    const session = Object.assign(Object.create(AgentSession.prototype), {
      agent: {},
      _extensionRunner: runner,
    });
    AgentSession.prototype._installAgentToolHooks.call(session);
    await runner.emit({ type: "session_start" });
    await runner.emitBeforeAgentStart(
      { messageType: "user", prompt: "review" },
      {
        cwd,
        skills: [{ name: "review", filePath: join(cwd, "guide.md") }],
      },
    );
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
    expect(blockedCalls).toBe(1);
    expect(resultEvents).toBe(0);
    await expectToolMetrics(
      [{ ids: ["native-blocked"], skill, error: true }],
      "synthetic",
      "local-model",
    );
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
      {
        id: "invalid",
        name: "read",
        arguments: { path: "invalid-proposal-body" },
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
      const verdict =
        call.id === "invalid"
          ? undefined
          : await pi.emit(
              "tool_call",
              { toolCallId: call.id, toolName: call.name, input },
              ctx,
            );
      expect(input.path).toBe(
        call.id === "invalid"
          ? "invalid-proposal-body"
          : call.id === "transform-out" || call.id === "blocked-later-in"
            ? "ordinary.md"
            : "missing.md",
      );
      expect(verdict?.block).toBe(call.id === "blocked" ? true : undefined);
      // A later tool_call handler may override our own transform.
      if (call.id === "result-overrides") input.path = "ordinary.md";
      if (call.id === "blocked-later-in") input.path = "missing.md";
      if (call.id === "blocked-later-out") input.path = "ordinary.md";
      const blocked = call.id.startsWith("blocked") || call.id === "invalid";
      // Blocked calls end without invoking tool_result.
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
          result: { content: [{ type: "text", text: `final-${call.id}` }] },
        },
        ctx,
      );
    }
    await pi.emit(
      "turn_end",
      {
        message,
        toolResults: calls.map((call) => ({
          role: "toolResult",
          toolCallId: call.id,
          toolName: call.name,
          content: [{ type: "text", text: `final-${call.id}` }],
          isError: call.id.startsWith("blocked") || call.id === "invalid",
          timestamp: Date.now(),
        })),
      },
      ctx,
    );
    await pi.emit("session_shutdown", {}, ctx);
    const exported = spans.getFinishedSpans();
    expect(exported).toHaveLength(8);
    const generation = exported.find(
      (span) => span.attributes["gen_ai.operation.name"] === "streamText",
    )!;
    const tools = exported.filter(
      (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(7);
    for (const span of tools) {
      const id = span.attributes["gen_ai.tool.call.id"];
      const hasInput =
        mode === "full" && (id === "transform-in" || id === "transform-out");
      expect(span.attributes["gen_ai.tool.call.arguments"] !== undefined).toBe(
        hasInput,
      );
      if (hasInput) {
        expect(
          JSON.parse(span.attributes["gen_ai.tool.call.arguments"] as string),
        ).toEqual({
          observation: "tool_result.input",
          input: { path: id === "transform-in" ? "missing.md" : "ordinary.md" },
        });
      }
      expect(span.attributes["gen_ai.tool.call.result"]).toBe(
        mode === "full" ? JSON.stringify(`final-${id}`) : undefined,
      );
      expect(span.attributes["gen_ai.skill.name"]).toBe(
        id === "transform-in" || id === "blocked" || id === "blocked-later-in"
          ? "review"
          : undefined,
      );
      expect(span.status.code).toBe(
        String(id).startsWith("blocked") || id === "invalid" ? 2 : 1,
      );
      expect(span.spanContext().traceId).toBe(generation.spanContext().traceId);
      expect(span.parentSpanContext?.spanId).toBe(
        generation.spanContext().spanId,
      );
    }
    await expectToolMetrics([
      { ids: ["transform-in"], skill: "review" },
      { ids: ["transform-out", "result-overrides"] },
      { ids: ["blocked", "blocked-later-in"], skill: "review", error: true },
      { ids: ["blocked-later-out", "invalid"], error: true },
    ]);
    expect(generations).toHaveLength(1);
    const proposals = generations[0].output
      .flatMap((message: any) => message.parts)
      .filter((part: any) => part.tool_call)
      .map((part: any) => part.tool_call);
    expect(proposals).toHaveLength(calls.length);
    expect(proposals.every((call: any) => !call.input_json)).toBe(true);
    expect(JSON.stringify(generations)).not.toContain("invalid-proposal-body");
  });
});

describe("pi export loss: actual SDK", () => {
  const warning =
    "Agent Observability: some telemetry could not be exported and may be lost.";
  const cases = MODES.flatMap((mode) =>
    ["ui", "headless", "throwing-ui"].flatMap((sink) =>
      ["rejection", "retries", "queue-turn", "queue-summary", "shutdown"].map(
        (failure) => ({ mode, sink, failure }),
      ),
    ),
  );
  let savedEnv: Record<string, string | undefined>;
  let cwd: string;
  let receiver: Agento11yTestServer | undefined;
  let pi: SyntheticPi;
  let ctx: any;
  let stderr: MockInstance<typeof process.stderr.write>;
  let stdout: MockInstance<typeof process.stdout.write>;

  beforeEach(() => {
    savedEnv = snapshotAndClearTestEnv();
    cwd = mkdtempSync(join(tmpdir(), "pi-export-loss-"));
    receiver = undefined;
    pi = new SyntheticPi();
    telemetryFactory.mockReset();
    registerExtension(pi as any);
    stderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    stdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
  });

  afterEach(async () => {
    try {
      await pi.emit("session_shutdown", {}, ctx);
      if (receiver) await closeServer(receiver.server);
    } finally {
      stderr.mockRestore();
      stdout.mockRestore();
      restoreEnv(savedEnv);
      rmSync(cwd, { recursive: true, force: true });
    }
  });

  it.each(cases)("warns once for $failure through $sink in $mode", async ({
    mode,
    sink,
    failure,
  }) => {
    receiver = await startAgento11yTestServer({
      export: (body) => {
        if (failure === "rejection")
          return {
            json: {
              results: body.generations?.map((generation, index) => ({
                generation_id: generation.id,
                accepted: index !== 0,
                error:
                  "private evaluated body https://private.invalid token=secret",
              })),
            },
          };
        if (failure === "retries")
          return {
            status: 503,
            json: {
              error:
                "private evaluated body https://private.invalid token=secret",
            },
          };
        if (failure === "shutdown") return { delayMs: 1000 };
        return {
          json: {
            results: body.generations?.map((generation) => ({
              generation_id: generation.id,
              accepted: true,
            })),
          },
        };
      },
    });
    const notify = vi.fn(() => {
      if (sink === "throwing-ui")
        throw new Error("private notification details");
    });
    ctx = {
      cwd,
      hasUI: sink !== "headless",
      ui: { notify },
      sessionManager: {
        getSessionId: () => "export-loss-session",
        getHeader: () => ({
          id: "export-loss-session",
          timestamp: "2026-01-01T00:00:00Z",
        }),
      },
      model: { provider: "synthetic", id: "test-model" },
    };
    Object.assign(process.env, {
      HOME: cwd,
      USERPROFILE: cwd,
      XDG_CONFIG_HOME: join(cwd, ".config"),
      XDG_STATE_HOME: join(cwd, ".state"),
      AGENTO11Y_ENDPOINT: receiver.baseUrl,
      AGENTO11Y_AUTH_MODE: "none",
      AGENTO11Y_GUARDS_ENABLED: "false",
      AGENTO11Y_DEBUG: "false",
      AGENTO11Y_CONTENT_CAPTURE_MODE: mode,
      AGENTO11Y_QUEUE_SIZE: failure.startsWith("queue") ? "1" : "100",
      AGENTO11Y_EXPORT_TIMEOUT_MS: failure === "shutdown" ? "30" : "1000",
      AGENTO11Y_MAX_RETRIES: "1",
      AGENTO11Y_MAX_BACKOFF_MS: "1",
    });
    await pi.emit("session_start", { reason: "startup" }, ctx);
    const message: PiAssistantMessage = {
      role: "assistant",
      provider: "synthetic",
      model: "test-model",
      content: [{ type: "text", text: "private evaluated body" }],
      usage: {
        input: 1,
        output: 1,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 2,
      },
      stopReason: "stop",
      timestamp: Date.now(),
    };
    for (let turn = 0; turn < 3; turn++) {
      if (failure === "queue-summary" && turn > 0) {
        await pi.emit(
          "session_compact",
          {
            compactionEntry: {
              id: `summary-${turn}`,
              summary: "private evaluated body",
              timestamp: new Date().toISOString(),
            },
          },
          ctx,
        );
      } else {
        await pi.emit("turn_start", {}, ctx);
        await pi.emit(
          "turn_end",
          {
            message: { ...message, timestamp: message.timestamp + turn },
            toolResults: [],
          },
          ctx,
        );
      }
    }
    if (failure.startsWith("queue")) {
      expect(
        sink === "headless"
          ? stderr.mock.calls.length
          : notify.mock.calls.length,
      ).toBe(1);
    }
    await pi.emit("session_shutdown", {}, ctx);
    expect(notify.mock.calls).toEqual(
      sink === "headless" ? [] : [[warning, "warning"]],
    );
    expect(stderr.mock.calls.map(([chunk]) => String(chunk))).toEqual(
      sink === "ui" ? [] : [`${warning}\n`],
    );
    expect(stdout).not.toHaveBeenCalled();
    expect(receiver.errors).toEqual([]);
    expect(receiver.exportCount).toBe(failure === "retries" ? 2 : 1);
    expect(telemetryFactory).not.toHaveBeenCalled();
  });
});
