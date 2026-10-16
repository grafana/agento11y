import type { ContentCaptureMode, Message } from "@grafana/agento11y";
import { describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import { stableOpencodeCompactionGenerationId } from "./lineage.js";
import { createRedactor } from "./redaction.js";
import {
  mapNativeCompactionGeneration,
  mapNativeGeneration,
  mapNativeMessages,
  mapNativeUsage,
  type NativeBlock,
  type NativeCompactionStart,
  type NativeCompactionTerminal,
  type NativeEventOf,
  type NativeTokens,
  nativeOutput,
  nativeToolRecords,
} from "./v2-mappers.js";
import { nativeFixture } from "./v2-testdata.js";

const redactor = createRedactor();
const tokens: NativeTokens = {
  input: 10,
  output: 20,
  reasoning: 3,
  cache: { read: 4, write: 5 },
};
const blocks: NativeBlock[] = [
  { type: "text", ordinal: 0, text: "hello", ended: true },
  { type: "reasoning", ordinal: 0, text: "thinking", ended: true },
  {
    type: "tool",
    id: "call-1",
    name: "bash",
    input: { command: "echo hello" },
    calledAt: 10,
    completedAt: 20,
    output: "hello",
  },
];

describe("native compaction generation mapping", () => {
  const config: Agento11yOpencodeConfig = {
    endpoint: "http://127.0.0.1:1",
    auth: { mode: "none" },
    agentName: "custom-opencode",
    agentVersion: "not-mapped-by-compaction",
    contentCapture: "full",
    redactInputMessages: true,
    debug: false,
  };
  const start: NativeCompactionStart = {
    startedAt: 100,
    input: [
      { role: "user", parts: [{ type: "text", text: "observed prompt" }] },
      ...nativeOutput(blocks),
    ],
    parent: "frozen-parent-for-capture",
  };
  function ended(
    data: Partial<NativeEventOf<"session.compaction.ended">["data"]> = {},
  ): NativeEventOf<"session.compaction.ended"> {
    return {
      id: "compaction-terminal-1",
      type: "session.compaction.ended",
      created: 200,
      durable: { aggregateID: "s", seq: 2, version: 1 },
      location: nativeFixture.location,
      data: {
        sessionID: "s",
        reason: "auto",
        model: nativeFixture.model,
        text: "  observed summary\n",
        recent: "RECENT_CONTEXT_NOT_OUTPUT",
        ...data,
      },
    };
  }
  function failed(
    data: Partial<NativeEventOf<"session.compaction.failed">["data"]> = {},
  ): NativeEventOf<"session.compaction.failed"> {
    return {
      id: "compaction-failure-1",
      type: "session.compaction.failed",
      created: 200,
      durable: { aggregateID: "s", seq: 2, version: 1 },
      location: nativeFixture.location,
      data: {
        sessionID: "s",
        reason: "manual",
        error: { type: "compaction.failed", message: nativeFixture.secret },
        ...data,
      },
    };
  }

  it("returns only pure SYNC fields; capture adds identity/lineage/version/directory tags", () => {
    const terminal = ended();
    const before = JSON.stringify({ terminal, start });
    const mapped = mapNativeCompactionGeneration(
      terminal,
      start,
      config,
      redactor,
    );
    expect(mapped.seed).toEqual({
      id: stableOpencodeCompactionGenerationId("s", terminal.id),
      mode: "SYNC",
      operationName: "generateText",
      agentName: "custom-opencode",
      model: {
        provider: nativeFixture.model.providerID,
        name: nativeFixture.model.id,
      },
      startedAt: new Date(100),
      contentCapture: "full",
      tags: { "opencode.call_kind": "compaction" },
    });
    expect(mapped.result).toMatchObject({
      responseModel: nativeFixture.model.id,
      stopReason: "end_turn",
      completedAt: new Date(200),
      metadata: {
        "opencode.usage_scope": "logical_compaction",
        "opencode.compaction.reason": "auto",
        "opencode.settlement": "compaction.ended",
        "opencode.model_source": "event",
        "opencode.input_source": "first_observed_prefix",
        "opencode.start_time_approximate": false,
        "opencode.completion_time_approximate": true,
      },
    });
    expect(mapped).not.toHaveProperty("error");
    expect(JSON.stringify(mapped)).not.toContain("RECENT_CONTEXT_NOT_OUTPUT");
    expect(JSON.stringify({ terminal, start })).toBe(before);
    expect(
      mapNativeCompactionGeneration(terminal, start, config, redactor),
    ).toEqual(mapped);
  });

  it.each([
    ["full", true, true],
    ["no_tool_content", true, false],
    ["metadata_only", false, false],
    ["full_with_metadata_spans", true, true],
  ] as const)("applies capture mode %s to compaction input and summary", (contentCapture, bodies, tools) => {
    const mapped = mapNativeCompactionGeneration(
      ended({ tokens, cost: 0.001 }),
      start,
      { ...config, contentCapture },
      redactor,
    );
    expect(mapped.seed.contentCapture).toBe(contentCapture);
    expect(mapped.seed.tags).toEqual({ "opencode.call_kind": "compaction" });
    expect(mapped.result.input).toEqual(
      bodies ? mapNativeMessages(start.input, redactor, contentCapture) : [],
    );
    expect(JSON.stringify(mapped.result.input).includes("echo hello")).toBe(
      tools,
    );
    expect(mapped.result.output).toEqual(
      bodies
        ? [
            {
              role: "assistant",
              parts: [{ type: "text", text: "  observed summary\n" }],
            },
          ]
        : [],
    );
    expect(mapped.result.usage).toEqual(mapNativeUsage(tokens));
    expect(mapped.result.metadata).toMatchObject({
      cost_usd: 0.001,
      "opencode.usage_scope": "logical_compaction",
      "opencode.compaction.reason": "auto",
      "opencode.settlement": "compaction.ended",
      "opencode.model_source": "event",
      "opencode.input_source": "first_observed_prefix",
      "opencode.start_time_approximate": false,
      "opencode.completion_time_approximate": true,
    });
  });

  const zeroTokens: NativeTokens = {
    input: 0,
    output: 0,
    reasoning: 0,
    cache: { read: 0, write: 0 },
  };
  for (const terminal of [ended, failed]) {
    it.each([
      { name: "present", tokens, cost: 0.001 },
      { name: "explicit zero", tokens: zeroTokens, cost: 0 },
      { name: "absent", tokens: undefined, cost: undefined },
      { name: "tokens only", tokens, cost: undefined },
      { name: "cost only", tokens: undefined, cost: 0 },
    ])(`preserves $name usage and cost for ${terminal().type}`, ({
      tokens: usage,
      cost,
    }) => {
      const mapped = mapNativeCompactionGeneration(
        terminal({ tokens: usage, cost }),
        start,
        config,
        redactor,
      );
      expect(mapped.result.usage).toEqual(mapNativeUsage(usage));
      expect(Object.hasOwn(mapped.result, "usage")).toBe(usage !== undefined);
      expect(Object.hasOwn(mapped.result.metadata ?? {}, "cost_usd")).toBe(
        cost !== undefined,
      );
      if (cost !== undefined)
        expect(mapped.result.metadata?.cost_usd).toBe(cost);
    });
  }

  it.each([
    [true, true],
    [true, false],
    [false, true],
    [false, false],
  ])("maps observed start=%s and model=%s without fallbacks", (hasStart, hasModel) => {
    const mapped = mapNativeCompactionGeneration(
      ended({ model: hasModel ? nativeFixture.model : undefined }),
      hasStart ? start : undefined,
      config,
      redactor,
    );
    expect(mapped.seed.model).toEqual(
      hasModel
        ? {
            provider: nativeFixture.model.providerID,
            name: nativeFixture.model.id,
          }
        : { provider: "unknown", name: "unknown" },
    );
    expect(mapped.result.responseModel).toBe(
      hasModel ? nativeFixture.model.id : "unknown",
    );
    expect(mapped.seed.id).toBe(
      stableOpencodeCompactionGenerationId("s", "compaction-terminal-1"),
    );
    expect(mapped.seed.startedAt).toEqual(new Date(hasStart ? 100 : 200));
    expect(mapped.result.completedAt).toEqual(new Date(200));
    if (!hasStart) {
      expect(mapped.result.input).toEqual([]);
      expect(mapped.seed).not.toHaveProperty("parentGenerationIds");
    }
    expect(mapped.result.metadata).toMatchObject({
      "opencode.model_source": hasModel ? "event" : "unavailable",
      "opencode.input_source": hasStart
        ? "first_observed_prefix"
        : "unavailable",
      "opencode.start_time_approximate": !hasStart,
      "opencode.completion_time_approximate": true,
    });
  });

  it.each([
    true,
    false,
  ])("exports only the failure error type (observed start=%s)", (hasStart) => {
    const mapped = mapNativeCompactionGeneration(
      failed({ tokens, cost: 0.001 }),
      hasStart ? start : undefined,
      config,
      redactor,
    );
    expect(mapped.error).toBe("compaction.failed");
    expect(mapped.seed.model).toEqual({ provider: "unknown", name: "unknown" });
    expect(mapped.result.output).toEqual([]);
    expect(mapped.result.stopReason).toBe("error");
    expect(mapped.result.usage).toEqual(mapNativeUsage(tokens));
    expect(mapped.result.metadata).toMatchObject({
      cost_usd: 0.001,
      "opencode.compaction.reason": "manual",
      "opencode.settlement": "compaction.failed",
      "opencode.model_source": "unavailable",
    });
    expect(JSON.stringify(mapped)).not.toContain(nativeFixture.secret);
    expect(JSON.stringify(mapped)).not.toContain("message");
  });

  it.each([
    ["", ""],
    [" \n\t", "RECENT_CONTEXT_NOT_OUTPUT"],
    ["summary", "RECENT_CONTEXT_NOT_OUTPUT"],
  ])("excludes recent and opaque native fields with summary=%j, recent=%j", (text, recent) => {
    const terminal = ended({ text, recent, tokens });
    Object.assign(terminal.data, {
      providerState: { secret: "OPAQUE_PROVIDER_STATE" },
      providerContext: { secret: "OPAQUE_PROVIDER_CONTEXT" },
    });
    const mapped = mapNativeCompactionGeneration(
      terminal,
      undefined,
      config,
      redactor,
    );
    expect(mapped.result.output).toEqual(
      text.trim()
        ? [{ role: "assistant", parts: [{ type: "text", text }] }]
        : [],
    );
    expect(mapped.result.usage).toEqual(mapNativeUsage(tokens));
    expect(JSON.stringify(mapped)).not.toMatch(
      /OPAQUE_PROVIDER|RECENT_CONTEXT_NOT_OUTPUT|providerState|providerContext/,
    );
  });

  it.each([
    true,
    false,
  ])("always fully redacts the summary (redactInputMessages=%s)", (redactInputMessages) => {
    const text = `Summary ${nativeFixture.secret}; DATABASE_PASSWORD=${nativeFixture.envSecret}`;
    const fullRedactor = {
      redact: vi.fn(redactor.redact),
      redactLightweight: vi.fn(redactor.redactLightweight),
    };
    const mapped = mapNativeCompactionGeneration(
      ended({ text }),
      {
        startedAt: 100,
        input: [
          {
            role: "user",
            parts: [{ type: "text", text: nativeFixture.secret }],
          },
        ],
      },
      { ...config, redactInputMessages },
      fullRedactor,
    );
    expect(mapped.result.output).toEqual([
      {
        role: "assistant",
        parts: [{ type: "text", text: redactor.redact(text) }],
      },
    ]);
    expect(fullRedactor.redact).toHaveBeenCalledWith(text);
    expect(fullRedactor.redactLightweight).not.toHaveBeenCalled();
    expect(JSON.stringify(mapped.result.output)).not.toContain(
      nativeFixture.secret,
    );
    expect(JSON.stringify(mapped.result.output)).not.toContain(
      nativeFixture.envSecret,
    );
    expect(
      JSON.stringify(mapped.result.input).includes(nativeFixture.secret),
    ).toBe(!redactInputMessages);
  });

  it.each([
    "",
    "named-agent",
  ])("uses the native base identity for prefix=%j", (agentName) => {
    const terminal: NativeCompactionTerminal = ended();
    expect(
      mapNativeCompactionGeneration(
        terminal,
        undefined,
        { ...config, agentName },
        redactor,
      ).seed.agentName,
    ).toBe(agentName || "opencode");
  });
});

describe("native Promise payload mapping", () => {
  it.each([
    {
      name: "cache and reasoning subsets",
      value: tokens,
      input: 19,
      output: 23,
      total: 42,
    },
    {
      name: "observed zero usage",
      value: {
        input: 0,
        output: 0,
        reasoning: 0,
        cache: { read: 0, write: 0 },
      },
      input: 0,
      output: 0,
      total: 0,
    },
  ])("normalizes $name without double counting", ({
    value,
    input,
    output,
    total,
  }) => {
    expect(mapNativeUsage(value)).toEqual({
      inputTokens: input,
      outputTokens: output,
      totalTokens: total,
      inputSemantics: "inclusive",
      reasoningTokens: value.reasoning,
      cacheReadInputTokens: value.cache.read,
      cacheWriteInputTokens: value.cache.write,
    });
  });

  it("leaves absent usage absent, even when output was observed", () => {
    expect(mapNativeUsage()).toBeUndefined();
    expect(
      mapNativeGeneration(
        {
          model: { id: "m", providerID: "p" },
          input: [],
          blocks,
          terminalAt: 100,
          settlement: "shutdown",
        },
        redactor,
        "full",
      ).usage,
    ).toBeUndefined();
  });

  it.each([
    ["full", true, true],
    ["no_tool_content", true, false],
    ["metadata_only", false, false],
    ["full_with_metadata_spans", true, true],
  ] as const)("applies %s to messages AND tool content", (capture, bodies, tools) => {
    const mapped = mapNativeGeneration(
      {
        model: { id: "model", providerID: "provider" },
        input: [{ role: "user", parts: [{ type: "text", text: "prompt" }] }],
        blocks,
        tokens,
        cost: 0.001,
        terminalAt: 200,
        streamedAt: 100,
        settlement: "step.ended",
      },
      redactor,
      capture,
    );
    expect(mapped.responseModel).toBe("model");
    expect(mapped.input).toHaveLength(bodies ? 1 : 0);
    expect(
      mapped.output?.filter((message) =>
        message.parts?.some(
          (part) => part.type === "text" || part.type === "thinking",
        ),
      ),
    ).toHaveLength(bodies ? 2 : 0);
    const serialized = JSON.stringify(mapped.output);
    expect(serialized.includes("echo hello")).toBe(tools);
    expect(serialized).toContain("call-1");
    expect(mapped.usage).toEqual(mapNativeUsage(tokens));
    expect(mapped.completedAt).toEqual(new Date(100));
    expect(mapped.metadata).toMatchObject({
      cost_usd: 0.001,
      "opencode.completion_time_approximate": false,
    });
  });

  it.each([
    {
      name: "small payloads",
      input: '{"path":"file"}',
      output: "result",
      wantInput: '{"path":"file"}',
      wantOutput: "result",
    },
    {
      name: "exact byte limits",
      input: JSON.stringify("x".repeat(4094)),
      output: "x".repeat(1 << 20),
      wantInput: JSON.stringify("x".repeat(4094)),
      wantOutput: "x".repeat(1 << 20),
    },
    {
      name: "one byte over the limits",
      input: JSON.stringify("x".repeat(4095)),
      output: "x".repeat((1 << 20) + 1),
      wantInput: JSON.stringify(`"${"x".repeat(4095)} [truncated]`),
      wantOutput: `${"x".repeat(1 << 20)} [truncated]`,
    },
    {
      name: "two-byte characters across the cut",
      input: JSON.stringify("é".repeat(3000)),
      output: `x${"é".repeat(1 << 19)}`,
      wantInput: JSON.stringify(`"${"é".repeat(2047)} [truncated]`),
      wantOutput: `x${"é".repeat((1 << 19) - 1)} [truncated]`,
    },
    {
      name: "four-byte characters across the cut",
      input: JSON.stringify("🙂".repeat(1100)),
      output: `xxx${"🙂".repeat(1 << 18)}`,
      wantInput: JSON.stringify(`"${"🙂".repeat(1023)} [truncated]`),
      wantOutput: `xxx${"🙂".repeat((1 << 18) - 1)} [truncated]`,
    },
    {
      name: "JSON unicode escapes below the raw byte limit",
      input: "{}",
      output: "\0".repeat(1 << 20),
      wantInput: "{}",
      wantOutput: `${"\0".repeat(Math.floor((1 << 20) / 6))} [truncated]`,
    },
    {
      name: "JSON short escapes",
      input: "{}",
      output: "\n".repeat(1 << 20),
      wantInput: "{}",
      wantOutput: `${"\n".repeat(1 << 19)} [truncated]`,
    },
    {
      name: "JSON quotes",
      input: "{}",
      output: '"'.repeat(1 << 20),
      wantInput: "{}",
      wantOutput: `${'"'.repeat(1 << 19)} [truncated]`,
    },
    {
      name: "JSON backslashes",
      input: "{}",
      output: "\\".repeat(1 << 20),
      wantInput: "{}",
      wantOutput: `${"\\".repeat(1 << 19)} [truncated]`,
    },
    {
      name: "unpaired surrogates",
      input: "{}",
      output: "\ud800".repeat(1 << 20),
      wantInput: "{}",
      wantOutput: `${"\ud800".repeat(Math.floor((1 << 20) / 6))} [truncated]`,
    },
  ])("caps tool content in output and retained input: $name", ({
    input,
    output,
    wantInput,
    wantOutput,
  }) => {
    const tool: NativeBlock = {
      type: "tool",
      id: "c",
      name: "read",
      rawInput: input,
      completedAt: 20,
      output,
    };
    const mapped = mapNativeGeneration(
      {
        model: { id: "m", providerID: "p" },
        input: nativeOutput([tool]),
        blocks: [tool],
        terminalAt: 30,
        settlement: "step.ended",
      },
      redactor,
      "full",
    );
    const expected = [
      {
        role: "assistant",
        parts: [
          {
            type: "tool_call",
            toolCall: { id: "c", name: "read", inputJSON: wantInput },
          },
        ],
      },
      {
        role: "tool",
        parts: [
          {
            type: "tool_result",
            toolResult: { toolCallId: "c", name: "read", content: wantOutput },
          },
        ],
      },
    ];
    expect(mapped.output).toEqual(expected);
    expect(mapped.input).toEqual(expected);
    expect(Buffer.byteLength(JSON.stringify(wantOutput))).toBeLessThanOrEqual(
      (1 << 20) + '" [truncated]"'.length,
    );
    expect(JSON.parse(wantInput)).toBeDefined();
    expect(tool.rawInput).toBe(input);
    expect(tool.output).toBe(output);
  });

  it.each([
    true,
    false,
  ])("redacts each content class; user opt-out=%s", (redactInputMessages) => {
    const secret = "ghp_abcdefghijklmnopqrstuvwxyz1234567890";
    const messages: Message[] = [
      { role: "user", parts: [{ type: "text", text: secret }] },
      { role: "system", parts: [{ type: "text", text: secret }] },
      {
        role: "assistant",
        parts: [
          { type: "text", text: secret },
          { type: "thinking", thinking: secret },
          {
            type: "tool_call",
            toolCall: {
              id: "c",
              name: "bash",
              inputJSON: JSON.stringify({ token: secret }),
            },
          },
        ],
      },
      {
        role: "tool",
        parts: [
          {
            type: "tool_result",
            toolResult: { toolCallId: "c", content: secret },
          },
        ],
      },
    ];
    const result = mapNativeMessages(
      messages,
      redactor,
      "full",
      redactInputMessages,
    );
    expect(JSON.stringify(result[0]).includes(secret)).toBe(
      !redactInputMessages,
    );
    expect(JSON.stringify(result.slice(1))).not.toContain(secret);
    expect(messages[0]?.parts?.[0]).toEqual({ type: "text", text: secret });
  });

  it.each([
    "metadata_only",
    "no_tool_content",
  ] satisfies ContentCaptureMode[])("drops secrets under %s even when user redaction is disabled", (capture) => {
    const result = mapNativeMessages(
      [
        {
          role: "assistant",
          parts: [
            {
              type: "tool_call",
              toolCall: { id: "c", name: "tool", inputJSON: "secret-input" },
            },
            {
              type: "tool_result",
              toolResult: {
                toolCallId: "c",
                content: "secret-result",
                contentJSON: "secret-json",
              },
            },
          ],
        },
      ],
      redactor,
      capture,
      false,
    );
    expect(JSON.stringify(result)).not.toContain("secret");
  });

  it.each([
    undefined,
    40,
  ])("marks completion fallback accurately (streamed=%s)", (streamedAt) => {
    const result = mapNativeGeneration(
      {
        model: { id: "m", providerID: "p" },
        input: [],
        blocks: [],
        streamedAt,
        terminalAt: 100,
        settlement: "step.failed",
      },
      redactor,
      "full",
    );
    expect(result.completedAt).toEqual(new Date(streamedAt ?? 100));
    expect(result.metadata?.["opencode.completion_time_approximate"]).toBe(
      streamedAt === undefined,
    );
    expect(result.metadata).not.toHaveProperty("cost_usd");
  });

  it("filters blank native blocks without trimming meaningful text or reasoning", () => {
    const output = nativeOutput([
      { type: "text", ordinal: 0, text: " \n\t", ended: true },
      { type: "reasoning", ordinal: 1, text: "\t\n ", ended: true },
      { type: "text", ordinal: 2, text: "  answer\n", ended: true },
      { type: "reasoning", ordinal: 3, text: "\n thought  ", ended: true },
    ]);
    expect(output).toEqual([
      { role: "assistant", parts: [{ type: "text", text: "  answer\n" }] },
      {
        role: "assistant",
        parts: [{ type: "thinking", thinking: "\n thought  " }],
      },
    ]);
  });

  it.each([
    "full",
    "no_tool_content",
    "metadata_only",
    "full_with_metadata_spans",
  ] satisfies ContentCaptureMode[])("filters blank history parts under %s without trimming content", (capture) => {
    const messages: Message[] = [
      { role: "user", parts: [{ type: "text", text: " \n\t" }] },
      {
        role: "user",
        parts: [
          { type: "text", text: "\t" },
          { type: "text", text: "  prompt\n" },
        ],
      },
      {
        role: "assistant",
        parts: [
          { type: "text", text: " \n" },
          { type: "thinking", thinking: "\t" },
          { type: "thinking", thinking: "\n thought  " },
        ],
      },
    ];
    expect(mapNativeMessages(messages, redactor, capture)).toEqual(
      capture === "metadata_only"
        ? []
        : [
            { role: "user", parts: [{ type: "text", text: "  prompt\n" }] },
            {
              role: "assistant",
              parts: [{ type: "thinking", thinking: "\n thought  " }],
            },
          ],
    );
  });

  it("preserves observed content order and error identity", () => {
    const output = nativeOutput([
      ...blocks,
      {
        type: "tool",
        id: "failed",
        name: "read",
        rawInput: "malformed",
        completedAt: 30,
        output: "refused",
        isError: true,
      },
    ]);
    expect(output.map((message) => message.role)).toEqual([
      "assistant",
      "assistant",
      "assistant",
      "tool",
      "assistant",
      "tool",
    ]);
    expect(output.at(-1)?.parts?.[0]).toMatchObject({
      type: "tool_result",
      toolResult: { toolCallId: "failed", isError: true },
    });
  });

  it("doesn't invent spans or times for uncalled/unterminated/Code Mode inner calls", () => {
    const records = nativeToolRecords("s", [
      ...blocks,
      { type: "tool", id: "not-executed", name: "bad", completedAt: 30 },
      { type: "tool", id: "running", name: "pending", calledAt: 15 },
    ]);
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      sessionID: "s",
      toolCallId: "call-1",
      startedAt: 10,
      completedAt: 20,
    });
  });
});
