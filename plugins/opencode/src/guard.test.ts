import { describe, expect, it } from "vitest";
import type { GuardResult } from "./guard.js";
import { runToolCallGuard } from "./guard.js";

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
  it("returns undefined when Sigil allows the tool call", async () => {
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

  it("returns a wrapped policy-deny result when Sigil denies the tool call", async () => {
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

  it("omits the Reason clause when Sigil denies without a reason", async () => {
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
