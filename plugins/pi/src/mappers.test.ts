import { describe, expect, it } from "vitest";
import {
  extractRequestControls,
  MAX_TITLE_LEN,
  mapGenerationResult,
  mapGenerationStart,
  mapTools,
  mapUserMessage,
  type PiAssistantMessage,
  type PiToolInfo,
  type PiToolResult,
  type PiUserMessage,
  resolveConversationTitle,
  userMessageText,
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

function makeToolInfo(overrides?: Partial<PiToolInfo>): PiToolInfo {
  return {
    name: "bash",
    description: "Run a shell command",
    parameters: {
      type: "object",
      properties: { command: { type: "string" } },
      required: ["command"],
    },
    ...overrides,
  };
}

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

  it("sets systemPrompt when provided and non-empty", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      systemPrompt: "You are a helpful agent.",
    });
    expect(start.systemPrompt).toBe("You are a helpful agent.");
  });

  it("omits systemPrompt when undefined or empty", () => {
    const a = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
    });
    const b = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      systemPrompt: "",
    });
    expect(a.systemPrompt).toBeUndefined();
    expect(b.systemPrompt).toBeUndefined();
  });

  it("sets request controls when provided", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      requestControls: {
        maxTokens: 1024,
        temperature: 0.2,
        topP: 0.9,
        toolChoice: "auto",
        thinkingBudgetTokens: 4096,
      },
    });
    expect(start.maxTokens).toBe(1024);
    expect(start.temperature).toBe(0.2);
    expect(start.topP).toBe(0.9);
    expect(start.toolChoice).toBe("auto");
    expect(start.metadata).toEqual({
      "sigil.gen_ai.request.thinking.budget_tokens": 4096,
    });
  });

  it("leaves request controls unset when fields are missing", () => {
    const start = mapGenerationStart(makeMsg(), {
      conversationId: "s",
      agentName: "pi",
      startedAt: 0,
      requestControls: {},
    });
    expect(start.maxTokens).toBeUndefined();
    expect(start.temperature).toBeUndefined();
    expect(start.topP).toBeUndefined();
    expect(start.toolChoice).toBeUndefined();
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
  it("maps usage and metadata (no content)", () => {
    const msg = makeMsg();
    const result = mapGenerationResult(msg, [], "metadata_only");

    expect(result.usage).toEqual({
      inputTokens: 100,
      outputTokens: 50,
      totalTokens: 165,
      cacheReadInputTokens: 10,
      cacheWriteInputTokens: 5,
    });
    expect(result.responseModel).toBe("claude-sonnet-4-20250514");
    expect(result.stopReason).toBe("end_turn");
    expect(result.completedAt).toEqual(new Date(1700000001000));
    expect(result.metadata?.cost_usd).toBe(0.012);
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

  it("full_with_metadata_spans matches full for proto-export tool bodies", () => {
    // Per the SDK contract (ContentCaptureModeFullWithMetadataSpans in
    // go/sigil/content_capture.go), the proto export gets full generation
    // content under this mode; only the OTel span side is reduced.
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
    const result = mapGenerationResult(
      msg,
      toolResults,
      "full_with_metadata_spans",
    );

    const toolCallPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_call");
    expect(
      (toolCallPart as { toolCall: { inputJSON: string } }).toolCall.inputJSON,
    ).toBe(JSON.stringify({ command: "ls" }));

    const toolResultPart = result.output
      ?.flatMap((m) => m.parts ?? [])
      .find((p) => p.type === "tool_result");
    expect(
      (toolResultPart as { toolResult: { content: string } }).toolResult
        .content,
    ).toBe("file.txt");
  });

  it("mapTools emits descriptions and schemas under full_with_metadata_spans", () => {
    const catalog: PiToolInfo[] = [
      {
        name: "bash",
        description: "Run a shell command",
        parameters: { type: "object" },
      },
    ];
    const defs = mapTools(
      catalog,
      new Set(["bash"]),
      "full_with_metadata_spans",
    );
    expect(defs).toEqual([
      {
        name: "bash",
        description: "Run a shell command",
        inputSchemaJSON: JSON.stringify({ type: "object" }),
      },
    ]);
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
  it("returns name-only under metadata_only", () => {
    const catalog = [
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "read", description: "Read a file" }),
    ];
    const defs = mapTools(catalog, new Set(["bash", "read"]), "metadata_only");
    expect(defs).toEqual([{ name: "bash" }, { name: "read" }]);
  });

  it("returns name-only under no_tool_content", () => {
    const catalog = [makeToolInfo({ name: "bash" })];
    const defs = mapTools(catalog, new Set(["bash"]), "no_tool_content");
    expect(defs).toEqual([{ name: "bash" }]);
  });

  it("returns name+description+inputSchemaJSON under full", () => {
    const catalog = [
      makeToolInfo({
        name: "bash",
        description: "Run a shell command",
        parameters: { type: "object", properties: { cmd: { type: "string" } } },
      }),
    ];
    const defs = mapTools(catalog, new Set(["bash"]), "full");
    expect(defs).toHaveLength(1);
    expect(defs[0]).toEqual({
      name: "bash",
      description: "Run a shell command",
      inputSchemaJSON:
        '{"type":"object","properties":{"cmd":{"type":"string"}}}',
    });
  });

  it("filters by activeNames when set is non-empty", () => {
    const catalog = [
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "read" }),
      makeToolInfo({ name: "write" }),
    ];
    const defs = mapTools(catalog, new Set(["bash", "write"]), "metadata_only");
    expect(defs.map((d) => d.name)).toEqual(["bash", "write"]);
  });

  it("emits the full catalog when activeNames is null (no filter)", () => {
    // null means the active-set API is unavailable (older pi versions);
    // emit the registry so something useful still ships.
    const catalog = [
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "read" }),
    ];
    const defs = mapTools(catalog, null, "metadata_only");
    expect(defs.map((d) => d.name)).toEqual(["bash", "read"]);
  });

  it("emits no tools when activeNames is an empty Set", () => {
    // An empty Set means "no tools offered this turn" — different from null.
    const catalog = [
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "read" }),
    ];
    expect(mapTools(catalog, new Set(), "metadata_only")).toEqual([]);
  });

  it("handles an empty catalog", () => {
    expect(mapTools([], new Set(["bash"]), "full")).toEqual([]);
  });

  it("deduplicates by tool name", () => {
    const catalog = [
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "bash" }),
      makeToolInfo({ name: "read" }),
    ];
    const defs = mapTools(catalog, new Set(["bash", "read"]), "metadata_only");
    expect(defs.map((d) => d.name)).toEqual(["bash", "read"]);
  });

  it("skips description under full when value is empty", () => {
    const catalog = [makeToolInfo({ name: "bash", description: "" })];
    const defs = mapTools(catalog, new Set(["bash"]), "full");
    expect(defs[0]?.description).toBeUndefined();
  });
});

describe("extractRequestControls", () => {
  it("extracts Anthropic-shaped payload with thinking", () => {
    const ctrls = extractRequestControls({
      max_tokens: 4096,
      temperature: 0.2,
      top_p: 0.9,
      tool_choice: { type: "auto" },
      thinking: { type: "enabled", budget_tokens: 2048 },
    });
    expect(ctrls).toEqual({
      maxTokens: 4096,
      temperature: 0.2,
      topP: 0.9,
      toolChoice: "auto",
      thinkingBudgetTokens: 2048,
    });
  });

  it("preserves the forced tool name in Anthropic tool_choice", () => {
    const ctrls = extractRequestControls({
      tool_choice: { type: "tool", name: "search" },
    });
    expect(ctrls.toolChoice).toBe("tool:search");
  });

  it("falls back to type when forced-tool name is missing", () => {
    const ctrls = extractRequestControls({
      tool_choice: { type: "tool" },
    });
    expect(ctrls.toolChoice).toBe("tool");
  });

  it("accepts OpenAI Chat max_tokens", () => {
    const ctrls = extractRequestControls({ max_tokens: 512 });
    expect(ctrls.maxTokens).toBe(512);
  });

  it("accepts OpenAI Chat max_completion_tokens", () => {
    const ctrls = extractRequestControls({ max_completion_tokens: 1024 });
    expect(ctrls.maxTokens).toBe(1024);
  });

  it("accepts OpenAI Responses max_output_tokens", () => {
    const ctrls = extractRequestControls({ max_output_tokens: 2000 });
    expect(ctrls.maxTokens).toBe(2000);
  });

  it("reads Gemini config wrapper (pi's @google/genai SDK shape)", () => {
    // Matches the payload pi's google.js builds: temperature/maxOutputTokens
    // spread into `config`, plus toolConfig and thinkingConfig nests.
    const ctrls = extractRequestControls({
      model: "gemini-2.0-flash",
      contents: [],
      config: {
        temperature: 0.7,
        topP: 0.95,
        maxOutputTokens: 8192,
        toolConfig: { functionCallingConfig: { mode: "AUTO" } },
        thinkingConfig: { thinkingBudget: 1024 },
      },
    });
    expect(ctrls).toEqual({
      maxTokens: 8192,
      temperature: 0.7,
      topP: 0.95,
      toolChoice: "AUTO",
      thinkingBudgetTokens: 1024,
    });
  });

  it("reads the legacy generationConfig nest too", () => {
    const ctrls = extractRequestControls({
      generationConfig: {
        temperature: 0.7,
        topP: 0.95,
        maxOutputTokens: 8192,
      },
    });
    expect(ctrls).toEqual({
      maxTokens: 8192,
      temperature: 0.7,
      topP: 0.95,
    });
  });

  it("prefers top-level fields over Gemini config wrapper", () => {
    // If both shapes appear (shouldn't happen in practice), top-level wins.
    const ctrls = extractRequestControls({
      max_tokens: 256,
      config: { maxOutputTokens: 8192 },
    });
    expect(ctrls.maxTokens).toBe(256);
  });

  it("accepts string tool_choice", () => {
    expect(extractRequestControls({ tool_choice: "required" }).toolChoice).toBe(
      "required",
    );
  });

  it("accepts camelCase toolChoice and topP", () => {
    const ctrls = extractRequestControls({
      toolChoice: "none",
      topP: 0.5,
    });
    expect(ctrls.toolChoice).toBe("none");
    expect(ctrls.topP).toBe(0.5);
  });

  it("returns {} for null", () => {
    expect(extractRequestControls(null)).toEqual({});
  });

  it("returns {} for undefined", () => {
    expect(extractRequestControls(undefined)).toEqual({});
  });

  it("returns {} for a string", () => {
    expect(extractRequestControls("hello")).toEqual({});
  });

  it("returns {} for a number", () => {
    expect(extractRequestControls(42)).toEqual({});
  });

  it("returns {} for an array", () => {
    expect(extractRequestControls([1, 2])).toEqual({});
  });

  it("ignores non-finite numbers", () => {
    const ctrls = extractRequestControls({
      max_tokens: Number.NaN,
      temperature: Number.POSITIVE_INFINITY,
    });
    expect(ctrls).toEqual({});
  });

  it("skips thinking budget when not numeric", () => {
    const ctrls = extractRequestControls({
      thinking: { type: "enabled", budget_tokens: "lots" },
    });
    expect(ctrls.thinkingBudgetTokens).toBeUndefined();
  });
});
