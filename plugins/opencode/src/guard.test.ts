import { describe, expect, it } from "vitest";
import { runToolCallGuard } from "./guard.js";

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

    expect(res?.block).toBe(true);
    expect(res?.reason).toContain("A Grafana AI Observability policy");
    expect(res?.reason).toContain('"bash"');
    expect(res?.reason).toContain("Reason: blocked by rule");
    expect(res?.reason).toContain("Stop and tell the user");
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

    expect(res?.block).toBe(true);
    expect(res?.reason).toContain("A Grafana AI Observability policy");
    expect(res?.reason).toContain('"bash"');
    expect(res?.reason).not.toContain("Reason:");
    expect(res?.reason).toContain("Stop and tell the user");
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

    expect(res?.block).toBe(true);
    expect(res?.reason).toContain("could not evaluate");
    expect(res?.reason).toContain("safety measure");
    expect(res?.reason).toContain('"bash"');
    expect(res?.reason).toContain("network down");
    expect(res?.reason).not.toContain(
      "A Grafana AI Observability policy blocked",
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
});
