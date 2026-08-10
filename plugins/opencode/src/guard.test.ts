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
      toolCallId: "c1",
      toolName: "bash",
      input: { command: "ls" },
      failOpen: true,
    });

    expect(res).toBeUndefined();
    expect(calls).toHaveLength(1);
    expect((calls[0] as any).phase).toBe("postflight");
    expect((calls[0] as any).input.output[0].parts[0].toolCall.inputJSON).toBe(
      JSON.stringify({ command: "ls" }),
    );
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

  it("ignores a transform whose tool_call id does not match", async () => {
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
