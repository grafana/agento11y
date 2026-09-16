import type {
  Agento11yClient,
  HookEvaluateRequest,
  HookEvaluateResponse,
  HookInput,
  Message,
} from "@grafana/agento11y";
import { describe, expect, it, vi } from "vitest";
import {
  type GuardArgs,
  type GuardBlockResult,
  type GuardResult,
  type GuardTransformResult,
  type PreflightTransformArgs,
  runPreflightTransform,
  runToolCallGuard,
} from "./guard.js";

function expectBlock(result: GuardResult): asserts result is GuardBlockResult {
  if (!result || !("block" in result)) {
    throw new Error(`expected a block result, got ${JSON.stringify(result)}`);
  }
}

function expectTransform(
  result: GuardResult,
): asserts result is GuardTransformResult {
  if (!result || !("transform" in result)) {
    throw new Error(
      `expected a transform result, got ${JSON.stringify(result)}`,
    );
  }
}

function makeClient(
  evaluateHook: (
    req: HookEvaluateRequest,
    override?: unknown,
  ) => Promise<HookEvaluateResponse>,
): {
  client: Agento11yClient;
  calls: Array<{ req: HookEvaluateRequest; override: unknown }>;
} {
  const calls: Array<{ req: HookEvaluateRequest; override: unknown }> = [];
  const client = {
    evaluateHook: vi.fn(
      async (req: HookEvaluateRequest, override?: unknown) => {
        calls.push({ req, override });
        return evaluateHook(req, override);
      },
    ),
  } as unknown as Agento11yClient;
  return { client, calls };
}

function makeArgs(overrides?: Partial<GuardArgs>): GuardArgs {
  return {
    client: {} as Agento11yClient,
    agentName: "pi",
    agentVersion: "1.0.0",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
    conversationId: "pi-session-1",
    toolCallId: "c1",
    toolName: "bash",
    input: { command: "ls" },
    failOpen: true,
    ...overrides,
  };
}

describe("runToolCallGuard", () => {
  it("returns undefined when the server allows", async () => {
    const { client } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
    }));

    const result = await runToolCallGuard(makeArgs({ client }));
    expect(result).toBeUndefined();
  });

  it("returns block with wrapped policy-deny reason when the server denies", async () => {
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "blocked rm -rf",
      evaluations: [],
    }));

    const result = await runToolCallGuard(makeArgs({ client }));
    expectBlock(result);
    expect(result.block).toBe(true);
    expect(result.reason).toContain("blocked rm -rf");
    expect(result.reason).toContain("A Grafana Agent Observability policy");
    expect(result.reason).toContain('"bash"');
    expect(result.reason).toContain("Stop and tell the user");
  });

  it("returns the raw reason for an evaluation-failure deny", async () => {
    // The local daemon answers with this rule id when its own chained Cloud
    // hook call failed under GUARDS_FAIL_OPEN=false. No policy ran, so the
    // message must not claim one blocked the call.
    const { client } = makeClient(async () => ({
      action: "deny",
      ruleId: "__agento11y_guard_evaluation_failure",
      reason:
        'agento11y could not evaluate the Grafana Agent Observability guard for the "bash" tool call, so it was blocked as a safety measure. Details: connection refused',
      evaluations: [],
    }));

    const result = await runToolCallGuard(makeArgs({ client }));
    expectBlock(result);
    expect(result.reason).toContain("could not evaluate");
    expect(result.reason).toContain("connection refused");
    expect(result.reason).not.toContain("A Grafana Agent Observability policy");
  });

  it("omits the Reason clause when the deny reason is empty", async () => {
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "   ",
      evaluations: [],
    }));

    const result = await runToolCallGuard(makeArgs({ client }));
    expectBlock(result);
    expect(result.block).toBe(true);
    expect(result.reason).toContain("A Grafana Agent Observability policy");
    expect(result.reason).toContain('"bash"');
    expect(result.reason).not.toContain("Reason:");
    expect(result.reason).toContain("Stop and tell the user");
  });

  it("returns undefined and logs a warning on transport errors when failOpen", async () => {
    const { client } = makeClient(async () => {
      throw new Error("network down");
    });
    const warn = vi.fn();

    const result = await runToolCallGuard(
      makeArgs({ client, logger: { warn } }),
    );
    expect(result).toBeUndefined();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("guard eval failed"),
    );
  });

  it("fails open when tool input cannot be serialized", async () => {
    const { client, calls } = makeClient(async () => {
      throw new Error("should not call evaluateHook");
    });
    const warn = vi.fn();

    const result = await runToolCallGuard(
      makeArgs({ client, input: { value: 1n }, logger: { warn } }),
    );

    expect(result).toBeUndefined();
    expect(calls).toHaveLength(0);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("guard eval failed"),
    );
  });

  it("returns block with fail-closed message on transport errors when failOpen=false", async () => {
    const { client } = makeClient(async () => {
      throw new Error("network down");
    });
    const warn = vi.fn();

    const result = await runToolCallGuard(
      makeArgs({ client, failOpen: false, logger: { warn } }),
    );
    expectBlock(result);
    expect(result.block).toBe(true);
    expect(result.reason).toContain("could not evaluate");
    expect(result.reason).toContain("safety measure");
    expect(result.reason).toContain('"bash"');
    expect(result.reason).toContain("network down");
    expect(result.reason).not.toContain(
      "A Grafana Agent Observability policy blocked",
    );
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("guard eval failed"),
    );
  });

  it("builds a postflight request with the expected shape", async () => {
    const { client, calls } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
    }));

    await runToolCallGuard(
      makeArgs({
        client,
        toolCallId: "c1",
        toolName: "bash",
        input: { command: "ls" },
      }),
    );

    expect(calls).toHaveLength(1);
    const { req, override } = calls[0]!;
    expect(req.phase).toBe("postflight");
    expect(req.context.agentName).toBe("pi");
    expect(req.context.agentVersion).toBe("1.0.0");
    expect(req.context.conversationId).toBe("pi-session-1");
    expect(req.context.model).toEqual({
      provider: "anthropic",
      name: "claude-sonnet-4",
    });
    expect(req.input.output).toHaveLength(1);
    const msg = req.input.output![0]!;
    expect(msg.role).toBe("assistant");
    expect(msg.parts).toHaveLength(1);
    const part = msg.parts![0]!;
    expect(part.type).toBe("tool_call");
    expect(part.type === "tool_call" && part.toolCall).toEqual({
      id: "c1",
      name: "bash",
      inputJSON: '{"command":"ls"}',
    });
    expect(override).toEqual({ enabled: true });

    await runToolCallGuard(makeArgs({ client, conversationId: undefined }));
    expect(calls[1]!.req.context).not.toHaveProperty("conversationId");
  });

  it("returns a transform result when the server emits redacted tool_call args", async () => {
    const { client } = makeClient(async () => ({
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
                  inputJSON: '{"command":"echo [REDACTED_KEY]"}',
                },
              },
            ],
          },
        ],
      },
    }));

    const warn = vi.fn();
    const result = await runToolCallGuard(
      makeArgs({
        client,
        toolCallId: "c1",
        toolName: "bash",
        input: { command: "echo sk-real-secret" },
        logger: { warn },
      }),
    );
    expectTransform(result);
    expect(result.transform).toEqual({
      command: "echo [REDACTED_KEY]",
    });
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("transform for c1 applied"),
    );
  });

  it("ignores a transform aimed at a different toolCallId even when it is the only rewritten call", async () => {
    const { client } = makeClient(async () => ({
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
                  id: "different-call-id",
                  name: "bash",
                  inputJSON: '{"command":"echo X"}',
                },
              },
            ],
          },
        ],
      },
    }));

    const result = await runToolCallGuard(
      makeArgs({
        client,
        toolCallId: "c1",
        toolName: "bash",
      }),
    );
    expect(result).toBeUndefined();
  });

  it("rejects a conflicting ID even when the echoed name has surrounding whitespace", async () => {
    const { client } = makeClient(async () => ({
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
                  inputJSON: '{"path":"a"}',
                },
              },
              {
                type: "tool_call",
                toolCall: {
                  id: "different-call-id",
                  name: " Bash ",
                  inputJSON: '{"command":"echo X"}',
                },
              },
            ],
          },
        ],
      },
    }));

    const result = await runToolCallGuard(
      makeArgs({
        client,
        toolCallId: "c1",
        toolName: "bash",
      }),
    );
    expect(result).toBeUndefined();
  });

  it("logs and drops a transform whose inputJSON cannot be parsed", async () => {
    const { client } = makeClient(async () => ({
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
                  inputJSON: "not valid json",
                },
              },
            ],
          },
        ],
      },
    }));

    const warn = vi.fn();
    const result = await runToolCallGuard(
      makeArgs({
        client,
        toolCallId: "c1",
        toolName: "bash",
        logger: { warn },
      }),
    );
    expect(result).toBeUndefined();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("invalid JSON arguments"),
    );
  });

  it("prefers deny over transform when both are present", async () => {
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "blocked",
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
                  inputJSON: '{"command":"echo redacted"}',
                },
              },
            ],
          },
        ],
      },
    }));

    const result = await runToolCallGuard(
      makeArgs({ client, toolCallId: "c1", toolName: "bash" }),
    );
    expectBlock(result);
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
      const { client } = makeClient(async () => ({
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
      const result = await runToolCallGuard(
        makeArgs({ client, toolCallId, toolName }),
      );
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
      const { client } = makeClient(async () => ({
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
      const result = await runToolCallGuard(
        makeArgs({
          client,
          toolCallId,
          toolName,
          logger: { warn },
        }),
      );
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

function makePreflightArgs(
  overrides?: Partial<PreflightTransformArgs>,
): PreflightTransformArgs {
  return {
    client: {} as Agento11yClient,
    agentName: "pi",
    agentVersion: "1.0.0",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
    conversationId: "pi-session-1",
    messages: [{ role: "user", parts: [{ type: "text", text: "hi" }] }],
    ...overrides,
  };
}

describe("runPreflightTransform", () => {
  it("sends a preflight request with phases override", async () => {
    const { client, calls } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
    }));

    await runPreflightTransform(makePreflightArgs({ client }));

    expect(calls).toHaveLength(1);
    const { req, override } = calls[0]!;
    expect(req.phase).toBe("preflight");
    expect(req.context).toEqual({
      agentName: "pi",
      agentVersion: "1.0.0",
      conversationId: "pi-session-1",
      model: { provider: "anthropic", name: "claude-sonnet-4" },
    });
    expect((req.input as HookInput).messages).toEqual([
      { role: "user", parts: [{ type: "text", text: "hi" }] },
    ]);
    expect(override).toEqual({ enabled: true, phases: ["preflight"] });

    await runPreflightTransform(
      makePreflightArgs({ client, conversationId: undefined }),
    );
    expect(calls[1]!.req.context).not.toHaveProperty("conversationId");
  });

  it("returns the redacted messages from transformedInput", async () => {
    const redacted: Message[] = [
      { role: "user", parts: [{ type: "text", text: "hi [REDACTED]" }] },
    ];
    const { client } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
      transformedInput: { messages: redacted },
    }));

    const result = await runPreflightTransform(makePreflightArgs({ client }));
    expect(result).toEqual({ messages: redacted });
  });

  it("returns undefined when the server does not emit transformedInput", async () => {
    const { client } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
    }));

    const result = await runPreflightTransform(makePreflightArgs({ client }));
    expect(result).toBeUndefined();
  });

  it("returns undefined when the server returns an empty messages list", async () => {
    const { client } = makeClient(async () => ({
      action: "allow",
      evaluations: [],
      transformedInput: { messages: [] },
    }));

    const result = await runPreflightTransform(makePreflightArgs({ client }));
    expect(result).toBeUndefined();
  });

  it("fails open on transport errors, logging a warning", async () => {
    const { client } = makeClient(async () => {
      throw new Error("network down");
    });
    const warn = vi.fn();

    const result = await runPreflightTransform(
      makePreflightArgs({ client, logger: { warn } }),
    );
    expect(result).toBeUndefined();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("preflight transform eval failed"),
    );
  });

  it("surfaces a preflight deny without a transform as no-op (cannot block via context)", async () => {
    // The pi `context` event has no `block` field, so a preflight deny
    // verdict cannot be enforced here. Without transformedInput there is
    // nothing to apply, so we surface no transform and let the original
    // messages flow through.
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "preflight deny",
      evaluations: [],
    }));

    const result = await runPreflightTransform(makePreflightArgs({ client }));
    expect(result).toBeUndefined();
  });
});
