import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import { _resetHookState, createAgento11yHooks } from "./hooks.js";
import {
  assistantMessage,
  baseConfig,
  emitMessageUpdated,
} from "./hooks.testutil.js";

const SIGIL_OPENCODE_SCOPE = "sigil-opencode";
const SIGIL_OPERATION_DURATION_METRIC = "gen_ai.client.operation.duration";

interface CapturedRequest {
  url: string | undefined;
  headers: IncomingHttpHeaders;
  body: string;
}

interface OtlpServer {
  endpoint: string;
  requests: CapturedRequest[];
  close: () => Promise<void>;
}

async function startOtlpServer(): Promise<OtlpServer> {
  const requests: CapturedRequest[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      requests.push({
        url: req.url,
        headers: req.headers,
        body: Buffer.concat(chunks).toString("utf-8"),
      });
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("OTLP server did not bind to a TCP port");
  }
  return {
    endpoint: `http://127.0.0.1:${address.port}/otlp`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) =>
        server.close((err) => (err ? reject(err) : resolve())),
      ),
  };
}

async function startAgento11yServer(): Promise<{
  server: Server;
  baseUrl: string;
}> {
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        let parsed: any;
        try {
          parsed = JSON.parse(body);
        } catch {
          res.statusCode = 400;
          res.end("{}");
          return;
        }
        const results = (parsed.generations ?? []).map((g: any) => ({
          generation_id: g?.id ?? "",
          accepted: true,
        }));
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ results }));
      });
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error(
          "Agent Observability server did not bind to a TCP port",
        );
      }
      resolve({ server, baseUrl: `http://127.0.0.1:${addr.port}` });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) =>
    server.close((err) => (err ? reject(err) : resolve())),
  );
}

function opencodeMessageFixture() {
  const sessionID = "otlp-sess-1";
  const messageID = "otlp-msg-1";
  const userMessage = {
    id: "user-1",
    sessionID,
    role: "user",
    time: { created: 1_700_000_000_000 },
    agent: "build",
    model: { providerID: "anthropic", modelID: "claude-sonnet-4-opencode" },
    system: "you are a helpful assistant",
    tools: { Bash: true },
  } as const;
  const userParts = [
    {
      id: "user-text-1",
      sessionID,
      messageID: "user-1",
      type: "text",
      text: "hello",
    },
  ];
  const assistantMessage = {
    id: messageID,
    sessionID,
    role: "assistant",
    time: { created: 1_700_000_001_000, completed: 1_700_000_002_500 },
    parentID: "user-1",
    modelID: "claude-sonnet-4-opencode",
    providerID: "anthropic",
    mode: "build",
    path: { cwd: "/repo", root: "/repo" },
    cost: 0.001,
    tokens: {
      input: 10,
      output: 5,
      reasoning: 0,
      cache: { read: 0, write: 0 },
    },
    finish: "end_turn",
  } as const;
  const assistantParts = [
    {
      id: "assist-text-1",
      sessionID,
      messageID,
      type: "text",
      text: "hi",
    },
    {
      id: "assist-tool-1",
      sessionID,
      messageID,
      type: "tool",
      callID: "tc-otlp-1",
      tool: "Bash",
      state: {
        status: "completed",
        input: { command: "ls" },
        output: "main.go",
        title: "ls",
        metadata: {},
        time: { start: 1_700_000_001_500, end: 1_700_000_002_000 },
      },
    },
  ];
  return {
    sessionID,
    userMessage,
    userParts,
    assistantMessage,
    assistantParts,
  };
}

const ENV_KEYS = [
  "SIGIL_ENDPOINT",
  "SIGIL_AUTH_MODE",
  "SIGIL_AUTH_TENANT_ID",
  "SIGIL_AUTH_TOKEN",
  "SIGIL_AGENT_NAME",
  "SIGIL_AGENT_VERSION",
  "SIGIL_CONTENT_CAPTURE_MODE",
  "SIGIL_DEBUG",
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_EXPORTER_OTLP_HEADERS",
  "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
] as const;

describe("createAgento11yHooks OTLP wiring", () => {
  let otlp: OtlpServer;
  let sigil: { server: Server; baseUrl: string };
  let savedEnv: Record<string, string | undefined> = {};

  beforeEach(async () => {
    _resetHookState();
    const credentialKeys = Object.keys(process.env).filter((key) =>
      /^(?:AGENTO11Y_|SIGIL_|OTEL_)/.test(key),
    );
    for (const k of new Set([...ENV_KEYS, ...credentialKeys])) {
      savedEnv[k] = process.env[k];
      delete process.env[k];
    }
    otlp = await startOtlpServer();
    sigil = await startAgento11yServer();
  });

  afterEach(async () => {
    await closeServer(sigil.server);
    await otlp.close();
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
    savedEnv = {};
    _resetHookState();
  });

  async function runOneTurn(otlpEnabled: boolean) {
    const {
      sessionID,
      userMessage,
      userParts,
      assistantMessage,
      assistantParts,
    } = opencodeMessageFixture();

    const config: Agento11yOpencodeConfig = {
      endpoint: sigil.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "metadata_only",
      redactInputMessages: true,
      debug: false,
      ...(otlpEnabled && {
        otlp: {
          endpoint: otlp.endpoint,
          headers: { "X-Test-Header": "present" },
        },
      }),
    };

    const fakeClient = {
      session: {
        message: async () => ({ data: { parts: assistantParts } }),
      },
    } as any;

    const hooks = await createAgento11yHooks(config, fakeClient);
    if (!hooks)
      throw new Error("expected createAgento11yHooks to return hooks");

    hooks.chatMessage(
      { sessionID },
      { message: userMessage as any, parts: userParts as any },
    );
    await hooks.toolExecuteBefore(
      { sessionID, callID: "tc-otlp-1", tool: "Bash" },
      { args: { command: "ls" } },
    );
    hooks.toolExecuteAfter(
      {
        sessionID,
        callID: "tc-otlp-1",
        tool: "Bash",
        args: { command: "ls" },
      },
      { title: "ls", output: "main.go", metadata: {} },
    );
    await hooks.event({
      event: {
        type: "message.updated",
        properties: { info: assistantMessage as any },
      },
    });
    await hooks.event({
      event: {
        type: "session.idle",
        properties: { info: { id: sessionID } },
      },
    });
    // session.idle's forceFlush is fire-and-forget, so it cannot be awaited
    // here. Disposal is the awaited path: it shuts the providers down, which
    // drains the OTLP exporters.
    await hooks.dispose();
  }

  it("forwards agento11y SDK spans and metrics through the configured OTLP endpoint", async () => {
    await runOneTurn(true);

    const traceReqs = otlp.requests.filter((r) => r.url === "/otlp/v1/traces");
    const metricReqs = otlp.requests.filter(
      (r) => r.url === "/otlp/v1/metrics",
    );
    expect(traceReqs.length).toBeGreaterThan(0);
    expect(metricReqs.length).toBeGreaterThan(0);

    for (const req of [...traceReqs, ...metricReqs]) {
      expect(req.headers["x-test-header"]).toBe("present");
    }

    const traceScopes = new Set<string>();
    type AttributeKV = {
      key?: string;
      value?: {
        stringValue?: string;
        intValue?: string | number;
        doubleValue?: number;
        boolValue?: boolean;
      };
    };
    type Span = {
      name?: string;
      attributes?: AttributeKV[];
    };
    const spans: Span[] = [];
    for (const req of traceReqs) {
      const payload = JSON.parse(req.body) as {
        resourceSpans?: Array<{
          scopeSpans?: Array<{
            scope?: { name?: string };
            spans?: Span[];
          }>;
        }>;
      };
      for (const rs of payload.resourceSpans ?? []) {
        for (const ss of rs.scopeSpans ?? []) {
          if (ss.scope?.name) traceScopes.add(ss.scope.name);
          for (const sp of ss.spans ?? []) spans.push(sp);
        }
      }
    }
    expect(traceScopes.has(SIGIL_OPENCODE_SCOPE)).toBe(true);

    const hasExecuteToolSpan = spans.some((sp) =>
      sp.attributes?.some(
        (a) =>
          a.key === "gen_ai.operation.name" &&
          a.value?.stringValue === "execute_tool",
      ),
    );
    expect(hasExecuteToolSpan).toBe(true);

    let sawDurationMetric = false;
    let sawExecuteToolDuration = false;
    for (const req of metricReqs) {
      const payload = JSON.parse(req.body) as {
        resourceMetrics?: Array<{
          scopeMetrics?: Array<{
            scope?: { name?: string };
            metrics?: Array<{
              name: string;
              histogram?: {
                dataPoints?: Array<{
                  attributes?: AttributeKV[];
                }>;
              };
            }>;
          }>;
        }>;
      };
      for (const rm of payload.resourceMetrics ?? []) {
        for (const sm of rm.scopeMetrics ?? []) {
          if (sm.scope?.name !== SIGIL_OPENCODE_SCOPE) continue;
          for (const m of sm.metrics ?? []) {
            if (m.name !== SIGIL_OPERATION_DURATION_METRIC) continue;
            sawDurationMetric = true;
            for (const dp of m.histogram?.dataPoints ?? []) {
              const op = dp.attributes?.find(
                (a) => a.key === "gen_ai.operation.name",
              );
              if (op?.value?.stringValue === "execute_tool") {
                sawExecuteToolDuration = true;
              }
            }
          }
        }
      }
    }
    expect(sawDurationMetric).toBe(true);
    expect(sawExecuteToolDuration).toBe(true);
  });

  it.each(
    (
      [
        "full",
        "no_tool_content",
        "full_with_metadata_spans",
        "metadata_only",
      ] as const
    ).flatMap((contentCapture) =>
      [false, true].map((generationError) => ({
        contentCapture,
        generationError,
      })),
    ),
  )("exports effective skill children in $contentCapture (generation error=$generationError)", async ({
    contentCapture,
    generationError,
  }) => {
    const sessionID = "skill-otlp-session";
    const messageID = "skill-otlp-message";
    const secret = "glc_abcdefghijklmnopqrstuv";
    const privatePath = "/private/skills/review/SKILL.md";
    const privateURI = "file:///private/skills/review/SKILL.md";
    const specs = [
      {
        id: "effective",
        tool: "skill",
        before: "old",
        after: "team:effective",
        terminal: "old",
        failed: false,
      },
      {
        id: "failure",
        tool: "skill",
        before: "old",
        after: "team:failed",
        terminal: "old",
        failed: true,
      },
      {
        id: "retained",
        tool: "skill",
        before: "team:retained",
        after: undefined,
        terminal: "old",
        failed: false,
      },
      {
        id: "failure-cleared",
        tool: "skill",
        before: "old",
        after: "two words",
        terminal: "old",
        failed: true,
      },
      {
        id: "cleared",
        tool: "skill",
        before: "old",
        after: "two words",
        terminal: "old",
        failed: false,
      },
      {
        id: "secret",
        tool: "skill",
        before: secret,
        after: secret,
        terminal: secret,
        failed: false,
      },
      {
        id: "ordinary",
        tool: "bash",
        before: "review",
        after: "review",
        terminal: "review",
        failed: false,
      },
    ];
    const parts = specs.map((spec) => ({
      id: `part-${spec.id}`,
      sessionID,
      messageID,
      type: "tool",
      callID: spec.id,
      tool: spec.tool,
      state: {
        status: spec.failed ? "error" : "completed",
        input: { name: spec.terminal },
        output: "tool-result-marker",
        error: spec.failed ? "native tool failure" : undefined,
        title: privatePath,
        metadata: {
          path: privatePath,
          uri: privateURI,
          description: "private skill description",
        },
        time: { start: 1_700_000_001_200, end: 1_700_000_001_500 },
      },
    }));
    const fetchMessage = vi.fn(async () => ({ data: { parts } }));
    const hooks = await createAgento11yHooks(
      baseConfig({
        endpoint: sigil.baseUrl,
        contentCapture,
        otlp: { endpoint: otlp.endpoint, headers: {} },
      }),
      { session: { message: fetchMessage } } as any,
    );
    if (!hooks) throw new Error("expected hooks");
    const now = vi.spyOn(Date, "now");
    try {
      for (const spec of specs) {
        const call = { sessionID, callID: spec.id, tool: spec.tool };
        const args = { name: spec.before as string | undefined };
        now.mockReturnValue(1_700_000_001_200);
        await hooks.toolExecuteBefore(call, { args });
        now.mockReturnValue(1_700_000_001_500);
        if (spec.failed) {
          // A later plugin mutates the shared arguments, then the tool throws.
          args.name = spec.after;
        } else {
          hooks.toolExecuteAfter(
            {
              ...call,
              args: spec.after === undefined ? undefined : { name: spec.after },
            },
            {
              title: privatePath,
              output: "tool-result-marker",
              metadata: { uri: privateURI },
            },
          );
        }
      }
      const message = assistantMessage(
        sessionID,
        messageID,
        generationError
          ? {
              error: {
                name: "APIError",
                data: { message: "provider failure", isRetryable: false },
              },
            }
          : {},
      );
      await emitMessageUpdated(hooks, message);
      await emitMessageUpdated(hooks, message);
    } finally {
      now.mockRestore();
      await hooks.dispose();
    }
    expect(fetchMessage).toHaveBeenCalledTimes(
      contentCapture === "metadata_only" ? 0 : 1,
    );

    const spans = otlp.requests
      .filter((req) => req.url === "/otlp/v1/traces")
      .flatMap((req) => JSON.parse(req.body).resourceSpans ?? [])
      .flatMap((resource) => resource.scopeSpans ?? [])
      .flatMap((scope) => scope.spans ?? []);
    const attrs = (item: any): Record<string, any> =>
      Object.fromEntries(
        (item.attributes ?? []).map((attr: any) => [
          attr.key,
          attr.value?.stringValue ??
            attr.value?.intValue ??
            attr.value?.doubleValue ??
            attr.value?.boolValue,
        ]),
      );
    const generationSpans = spans.filter(
      (span) => attrs(span)["gen_ai.operation.name"] !== "execute_tool",
    );
    expect(generationSpans).toHaveLength(1);
    const generation = generationSpans[0];
    expect(generation.status?.code).toBe(generationError ? 2 : 1);
    const tools = spans.filter(
      (span) => attrs(span)["gen_ai.operation.name"] === "execute_tool",
    );
    expect(tools).toHaveLength(specs.length);
    for (const spec of specs) {
      const matches = tools.filter(
        (span) => attrs(span)["gen_ai.tool.call.id"] === spec.id,
      );
      expect(matches).toHaveLength(1);
      const span = matches[0];
      const attributes = attrs(span);
      const expectedSkill =
        spec.id === "effective"
          ? "team:effective"
          : spec.id === "failure"
            ? "team:failed"
            : spec.id === "retained"
              ? "team:retained"
              : undefined;
      expect(span.traceId).toBe(generation.traceId);
      expect(span.parentSpanId).toBe(generation.spanId);
      expect(BigInt(span.startTimeUnixNano)).toBe(1_700_000_001_200_000_000n);
      expect(BigInt(span.endTimeUnixNano)).toBe(1_700_000_001_500_000_000n);
      expect(span.name).toBe(
        `execute_tool ${spec.tool}${expectedSkill ? ` ${expectedSkill}` : ""}`,
      );
      expect(attributes["gen_ai.tool.name"]).toBe(spec.tool);
      expect(attributes["gen_ai.tool.type"]).toBe("function");
      expect(attributes["gen_ai.skill.name"]).toBe(expectedSkill);
      expect(span.status?.code).toBe(spec.failed ? 2 : 1);
      expect(attributes["error.type"]).toBe(
        spec.failed ? "tool_execution_error" : undefined,
      );
      expect(attributes["gen_ai.tool.call.arguments"] !== undefined).toBe(
        contentCapture === "full",
      );
      expect(attributes["gen_ai.tool.call.result"] !== undefined).toBe(
        contentCapture === "full" && !spec.failed,
      );
      for (const key of Object.keys(attributes)) {
        if (key.startsWith("gen_ai.skill."))
          expect(key).toBe("gen_ai.skill.name");
        expect(key).not.toMatch(/(?:path|uri|description)$/i);
      }
    }
    const wire = JSON.stringify(spans);
    expect(wire).not.toContain(privatePath);
    expect(wire).not.toContain(privateURI);
    expect(wire).not.toContain(secret);
    if (contentCapture !== "full")
      expect(wire).not.toContain("tool-result-marker");

    const metrics = otlp.requests
      .filter((req) => req.url === "/otlp/v1/metrics")
      .flatMap((req) => JSON.parse(req.body).resourceMetrics ?? [])
      .flatMap((resource) => resource.scopeMetrics ?? [])
      .flatMap((scope) => scope.metrics ?? []);
    const duration = metrics
      .filter((metric) => metric.name === SIGIL_OPERATION_DURATION_METRIC)
      .flatMap((metric) => metric.histogram?.dataPoints ?? []);
    expect([...new Set(metrics.map((metric) => metric.name))].sort()).toEqual([
      "gen_ai.client.operation.duration",
      "gen_ai.client.token.usage",
      "gen_ai.client.tool_calls_per_operation",
    ]);
    const bounds = [
      0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48,
      40.96, 81.92,
    ];
    const identity = {
      "gen_ai.agent.name": "opencode:build",
      "gen_ai.agent.version": "test-version",
      "gen_ai.provider.name": "anthropic",
      "gen_ai.request.model": "claude-sonnet-4",
    };
    const expectedTools = [
      { tool: "skill", skill: "team:effective", failed: false, count: 1 },
      { tool: "skill", skill: "team:failed", failed: true, count: 1 },
      { tool: "skill", skill: "team:retained", failed: false, count: 1 },
      { tool: "skill", skill: undefined, failed: false, count: 2 },
      { tool: "skill", skill: undefined, failed: true, count: 1 },
      { tool: "bash", skill: undefined, failed: false, count: 1 },
    ].map(({ tool, skill, failed, count }) => ({
      attributes: {
        ...identity,
        "gen_ai.operation.name": "execute_tool",
        "gen_ai.tool.name": tool,
        "error.type": failed ? "tool_execution_error" : "",
        "error.category": failed ? "sdk_error" : "",
        ...(skill && {
          "agento11y.tool.execution.kind": "load_skill",
          "gen_ai.skill.name": skill,
        }),
      },
      count,
      sum: 0.3 * count,
      min: 0.3,
      max: 0.3,
      explicitBounds: bounds,
      bucketCounts: bounds
        .concat(Infinity)
        .map((_, index) => (index === 5 ? count : 0)),
    }));
    const expectedDuration = [
      ...expectedTools,
      {
        attributes: {
          ...identity,
          "gen_ai.operation.name": "streamText",
          "error.type": generationError ? "provider_call_error" : "",
          "error.category": generationError ? "sdk_error" : "",
        },
        count: 1,
        sum: 1.5,
        min: 1.5,
        max: 1.5,
        explicitBounds: bounds,
        bucketCounts: bounds
          .concat(Infinity)
          .map((_, index) => (index === 8 ? 1 : 0)),
      },
    ];
    const actualDuration = duration.map((point) => ({
      attributes: attrs(point),
      count: Number(point.count),
      sum: point.sum,
      min: point.min,
      max: point.max,
      explicitBounds: point.explicitBounds,
      bucketCounts: point.bucketCounts.map(Number),
    }));
    expect(actualDuration).toHaveLength(expectedDuration.length);
    expect(actualDuration).toEqual(expect.arrayContaining(expectedDuration));
    expect(actualDuration.reduce((sum, point) => sum + point.count, 0)).toBe(
      specs.length + 1,
    );
    const allowedKeys = new Set([
      ...Object.keys(identity),
      "gen_ai.operation.name",
      "gen_ai.tool.name",
      "error.type",
      "error.category",
      "agento11y.tool.execution.kind",
      "gen_ai.skill.name",
      "gen_ai.token.type",
      "gen_ai.token.semantics",
    ]);
    for (const metric of metrics) {
      const points =
        metric.histogram?.dataPoints ?? metric.sum?.dataPoints ?? [];
      for (const point of points) {
        for (const key of Object.keys(attrs(point))) {
          expect(allowedKeys.has(key), key).toBe(true);
        }
        if (attrs(point)["gen_ai.operation.name"] !== "execute_tool") {
          expect(attrs(point)).not.toHaveProperty("gen_ai.skill.name");
          expect(attrs(point)).not.toHaveProperty(
            "agento11y.tool.execution.kind",
          );
        }
      }
    }
    const metricWire = JSON.stringify(metrics);
    for (const value of [
      secret,
      privatePath,
      privateURI,
      "private skill description",
      "tool-result-marker",
      "two words",
      "old",
    ]) {
      expect(metricWire).not.toContain(value);
    }
  });

  it("does not contact the OTLP endpoint when config.otlp is absent", async () => {
    await runOneTurn(false);
    expect(otlp.requests).toEqual([]);
  });

  // A tool that fires tool.execute.before but never tool.execute.after
  // (errored or denied upstream). In metadata_only the terminal parts are not
  // fetched, so the only source for a span is the drained active entry.
  async function runNeverCompletedTurn() {
    const sessionID = "otlp-sess-fail";
    const userMessage = {
      id: "user-fail-1",
      sessionID,
      role: "user",
      time: { created: 1_700_000_000_000 },
      system: "you are a helpful assistant",
      tools: { bash: true },
    };
    const assistantMessage = {
      id: "otlp-msg-fail",
      sessionID,
      role: "assistant",
      time: { created: 1_700_000_001_000, completed: 1_700_000_002_500 },
      parentID: "user-fail-1",
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
    };

    const config: Agento11yOpencodeConfig = {
      endpoint: sigil.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "metadata_only",
      redactInputMessages: true,
      debug: false,
      otlp: {
        endpoint: otlp.endpoint,
        headers: { "X-Test-Header": "present" },
      },
    };

    const fakeClient = {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any;

    const hooks = await createAgento11yHooks(config, fakeClient);
    if (!hooks)
      throw new Error("expected createAgento11yHooks to return hooks");

    hooks.chatMessage(
      { sessionID },
      { message: userMessage as any, parts: [] as any },
    );
    await hooks.toolExecuteBefore(
      { sessionID, callID: "tc-fail-1", tool: "bash" },
      { args: { command: "false" } },
    );
    // No toolExecuteAfter.
    await hooks.event({
      event: {
        type: "message.updated",
        properties: { info: assistantMessage as any },
      },
    });
    await hooks.event({
      event: {
        type: "session.idle",
        properties: { info: { id: sessionID } },
      },
    });
    await hooks.dispose();
  }

  it("exports an error execute_tool span for a tool that never completes in metadata_only", async () => {
    await runNeverCompletedTurn();

    const traceReqs = otlp.requests.filter((r) => r.url === "/otlp/v1/traces");
    expect(traceReqs.length).toBeGreaterThan(0);

    type AttributeKV = { key?: string; value?: { stringValue?: string } };
    type Span = { name?: string; attributes?: AttributeKV[] };
    const spans: Span[] = [];
    for (const req of traceReqs) {
      const payload = JSON.parse(req.body) as {
        resourceSpans?: Array<{
          scopeSpans?: Array<{ spans?: Span[] }>;
        }>;
      };
      for (const rs of payload.resourceSpans ?? []) {
        for (const ss of rs.scopeSpans ?? []) {
          for (const sp of ss.spans ?? []) spans.push(sp);
        }
      }
    }

    const toolSpans = spans.filter((sp) =>
      sp.attributes?.some(
        (a) =>
          a.key === "gen_ai.operation.name" &&
          a.value?.stringValue === "execute_tool",
      ),
    );
    expect(toolSpans).toHaveLength(1);

    const toolSpan = toolSpans[0]!;
    const callId = toolSpan.attributes?.find(
      (a) => a.key === "gen_ai.tool.call.id",
    );
    expect(callId?.value?.stringValue).toBe("tc-fail-1");
    const errorType = toolSpan.attributes?.find((a) => a.key === "error.type");
    expect(errorType?.value?.stringValue).toBe("tool_execution_error");
  });
});
