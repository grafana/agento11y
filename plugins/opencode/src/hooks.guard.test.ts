import { createServer, type Server } from "node:http";
import type { Part } from "@opencode-ai/sdk";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import {
  _DEFAULT_GUARD_TOAST_DELAY_MS,
  _peekPendingGenerations,
  _peekToolExecutionState,
  _resetHookState,
  _setGuardToastDelayMs,
  type Agento11yHooks,
  createAgento11yHooks,
} from "./hooks.js";
import {
  emitMessageUpdated,
  emitServerInstanceDisposed,
  emitSessionCreated,
  inFlightAssistantMessage,
} from "./hooks.testutil.js";

type HookServer = {
  server: Server;
  baseUrl: string;
  captures: Array<Record<string, any>>;
};

function startHookServer(
  response: Record<string, unknown>,
): Promise<HookServer> {
  const captures: Array<Record<string, any>> = [];
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        captures.push(JSON.parse(body));
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(response));
      });
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error("expected AddressInfo from server.address()");
      }
      resolve({
        server,
        baseUrl: `http://127.0.0.1:${addr.port}`,
        captures,
      });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}

/** Encodes a transformed tool payload the way the server does. */
function base64Json(value: unknown): string {
  return Buffer.from(JSON.stringify(value), "utf8").toString("base64");
}

/**
 * Drives the two hooks the way opencode drives them: it hands
 * `tool.execute.before` its own `{ args }` container, ignores what the hook
 * returns, and reports the same `args` object to `tool.execute.after`
 * (opencode 1.18.10, `SessionTools.resolve` and `SessionPrompt.handleSubtask`).
 *
 * No tool runs here. Nothing writes to `args` after the hook either, so what
 * `args` holds on return is what a real host would have executed.
 */
async function runToolLikeOpencode(
  hooks: Agento11yHooks,
  input: { tool: string; sessionID: string; callID: string },
  args: Record<string, unknown>,
): Promise<void> {
  await hooks.toolExecuteBefore(input, { args });
  hooks.toolExecuteAfter(
    { ...input, args },
    { title: input.tool, output: "ok", metadata: {} },
  );
}

async function seedLinkedSession(
  hooks: Agento11yHooks,
  parentSessionID: string,
  childSessionID: string,
): Promise<void> {
  await emitMessageUpdated(
    hooks,
    inFlightAssistantMessage(parentSessionID, `msg-${parentSessionID}`),
  );
  await emitSessionCreated(hooks, childSessionID, parentSessionID);
}

function config(endpoint: string): Agento11yOpencodeConfig {
  return {
    endpoint,
    auth: { mode: "none" },
    agentName: "opencode",
    agentVersion: "test-version",
    contentCapture: "full",
    redactInputMessages: true,
    debug: false,
    guards: { enabled: true, timeoutMs: 1500, failOpen: true },
  };
}

describe("opencode guards", () => {
  const servers: Server[] = [];

  beforeEach(() => _resetHookState());

  afterEach(async () => {
    // A scheduled toast outlives the case that caused it, and would otherwise
    // arrive in the next one after its arrays were reset.
    await flushToasts();
    await Promise.all(servers.splice(0).map(closeServer));
  });

  it("blocks denied tool.execute.before calls", async () => {
    const hookServer = await startHookServer({
      action: "deny",
      reason: "blocked demo tool",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    hooks.chatMessage(
      {
        sessionID: "sess-1",
        agent: "build",
        model: { providerID: "anthropic", modelID: "claude-sonnet-4" },
      },
      {
        message: {
          id: "msg-user",
          sessionID: "sess-1",
          role: "user",
          system: "",
          tools: {},
        } as any,
        parts: [],
      },
    );

    await expect(
      hooks.toolExecuteBefore(
        {
          sessionID: "sess-1",
          callID: "call-1",
          tool: "third-party-test-mcp_third_party_test_mcp_leak_fake_credential",
        },
        { args: { demo: true } },
      ),
    ).rejects.toThrow("blocked demo tool");

    expect(hookServer.captures).toHaveLength(1);
    expect(hookServer.captures[0]).toMatchObject({
      phase: "postflight",
      context: {
        agent_name: "opencode:build",
        agent_version: "test-version",
        conversation_id: "sess-1",
        model: { provider: "anthropic", name: "claude-sonnet-4" },
      },
      input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "third-party-test-mcp_third_party_test_mcp_leak_fake_credential",
                  input_json: { demo: true },
                },
              },
            ],
          },
        ],
      },
    });

    await emitServerInstanceDisposed(hooks);
  });

  it("rewrites the args opencode runs the tool with, and exports those", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  // The server base64-encodes response payloads, so a stub
                  // sending raw JSON here would exercise a shape it never
                  // emits. conformance/hooks/README.md.
                  input_json: base64Json({ command: "echo [REDACTED]" }),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    // A secret key sits alongside the command; the server drops it and
    // rewrites the command.
    const args: Record<string, unknown> = {
      command: "echo sonia@grafana.com",
      apiKey: "sk-secret",
    };

    // Must not throw (allow + transform, not a block).
    await runToolLikeOpencode(
      hooks,
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      args,
    );

    // Wholesale replacement, on the object the tool read: dropped key gone,
    // command redacted.
    expect(args).toEqual({ command: "echo [REDACTED]" });

    // The span reads the same object the tool ran with, not a copy that can
    // disagree with it.
    const exported = _peekToolExecutionState().completed;
    expect(exported).toHaveLength(1);
    expect(exported[0]?.input).toBe(args);

    await emitServerInstanceDisposed(hooks);
  });

  // Frozen args stand in for any host that hands the hook an object the plugin
  // cannot write to. Which statement throws depends on the server's set:
  // `delete` when the server dropped a key, `Object.assign` when it dropped
  // none. Running the tool anyway would send it the unredacted originals, so
  // the plugin blocks the call instead.
  it.each([
    {
      name: "Object.assign throws",
      args: { command: "echo sonia@grafana.com" },
      wantDetail: /Cannot assign to read only property/,
    },
    {
      name: "delete throws",
      args: { command: "echo sonia@grafana.com", apiKey: "sk-secret" },
      wantDetail: /Cannot delete property/,
    },
  ])("blocks the call and exports no span when $name", async (tc) => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  input_json: base64Json({ command: "echo [REDACTED]" }),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const args = Object.freeze({ ...tc.args }) as Record<string, unknown>;

    const err = await hooks
      .toolExecuteBefore(
        { sessionID: "sess-1", callID: "call-1", tool: "bash" },
        { args },
      )
      .catch((e: unknown) => e as Error);

    expect(err).toBeInstanceOf(Error);
    expect(err?.message).toMatch(
      /could not apply the redacted arguments .* for the "bash" tool call/,
    );
    expect(err?.message).toMatch(tc.wantDetail);
    // Same closing line as a policy deny, so the model stops instead of
    // retrying the call or working around it.
    expect(err?.message).toContain("Stop and tell the user");

    expect(args).toEqual(tc.args);
    // Same as a deny: the blocked call leaves no record to export, so no span
    // reports a call that never ran.
    expect(_peekToolExecutionState().active).toHaveLength(0);
    expect(_peekToolExecutionState().completed).toHaveLength(0);

    await emitServerInstanceDisposed(hooks);
  });

  it("leaves tool.execute.before args unchanged when Agent Observability allows without a transform", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const args: Record<string, unknown> = { command: "ls" };
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      { args },
    );

    expect(args).toEqual({ command: "ls" });

    await emitServerInstanceDisposed(hooks);
  });

  it("strips all args when Agent Observability returns an empty-object transform", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  input_json: base64Json({}),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const args: Record<string, unknown> = { command: "secret", apiKey: "x" };
    await runToolLikeOpencode(
      hooks,
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      args,
    );

    // An intentional "strip all arguments" transform empties the object.
    expect(args).toEqual({});

    await emitServerInstanceDisposed(hooks);
  });

  it("blocks the call when args are not a plain object", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  input_json: base64Json({ 0: "x" }),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    // opencode hands tool args as an object; an array here stands in for any
    // non-plain-object value the redacted set cannot be written into.
    const args: unknown[] = ["ls", "-la"];
    await expect(
      hooks.toolExecuteBefore(
        { sessionID: "sess-1", callID: "call-1", tool: "bash" },
        { args },
      ),
    ).rejects.toThrow("args are not a plain object");

    expect(args).toEqual(["ls", "-la"]);
    expect(_peekToolExecutionState().active).toHaveLength(0);

    await emitServerInstanceDisposed(hooks);
  });

  it("sets permission.ask output to deny when Agent Observability denies", async () => {
    const hookServer = await startHookServer({
      action: "deny",
      reason: "blocked permission",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const output: { status: "ask" | "deny" | "allow" } = { status: "ask" };
    await hooks.permissionAsk(
      {
        id: "perm-1",
        sessionID: "sess-1",
        messageID: "msg-1",
        callID: "call-1",
        type: "bash",
        pattern: "rm *",
        title: "Run shell command",
        metadata: { command: "rm -rf /tmp/demo" },
        time: { created: Date.now() },
      },
      output,
    );

    expect(output.status).toBe("deny");
    expect(hookServer.captures[0]?.context?.conversation_id).toBe("sess-1");
    expect(
      hookServer.captures[0]?.input?.output?.[0]?.parts?.[0],
    ).toMatchObject({
      kind: "tool_call",
      tool_call: {
        id: "call-1",
        name: "bash",
        // Tool arguments travel as embedded JSON, which is what an
        // argument-level rule matches on. conformance/hooks/README.md.
        input_json: {
          pattern: "rm *",
          title: "Run shell command",
          metadata: { command: "rm -rf /tmp/demo" },
        },
      },
    });

    await emitServerInstanceDisposed(hooks);
  });

  it.each([
    {
      name: "linked child",
      physicalSessionID: "sess-child",
      wantConversationID: "sess-root",
      setup: async (hooks: Agento11yHooks) => {
        await seedLinkedSession(hooks, "sess-root", "sess-child");
      },
    },
    {
      name: "nested child",
      physicalSessionID: "sess-grandchild",
      wantConversationID: "sess-root",
      setup: async (hooks: Agento11yHooks) => {
        await seedLinkedSession(hooks, "sess-root", "sess-child");
        await seedLinkedSession(hooks, "sess-child", "sess-grandchild");
      },
    },
    {
      name: "unresolved child",
      physicalSessionID: "sess-unresolved",
      wantConversationID: "sess-unresolved",
      setup: async (hooks: Agento11yHooks) => {
        await emitSessionCreated(hooks, "sess-unresolved", "sess-unseen");
      },
    },
  ])("uses exported conversation identity for a $name tool guard", async ({
    physicalSessionID,
    wantConversationID,
    setup,
  }) => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");
    await setup(hooks);

    const args = { command: "ls" };
    await hooks.toolExecuteBefore(
      {
        sessionID: physicalSessionID,
        callID: "lineage-call",
        tool: "bash",
      },
      { args },
    );

    expect(hookServer.captures[0]?.context?.conversation_id).toBe(
      wantConversationID,
    );
    expect(hookServer.captures[0]?.context).not.toHaveProperty("session_id");
    expect(_peekToolExecutionState().active).toEqual([
      expect.objectContaining({ sessionID: physicalSessionID }),
    ]);

    hooks.toolExecuteAfter(
      {
        sessionID: physicalSessionID,
        callID: "lineage-call",
        tool: "bash",
        args,
      },
      { title: "bash", output: "ok", metadata: {} },
    );
    await emitServerInstanceDisposed(hooks);
  });

  it("uses the root conversation for a nested child's permission guard", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");
    await seedLinkedSession(hooks, "sess-root", "sess-child");
    await seedLinkedSession(hooks, "sess-child", "sess-grandchild");

    const output: { status: "ask" | "deny" | "allow" } = { status: "ask" };
    await hooks.permissionAsk(
      {
        id: "perm-nested",
        sessionID: "sess-grandchild",
        messageID: "msg-nested",
        callID: "call-nested",
        type: "bash",
        pattern: "*",
        title: "Run shell command",
        metadata: {},
        time: { created: Date.now() },
      },
      output,
    );

    expect(hookServer.captures[0]?.context?.conversation_id).toBe("sess-root");
    expect(output.status).toBe("ask");
    await emitServerInstanceDisposed(hooks);
  });
});

type PreflightServer = {
  server: Server;
  baseUrl: string;
  captures: Array<Record<string, any>>;
};

/**
 * Hook server whose response depends on the request, so a test can drive
 * several provider steps with different conversation content. `respond`
 * returning `"error"` stands in for a failing guard backend, and returning
 * undefined for a hanging one (timeout).
 */
function startPreflightServer(
  respond: (
    req: Record<string, any>,
    callIndex: number,
  ) => Record<string, unknown> | "error" | undefined,
): Promise<PreflightServer> {
  const captures: Array<Record<string, any>> = [];
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        const parsed = JSON.parse(body);
        captures.push(parsed);
        const payload = respond(parsed, captures.length - 1);
        if (payload === undefined) return; // never answers
        if (payload === "error") {
          res.statusCode = 503;
          res.end("guard backend unavailable");
          return;
        }
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(payload));
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

function textPart(
  id: string,
  messageID: string,
  text: string,
  sessionID = "sess-1",
): Part {
  return {
    id,
    sessionID,
    messageID,
    type: "text",
    text,
  } as Part;
}

function userEntry(id: string, text: string, sessionID = "sess-1") {
  return {
    info: { id, role: "user", sessionID } as any,
    parts: [textPart(`${id}-p1`, id, text, sessionID)],
  };
}

function assistantEntry(id: string, text: string) {
  return {
    info: { id, role: "assistant", sessionID: "sess-1" } as any,
    parts: [textPart(`${id}-p1`, id, text)],
  };
}

function toolEntry(id: string) {
  return {
    info: { id, role: "assistant", sessionID: "sess-1" } as any,
    parts: [
      {
        id: `${id}-p1`,
        sessionID: "sess-1",
        messageID: id,
        type: "tool",
        callID: "call-1",
        tool: "bash",
        state: {
          status: "completed",
          input: { command: "echo secret" },
          output: "secret output",
          title: "bash",
          metadata: {},
          time: { start: 1, end: 2 },
        },
      } as Part,
    ],
  };
}

/** Response echoing one transformed message per forwarded message. */
function transformResponse(texts: Array<string | null>) {
  return {
    action: "allow",
    evaluations: [],
    transformed_input: {
      messages: texts.map((text) => ({
        role: "user",
        parts: text === null ? [] : [{ kind: "text", text }],
      })),
    },
  };
}

async function hooksFor(
  baseUrl: string,
  overrides?: Partial<Agento11yOpencodeConfig>,
) {
  const hooks = await createAgento11yHooks(
    { ...config(baseUrl), ...overrides },
    stubClient(),
  );
  if (!hooks) throw new Error("expected hooks");
  return hooks;
}

/** Lets a scheduled guard toast run. */
async function flushToasts(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 5));
}

/** Toast bodies the plugin sent, in order. */
let toasts: Array<{ title?: string; message: string; variant: string }> = [];
/** `app.log` bodies the plugin sent, in order. */
let logs: Array<{ service: string; level: string; message: string }> = [];
/** Session ids the plugin asked opencode to abort, in order. */
let aborts: string[] = [];

function stubClient() {
  return {
    session: {
      message: async () => ({ data: { parts: [] } }),
      abort: async (options: { path: { id: string } }) => {
        aborts.push(options.path.id);
        return { data: true };
      },
    },
    app: {
      log: async (options: { body: any }) => {
        logs.push(options.body);
        return { data: true };
      },
    },
    tui: {
      showToast: async (options: { body: any }) => {
        toasts.push(options.body);
        return { data: true };
      },
    },
  } as any;
}

type Entry = { info: any; parts: Part[] };

/** The text of every part, in order. `undefined` for a part with no text. */
function partTexts(messages: Entry[]): Array<Array<string | undefined>> {
  return messages.map((msg) =>
    (msg.parts ?? []).map((part) => (part as { text?: string }).text),
  );
}

describe("opencode preflight guard", () => {
  const servers: Server[] = [];

  beforeEach(() => {
    _resetHookState();
    toasts = [];
    logs = [];
    aborts = [];
    // The delay exists to lose a race with opencode's own toast; a case that
    // asserts the toast should not wait it out. `defers the guard toast` covers
    // the default.
    _setGuardToastDelayMs(0);
  });

  afterEach(async () => {
    // A scheduled toast outlives the case that caused it, and would otherwise
    // arrive in the next one after its arrays were reset.
    await flushToasts();
    await Promise.all(servers.splice(0).map(closeServer));
  });

  type PreflightCase = {
    name: string;
    /** `"error"` stands in for a failing backend, undefined for a hanging one. */
    respond?: (
      req: Record<string, any>,
      callIndex: number,
    ) => Record<string, unknown> | "error" | undefined;
    config?: Partial<Agento11yOpencodeConfig>;
    /** Runs before the transform, to seed session context. */
    setup?: (
      hooks: Awaited<ReturnType<typeof hooksFor>>,
    ) => void | Promise<void>;
    build: () => { messages: Entry[]; pinned?: Part };
    wantCalls: number;
    wantTexts: Array<Array<string | undefined>>;
    /** Expected `input.messages` of the first evaluation. */
    wantForwarded?: unknown;
    /** Substrings that must each appear in a debug log line. */
    wantLog?: string[];
    /** Substring the call must reject with, when it refuses the turn. */
    wantThrow?: string;
    assert?: (args: {
      messages: Entry[];
      pinned?: Part;
      captures: Array<Record<string, any>>;
    }) => void;
  };

  const cases: PreflightCase[] = [
    {
      name: "evaluates nothing and leaves messages untouched when guards are disabled",
      config: { guards: { enabled: false, timeoutMs: 1500, failOpen: true } },
      respond: () => transformResponse(["[REDACTED]"]),
      build: () => ({
        messages: [
          userEntry("m1", "authorization=secret-123"),
          assistantEntry("m2", "ok"),
        ],
      }),
      wantCalls: 0,
      wantTexts: [["authorization=secret-123"], ["ok"]],
    },
    {
      name: "rewrites provider-bound text and carries the execution context",
      respond: () => transformResponse(["authorization=[REDACTED]", "ok"]),
      // The hook input has neither agent nor model; both come from the session.
      setup: (hooks) =>
        hooks.chatMessage(
          {
            sessionID: "sess-1",
            agent: "build",
            model: { providerID: "anthropic", modelID: "claude-sonnet-4" },
          },
          {
            message: {
              id: "m1",
              sessionID: "sess-1",
              role: "user",
              system: "",
              tools: {},
            } as any,
            parts: [],
          },
        ),
      build: () => ({
        messages: [
          userEntry("m1", "authorization=secret-123"),
          assistantEntry("m2", "ok"),
        ],
      }),
      wantCalls: 1,
      wantTexts: [["authorization=[REDACTED]"], ["ok"]],
      wantForwarded: [
        {
          role: "user",
          parts: [{ kind: "text", text: "authorization=secret-123" }],
        },
        { role: "assistant", parts: [{ kind: "text", text: "ok" }] },
      ],
      assert: ({ captures }) => {
        expect(captures[0]).toMatchObject({
          phase: "preflight",
          context: {
            agent_name: "opencode:build",
            agent_version: "test-version",
            conversation_id: "sess-1",
            model: { provider: "anthropic", name: "claude-sonnet-4" },
          },
        });
      },
    },
    {
      name: "forwards a placeholder for a slot with no redactable text",
      respond: () => transformResponse(["a", null, "c"]),
      build: () => ({
        messages: [
          userEntry("m1", "one"),
          toolEntry("m2"),
          userEntry("m3", "three"),
        ],
      }),
      wantCalls: 1,
      wantTexts: [["a"], [undefined], ["c"]],
      wantForwarded: [
        { role: "user", parts: [{ kind: "text", text: "one" }] },
        { role: "assistant", parts: [] },
        { role: "user", parts: [{ kind: "text", text: "three" }] },
      ],
      assert: ({ messages }) => {
        expect((messages[1].parts[0] as any).state.output).toBe(
          "secret output",
        );
      },
    },
    {
      name: "omits conversation identity when outgoing messages have none",
      build: () => {
        const message = userEntry("m1", "hello");
        delete message.info.sessionID;
        return { messages: [message] };
      },
      wantCalls: 1,
      wantTexts: [["hello"]],
      assert: ({ captures }) => {
        expect(captures[0]?.context).not.toHaveProperty("conversation_id");
        expect(captures[0]?.context).not.toHaveProperty("session_id");
      },
    },
    {
      name: "skips the evaluation when no slot carries redactable text",
      respond: () => transformResponse(["[R]"]),
      build: () => ({
        messages: [
          { info: { id: "m1", role: "user", sessionID: "sess-1" }, parts: [] },
        ],
      }),
      wantCalls: 0,
      wantTexts: [[]],
    },
    {
      name: "leaves messages untouched when the response carries no transform",
      respond: () => ({ action: "allow", evaluations: [] }),
      build: () => ({
        messages: [userEntry("m1", "hello"), assistantEntry("m2", "ok")],
      }),
      wantCalls: 1,
      wantTexts: [["hello"], ["ok"]],
    },
    {
      name: "discards a transform with too few messages",
      respond: () => transformResponse(["[R1]", "[R2]"]),
      build: () => ({
        messages: [
          userEntry("m1", "one"),
          assistantEntry("m2", "two"),
          userEntry("m3", "three"),
        ],
      }),
      wantCalls: 1,
      wantTexts: [["one"], ["two"], ["three"]],
    },
    {
      name: "discards a transform with extra messages",
      respond: () => transformResponse(["[R1]", "[R2]", "[R3]"]),
      build: () => ({
        messages: [userEntry("m1", "one"), assistantEntry("m2", "two")],
      }),
      wantCalls: 1,
      wantTexts: [["one"], ["two"]],
    },
    {
      // The SDK turns a failed evaluation into a synthetic allow unless it is
      // asked to throw, which would send an unredacted conversation to the
      // provider with nothing in the log.
      name: "fails open when the evaluation errors, and logs it",
      respond: () => "error",
      config: { debug: true },
      build: () => ({ messages: [userEntry("m1", "hello")] }),
      wantCalls: 1,
      wantTexts: [["hello"]],
      wantLog: ["preflight transform eval failed"],
    },
    {
      // A timeout at the default 1500ms is routine, so it has to be
      // recoverable from the debug log too.
      name: "fails open when the evaluation times out, and logs it",
      respond: () => undefined,
      config: {
        debug: true,
        guards: { enabled: true, timeoutMs: 10, failOpen: true },
      },
      build: () => ({ messages: [userEntry("m1", "hello")] }),
      wantCalls: 1,
      wantTexts: [["hello"]],
      wantLog: ["preflight transform eval failed"],
    },
    {
      // Fail-closed reaches this hook too. The refusal itself is asserted
      // separately, because this table drives the redaction write-back and a
      // rejected call has nothing to write.
      name: "leaves the conversation alone when a failed evaluation refuses the turn",
      respond: () => "error",
      config: { guards: { enabled: true, timeoutMs: 1500, failOpen: false } },
      build: () => ({ messages: [userEntry("m1", "hello")] }),
      wantCalls: 1,
      wantTexts: [["hello"]],
      wantThrow: "stopped as a safety measure",
    },
    {
      // The server echoes the whole conversation back with only matched
      // substrings replaced, so a count of the conversation would read the same
      // whether one message or none was redacted.
      name: "logs how many messages were rewritten, not how many were evaluated",
      respond: () => transformResponse(["token=[R]", "unchanged"]),
      config: { debug: true },
      build: () => ({
        messages: [
          userEntry("m1", "token=alpha"),
          assistantEntry("m2", "unchanged"),
        ],
      }),
      wantCalls: 1,
      wantTexts: [["token=[R]"], ["unchanged"]],
      wantLog: ["rewrote 1 of 2 messages"],
    },
    {
      // Both dispatch sites pass `{}` as input, so the handler cannot tell the
      // compaction call from a provider step and evaluates both. Compaction
      // hands over a structuredClone of prior history.
      name: "evaluates the compaction dispatch as well",
      respond: () => transformResponse(["summarize this: [R]"]),
      build: () => ({
        messages: structuredClone([
          userEntry("m1", "summarize this: token=alpha"),
        ]),
      }),
      wantCalls: 1,
      wantTexts: [["summarize this: [R]"]],
    },
    {
      name: "rewrites a frozen message entry without mutating it",
      respond: () => transformResponse(["token=[R]"]),
      build: () => {
        const pinned = Object.freeze(textPart("m1-p1", "m1", "token=alpha"));
        return {
          pinned,
          messages: [
            Object.freeze({
              info: { id: "m1", role: "user", sessionID: "sess-1" } as any,
              parts: Object.freeze([pinned]) as unknown as Part[],
            }),
          ],
        };
      },
      wantCalls: 1,
      wantTexts: [["token=[R]"]],
      assert: ({ pinned }) => {
        expect((pinned as any).text).toBe("token=alpha");
      },
    },
  ];

  it.each(cases)("$name", async ({
    respond,
    config: overrides,
    setup,
    build,
    wantCalls,
    wantTexts,
    wantForwarded,
    wantLog,
    wantThrow,
    assert,
  }) => {
    const hookServer = await startPreflightServer(
      respond ?? (() => ({ action: "allow", evaluations: [] })),
    );
    servers.push(hookServer.server);
    const stderr = vi.spyOn(console, "error").mockImplementation(() => {});

    const hooks = await hooksFor(hookServer.baseUrl, overrides);
    await setup?.(hooks);
    const { messages, pinned } = build();
    const ids = messages.map((msg) => msg.info.id);

    const call = hooks.messagesTransform({ messages });
    if (wantThrow !== undefined) {
      await expect(call).rejects.toThrow(wantThrow);
    } else {
      await expect(call).resolves.toBeUndefined();
    }

    const lines = stderr.mock.calls.map((call) => call.join(" "));
    stderr.mockRestore();
    expect(hookServer.captures).toHaveLength(wantCalls);
    expect(partTexts(messages)).toEqual(wantTexts);
    // Message count and order never change, whatever the server returned.
    expect(messages.map((msg) => msg.info.id)).toEqual(ids);
    if (wantForwarded !== undefined) {
      expect(hookServer.captures[0]?.input?.messages).toEqual(wantForwarded);
    }
    for (const substring of wantLog ?? []) {
      expect(lines).toContainEqual(expect.stringContaining(substring));
    }
    assert?.({ messages, pinned, captures: hookServer.captures });

    await emitServerInstanceDisposed(hooks);
  });

  it("evaluates every provider step against that step's own content", async () => {
    // opencode dispatches this hook once per step with a fresh, unredacted copy
    // of the conversation, so a reused message id whose text changed must never
    // receive the earlier step's transform.
    const hookServer = await startPreflightServer((req) => {
      const first = req.input?.messages?.[0]?.parts?.[0]?.text as string;
      return transformResponse([first.replace(/alpha|beta/, "[R]")]);
    });
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);

    const step1 = [userEntry("m1", "token=alpha")];
    await hooks.messagesTransform({ messages: step1 });
    const step2 = [userEntry("m1", "token=beta")];
    await hooks.messagesTransform({ messages: step2 });

    // One evaluation per step, each seeing the current text.
    expect(hookServer.captures).toHaveLength(2);
    expect(hookServer.captures[0]?.input?.messages?.[0]?.parts?.[0]?.text).toBe(
      "token=alpha",
    );
    expect(hookServer.captures[1]?.input?.messages?.[0]?.parts?.[0]?.text).toBe(
      "token=beta",
    );
    expect((step1[0].parts[0] as any).text).toBe("token=[R]");
    expect((step2[0].parts[0] as any).text).toBe("token=[R]");

    await emitServerInstanceDisposed(hooks);
  });
});

/** A `chat.message` payload carrying one text part, or none when text is null. */
function chatMessagePayload(text: string | null, sessionID = "sess-1") {
  return {
    input: {
      sessionID,
      agent: "build",
      model: { providerID: "anthropic", modelID: "claude-sonnet-4" },
    },
    output: {
      message: {
        id: "m1",
        sessionID,
        role: "user",
        system: "",
        tools: {},
      } as any,
      parts: text === null ? [] : [textPart("m1-p1", "m1", text, sessionID)],
    },
  };
}

describe("opencode prompt guard", () => {
  const servers: Server[] = [];

  beforeEach(() => {
    _resetHookState();
    toasts = [];
    logs = [];
    aborts = [];
    // The delay exists to lose a race with opencode's own toast; a case that
    // asserts the toast should not wait it out. `defers the guard toast` covers
    // the default.
    _setGuardToastDelayMs(0);
  });

  afterEach(async () => {
    // A scheduled toast outlives the case that caused it, and would otherwise
    // arrive in the next one after its arrays were reset.
    await flushToasts();
    await Promise.all(servers.splice(0).map(closeServer));
  });

  type PromptCase = {
    name: string;
    respond?: (
      req: Record<string, any>,
    ) => Record<string, unknown> | "error" | undefined;
    config?: Partial<Agento11yOpencodeConfig>;
    /** Text of the submitted message, or null for a message with no parts. */
    text?: string | null;
    wantCalls: number;
    /** Substring the thrown error and the toast must both carry. */
    wantBlock?: string;
    assert?: (args: { captures: Array<Record<string, any>> }) => void;
  };

  const denyResponse = {
    action: "deny",
    reason: "secrets are not allowed in prompts",
    evaluations: [],
  };

  const cases: PromptCase[] = [
    {
      name: "refuses the turn when the guard denies the prompt",
      respond: () => denyResponse,
      wantBlock: "secrets are not allowed in prompts",
      wantCalls: 1,
      assert: ({ captures }) => {
        expect(captures[0]).toMatchObject({
          phase: "preflight",
          context: {
            agent_name: "opencode:build",
            agent_version: "test-version",
            conversation_id: "sess-1",
            model: { provider: "anthropic", name: "claude-sonnet-4" },
          },
          input: {
            messages: [
              { role: "user", parts: [{ kind: "text", text: "key=abc" }] },
            ],
          },
        });
      },
    },
    {
      name: "lets the turn through when the guard allows",
      respond: () => ({ action: "allow", evaluations: [] }),
      wantCalls: 1,
    },
    {
      // The parts here are about to be persisted as the user's own message, so
      // a transform must not rewrite them. Redaction belongs to the messages
      // transform, which rewrites only the provider-bound copy.
      name: "ignores a transform the allow response carries",
      respond: () => ({
        action: "allow",
        evaluations: [],
        transformed_input: {
          messages: [
            { role: "user", parts: [{ kind: "text", text: "[GONE]" }] },
          ],
        },
      }),
      wantCalls: 1,
    },
    {
      name: "evaluates nothing when guards are disabled",
      config: { guards: { enabled: false, timeoutMs: 1500, failOpen: true } },
      respond: () => denyResponse,
      wantCalls: 0,
    },
    {
      name: "evaluates nothing when the message carries no text",
      respond: () => denyResponse,
      text: null,
      wantCalls: 0,
    },
    {
      name: "lets the turn through when the evaluation fails and fail-open is on",
      respond: () => "error",
      wantCalls: 1,
    },
    {
      // Unlike the messages transform, this hook can refuse, so the fail-open
      // flag reaches preflight here.
      name: "refuses the turn when the evaluation fails and fail-open is off",
      config: { guards: { enabled: true, timeoutMs: 1500, failOpen: false } },
      respond: () => "error",
      wantBlock: "blocked this message",
      wantCalls: 1,
    },
  ];

  it.each(cases)("$name", async ({
    respond,
    config: overrides,
    text,
    wantCalls,
    wantBlock,
    assert,
  }) => {
    const hookServer = await startPreflightServer(
      respond ?? (() => ({ action: "allow", evaluations: [] })),
    );
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl, overrides);
    const { input, output } = chatMessagePayload(
      text === undefined ? "key=abc" : text,
    );

    const call = hooks.chatMessage(input, output);
    if (wantBlock !== undefined) {
      await expect(call).rejects.toThrow(wantBlock);
      await flushToasts();
      // The durable copy goes through opencode's own logger; the toast is
      // scheduled late so the host's failure toast does not replace it.
      expect(logs).toHaveLength(1);
      expect(logs[0]).toMatchObject({ service: "agento11y", level: "error" });
      expect(logs[0]?.message).toContain(wantBlock);
      expect(toasts).toHaveLength(1);
      expect(toasts[0]).toMatchObject({ variant: "error" });
      expect(toasts[0]?.message).toContain(wantBlock);
    } else {
      await expect(call).resolves.toBeUndefined();
      expect(toasts).toHaveLength(0);
      expect(logs).toHaveLength(0);
    }

    expect(hookServer.captures).toHaveLength(wantCalls);
    // The submitted parts are never rewritten here.
    if (text !== null) {
      expect((output.parts[0] as any).text).toBe("key=abc");
    }
    assert?.({ captures: hookServer.captures });

    await emitServerInstanceDisposed(hooks);
  });

  it("uses the root conversation for a linked child's submitted prompt", async () => {
    const hookServer = await startPreflightServer(() => ({
      action: "allow",
      evaluations: [],
    }));
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);
    await seedLinkedSession(hooks, "sess-root", "sess-child");
    const { input, output } = chatMessagePayload("key=abc", "sess-child");

    await hooks.chatMessage(input, output);

    expect(hookServer.captures[0]?.context?.conversation_id).toBe("sess-root");
    await emitServerInstanceDisposed(hooks);
  });

  it("drops the refused turn's user-side data", async () => {
    // opencode creates some assistant messages without a `chat.message` of
    // their own, so a leftover pending entry would be exported against one of
    // those carrying the parts of the prompt that never ran.
    const hookServer = await startPreflightServer(() => denyResponse);
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);
    const { input, output } = chatMessagePayload("key=abc");
    await expect(hooks.chatMessage(input, output)).rejects.toThrow();

    expect(_peekPendingGenerations()).toEqual([]);

    await emitServerInstanceDisposed(hooks);
  });

  it("still records session context for the export when the prompt is blocked", async () => {
    // The block aborts the turn, but the stores are written before the
    // evaluation so the guard request can name the agent and model.
    const hookServer = await startPreflightServer(() => denyResponse);
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);
    const { input, output } = chatMessagePayload("key=abc");
    await expect(hooks.chatMessage(input, output)).rejects.toThrow();

    expect(hookServer.captures[0]?.context?.agent_name).toBe("opencode:build");

    await emitServerInstanceDisposed(hooks);
  });

  it("refuses a started turn when the outgoing conversation is denied", async () => {
    // This is the deny `chat.message` cannot see, because the text is not in the
    // message the user just submitted.
    const hookServer = await startPreflightServer(() => denyResponse);
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);
    await seedLinkedSession(hooks, "sess-root", "sess-child");
    const messages = [userEntry("m1", "one", "sess-child")];

    await expect(hooks.messagesTransform({ messages })).rejects.toThrow(
      "secrets are not allowed in prompts",
    );

    await flushToasts();
    expect(logs).toHaveLength(1);
    expect(logs[0]?.message).toContain("the turn was stopped");
    expect(toasts).toHaveLength(1);
    expect(hookServer.captures[0]?.context?.conversation_id).toBe("sess-root");
    // Guard context follows export lineage, but cleanup still targets the
    // physical child session whose assistant row opencode already wrote.
    expect(aborts).toEqual(["sess-child"]);
    // A refused turn never reaches the provider, so nothing is rewritten.
    expect(partTexts(messages)).toEqual([["one"]]);

    await emitServerInstanceDisposed(hooks);
  });

  it("refuses a started turn when the evaluation fails and fail-open is off", async () => {
    const hookServer = await startPreflightServer(() => "error");
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl, {
      guards: { enabled: true, timeoutMs: 1500, failOpen: false },
    });

    await expect(
      hooks.messagesTransform({ messages: [userEntry("m1", "one")] }),
    ).rejects.toThrow("stopped as a safety measure");

    await emitServerInstanceDisposed(hooks);
  });
});

describe("guard toast timing", () => {
  const servers: Server[] = [];

  beforeEach(() => {
    _resetHookState();
    toasts = [];
    logs = [];
    aborts = [];
    // Restores the shipped value the other suites turn off, so this one holds
    // the real delay to being long enough.
    _setGuardToastDelayMs(_DEFAULT_GUARD_TOAST_DELAY_MS);
  });

  afterEach(async () => {
    _setGuardToastDelayMs(0);
    await flushToasts();
    await Promise.all(servers.splice(0).map(closeServer));
  });

  it("defers the guard toast so the host's failure toast cannot replace it", async () => {
    // Refusing a turn fails the prompt request, and opencode answers that with
    // its own toast. Its store holds one toast, so ours has to arrive after the
    // throw the host is still reacting to.
    //
    // Deliberately does not set the delay: this is the one case that holds the
    // shipped default to being long enough to lose that race, which is the only
    // reason the toast is worth showing at all.
    const hookServer = await startPreflightServer(() => ({
      action: "deny",
      reason: "policy says no",
      evaluations: [],
    }));
    servers.push(hookServer.server);

    const hooks = await hooksFor(hookServer.baseUrl);
    const { input, output } = chatMessagePayload("key=abc");

    await expect(hooks.chatMessage(input, output)).rejects.toThrow();
    // The log is written before the throw; the toast is not, and is still not
    // there several event-loop turns later.
    expect(logs).toHaveLength(1);
    await flushToasts();
    expect(toasts).toHaveLength(0);

    await new Promise((resolve) => setTimeout(resolve, 1000));
    expect(toasts).toHaveLength(1);
    expect(toasts[0]?.message).toContain("policy says no");

    await emitServerInstanceDisposed(hooks);
  });
});
