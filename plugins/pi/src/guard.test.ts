import type {
  HookEvaluateRequest,
  HookEvaluateResponse,
  SigilClient,
} from "@grafana/sigil-sdk-js";
import { describe, expect, it, vi } from "vitest";
import { type GuardArgs, runToolCallGuard } from "./guard.js";

function makeClient(
  evaluateHook: (
    req: HookEvaluateRequest,
    override?: unknown,
  ) => Promise<HookEvaluateResponse>,
): {
  client: SigilClient;
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
  } as unknown as SigilClient;
  return { client, calls };
}

function makeArgs(overrides?: Partial<GuardArgs>): GuardArgs {
  return {
    client: {} as SigilClient,
    agentName: "pi",
    agentVersion: "1.0.0",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
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
    expect(result?.block).toBe(true);
    expect(result?.reason).toContain("blocked rm -rf");
    expect(result?.reason).toContain("A Grafana AI Observability policy");
    expect(result?.reason).toContain('"bash"');
    expect(result?.reason).toContain("Stop and tell the user");
  });

  it("omits the Reason clause when the deny reason is empty", async () => {
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "   ",
      evaluations: [],
    }));

    const result = await runToolCallGuard(makeArgs({ client }));
    expect(result?.block).toBe(true);
    expect(result?.reason).toContain("A Grafana AI Observability policy");
    expect(result?.reason).toContain('"bash"');
    expect(result?.reason).not.toContain("Reason:");
    expect(result?.reason).toContain("Stop and tell the user");
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
    expect(result?.block).toBe(true);
    expect(result?.reason).toContain("could not evaluate");
    expect(result?.reason).toContain("safety measure");
    expect(result?.reason).toContain('"bash"');
    expect(result?.reason).toContain("network down");
    expect(result?.reason).not.toContain(
      "A Grafana AI Observability policy blocked",
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
  });
});
