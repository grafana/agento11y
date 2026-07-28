// Tests terminal detection (`finish` is not a completion signal) and token
// accumulation across the provider steps of one assistant message.

import type { AssistantMessage } from "@opencode-ai/sdk";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { createAgento11yClientMock, createTelemetryProvidersMock } = vi.hoisted(
  () => ({
    createAgento11yClientMock: vi.fn(),
    createTelemetryProvidersMock: vi.fn(),
  }),
);

vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));
vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: createTelemetryProvidersMock,
}));

import { _resetHookState, createAgento11yHooks } from "./hooks.js";
import {
  assistantMessage,
  baseConfig,
  type CapturedGeneration,
  emitMessageUpdated,
  emitPartUpdated,
  emitSessionDeleted,
  inFlightAssistantMessage,
  makeAgento11yMock,
  makeOpencodeClient,
  stepFinishPart,
  type TestHooks,
} from "./hooks.testutil.js";

async function setup(
  config = baseConfig(),
  client = makeOpencodeClient(),
): Promise<{
  hooks: TestHooks;
  generations: CapturedGeneration[];
  client: ReturnType<typeof makeOpencodeClient>;
}> {
  const { sigil, generations } = makeAgento11yMock();
  createAgento11yClientMock.mockReturnValue(sigil);
  const hooks = await createAgento11yHooks(config, client);
  if (!hooks) throw new Error("expected hooks");
  return { hooks, generations, client };
}

function usageOf(generation: CapturedGeneration): Record<string, number> {
  return (generation.result as { usage: Record<string, number> }).usage;
}

function costOf(generation: CapturedGeneration): number {
  return (generation.result as { metadata: { cost: number } }).metadata.cost;
}

const completedTime = {
  created: 1_700_000_001_000,
  completed: 1_700_000_003_000,
};

describe("opencode terminal detection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("does not export while only finish is set", async () => {
    const { hooks, generations } = await setup();

    // opencode sets `finish` and publishes message.updated at every
    // step-finish, so four steps means four non-terminal updates.
    for (let step = 0; step < 4; step++) {
      await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 10 }));
      await emitMessageUpdated(hooks, inFlightAssistantMessage("s1", "m1"));
    }

    expect(generations).toHaveLength(0);
  });

  it("exports once on the first time.completed and ignores repeats", async () => {
    const { hooks, generations } = await setup();

    await emitMessageUpdated(hooks, inFlightAssistantMessage("s1", "m1"));
    expect(generations).toHaveLength(0);

    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));

    expect(generations).toHaveLength(1);
  });

  it("does not fetch the message body on step-finish parts", async () => {
    const { hooks, generations, client } = await setup();

    await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 10 }));

    expect(generations).toHaveLength(0);
    expect(client.session.message).not.toHaveBeenCalled();
  });

  it("does not re-export recorded turns on session.idle", async () => {
    const { hooks, generations, client } = await setup();

    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));
    const fetchesAfterRecord = client.session.message.mock.calls.length;
    await hooks.event({
      event: { type: "session.idle", properties: { info: { id: "s1" } } },
    });

    expect(generations).toHaveLength(1);
    // Idle must not walk session history looking for older messages (#315).
    expect(client.session.message.mock.calls.length).toBe(fetchesAfterRecord);
  });
});

describe("opencode token accumulation", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  // `assistantMessage` defaults to tokens {input: 10, output: 5}, which is the
  // last-step value the fallback cases expect.
  const cases: {
    name: string;
    parts: unknown[];
    final: AssistantMessage;
    wantUsage: Record<string, number>;
    wantCost?: number;
    wantError?: string;
  }[] = [
    {
      name: "sums every step of the message",
      parts: [
        stepFinishPart("s1", "m1", {
          input: 100,
          output: 20,
          reasoning: 5,
          cache: { read: 30, write: 10 },
        }),
        stepFinishPart("s1", "m1", {
          input: 120,
          output: 25,
          reasoning: 7,
          cache: { read: 40, write: 0 },
        }),
        stepFinishPart("s1", "m1", {
          input: 90,
          output: 15,
          reasoning: 3,
          cache: { read: 0, write: 4 },
        }),
      ],
      final: assistantMessage("s1", "m1", { cost: 0.06 }),
      wantUsage: {
        inputTokens: 310,
        outputTokens: 60,
        reasoningTokens: 15,
        cacheReadInputTokens: 70,
        cacheWriteInputTokens: 14,
      },
      // Cost is the message aggregate, which upstream sums over the same steps.
      wantCost: 0.06,
    },
    {
      name: "reports a single step unchanged",
      parts: [
        stepFinishPart("s1", "m1", {
          input: 80,
          output: 12,
          reasoning: 3,
          cache: { read: 4, write: 2 },
        }),
      ],
      final: assistantMessage("s1", "m1"),
      // `input` stays cache-adjusted (80, not 80+4+2) and `output` keeps
      // reasoning out (12, not 12+3), matching opencode's `Session.getUsage`.
      wantUsage: {
        inputTokens: 80,
        outputTokens: 12,
        reasoningTokens: 3,
        cacheReadInputTokens: 4,
        cacheWriteInputTokens: 2,
      },
    },
    {
      name: "falls back to the message totals when no step part was seen",
      parts: [],
      final: assistantMessage("s1", "m1"),
      wantUsage: {
        inputTokens: 10,
        outputTokens: 5,
        reasoningTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
    },
    {
      name: "falls back when no step part carried a parseable count",
      parts: [
        { ...stepFinishPart("s1", "m1"), tokens: undefined },
        {
          ...stepFinishPart("s1", "m1"),
          tokens: { input: "12", output: null, cache: "nope" },
        },
      ],
      final: assistantMessage("s1", "m1"),
      // Zeros from a malformed payload must not pass as observed usage. That
      // would export zero tokens next to a non-zero cost.
      wantUsage: {
        inputTokens: 10,
        outputTokens: 5,
        reasoningTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
    },
    {
      name: "skips unparseable fields next to a valid step",
      parts: [
        {
          ...stepFinishPart("s1", "m1"),
          tokens: { input: "12", output: null, cache: "nope" },
        },
        stepFinishPart("s1", "m1", { input: 5 }),
      ],
      wantUsage: {
        inputTokens: 5,
        outputTokens: 0,
        reasoningTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
      final: assistantMessage("s1", "m1"),
    },
    {
      name: "exports the tokens accumulated before an abort",
      parts: [
        stepFinishPart("s1", "m1", { input: 40, output: 8 }),
        stepFinishPart("s1", "m1", { input: 60, output: 7 }),
      ],
      final: assistantMessage("s1", "m1", {
        time: completedTime,
        error: { name: "MessageAbortedError", data: { message: "aborted" } },
      }),
      wantUsage: {
        inputTokens: 100,
        outputTokens: 15,
        reasoningTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
      wantError: "aborted",
    },
    {
      name: "exports the tokens accumulated before a mid-stream error",
      parts: [
        stepFinishPart("s1", "m1", { input: 25, output: 4 }),
        stepFinishPart("s1", "m1", { input: 35, output: 6 }),
      ],
      // A provider error can arrive before `time.completed` is set.
      final: inFlightAssistantMessage("s1", "m1", {
        error: {
          name: "APIError",
          data: { message: "boom", statusCode: 500, isRetryable: false },
        },
      }),
      wantUsage: {
        inputTokens: 60,
        outputTokens: 10,
        reasoningTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
      wantError: "api_error: 500",
    },
  ];

  for (const tc of cases) {
    it(tc.name, async () => {
      const { hooks, generations } = await setup();

      for (const part of tc.parts) await emitPartUpdated(hooks, part);
      await emitMessageUpdated(hooks, tc.final);

      expect(generations).toHaveLength(1);
      expect(usageOf(generations[0]!)).toEqual(tc.wantUsage);
      if (tc.wantCost !== undefined) {
        expect(costOf(generations[0]!)).toBe(tc.wantCost);
      }
      if (tc.wantError !== undefined) {
        expect((generations[0]!.callError as Error).message).toBe(tc.wantError);
      }
    });
  }

  const clearCases: {
    name: string;
    clear: (hooks: TestHooks) => Promise<void>;
  }[] = [
    {
      name: "drops partial totals when the session is deleted",
      clear: (hooks) => emitSessionDeleted(hooks, "s1"),
    },
    {
      name: "drops partial totals on hook state reset",
      clear: async () => {
        _resetHookState();
      },
    },
  ];

  for (const tc of clearCases) {
    it(tc.name, async () => {
      const { hooks, generations } = await setup();

      await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 100 }));
      await tc.clear(hooks);

      await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 7 }));
      await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));

      expect(generations).toHaveLength(1);
      expect(usageOf(generations[0]!).inputTokens).toBe(7);
    });
  }

  it("keeps concurrent messages and sessions separate", async () => {
    const { hooks, generations } = await setup();

    await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 11 }));
    await emitPartUpdated(hooks, stepFinishPart("s1", "m2", { input: 22 }));
    await emitPartUpdated(hooks, stepFinishPart("s2", "m1", { input: 33 }));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m2"));
    await emitMessageUpdated(hooks, assistantMessage("s2", "m1"));

    expect(generations.map((gen) => usageOf(gen).inputTokens)).toEqual([
      11, 22, 33,
    ]);
  });

  it("does not reuse a recorded message's totals for the next turn", async () => {
    const { hooks, generations } = await setup();

    await emitPartUpdated(hooks, stepFinishPart("s1", "m1", { input: 40 }));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));
    await emitPartUpdated(hooks, stepFinishPart("s1", "m2", { input: 7 }));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m2"));

    expect(usageOf(generations[1]!).inputTokens).toBe(7);
  });

  it("accumulates in metadata_only without fetching the message body", async () => {
    const { hooks, generations, client } = await setup(
      baseConfig({ contentCapture: "metadata_only" }),
    );

    await emitPartUpdated(
      hooks,
      stepFinishPart("s1", "m1", { input: 10, output: 2 }),
    );
    await emitPartUpdated(
      hooks,
      stepFinishPart("s1", "m1", { input: 20, output: 3 }),
    );
    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));

    expect(usageOf(generations[0]!).inputTokens).toBe(30);
    expect(usageOf(generations[0]!).outputTokens).toBe(5);
    expect(client.session.message).not.toHaveBeenCalled();
  });

  // The exported payload does not say which source the tokens came from. The
  // log line is the only signal that a host stopped emitting step-finish parts
  // and put last-step tokens back next to all-steps cost.
  it("debug-logs the fallback and stays quiet when steps were observed", async () => {
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const fellBack = () =>
      logged.mock.calls.some(
        ([msg]) =>
          typeof msg === "string" && msg.includes("no step-finish tokens"),
      );
    const { hooks } = await setup(baseConfig({ debug: true }));

    await emitMessageUpdated(hooks, assistantMessage("s1", "m1"));
    expect(fellBack()).toBe(true);

    logged.mockClear();
    await emitPartUpdated(hooks, stepFinishPart("s1", "m2", { input: 5 }));
    await emitMessageUpdated(hooks, assistantMessage("s1", "m2"));
    expect(fellBack()).toBe(false);

    logged.mockRestore();
  });
});
