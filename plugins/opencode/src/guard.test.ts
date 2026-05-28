import { describe, expect, it } from "vitest";
import { denyResult, runToolCallGuard } from "./guard.js";

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

  it("returns a block result when Sigil denies the tool call", async () => {
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

    expect(res).toEqual({ block: true, reason: "blocked by rule" });
  });

  it("denies when the SDK throws (fail-closed mode)", async () => {
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
    expect(res?.reason).toContain("sigil guard evaluation failed");
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

  it("uses the default deny reason when Sigil omits one", () => {
    expect(denyResult(undefined)).toEqual({
      block: true,
      reason: "tool call denied by Sigil guard",
    });
  });
});
