import type { AssistantMessage, Part } from "@opencode-ai/sdk";
import { describe, expect, it } from "vitest";
import {
  legacyToolOverrideNames,
  mapGeneration,
  mapInputMessages,
  mapOutputMessages,
  mapToolDefinitions,
} from "./mappers.js";
import { Redactor } from "./redact.js";

const redactor = new Redactor();

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
  it("maps TextParts to Sigil user messages", () => {
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
    // Fresh input excludes cache reads/writes: input(100) - read(5) - write(3) = 92.
    expect(result.usage?.inputTokens).toBe(92);
    expect(result.metadata?.cost).toBe(0.01);
  });

  it("maps response model, stop reason, and completion timestamp from assistant message", () => {
    const msg = makeAssistantMsg();
    const result = mapGeneration(msg, [], [], redactor);
    expect(result.responseModel).toBe("claude-opus-4-20250514");
    expect(result.stopReason).toBe("end_turn");
    expect(result.completedAt).toBeInstanceOf(Date);
  });
});
