// Pi guards: real-SDK-over-HTTP integration tests.
//
// Drives the @grafana/agento11y-pi extension through a faked pi host, but unlike
// the handler-wiring unit tests (index.test.ts) the Sigil client here is the
// real JS SDK pointed at a local HTTP server. Each test exercises the full
// preflight (`context`) or postflight (`tool_call`) path end to end: real
// Agento11yClient.evaluateHook -> HTTP hooks:evaluate round-trip -> wire parse ->
// in-place write-back into pi's messages / tool input.
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

function toolResultMsg(text: string): PiToolResult {
  return {
    role: "toolResult",
    toolCallId: "tc-1",
    toolName: "bash",
    content: [{ type: "text", text }],
    isError: false,
    timestamp: 1,
  };
}

// Wire-shaped (snake_case) hook response bodies, matching what the Sigil API
// emits and what js/src/hooks.ts parses.
function preflightTransform(messages: unknown[]): HookResponse {
  return {
    json: { action: "allow", evaluations: [], transformed_input: { messages } },
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
  let nextHook: (call: { phase: string }) => HookResponse = () => ALLOW;

  beforeEach(async () => {
    server = await startAgento11yTestServer({ hook: (call) => nextHook(call) });
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

  // Boot a session and cache an assistant model so preflight forwards a real
  // model rather than the unknown fallback.
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
    const result = await pi.handlers.get("context")!(event, ctx);
    return { result, event };
  }

  async function runToolCall(
    pi: FakePi,
    input: Record<string, unknown>,
  ): Promise<{ result: unknown; event: { input: Record<string, unknown> } }> {
    const event = { toolCallId: "tc-1", toolName: "bash", input };
    const result = await pi.handlers.get("tool_call")!(event, ctx);
    return { result, event };
  }

  describe("preflight (context)", () => {
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
          // The forwarded body carried the unredacted original.
          const sent = call.body.input?.messages as
            | Array<{ parts: Array<{ text: string }> }>
            | undefined;
          expect(sent?.[0]?.parts[0]?.text).toBe("email leak@example.com");
        },
      },
      {
        name: "redacts tool-result content via the tool_result part",
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
                },
              },
            ],
          },
        ]),
        messages: () => [toolResultMsg("out sk-deadbeef")],
        assert: ({ event }) => {
          expect((event.messages[0] as PiToolResult).content[0]).toMatchObject({
            type: "text",
            text: "out [REDACTED_API_KEY]",
          });
        },
      },
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
        assert: ({ event }) => {
          const asst = event.messages[1] as PiAssistantMessage;
          expect(asst.content[0]).toEqual({
            type: "thinking",
            thinking: "opaque-sig",
          });
          expect(asst.content[1]).toMatchObject({
            type: "text",
            text: "answer red",
          });
        },
      },
      {
        name: "applies Go/proto-encoded wire messages (role+Payload shape)",
        hook: {
          json: {
            action: "allow",
            evaluations: [],
            transformed_input: {
              messages: [{ role: 1, parts: [{ Payload: { Text: "hi [R]" } }] }],
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
});
