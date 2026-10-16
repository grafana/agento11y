import { createServer, type Server } from "node:http";
import * as zlib from "node:zlib";
import type { Plugin } from "@opencode/plugin";
import type {
  SessionHttpRequest,
  SessionWebSocketSend,
} from "@opencode/plugin/promise/session";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createAgento11yClient } from "./client.js";
import type { Agento11yOpencodeConfig } from "./config.js";
import { createFinalCompactionGuards } from "./v2-request-guards.js";

const HTTP = "session.http.request";
const WS = "session.experimental.ws.send";
const AUTH = "Bearer fixture-token";
const SIGV4 =
  "AWS4-HMAC-SHA256 Credential=AKIDFIXTURE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=fixture-signature";

const fixtures = {
  "openai-chat": {
    url: "https://api.openai.com/v1/chat/completions",
    body: () => ({
      model: "gpt-5",
      stream: true,
      messages: [
        { role: "system", content: "system-secret" },
        { role: "user", content: "summary-secret" },
        { role: "assistant", content: "answer-secret" },
        { role: "user", content: "recent-secret" },
      ],
    }),
  },
  "openai-responses": {
    url: "https://api.openai.com/v1/responses",
    body: (): Record<string, unknown> => ({
      model: "gpt-5",
      stream: true,
      instructions: "system-secret",
      input: [
        {
          type: "message",
          role: "user",
          content: [{ type: "input_text", text: "summary-secret" }],
        },
        { type: "function_call_output", call_id: "c1", output: "tool-secret" },
        { type: "compaction_trigger" },
      ],
    }),
  },
  "openai-responses-compact": {
    url: "https://api.openai.com/v1/responses/compact",
    body: () => ({
      model: "gpt-5",
      input: [
        {
          type: "message",
          role: "user",
          content: [{ type: "input_text", text: "summary-secret" }],
        },
      ],
    }),
  },
  "anthropic-messages": {
    url: "https://api.anthropic.com/v1/messages?beta=true",
    body: () => ({
      model: "claude",
      stream: true,
      max_tokens: 100,
      system: [{ type: "text", text: "system-secret" }],
      messages: [
        {
          role: "user",
          content: [{ type: "text", text: "summary-secret" }],
        },
      ],
    }),
  },
  gemini: {
    url: "https://generativelanguage.googleapis.com/v1beta/models/gemini:streamGenerateContent?alt=sse",
    body: () => ({
      systemInstruction: { parts: [{ text: "system-secret" }] },
      contents: [{ role: "user", parts: [{ text: "summary-secret" }] }],
    }),
  },
  "bedrock-converse": {
    url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v1%3A0/converse-stream",
    body: () => ({
      modelId: "anthropic.claude-v1:0",
      system: [{ text: "system-secret" }],
      messages: [{ role: "user", content: [{ text: "summary-secret" }] }],
    }),
  },
} as const;
type Fixture = keyof typeof fixtures;

function request(
  fixture: Fixture,
  options: {
    body?: unknown;
    headers?: Record<string, string>;
    encode?: (text: string) => Uint8Array;
  } = {},
) {
  const { url, body } = fixtures[fixture];
  const text = JSON.stringify(options.body ?? body());
  const bytes = options.encode?.(text) ?? new TextEncoder().encode(text);
  return new Request(url, {
    method: "POST",
    headers: {
      authorization: AUTH,
      "content-type": "application/json",
      "content-length": String(bytes.byteLength),
      ...options.headers,
    },
    body: new Uint8Array(bytes),
    signal: new AbortController().signal,
  });
}

function httpEvent(req: Request, kind = "compaction"): SessionHttpRequest {
  return {
    sessionID: "ses_final",
    agent: "build",
    model: { providerID: "fixture", id: "model" },
    kind,
    request: req,
  } as unknown as SessionHttpRequest;
}

function frame(extra: Record<string, unknown> = {}) {
  const { stream: _stream, ...body } = fixtures["openai-responses"].body();
  return JSON.stringify({ ...body, type: "response.create", ...extra });
}

function wsEvent(value: string, kind = "compaction"): SessionWebSocketSend {
  return {
    sessionID: "ses_final",
    agent: "build",
    model: { providerID: "fixture", id: "model" },
    kind,
    frame: value,
  } as unknown as SessionWebSocketSend;
}

function host(failAt = 0) {
  const callbacks = new Map<string, (event: any) => Promise<void> | void>();
  const disposed: string[] = [];
  let registrations = 0;
  return {
    callbacks,
    disposed,
    context: {
      session: {
        hook: async (
          name: string,
          callback: (event: any) => Promise<void> | void,
        ) => {
          if (++registrations === failAt)
            throw new Error("registration-secret Authorization: fixture-token");
          const key = `session.${name}`;
          callbacks.set(key, callback);
          return {
            dispose: async () => {
              disposed.push(key);
              callbacks.delete(key);
            },
          };
        },
      },
    } as unknown as Pick<Plugin.Context, "session">,
    async send(key: string, event: any) {
      await callbacks.get(key)?.(event);
    },
  };
}

function redact(value: unknown): any {
  if (typeof value === "string") return value.replaceAll("secret", "clean");
  if (Array.isArray(value)) return value.map(redact);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [key, redact(child)]),
    );
  return value;
}

async function bytes(req: Request) {
  return Buffer.from(await req.clone().arrayBuffer());
}

async function receiver() {
  const calls: { path: string; body: any }[] = [];
  let respond: (body: any) => unknown = () => ({ action: "allow" });
  let status = 200;
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      calls.push({ path: req.url ?? "", body });
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(respond(body)));
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string")
    throw new Error("expected loopback listener");
  return {
    server,
    calls,
    endpoint: `http://127.0.0.1:${address.port}`,
    response(fn: typeof respond, code = 200) {
      respond = fn;
      status = code;
    },
  };
}

function close(server: Server) {
  return new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
}

const redacting = (body: any) => ({
  action: "allow",
  transformed_input: { messages: redact(body.input.messages) },
});
const echoing = (body: any) => ({
  action: "allow",
  transformed_input: { messages: body.input.messages },
});
const denying = () => ({ action: "deny", reason: "fixture policy" });

describe("final compaction guards through the real SDK decoder", () => {
  let wire: Awaited<ReturnType<typeof receiver>>;
  let client: NonNullable<ReturnType<typeof createAgento11yClient>>;
  let config: Agento11yOpencodeConfig;
  let cleanup: (() => Promise<void>) | undefined;
  let warning: ReturnType<typeof vi.spyOn>;
  const errors: unknown[] = [];

  beforeEach(async () => {
    wire = await receiver();
    config = {
      endpoint: wire.endpoint,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "config-version",
      contentCapture: "metadata_only",
      redactInputMessages: true,
      debug: false,
      guards: { enabled: true, failOpen: true, timeoutMs: 1000 },
    };
    const created = createAgento11yClient(config);
    if (!created) throw new Error("fixture client unavailable");
    client = created;
    warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    errors.length = 0;
  });

  afterEach(async () => {
    await cleanup?.();
    cleanup = undefined;
    await client.shutdown();
    await close(wire.server);
    expect(
      wire.calls.every((call) => call.path === "/api/v1/hooks:evaluate"),
    ).toBe(true);
    const diagnostics = JSON.stringify([
      warning.mock.calls,
      errors.map(String),
    ]);
    expect(diagnostics).not.toMatch(
      /secret|fixture-token|fixture-signature|AKIDFIXTURE|Authorization|Bearer/,
    );
    warning.mockRestore();
  });

  async function setup(failAt = 0) {
    const native = host(failAt);
    cleanup = await createFinalCompactionGuards({
      client,
      config,
      context: native.context,
      agentVersion: "plugin-version",
    }).register();
    return native;
  }

  async function rejected(pending: unknown) {
    const error = await Promise.resolve(pending).then(
      () => undefined,
      (failure: unknown) => failure,
    );
    if (!error) throw new Error("expected the attempt to be blocked");
    errors.push(error);
    return String(error);
  }

  it.each([
    undefined,
    false,
  ])("registers nothing when guards.enabled is %s", async (enabled) => {
    config.guards =
      enabled === undefined ? undefined : { ...config.guards!, enabled };
    const native = await setup();
    expect(native.callbacks.size).toBe(0);
    await native.send(HTTP, httpEvent(request("openai-chat")));
    await native.send(WS, wsEvent(frame()));
    expect(wire.calls).toHaveLength(0);
  });

  it.each([
    "primary",
    "title",
    "generate",
  ])("ignores %s requests without evaluating them", async (kind) => {
    wire.response(denying);
    const native = await setup();
    const original = request("openai-chat", {
      headers: { "content-encoding": "br" },
    });
    const event = httpEvent(original, kind);
    await native.send(HTTP, event);
    const ws = wsEvent("not json", kind);
    await native.send(WS, ws);
    expect(event.request).toBe(original);
    expect(ws.frame).toBe("not json");
    expect(wire.calls).toHaveLength(0);
  });

  it.each(
    Object.keys(fixtures) as Fixture[],
  )("redacts the %s compaction body that is actually sent", async (fixture) => {
    wire.response(redacting);
    const native = await setup();
    const original = request(fixture);
    const before = await bytes(original);
    const event = httpEvent(original);
    await native.send(HTTP, event);
    const sent = event.request;
    expect(sent).not.toBe(original);
    const body = await bytes(sent);
    expect(body.toString("utf8")).not.toContain("secret");
    expect(JSON.parse(body.toString("utf8"))).toEqual(
      redact(fixtures[fixture].body()),
    );
    expect(sent.headers.get("content-length")).toBe(String(body.byteLength));
    expect(sent.headers.get("authorization")).toBe(AUTH);
    expect(sent.headers.get("content-type")).toBe("application/json");
    expect(sent.method).toBe("POST");
    expect(sent.url).toBe(original.url);
    expect(await bytes(original)).toEqual(before);
    expect(wire.calls).toHaveLength(1);
    expect(wire.calls[0].body).toMatchObject({
      phase: "preflight",
      context: {
        agent_name: "opencode:build",
        agent_version: "plugin-version",
        conversation_id: "ses_final",
        model: { provider: "fixture", name: "model" },
      },
    });
    expect(JSON.stringify(wire.calls[0].body.input)).toContain(
      "summary-secret",
    );
  });

  it("forwards the caller's abort signal on a rebuilt request", async () => {
    wire.response(redacting);
    const native = await setup();
    const controller = new AbortController();
    const original = new Request(fixtures["openai-chat"].url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(fixtures["openai-chat"].body()),
      signal: controller.signal,
    });
    const event = httpEvent(original);
    await native.send(HTTP, event);
    expect(event.request.headers.has("content-length")).toBe(false);
    controller.abort();
    expect(event.request.signal.aborted).toBe(true);
  });

  it.each([
    ["no transform", () => ({ action: "allow" })],
    ["an unchanged transform", echoing],
  ])("keeps the original request object and bytes on allow with %s", async (_name, respond) => {
    wire.response(respond);
    const native = await setup();
    const original = request("anthropic-messages");
    const before = await bytes(original);
    const event = httpEvent(original);
    await native.send(HTTP, event);
    expect(event.request).toBe(original);
    expect(await bytes(event.request)).toEqual(before);
    expect(wire.calls).toHaveLength(1);
  });

  it("blocks a denied compaction attempt on HTTP and WebSocket", async () => {
    wire.response(denying);
    const native = await setup();
    const original = request("gemini");
    const event = httpEvent(original);
    expect(await rejected(native.send(HTTP, event))).toContain(
      "fixture policy",
    );
    expect(event.request).toBe(original);
    const ws = wsEvent(frame());
    const value = ws.frame;
    expect(await rejected(native.send(WS, ws))).toContain("fixture policy");
    expect(ws.frame).toBe(value);
    expect(wire.calls).toHaveLength(2);
  });

  it.each([
    [
      "an unknown endpoint",
      () =>
        new Request("https://api.example.com/v1/complete", {
          method: "POST",
          body: JSON.stringify(fixtures["openai-chat"].body()),
        }),
    ],
    [
      "an unsupported encoding",
      () => request("openai-chat", { headers: { "content-encoding": "br" } }),
    ],
    [
      "a corrupt gzip body",
      () => request("openai-chat", { headers: { "content-encoding": "gzip" } }),
    ],
    [
      "an unparseable body",
      () =>
        new Request(fixtures["openai-chat"].url, {
          method: "POST",
          body: '{"messages": [secret',
        }),
    ],
    ["a bodiless request", () => new Request(fixtures["openai-chat"].url)],
    [
      "an oversized body",
      () =>
        request("openai-chat", {
          headers: { "content-length": String(64 * 1024 * 1024) },
        }),
    ],
    [
      "an unknown message shape",
      () =>
        request("openai-chat", {
          body: {
            messages: [{ role: "user", content: "x-secret", name: "fixture" }],
          },
        }),
    ],
  ])("refuses %s even with failOpen", async (_name, build) => {
    wire.response(denying);
    const native = await setup();
    const original = build();
    const event = httpEvent(original);
    expect(await rejected(native.send(HTTP, event))).toContain(
      "could not verify a compaction request",
    );
    expect(event.request).toBe(original);
    expect(wire.calls).toHaveLength(0);
  });

  const zstd = (zlib as any).zstdCompressSync as
    | ((input: string) => Uint8Array)
    | undefined;
  it.each([
    [
      "gzip",
      (text: string) => zlib.gzipSync(text),
      (data: Buffer) => zlib.gunzipSync(data),
    ],
    ...(zstd
      ? [
          [
            "zstd",
            zstd,
            (data: Buffer) => (zlib as any).zstdDecompressSync(data) as Buffer,
          ] as const,
        ]
      : []),
  ] as const)("re-compresses a redacted %s body with the same encoding", async (encoding, encode, decode) => {
    wire.response(redacting);
    const native = await setup();
    const event = httpEvent(
      request("openai-responses", {
        headers: { "content-encoding": encoding },
        encode,
      }),
    );
    await native.send(HTTP, event);
    expect(event.request.headers.get("content-encoding")).toBe(encoding);
    const sent = await bytes(event.request);
    expect(event.request.headers.get("content-length")).toBe(
      String(sent.byteLength),
    );
    expect(JSON.parse(Buffer.from(decode(sent)).toString("utf8"))).toEqual(
      redact(fixtures["openai-responses"].body()),
    );
  });

  it("refuses zstd bodies when the runtime has no zstd", async () => {
    vi.resetModules();
    vi.doMock("node:zlib", async (original) => ({
      ...(await original<typeof import("node:zlib")>()),
      zstdCompressSync: undefined,
      zstdDecompressSync: undefined,
    }));
    try {
      const { createFinalCompactionGuards: create } = await import(
        "./v2-request-guards.js"
      );
      const native = host();
      cleanup = await create({
        client,
        config,
        context: native.context,
      }).register();
      const original = request("openai-chat", {
        headers: { "content-encoding": "zstd" },
        encode: zstd,
      });
      const event = httpEvent(original);
      expect(await rejected(native.send(HTTP, event))).toContain("(encoding)");
      expect(event.request).toBe(original);
      expect(wire.calls).toHaveLength(0);
    } finally {
      vi.doUnmock("node:zlib");
      vi.resetModules();
    }
  });

  describe("signed Bedrock requests", () => {
    const signedRequest = () =>
      request("bedrock-converse", {
        headers: {
          authorization: SIGV4,
          "x-amz-date": "20260101T000000Z",
          "x-amz-content-sha256": "fixture-hash",
        },
      });

    it("refuses a transform it cannot apply without breaking the signature", async () => {
      wire.response(redacting);
      const native = await setup();
      const original = signedRequest();
      const event = httpEvent(original);
      expect(await rejected(native.send(HTTP, event))).toContain("(rewrite)");
      expect(event.request).toBe(original);
    });

    it.each([
      ["deny", denying, "fixture policy"],
      ["allow", () => ({ action: "allow" }), undefined],
      ["an unchanged transform", echoing, undefined],
    ] as const)("still enforces %s", async (_name, respond, reason) => {
      wire.response(respond);
      const native = await setup();
      const original = signedRequest();
      const event = httpEvent(original);
      if (reason)
        expect(await rejected(native.send(HTTP, event))).toContain(reason);
      else await native.send(HTTP, event);
      expect(event.request).toBe(original);
    });
  });

  it("refuses a transform on a stateful Responses continuation but allows it unchanged", async () => {
    const body = {
      ...fixtures["openai-responses"].body(),
      previous_response_id: "resp_1",
    };
    const native = await setup();
    wire.response(redacting);
    const refused = httpEvent(request("openai-responses", { body }));
    expect(await rejected(native.send(HTTP, refused))).toContain("(rewrite)");
    wire.response(() => ({ action: "allow" }));
    const original = request("openai-responses", { body });
    const allowed = httpEvent(original);
    await native.send(HTTP, allowed);
    expect(allowed.request).toBe(original);
  });

  it.each([
    ["allow", () => ({ action: "allow" }), undefined],
    ["an unchanged transform", echoing, undefined],
    ["deny", denying, "fixture policy"],
    ["a required transform", redacting, "(rewrite)"],
  ] as const)("inspects WebSocket frames on %s without rewriting them", async (_name, respond, failure) => {
    wire.response(respond);
    const native = await setup();
    const value = frame();
    const event = wsEvent(value);
    if (failure)
      expect(await rejected(native.send(WS, event))).toContain(failure);
    else await native.send(WS, event);
    expect(event.frame).toBe(value);
    expect(wire.calls).toHaveLength(1);
  });

  it("refuses WebSocket frames it cannot inspect", async () => {
    const native = await setup();
    const event = wsEvent(JSON.stringify({ type: "session.update" }));
    expect(await rejected(native.send(WS, event))).toContain("(shape)");
    expect(wire.calls).toHaveLength(0);
  });

  it.each([
    true,
    false,
  ])("lets only evaluator outages honor failOpen=%s", async (failOpen) => {
    config.guards!.failOpen = failOpen;
    wire.response(
      () => ({ detail: "Authorization fixture-token source-secret" }),
      503,
    );
    const native = await setup();
    const original = request("openai-chat");
    const event = httpEvent(original);
    if (failOpen) await native.send(HTTP, event);
    else
      expect(await rejected(native.send(HTTP, event))).toContain(
        "could not evaluate",
      );
    expect(event.request).toBe(original);
  });

  it("refuses malformed transforms from the real decoder even with failOpen", async () => {
    wire.response(() => ({
      action: "allow",
      transformed_input: {
        messages: [{ role: "user", parts: [{ text: 12 }] }],
      },
    }));
    const native = await setup();
    const original = request("openai-chat");
    const event = httpEvent(original);
    expect(await rejected(native.send(HTTP, event))).toMatch(/transform/);
    expect(event.request).toBe(original);
  });

  it.each([
    [
      "role",
      (messages: any[]) => {
        messages[1].role = "system";
      },
    ],
    ["message count", (messages: any[]) => messages.pop()],
  ])("refuses a decoded transform that changes %s", async (_name, change) => {
    wire.response((body) => {
      const messages = redact(body.input.messages);
      change(messages);
      return { action: "allow", transformed_input: { messages } };
    });
    const native = await setup();
    const original = request("openai-chat");
    const event = httpEvent(original);
    expect(await rejected(native.send(HTTP, event))).toContain("(transform)");
    expect(event.request).toBe(original);
  });

  it("joins an active check during cleanup and rejects checks after it", async () => {
    let release!: () => void;
    const evaluated = new Promise<void>((resolve) => {
      release = resolve;
    });
    const evaluateHook = client.evaluateHook.bind(client);
    vi.spyOn(client, "evaluateHook").mockImplementation(async (...args) => {
      await evaluated;
      return evaluateHook(...args);
    });
    wire.response(denying);
    const native = await setup();
    const callback = native.callbacks.get(HTTP)!;
    const pending = callback(httpEvent(request("openai-chat")));
    let disposed = false;
    const disposing = cleanup!().then(() => {
      disposed = true;
    });
    await Promise.resolve();
    expect(disposed).toBe(false);
    expect(
      await rejected(callback(httpEvent(request("openai-chat")))),
    ).toContain("shutting down");
    await callback(httpEvent(request("openai-chat"), "primary"));
    release();
    expect(await rejected(pending)).toContain("fixture policy");
    await disposing;
    expect(disposed).toBe(true);
    expect(native.callbacks.size).toBe(0);
    cleanup = undefined;
  });

  it.each([
    1, 2,
  ])("rolls back acquired registrations when registration %i fails", async (failAt) => {
    const native = host(failAt);
    const error = await rejected(
      createFinalCompactionGuards({
        client,
        config,
        context: native.context,
      }).register(),
    );
    expect(error).toContain("compaction guard registration failed");
    expect(native.callbacks.size).toBe(0);
    expect(native.disposed).toHaveLength(failAt - 1);
  });

  it("returns memoized cleanup for both registrations", async () => {
    const native = await setup();
    expect([...native.callbacks.keys()]).toEqual([HTTP, WS]);
    const first = cleanup!();
    expect(cleanup!()).toBe(first);
    await first;
    expect(native.disposed).toEqual([WS, HTTP]);
    cleanup = undefined;
  });
});
