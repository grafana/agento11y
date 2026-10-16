import { createServer, type Server } from "node:http";
import {
  type HookEvaluateRequest,
  HookTransformError,
} from "@grafana/agento11y";
import type { Plugin } from "@opencode/plugin";
import type { PermissionEvaluation } from "@opencode/plugin/promise/permission";
import type {
  SessionPrompt,
  SessionRequest,
} from "@opencode/plugin/promise/session";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createAgento11yClient } from "./client.js";
import type { Agento11yOpencodeConfig } from "./config.js";
import {
  applyNativeTransform,
  createNativeGuards,
  NativeGuardTransformError,
  nativePermissionInput,
  nativePromptMessages,
  projectNativeRequest,
} from "./v2-guards.js";

function request(): SessionRequest {
  return {
    sessionID: "ses_guard",
    model: { providerID: "fixture", id: "model" },
    system: [
      { type: "text", text: "system-secret", cache: { type: "ephemeral" } },
    ],
    messages: [
      { role: "user", content: [{ type: "text", text: "user-secret" }] },
      {
        role: "assistant",
        content: [
          { type: "reasoning", text: "reasoning-secret" },
          { type: "text", text: "answer-secret" },
        ],
      },
    ],
    options: { temperature: 0.2 },
  } as unknown as SessionRequest;
}
function tool() {
  return {
    sessionID: "ses_guard",
    messageID: "msg_guard",
    agent: "build",
    id: "call_guard",
    tool: "bash",
    input: { command: "tool-secret" },
  };
}
function permission() {
  return {
    sessionID: "ses_guard",
    agent: "build",
    action: "bash",
    resources: ["permission-secret"],
    metadata: { command: "tool-secret" },
    source: { type: "tool", id: "call_guard", messageID: "msg_guard" },
    effect: "ask",
    message: "host question",
  };
}
function prompt(): SessionPrompt {
  return {
    sessionID: "ses_guard",
    messageID: "msg_guard",
    delivery: "queued",
    prompt: {
      text: "prompt-secret",
      files: [{ uri: "file:///fixture/image.png", name: "image.png" }],
      agents: [],
      skills: [],
    },
  } as unknown as SessionPrompt;
}
function toolTransform(
  input: unknown = { command: "clean" },
  extra: Record<string, unknown> = {},
) {
  return {
    output: [
      {
        role: "assistant",
        parts: [
          {
            type: "tool_call",
            toolCall: {
              id: "call_guard",
              name: "bash",
              inputJSON: JSON.stringify(input),
              ...extra,
            },
          },
        ],
      },
    ],
  };
}

function host(failAt = 0) {
  const callbacks = new Map<string, (event: any) => Promise<void> | void>();
  const disposed: string[] = [];
  const sessionGet = vi.fn(
    async (_input: { sessionID: string }): Promise<any> => ({}),
  );
  let registrations = 0;
  const hook =
    (domain: string) =>
    async (name: string, callback: (event: any) => Promise<void> | void) => {
      if (++registrations === failAt)
        throw new Error(
          "registration-source-secret Authorization: fixture-token",
        );
      const key = `${domain}.${name}`;
      callbacks.set(key, callback);
      return {
        dispose: async () => {
          disposed.push(key);
          callbacks.delete(key);
        },
      };
    };
  return {
    callbacks,
    disposed,
    sessionGet,
    context: {
      app: { name: "opencode", version: "2.0.29", channel: "latest" },
      session: { hook: hook("session"), get: sessionGet },
      tool: { hook: hook("tool") },
      permission: { hook: hook("permission") },
    } as unknown as Plugin.Context,
    async send(key: string, event: any, consumer: () => void) {
      await callbacks.get(key)?.(event);
      if (key !== "permission.evaluate" || event.effect !== "deny") consumer();
    },
  };
}

const providerHooks = ["context", "compaction", "title", "generate"] as const;
const allHooks = [
  "session.prompt",
  ...providerHooks.map((name) => `session.${name}`),
  "tool.execute.before",
  "permission.evaluate",
];
function eventFor(key: string): any {
  if (key === "session.prompt") return prompt();
  if (key === "tool.execute.before") return tool();
  if (key === "permission.evaluate") return permission();
  return request();
}

function transformText(value: unknown): any {
  if (typeof value === "string") return value.replaceAll("secret", "clean");
  if (Array.isArray(value)) return value.map(transformText);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [key, transformText(child)]),
    );
  return value;
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

describe("native guard source slots", () => {
  it("projects prompt admission without attachment or stored-prompt mutation", () => {
    const event = prompt();
    const before = structuredClone(event);
    expect(nativePromptMessages(event)).toEqual([
      { role: "user", parts: [{ type: "text", text: "prompt-secret" }] },
    ]);
    expect(event).toEqual(before);
    expect(
      nativePermissionInput(permission() as unknown as PermissionEvaluation),
    ).toEqual({
      resources: ["permission-secret"],
      metadata: { command: "tool-secret" },
    });
  });

  it("preserves order, media, unsupported parts, cache hints and identity while replacing only text slots", () => {
    const event: any = request();
    const image = {
      type: "media",
      media: {
        mediaType: "image/png",
        source: { type: "bytes", data: new Uint8Array([1, 2, 3]) },
      },
      filename: "image.png",
    };
    const opaque = { type: "future", data: { bytes: "opaque" } };
    event.messages.splice(1, 0, {
      id: "media-only",
      role: "user",
      content: [image, opaque],
    });
    event.messages[0].metadata = { retained: true };
    event.messages[0].content.push(image);
    event.messages.push({
      role: "assistant",
      content: [
        {
          type: "tool-call",
          id: "call",
          name: "read",
          input: { path: "keep" },
        },
      ],
    });
    event.messages.push({
      role: "tool",
      content: [
        {
          type: "tool-result",
          id: "call",
          name: "read",
          result: {
            type: "content",
            value: [
              { type: "text", text: "result-secret" },
              { type: "file", uri: "file:///fixture", mime: "image/png" },
            ],
          },
        },
      ],
    });
    const before = structuredClone(event);
    const projection = projectNativeRequest(event);
    expect(projection.messages.map((message) => message.role)).toEqual([
      "system",
      "user",
      "assistant",
      "assistant",
      "tool",
    ]);
    const transformed = structuredClone(projection.messages);
    for (const message of transformed)
      for (const part of message.parts ?? []) {
        if (part.type === "text")
          part.text = part.text.replace("secret", "clean");
        if (part.type === "thinking")
          part.thinking = part.thinking.replace("secret", "clean");
        if (part.type === "tool_result" && part.toolResult.content)
          part.toolResult.content = "result-clean";
      }
    const output = applyNativeTransform(event, projection, transformed);
    expect(output.system[0]).toEqual({
      ...event.system[0],
      text: "system-clean",
    });
    expect(output.messages[0].content[0]).toEqual({
      type: "text",
      text: "user-clean",
    });
    expect(output.messages[0].content[1]).toBe(image);
    expect(output.messages[1]).toBe(event.messages[1]);
    expect(output.messages[2].content[0]).toEqual({
      type: "reasoning",
      text: "reasoning-clean",
    });
    expect(output.messages[3]).toBe(event.messages[3]);
    expect(output.messages[4].content[0]).toMatchObject({
      result: {
        value: [{ type: "text", text: "result-clean" }, { type: "file" }],
      },
    });
    expect(event).toEqual(before);
  });

  it.each([
    [
      "Gemini signed text",
      (event: any) => {
        event.messages[0].content[0].providerMetadata = {
          google: { thoughtSignature: "signature-secret" },
        };
      },
    ],
    [
      "Anthropic prefix-bound reasoning",
      (event: any) => {
        event.messages[1].content[0].encrypted = "signature-secret";
      },
    ],
    [
      "Bedrock reasoning",
      (event: any) => {
        event.messages[1].content[0].providerMetadata = {
          bedrock: { signature: "signature-secret" },
        };
      },
    ],
    [
      "native message state",
      (event: any) => {
        event.messages[0].native = { provider: { history: "opaque-secret" } };
      },
    ],
    [
      "encrypted compaction",
      (event: any) => {
        event.messages[0].content.push({
          type: "compaction",
          encrypted: "opaque-secret",
          provider: "fixture",
        });
      },
    ],
    [
      "plaintext checkpoint",
      (event: any) => {
        event.messages[0].content.push({
          type: "compaction",
          text: "checkpoint-secret",
          provider: "fixture",
        });
      },
    ],
    [
      "continuation",
      (event: any) => {
        event.options.previousResponseId = "opaque-secret";
      },
    ],
  ])("refuses rewrites with %s but preserves unchanged state", (_name, setup) => {
    const event = request();
    setup(event);
    const projection = projectNativeRequest(event);
    const before = structuredClone(event);
    expect(
      applyNativeTransform(
        event,
        projection,
        structuredClone(projection.messages),
      ),
    ).toEqual({ system: event.system, messages: event.messages });
    expect(() =>
      applyNativeTransform(
        event,
        projection,
        transformText(projection.messages),
      ),
    ).toThrow(NativeGuardTransformError);
    expect(event).toEqual(before);
  });

  it.each([
    [
      "role",
      (messages: any[]) => {
        messages[1].role = "system";
      },
    ],
    [
      "drop message",
      (messages: any[]) => {
        messages.pop();
      },
    ],
    [
      "add message",
      (messages: any[]) => {
        messages.push({
          role: "user",
          parts: [{ type: "text", text: "extra" }],
        });
      },
    ],
    [
      "drop part",
      (messages: any[]) => {
        messages[2].parts.pop();
      },
    ],
    [
      "add metadata",
      (messages: any[]) => {
        messages[0].parts[0].metadata = { providerType: "x" };
      },
    ],
    [
      "change kind",
      (messages: any[]) => {
        messages[0].parts[0] = { type: "thinking", thinking: "changed" };
      },
    ],
    [
      "non-string",
      (messages: any[]) => {
        messages[2].parts[1].text = 42;
      },
    ],
  ])("rejects %s atomically after an earlier valid replacement", (_name, change) => {
    const event = request();
    const before = structuredClone(event);
    const projection = projectNativeRequest(event);
    const transformed = transformText(projection.messages);
    change(transformed);
    expect(() => applyNativeTransform(event, projection, transformed)).toThrow(
      NativeGuardTransformError,
    );
    expect(event).toEqual(before);
  });

  it("does not rewrite structured tool arguments or JSON results through text slots", () => {
    const event: any = request();
    event.messages.push({
      role: "assistant",
      content: [
        {
          type: "tool-call",
          id: "call",
          name: "read",
          input: { path: "secret" },
        },
      ],
    });
    event.messages.push({
      role: "tool",
      content: [
        {
          type: "tool-result",
          id: "call",
          name: "read",
          result: { type: "json", value: { text: "secret" } },
        },
      ],
    });
    const projection = projectNativeRequest(event);
    expect(() =>
      applyNativeTransform(
        event,
        projection,
        transformText(projection.messages),
      ),
    ).toThrow(NativeGuardTransformError);
  });
});

describe("native awaited guards through the real SDK decoder", () => {
  let wire: Awaited<ReturnType<typeof receiver>>;
  let client: NonNullable<ReturnType<typeof createAgento11yClient>>;
  let config: Agento11yOpencodeConfig;
  let cleanup: (() => Promise<void>) | undefined;
  let warning: ReturnType<typeof vi.spyOn>;
  beforeEach(async () => {
    wire = await receiver();
    config = {
      endpoint: wire.endpoint,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "fixture",
      contentCapture: "metadata_only",
      redactInputMessages: true,
      debug: false,
      guards: { enabled: true, failOpen: true, timeoutMs: 1000 },
    };
    const created = createAgento11yClient(config);
    if (!created) throw new Error("fixture client unavailable");
    client = created;
    warning = vi.spyOn(console, "warn").mockImplementation(() => {});
  });
  afterEach(async () => {
    await cleanup?.();
    cleanup = undefined;
    await client.shutdown();
    await close(wire.server);
    expect(
      wire.calls.every((call) => call.path === "/api/v1/hooks:evaluate"),
    ).toBe(true);
    warning.mockRestore();
  });
  async function setup(failAt = 0) {
    const native = host(failAt);
    const guards = createNativeGuards({
      client,
      config,
      context: native.context,
    });
    cleanup = await guards.register();
    return { ...native, guards };
  }

  it.each([
    undefined,
    false,
  ])("registers no hooks or evaluations when enabled is %s", async (enabled) => {
    if (enabled === undefined) config.guards = undefined;
    else config.guards = { ...config.guards!, enabled };
    const native = await setup();
    expect(native.callbacks.size).toBe(0);
    for (const key of allHooks) await native.send(key, eventFor(key), () => {});
    expect(wire.calls).toHaveLength(0);
  });

  it.each(allHooks)("denies %s before its consumer", async (key) => {
    wire.response(() => ({ action: "deny", reason: "fixture policy" }));
    const native = await setup();
    let providerOrTool = 0;
    const event = eventFor(key);
    const pending = native.send(key, event, () => providerOrTool++);
    if (key === "permission.evaluate") {
      await pending;
      expect(event).toMatchObject({
        effect: "deny",
        message: expect.stringContaining("fixture policy"),
      });
    } else await expect(pending).rejects.toThrow("fixture policy");
    expect(providerOrTool).toBe(0);
    expect(wire.calls).toHaveLength(1);
    expect(wire.calls[0].body.context.conversation_id).toBe("ses_guard");
  });

  it.each(
    providerHooks,
  )("transforms %s independently, without exporting auxiliary generations", async (name) => {
    wire.response((body) => ({
      action: "allow",
      transformed_input: { messages: transformText(body.input.messages) },
    }));
    const native = await setup();
    const event = request();
    const original = structuredClone(event);
    let provider = 0;
    await native.send(`session.${name}`, event, () => {
      provider++;
      expect(event.system[0].text).toBe("system-clean");
      expect(event.messages[0].content[0]).toEqual({
        type: "text",
        text: "user-clean",
      });
    });
    expect(provider).toBe(1);
    expect(event.options).toEqual(original.options);
    expect(wire.calls).toHaveLength(1);
    expect(wire.calls[0].body).toMatchObject({
      phase: "preflight",
      context: { model: { provider: "fixture", name: "model" } },
    });
  });

  it("admits but does not transform the stored prompt", async () => {
    wire.response((body) => ({
      action: "allow",
      transformed_input: { messages: transformText(body.input.messages) },
    }));
    const native = await setup();
    const event = prompt();
    const original = structuredClone(event);
    let provider = 0;
    await native.send("session.prompt", event, () => provider++);
    expect(event).toEqual(original);
    expect(provider).toBe(1);
  });

  const stored = { agent: "review", model: { providerID: "stored", id: "db" } };
  it.each([
    {
      source: "context hook",
      context: true,
      forget: false,
      session: stored,
      version: "fixture",
      agent: "plan",
      model: { provider: "fixture", name: "model" },
    },
    {
      source: "session lookup",
      context: false,
      forget: false,
      session: stored,
      version: undefined,
      agent: "review",
      model: { provider: "stored", name: "db" },
    },
    {
      source: "lookup after session deletion",
      context: true,
      forget: true,
      session: stored,
      version: undefined,
      agent: "review",
      model: { provider: "stored", name: "db" },
    },
    {
      source: "failed lookup",
      context: false,
      forget: false,
      session: new Error("Authorization: fixture-token"),
      version: undefined,
      agent: undefined,
      model: { provider: "unknown", name: "unknown" },
    },
  ])("sends v1 agent and model identity from the $source", async ({
    context,
    forget,
    session,
    version,
    agent,
    model,
  }) => {
    config.agentVersion = version;
    const native = await setup();
    native.sessionGet.mockImplementation(async () => {
      if (session instanceof Error) throw session;
      return session;
    });
    const aux = { providerID: "aux", id: "aux-model" };
    if (context)
      await native.send(
        "session.context",
        { ...request(), agent: "plan" },
        () => {},
      );
    if (forget) native.guards.forget("ses_guard");
    // Auxiliary requests name their own agent or model, not the session's.
    await native.send(
      "session.generate",
      { ...request(), agent: "aux", model: aux },
      () => {},
    );
    await native.send("session.title", { ...request(), model: aux }, () => {});
    await native.send("session.prompt", prompt(), () => {});
    await native.send("tool.execute.before", tool(), () => {});
    const { agent: _unattributed, ...evaluation } = permission();
    await native.send("permission.evaluate", evaluation, () => {});
    const name = agent ? `opencode:${agent}` : "opencode";
    const auxModel = { provider: "aux", name: "aux-model" };
    expect(wire.calls.map((call) => call.body.context)).toEqual(
      [
        ...(context
          ? [
              {
                agent_name: "opencode:plan",
                model: { provider: "fixture", name: "model" },
              },
            ]
          : []),
        { agent_name: "opencode:aux", model: auxModel },
        { agent_name: name, model: auxModel },
        { agent_name: name, model },
        { agent_name: "opencode:build", model },
        { agent_name: name, model },
      ].map((expected) =>
        expect.objectContaining({
          ...expected,
          agent_version: version ?? "2.0.29",
        }),
      ),
    );
    expect(native.sessionGet).toHaveBeenCalledTimes(context && !forget ? 0 : 4);
    expect(JSON.stringify(warning.mock.calls)).not.toContain("fixture-token");
  });

  it("bounds a hung session lookup and evaluates without an identity", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const evaluate = vi
        .spyOn(client, "evaluateHook")
        .mockResolvedValue({ action: "allow" } as never);
      const native = await setup();
      native.sessionGet.mockReturnValue(new Promise(() => {}));
      let consumers = 0;
      const pending = native.send(
        "session.prompt",
        prompt(),
        () => consumers++,
      );
      await vi.advanceTimersByTimeAsync(999);
      expect(evaluate).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await pending;
      expect(consumers).toBe(1);
      expect(evaluate.mock.calls[0]?.[0].context).toMatchObject({
        agentName: "opencode",
        model: { provider: "unknown", name: "unknown" },
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it.each([
    "sdk",
    "proto-json",
    "go-json",
    "content",
  ])("applies supported %s text encodings from the real decoder", async (encoding) => {
    const event = request();
    wire.response(() => ({
      action: "allow",
      transformed_input: {
        messages: transformText(projectNativeRequest(event).messages).map(
          (message: any) => {
            if (
              encoding === "content" &&
              message.parts.length === 1 &&
              message.parts[0].type === "text"
            )
              return { role: message.role, content: message.parts[0].text };
            return {
              role: message.role,
              parts: message.parts.map((part: any) => {
                if (encoding === "go-json")
                  return {
                    kind: part.type,
                    Payload:
                      part.type === "text"
                        ? { Text: part.text }
                        : { Thinking: part.thinking },
                  };
                if (encoding === "proto-json")
                  return { kind: part.type, ...part };
                return part;
              }),
            };
          },
        ),
      },
    }));
    const native = await setup();
    let providers = 0;
    await native.send("session.context", event, () => providers++);
    expect(providers).toBe(1);
    expect(event.system[0].text).toBe("system-clean");
    expect(event.messages[0].content[0]).toEqual({
      type: "text",
      text: "user-clean",
    });
  });

  it("preserves signed and opaque state on an unchanged decoded response", async () => {
    const event: any = request();
    event.messages[0].native = { fixture: { state: "opaque-secret" } };
    event.messages[1].content[0].providerMetadata = {
      fixture: { signature: "signature-secret" },
    };
    wire.response((body) => ({
      action: "allow",
      transformed_input: { messages: body.input.messages },
    }));
    const native = await setup();
    const system = event.system;
    const messages = event.messages;
    let providers = 0;
    await native.send("session.context", event, () => providers++);
    expect(providers).toBe(1);
    expect(event.system).toBe(system);
    expect(event.messages).toBe(messages);
    expect(JSON.stringify(wire.calls)).not.toMatch(
      /opaque-secret|signature-secret/,
    );
  });

  it.each([
    "execute.before",
    "evaluate",
  ])("refuses unserializable %s inputs even with fail-open", async (name) => {
    const native = await setup();
    const event: any = name === "evaluate" ? permission() : tool();
    const cycle: any = {};
    cycle.self = cycle;
    if (name === "evaluate") event.metadata = cycle;
    else event.input = cycle;
    let tools = 0;
    const pending = native.send(
      name === "evaluate" ? "permission.evaluate" : "tool.execute.before",
      event,
      () => tools++,
    );
    if (name === "evaluate") {
      await pending;
      expect(event.effect).toBe("deny");
    } else await expect(pending).rejects.toThrow(/transform/);
    expect(tools).toBe(0);
    expect(wire.calls).toHaveLength(0);
  });

  it("replaces validated tool arguments instead of mutating the original input", async () => {
    wire.response(() => ({
      action: "allow",
      transformed_input: toolTransform(),
    }));
    const native = await setup();
    const event = tool();
    const originalInput = event.input;
    let tools = 0;
    await native.send("tool.execute.before", event, () => {
      tools++;
      expect(event.input).toEqual({ command: "clean" });
    });
    expect(tools).toBe(1);
    expect(originalInput).toEqual({ command: "tool-secret" });
  });

  it("denies permission rewrites the hook cannot apply, without changing host resources", async () => {
    wire.response(() => ({
      action: "allow",
      transformed_input: toolTransform(),
    }));
    const native = await setup();
    const event = permission();
    const resources = event.resources;
    let tools = 0;
    await native.send("permission.evaluate", event, () => tools++);
    expect(tools).toBe(0);
    expect(event.effect).toBe("deny");
    expect(event.message).toContain("could not safely apply");
    expect(event.resources).toBe(resources);
    expect(event.metadata).toEqual({ command: "tool-secret" });
  });

  it.each(
    allHooks.flatMap((key) =>
      [true, false].map((failOpen) => ({ key, failOpen })),
    ),
  )("only evaluator outages honor failOpen=$failOpen for $key", async ({
    key,
    failOpen,
  }) => {
    config.guards!.failOpen = failOpen;
    wire.response(
      () => ({ detail: "Authorization fixture-token source-secret" }),
      503,
    );
    const native = await setup();
    const event = eventFor(key);
    let consumers = 0;
    const pending = native.send(key, event, () => consumers++);
    if (!failOpen && key !== "permission.evaluate")
      await expect(pending).rejects.toThrow("could not evaluate");
    else await pending;
    expect(consumers).toBe(failOpen ? 1 : 0);
    expect(JSON.stringify(warning.mock.calls)).not.toMatch(
      /fixture-token|source-secret|Authorization/,
    );
  });

  it.each([
    null,
    {},
    { messages: [] },
    { messages: [{ role: "user", parts: [{ text: 12 }] }] },
    {
      messages: [
        { role: "user", parts: [{ kind: "future", data: "source-secret" }] },
      ],
    },
    {
      output: [
        {
          role: "assistant",
          parts: [{ tool_call: { name: "bash", input_json: 42 } }],
        },
      ],
    },
    { tools: "source-secret" },
    { system_prompt: 42 },
  ])("strictly refuses malformed transforms from the real decoder: %j", async (transformed_input) => {
    wire.response(() => ({ action: "allow", transformed_input }));
    await expect(
      client.evaluateHook(
        {
          phase: "preflight",
          context: {
            agentName: "fixture",
            model: { provider: "fixture", name: "model" },
          },
          input: { messages: nativePromptMessages(prompt()) },
        } as HookEvaluateRequest,
        {
          enabled: true,
          phases: ["preflight"],
          strictTransforms: true,
          failOpen: true,
        },
      ),
    ).rejects.toBeInstanceOf(HookTransformError);
    const native = await setup();
    let consumers = 0;
    for (const key of allHooks) {
      const event = eventFor(key);
      const pending = native.send(key, event, () => consumers++);
      if (key === "permission.evaluate") {
        await pending;
        expect(event.effect).toBe("deny");
      } else await expect(pending).rejects.toThrow(/transform/);
    }
    expect(consumers).toBe(0);
    expect(JSON.stringify(warning.mock.calls)).not.toMatch(
      /source-secret|fixture-token|Authorization/,
    );
  });

  it.each([
    [
      "role",
      (messages: any[]) => {
        messages[1].role = "system";
      },
    ],
    [
      "message count",
      (messages: any[]) => {
        messages.pop();
      },
    ],
    [
      "part kind",
      (messages: any[]) => {
        messages[2].parts[0] = { type: "text", text: "unsafe" };
      },
    ],
  ])("refuses decoded but unapplyable %s changes before the provider", async (_name, change) => {
    wire.response((body) => {
      const messages = transformText(body.input.messages);
      change(messages);
      return { action: "allow", transformed_input: { messages } };
    });
    const native = await setup();
    const event = request();
    const before = structuredClone(event);
    let providers = 0;
    await expect(
      native.send("session.context", event, () => providers++),
    ).rejects.toThrow(NativeGuardTransformError);
    expect(providers).toBe(0);
    expect(event).toEqual(before);
  });

  it.each(
    [
      toolTransform([]),
      toolTransform(null),
      toolTransform({ command: "clean" }, { id: "wrong-call" }),
      { system_prompt: "clean" },
      { messages: [{ role: "user", content: "clean" }] },
      toolTransform(
        { command: "clean" },
        {
          input_json: Buffer.from(
            JSON.stringify({ command: "tool-secret" }),
          ).toString("base64"),
        },
      ),
      {
        output: [
          {
            role: "assistant",
            parts: [
              {
                ...toolTransform().output[0].parts[0],
                tool_call: {
                  id: "call_guard",
                  name: "bash",
                  input_json: Buffer.from(
                    JSON.stringify({ command: "tool-secret" }),
                  ).toString("base64"),
                },
              },
            ],
          },
        ],
      },
    ].flatMap((transformed_input) =>
      [false, true].map((failOpen) => ({ transformed_input, failOpen })),
    ),
  )("refuses unapplyable tool transforms with failOpen=$failOpen: $transformed_input", async ({
    transformed_input,
    failOpen,
  }) => {
    config.guards!.failOpen = failOpen;
    wire.response(() => ({ action: "allow", transformed_input }));
    const native = await setup();
    const event = tool();
    const before = structuredClone(event);
    let tools = 0;
    await expect(
      native.send("tool.execute.before", event, () => tools++),
    ).rejects.toThrow(/transform/);
    expect(tools).toBe(0);
    expect(event).toEqual(before);
    expect(JSON.stringify(warning.mock.calls)).not.toContain("tool-secret");
  });

  it("refuses unsafe signed rewrites without exposing source or signature", async () => {
    wire.response((body) => ({
      action: "allow",
      transformed_input: { messages: transformText(body.input.messages) },
    }));
    const native = await setup();
    const event: any = request();
    event.messages[0].content[0].providerMetadata = {
      google: { thoughtSignature: "signature-secret" },
    };
    const before = structuredClone(event);
    let providers = 0;
    const failure = await native
      .send("session.context", event, () => providers++)
      .catch((error: Error) => error);
    expect(failure).toBeInstanceOf(NativeGuardTransformError);
    expect(String(failure)).not.toMatch(
      /source-secret|signature-secret|system-secret|user-secret|Authorization/,
    );
    expect(event).toEqual(before);
    expect(providers).toBe(0);
  });

  it.each([
    false,
    true,
  ])("awaits enforcement independently of capture and joins active calls during cleanup=%s", async (stop) => {
    let release!: () => void;
    const evaluated = new Promise<void>((resolve) => {
      release = resolve;
    });
    const evaluateHook = client.evaluateHook.bind(client);
    vi.spyOn(client, "evaluateHook").mockImplementation(async (...args) => {
      await evaluated;
      return evaluateHook(...args);
    });
    wire.response(() => ({ action: "deny" }));
    const native = await setup();
    let provider = 0;
    const pending = native.send("session.context", request(), () => provider++);
    await Promise.resolve();
    expect(provider).toBe(0);
    expect(wire.calls).toHaveLength(0);
    let disposed = false;
    const disposing = stop
      ? cleanup?.().then(() => {
          disposed = true;
        })
      : undefined;
    await Promise.resolve();
    expect(disposed).toBe(false);
    release();
    await expect(pending).rejects.toThrow("blocked");
    expect(provider).toBe(0);
    await disposing;
    expect(disposed).toBe(stop);
  });

  it.each([
    2, 5, 7,
  ])("rolls back every acquired registration when registration %i fails", async (failAt) => {
    const native = host(failAt);
    await expect(
      createNativeGuards({
        client,
        config,
        context: native.context,
      }).register(),
    ).rejects.toThrow("native guard registration failed");
    expect(native.callbacks.size).toBe(0);
    expect(native.disposed).toHaveLength(failAt - 1);
    expect(wire.calls).toHaveLength(0);
  });

  it("attempts every disposal when one registration cleanup rejects", async () => {
    const native = host();
    const hook = native.context.session.hook;
    let disposed = 0;
    vi.spyOn(native.context.session, "hook").mockImplementation(
      async (...args: Parameters<typeof hook>) => {
        const registration = await hook(...args);
        return {
          dispose: async () => {
            disposed++;
            await registration.dispose();
            throw new Error("cleanup-source-secret");
          },
        };
      },
    );
    cleanup = await createNativeGuards({
      client,
      config,
      context: native.context,
    }).register();
    await expect(cleanup()).rejects.toThrow("native guard cleanup failed");
    expect(disposed).toBe(5);
    expect(native.callbacks.size).toBe(0);
    cleanup = undefined;
  });

  it("returns memoized cleanup for all seven registrations", async () => {
    const native = await setup();
    expect([...native.callbacks.keys()]).toEqual(allHooks);
    const first = cleanup!();
    expect(cleanup!()).toBe(first);
    await first;
    expect(native.callbacks.size).toBe(0);
    expect(native.disposed).toHaveLength(7);
  });
});
