import { describe, expect, it } from "vitest";
import {
  applyGuardTransform,
  projectGuardMessages,
} from "./guardProjection.js";

describe("request-local guard projection", () => {
  const messages = [
    {
      role: "system",
      content: "secret",
      sections: { rules: "secret", removed: null },
      toolsAdded: [
        {
          name: "lookup",
          description: "secret",
          parameters: { type: "object", description: "secret" },
        },
      ],
      toolsRemoved: [{ name: "old" }],
      timestamp: 1,
    },
    {
      role: "user",
      content: [
        { type: "text", text: "secret" },
        { type: "image", data: "opaque" },
      ],
      timestamp: 2,
    },
    {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "opaque", signature: "signed" },
        { type: "text", text: "secret", textSignature: "keep" },
        {
          type: "toolCall",
          id: "c",
          name: "lookup",
          arguments: { query: "secret" },
          thoughtSignature: "keep",
        },
      ],
      timestamp: 3,
    },
    {
      role: "toolResult",
      toolCallId: "c",
      toolName: "lookup",
      content: [{ type: "text", text: "secret" }],
      isError: false,
      details: { keep: "opaque" },
      timestamp: 4,
    },
    {
      role: "custom",
      customType: "x",
      content: "secret",
      display: false,
      timestamp: 5,
    },
    {
      role: "bashExecution",
      command: "secret",
      output: "secret",
      truncated: true,
      fullOutputPath: "secret",
      exitCode: 0,
      timestamp: 6,
    },
    {
      role: "compactionSummary",
      summary: "secret",
      tokensBefore: 42,
      timestamp: 7,
    },
    {
      role: "branchSummary",
      summary: "secret",
      fromId: "branch",
      timestamp: 8,
    },
    {
      role: "bashExecution",
      command: "excluded",
      output: "excluded",
      excludeFromContext: true,
    },
    { role: "future", content: "opaque" },
  ];
  it("transforms supported slots on copies and preserves opaque fields", () => {
    const original = structuredClone(messages);
    const projection = projectGuardMessages(messages);
    expect(projection.messages.map((message) => message.role)).toEqual([
      "system",
      "user",
      "assistant",
      "tool",
      "user",
      "user",
      "user",
      "user",
    ]);
    const transformed = JSON.parse(
      JSON.stringify(projection.messages).replaceAll("secret", "safe"),
    );
    const result = applyGuardTransform(messages, projection, transformed);
    expect(messages).toEqual(original);
    expect(result[0]).toEqual({
      ...original[0],
      content: "safe",
      sections: { rules: "safe", removed: null },
      toolsAdded: [
        {
          name: "lookup",
          description: "safe",
          parameters: { type: "object", description: "safe" },
        },
      ],
    });
    expect(result[2]!.content).toEqual([
      { type: "thinking", thinking: "opaque", signature: "signed" },
      { type: "text", text: "safe", textSignature: "keep" },
      {
        type: "toolCall",
        id: "c",
        name: "lookup",
        arguments: { query: "safe" },
        thoughtSignature: "keep",
      },
    ]);
    expect(result[3]).toEqual({
      ...original[3],
      content: [{ type: "text", text: "safe" }],
    });
    expect(result[8]).toBe(messages[8]);
    expect(result[9]).toBe(messages[9]);
    expect(result[1]!.content?.[1]).toBe(messages[1]!.content?.[1]);
  });
  it.each([
    ["omitted", {}, "{}"],
    ["undefined", { arguments: undefined }, "{}"],
    ["null", { arguments: null }, "null"],
    ["empty", { arguments: {} }, "{}"],
  ])("preserves %s tool arguments during text rewrites", (_name, args, inputJSON) => {
    const native = [
      { role: "user", content: "secret" },
      {
        role: "assistant",
        content: [{ type: "toolCall", id: "c", name: "lookup", ...args }],
      },
    ];
    const original = structuredClone(native);
    const projection = projectGuardMessages(native);
    const transformed = JSON.parse(
      JSON.stringify(projection.messages).replaceAll("secret", "safe"),
    );
    const result = applyGuardTransform(native, projection, transformed);
    expect(projection.messages[1]?.parts).toEqual([
      {
        type: "tool_call",
        toolCall: { id: "c", name: "lookup", inputJSON },
      },
    ]);
    expect(native).toEqual(original);
    expect(result).toEqual([{ ...native[0], content: "safe" }, native[1]]);
    expect(result[1]).toBe(native[1]);
  });
  it.each([
    ["reordered messages", (value: any[]) => value.reverse()],
    [
      "wrong name",
      (value: any[]) => {
        value[1].name = "pi:9";
      },
    ],
    [
      "wrong role",
      (value: any[]) => {
        value[1].role = "assistant";
      },
    ],
    [
      "wrong call id",
      (value: any[]) => {
        value[2].parts[1].toolCall.id = "other";
      },
    ],
    [
      "wrong call name",
      (value: any[]) => {
        value[2].parts[1].toolCall.name = "other";
      },
    ],
    [
      "extra field",
      (value: any[]) => {
        value[0].unexpected = true;
      },
    ],
    [
      "missing part",
      (value: any[]) => {
        value[0].parts.pop();
      },
    ],
    [
      "invalid late JSON",
      (value: any[]) => {
        value[2].parts[1].toolCall.inputJSON = "{";
      },
    ],
    [
      "nonobject arguments",
      (value: any[]) => {
        value[2].parts[1].toolCall.inputJSON = "[]";
      },
    ],
    [
      "missing text",
      (value: any[]) => {
        delete value[7].parts[0].text;
      },
    ],
    [
      "empty text",
      (value: any[]) => {
        value[7].parts[0].text = "";
      },
    ],
  ])("rejects %s atomically", (_name, change) => {
    const projection = projectGuardMessages(messages);
    const transformed = structuredClone(projection.messages);
    transformed[0]!.parts![0] = { type: "text", text: "safe" };
    change(transformed);
    const original = structuredClone(messages);
    expect(() =>
      applyGuardTransform(messages, projection, transformed),
    ).toThrow();
    expect(messages).toEqual(original);
  });
});
