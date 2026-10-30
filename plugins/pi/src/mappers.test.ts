import {
  Agento11yClient,
  createSecretRedactionSanitizer,
  type ExportGenerationsRequest,
  type GenerationExporter,
  type Message,
  redactSecretTextLightweight,
} from "@grafana/agento11y";
import { describe, expect, expectTypeOf, it } from "vitest";
import {
  applyRedactedText,
  MAX_TITLE_LEN,
  type MapGenerationStartOptions,
  mapAgentMessagesForHook,
  mapGenerationResult,
  mapGenerationStart,
  mapPiUsage,
  mapSummaryGenerationResult,
  mapSummaryGenerationStart,
  mapTools,
  mapUserMessage,
  type PiAgentMessage,
  type PiAssistantMessage,
  type PiSummaryEntryLike,
  type PiToolResult,
  type PiUsageLike,
  type PiUserMessage,
  redactSharedWords,
  redactSpans,
  removedSpans,
  resolveConversationTitle,
  userMessageText,
  userTextRedactions,
} from "./mappers.js";

function makeMsg(overrides?: Partial<PiAssistantMessage>): PiAssistantMessage {
  return {
    role: "assistant",
    content: [{ type: "text", text: "Hello world" }],
    provider: "anthropic",
    model: "claude-sonnet-4-20250514",
    responseId: "resp-1",
    usage: {
      input: 100,
      output: 50,
      cacheRead: 10,
      cacheWrite: 5,
      totalTokens: 165,
      cost: {
        input: 0.003,
        output: 0.006,
        cacheRead: 0.001,
        cacheWrite: 0.002,
        total: 0.012,
      },
    },
    stopReason: "stop",
    timestamp: 1700000001000,
    ...overrides,
  };
}

function makeToolResult(overrides?: Partial<PiToolResult>): PiToolResult {
  return {
    role: "toolResult",
    toolCallId: "call-1",
    toolName: "bash",
    content: [{ type: "text", text: "output" }],
    isError: false,
    timestamp: 1700000002000,
    ...overrides,
  };
}

function makeUserMsg(overrides?: Partial<PiUserMessage>): PiUserMessage {
  return {
    role: "user",
    content: "hey",
    timestamp: 1700000000000,
    ...overrides,
  };
}

const CAPTURE_MODES = [
  "full",
  "full_with_metadata_spans",
  "no_tool_content",
  "metadata_only",
] as const;

const AUXILIARY_USAGE_FIELDS = [
  ["input", "pi.tool_usage.input_tokens"],
  ["output", "pi.tool_usage.output_tokens"],
  ["totalTokens", "pi.tool_usage.total_tokens"],
  ["cacheRead", "pi.tool_usage.cache_read_input_tokens"],
  ["cacheWrite", "pi.tool_usage.cache_write_input_tokens"],
  ["reasoning", "pi.tool_usage.reasoning_tokens"],
  ["cacheWrite1h", "pi.tool_usage.cache_write_1h_input_tokens"],
  ["cost", "pi.tool_usage.cost_usd"],
] as const;

const INVALID_USAGE_VALUES: unknown[] = [
  undefined,
  null,
  -1,
  Number.NaN,
  Number.POSITIVE_INFINITY,
  Number.NEGATIVE_INFINITY,
  "12",
  "secret",
  true,
  {},
  [],
];

describe("mapGenerationStart", () => {
  it("maps model, conversation, agent info", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "session-abc",
      agentName: "pi",
      agentVersion: "1.0.0",
      startedAt: 1700000000000,
    });
    expect(start.model).toEqual({
      provider: "anthropic",
      name: "claude-sonnet-4-20250514",
    });
    expect(start.conversationId).toBe("session-abc");
    expect(start.agentName).toBe("pi");
    expect(start.agentVersion).toBe("1.0.0");
    expect(start.startedAt).toEqual(new Date(1700000000000));
  });

  it("sets conversationTitle when provided and omits it when empty", () => {
    const withTitle = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      conversationTitle: "summarize the go files",
      agentName: "pi",
      startedAt: 0,
    });
    expect(withTitle.conversationTitle).toBe("summarize the go files");

    const without = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    expect(without.conversationTitle).toBeUndefined();
  });

  it("includes tools whenever provided", () => {
    const tools = [{ name: "bash" }, { name: "read" }];
    const result = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      tools,
    });
    expect(result.tools).toEqual(tools);
  });

  it("sets thinkingEnabled when message has a thinking block", () => {
    const msg = makeMsg({
      content: [
        { type: "thinking", thinking: "let me think..." },
        { type: "text", text: "answer" },
      ],
    });
    const start = mapGenerationStart(msg, { agentName: "pi", startedAt: 0 });
    expect(start.thinkingEnabled).toBe(true);
  });

  it("omits thinkingEnabled when no thinking blocks present", () => {
    const start = mapGenerationStart(makeMsg(), {
      agentName: "pi",
      startedAt: 0,
    });
    expect(start.thinkingEnabled).toBeUndefined();
  });

  it("propagates agentVersion into effectiveVersion", () => {
    const msg = makeMsg();
    const a = mapGenerationStart(msg, {
      conversationId: "s",
      agentName: "pi",
      agentVersion: "1.4.7",
      startedAt: 0,
      tools: [{ name: "bash" }],
    });
    const b = mapGenerationStart(msg, {
      conversationId: "s",
      agentName: "pi",
      agentVersion: "1.4.7",
      startedAt: 0,
      tools: [{ name: "read" }],
    });
    expect(a.effectiveVersion).toBe("1.4.7");
    expect(a.effectiveVersion).toBe(b.effectiveVersion);
    expect(a.effectiveVersion).toBe(a.agentVersion);
  });

  it("leaves effectiveVersion unset when agentVersion is missing", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    expect(start.effectiveVersion).toBeUndefined();
  });

  it("attaches tags when provided", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      tags: { "git.branch": "main" },
    });
    expect(start.tags).toEqual({ "git.branch": "main" });
  });

  it("omits tags when empty or undefined", () => {
    const a = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      tags: {},
    });
    const b = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    expect(a.tags).toBeUndefined();
    expect(b.tags).toBeUndefined();
  });

  it("does not accept unverified prompts or request controls", () => {
    expectTypeOf<
      Extract<
        keyof MapGenerationStartOptions,
        "systemPrompt" | "requestControls"
      >
    >().toBeNever();
    const unverified = {
      systemPrompt: "You are a helpful agent.",
      requestControls: {
        maxTokens: 1024,
        temperature: 0.2,
        topP: 0.9,
        toolChoice: "auto",
        thinkingBudgetTokens: 4096,
      },
      tools: [
        { name: "bash", description: "unverified", inputSchemaJSON: "{}" },
      ],
    };
    const start = mapGenerationStart(makeMsg(), {
      agentName: "pi",
      startedAt: 0,
      ...unverified,
    });
    expect(start.systemPrompt).toBeUndefined();
    expect(start.maxTokens).toBeUndefined();
    expect(start.temperature).toBeUndefined();
    expect(start.topP).toBeUndefined();
    expect(start.toolChoice).toBeUndefined();
    expect(start.metadata).toBeUndefined();
    expect(start.tools).toEqual([{ name: "bash" }]);
    expect(JSON.stringify(start)).not.toContain("unverified");
  });

  it("passes caller metadata through", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      metadata: { "pi.fork.parent_session_id": "trunk-conv" },
    });
    expect(start.metadata).toEqual({
      "pi.fork.parent_session_id": "trunk-conv",
    });
  });

  it("omits metadata when the caller passes an empty bag", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      metadata: {},
    });
    expect(start.metadata).toBeUndefined();
  });

  it("copies generationId to start.id when set", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      generationId: "pi-abcdef0123456789abcdef01",
    });
    expect(start.id).toBe("pi-abcdef0123456789abcdef01");
  });

  it("omits start.id when generationId is missing or empty", () => {
    const a = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    const b = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      generationId: "",
    });
    expect(a.id).toBeUndefined();
    expect(b.id).toBeUndefined();
  });

  it("copies parentGenerationIds when set", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      parentGenerationIds: ["pi-deadbeefcafebabe00010203"],
    });
    expect(start.parentGenerationIds).toEqual(["pi-deadbeefcafebabe00010203"]);
  });

  it("omits parentGenerationIds when empty or undefined", () => {
    const a = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    const b = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      parentGenerationIds: [],
    });
    expect(a.parentGenerationIds).toBeUndefined();
    expect(b.parentGenerationIds).toBeUndefined();
  });
});

describe("mapGenerationResult", () => {
  it.each([
    { responseModel: undefined, expected: "claude-sonnet-4-20250514" },
    { responseModel: null, expected: "claude-sonnet-4-20250514" },
    {
      responseModel: "physical-response-model",
      expected: "physical-response-model",
    },
    { responseModel: "", expected: "" },
  ])("maps response identity, usage and metadata: $responseModel", ({
    responseModel,
    expected,
  }) => {
    const msg = makeMsg({ responseModel });
    for (const mode of CAPTURE_MODES) {
      const start = mapGenerationStart(msg, { agentName: "pi", startedAt: 0 });
      const result = mapGenerationResult(
        msg,
        [
          makeToolResult({
            usage: { input: 999, cost: { total: 123 } },
          }),
        ],
        mode,
      );

      expect(result.usage).toEqual({
        inputTokens: 100,
        outputTokens: 50,
        totalTokens: 165,
        cacheReadInputTokens: 10,
        cacheWriteInputTokens: 5,
      });
      expect(start.model).toEqual({ provider: msg.provider, name: msg.model });
      expect(result.responseModel).toBe(expected);
      expect(result.stopReason).toBe("end_turn");
      expect(result.completedAt).toEqual(new Date(1700000001000));
      expect(result.metadata).toEqual({
        cost_usd: 0.012,
        "pi.tool_usage.input_tokens": 999,
        "pi.tool_usage.cost_usd": 123,
      });
    }
  });

  it("omits cost_usd when usage.cost is missing", () => {
    const msg = makeMsg({
      usage: {
        input: 1,
        output: 2,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 3,
        // intentionally no `cost`
      },
    });
    const result = mapGenerationResult(msg, [], "metadata_only");
    expect(result.metadata).toEqual({});
  });

  it("uses provider-reported totalTokens (includes cache)", () => {
    const msg = makeMsg({
      usage: {
        input: 100,
        output: 50,
        cacheRead: 200,
        cacheWrite: 30,
        totalTokens: 380,
        cost: {
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheWrite: 0,
          total: 0,
        },
      },
    });
    const result = mapGenerationResult(msg, [], "metadata_only");
    expect(result.usage?.totalTokens).toBe(380);
  });

  it("metadata_only with no tool calls/results produces no output", () => {
    const result = mapGenerationResult(makeMsg(), [], "metadata_only");
    expect(result.output).toBeUndefined();
  });

  it("metadata_only emits structural tool parts with empty bodies (so SDK can count tool calls)", () => {
    const msg = makeMsg({
      content: [
        { type: "text", text: "I'll run that command" },
        {
          type: "toolCall",
          id: "c1",
          name: "bash",
          arguments: { command: "ls" },
        },
      ],
    });
    const toolResults = [
      makeToolResult({
        toolCallId: "c1",
        content: [{ type: "text", text: "file.txt" }],
      }),
    ];
    const result = mapGenerationResult(msg, toolResults, "metadata_only");

    // text/thinking suppressed; structural tool_call + tool_result present.
    expect(result.output).toHaveLength(2);
    const partTypes = result.output?.flatMap(
      (m) => m.parts?.map((p) => p.type) ?? [],
    );
    expect(partTypes).not.toContain("text");
    expect(partTypes).toContain("tool_call");
    expect(partTypes).toContain("tool_result");

    const toolCallPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_call");
    expect(
      (toolCallPart as { toolCall: { inputJSON: string } }).toolCall.inputJSON,
    ).toBe("");

    const toolResultPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_result");
    expect(
      (toolResultPart as { toolResult: { content: string } }).toolResult
        .content,
    ).toBe("");
  });

  it("no_tool_content emits text/thinking + structural tool parts with empty bodies", () => {
    const msg = makeMsg({
      content: [
        { type: "text", text: "I'll run that command" },
        { type: "thinking", thinking: "deciding" },
        {
          type: "toolCall",
          id: "c1",
          name: "bash",
          arguments: { command: "ls" },
        },
      ],
    });
    const toolResults = [
      makeToolResult({
        toolCallId: "c1",
        content: [{ type: "text", text: "file.txt" }],
      }),
    ];
    const result = mapGenerationResult(msg, toolResults, "no_tool_content");

    // text + thinking + tool_call + tool_result = 4 messages.
    expect(result.output).toHaveLength(4);
    const partTypes = result.output?.flatMap(
      (m) => m.parts?.map((p) => p.type) ?? [],
    );
    expect(partTypes).toContain("text");
    expect(partTypes).toContain("thinking");
    expect(partTypes).toContain("tool_call");
    expect(partTypes).toContain("tool_result");

    const toolCallPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_call");
    expect(
      (toolCallPart as { toolCall: { inputJSON: string } }).toolCall.inputJSON,
    ).toBe("");

    const toolResultPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_result");
    expect(
      (toolResultPart as { toolResult: { content: string } }).toolResult
        .content,
    ).toBe("");
  });

  it("full mode emits assistant text, tool_call, and tool_result", () => {
    const msg = makeMsg({
      content: [
        { type: "text", text: "I'll run that command" },
        {
          type: "toolCall",
          id: "c1",
          name: "bash",
          arguments: { command: "ls" },
        },
      ],
    });
    const toolResults = [
      makeToolResult({
        toolCallId: "c1",
        content: [{ type: "text", text: "file.txt" }],
      }),
    ];
    const result = mapGenerationResult(msg, toolResults, "full");

    expect(result.output).toHaveLength(3);
    const roles = result.output?.map((m) => m.role);
    expect(roles).toContain("assistant");
    expect(roles).toContain("tool");
  });

  describe.each(CAPTURE_MODES)("finalized results in %s", (mode) => {
    it.each([
      { command: "ls" },
      undefined,
    ])("maps observed or omitted tool proposals: %j", (args) => {
      const msg = makeMsg({
        content: [
          { type: "text", text: "I'll run that command" },
          {
            type: "toolCall",
            id: "c1",
            name: "bash",
            arguments: args,
          },
        ],
      });
      const toolResults = [
        makeToolResult({
          toolCallId: "c1",
          content: [{ type: "text", text: "file.txt" }],
        }),
      ];
      const result = mapGenerationResult(msg, toolResults, mode);

      const toolCallPart = result.output
        ?.flatMap((m) => m.parts ?? [])
        .find((p) => p.type === "tool_call");
      expect(
        (toolCallPart as { toolCall: { inputJSON: string } }).toolCall
          .inputJSON,
      ).toBe(
        (mode === "full" || mode === "full_with_metadata_spans") && args
          ? JSON.stringify(args)
          : "",
      );

      const toolResultPart = result.output
        ?.flatMap((m) => m.parts ?? [])
        .find((p) => p.type === "tool_result");
      expect(
        (toolResultPart as { toolResult: { content: string } }).toolResult
          .content,
      ).toBe(
        mode === "full" || mode === "full_with_metadata_spans"
          ? "file.txt"
          : "",
      );
    });

    it.each([
      false,
      true,
    ])("counts finalized host aggregates once, with nested records=%s", (nested) => {
      const aggregate = {
        input: 200,
        output: 40,
        totalTokens: 275,
        cacheRead: 20,
        cacheWrite: 15,
        cacheWrite1h: 5,
        reasoning: 10,
        cost: { total: 0.5 },
        model: "unattributed-tool-model",
        provider: "unattributed-tool-provider",
        secret: "private",
        unknownCount: 999,
      };
      const topLevel = {
        ...makeToolResult({ usage: aggregate }),
        ...(nested
          ? {
              nestedCalls: {
                calls: [
                  {
                    id: "call-1/1",
                    name: "child",
                    status: "ok",
                    usage: aggregate,
                  },
                ],
                complete: true,
              },
              details: { usage: aggregate, results: [{ usage: aggregate }] },
            }
          : {}),
      };
      const toolResults = [
        topLevel,
        makeToolResult({
          toolCallId: "call-2",
          isError: true,
          usage: { input: 3, output: 2, totalTokens: 5, cost: { total: 0.25 } },
        }),
      ];
      const msg = makeMsg();
      msg.usage.cacheWrite1h = 3;
      msg.usage.reasoning = 7;
      const before = structuredClone({ msg, toolResults });
      const baseline = mapGenerationResult(msg, [], mode);
      const result = mapGenerationResult(msg, toolResults, mode);
      expect(result.metadata).toEqual({
        cost_usd: 0.012,
        "pi.usage.cache_write_1h_input_tokens": 3,
        "pi.tool_usage.input_tokens": 203,
        "pi.tool_usage.output_tokens": 42,
        "pi.tool_usage.total_tokens": 280,
        "pi.tool_usage.cache_read_input_tokens": 20,
        "pi.tool_usage.cache_write_input_tokens": 15,
        "pi.tool_usage.cache_write_1h_input_tokens": 5,
        "pi.tool_usage.reasoning_tokens": 10,
        "pi.tool_usage.cost_usd": 0.75,
      });
      expect(result.usage).toEqual(baseline.usage);
      expect(result.responseModel).toBe(baseline.responseModel);
      expect(result.responseId).toBe(baseline.responseId);
      expect(
        result.output?.filter((message) => message.role === "tool"),
      ).toHaveLength(2);
      expect({ msg, toolResults }).toEqual(before);
    });

    it.each(
      INVALID_USAGE_VALUES,
    )("omits invalid auxiliary fields: %j", (value) => {
      const usage = Object.fromEntries(
        AUXILIARY_USAGE_FIELDS.map(([source]) => [
          source,
          source === "cost" ? { total: value } : value,
        ]),
      ) as PiUsageLike;
      const msg = makeMsg();
      const result = mapGenerationResult(
        msg,
        [makeToolResult({ usage })],
        mode,
      );
      expect(result.metadata).toEqual({ cost_usd: 0.012 });
      expect(result.usage).toEqual(mapPiUsage(msg.usage));
      expect(result.responseModel).toBe(msg.model);
    });

    it.each(
      AUXILIARY_USAGE_FIELDS,
    )("validates and bounds %s", (source, key) => {
      const withValue = (value: unknown): PiToolResult =>
        makeToolResult({
          usage: {
            [source]: source === "cost" ? { total: value } : value,
          } as PiUsageLike,
        });
      for (const value of [0, 1.5, Number.MAX_VALUE]) {
        const result = mapGenerationResult(makeMsg(), [withValue(value)], mode);
        expect(result.metadata).toEqual({ cost_usd: 0.012, [key]: value });
      }
      for (const invalid of INVALID_USAGE_VALUES) {
        const result = mapGenerationResult(
          makeMsg(),
          [withValue(2), withValue(invalid), withValue(3)],
          mode,
        );
        expect(result.metadata).toEqual({ cost_usd: 0.012, [key]: 5 });
      }
      const overflowing = [
        withValue(Number.MAX_VALUE),
        withValue(Number.MAX_VALUE),
        withValue(1),
      ];
      overflowing.push(makeToolResult({ usage: { input: 1, output: 1 } }));
      const result = mapGenerationResult(makeMsg(), overflowing, mode);
      expect(result.metadata).not.toHaveProperty(key);
      expect(result.metadata?.cost_usd).toBe(0.012);
      const unaffected = source === "input" ? "output_tokens" : "input_tokens";
      expect(result.metadata?.[`pi.tool_usage.${unaffected}`]).toBe(1);
    });

    it.each([
      { usage: undefined },
      { usage: {} },
      { usage: { cost: { input: 1, output: 2, totalcost: 3 } } },
      { usage: { cost: 0.5 } },
      {
        usage: { totalcost: 3, total_tokens: 10, inputTokens: 5, unknown: 123 },
      },
      { details: { usage: { input: 100, cost: { total: 0.5 } } } },
      {
        nestedCalls: {
          calls: [{ id: "child", usage: { input: 100 } }],
          complete: true,
        },
      },
    ])("does not infer aggregate usage or cost from other fields: %j", (fields) => {
      const msg = makeMsg();
      const toolResult = { ...makeToolResult(), ...fields } as PiToolResult;
      const result = mapGenerationResult(msg, [toolResult], mode);
      expect(result.metadata).toEqual({ cost_usd: 0.012 });
    });
  });

  it("skips redacted thinking blocks", () => {
    const msg = makeMsg({
      content: [
        { type: "thinking", thinking: "encrypted-blob", redacted: true },
        { type: "text", text: "result" },
      ],
    });
    const result = mapGenerationResult(msg, [], "full");
    const allText = result.output
      ?.map(
        (m) =>
          m.parts
            ?.map((p) => {
              if (p.type === "text") return p.text;
              if (p.type === "thinking") return p.thinking;
              return "";
            })
            .join("") ?? "",
      )
      .join("");
    expect(allText).not.toContain("encrypted-blob");
    expect(allText).toContain("result");
  });

  it("maps stop reasons", () => {
    expect(
      mapGenerationResult(makeMsg({ stopReason: "stop" }), [], "metadata_only")
        .stopReason,
    ).toBe("end_turn");
    expect(
      mapGenerationResult(
        makeMsg({ stopReason: "length" }),
        [],
        "metadata_only",
      ).stopReason,
    ).toBe("max_tokens");
    expect(
      mapGenerationResult(
        makeMsg({ stopReason: "toolUse" }),
        [],
        "metadata_only",
      ).stopReason,
    ).toBe("tool_use");
    expect(
      mapGenerationResult(makeMsg({ stopReason: "error" }), [], "metadata_only")
        .stopReason,
    ).toBe("error");
  });
});

describe("mapUserMessage", () => {
  it("maps string content to a single text part in full mode", () => {
    const msg = makeUserMsg({ content: "hello world" });
    const out = mapUserMessage(msg, "full");
    expect(out).toEqual({
      role: "user",
      parts: [{ type: "text", text: "hello world" }],
    });
  });

  it("maps a TextContent array to a joined text part", () => {
    const msg = makeUserMsg({
      content: [
        { type: "text", text: "first" },
        { type: "text", text: "second" },
      ],
    });
    const out = mapUserMessage(msg, "full");
    expect(out?.role).toBe("user");
    const text = (out?.parts?.[0] as { type: "text"; text: string }).text;
    expect(text).toContain("first");
    expect(text).toContain("second");
  });

  it("filters out image content and keeps text only", () => {
    const msg = makeUserMsg({
      content: [
        { type: "text", text: "look at this" },
        { type: "image", data: "ZmFrZQ==", mimeType: "image/png" },
        { type: "text", text: "thanks" },
      ],
    });
    const out = mapUserMessage(msg, "full");
    expect(out?.parts).toHaveLength(1);
    const part = out?.parts?.[0] as { type: "text"; text: string };
    expect(part.type).toBe("text");
    expect(part.text).toContain("look at this");
    expect(part.text).toContain("thanks");
    const partTypes = out?.parts?.map((p) => p.type);
    expect(partTypes).not.toContain("image");
  });

  it("returns null for whitespace-only string content", () => {
    expect(
      mapUserMessage(makeUserMsg({ content: "   \n\t" }), "full"),
    ).toBeNull();
  });

  it("returns null for empty content array", () => {
    expect(mapUserMessage(makeUserMsg({ content: [] }), "full")).toBeNull();
  });

  it("returns null for an image-only array (no text parts)", () => {
    const msg = makeUserMsg({
      content: [{ type: "image", data: "ZmFrZQ==", mimeType: "image/png" }],
    });
    expect(mapUserMessage(msg, "full")).toBeNull();
  });

  it("returns null in metadata_only mode regardless of content", () => {
    expect(
      mapUserMessage(makeUserMsg({ content: "hey" }), "metadata_only"),
    ).toBeNull();
    expect(
      mapUserMessage(
        makeUserMsg({
          content: [{ type: "text", text: "hey" }],
        }),
        "metadata_only",
      ),
    ).toBeNull();
  });

  it("emits text in no_tool_content mode", () => {
    const out = mapUserMessage(
      makeUserMsg({ content: "hey" }),
      "no_tool_content",
    );
    expect(out).toEqual({
      role: "user",
      parts: [{ type: "text", text: "hey" }],
    });
  });
});

describe("resolveConversationTitle", () => {
  it("prefers a user-set session name over the first prompt", () => {
    expect(
      resolveConversationTitle({
        sessionName: "My named session",
        firstUserText: "summarize the go files",
        conversationId: "pi-conv-1",
        contentCapture: "full",
      }),
    ).toBe("My named session");
  });

  it("derives from the first prompt when no session name is set", () => {
    expect(
      resolveConversationTitle({
        firstUserText: "summarize the go files",
        conversationId: "pi-conv-1",
        contentCapture: "full",
      }),
    ).toBe("summarize the go files");
  });

  it("trims whitespace from session name and derived title", () => {
    expect(
      resolveConversationTitle({
        sessionName: "  spaced name  ",
        contentCapture: "full",
      }),
    ).toBe("spaced name");
    expect(
      resolveConversationTitle({
        firstUserText: "  hi there  ",
        contentCapture: "full",
      }),
    ).toBe("hi there");
  });

  it("ignores a blank session name and falls through to the prompt", () => {
    expect(
      resolveConversationTitle({
        sessionName: "   ",
        firstUserText: "do the thing",
        conversationId: "pi-conv-1",
        contentCapture: "full",
      }),
    ).toBe("do the thing");
  });

  it("suppresses the derived title in metadata_only but keeps the session name", () => {
    expect(
      resolveConversationTitle({
        firstUserText: "summarize the go files",
        conversationId: "pi-conv-1",
        contentCapture: "metadata_only",
      }),
    ).toBe("pi-conv-1");
    expect(
      resolveConversationTitle({
        sessionName: "My named session",
        firstUserText: "summarize the go files",
        conversationId: "pi-conv-1",
        contentCapture: "metadata_only",
      }),
    ).toBe("My named session");
  });

  it("falls back to the conversation id when nothing else is available", () => {
    expect(
      resolveConversationTitle({
        conversationId: "pi-conv-1",
        contentCapture: "full",
      }),
    ).toBe("pi-conv-1");
  });

  it("returns undefined when there is no name, prompt, or id", () => {
    expect(
      resolveConversationTitle({ contentCapture: "full" }),
    ).toBeUndefined();
  });

  it("caps the title at MAX_TITLE_LEN code points without splitting surrogates", () => {
    const long = "a".repeat(150);
    expect(
      resolveConversationTitle({ firstUserText: long, contentCapture: "full" }),
    ).toHaveLength(MAX_TITLE_LEN);

    // 150 two-code-unit emoji = 150 code points; the cap counts code points,
    // so it clips to 100 without slicing mid-surrogate into a replacement char.
    const emoji = "😀".repeat(150);
    const clipped = resolveConversationTitle({
      firstUserText: emoji,
      contentCapture: "full",
    });
    expect(Array.from(clipped ?? "")).toHaveLength(MAX_TITLE_LEN);
    expect(clipped).not.toContain("\uFFFD");
  });

  // Regression: the title was clipped before the SDK's sanitizer redacted it.
  // A secret the clip split no longer matched its pattern, so its first part
  // was exported in the title while the same prompt's input was redacted.
  describe("redacts a secret before the clip can split it", () => {
    // What the SDK's generation sanitizer does to conversationTitle on export.
    const exported = (title: string | undefined) =>
      redactSecretTextLightweight(title ?? "");
    const token = `ghp_${"A".repeat(36)}`;

    it("a token across the clip", () => {
      const title = resolveConversationTitle({
        firstUserText: `${"x".repeat(70)} ${token} please`,
        contentCapture: "full",
      });
      expect(exported(title)).toBe(
        `${"x".repeat(70)} [REDACTED:github-pat] please`,
      );
    });

    it("a token whose marker crosses the clip", () => {
      const title = resolveConversationTitle({
        firstUserText: `${"x".repeat(85)} ${token} please`,
        contentCapture: "full",
      });
      expect(exported(title)).toBe("x".repeat(85));
    });

    it("a token in a session name", () => {
      const title = resolveConversationTitle({
        sessionName: `${"y".repeat(70)} ${token}`,
        firstUserText: "unused",
        contentCapture: "metadata_only",
      });
      expect(exported(title)).not.toContain("ghp_");
      expect(exported(title)).toContain("[REDACTED:github-pat]");
    });
  });

  // Regression: the SDK sends the title twice, as conversationTitle and as the
  // agento11y.conversation.title metadata mirror, and its sanitizer redacts
  // only the first. A whole token in the first prompt went out unredacted in
  // the mirror. Pi now hands the SDK a title that is already redacted.
  it("exports no secret in the title or its metadata mirror", async () => {
    const token = `ghp_${"A".repeat(36)}`;
    for (const prompt of [
      `deploy ${token} now`,
      `${"x".repeat(70)} ${token} please`,
    ]) {
      const requests: ExportGenerationsRequest[] = [];
      const exporter: GenerationExporter = {
        exportGenerations: async (request) => {
          requests.push(structuredClone(request));
          return {
            results: request.generations.map((g) => ({
              generationId: g.id,
              accepted: true,
            })),
          };
        },
        exportWorkflowSteps: async () => ({ results: [] }),
      };
      // As createAgento11yClient configures it, with the export captured.
      const client = new Agento11yClient({
        generationExporter: exporter,
        contentCapture: "full",
        generationSanitizer: createSecretRedactionSanitizer({
          redactInputMessages: true,
        }),
      });
      const recorder = client.startGeneration({
        conversationId: "pi-conv-1",
        conversationTitle: resolveConversationTitle({
          firstUserText: prompt,
          contentCapture: "full",
        }),
        model: { provider: "anthropic", name: "claude" },
      });
      recorder.end();
      await client.shutdown();

      const generation = requests[0]?.generations[0];
      expect(generation?.conversationTitle).toContain("[REDACTED:github-pat]");
      expect(JSON.stringify(generation)).not.toContain("ghp_");
    }
  });

  it("keeps text that only looks like the start of a marker", () => {
    const text = `see [REDACTED: notes ${"z".repeat(120)}`;
    expect(
      resolveConversationTitle({ firstUserText: text, contentCapture: "full" }),
    ).toBe(Array.from(text).slice(0, MAX_TITLE_LEN).join(""));
  });
});

describe("userMessageText", () => {
  it("returns string content as-is", () => {
    expect(userMessageText(makeUserMsg({ content: "hello" }))).toBe("hello");
  });

  it("joins text parts and drops images", () => {
    const text = userMessageText(
      makeUserMsg({
        content: [
          { type: "text", text: "first" },
          { type: "image", data: "ZmFrZQ==", mimeType: "image/png" },
          { type: "text", text: "second" },
        ],
      }),
    );
    expect(text).toBe("first\nsecond");
  });
});

describe("mapGenerationResult input wiring", () => {
  it("sets input when a non-empty list is passed", () => {
    const msg = makeMsg();
    const input = [
      { role: "user", parts: [{ type: "text" as const, text: "hey" }] },
    ];
    const result = mapGenerationResult(msg, [], "full", input);
    expect(result.input).toEqual(input);
  });

  it("omits input when not passed", () => {
    const result = mapGenerationResult(makeMsg(), [], "full");
    expect(result.input).toBeUndefined();
  });

  it("omits input when an empty array is passed", () => {
    const result = mapGenerationResult(makeMsg(), [], "full", []);
    expect(result.input).toBeUndefined();
  });
});

describe("mapTools", () => {
  it.each([
    { tools: [], want: [] },
    { tools: [{ name: "bash" }], want: [{ name: "bash" }] },
    {
      tools: [
        {
          name: "bash",
          description: "unverified",
          parameters: { type: "object" },
        },
        { name: "bash", inputSchemaJSON: "{}" },
        { name: "read" },
      ],
      want: [{ name: "bash" }, { name: "read" }],
    },
  ])("keeps only distinct called names: $tools", ({ tools, want }) => {
    expect(mapTools(tools)).toEqual(want);
  });
});

describe("mapAgentMessagesForHook", () => {
  it("maps user, assistant, and tool result messages 1:1 regardless of contentCapture", () => {
    const messages: PiAgentMessage[] = [
      { role: "user", content: "hello secret@example.com", timestamp: 1 },
      {
        role: "assistant",
        content: [
          { type: "text", text: "hi" },
          {
            type: "toolCall",
            id: "c1",
            name: "bash",
            arguments: { cmd: "ls" },
          },
        ],
        provider: "anthropic",
        model: "claude-sonnet-4",
        usage: {
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 0,
        },
        stopReason: "toolUse",
        timestamp: 2,
      },
      {
        role: "toolResult",
        toolCallId: "c1",
        toolName: "bash",
        content: [{ type: "text", text: "output" }],
        isError: false,
        timestamp: 3,
      },
    ];
    const out = mapAgentMessagesForHook(messages);
    expect(out).toHaveLength(3);
    expect(out[0]).toEqual({
      role: "user",
      parts: [{ type: "text", text: "hello secret@example.com" }],
    });
    expect(out[1]).toMatchObject({
      role: "assistant",
      parts: [{ type: "text", text: "hi" }],
    });
    expect(out[2]?.role).toBe("tool");
  });

  it("drops thinking parts from the forward payload but keeps the message slot", () => {
    const messages: PiAgentMessage[] = [
      {
        role: "assistant",
        content: [
          { type: "thinking", thinking: "reasoning" },
          { type: "text", text: "answer" },
        ],
        provider: "anthropic",
        model: "claude-sonnet-4",
        usage: {
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 0,
        },
        stopReason: "stop",
        timestamp: 1,
      },
    ];
    const out = mapAgentMessagesForHook(messages);
    expect(out).toHaveLength(1);
    const parts = out[0]?.parts ?? [];
    expect(parts.some((p) => p.type === "thinking")).toBe(false);
    expect(parts).toEqual([{ type: "text", text: "answer" }]);
  });

  it("emits a placeholder for unknown/custom agent message subtypes", () => {
    const messages: PiAgentMessage[] = [
      { role: "user", content: "hi", timestamp: 1 },
      // Simulated custom AgentMessage (e.g. a UI-only notification).
      { role: "bash_execution" } as PiAgentMessage,
    ];
    const out = mapAgentMessagesForHook(messages);
    // Alignment must be preserved — unknown roles must not be dropped
    // because that would break index-aligned write-back.
    expect(out).toHaveLength(2);
    expect(out[1]?.role).toBe("bash_execution");
    expect(out[1]?.parts).toEqual([]);
  });

  it("emits a placeholder for null/non-object slots so alignment is preserved", () => {
    const messages = [
      { role: "user", content: "hi", timestamp: 1 },
      null,
      { role: "user", content: "bye", timestamp: 2 },
    ] as unknown as PiAgentMessage[];
    const out = mapAgentMessagesForHook(messages);
    // The null slot must not be dropped, otherwise forward shrinks below
    // piMessages and the whole transform is discarded on write-back.
    expect(out).toHaveLength(3);
    expect(out[1]).toEqual({ role: "unknown", parts: [] });
  });

  it("emits a user message with an empty parts array when content is image-only", () => {
    const messages: PiAgentMessage[] = [
      {
        role: "user",
        content: [{ type: "image", data: "x", mimeType: "image/png" }],
        timestamp: 1,
      },
    ];
    const out = mapAgentMessagesForHook(messages);
    expect(out[0]).toEqual({ role: "user", parts: [] });
  });
});

describe("applyRedactedText", () => {
  it("overwrites string user content with the redacted text", () => {
    const pi: PiAgentMessage[] = [
      {
        role: "user",
        content: "my email is leak@example.com",
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [
      {
        role: "user",
        parts: [{ type: "text", text: "my email is [REDACTED_EMAIL]" }],
      },
    ];
    const ok = applyRedactedText(pi, redacted);
    expect(ok).toBe(true);
    expect((pi[0] as PiUserMessage).content).toBe(
      "my email is [REDACTED_EMAIL]",
    );
  });

  it("overwrites the first text block of array-shaped user content and clears the rest", () => {
    const pi: PiAgentMessage[] = [
      {
        role: "user",
        content: [
          { type: "text", text: "hi leak@example.com" },
          { type: "image", data: "x", mimeType: "image/png" },
          { type: "text", text: "bye" },
        ],
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [
      {
        role: "user",
        parts: [{ type: "text", text: "hi [REDACTED_EMAIL]\nbye" }],
      },
    ];
    applyRedactedText(pi, redacted);
    const content = (pi[0] as PiUserMessage).content as Array<{
      type: string;
      text?: string;
    }>;
    expect(content[0]?.text).toBe("hi [REDACTED_EMAIL]\nbye");
    // Image must not be touched.
    expect(content[1]).toEqual({
      type: "image",
      data: "x",
      mimeType: "image/png",
    });
    // Trailing text part cleared so the rendered message equals the
    // redacted payload exactly.
    expect(content[2]?.text).toBe("");
  });

  it("leaves provider-signed thinking parts on assistant messages untouched", () => {
    const pi: PiAgentMessage[] = [
      {
        role: "assistant",
        content: [
          { type: "thinking", thinking: "opaque-sig" },
          { type: "text", text: "original secret" },
          {
            type: "toolCall",
            id: "c1",
            name: "bash",
            arguments: { cmd: "ls" },
          },
        ],
        provider: "anthropic",
        model: "claude-sonnet-4",
        usage: {
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 0,
        },
        stopReason: "toolUse",
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [
      {
        role: "assistant",
        parts: [{ type: "text", text: "original [REDACTED]" }],
      },
    ];
    applyRedactedText(pi, redacted);
    const content = (pi[0] as PiAssistantMessage).content;
    expect(content[0]).toEqual({ type: "thinking", thinking: "opaque-sig" });
    expect(content[1]).toMatchObject({
      type: "text",
      text: "original [REDACTED]",
    });
    expect(content[2]).toMatchObject({
      type: "toolCall",
      id: "c1",
      name: "bash",
      arguments: { cmd: "ls" },
    });
  });

  it("returns false when message counts diverge", () => {
    const pi: PiAgentMessage[] = [
      { role: "user", content: "a", timestamp: 1 },
      { role: "user", content: "b", timestamp: 2 },
    ];
    const redacted: Message[] = [
      { role: "user", parts: [{ type: "text", text: "a" }] },
    ];
    expect(applyRedactedText(pi, redacted)).toBe(false);
    // pi unchanged.
    expect((pi[0] as PiUserMessage).content).toBe("a");
    expect((pi[1] as PiUserMessage).content).toBe("b");
  });

  it("skips a null/non-object pi slot and still redacts the rest of the turn", () => {
    const pi = [
      { role: "user", content: "leak@example.com", timestamp: 1 },
      null,
      { role: "user", content: "also leak@example.com", timestamp: 2 },
    ] as unknown as PiAgentMessage[];
    const redacted: Message[] = [
      { role: "user", parts: [{ type: "text", text: "[REDACTED_EMAIL]" }] },
      { role: "unknown", parts: [] },
      {
        role: "user",
        parts: [{ type: "text", text: "also [REDACTED_EMAIL]" }],
      },
    ];
    const ok = applyRedactedText(pi, redacted);
    expect(ok).toBe(true);
    expect((pi[0] as PiUserMessage).content).toBe("[REDACTED_EMAIL]");
    expect(pi[1]).toBeNull();
    expect((pi[2] as PiUserMessage).content).toBe("also [REDACTED_EMAIL]");
  });

  it("no-ops when redacted message has no extractable text", () => {
    const pi: PiAgentMessage[] = [
      {
        role: "user",
        content: "hi",
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [{ role: "user", parts: [] }];
    const ok = applyRedactedText(pi, redacted);
    expect(ok).toBe(true);
    expect((pi[0] as PiUserMessage).content).toBe("hi");
  });

  it("tolerates the legacy `content` shorthand on the redacted side", () => {
    const pi: PiAgentMessage[] = [
      { role: "user", content: "hi leak@example.com", timestamp: 1 },
    ];
    const redacted: Message[] = [
      { role: "user", content: "hi [REDACTED_EMAIL]" },
    ];
    applyRedactedText(pi, redacted);
    expect((pi[0] as PiUserMessage).content).toBe("hi [REDACTED_EMAIL]");
  });

  it("overwrites tool-result text in place from the redacted tool_result part", () => {
    // The server keeps the `tool_result` part shape and redacts its
    // `content` field in place (grafana/sigil
    // `internal/eval/hooks/transform.go`). The plugin must read the redaction
    // off the tool_result part, not from a synthetic text part that the
    // server never emits.
    const pi: PiAgentMessage[] = [
      {
        role: "toolResult",
        toolCallId: "c1",
        toolName: "bash",
        content: [{ type: "text", text: "secret-output sk-XXXX" }],
        isError: false,
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [
      {
        role: "tool",
        parts: [
          {
            type: "tool_result",
            toolResult: {
              toolCallId: "c1",
              name: "bash",
              content: "secret-output [REDACTED_API_KEY]",
              isError: false,
            },
          },
        ],
      },
    ];
    const ok = applyRedactedText(pi, redacted);
    expect(ok).toBe(true);
    expect((pi[0] as PiToolResult).content[0]).toMatchObject({
      type: "text",
      text: "secret-output [REDACTED_API_KEY]",
    });
  });

  it("no-ops a tool-result message when the redacted tool_result part is absent", () => {
    // Defensive: if the server returns the tool role with no tool_result
    // part (e.g. a transformed payload that dropped it), leave pi untouched
    // rather than guessing from any other parts.
    const pi: PiAgentMessage[] = [
      {
        role: "toolResult",
        toolCallId: "c1",
        toolName: "bash",
        content: [{ type: "text", text: "original" }],
        isError: false,
        timestamp: 1,
      },
    ];
    const redacted: Message[] = [{ role: "tool", parts: [] }];
    const ok = applyRedactedText(pi, redacted);
    expect(ok).toBe(true);
    expect((pi[0] as PiToolResult).content[0]).toMatchObject({
      type: "text",
      text: "original",
    });
  });
});

describe("mapPiUsage", () => {
  describe.each(CAPTURE_MODES)("cache detail in %s", (contentCapture) => {
    it.each([
      0,
      3,
      ...INVALID_USAGE_VALUES,
    ])("keeps only valid cacheWrite1h as non-additive metadata: %j", (value) => {
      const msg = makeMsg();
      Object.assign(msg.usage, { cacheWrite1h: value });
      const ordinary = mapGenerationResult(msg, [], contentCapture);
      const summary = mapSummaryGenerationResult({
        entry: { usage: msg.usage },
        contentCapture,
        completedAt: msg.timestamp,
      });
      const valid =
        typeof value === "number" && Number.isFinite(value) && value >= 0;
      const expected = {
        cost_usd: 0.012,
        ...(valid ? { "pi.usage.cache_write_1h_input_tokens": value } : {}),
      };
      expect(ordinary.metadata).toEqual(expected);
      expect(summary.metadata).toEqual(expected);
      expect(ordinary.usage).toEqual({
        inputTokens: 100,
        outputTokens: 50,
        totalTokens: 165,
        cacheReadInputTokens: 10,
        cacheWriteInputTokens: 5,
      });
      expect(summary.usage).toEqual(ordinary.usage);
      expect(ordinary.responseModel).toBe(msg.model);
      expect(summary.responseModel).toBeUndefined();
    });

    it("does not turn a cache subset into summary totals or infer tool totals", () => {
      const summary = mapSummaryGenerationResult({
        entry: { usage: { cacheWrite1h: 7 } },
        contentCapture,
        completedAt: 0,
      });
      expect(summary.usage).toBeUndefined();
      expect(summary.metadata).toEqual({
        "pi.usage.cache_write_1h_input_tokens": 7,
      });
      const msg = makeMsg();
      delete msg.usage.cost;
      const result = mapGenerationResult(
        msg,
        [
          makeToolResult({
            usage: { cacheWrite1h: 7, cost: { total: 0.5 } },
          }),
        ],
        contentCapture,
      );
      expect(result.metadata).toEqual({
        "pi.tool_usage.cache_write_1h_input_tokens": 7,
        "pi.tool_usage.cost_usd": 0.5,
      });
      expect(result.usage).toEqual(mapPiUsage(msg.usage));
    });
  });

  it.each([
    {
      name: "full usage block with positive reasoning",
      usage: {
        input: 120000,
        output: 8000,
        reasoning: 7,
        cacheRead: 40,
        cacheWrite: 12,
        cacheWrite1h: 9,
        totalTokens: 128052,
        cost: { total: 2.5 },
      },
      want: {
        inputTokens: 120000,
        outputTokens: 8000,
        totalTokens: 128052,
        cacheReadInputTokens: 40,
        cacheWriteInputTokens: 12,
        reasoningTokens: 7,
      },
    },
    {
      name: "missing optional fields default to 0",
      usage: { input: 10, output: 5 },
      want: {
        inputTokens: 10,
        outputTokens: 5,
        totalTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
    },
    {
      name: "explicit zero reasoning without cost",
      usage: {
        input: 1,
        output: 2,
        reasoning: 0,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 3,
      },
      want: {
        inputTokens: 1,
        outputTokens: 2,
        totalTokens: 3,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
        reasoningTokens: 0,
      },
    },
    {
      name: "empty usage maps to all zeros",
      usage: {},
      want: {
        inputTokens: 0,
        outputTokens: 0,
        totalTokens: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
      },
    },
  ])("maps $name", ({ usage, want }) => {
    expect(mapPiUsage(usage)).toEqual(want);
  });

  it.each([
    undefined,
    0,
    7,
  ])("matches the turn path with reasoning=%s", (reasoning) => {
    // Guard against drift: mapGenerationResult must stay byte-identical to
    // mapPiUsage so turn and summary generations report tokens the same way.
    const msg = makeMsg();
    if (reasoning !== undefined) Object.assign(msg.usage, { reasoning });
    const usage = mapGenerationResult(msg, [], "metadata_only").usage;
    expect(usage).toEqual(mapPiUsage(msg.usage));
    expect(usage?.reasoningTokens).toBe(reasoning);
    expect(Object.hasOwn(usage!, "reasoningTokens")).toBe(
      reasoning !== undefined,
    );
  });
});

function makeSummaryEntry(
  overrides?: Partial<PiSummaryEntryLike>,
): PiSummaryEntryLike {
  return {
    id: "c1",
    timestamp: "2025-01-01T00:00:05.000Z",
    summary: "Earlier we refactored the exporter and fixed two tests.",
    tokensBefore: 152000,
    usage: {
      input: 120000,
      output: 8000,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 128000,
      cost: { total: 2.5 },
    },
    ...overrides,
  };
}

describe("mapSummaryGenerationStart", () => {
  const baseOpts = {
    kind: "compaction" as const,
    conversationId: "conv-1",
    conversationTitle: "refactor the exporter",
    agentName: "pi",
    agentVersion: "0.82.1",
    model: { provider: "anthropic", name: "claude-sonnet-4" },
    startedAt: 1_700_000_000_000,
  };

  it("maps conversation, agent, model and timing fields", () => {
    const start = mapSummaryGenerationStart(baseOpts);
    expect(start.conversationId).toBe("conv-1");
    expect(start.conversationTitle).toBe("refactor the exporter");
    expect(start.agentName).toBe("pi");
    expect(start.agentVersion).toBe("0.82.1");
    expect(start.effectiveVersion).toBe("0.82.1");
    expect(start.model).toEqual({ provider: "unknown", name: "unknown" });
    expect(start.metadata).toMatchObject({
      "pi.summary.model_attribution": "unknown",
      "pi.summary.operation_aggregate": true,
      "pi.summary.selected_model": "claude-sonnet-4",
      "pi.summary.selected_provider": "anthropic",
    });
    expect(start.startedAt).toEqual(new Date(1_700_000_000_000));
  });

  it("does not set operationName so the SYNC default generateText applies", () => {
    expect(mapSummaryGenerationStart(baseOpts).operationName).toBeUndefined();
  });

  it("omits conversationTitle when it is unavailable", () => {
    const start = mapSummaryGenerationStart({
      ...baseOpts,
      conversationTitle: undefined,
    });
    expect("conversationTitle" in start).toBe(false);
  });

  it("adds pi.call_kind on top of the built-in tags", () => {
    const start = mapSummaryGenerationStart({
      ...baseOpts,
      tags: { cwd: "/repo", "git.branch": "main" },
    });
    expect(start.tags).toEqual({
      cwd: "/repo",
      "git.branch": "main",
      "pi.call_kind": "compaction",
    });
  });

  it.each(["compaction", "branch_summary"] as const)("tags kind %s", (kind) => {
    const start = mapSummaryGenerationStart({ ...baseOpts, kind });
    expect(start.tags?.["pi.call_kind"]).toBe(kind);
  });

  it("emits pi.call_kind even when there are no built-in tags", () => {
    expect(
      mapSummaryGenerationStart({ ...baseOpts, tags: undefined }).tags,
    ).toEqual({ "pi.call_kind": "compaction" });
  });

  it("sets the deterministic id and parent lineage when supplied", () => {
    const start = mapSummaryGenerationStart({
      ...baseOpts,
      generationId: "pi-abc",
      parentGenerationIds: ["pi-def"],
    });
    expect(start.id).toBe("pi-abc");
    expect(start.parentGenerationIds).toEqual(["pi-def"]);
  });

  it("omits id and parentGenerationIds when lineage is unavailable", () => {
    const start = mapSummaryGenerationStart({
      ...baseOpts,
      generationId: undefined,
      parentGenerationIds: [],
    });
    expect(start.id).toBeUndefined();
    expect(start.parentGenerationIds).toBeUndefined();
  });

  it("copies supplied metadata, e.g. a fork's trunk link", () => {
    const metadata = { "pi.fork.parent_session_id": "trunk-conv" };
    const start = mapSummaryGenerationStart({ ...baseOpts, metadata });
    expect(start.metadata).toMatchObject(metadata);
    expect(start.metadata).not.toBe(metadata);
  });

  it.each([
    undefined,
    { provider: "virtual", name: "router" },
  ])("retains selected identity separately without physical attribution: %s", (model) => {
    const start = mapSummaryGenerationStart({ ...baseOpts, model });
    expect(start.model).toEqual({ provider: "unknown", name: "unknown" });
    expect(start.metadata).toEqual({
      "pi.summary.model_attribution": "unknown",
      "pi.summary.operation_aggregate": true,
      ...(model
        ? {
            "pi.summary.selected_model": model.name,
            "pi.summary.selected_provider": model.provider,
          }
        : {}),
    });
  });
});

describe("mapSummaryGenerationResult", () => {
  const completedAt = 1_700_000_005_000;

  it("maps usage, stop reason, completion time and metadata", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry(),
      contentCapture: "full",
      completedAt,
      reason: "threshold",
      willRetry: false,
    });
    expect(result.usage).toEqual({
      inputTokens: 120000,
      outputTokens: 8000,
      totalTokens: 128000,
      cacheReadInputTokens: 0,
      cacheWriteInputTokens: 0,
    });
    expect(result.stopReason).toBe("end_turn");
    expect(result.completedAt).toEqual(new Date(completedAt));
    expect(result.metadata).toEqual({
      cost_usd: 2.5,
      "pi.tokens_before": 152000,
      "pi.compaction.reason": "threshold",
      "pi.compaction.will_retry": false,
    });
  });

  it.each(
    CAPTURE_MODES,
  )("preserves summary capture and unknown model identity in %s", (contentCapture) => {
    const entry = makeSummaryEntry();
    entry.usage!.cacheWrite1h = 0;
    const result = mapSummaryGenerationResult({
      entry,
      contentCapture,
      completedAt,
    });
    expect(result.output).toEqual(
      contentCapture === "metadata_only"
        ? undefined
        : [
            {
              role: "assistant",
              parts: [
                {
                  type: "text",
                  text: "Earlier we refactored the exporter and fixed two tests.",
                },
              ],
            },
          ],
    );
    expect(result.usage).toEqual(mapPiUsage(entry.usage!));
    expect(result.metadata).toEqual({
      cost_usd: 2.5,
      "pi.tokens_before": 152000,
      "pi.usage.cache_write_1h_input_tokens": 0,
    });
    expect(result.responseModel).toBeUndefined();
  });

  it("drops the summary text in metadata_only but keeps usage and metadata", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry(),
      contentCapture: "metadata_only",
      completedAt,
    });
    expect(result.output).toBeUndefined();
    expect(result.usage).toBeDefined();
    expect(result.metadata?.["pi.tokens_before"]).toBe(152000);
    // The summary must not be smuggled through metadata: content capture
    // never strips metadata or tags.
    expect(JSON.stringify(result)).not.toContain("refactored the exporter");
  });

  it("omits output for an empty or whitespace-only summary", () => {
    expect(
      mapSummaryGenerationResult({
        entry: makeSummaryEntry({ summary: "   " }),
        contentCapture: "full",
        completedAt,
      }).output,
    ).toBeUndefined();
    expect(
      mapSummaryGenerationResult({
        entry: makeSummaryEntry({ summary: undefined }),
        contentCapture: "full",
        completedAt,
      }).output,
    ).toBeUndefined();
  });

  it("omits usage entirely when the entry has none", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry({ usage: undefined }),
      contentCapture: "full",
      completedAt,
    });
    expect(result.usage).toBeUndefined();
    // tokensBefore is a context estimate, not this call's input tokens, so it
    // must not be substituted for the missing usage.
    expect(result.metadata?.["pi.tokens_before"]).toBe(152000);
    expect(result.metadata?.cost_usd).toBeUndefined();
  });

  it("omits usage when the entry reports a cost but no token counts", () => {
    // Mapping this would export five zeros, which reads as "no tokens used"
    // instead of "the host did not report tokens". The cost still stands on
    // its own.
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry({ usage: { cost: { total: 1.75 } } }),
      contentCapture: "full",
      completedAt,
    });
    expect(result.usage).toBeUndefined();
    expect(result.metadata?.cost_usd).toBe(1.75);
  });

  it("records a zero cost, like the turn path does", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry({
        usage: { input: 1, output: 2, totalTokens: 3, cost: { total: 0 } },
      }),
      contentCapture: "full",
      completedAt,
    });
    expect(result.metadata?.cost_usd).toBe(0);
  });

  it("omits cost_usd when pi priced nothing", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry({
        usage: { input: 1, output: 2, totalTokens: 3, cost: {} },
      }),
      contentCapture: "full",
      completedAt,
    });
    expect(result.metadata).not.toHaveProperty("cost_usd");
  });

  it("omits compaction-only metadata for a branch summary", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry({ tokensBefore: undefined }),
      contentCapture: "full",
      completedAt,
    });
    expect(result.metadata).toEqual({ cost_usd: 2.5 });
  });

  it("records will_retry=true for overflow recovery", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry(),
      contentCapture: "full",
      completedAt,
      reason: "overflow",
      willRetry: true,
    });
    expect(result.metadata?.["pi.compaction.reason"]).toBe("overflow");
    expect(result.metadata?.["pi.compaction.will_retry"]).toBe(true);
  });

  it("omits responseModel because summary entries lack physical identity", () => {
    const result = mapSummaryGenerationResult({
      entry: makeSummaryEntry(),
      contentCapture: "full",
      completedAt,
    });
    expect(result.responseModel).toBeUndefined();
  });
});

describe("guard redaction spans", () => {
  it("finds what the guard removed around each token", () => {
    expect(
      removedSpans(
        "Mail jane@example.com and bob@example.com today",
        "Mail [REDACTED] and [REDACTED] today",
      ),
    ).toEqual(["jane@example.com", "bob@example.com"]);
  });

  it("treats text changed some other way, including to empty, as removed whole", () => {
    expect(removedSpans("Project Falcon update", "")).toEqual([
      "Project Falcon update",
    ]);
    expect(removedSpans("Project Falcon", "something else")).toEqual([
      "Project Falcon",
    ]);
    expect(removedSpans("same", "same")).toEqual([]);
  });

  it("keeps only the text around an earlier token", () => {
    // A second guard redacted across the first one's token.
    expect(removedSpans("a [REDACTED] b secret c", "a [REDACTED] c")).toEqual([
      "b secret",
    ]);
  });

  it("redacts every occurrence, merging overlaps whatever the order", () => {
    const value = "Falcon Project Falcon";
    expect(redactSpans(value, ["Falcon", "Project Falcon"])).toBe(
      "[REDACTED] [REDACTED]",
    );
    expect(redactSpans(value, ["Project Falcon", "Falcon"])).toBe(
      "[REDACTED] [REDACTED]",
    );
    expect(redactSpans("abcd", ["abc", "bcd"])).toBe("[REDACTED]");
    expect(redactSpans("nothing here", ["Falcon"])).toBe("nothing here");
  });

  it("redacts the words a title shares with a wholly redacted prompt", () => {
    const prompt = "Status update on Project Falcon for the board";
    expect(redactSharedWords("Project Falcon, quarterly", [prompt])).toBe(
      "[REDACTED] [REDACTED], quarterly",
    );
    // Short words and the token itself are left alone.
    expect(redactSharedWords("on the [REDACTED] list", [prompt])).toBe(
      "on the [REDACTED] list",
    );
    expect(redactSharedWords("Project Falcon", [])).toBe("Project Falcon");
  });

  it("records a redaction to empty text", () => {
    const user = (text: string) => ({
      role: "user",
      parts: [{ type: "text" as const, text }],
    });
    expect([
      ...userTextRedactions([user("Project Falcon")], [user("")]),
    ]).toEqual([["Project Falcon", ""]]);
  });
});
