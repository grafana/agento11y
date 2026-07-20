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
    expect(result.reason).toContain("A Grafana AI Observability policy");
    expect(result.reason).toContain('"bash"');
    expect(result.reason).toContain("Stop and tell the user");
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
    expect(result.reason).toContain("A Grafana AI Observability policy");
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

  it("ignores transformed_input that targets a different toolCallId", async () => {
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

    // A transform was present but none of its parts matched this call, so the
    // original input is left unchanged. Log it so a no-op transform is
    // distinguishable from a plain allow in the debug log.
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
      expect.stringContaining("no part matched c1"),
    );
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

function makePreflightArgs(
  overrides?: Partial<PreflightTransformArgs>,
): PreflightTransformArgs {
  return {
    client: {} as Agento11yClient,
    agentName: "pi",
    agentVersion: "1.0.0",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
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
      model: { provider: "anthropic", name: "claude-sonnet-4" },
    });
    expect((req.input as HookInput).messages).toEqual([
      { role: "user", parts: [{ type: "text", text: "hi" }] },
    ]);
    expect(override).toEqual({ enabled: true, phases: ["preflight"] });
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
    // verdict cannot be enforced at this seam. Without transformedInput
    // there is nothing to apply, so we surface no transform and let the
    // original messages flow through.
    const { client } = makeClient(async () => ({
      action: "deny",
      reason: "preflight deny",
      evaluations: [],
    }));

    const result = await runPreflightTransform(makePreflightArgs({ client }));
    expect(result).toBeUndefined();
  });
});
