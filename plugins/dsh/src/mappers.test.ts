import { describe, expect, it } from "vitest";
import type { DshGenerateOptions, DshMessage } from "./dsh.js";
import {
  agentNameFor,
  contentText,
  DSH_PURPOSE_METADATA_KEY,
  mapGenerationResult,
  mapGenerationStart,
  mapMessages,
  mapSystemPrompt,
  mapTools,
  mapUsage,
  resolveConversationTitle,
} from "./mappers.js";

const token = () => "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";

describe("mapUsage", () => {
  it("returns undefined when dsh reported no usage", () => {
    expect(mapUsage(undefined)).toBeUndefined();
  });

  it("prefers the exact total and maps the disjoint token buckets", () => {
    expect(
      mapUsage({
        inputTokens: 100,
        outputTokens: 20,
        totalTokens: 999,
        cacheReadTokens: 300,
        cacheWriteTokens: 5,
        reasoningTokens: 7,
      }),
    ).toEqual({
      inputTokens: 100,
      outputTokens: 20,
      totalTokens: 999,
      cacheReadInputTokens: 300,
      cacheWriteInputTokens: 5,
      reasoningTokens: 7,
    });
  });

  it("derives the total and omits cache fields that dsh omitted", () => {
    const usage = mapUsage({ inputTokens: 10, outputTokens: 2 });
    expect(usage).toEqual({
      inputTokens: 10,
      outputTokens: 2,
      totalTokens: 12,
    });
    expect(usage?.inputSemantics).toBeUndefined();
  });
});

describe("mapTools", () => {
  it("deduplicates tools and redacts their bodies", () => {
    const secret = token();
    const tools = mapTools([
      {
        name: "read_file",
        description: `Read ${secret}`,
        parameters: { type: "object", token: secret },
      },
      { name: "read_file", description: "duplicate" },
      { name: "", description: "unnamed" },
    ]);

    expect(tools).toHaveLength(1);
    expect(tools[0]?.name).toBe("read_file");
    expect(tools[0]?.description).not.toContain(secret);
    expect(JSON.parse(tools[0]?.inputSchemaJSON ?? "")).toEqual({
      type: "object",
      token: "[REDACTED:json-secret-field]",
    });
  });

  it("returns an empty list when dsh sent no tools", () => {
    expect(mapTools(undefined)).toEqual([]);
    expect(mapTools([])).toEqual([]);
  });
});

describe("mapMessages", () => {
  const messages: DshMessage[] = [
    { role: "user", content: [{ type: "text", text: "hi" }] },
    {
      role: "assistant",
      content: [
        { type: "reasoning", text: "thinking" },
        { type: "text", text: "calling a tool" },
        {
          type: "tool-call",
          id: "call-1",
          name: "read_file",
          arguments: '{"path":"README.md"}',
        },
      ],
    },
    {
      role: "user",
      source: { kind: "tool" },
      content: [
        {
          type: "tool-result",
          toolCallId: "call-1",
          isError: false,
          content: [{ type: "text", text: "file body" }],
        },
      ],
    },
  ];

  it("maps complete message content before capture-mode stripping", () => {
    const mapped = mapMessages(messages);
    expect(mapped.map((message) => message.role)).toEqual([
      "user",
      "assistant",
      "tool",
    ]);
    expect(mapped[0]?.parts).toEqual([{ type: "text", text: "hi" }]);
    expect(mapped[1]?.parts).toEqual([
      { type: "thinking", thinking: "thinking" },
      { type: "text", text: "calling a tool" },
      {
        type: "tool_call",
        toolCall: {
          id: "call-1",
          name: "read_file",
          inputJSON: '{"path":"README.md"}',
        },
      },
    ]);
    expect(mapped[2]?.parts).toEqual([
      {
        type: "tool_result",
        toolResult: {
          toolCallId: "call-1",
          content: "file body",
          isError: false,
        },
      },
    ]);
  });

  it("splits mixed human text and tool results into valid message roles", () => {
    const mapped = mapMessages([
      {
        role: "user",
        source: { kind: "user" },
        content: [
          { type: "text", text: "before" },
          {
            type: "tool-result",
            toolCallId: "call-1",
            content: [{ type: "text", text: "result" }],
          },
          { type: "text", text: "after" },
        ],
      },
    ]);

    expect(mapped.map((message) => message.role)).toEqual(["user", "tool"]);
    expect(mapped[0]?.parts).toEqual([
      { type: "text", text: "before" },
      { type: "text", text: "after" },
    ]);
    expect(mapped[1]?.parts?.[0]?.type).toBe("tool_result");
  });

  it("omits empty and unrepresentable messages", () => {
    expect(
      mapMessages([
        { role: "user", content: [{ type: "image" }] },
        { role: "user", content: [{ type: "text", text: "  " }] },
        {
          role: "assistant",
          content: [
            { type: "reasoning", text: "" },
            { type: "tool-call", id: "call-1", name: "", arguments: "{}" },
          ],
        },
        { role: "user", content: [] },
      ]),
    ).toEqual([]);
  });
});

describe("contentText", () => {
  it("joins text blocks and drops the rest", () => {
    expect(
      contentText([
        { type: "text", text: "one" },
        { type: "image" },
        { type: "text", text: "two" },
      ]),
    ).toBe("one\ntwo");
  });
});

describe("agentNameFor", () => {
  it("suffixes only subagent sessions", () => {
    expect(
      agentNameFor("dsh", {
        id: "session-2",
        header: { id: "session-2", origin: "subagent" },
      }),
    ).toBe("dsh/subagent");
    expect(agentNameFor("dsh", { id: "s", header: { id: "s" } })).toBe("dsh");
    expect(agentNameFor("dsh", undefined)).toBe("dsh");
  });
});

describe("system message mapping", () => {
  const request: DshGenerateOptions = {
    provider: "deepseek",
    model: "deepseek-chat",
    system: "base policy",
    messages: [
      { role: "user", content: [{ type: "text", text: "before" }] },
      {
        role: "system",
        content: [
          { type: "text", text: "history " },
          { type: "text", text: "policy" },
        ],
      },
      { role: "user", content: [{ type: "text", text: "after" }] },
    ],
  };

  it("keeps system history in place and reserves systemPrompt for the dedicated slot", () => {
    expect(mapSystemPrompt(request)).toBe("base policy");
    const mapped = mapMessages(request.messages);
    expect(mapped.map((message) => message.role)).toEqual([
      "user",
      "user",
      "user",
    ]);
    expect(mapped[1]?.parts).toEqual([
      { type: "text", text: "history " },
      { type: "text", text: "policy" },
    ]);
  });

  it("redacts system history before representing it as user input", () => {
    const mapped = mapMessages([
      {
        role: "system",
        content: [{ type: "text", text: "TOKEN=plain-value" }],
      },
    ]);
    expect(mapped[0]?.parts?.[0]).toEqual({
      type: "text",
      text: "TOKEN=[REDACTED:env-secret-value]",
    });
  });
});

describe("mapGenerationStart", () => {
  const request: DshGenerateOptions = {
    provider: "deepseek",
    model: "deepseek-chat",
    system: "you are helpful",
    maxTokens: 4096,
    temperature: 0.3,
    tools: [{ name: "read_file" }],
  };

  it("maps the request controls, context, system prompt, and tools", () => {
    const start = mapGenerationStart(request, {
      conversationId: "session-1",
      conversationTitle: "Fix the parser",
      agentName: "dsh",
      agentVersion: "test-version",
      startedAt: 1_000,
      tags: { cwd: "/repo" },
    });
    expect(start).toMatchObject({
      conversationId: "session-1",
      conversationTitle: "Fix the parser",
      agentName: "dsh",
      agentVersion: "test-version",
      effectiveVersion: "test-version",
      model: { provider: "deepseek", name: "deepseek-chat" },
      maxTokens: 4096,
      temperature: 0.3,
      systemPrompt: "you are helpful",
      tools: [{ name: "read_file" }],
      tags: { cwd: "/repo" },
    });
  });

  it("records the purpose of an auxiliary call", () => {
    expect(
      mapGenerationStart(
        { ...request, purpose: "compaction" },
        { agentName: "dsh", startedAt: 1_000 },
      ).metadata,
    ).toEqual({ [DSH_PURPOSE_METADATA_KEY]: "compaction" });
  });
});

describe("mapGenerationResult", () => {
  const request: DshGenerateOptions = {
    provider: "deepseek",
    model: "deepseek-chat",
    messages: [{ role: "user", content: [{ type: "text", text: "hi" }] }],
  };

  it("maps ordered output blocks into one assistant message", () => {
    const result = mapGenerationResult({
      request,
      completedAt: 2_000,
      outputBlocks: [
        { type: "text", text: "before" },
        {
          type: "tool-call",
          id: "call-1",
          name: "read_file",
          arguments: '{"path":"README.md"}',
        },
        { type: "reasoning", text: "after" },
      ],
      usage: { inputTokens: 10, outputTokens: 2 },
      finishReason: { kind: "tool-calls" },
    });

    expect(result).toMatchObject({
      responseModel: "deepseek-chat",
      usage: { totalTokens: 12 },
      stopReason: "tool-calls",
      input: [{ role: "user" }],
      output: [
        {
          role: "assistant",
          parts: [
            { type: "text", text: "before" },
            {
              type: "tool_call",
              toolCall: { id: "call-1", name: "read_file" },
            },
            { type: "thinking", thinking: "after" },
          ],
        },
      ],
    });
  });

  it("omits output when no block is representable", () => {
    expect(
      mapGenerationResult({
        request,
        completedAt: 2_000,
        outputBlocks: [{ type: "image" }],
      }).output,
    ).toBeUndefined();
  });
});

describe("resolveConversationTitle", () => {
  it("redacts before clipping and falls back to the conversation id", () => {
    const secret = token();
    const title = resolveConversationTitle({
      sessionTitle: `${"x".repeat(95)}${secret}`,
      conversationId: "session-1",
    });
    expect(title).not.toContain(secret);
    expect(Array.from(title ?? "").length).toBeLessThanOrEqual(100);
    expect(resolveConversationTitle({ conversationId: "session-1" })).toBe(
      "session-1",
    );
  });
});
