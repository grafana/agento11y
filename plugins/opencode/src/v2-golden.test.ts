// Native v2 capture uses the real agento11y SDK and OTLP exporters, only over loopback.
// UPDATE_GOLDENS=1 refreshes the full wire fixture.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createAgento11yClient } from "./client.js";
import type { Agento11yOpencodeConfig } from "./config.js";
import {
  stableOpencodeCompactionGenerationId,
  stableOpencodeGenerationId,
} from "./lineage.js";
import { createRedactor } from "./redaction.js";
import {
  createOwnedTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";
import { createNativeCapture } from "./v2-events.js";
import { nativeFixture, nativeTranscript } from "./v2-testdata.js";

const GOLDEN_PATH = fileURLToPath(
  new URL(
    "./testdata/golden/opencode-v2-full-message.golden.json",
    import.meta.url,
  ),
);
const REDACTED = "[REDACTED:grafana-cloud-token]";

interface Request {
  path: string;
  body: any;
  headers: Record<string, string | string[] | undefined>;
}
async function loopbackReceiver() {
  const requests: Request[] = [];
  const errors: string[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      let body: any;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      } catch {
        errors.push(`invalid JSON on ${req.url}`);
        res.writeHead(400).end("{}");
        return;
      }
      const path = req.url ?? "";
      requests.push({ path, body, headers: req.headers });
      const response =
        path === "/api/v1/generations:export"
          ? {
              results: body.generations.map((generation: any) => ({
                generation_id: generation.id,
                accepted: true,
              })),
            }
          : {};
      if (
        ![
          "/api/v1/generations:export",
          "/otlp/v1/traces",
          "/otlp/v1/metrics",
        ].includes(path)
      ) {
        errors.push(`unexpected request ${path}`);
      }
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify(response));
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string")
    throw new Error("expected loopback TCP listener");
  return {
    server,
    requests,
    errors,
    endpoint: `http://127.0.0.1:${address.port}`,
  };
}
function close(server: Server): Promise<void> {
  return new Promise((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
}
function attributes(item: any): Record<string, any> {
  return Object.fromEntries(
    (item.attributes ?? []).map(({ key, value }: any) => [
      key,
      value.stringValue ??
        value.intValue ??
        value.doubleValue ??
        value.boolValue,
    ]),
  );
}
function spansFrom(requests: Request[]): any[] {
  return requests
    .filter((request) => request.path === "/otlp/v1/traces")
    .flatMap((request) => request.body.resourceSpans ?? [])
    .flatMap((resource: any) => resource.scopeSpans ?? [])
    .flatMap((scope: any) => scope.spans ?? []);
}
function metricsFrom(requests: Request[]): any[] {
  return requests
    .filter((request) => request.path === "/otlp/v1/metrics")
    .flatMap((request) => request.body.resourceMetrics ?? [])
    .flatMap((resource: any) => resource.scopeMetrics ?? [])
    .flatMap((scope: any) => scope.metrics ?? []);
}
function outputPart(turn: any, key: string): any {
  const parts = turn.output.flatMap((message: any) => message.parts);
  const part = parts.find((candidate: any) => candidate[key] !== undefined);
  expect(part, `expected output ${key}`).toBeDefined();
  return part[key];
}
function decodeInput(part: any): string {
  return Buffer.from(part.input_json, "base64").toString("utf8");
}

// Only OTel-generated identifiers are dynamic. Fixture timestamps, stable
// generation/call/session IDs, versions and every semantic field
// remain visible to the golden comparison.
function normalize(value: any): any {
  if (Array.isArray(value)) return value.map(normalize);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [
        key,
        ["trace_id", "span_id", "parent_span_id"].includes(key) && child
          ? "<DYNAMIC-ID>"
          : normalize(child),
      ]),
    );
  return value;
}
function assertGolden(value: unknown) {
  const formatted = `${JSON.stringify(normalize(value), null, 2)}\n`;
  if (process.env.UPDATE_GOLDENS === "1") writeFileSync(GOLDEN_PATH, formatted);
  expect(formatted).toBe(readFileSync(GOLDEN_PATH, "utf8"));
}

describe("native v2 capture: real SDK golden and OTLP", () => {
  let savedEnv: Record<string, string | undefined>;
  let home: string;
  let receiver: Awaited<ReturnType<typeof loopbackReceiver>>;
  let client: ReturnType<typeof createAgento11yClient>;
  let telemetry: TelemetryProviders | undefined;
  let capture: ReturnType<typeof createNativeCapture> | undefined;

  beforeEach(async () => {
    savedEnv = {};
    for (const key of new Set([
      ...Object.keys(process.env).filter((key) =>
        /^(AGENTO11Y_|SIGIL_|OTEL_)/.test(key),
      ),
      "HOME",
      "USERPROFILE",
      "XDG_CONFIG_HOME",
    ])) {
      savedEnv[key] = process.env[key];
      delete process.env[key];
    }
    home = mkdtempSync(join(tmpdir(), "opencode-v2-golden-"));
    process.env.HOME = home;
    process.env.USERPROFILE = home;
    process.env.XDG_CONFIG_HOME = home;
    receiver = await loopbackReceiver();
  });
  afterEach(async () => {
    try {
      const captureResults = await Promise.allSettled([
        Promise.resolve().then(() => capture?.shutdown()),
      ]);
      const exporterResults = await Promise.allSettled([
        Promise.resolve().then(() => client?.shutdown()),
        Promise.resolve().then(() => telemetry?.shutdown()),
      ]);
      const failures = [...captureResults, ...exporterResults].filter(
        (result) => result.status === "rejected",
      );
      if (failures.length) {
        throw new AggregateError(
          failures.map((failure) => failure.reason),
          "exporter cleanup failed",
        );
      }
    } finally {
      await close(receiver.server);
      rmSync(home, { recursive: true, force: true });
      for (const key of Object.keys(process.env)) {
        if (/^(AGENTO11Y_|SIGIL_|OTEL_)/.test(key)) delete process.env[key];
      }
      for (const [key, value] of Object.entries(savedEnv)) {
        if (value === undefined) delete process.env[key];
        else process.env[key] = value;
      }
      capture = undefined;
      client = null;
      telemetry = undefined;
    }
  });

  async function run(
    contentCapture: Agento11yOpencodeConfig["contentCapture"],
  ) {
    const config: Agento11yOpencodeConfig = {
      endpoint: receiver.endpoint,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "native-fixture-v1",
      contentCapture,
      redactInputMessages: true,
      debug: false,
      // Capture redaction is local and must not depend on remote guards.
      guards: { enabled: false, timeoutMs: 1500, failOpen: true },
    };
    telemetry = await createOwnedTelemetryProviders(
      {
        endpoint: `${receiver.endpoint}/otlp`,
        headers: { "X-Native-Fixture": "present" },
      },
      "native-fixture-instance",
    );
    client = createAgento11yClient(config, telemetry);
    if (!client) throw new Error("real SDK client construction failed");
    const diagnostics: string[] = [];
    capture = createNativeCapture({
      client,
      config,
      redactor: createRedactor(),
      ...nativeFixture.location,
      debugLog: (message) => diagnostics.push(message),
    });
    const transcript = nativeTranscript();
    // No v1 hook, session.message fetch, or live OpenCode server is involved.
    async function* subscription() {
      yield* transcript;
    }
    for await (const event of subscription()) capture.handleEvent(event);
    await capture.drain();
    // A replay and the execution-level terminal must not produce another turn
    // or duplicate an execute_tool span after the step-level terminal.
    for (const event of transcript) capture.handleEvent(event);
    await capture.drain();
    await capture.shutdown();
    await client.shutdown();
    await telemetry.shutdown();
    capture = undefined;
    client = null;
    telemetry = undefined;

    expect(diagnostics).toEqual([]);
    expect(receiver.errors).toEqual([]);
    const exports = receiver.requests.filter(
      (request) => request.path === "/api/v1/generations:export",
    );
    const generations = exports.flatMap((request) => request.body.generations);
    expect(generations).toHaveLength(2);
    const turn = generations.find(
      (generation: any) =>
        generation.id ===
        stableOpencodeGenerationId(
          nativeFixture.sessionID,
          nativeFixture.assistantMessageID,
        ),
    );
    const terminal = transcript.find(
      (event) => event.type === "session.compaction.ended",
    );
    if (!terminal) throw new Error("missing compaction fixture");
    const compaction = generations.find(
      (generation: any) =>
        generation.id ===
        stableOpencodeCompactionGenerationId(
          nativeFixture.sessionID,
          terminal.id,
        ),
    );
    expect(compaction).toMatchObject({
      conversation_id: nativeFixture.sessionID,
      agent_name: "opencode",
      operation_name: "generateText",
      mode: "GENERATION_MODE_SYNC",
      model: { provider: "anthropic", name: nativeFixture.model.id },
      tags: {
        "opencode.call_kind": "compaction",
        cwd: nativeFixture.location.directory,
      },
      input: [],
      started_at: new Date(terminal.created).toISOString(),
      completed_at: new Date(terminal.created).toISOString(),
      usage: { input_tokens: "100", output_tokens: "20", total_tokens: "120" },
      metadata: {
        cost_usd: 0.001,
        "opencode.usage_scope": "logical_compaction",
        "opencode.compaction.reason": "auto",
        "opencode.settlement": "compaction.ended",
        "opencode.input_source": "unavailable",
        "opencode.model_source": "event",
        "opencode.start_time_approximate": true,
        "opencode.completion_time_approximate": true,
      },
    });
    expect(compaction.parent_generation_ids).toBeUndefined();
    expect(compaction.output).toEqual(
      contentCapture === "metadata_only"
        ? []
        : [
            {
              role: "MESSAGE_ROLE_ASSISTANT",
              name: "",
              parts: [{ text: `Earlier summary; token: ${REDACTED}` }],
            },
          ],
    );
    expect(turn.conversation_id).toBe(nativeFixture.sessionID);
    expect(turn.agent_name).toBe("opencode:build");
    expect(turn.tags).toEqual({ cwd: nativeFixture.location.directory });
    expect(turn.model).toEqual({
      provider: "anthropic",
      name: nativeFixture.model.id,
    });
    expect(turn.operation_name).toBe("streamText");
    expect(turn.mode).toBe("GENERATION_MODE_STREAM");
    expect(turn.started_at).toBe(new Date(nativeFixture.started).toISOString());
    expect(turn.completed_at).toBe(
      new Date(nativeFixture.streamed).toISOString(),
    );
    expect(turn.usage).toEqual({
      input_tokens: "250",
      output_tokens: "60",
      total_tokens: "310",
      cache_read_input_tokens: "30",
      cache_write_input_tokens: "20",
      reasoning_tokens: "10",
      input_semantics: "TOKEN_INPUT_SEMANTICS_INCLUSIVE",
    });
    expect(turn.stop_reason).toBe("stop");
    expect(turn.call_error).toBe("");
    expect(turn.metadata).toMatchObject({
      "agento11y.sdk.content_capture_mode": contentCapture,
      "opencode.input_source": "first_observed_prefix",
      "opencode.completion_time_approximate": false,
      "opencode.settlement": "step.ended",
      cost_usd: 0.005,
    });
    expect(turn.tools.map((tool: any) => tool.name)).toEqual(["bash"]);
    // Native capture does not observe the final provider prompt; do not manufacture one.
    expect(turn.system_prompt).toBe(
      contentCapture === "metadata_only"
        ? ""
        : `Prefer concise answers; token: ${REDACTED}\n\nUse the observed repository context; DATABASE_PASSWORD=[REDACTED:env-secret-value]`,
    );
    if (contentCapture !== "metadata_only") {
      expect(turn.input.map((message: any) => message.parts[0].text)).toEqual([
        `Earlier summary; token: ${REDACTED}`,
        "Recent context: inspect Go files; DATABASE_PASSWORD=[REDACTED:env-secret-value]",
        `list go files; token: ${REDACTED}; DATABASE_PASSWORD=[REDACTED:env-secret-value]`,
      ]);
      expect(
        turn.input.every(
          (message: any) => message.role === "MESSAGE_ROLE_USER",
        ),
      ).toBe(true);
    }
    const wire = JSON.stringify(receiver.requests);
    expect(wire).not.toContain(nativeFixture.secret);
    expect(wire).not.toContain(nativeFixture.envSecret);
    expect(wire).not.toContain("DO_NOT_EXPORT_PROVIDER_STATE");
    assertTelemetry(turn, compaction, contentCapture);
    return {
      turn,
      exports: exports.map(({ path, body }) => ({
        path,
        generations: body.generations,
      })),
    };
  }

  function assertTelemetry(turn: any, compaction: any, mode: string) {
    const otlpRequests = receiver.requests.filter((request) =>
      request.path.startsWith("/otlp/"),
    );
    expect(
      otlpRequests.some((request) => request.path.endsWith("/traces")),
    ).toBe(true);
    expect(
      otlpRequests.some((request) => request.path.endsWith("/metrics")),
    ).toBe(true);
    for (const request of receiver.requests)
      expect(request.headers.authorization).toBeUndefined();
    for (const request of otlpRequests) {
      expect(request.headers["x-native-fixture"]).toBe("present");
      const resources =
        request.body.resourceSpans ?? request.body.resourceMetrics;
      for (const resource of resources) {
        expect(attributes(resource.resource)).toMatchObject({
          "service.name": "sigil-opencode",
          "service.instance.id": "native-fixture-instance",
        });
        for (const scope of resource.scopeSpans ?? resource.scopeMetrics) {
          expect(scope.scope.name).toBe("sigil-opencode");
        }
      }
    }
    const spans = spansFrom(receiver.requests);
    const generationSpans = spans.filter(
      (span) => attributes(span)["gen_ai.operation.name"] === "streamText",
    );
    const toolSpans = spans.filter(
      (span) => attributes(span)["gen_ai.operation.name"] === "execute_tool",
    );
    expect(generationSpans).toHaveLength(1);
    const compactionSpans = spans.filter(
      (span) => attributes(span)["gen_ai.operation.name"] === "generateText",
    );
    expect(compactionSpans).toHaveLength(1);
    expect(compactionSpans[0].traceId).toBe(compaction.trace_id);
    expect(compactionSpans[0].spanId).toBe(compaction.span_id);
    expect(attributes(compactionSpans[0])).toMatchObject({
      "gen_ai.usage.input_tokens": 100,
      "gen_ai.usage.output_tokens": 20,
    });
    expect(toolSpans).toHaveLength(1);
    const generation = generationSpans[0];
    expect(generation.traceId).toBe(turn.trace_id);
    expect(generation.spanId).toBe(turn.span_id);
    expect(attributes(generation)).toMatchObject({
      "gen_ai.usage.input_tokens": 250,
      "gen_ai.usage.output_tokens": 60,
      "gen_ai.usage.cache_read_input_tokens": 30,
      "gen_ai.usage.cache_write_input_tokens": 20,
      "gen_ai.usage.reasoning_tokens": 10,
      "gen_ai.token.semantics": "inclusive",
    });
    const tool = toolSpans[0];
    // The plugin owns providers, not a global OTel context manager. Correlate
    // independent tool spans by native call/session identity, not an invented parent.
    expect(tool.traceId).toMatch(/^[0-9a-f]{32}$/);
    expect(attributes(tool)).toMatchObject({
      "gen_ai.tool.name": "bash",
      "gen_ai.tool.call.id": nativeFixture.toolID,
      "gen_ai.conversation.id": nativeFixture.sessionID,
    });
    expect(Number(tool.endTimeUnixNano) - Number(tool.startTimeUnixNano)).toBe(
      400_000_000,
    );
    if (mode === "full") {
      expect(attributes(tool)["gen_ai.tool.call.arguments"]).toContain(
        REDACTED,
      );
      expect(attributes(tool)["gen_ai.tool.call.result"]).toContain(REDACTED);
    } else {
      expect(attributes(tool)["gen_ai.tool.call.arguments"]).toBeUndefined();
      expect(attributes(tool)["gen_ai.tool.call.result"]).toBeUndefined();
    }
    const metrics = metricsFrom(receiver.requests);
    const durations = metrics
      .filter((metric) => metric.name === "gen_ai.client.operation.duration")
      .flatMap((metric) => metric.histogram.dataPoints);
    expect(
      durations.some(
        (point) =>
          attributes(point)["gen_ai.operation.name"] === "execute_tool" &&
          point.sum === 0.4,
      ),
    ).toBe(true);
    expect(
      durations.some(
        (point) =>
          attributes(point)["gen_ai.operation.name"] === "streamText" &&
          point.sum === 1,
      ),
    ).toBe(true);
    const compactionDurations = durations.filter(
      (point) => attributes(point)["gen_ai.operation.name"] === "generateText",
    );
    expect(compactionDurations).toHaveLength(1);
    expect(compactionDurations[0].count).toBe(1);
    expect(compactionDurations[0].sum).toBe(0);
    const firstToken = metrics
      .filter((metric) => metric.name === "gen_ai.client.time_to_first_token")
      .flatMap((metric) => metric.histogram.dataPoints);
    expect(firstToken).toHaveLength(1);
    expect(firstToken[0].sum).toBe(0.15);
    expect(firstToken[0].count).toBe(1);
    const tokens = metrics
      .filter((metric) => metric.name === "gen_ai.client.token.usage")
      .flatMap((metric) => metric.histogram.dataPoints);
    for (const [type, count] of Object.entries({
      input: 250,
      output: 60,
      cache_read: 30,
      cache_write: 20,
      reasoning: 10,
    })) {
      const point = tokens.find(
        (point) =>
          attributes(point)["gen_ai.token.type"] === type &&
          attributes(point)["gen_ai.operation.name"] === "streamText",
      );
      expect(point, `token metric ${type}`).toBeDefined();
      expect(point.sum).toBe(count);
      expect(attributes(point)["gen_ai.token.semantics"]).toBe("inclusive");
    }
    for (const [type, count] of Object.entries({ input: 100, output: 20 })) {
      const points = tokens.filter(
        (point) =>
          attributes(point)["gen_ai.token.type"] === type &&
          attributes(point)["gen_ai.operation.name"] === "generateText",
      );
      expect(points).toHaveLength(1);
      expect(points[0].count).toBe(1);
      expect(points[0].sum).toBe(count);
    }
  }

  it("matches the full native golden and redacts locally with guards disabled", async () => {
    const { turn, exports } = await run("full");
    expect(turn.input[2].parts[0].text).toBe(
      `list go files; token: ${REDACTED}; DATABASE_PASSWORD=[REDACTED:env-secret-value]`,
    );
    expect(outputPart(turn, "text")).toBe(
      `I will list the Go files; token: ${REDACTED}`,
    );
    expect(outputPart(turn, "thinking")).toBe(
      `I should inspect the repo; token: ${REDACTED}`,
    );
    expect(JSON.parse(decodeInput(outputPart(turn, "tool_call")))).toEqual({
      command: `ls *.go; echo ${REDACTED}`,
    });
    expect(outputPart(turn, "tool_result").content).toBe(
      `main.go\nutil.go\ntoken: ${REDACTED}\nDATABASE_PASSWORD=[REDACTED:env-secret-value]`,
    );
    assertGolden(exports);
  });

  it("keeps text and tool identity but removes tool bodies in no_tool_content", async () => {
    const { turn } = await run("no_tool_content");
    expect(turn.input[2].parts[0].text).toContain(REDACTED);
    expect(outputPart(turn, "text")).toBe(
      `I will list the Go files; token: ${REDACTED}`,
    );
    expect(outputPart(turn, "thinking")).toContain(REDACTED);
    const call = outputPart(turn, "tool_call");
    expect(call).toMatchObject({
      id: nativeFixture.toolID,
      name: "bash",
      input_json: "",
    });
    expect(outputPart(turn, "tool_result")).toMatchObject({
      tool_call_id: nativeFixture.toolID,
      name: "bash",
      content: "",
      content_json: "",
    });
  });

  it("keeps generation content but removes tool span bodies in full_with_metadata_spans", async () => {
    const { turn } = await run("full_with_metadata_spans");
    expect(JSON.parse(decodeInput(outputPart(turn, "tool_call")))).toEqual({
      command: `ls *.go; echo ${REDACTED}`,
    });
    expect(outputPart(turn, "tool_result").content).toContain(REDACTED);
  });

  it("exports metadata and usage but no transcript or tool bodies in metadata_only", async () => {
    const { turn } = await run("metadata_only");
    expect(turn.input).toEqual([]);
    // The SDK retains structural tool identities in metadata-only mode.
    expect(turn.output).toHaveLength(2);
    expect(outputPart(turn, "tool_call")).toEqual({
      id: nativeFixture.toolID,
      name: "bash",
      input_json: "",
    });
    expect(outputPart(turn, "tool_result")).toEqual({
      tool_call_id: nativeFixture.toolID,
      name: "bash",
      content: "",
      content_json: "",
      is_error: false,
    });
    const parts = turn.output.flatMap((message: any) => message.parts);
    expect(
      parts.every(
        (part: any) => part.text === undefined && part.thinking === undefined,
      ),
    ).toBe(true);
    expect(turn.metadata["agento11y.conversation.title"]).toBeUndefined();
    const serialized = JSON.stringify(receiver.requests);
    for (const body of [
      nativeFixture.title,
      "list go files;",
      "I should inspect",
      "I will list",
      "ls *.go",
      "main.go",
      "Earlier summary",
      "Recent context:",
      "Prefer concise answers",
      "Use the observed repository context",
    ]) {
      expect(serialized).not.toContain(body);
    }
  });
});
