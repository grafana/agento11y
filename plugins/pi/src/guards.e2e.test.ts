// Pi guards: real-SDK-over-HTTP integration tests.
//
// Drives the @grafana/agento11y-pi extension through a faked pi host, but unlike
// the handler-wiring unit tests (index.test.ts) the agento11y client here is the
// real JS SDK pointed at a local HTTP server. Each test exercises the full
// preflight (`context_with_system`) or postflight (`tool_call`) end to end:
// real Agento11yClient.evaluateHook -> HTTP hooks:evaluate -> wire parse ->
// copy-on-write native messages / in-place tool input.
//
// The unit suites already prove the mapper/guard branches; this suite adds the
// transport + wire-decode dimension and asserts both what the plugin sent and
// what it applied.

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import registerExtension from "./index.js";
import type {
  PiAgentMessage,
  PiAssistantMessage,
  PiToolResult,
  PiUserMessage,
} from "./mappers.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";
import {
  type Agento11yTestServer,
  closeServer,
  type HookCall,
  type HookResponse,
  startAgento11yTestServer,
} from "./testHttp.js";

// Minimal pi host: registerExtension subscribes handlers; tests invoke a
// handler directly (via the map) so they can capture its return value, which
// pi's emit path would otherwise swallow.
class FakePi {
  handlers = new Map<string, (event: any, ctx: any) => Promise<any> | any>();
  on(event: string, handler: (event: any, ctx: any) => Promise<any> | any) {
    this.handlers.set(event, handler);
  }
  async emit(event: string, payload: any, ctx: any) {
    const handler = this.handlers.get(event);
    if (!handler) return undefined;
    return handler(payload, ctx);
  }
}

const ctx = {
  cwd: "/nonexistent-guards-workspace",
  model: { provider: "anthropic", id: "claude-sonnet-4" },
  sessionManager: {
    getSessionFile: () => "guards-session.jsonl",
    getSessionId: () => "guards-conv-1",
  },
};

function assistantFixture(
  content: PiAssistantMessage["content"] = [{ type: "text", text: "ok" }],
): PiAssistantMessage {
  return {
    role: "assistant",
    content,
    provider: "anthropic",
    model: "claude-sonnet-4",
    usage: {
      input: 0,
      output: 0,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 0,
    },
    stopReason: "stop",
    timestamp: 1,
  };
}

function userMsg(content: PiUserMessage["content"]): PiUserMessage {
  return { role: "user", content, timestamp: 1 };
}

function toolResultMsg(text: string, isError = false): PiToolResult {
  return {
    role: "toolResult",
    toolCallId: "tc-1",
    toolName: "bash",
    content: [{ type: "text", text }],
    isError,
    timestamp: 1,
  };
}

// Wire-shaped (snake_case) hook response bodies, matching what the Agent Observability API
// emits and what js/src/hooks.ts parses.
function preflightTransform(messages: unknown[]): HookResponse {
  return {
    json: {
      action: "allow",
      evaluations: [],
      transformed_input: {
        messages: messages.map((message, index) => ({
          name: `pi:${index}`,
          ...(message as object),
        })),
      },
    },
  };
}
function postflightTransform(output: unknown[]): HookResponse {
  return {
    json: { action: "allow", evaluations: [], transformed_input: { output } },
  };
}
function deny(reason: string): HookResponse {
  return { json: { action: "deny", reason, evaluations: [] } };
}
const ALLOW: HookResponse = { json: { action: "allow", evaluations: [] } };

interface GuardEnv {
  enabled?: boolean;
  failOpen?: boolean;
  timeoutMs?: number;
}

function setGuardEnv(env: GuardEnv): void {
  process.env.SIGIL_GUARDS_ENABLED = String(env.enabled ?? true);
  process.env.SIGIL_GUARDS_FAIL_OPEN = String(env.failOpen ?? true);
  process.env.SIGIL_GUARDS_TIMEOUT_MS = String(env.timeoutMs ?? 1500);
}

describe("pi guards: real-SDK over HTTP", () => {
  let server: Agento11yTestServer;
  let savedEnv: Record<string, string | undefined> = {};
  // Set per-test before setup() runs the session_start that builds the client.
  let nextHook: (call: HookCall) => HookResponse = () => ALLOW;
  let generations: any[];

  beforeEach(async () => {
    server = await startAgento11yTestServer({ hook: (call) => nextHook(call) });
    generations = [];
    server.server.on("request", (req) => {
      if (req.url !== "/api/v1/generations:export") return;
      let raw = "";
      req.on("data", (chunk) => {
        raw += chunk;
      });
      req.on("end", () => {
        generations.push(...JSON.parse(raw).generations);
      });
    });
    nextHook = () => ALLOW;
    savedEnv = snapshotAndClearTestEnv();

    process.env.HOME = "/nonexistent-guards-home";
    process.env.USERPROFILE = process.env.HOME;
    process.env.XDG_CONFIG_HOME = "/nonexistent-guards-home/.config";
    process.env.SIGIL_ENDPOINT = server.baseUrl;
    process.env.SIGIL_AUTH_TENANT_ID = "tenant";
    process.env.SIGIL_AUTH_TOKEN = "token";
    process.env.SIGIL_AGENT_NAME = "pi";
    process.env.SIGIL_AGENT_VERSION = "test-version";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.SIGIL_DEBUG = "";
    setGuardEnv({});
  });

  afterEach(async () => {
    await closeServer(server.server);
    restoreEnv(savedEnv);
    savedEnv = {};
  });

  // Cache a previous assistant model for tool guards. Ordinary preflight uses
  // ctx.model instead (including before any assistant has completed).
  async function setup(): Promise<FakePi> {
    const pi = new FakePi();
    registerExtension(pi as any);
    await pi.emit("session_start", {}, ctx);
    await pi.emit("turn_start", {}, ctx);
    await pi.emit("message_end", { message: assistantFixture() }, ctx);
    return pi;
  }

  async function runContext(
    pi: FakePi,
    messages: PiAgentMessage[],
  ): Promise<{ result: unknown; event: { messages: PiAgentMessage[] } }> {
    const event = { messages };
    const before = structuredClone(messages);
    const result = await pi.handlers.get("context_with_system")!(event, ctx);
    expect(event.messages).toBe(messages);
    expect(messages).toEqual(before);
    if (result) expect(result.messages).not.toBe(messages);
    // Simulate Pi accepting the returned replacement for the provider request.
    return { result, event: { messages: result?.messages ?? messages } };
  }

  async function runToolCall(
    pi: FakePi,
    input: Record<string, unknown>,
  ): Promise<{ result: unknown; event: { input: Record<string, unknown> } }> {
    const event = { toolCallId: "tc-1", toolName: "bash", input };
    const result = await pi.handlers.get("tool_call")!(event, ctx);
    return { result, event };
  }

  describe.each([
    "AGENTO11Y_TAGS",
    "SIGIL_TAGS",
  ])("%s guard/export tag parity", (spelling) => {
    it.each<[string, Record<string, string>]>([
      ["team=prod,team=", { team: "" }],
      ["team=,team=prod", { team: "prod" }],
      ["team=prod,team=dev", { team: "dev" }],
      ["team=", { team: "" }],
      ["user=prod,user=", { user: "" }],
    ])("uses the same effective tags for %s", async (raw, expected) => {
      process.env[spelling] = raw;
      process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
      process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user";
      process.env.AGENTO11Y_USER_ID = "automatic-user";
      const pi = await setup();
      await runContext(pi, [userMsg("hello")]);
      await runToolCall(pi, { command: "true" });
      await pi.emit(
        "turn_end",
        { message: assistantFixture(), toolResults: [] },
        ctx,
      );
      await pi.emit("session_shutdown", {}, ctx);

      expect(generations).toHaveLength(1);
      const tags = {
        user: "automatic-user",
        ...expected,
        cwd: ctx.cwd,
      };
      expect(generations[0].tags).toEqual(tags);
      expect(server.hookCalls).toHaveLength(2);
      for (const call of server.hookCalls)
        expect(call.body.context?.tags).toEqual(generations[0].tags);
      expect(server.errors).toEqual([]);
    });
  });

  describe("preflight (context_with_system)", () => {
    interface PreflightCase {
      name: string;
      env?: GuardEnv;
      hook?: HookResponse;
      messages: () => PiAgentMessage[];
      assert: (args: {
        result: unknown;
        event: { messages: PiAgentMessage[] };
        server: Agento11yTestServer;
      }) => void;
    }

    const cases: PreflightCase[] = [
      {
        name: "redacts user text and forwards the original",
        hook: preflightTransform([
          { role: "user", parts: [{ type: "text", text: "email [REDACTED]" }] },
        ]),
        messages: () => [userMsg("email leak@example.com")],
        assert: ({ result, event, server }) => {
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "email [REDACTED]",
          );
          expect(result).toEqual({ messages: event.messages });
          expect(server.hookCalls).toHaveLength(1);
          const call = server.hookCalls[0]!;
          expect(call.phase).toBe("preflight");
          expect(call.body.context?.conversation_id).toBe("guards-conv-1");
          // The forwarded body carried the unredacted original. The server
          // dispatches on the snake_case `kind`, so the part carries it and
          // nothing else. conformance/hooks/README.md.
          const sent = call.body.input?.messages;
          expect(sent?.[0]?.role).toBe("user");
          expect(sent?.[0]?.parts).toEqual([
            { kind: "text", text: "email leak@example.com" },
          ]);
        },
      },
      ...[false, true].map(
        (isError): PreflightCase => ({
          name: `redacts tool-result content with isError=${isError}`,
          hook: preflightTransform([
            {
              role: "tool",
              parts: [
                {
                  type: "tool_result",
                  toolResult: {
                    tool_call_id: "tc-1",
                    name: "bash",
                    content: "out [REDACTED_API_KEY]",
                    ...(isError ? { is_error: true } : {}),
                  },
                },
              ],
            },
          ]),
          messages: () => [toolResultMsg("out sk-deadbeef", isError)],
          assert: ({ event, server }) => {
            expect(
              (event.messages[0] as PiToolResult).content[0],
            ).toMatchObject({
              type: "text",
              text: "out [REDACTED_API_KEY]",
            });
            expect((event.messages[0] as PiToolResult).isError).toBe(isError);
            // The server dispatches on the snake_case `kind`. An untagged
            // tool_result part arrives empty, so redaction has nothing to
            // rewrite. conformance/hooks/README.md.
            const sent = server.hookCalls[0]!.body.input?.messages;
            expect(sent?.[0]?.parts).toEqual([
              {
                kind: "tool_result",
                tool_result: {
                  tool_call_id: "tc-1",
                  name: "bash",
                  content: "out sk-deadbeef",
                  ...(isError ? { is_error: true } : {}),
                },
              },
            ]);
          },
        }),
      ),
      {
        name: "keeps assistant thinking parts untouched while redacting text",
        hook: preflightTransform([
          { role: "user", parts: [{ type: "text", text: "hi [REDACTED]" }] },
          { role: "assistant", parts: [{ type: "text", text: "answer red" }] },
        ]),
        messages: () => [
          userMsg("hi secret@example.com"),
          assistantFixture([
            { type: "thinking", thinking: "opaque-sig" },
            { type: "text", text: "answer original" },
          ]),
        ],
        assert: ({ event, server }) => {
          const asst = event.messages[1] as PiAssistantMessage;
          expect(asst.content[0]).toEqual({
            type: "thinking",
            thinking: "opaque-sig",
          });
          expect(asst.content[1]).toMatchObject({
            type: "text",
            text: "answer red",
          });
          // The ordinary projection drops thinking blocks on purpose
          // (their provider signature must round-trip unchanged), so the
          // assistant message goes out as one text part.
          const sent = server.hookCalls[0]!.body.input?.messages;
          expect(sent?.[1]?.parts).toEqual([
            { kind: "text", text: "answer original" },
          ]);
        },
      },
      {
        name: "applies Go/proto-encoded wire messages (role+Payload shape)",
        hook: {
          json: {
            action: "allow",
            evaluations: [],
            transformed_input: {
              messages: [
                {
                  role: 1,
                  name: "pi:0",
                  parts: [{ Payload: { Text: "hi [R]" } }],
                },
              ],
            },
          },
        },
        messages: () => [userMsg("hi secret@example.com")],
        assert: ({ event }) => {
          expect((event.messages[0] as PiUserMessage).content).toBe("hi [R]");
        },
      },
      {
        name: "leaves messages unchanged when the server applies no transform",
        hook: ALLOW,
        messages: () => [userMsg("nothing to redact")],
        assert: ({ result, event, server }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "nothing to redact",
          );
          expect(server.hookCalls).toHaveLength(1);
        },
      },
      {
        name: "fails open when the redacted message count diverges",
        hook: preflightTransform([
          { role: "user", parts: [{ type: "text", text: "a" }] },
          { role: "user", parts: [{ type: "text", text: "b" }] },
        ]),
        messages: () => [userMsg("original")],
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe("original");
        },
      },
      {
        name: "ignores a preflight deny (context cannot block)",
        hook: deny("preflight deny"),
        messages: () => [userMsg("still here")],
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "still here",
          );
        },
      },
      {
        name: "fails open on a 5xx hook error (failOpen=true)",
        hook: { status: 500, json: { error: "boom" } },
        messages: () => [userMsg("kept on error")],
        assert: ({ result, event, server }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "kept on error",
          );
          expect(server.hookCalls).toHaveLength(1);
        },
      },
      {
        name: "fails open on a hook timeout",
        env: { timeoutMs: 50 },
        hook: {
          delayMs: 250,
          ...preflightTransform([
            { role: "user", parts: [{ type: "text", text: "too late" }] },
          ]),
        },
        messages: () => [userMsg("kept on timeout")],
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "kept on timeout",
          );
        },
      },
      {
        name: "does not call the hook when guards are disabled",
        env: { enabled: false },
        hook: preflightTransform([
          { role: "user", parts: [{ type: "text", text: "[REDACTED]" }] },
        ]),
        messages: () => [userMsg("untouched")],
        assert: ({ result, event, server }) => {
          expect(result).toBeUndefined();
          expect((event.messages[0] as PiUserMessage).content).toBe(
            "untouched",
          );
          expect(server.hookCalls).toHaveLength(0);
        },
      },
    ];

    it.each(cases)("$name", async ({ env, hook, messages, assert }) => {
      if (env) setGuardEnv(env);
      nextHook = () => hook ?? ALLOW;
      const pi = await setup();
      const { result, event } = await runContext(pi, messages());
      assert({ result, event, server });
      expect(server.errors).toEqual([]);
    });
  });

  describe("ordinary request observation and export", () => {
    interface Entry {
      type: "message";
      id: string;
      parentId: string | null;
      message: PiAgentMessage;
    }

    function session(entries: Entry[] = []) {
      return {
        ...ctx,
        sessionManager: {
          ...ctx.sessionManager,
          getEntries: () => entries,
          getBranch: () => entries,
          buildSessionProjection: () => ({
            entries: entries.map((sourceEntry) => ({
              sourceEntry,
              messages: [sourceEntry.message],
            })),
          }),
        },
      };
    }

    async function boot(liveCtx: ReturnType<typeof session>) {
      const pi = new FakePi();
      registerExtension(pi as any);
      await pi.emit("session_start", {}, liveCtx);
      await pi.emit("turn_start", {}, liveCtx);
      return pi;
    }

    async function finish(
      pi: FakePi,
      liveCtx: ReturnType<typeof session>,
      entries: Entry[],
    ) {
      const message = assistantFixture();
      await pi.emit("message_end", { message }, liveCtx);
      entries.push({
        type: "message",
        id: `a-${entries.length}`,
        parentId: entries.at(-1)?.id ?? null,
        message,
      });
      await pi.emit("turn_end", { message, toolResults: [] }, liveCtx);
    }

    function redactFalcon(call: HookCall): HookResponse {
      if (call.phase !== "preflight") return ALLOW;
      return {
        json: {
          action: "allow",
          evaluations: [],
          transformed_input: {
            messages: call.body.input!.messages!.map((message) => ({
              ...message,
              parts: message.parts!.map((part) =>
                part.kind === "text"
                  ? {
                      ...part,
                      text: part.text!.replaceAll(
                        "Project Falcon",
                        "[REDACTED]",
                      ),
                    }
                  : part,
              ),
            })),
          },
        },
      };
    }

    it("records only finalized, observed, guard-safe input/title after an earlier context prefix", async () => {
      nextHook = redactFalcon;
      const history = { ...userMsg("restored private history"), timestamp: 0 };
      const entries: Entry[] = [
        { type: "message", id: "restored", parentId: null, message: history },
      ];
      const liveCtx = session(entries);
      const pi = await boot(liveCtx);
      expect(pi.handlers.has("context")).toBe(false);
      // Pi awaits every message_end handler before appending. A later handler
      // replaces the entire message: the plugin must never capture this early body.
      const event = { message: userMsg("EARLY PRIVATE BODY") };
      await pi.emit("message_end", event, liveCtx);
      event.message = userMsg("Status update on Project Falcon for the board");
      entries.push({
        type: "message",
        id: "new-user",
        parentId: "restored",
        message: event.message,
      });
      // Simulate an earlier context handler adding a system prefix and an
      // opaque thinking-only message. Native ordinals must survive the gap.
      const prefix = { role: "system", content: "Earlier context prefix" };
      const opaque = assistantFixture([
        { type: "thinking", thinking: "signed-private-reasoning" },
      ]);
      const native = [prefix, history, opaque, structuredClone(event.message)];
      const result = await pi.emit(
        "context_with_system",
        { messages: native },
        liveCtx,
      );
      expect(native[3]).toEqual(event.message);
      expect(result.messages).not.toBe(native);
      expect(result.messages[0]).toBe(prefix);
      expect(result.messages[2]).toBe(opaque);
      expect(result.messages[3].content).toBe(
        "Status update on [REDACTED] for the board",
      );
      expect(
        server.hookCalls[0]!.body.input!.messages!.map(
          (message) => message.name,
        ),
      ).toEqual(["pi:0", "pi:1", "pi:3"]);
      const sent = JSON.stringify(server.hookCalls[0]!.body);
      expect(sent).toContain("Project Falcon");
      expect(sent).not.toContain("EARLY PRIVATE BODY");
      expect(sent).not.toContain("signed-private-reasoning");
      await finish(pi, liveCtx, entries);
      // A tool-loop request sees the same occurrence, but may not record it twice.
      await pi.emit("turn_start", {}, liveCtx);
      await pi.emit(
        "context_with_system",
        { messages: result.messages },
        liveCtx,
      );
      await finish(pi, liveCtx, entries);
      await pi.emit("session_shutdown", {}, liveCtx);
      expect(generations).toHaveLength(2);
      expect(generations[0].input).toEqual([
        {
          role: "MESSAGE_ROLE_USER",
          name: "",
          parts: [{ text: "Status update on [REDACTED] for the board" }],
        },
      ]);
      expect(generations[0].metadata["agento11y.conversation.title"]).toContain(
        "[REDACTED]",
      );
      expect(generations[1].input ?? []).toEqual([]);
      const exported = JSON.stringify(generations);
      for (const privateText of [
        "Falcon",
        "EARLY PRIVATE BODY",
        "restored private history",
        "signed-private-reasoning",
      ])
        expect(exported).not.toContain(privateText);
      expect(server.errors).toEqual([]);
    });

    it.each([
      "duplicate metadata",
      "missing observation",
      "empty transform",
      "missing identity",
      "wrong identity",
      "changed true tool-result flag",
    ])("omits body/title rather than using raw text: %s", async (failure) => {
      nextHook = (call) => {
        if (failure === "empty transform")
          return preflightTransform([
            { role: "user", parts: [{ type: "text", text: "" }] },
          ]);
        if (failure === "missing identity" || failure === "wrong identity")
          return {
            json: {
              action: "allow",
              evaluations: [],
              transformed_input: {
                messages: [
                  {
                    role: "user",
                    ...(failure === "wrong identity" ? { name: "pi:9" } : {}),
                    parts: [{ type: "text", text: "[REDACTED]" }],
                  },
                ],
              },
            },
          };
        const response = redactFalcon(call);
        if (failure === "changed true tool-result flag")
          response.json = JSON.parse(
            JSON.stringify(response.json).replaceAll(
              '"is_error":true',
              '"is_error":false',
            ),
          );
        return response;
      };
      const entries: Entry[] = [];
      const liveCtx = session(entries);
      const pi = await boot(liveCtx);
      const message = userMsg("Project Falcon private prompt");
      await pi.emit("message_end", { message }, liveCtx);
      entries.push({ type: "message", id: "u1", parentId: null, message });
      if (failure === "duplicate metadata") {
        // Distinct occurrences with the same role/timestamp cannot be joined
        // by text, even though the guard safely transforms both bodies.
        const duplicate = { ...message };
        await pi.emit("message_end", { message: duplicate }, liveCtx);
        entries.push({
          type: "message",
          id: "u2",
          parentId: "u1",
          message: duplicate,
        });
      }
      if (failure === "changed true tool-result flag") {
        // Changing status must invalidate the complete replacement.
        const tool = toolResultMsg("Project Falcon tool output", true);
        await pi.emit("message_end", { message: tool }, liveCtx);
        entries.push({
          type: "message",
          id: "tool",
          parentId: "u1",
          message: tool,
        });
      }
      const native = entries.map((entry) => structuredClone(entry.message));
      if (failure !== "missing observation") {
        const result = await pi.emit(
          "context_with_system",
          { messages: native },
          liveCtx,
        );
        if (failure === "duplicate metadata")
          expect(
            result.messages.map((message: PiUserMessage) => message.content),
          ).toEqual(["[REDACTED] private prompt", "[REDACTED] private prompt"]);
        else expect(result).toBeUndefined();
      }
      expect(native[0]).toEqual(message);
      await finish(pi, liveCtx, entries);
      await pi.emit("session_shutdown", {}, liveCtx);
      expect(generations).toHaveLength(1);
      expect(generations[0].input ?? []).toEqual([]);
      expect(generations[0].metadata["agento11y.conversation.title"]).toBe(
        "guards-conv-1",
      );
      expect(JSON.stringify(generations)).not.toContain("Falcon");
      expect(server.errors).toEqual([]);
    });

    it.each([
      {
        model: { provider: "openai", id: "switched-model" },
        expected: { provider: "openai", name: "switched-model" },
      },
      {
        model: { provider: "pi", id: "virtual", api: "pi-virtual" },
        expected: { provider: "unknown", name: "unknown" },
      },
    ])("sends effective tags and current ordinary model $model, not the previous assistant", async ({
      model,
      expected,
    }) => {
      process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
      process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user";
      process.env.AGENTO11Y_USER_ID = "auto-user";
      process.env.AGENTO11Y_TAGS = "team=configured-team,cwd=configured-cwd";
      const liveCtx = { ...session(), model };
      const pi = await boot(liveCtx);
      await pi.emit("message_end", { message: assistantFixture() }, liveCtx);
      await pi.emit(
        "context_with_system",
        { messages: [userMsg("hello")] },
        liveCtx,
      );
      await pi.emit(
        "tool_call",
        { toolCallId: "tc-1", toolName: "bash", input: { command: "ls" } },
        liveCtx,
      );
      await pi.emit("session_shutdown", {}, liveCtx);
      expect(server.hookCalls).toHaveLength(2);
      expect(server.hookCalls[0]!.body.context!.model).toEqual(expected);
      expect(server.hookCalls[1]!.body.context!.model).toEqual({
        provider: "anthropic",
        name: "claude-sonnet-4",
      });
      for (const call of server.hookCalls)
        expect((call.body.context as any).tags).toEqual({
          user: "auto-user",
          team: "configured-team",
          cwd: ctx.cwd,
        });
      expect(server.errors).toEqual([]);
    });
  });

  describe("postflight (tool_call)", () => {
    interface PostflightCase {
      name: string;
      env?: GuardEnv;
      hook?: HookResponse;
      input?: Record<string, unknown>;
      assert: (args: {
        result: unknown;
        event: { input: Record<string, unknown> };
        server: Agento11yTestServer;
      }) => void;
    }

    const redactedToolCall = (id: string, inputJson: string) =>
      postflightTransform([
        {
          role: "assistant",
          parts: [
            {
              type: "tool_call",
              toolCall: { id, name: "bash", input_json: inputJson },
            },
          ],
        },
      ]);

    const cases: PostflightCase[] = [
      {
        name: "redacts tool arguments in place",
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        assert: ({ result, event, server }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo [REDACTED]" });
          expect(server.hookCalls[0]!.phase).toBe("postflight");
          expect(server.hookCalls[0]!.body.context?.conversation_id).toBe(
            "guards-conv-1",
          );
        },
      },
      {
        // A tool-filter rule matches only what the server can decode. The
        // server dispatches on the snake_case `kind`. A part tagged only with
        // the SDK's camelCase `type` arrives empty, so a tool-filter rule sees
        // no tool calls. conformance/hooks/README.md.
        name: "sends the tool call as a kind:tool_call part with embedded input_json",
        assert: ({ server }) => {
          const sent = server.hookCalls[0]!.body.input?.output;
          expect(sent?.[0]?.parts).toEqual([
            {
              kind: "tool_call",
              tool_call: {
                id: "tc-1",
                name: "bash",
                // Tool arguments travel as embedded JSON, which is what an
                // argument-level rule matches on.
                input_json: { command: "echo sk-real-secret" },
              },
            },
          ]);
        },
      },
      {
        name: "drops keys the server omits from the redacted args",
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        input: { command: "echo sk-real-secret", token: "sk-leak" },
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          // `token` was not in the server's redacted set, so it must not
          // survive on the merged input with its original value.
          expect(event.input).toEqual({ command: "echo [REDACTED]" });
        },
      },
      {
        name: "fails open (no block) when the tool input cannot be mutated",
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        input: Object.freeze({ command: "echo sk-real-secret" }),
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "blocks when frozen tool input prevents a rewrite (failOpen=false)",
        env: { failOpen: false },
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        input: Object.freeze({ command: "echo sk-real-secret" }),
        assert: ({ result, event }) => {
          expect(result).toEqual({
            block: true,
            reason:
              "Agent Observability could not apply a required guard transformation.",
          });
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "blocks when sealed tool input prevents key deletion (failOpen=false)",
        env: { failOpen: false },
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        input: Object.seal({
          command: "echo sk-real-secret",
          token: "sk-leak",
        }),
        assert: ({ result }) => {
          expect(result).toMatchObject({ block: true });
        },
      },
      {
        name: "blocks after a partial rewrite hits a read-only property (failOpen=false)",
        env: { failOpen: false },
        hook: redactedToolCall(
          "tc-1",
          '{"other":"[REDACTED]","command":"echo [REDACTED]"}',
        ),
        input: Object.defineProperty({ other: "secret" }, "command", {
          value: "echo sk-real-secret",
          enumerable: true,
          writable: false,
        }),
        assert: ({ result, event }) => {
          expect(result).toMatchObject({ block: true });
          expect(event.input).toEqual({
            other: "[REDACTED]",
            command: "echo sk-real-secret",
          });
        },
      },
      {
        name: "blocks on a deny verdict with policy wording",
        hook: deny("blocked rm -rf"),
        assert: ({ result }) => {
          expect(result).toMatchObject({ block: true });
          const reason = (result as { reason: string }).reason;
          expect(reason).toContain("blocked rm -rf");
          expect(reason).toContain("A Grafana Agent Observability policy");
          expect(reason).toContain("Stop and tell the user");
        },
      },
      {
        name: "leaves input unchanged on a plain allow",
        hook: ALLOW,
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "ignores a transform aimed at a different toolCallId",
        hook: redactedToolCall("other-id", '{"command":"echo X"}'),
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "ignores an unparseable transform input_json",
        hook: redactedToolCall("tc-1", "not json"),
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "blocks fail-closed on a 5xx hook error (failOpen=false)",
        env: { failOpen: false },
        hook: { status: 500, json: { error: "boom" } },
        assert: ({ result }) => {
          expect(result).toMatchObject({ block: true });
          const reason = (result as { reason: string }).reason;
          expect(reason).toContain("could not evaluate");
          expect(reason).toContain("safety measure");
        },
      },
      {
        name: "fails open on a 5xx hook error (failOpen=true)",
        hook: { status: 500, json: { error: "boom" } },
        assert: ({ result, event }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
        },
      },
      {
        name: "does not call the hook when guards are disabled",
        env: { enabled: false },
        hook: redactedToolCall("tc-1", '{"command":"echo [REDACTED]"}'),
        assert: ({ result, event, server }) => {
          expect(result).toBeUndefined();
          expect(event.input).toEqual({ command: "echo sk-real-secret" });
          expect(server.hookCalls).toHaveLength(0);
        },
      },
    ];

    it.each(cases)("$name", async ({ env, hook, input, assert }) => {
      if (env) setGuardEnv(env);
      nextHook = () => hook ?? ALLOW;
      const pi = await setup();
      const { result, event } = await runToolCall(
        pi,
        input ?? { command: "echo sk-real-secret" },
      );
      assert({ result, event, server });
      expect(server.errors).toEqual([]);
    });
  });

  describe("built-in summary preparation over HTTP", () => {
    function summaryEvent(kind: "compaction" | "tree") {
      return kind === "compaction"
        ? {
            type: "session_before_compact" as const,
            reason: "manual",
            willRetry: false,
            signal: new AbortController().signal,
            branchEntries: [],
            customInstructions: "keep decisions",
            preparation: {
              messagesToSummarize: [
                userMsg("Project Falcon user"),
                assistantFixture([
                  {
                    type: "toolCall",
                    id: "tc-1",
                    name: "bash",
                    arguments: { command: "Project Falcon command" },
                  },
                ]),
                toolResultMsg("Project Falcon result"),
              ],
              turnPrefixMessages: [userMsg("Project Falcon prefix")],
              previousSummary: "Project Falcon previous",
              fileOps: {
                read: new Set(["Project Falcon/read"]),
                written: new Set(),
                edited: new Set(),
              },
              firstKeptEntryId: "kept",
              tokensBefore: 100,
              isSplitTurn: true,
              settings: {
                enabled: true,
                keepRecentTokens: 0,
                reserveTokens: 10,
              },
            },
          }
        : {
            type: "session_before_tree" as const,
            signal: new AbortController().signal,
            preparation: {
              userWantsSummary: true,
              targetId: "target",
              oldLeafId: "leaf",
              commonAncestorId: null,
              customInstructions: "Project Falcon focus",
              replaceInstructions: true,
              entriesToSummarize: [
                {
                  type: "message",
                  id: "u",
                  parentId: null,
                  timestamp: "time",
                  message: userMsg("Project Falcon user"),
                },
              ],
            },
          };
    }
    it.each([
      "compaction",
      "tree",
    ] as const)("applies validated %s preparation replacements through the real decoder", async (kind) => {
      nextHook = (call) => ({
        json: {
          action: "allow",
          evaluations: [],
          transformed_input: {
            messages: JSON.parse(
              JSON.stringify(call.body.input?.messages).replaceAll(
                "Project Falcon",
                "[R]",
              ),
            ),
          },
        },
      });
      const pi = await setup();
      const event = summaryEvent(kind);
      const original = structuredClone(event.preparation);
      const result = await pi.handlers.get(event.type)!(event, ctx);
      if (event.type === "session_before_compact") {
        expect(result).toBeUndefined();
        expect(event.preparation.messagesToSummarize[0]?.content).toBe(
          "[R] user",
        );
        expect(event.preparation.previousSummary).toBe("[R] previous");
        expect([...event.preparation.fileOps.read]).toEqual(["[R]/read"]);
      } else {
        expect(result).toEqual({ customInstructions: "[R] focus" });
        expect(event.preparation.entriesToSummarize[0]?.message.content).toBe(
          "[R] user",
        );
      }
      expect(JSON.stringify(original)).toContain("Project Falcon");
      expect(
        server.hookCalls.at(-1)?.body.context?.tags?.["pi.call_kind"],
      ).toBe(kind === "compaction" ? "compaction" : "branch_summary");
      await pi.emit("session_shutdown", {}, ctx);
      expect(server.errors).toEqual([]);
    });

    it.each([
      { outcome: "failure", failOpen: false, cancel: true },
      { outcome: "failure", failOpen: true, cancel: false },
      { outcome: "deny", failOpen: false, cancel: true },
      { outcome: "deny", failOpen: true, cancel: true },
      { outcome: "invalid", failOpen: false, cancel: true },
      { outcome: "invalid", failOpen: true, cancel: true },
    ])("handles $outcome with failOpen=$failOpen without raw fallback", async ({
      outcome,
      failOpen,
      cancel,
    }) => {
      setGuardEnv({ failOpen });
      nextHook = () =>
        outcome === "failure"
          ? { status: 500, json: { error: "private evaluator detail" } }
          : outcome === "deny"
            ? deny("private policy detail")
            : {
                json: {
                  action: "allow",
                  evaluations: [],
                  transformed_input: { messages: [] },
                },
              };
      const pi = await setup();
      for (const kind of ["compaction", "tree"] as const) {
        const event = summaryEvent(kind);
        const original = structuredClone(event.preparation);
        expect(await pi.handlers.get(event.type)!(event, ctx)).toEqual(
          cancel ? { cancel: true } : undefined,
        );
        expect(event.preparation).toEqual(original);
      }
      await pi.emit("session_shutdown", {}, ctx);
      expect(server.errors).toEqual([]);
    });
  });
});
