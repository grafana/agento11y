import type { ContentCaptureMode, Message } from "@grafana/agento11y";
import { describe, expect, it } from "vitest";
import { createRedactor } from "./redaction.js";
import {
  mapNativeGeneration,
  mapNativeMessages,
  mapNativeUsage,
  type NativeBlock,
  type NativeTokens,
  nativeOutput,
  nativeToolRecords,
} from "./v2-mappers.js";

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
