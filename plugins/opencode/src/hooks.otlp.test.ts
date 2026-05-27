import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { SigilOpencodeConfig } from "./config.js";
import { createSigilHooks } from "./hooks.js";

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

async function startSigilServer(): Promise<{
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
        throw new Error("Sigil server did not bind to a TCP port");
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

describe("createSigilHooks OTLP wiring", () => {
  let otlp: OtlpServer;
  let sigil: { server: Server; baseUrl: string };
  let savedEnv: Record<string, string | undefined> = {};

  beforeEach(async () => {
    for (const k of ENV_KEYS) {
      savedEnv[k] = process.env[k];
      delete process.env[k];
    }
    otlp = await startOtlpServer();
    sigil = await startSigilServer();
  });

  afterEach(async () => {
    await closeServer(sigil.server);
    await otlp.close();
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
    savedEnv = {};
  });

  async function runOneTurn(otlpEnabled: boolean) {
    const {
      sessionID,
      userMessage,
      userParts,
      assistantMessage,
      assistantParts,
    } = opencodeMessageFixture();

    const config: SigilOpencodeConfig = {
      endpoint: sigil.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "metadata_only",
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

    const hooks = await createSigilHooks(config, fakeClient);
    if (!hooks) throw new Error("expected createSigilHooks to return hooks");

    hooks.chatMessage(
      { sessionID },
      { message: userMessage as any, parts: userParts as any },
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
    // session.idle's forceFlush is fire-and-forget; only global.disposed
    // awaits shutdown, which drains the OTLP exporters.
    await hooks.event({
      event: { type: "global.disposed", properties: {} },
    });
  }

  it("forwards Sigil SDK spans and metrics through the configured OTLP endpoint", async () => {
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
    for (const req of traceReqs) {
      const payload = JSON.parse(req.body) as {
        resourceSpans?: Array<{
          scopeSpans?: Array<{ scope?: { name?: string } }>;
        }>;
      };
      for (const rs of payload.resourceSpans ?? []) {
        for (const ss of rs.scopeSpans ?? []) {
          if (ss.scope?.name) traceScopes.add(ss.scope.name);
        }
      }
    }
    expect(traceScopes.has(SIGIL_OPENCODE_SCOPE)).toBe(true);

    let sawDurationMetric = false;
    for (const req of metricReqs) {
      const payload = JSON.parse(req.body) as {
        resourceMetrics?: Array<{
          scopeMetrics?: Array<{
            scope?: { name?: string };
            metrics?: Array<{ name: string }>;
          }>;
        }>;
      };
      for (const rm of payload.resourceMetrics ?? []) {
        for (const sm of rm.scopeMetrics ?? []) {
          if (sm.scope?.name !== SIGIL_OPENCODE_SCOPE) continue;
          if (
            sm.metrics?.some((m) => m.name === SIGIL_OPERATION_DURATION_METRIC)
          ) {
            sawDurationMetric = true;
          }
        }
      }
    }
    expect(sawDurationMetric).toBe(true);
  });

  it("does not contact the OTLP endpoint when config.otlp is absent", async () => {
    await runOneTurn(false);
    expect(otlp.requests).toEqual([]);
  });
});
