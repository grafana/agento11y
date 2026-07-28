import type { Message } from "@grafana/agento11y";
import type { AssistantMessage, Part } from "@opencode-ai/sdk";
import { describe, expect, it } from "vitest";
import { createRedactor } from "./hooks.js";
import {
  applyRedactedText,
  legacyToolOverrideNames,
  mapGeneration,
  mapInputMessages,
  mapOutgoingMessagesForHook,
  mapOutputMessages,
  mapToolDefinitions,
  type OutgoingMessage,
  partTextKey,
} from "./mappers.js";

const redactor = createRedactor();

function makeAssistantMsg(
  overrides?: Partial<AssistantMessage>,
): AssistantMessage {
  return {
    id: "msg-1",
    sessionID: "sess-1",
    role: "assistant",
    parentID: "parent-1",
    modelID: "claude-opus-4-20250514",
    providerID: "anthropic",
    mode: "code",
    path: { cwd: "/tmp", root: "/tmp" },
    cost: 0.01,
    tokens: {
      input: 100,
      output: 50,
      reasoning: 10,
      cache: { read: 5, write: 3 },
    },
    time: { created: Date.now(), completed: Date.now() + 1000 },
    finish: "end_turn",
    ...overrides,
  } as AssistantMessage;
}

describe("mapInputMessages", () => {
  it("maps TextParts to agento11y user messages", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "hello world",
      },
    ] as Part[];
    const result = mapInputMessages(parts);
    expect(result).toHaveLength(1);
    expect(result[0].role).toBe("user");
    expect(result[0].parts?.[0]).toEqual({ type: "text", text: "hello world" });
  });

  it("skips non-text parts", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "file" as const,
        mime: "image/png",
        url: "...",
      },
    ] as Part[];
    expect(mapInputMessages(parts)).toHaveLength(0);
  });

  it("skips text parts with empty or whitespace-only text", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "",
      },
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "   ",
      },
      {
        id: "p3",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "\n\t",
      },
      {
        id: "p4",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "hello",
      },
    ] as Part[];
    const result = mapInputMessages(parts);
    expect(result).toHaveLength(1);
    expect(result[0].parts?.[0]).toEqual({ type: "text", text: "hello" });
  });

  it("omits input bodies in metadata_only mode", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "hello world",
      },
    ] as Part[];

    expect(mapInputMessages(parts, "metadata_only")).toEqual([]);
  });
});

describe("mapOutputMessages", () => {
  it("maps TextParts with lightweight redaction", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "The result is 42",
      },
    ] as Part[];
    const result = mapOutputMessages(parts, redactor);
    expect(result).toHaveLength(1);
    expect(result[0].role).toBe("assistant");
    expect(result[0].parts?.[0]).toEqual({
      type: "text",
      text: "The result is 42",
    });
  });

  it("redacts secrets in tool output but not in assistant text (lightweight)", () => {
    const secretToken = "glc_abcdefghijklmnopqrstuvwxyz1234";
    const textParts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: `Found token: ${secretToken}`,
      },
    ] as Part[];
    const result = mapOutputMessages(textParts, redactor);
    // Tier 1 patterns fire even in lightweight mode
    expect(result[0].parts?.[0]).toHaveProperty("type", "text");
    const textContent = (result[0].parts?.[0] as any).text;
    expect(textContent).not.toContain(secretToken);
    expect(textContent).toContain("[REDACTED:");
  });

  it("maps completed ToolParts to tool_call + tool_result with full redaction", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "tool" as const,
        callID: "call-1",
        tool: "bash",
        state: {
          status: "completed" as const,
          input: { command: "echo test" },
          output: "test output",
          title: "Run bash",
          metadata: {},
          time: { start: 1000, end: 2000 },
        },
      },
    ] as Part[];
    const result = mapOutputMessages(parts, redactor);
    expect(result).toHaveLength(2);
    expect(result[0].role).toBe("assistant");
    expect(result[0].parts?.[0].type).toBe("tool_call");
    const toolCall = (result[0].parts?.[0] as any).toolCall;
    expect(toolCall.inputJSON).toBe('{"command":"echo test"}');
    expect(result[1].role).toBe("tool");
    expect(result[1].parts?.[0].type).toBe("tool_result");
    const toolResult = (result[1].parts?.[0] as any).toolResult;
    expect(toolResult.content).toBe("test output");
  });

  it("keeps message text but omits tool bodies in no_tool_content mode", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "assistant reply",
      },
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m1",
        type: "tool" as const,
        callID: "call-1",
        tool: "bash",
        state: {
          status: "completed" as const,
          input: { command: "echo test" },
          output: "test output",
          title: "Run bash",
          metadata: {},
          time: { start: 1000, end: 2000 },
        },
      },
    ] as Part[];

    const result = mapOutputMessages(parts, redactor, "no_tool_content");

    expect(result[0].parts?.[0]).toEqual({
      type: "text",
      text: "assistant reply",
    });
    expect((result[1].parts?.[0] as any).toolCall.inputJSON).toBe("");
    expect((result[2].parts?.[0] as any).toolResult.content).toBe("");
  });

  it("omits text and tool bodies in metadata_only mode", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "assistant reply",
      },
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m1",
        type: "tool" as const,
        callID: "call-1",
        tool: "bash",
        state: {
          status: "completed" as const,
          input: { command: "echo test" },
          output: "test output",
          title: "Run bash",
          metadata: {},
          time: { start: 1000, end: 2000 },
        },
      },
    ] as Part[];

    const result = mapOutputMessages(parts, redactor, "metadata_only");

    expect(result).toHaveLength(2);
    expect((result[0].parts?.[0] as any).toolCall.inputJSON).toBe("");
    expect((result[1].parts?.[0] as any).toolResult.content).toBe("");
  });

  it("skips text parts with empty or whitespace-only text", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "",
      },
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "   ",
      },
      {
        id: "p3",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "actual content",
      },
    ] as Part[];
    const result = mapOutputMessages(parts, redactor);
    expect(result).toHaveLength(1);
    expect(result[0].parts?.[0]).toEqual({
      type: "text",
      text: "actual content",
    });
  });

  it("skips reasoning parts with empty or whitespace-only text", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "reasoning" as const,
        text: "",
        time: { start: 1000 },
      },
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m1",
        type: "reasoning" as const,
        text: "  ",
        time: { start: 1000 },
      },
      {
        id: "p3",
        sessionID: "s1",
        messageID: "m1",
        type: "reasoning" as const,
        text: "thinking about it",
        time: { start: 1000 },
      },
    ] as Part[];
    const result = mapOutputMessages(parts, redactor);
    expect(result).toHaveLength(1);
    expect(result[0].parts?.[0]).toEqual({
      type: "thinking",
      thinking: "thinking about it",
    });
  });

  it("maps error ToolParts to tool_call + tool_result with is_error flag", () => {
    const parts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "tool" as const,
        callID: "call-1",
        tool: "bash",
        state: {
          status: "error" as const,
          input: { command: "fail" },
          error: "command failed",
          metadata: {},
          time: { start: 1000, end: 2000 },
        },
      },
    ] as Part[];
    const result = mapOutputMessages(parts, redactor);
    expect(result).toHaveLength(2);
    expect(result[0].role).toBe("assistant");
    expect(result[0].parts?.[0].type).toBe("tool_call");
    const toolCall = (result[0].parts?.[0] as any).toolCall;
    expect(toolCall.id).toBe("call-1");
    expect(toolCall.name).toBe("bash");
    expect(result[1].role).toBe("tool");
    expect(result[1].parts?.[0].type).toBe("tool_result");
    const toolResult = (result[1].parts?.[0] as any).toolResult;
    expect(toolResult.toolCallId).toBe("call-1");
    expect(toolResult.isError).toBe(true);
    expect(toolResult.content).toBe("command failed");
  });
});

describe("legacyToolOverrideNames", () => {
  it("returns enabled names and drops disabled ones", () => {
    expect(
      legacyToolOverrideNames({ bash: true, read: true, write: false }),
    ).toEqual(["bash", "read"]);
  });

  it("returns empty array for undefined", () => {
    expect(legacyToolOverrideNames(undefined)).toEqual([]);
  });
});

describe("mapToolDefinitions", () => {
  it("builds sorted name-only function definitions", () => {
    expect(mapToolDefinitions(["write", "bash", "read"])).toEqual([
      { name: "bash", type: "function" },
      { name: "read", type: "function" },
      { name: "write", type: "function" },
    ]);
  });

  it("deduplicates names", () => {
    expect(mapToolDefinitions(["bash", "bash", "read"])).toEqual([
      { name: "bash", type: "function" },
      { name: "read", type: "function" },
    ]);
  });

  it("skips empty and non-string names", () => {
    expect(mapToolDefinitions(["", "bash", 42 as unknown as string])).toEqual([
      { name: "bash", type: "function" },
    ]);
  });

  it("returns no definitions for no names", () => {
    expect(mapToolDefinitions([])).toEqual([]);
  });
});

describe("mapGeneration", () => {
  it("maps usage tokens and cost from assistant message", () => {
    const msg = makeAssistantMsg();
    const userParts = [
      {
        id: "p1",
        sessionID: "s1",
        messageID: "m1",
        type: "text" as const,
        text: "hello",
      },
    ] as Part[];
    const assistantParts = [
      {
        id: "p2",
        sessionID: "s1",
        messageID: "m2",
        type: "text" as const,
        text: "hi there",
      },
    ] as Part[];
    const result = mapGeneration(msg, userParts, assistantParts, redactor);
    expect(result.input).toHaveLength(1);
    expect(result.output).toHaveLength(1);
    expect(result.usage?.inputTokens).toBe(100);
    expect(result.metadata?.cost).toBe(0.01);
  });

  it("maps response model, stop reason, and completion timestamp from assistant message", () => {
    const msg = makeAssistantMsg();
    const result = mapGeneration(msg, [], [], redactor);
    expect(result.responseModel).toBe("claude-opus-4-20250514");
    expect(result.stopReason).toBe("end_turn");
    expect(result.completedAt).toBeInstanceOf(Date);
  });

  it("prefers accumulated step tokens over the message's last-step tokens", () => {
    // msg.cost covers every step; msg.tokens only the last one.
    const msg = makeAssistantMsg({ cost: 0.06 });
    const result = mapGeneration(msg, [], [], redactor, "full", {
      input: 310,
      output: 60,
      reasoning: 15,
      cache: { read: 70, write: 14 },
    });
    expect(result.usage).toEqual({
      inputTokens: 310,
      outputTokens: 60,
      reasoningTokens: 15,
      cacheReadInputTokens: 70,
      cacheWriteInputTokens: 14,
    });
    expect(result.metadata?.cost).toBe(0.06);
  });

  it("keeps input cache-adjusted and reasoning out of output", () => {
    // opencode's getUsage subtracts cache read/write from input and reasoning
    // from output, so these pass through as reported: 80, not 80 + 20 + 10, and
    // 25, not 25 + 7.
    const result = mapGeneration(makeAssistantMsg(), [], [], redactor, "full", {
      input: 80,
      output: 25,
      reasoning: 7,
      cache: { read: 20, write: 10 },
    });
    expect(result.usage).toEqual({
      inputTokens: 80,
      outputTokens: 25,
      reasoningTokens: 7,
      cacheReadInputTokens: 20,
      cacheWriteInputTokens: 10,
    });
  });
});

function textPart(overrides: Partial<Part> & { text: string }): Part {
  return {
    id: "p1",
    sessionID: "s1",
    messageID: "m1",
    type: "text",
    ...overrides,
  } as Part;
}

function outgoing(
  role: string,
  parts: Part[],
  id = "m1",
): { info: { id: string; role: string; sessionID: string }; parts: Part[] } {
  return { info: { id, role, sessionID: "s1" }, parts };
}

describe("mapOutgoingMessagesForHook", () => {
  const cases: Array<{
    name: string;
    input: OutgoingMessage[];
    expected: Message[];
  }> = [
    {
      name: "user text",
      input: [outgoing("user", [textPart({ text: "authorization=abc" })])],
      expected: [
        { role: "user", parts: [{ type: "text", text: "authorization=abc" }] },
      ],
    },
    {
      name: "joins multiple text parts of one message",
      input: [
        outgoing("user", [
          textPart({ text: "first" }),
          textPart({ id: "p2", text: "second" }),
        ]),
      ],
      expected: [
        { role: "user", parts: [{ type: "text", text: "first\nsecond" }] },
      ],
    },
    {
      name: "assistant text",
      input: [outgoing("assistant", [textPart({ text: "sure" })])],
      expected: [
        { role: "assistant", parts: [{ type: "text", text: "sure" }] },
      ],
    },
    {
      name: "skips ignored user text but keeps the slot",
      input: [
        outgoing("user", [
          textPart({ text: "reminder", ignored: true } as any),
        ]),
      ],
      expected: [{ role: "user", parts: [] }],
    },
    {
      name: "skips an empty assistant text part, opencode's signed-reasoning separator",
      input: [
        outgoing("assistant", [
          textPart({ text: "" }),
          textPart({ id: "p2", text: "done" }),
        ]),
      ],
      expected: [
        { role: "assistant", parts: [{ type: "text", text: "done" }] },
      ],
    },
    {
      name: "skips reasoning parts because their provider signature must round-trip",
      input: [
        outgoing("assistant", [
          {
            id: "p1",
            sessionID: "s1",
            messageID: "m1",
            type: "reasoning",
            text: "thinking about secrets",
            time: { start: 1 },
            metadata: { anthropic: { signature: "sig" } },
          } as Part,
        ]),
      ],
      expected: [{ role: "assistant", parts: [] }],
    },
    {
      name: "skips tool-call and tool-result content, emitting a placeholder",
      input: [
        outgoing("assistant", [
          {
            id: "p1",
            sessionID: "s1",
            messageID: "m1",
            type: "tool",
            callID: "call-1",
            tool: "bash",
            state: {
              status: "completed",
              input: { command: "echo secret" },
              output: "secret output",
              title: "bash",
              metadata: {},
              time: { start: 1, end: 2 },
            },
          } as Part,
        ]),
      ],
      expected: [{ role: "assistant", parts: [] }],
    },
    {
      name: "emits a placeholder for a null slot",
      input: [null as unknown as OutgoingMessage],
      expected: [{ role: "unknown", parts: [] }],
    },
    {
      name: "emits a placeholder for a slot with no role",
      input: [{ parts: [textPart({ text: "orphan" })] }],
      expected: [{ role: "unknown", parts: [] }],
    },
    {
      name: "emits a placeholder for a message with no parts",
      input: [{ info: { id: "m1", role: "user", sessionID: "s1" } }],
      expected: [{ role: "user", parts: [] }],
    },
    {
      name: "preserves one slot per entry in order",
      input: [
        outgoing("user", [textPart({ text: "one" })], "m1"),
        outgoing("assistant", [], "m2"),
        outgoing("user", [textPart({ text: "three" })], "m3"),
      ],
      expected: [
        { role: "user", parts: [{ type: "text", text: "one" }] },
        { role: "assistant", parts: [] },
        { role: "user", parts: [{ type: "text", text: "three" }] },
      ],
    },
  ];

  it.each(cases)("$name", ({ input, expected }) => {
    expect(mapOutgoingMessagesForHook(input)).toEqual(expected);
  });
});

const toolPart = (messageID: string): Part =>
  ({
    id: "p1",
    sessionID: "s1",
    messageID,
    type: "tool",
    callID: "call-1",
    tool: "bash",
    state: {
      status: "completed",
      input: { command: "echo secret" },
      output: "secret output",
      title: "bash",
      metadata: {},
      time: { start: 1, end: 2 },
    },
  }) as Part;

const reasoningPart = (text: string): Part =>
  ({
    id: "p1",
    sessionID: "s1",
    messageID: "m1",
    type: "reasoning",
    text,
    time: { start: 1 },
    metadata: { anthropic: { signature: "sig" } },
  }) as Part;

/** The text of every part, in order. `undefined` for a part with no text. */
function partTexts(
  messages: OutgoingMessage[],
): Array<Array<string | undefined>> {
  return messages.map((msg) =>
    (msg?.parts ?? []).map((part) => (part as { text?: string }).text),
  );
}

describe("applyRedactedText", () => {
  type ApplyCase = {
    name: string;
    /** Built per case: the function writes into the array it is given. */
    build: () => { messages: OutgoingMessage[]; pinned?: Part };
    redacted: Message[];
    /** Rewritten message count, or null when the transform was discarded. */
    want: number | null;
    /** New text per part key. Only checked when a case sets it. */
    wantTextByPart?: Record<string, string>;
    wantTexts: Array<Array<string | undefined>>;
    assert?: (args: { messages: OutgoingMessage[]; pinned?: Part }) => void;
  };

  const cases: ApplyCase[] = [
    {
      name: "rewrites aligned text",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[REDACTED]" }] },
      ],
      want: 1,
      wantTextByPart: { [partTextKey("s1", "p1")]: "token=[REDACTED]" },
      wantTexts: [["token=[REDACTED]"]],
    },
    {
      name: "accepts the content shorthand",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [{ role: "user", content: "token=[REDACTED]" }],
      want: 1,
      wantTexts: [["token=[REDACTED]"]],
    },
    {
      // The common case on a partial match: the server echoes every forwarded
      // message back and only the matched substrings differ. Rewriting an
      // unchanged entry would collapse it into its first slot on every step.
      name: "reports zero rewrites when the text came back unchanged",
      build: () => ({
        messages: [
          outgoing("user", [
            textPart({ text: "keep" }),
            textPart({ id: "p2", text: "also keep" }),
          ]),
        ],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "keep\nalso keep" }] },
      ],
      want: 0,
      wantTexts: [["keep", "also keep"]],
    },
    {
      name: "counts only the messages the server changed",
      build: () => ({
        messages: [
          outgoing("user", [textPart({ text: "token=alpha" })], "m1"),
          outgoing("assistant", [textPart({ id: "p2", text: "sure" })], "m2"),
        ],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
        { role: "assistant", parts: [{ type: "text", text: "sure" }] },
      ],
      want: 1,
      wantTexts: [["token=[R]"], ["sure"]],
    },
    {
      name: "collapses multiple text parts into the first slot",
      build: () => ({
        messages: [
          outgoing("user", [
            textPart({ text: "keep" }),
            textPart({ id: "p2", text: "token=alpha" }),
          ]),
        ],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "keep\ntoken=[R]" }] },
      ],
      want: 1,
      // The emptied slot is reported too: an export that replayed only the
      // first would repeat the text the collapse removed.
      wantTextByPart: {
        [partTextKey("s1", "p1")]: "keep\ntoken=[R]",
        [partTextKey("s1", "p2")]: "",
      },
      wantTexts: [["keep\ntoken=[R]", ""]],
    },
    {
      // Writing "" back would delete content rather than redact it: opencode
      // drops an empty user text part, then drops the message once no part
      // survives, which would change the outgoing message count.
      name: "keeps the text when the server stripped it to nothing",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [{ role: "user", parts: [{ type: "text", text: "" }] }],
      want: 0,
      wantTexts: [["token=alpha"]],
    },
    {
      name: "keeps the text when the content shorthand is empty",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [{ role: "user", content: "" }],
      want: 0,
      wantTexts: [["token=alpha"]],
    },
    {
      name: "keeps the text when the server returned no text for the slot",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [{ role: "user", parts: [] }],
      want: 0,
      wantTexts: [["token=alpha"]],
    },
    {
      name: "discards the transform when the server sent fewer messages",
      build: () => ({
        messages: [
          outgoing("user", [textPart({ text: "token=alpha" })], "m1"),
          outgoing("user", [textPart({ text: "token=beta" })], "m2"),
        ],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
      ],
      want: null,
      wantTexts: [["token=alpha"], ["token=beta"]],
    },
    {
      name: "discards the transform when the server sent extra messages",
      build: () => ({
        messages: [outgoing("user", [textPart({ text: "token=alpha" })])],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
        { role: "user", parts: [{ type: "text", text: "extra" }] },
      ],
      want: null,
      wantTexts: [["token=alpha"]],
    },
    {
      name: "discards the transform when a later slot is missing",
      build: () => ({
        messages: [
          outgoing("user", [textPart({ text: "token=alpha" })], "m1"),
          outgoing("user", [textPart({ id: "p2", text: "token=beta" })], "m2"),
        ],
      }),
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
        undefined as unknown as Message,
      ],
      want: null,
      wantTexts: [["token=alpha"], ["token=beta"]],
    },
    {
      name: "leaves a tool part untouched while rewriting eligible slots",
      build: () => {
        const pinned = toolPart("m2");
        return {
          pinned,
          messages: [
            outgoing("user", [textPart({ text: "token=alpha" })], "m1"),
            outgoing("assistant", [pinned], "m2"),
            outgoing("user", [textPart({ text: "token=gamma" })], "m3"),
          ],
        };
      },
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R1]" }] },
        { role: "assistant", parts: [{ type: "text", text: "ignored" }] },
        { role: "user", parts: [{ type: "text", text: "token=[R3]" }] },
      ],
      want: 2,
      wantTexts: [["token=[R1]"], [undefined], ["token=[R3]"]],
      assert: ({ messages, pinned }) => {
        expect(messages[1]?.parts?.[0]).toBe(pinned);
        expect((pinned as any).state.output).toBe("secret output");
      },
    },
    {
      name: "leaves reasoning parts untouched",
      build: () => ({
        messages: [
          outgoing("assistant", [
            reasoningPart("token=alpha"),
            textPart({ id: "p2", text: "done" }),
          ]),
        ],
      }),
      redacted: [{ role: "assistant", parts: [{ type: "text", text: "[R]" }] }],
      want: 1,
      wantTexts: [["token=alpha", "[R]"]],
      assert: ({ messages }) => {
        expect((messages[0]?.parts?.[0] as any).metadata).toEqual({
          anthropic: { signature: "sig" },
        });
      },
    },
    {
      // Whether opencode freezes this hook output is version-dependent (it does
      // for `output.args` on >=1.14), so the write-back must not rely on
      // in-place mutation succeeding.
      name: "clones instead of mutating a frozen text part",
      build: () => {
        const pinned = Object.freeze(textPart({ text: "token=alpha" }));
        return {
          pinned,
          messages: [
            Object.freeze({
              info: { id: "m1", role: "user", sessionID: "s1" },
              parts: Object.freeze([pinned]) as unknown as Part[],
            }),
          ],
        };
      },
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
      ],
      want: 1,
      wantTexts: [["token=[R]"]],
      assert: ({ pinned }) => {
        expect((pinned as any).text).toBe("token=alpha");
      },
    },
    {
      name: "discards the transform when a frozen part sits in a frozen array",
      build: () => {
        const pinned = Object.freeze(textPart({ text: "token=alpha" }));
        return {
          pinned,
          messages: Object.freeze([
            {
              info: { id: "m1", role: "user", sessionID: "s1" },
              parts: [pinned],
            },
          ]) as unknown as OutgoingMessage[],
        };
      },
      redacted: [
        { role: "user", parts: [{ type: "text", text: "token=[R]" }] },
      ],
      want: null,
      wantTexts: [["token=alpha"]],
    },
  ];

  it.each(cases)("$name", ({
    build,
    redacted,
    want,
    wantTextByPart,
    wantTexts,
    assert,
  }) => {
    const { messages, pinned } = build();
    const ids = messages.map((msg) => msg?.info?.id);

    const applied = applyRedactedText(messages, redacted);

    expect(applied === null ? null : applied.messageCount).toBe(want);
    if (wantTextByPart !== undefined) {
      expect(Object.fromEntries(applied?.textByPart ?? [])).toEqual(
        wantTextByPart,
      );
    }
    expect(partTexts(messages)).toEqual(wantTexts);
    // Message count and order never change, whatever the server returned.
    expect(messages.map((msg) => msg?.info?.id)).toEqual(ids);
    assert?.({ messages, pinned });
  });
});
