import type { HookEvaluateResponse, Message } from "@grafana/agento11y";
import { describe, expect, it, vi } from "vitest";
import type {
  GuardResult,
  PreflightTransformArgs,
  PreflightTransformResult,
} from "./guard.js";
import { runPreflightTransform, runToolCallGuard } from "./guard.js";

/** Narrow a GuardResult to a block result, failing the test otherwise. */
function asBlock(res: GuardResult): { block: true; reason: string } {
  if (!res || !("block" in res)) {
    throw new Error(`expected a block result, got ${JSON.stringify(res)}`);
  }
  return res;
}

/** Narrow a GuardResult to a transform result, failing the test otherwise. */
function asTransform(res: GuardResult): { transform: Record<string, unknown> } {
  if (!res || !("transform" in res)) {
    throw new Error(`expected a transform result, got ${JSON.stringify(res)}`);
  }
  return res;
}

describe("runToolCallGuard", () => {
  it("returns undefined when Agent Observability allows the tool call", async () => {
    const calls: unknown[] = [];
    const client = {
      evaluateHook: async (req: unknown) => {
        calls.push(req);
        return { action: "allow", evaluations: [] };
      },
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      conversationId: "opencode-session-1",
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    expect(res).toBeUndefined();
    expect(calls).toHaveLength(1);
    expect((calls[0] as any).phase).toBe("postflight");
    expect((calls[0] as any).context.conversationId).toBe("opencode-session-1");
    expect((calls[0] as any).input.output[0].parts[0].toolCall.inputJSON).toBe(
      JSON.stringify({ command: "ls" }),
    );

    await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      conversationId: "   ",
      toolName: "bash",
      input: {},
      failOpen: true,
    });
    expect((calls[1] as any).context).not.toHaveProperty("conversationId");
  });

  it("returns a wrapped policy-deny result when Agent Observability denies the tool call", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "deny",
        reason: "blocked by rule",
        evaluations: [],
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "rm -rf /" },
      failOpen: true,
    });

    const block = asBlock(res);
    expect(block.block).toBe(true);
    expect(block.reason).toContain("A Grafana Agent Observability policy");
    expect(block.reason).toContain('"bash"');
    expect(block.reason).toContain("Reason: blocked by rule");
    expect(block.reason).toContain("Stop and tell the user");
  });

  it("returns the raw reason for an evaluation-failure deny", async () => {
    // The local daemon answers with this rule id when its own chained Cloud
    // hook call failed under GUARDS_FAIL_OPEN=false. No policy ran, so the
    // message must not claim one blocked the call.
    const client = {
      evaluateHook: async () => ({
        action: "deny",
        ruleId: "__agento11y_guard_evaluation_failure",
        reason:
          'agento11y could not evaluate the Grafana Agent Observability guard for the "bash" tool call, so it was blocked as a safety measure. Details: connection refused',
        evaluations: [],
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    const block = asBlock(res);
    expect(block.reason).toContain("could not evaluate");
    expect(block.reason).toContain("connection refused");
    expect(block.reason).not.toContain("A Grafana Agent Observability policy");
  });

  it("omits the Reason clause when Agent Observability denies without a reason", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "deny",
        reason: "   ",
        evaluations: [],
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    const block = asBlock(res);
    expect(block.block).toBe(true);
    expect(block.reason).toContain("A Grafana Agent Observability policy");
    expect(block.reason).toContain('"bash"');
    expect(block.reason).not.toContain("Reason:");
    expect(block.reason).toContain("Stop and tell the user");
  });

  it("returns a wrapped fail-closed message when the SDK throws (fail-closed mode)", async () => {
    const client = {
      evaluateHook: async () => {
        throw new Error("network down");
      },
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: {},
      failOpen: false,
    });

    const block = asBlock(res);
    expect(block.block).toBe(true);
    expect(block.reason).toContain("could not evaluate");
    expect(block.reason).toContain("safety measure");
    expect(block.reason).toContain('"bash"');
    expect(block.reason).toContain("network down");
    expect(block.reason).not.toContain(
      "A Grafana Agent Observability policy blocked",
    );
  });

  it("allows when the SDK throws (fail-open mode)", async () => {
    const client = {
      evaluateHook: async () => {
        throw new Error("network down");
      },
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: {},
      failOpen: true,
    });

    expect(res).toBeUndefined();
  });

  it("allows when JSON.stringify throws (fail-open mode)", async () => {
    const client = {
      evaluateHook: async () => {
        return { action: "allow", evaluations: [] };
      },
    };

    const circular: any = {};
    circular.self = circular;

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: circular,
      failOpen: true,
    });

    expect(res).toBeUndefined();
  });

  it("returns a transform when the server redacts the tool arguments", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "c1",
                    name: "bash",
                    inputJSON: JSON.stringify({ command: "echo [REDACTED]" }),
                  },
                },
              ],
            },
          ],
        },
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "echo sonia@grafana.com" },
      failOpen: true,
    });

    const t = asTransform(res);
    expect(t.transform).toEqual({ command: "echo [REDACTED]" });
  });

  it("prefers a deny over a transform when the server returns both", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "deny",
        reason: "pii detected",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "c1",
                    name: "bash",
                    inputJSON: JSON.stringify({ command: "redacted" }),
                  },
                },
              ],
            },
          ],
        },
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "leak" },
      failOpen: true,
    });

    expect(asBlock(res).reason).toContain("Reason: pii detected");
  });

  it("ignores a transform aimed at a different toolCallId even when it is the only rewritten call", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "other-call",
                    name: "bash",
                    inputJSON: JSON.stringify({ command: "x" }),
                  },
                },
              ],
            },
          ],
        },
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    expect(res).toBeUndefined();
  });

  it("rejects a conflicting ID even when the echoed name has surrounding whitespace", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "other",
                    name: "read",
                    inputJSON: JSON.stringify({ path: "a" }),
                  },
                },
                {
                  type: "tool_call",
                  toolCall: {
                    id: "other-call",
                    name: " Bash ",
                    inputJSON: JSON.stringify({ command: "x" }),
                  },
                },
              ],
            },
          ],
        },
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    expect(res).toBeUndefined();
  });

  it("drops a transform whose arguments are not a JSON object", async () => {
    const client = {
      evaluateHook: async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: {
                    id: "c1",
                    name: "bash",
                    inputJSON: JSON.stringify(["not", "an", "object"]),
                  },
                },
              ],
            },
          ],
        },
      }),
    };

    const res = await runToolCallGuard({
      client: client as any,
      agentName: "opencode",
      model: { provider: "anthropic", name: "claude" },
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    expect(res).toBeUndefined();
  });
});

describe("tool-call transform identity", () => {
  const redacted = '{"command":"echo [REDACTED]"}';
  const other = '{"command":"other call"}';
  const call = (id: string, name: string, inputJSON: string) => ({
    type: "tool_call" as const,
    toolCall: { id, name, inputJSON },
  });

  describe.each<[string, string, string | undefined, boolean, boolean]>([
    ["exact ID", "c1", "c1", true, true],
    ["trimmed exact ID", " c1 ", "\tc1 ", true, true],
    ["conflicting ID", "c1", "c2", false, false],
    ["trimmed conflicting ID", " c1 ", " c2 ", false, false],
    ["case-sensitive ID", "c1", "C1", false, false],
    ["response ID absent", "c1", undefined, false, true],
    ["response ID whitespace", "c1", " \t", false, true],
    ["caller ID absent", "", "c2", false, true],
    ["caller ID whitespace", " \t", "c2", false, true],
    ["both IDs absent", "", undefined, false, true],
    ["both IDs whitespace", " \t", " \t", false, true],
  ])("%s", (_label, toolCallId, id, exact, compatibleID) => {
    it.each<[string, string, string | undefined, boolean]>([
      ["same name", "bash", "bash", true],
      ["trimmed case-insensitive name", " bash ", "\tBash ", true],
      ["different name", "bash", "write", false],
      ["response name absent", "bash", undefined, true],
      ["response name whitespace", "bash", " \t", true],
      ["caller name absent", "", "write", true],
      ["caller name whitespace", " \t", "write", true],
      ["both names absent", "", undefined, true],
      ["both names whitespace", " \t", " \t", true],
    ])("%s", async (_label, toolName, name, compatibleName) => {
      const { client } = makePreflightClient(async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: [
            {
              role: "assistant",
              parts: [
                {
                  type: "tool_call",
                  toolCall: { id, name: name as string, inputJSON: redacted },
                },
              ],
            },
          ],
        },
      }));
      const result = await runToolCallGuard({
        client,
        toolCallId,
        toolName,
        agentName: "opencode",
        model: { provider: "anthropic", name: "claude" },
        input: {},
        failOpen: false,
      });
      expect(result).toEqual(
        exact || (compatibleID && compatibleName)
          ? { transform: JSON.parse(redacted) }
          : undefined,
      );
    });
  });

  it.each<
    [
      string,
      string,
      string,
      ReturnType<typeof call>[],
      string | undefined,
      string,
    ]
  >([
    ["no calls", "c1", "bash", [], undefined, ""],
    [
      "conflicting ID with unrelated call",
      "c1",
      "bash",
      [call("c2", "bash", redacted), call("c3", "read", other)],
      undefined,
      "no part matched",
    ],
    [
      "wrong name with unrelated call",
      "c1",
      "bash",
      [call("", "write", redacted), call("c3", "read", other)],
      undefined,
      "no part matched",
    ],
    [
      "ID-less match with unrelated call",
      "c1",
      "bash",
      [call("", "bash", redacted), call("c3", "read", other)],
      redacted,
      "",
    ],
    [
      "name-less match with unrelated call",
      "c1",
      "bash",
      [call("", "", redacted), call("c3", "read", other)],
      redacted,
      "",
    ],
    [
      "caller ID absent with unrelated call",
      "",
      "bash",
      [call("c2", "bash", redacted), call("c3", "read", other)],
      redacted,
      "",
    ],
    [
      "caller name absent with unrelated ID",
      "c1",
      "",
      [call("", "write", redacted), call("c3", "read", other)],
      redacted,
      "",
    ],
    [
      "whitespace name cannot override conflicting ID",
      "c1",
      "bash",
      [call("c2", " Bash ", redacted), call("c3", "read", other)],
      undefined,
      "no part matched",
    ],
    [
      "whitespace name matches without conflicting ID",
      "c1",
      "bash",
      [call("", " Bash ", redacted), call("c3", "read", other)],
      redacted,
      "",
    ],
    [
      "duplicate fallback",
      "c1",
      "bash",
      [call("", "bash", redacted), call("", "bash", redacted)],
      undefined,
      "no part matched",
    ],
    [
      "missing name makes fallback ambiguous",
      "c1",
      "bash",
      [call("", "bash", redacted), call("", "", other)],
      undefined,
      "no part matched",
    ],
    [
      "missing caller ID makes same names ambiguous",
      "",
      "bash",
      [call("c2", "bash", redacted), call("c3", "bash", other)],
      undefined,
      "no part matched",
    ],
    [
      "missing caller identity is ambiguous",
      "",
      "",
      [call("c2", "bash", redacted), call("c3", "read", other)],
      undefined,
      "no part matched",
    ],
    [
      "conflicting ID does not compete with fallback",
      "c1",
      "bash",
      [call("c2", "bash", other), call("", "bash", redacted)],
      redacted,
      "",
    ],
    [
      "exact ID precedes fallback and name",
      "c1",
      "bash",
      [call("", "bash", other), call("c1", "write", redacted)],
      redacted,
      "",
    ],
    [
      "trimmed exact ID precedes fallback",
      " c1 ",
      "bash",
      [call("", "bash", other), call(" c1 ", "", redacted)],
      redacted,
      "",
    ],
    [
      "duplicate exact ID",
      "c1",
      "bash",
      [call("c1", "bash", redacted), call("c1", "bash", redacted)],
      undefined,
      "no part matched",
    ],
    [
      "duplicate trimmed ID with different names",
      "c1",
      "bash",
      [
        call(" c1 ", "read", other),
        call("c1", "bash", redacted),
        call("", "bash", other),
      ],
      undefined,
      "no part matched",
    ],
    [
      "invalid exact arguments cannot use fallback",
      "c1",
      "bash",
      [call("c1", "bash", "not json"), call("", "bash", redacted)],
      undefined,
      "invalid JSON arguments",
    ],
    [
      "invalid duplicate exact arguments remain ambiguous",
      "c1",
      "bash",
      [call("c1", "bash", "not json"), call("c1", "bash", redacted)],
      undefined,
      "no part matched",
    ],
    [
      "invalid fallback arguments remain ambiguous",
      "c1",
      "bash",
      [call("", "bash", "not json"), call("", "bash", redacted)],
      undefined,
      "no part matched",
    ],
    [
      "invalid unrelated arguments are ignored",
      "c1",
      "bash",
      [call("c2", "bash", "not json"), call("", "bash", redacted)],
      redacted,
      "",
    ],
    [
      "malformed JSON",
      "c1",
      "bash",
      [call("c1", "bash", "not json")],
      undefined,
      "invalid JSON arguments",
    ],
    [
      "empty arguments",
      "c1",
      "bash",
      [call("c1", "bash", "")],
      undefined,
      "empty arguments",
    ],
    [
      "array arguments",
      "c1",
      "bash",
      [call("c1", "bash", "[]")],
      undefined,
      "not a JSON object",
    ],
    [
      "null arguments",
      "c1",
      "bash",
      [call("c1", "bash", "null")],
      undefined,
      "not a JSON object",
    ],
    [
      "number arguments",
      "c1",
      "bash",
      [call("c1", "bash", "42")],
      undefined,
      "not a JSON object",
    ],
    [
      "boolean arguments",
      "c1",
      "bash",
      [call("c1", "bash", "true")],
      undefined,
      "not a JSON object",
    ],
    [
      "empty object arguments",
      "c1",
      "bash",
      [call("c1", "bash", "{}")],
      "{}",
      "",
    ],
  ])("%s", async (_label, toolCallId, toolName, parts, want, wantLog) => {
    for (const ordered of [parts, [...parts].reverse()]) {
      const { client } = makePreflightClient(async () => ({
        action: "allow",
        evaluations: [],
        transformedInput: {
          output: ordered.map((part) => ({
            role: "assistant",
            parts: [{ type: "text", text: "unrelated text" }, part],
          })),
        },
      }));
      const warn = vi.fn();
      const result = await runToolCallGuard({
        client,
        toolCallId,
        toolName,
        logger: { warn },
        agentName: "opencode",
        model: { provider: "anthropic", name: "claude" },
        input: {},
        failOpen: false,
      });
      expect(result).toEqual(
        want ? { transform: JSON.parse(want) } : undefined,
      );
      if (wantLog)
        expect(warn).toHaveBeenCalledWith(expect.stringContaining(wantLog));
      if (!want)
        expect(warn).not.toHaveBeenCalledWith(
          expect.stringContaining("applied"),
        );
    }
  });
});

/**
 * Records the request and the hooks-config override of each call, so a test can
 * check the phase override that stops the SDK short-circuiting the call (see
 * `createAgento11yClient`).
 */
function makePreflightClient(respond: () => Promise<HookEvaluateResponse>): {
  client: PreflightTransformArgs["client"];
  calls: Array<{ req: any; override: unknown }>;
} {
  const calls: Array<{ req: any; override: unknown }> = [];
  const client = {
    evaluateHook: vi.fn(async (req: any, override?: unknown) => {
      calls.push({ req, override });
      return respond();
    }),
  } as unknown as PreflightTransformArgs["client"];
  return { client, calls };
}

function makePreflightArgs(
  overrides?: Partial<PreflightTransformArgs>,
): PreflightTransformArgs {
  return {
    client: {} as PreflightTransformArgs["client"],
    agentName: "opencode:build",
    agentVersion: "1.2.3",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
    conversationId: "opencode-session-1",
    messages: [{ role: "user", parts: [{ type: "text", text: "hi" }] }],
    failOpen: true,
    ...overrides,
  };
}

describe("runPreflightTransform", () => {
  const ALLOW: HookEvaluateResponse = { action: "allow", evaluations: [] };
  const REDACTED: Message[] = [
    {
      role: "user",
      parts: [{ type: "text", text: "authorization=[REDACTED]" }],
    },
  ];

  type PreflightCase = {
    name: string;
    /** Server response. Throwing stands in for a transport error. */
    respond?: () => Promise<HookEvaluateResponse>;
    args?: Partial<PreflightTransformArgs>;
    want?: PreflightTransformResult;
    /** Substrings that must each appear in a logged warning. */
    wantWarn?: string[];
    assert?: (calls: Array<{ req: any; override: unknown }>) => void;
  };

  const cases: PreflightCase[] = [
    {
      name: "sends the conversation and the execution context",
      assert: (calls) => {
        const { req, override } = calls[0]!;
        expect(req.phase).toBe("preflight");
        expect(req.context).toEqual({
          agentName: "opencode:build",
          agentVersion: "1.2.3",
          conversationId: "opencode-session-1",
          model: { provider: "anthropic", name: "claude-sonnet-4" },
        });
        expect(req.input.messages).toEqual([
          { role: "user", parts: [{ type: "text", text: "hi" }] },
        ]);
        // The opencode client pins `hooks.phases` to `["postflight"]`, so
        // without the override the SDK would answer allow without calling the
        // server. `failOpen: false` keeps it from turning a failed evaluation
        // into a synthetic allow, which would make a timeout unloggable here.
        expect(override).toEqual({
          enabled: true,
          phases: ["preflight"],
          failOpen: false,
        });
      },
    },
    {
      name: "omits a blank conversation ID",
      args: { conversationId: "   " },
      assert: (calls) => {
        expect(calls[0]!.req.context).not.toHaveProperty("conversationId");
      },
    },
    {
      name: "substitutes unknown for an unresolved provider and model name",
      args: { model: { provider: "", name: "" } },
      assert: (calls) => {
        expect(calls[0]!.req.context.model).toEqual({
          provider: "unknown",
          name: "unknown",
        });
      },
    },
    {
      name: "returns the redacted messages from transformedInput",
      respond: async () => ({
        ...ALLOW,
        transformedInput: { messages: REDACTED },
      }),
      want: { messages: REDACTED },
    },
    {
      name: "returns undefined when the server sends no transformedInput",
    },
    {
      name: "returns undefined when the server sends an empty message list",
      respond: async () => ({ ...ALLOW, transformedInput: { messages: [] } }),
    },
    {
      name: "fails open on a transport error, logging a warning",
      respond: async () => {
        throw new Error("network down");
      },
      wantWarn: ["preflight transform eval failed"],
    },
    {
      // The SDK aborts on its own timeout and, configured fail-closed, raises
      // it. Preflight cannot block, so the rejection still has to resolve to
      // "no transform".
      name: "fails open when the evaluation times out",
      respond: async () => {
        throw new Error("The operation was aborted due to timeout");
      },
      wantWarn: ["preflight transform eval failed"],
    },
    {
      name: "reports a deny as a reason to refuse the turn",
      respond: async () => ({
        action: "deny",
        reason: "preflight deny",
        evaluations: [],
      }),
      want: { block: expect.stringContaining("preflight deny") as any },
    },
    {
      // A deny stops the turn, so the transform it carries has nothing left to
      // rewrite: the conversation never reaches the provider.
      name: "refuses without the transform a deny response carries",
      respond: async () => ({
        action: "deny",
        reason: "preflight deny",
        evaluations: [],
        transformedInput: { messages: REDACTED },
      }),
      want: { block: expect.stringContaining("preflight deny") as any },
    },
    {
      // The daemon's fail-closed deny explains itself, so it is passed through
      // rather than wrapped as a policy decision.
      name: "passes a guard-evaluation-failure deny through unwrapped",
      respond: async () => ({
        action: "deny",
        ruleId: "__agento11y_guard_evaluation_failure",
        reason: "local daemon could not reach the cloud hook",
        evaluations: [],
      }),
      want: { block: "local daemon could not reach the cloud hook" },
    },
    {
      name: "refuses the turn when the evaluation fails and fail-open is off",
      args: { failOpen: false },
      respond: async () => {
        throw new Error("guard backend unavailable");
      },
      want: {
        block: expect.stringContaining("stopped as a safety measure") as any,
      },
      wantWarn: ["preflight transform eval failed"],
    },
  ];

  it.each(cases)("$name", async ({ respond, args, want, wantWarn, assert }) => {
    const { client, calls } = makePreflightClient(
      respond ?? (async () => ALLOW),
    );
    const warn = vi.fn();

    const res = await runPreflightTransform(
      makePreflightArgs({ client, logger: { warn }, ...args }),
    );

    expect(res).toEqual(want);
    expect(calls).toHaveLength(1);
    for (const substring of wantWarn ?? []) {
      expect(warn).toHaveBeenCalledWith(expect.stringContaining(substring));
    }
    if (!wantWarn) expect(warn).not.toHaveBeenCalled();
    assert?.(calls);
  });
});
