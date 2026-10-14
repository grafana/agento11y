import * as zlib from "node:zlib";
import type { Message } from "@grafana/agento11y";
import { describe, expect, it, vi } from "vitest";
import {
  applyRequestTransform,
  inspectHttpBody,
  MAX_BODY_BYTES,
  projectHttpBody,
  projectWebSocketFrame,
  RequestGuardRefusal,
  type RequestProjection,
  rewriteHttpBody,
} from "./v2-request-mappers.js";

const URLS = {
  "openai-chat": "https://api.openai.com/v1/chat/completions",
  "openai-responses": "https://api.openai.com/v1/responses",
  "openai-responses-compact": "https://api.openai.com/v1/responses/compact",
  "anthropic-messages": "https://api.anthropic.com/v1/messages?beta=true",
  gemini:
    "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse",
  "bedrock-converse":
    "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-v1%3A0/converse-stream",
} as const;

function chatBody(): any {
  return {
    model: "gpt-5",
    stream: true,
    messages: [
      { role: "system", content: "system-secret" },
      {
        role: "user",
        content: [
          { type: "text", text: "summary-secret" },
          { type: "image_url", image_url: { url: "data:image/png;base64,AA" } },
        ],
      },
      {
        role: "assistant",
        content: "answer-secret",
        reasoning_content: "reasoning-secret",
        tool_calls: [
          {
            id: "call_1",
            type: "function",
            function: { name: "read", arguments: '{"path":"a"}' },
          },
        ],
      },
      { role: "tool", tool_call_id: "call_1", content: "tool-secret" },
      { role: "user", content: "recent-secret" },
    ],
  };
}

function responsesBody(): any {
  return {
    model: "gpt-5",
    stream: true,
    store: false,
    instructions: "system-secret",
    input: [
      { type: "message", role: "developer", content: "developer-secret" },
      {
        type: "message",
        role: "user",
        content: [
          { type: "input_text", text: "summary-secret" },
          { type: "input_image", image_url: "data:image/png;base64,AA" },
        ],
      },
      {
        type: "reasoning",
        summary: [{ type: "summary_text", text: "reasoning-secret" }],
      },
      {
        type: "message",
        role: "assistant",
        status: "completed",
        content: [{ type: "output_text", text: "answer-secret" }],
      },
      {
        type: "function_call",
        call_id: "call_1",
        name: "read",
        arguments: '{"path":"a"}',
      },
      {
        type: "function_call_output",
        call_id: "call_1",
        output: "tool-secret",
      },
      {
        type: "function_call_output",
        call_id: "call_2",
        output: [{ type: "input_text", text: "listed-secret" }],
      },
      { type: "compaction_trigger" },
    ],
  };
}

function anthropicBody(): any {
  return {
    model: "claude-sonnet-4-5",
    stream: true,
    max_tokens: 1000,
    system: [
      {
        type: "text",
        text: "system-secret",
        cache_control: { type: "ephemeral" },
      },
    ],
    messages: [
      {
        role: "user",
        content: [
          { type: "text", text: "summary-secret" },
          {
            type: "document",
            source: {
              type: "text",
              media_type: "text/plain",
              data: "document-secret",
            },
            title: "notes.txt",
          },
        ],
      },
      {
        role: "assistant",
        content: [
          { type: "thinking", thinking: "reasoning-secret", signature: "" },
          { type: "text", text: "answer-secret" },
          {
            type: "tool_use",
            id: "toolu_1",
            name: "read",
            input: { path: "a" },
          },
        ],
      },
      {
        role: "user",
        content: [
          {
            type: "tool_result",
            tool_use_id: "toolu_1",
            content: "tool-secret",
          },
          {
            type: "tool_result",
            tool_use_id: "toolu_2",
            is_error: true,
            content: [{ type: "text", text: "error-secret" }],
          },
        ],
      },
    ],
  };
}

function geminiBody(): any {
  return {
    systemInstruction: { parts: [{ text: "system-secret" }] },
    contents: [
      {
        role: "user",
        parts: [
          { text: "summary-secret" },
          { inlineData: { mimeType: "image/png", data: "AA" } },
        ],
      },
      {
        role: "model",
        parts: [
          { text: "reasoning-secret", thought: true },
          { text: "answer-secret" },
          { functionCall: { id: "call_1", name: "read", args: { path: "a" } } },
        ],
      },
      {
        role: "user",
        parts: [
          {
            functionResponse: {
              id: "call_1",
              name: "read",
              response: { name: "read", content: "tool-secret" },
            },
          },
        ],
      },
    ],
    generationConfig: { maxOutputTokens: 1000 },
  };
}

function bedrockBody(): any {
  return {
    modelId: "anthropic.claude-sonnet-4-5-v1:0",
    system: [{ text: "system-secret" }, { cachePoint: { type: "default" } }],
    messages: [
      { role: "user", content: [{ text: "summary-secret" }] },
      {
        role: "assistant",
        content: [
          { text: "answer-secret" },
          {
            toolUse: {
              toolUseId: "tool_1",
              name: "read",
              input: { path: "a" },
            },
          },
        ],
      },
      {
        role: "user",
        content: [
          {
            toolResult: {
              toolUseId: "tool_1",
              content: [{ text: "tool-secret" }, { json: { ok: true } }],
              status: "success",
            },
          },
        ],
      },
    ],
    inferenceConfig: { maxTokens: 1000 },
  };
}

const cases = [
  {
    name: "OpenAI Chat Completions",
    url: URLS["openai-chat"],
    body: chatBody,
    protocol: "openai-chat",
    expected: [
      "system text* system-secret",
      "user text* summary-secret",
      'assistant thinking* reasoning-secret | text* answer-secret | tool_call read {"path":"a"}',
      "tool tool_result* call_1 tool-secret",
      "user text* recent-secret",
    ],
  },
  {
    name: "OpenAI Responses",
    url: URLS["openai-responses"],
    body: responsesBody,
    protocol: "openai-responses",
    expected: [
      "system text* system-secret",
      "system text* developer-secret",
      "user text* summary-secret",
      "assistant thinking* reasoning-secret",
      "assistant text* answer-secret",
      'assistant tool_call read {"path":"a"}',
      "tool tool_result* call_1 tool-secret",
      "tool tool_result* call_2 listed-secret",
    ],
  },
  {
    name: "OpenAI Responses compact endpoint",
    url: URLS["openai-responses-compact"],
    body: () => {
      const { stream: _stream, ...body } = responsesBody();
      body.input.pop();
      return body;
    },
    protocol: "openai-responses",
    expected: [
      "system text* system-secret",
      "system text* developer-secret",
      "user text* summary-secret",
      "assistant thinking* reasoning-secret",
      "assistant text* answer-secret",
      'assistant tool_call read {"path":"a"}',
      "tool tool_result* call_1 tool-secret",
      "tool tool_result* call_2 listed-secret",
    ],
  },
  {
    name: "Anthropic Messages",
    url: URLS["anthropic-messages"],
    body: anthropicBody,
    protocol: "anthropic-messages",
    expected: [
      "system text* system-secret",
      "user text* summary-secret | text* document-secret | text* notes.txt",
      'assistant thinking* reasoning-secret | text* answer-secret | tool_call read {"path":"a"}',
      "user tool_result* toolu_1 tool-secret | tool_result* toolu_2 error-secret",
    ],
  },
  {
    name: "Gemini streamGenerateContent",
    url: URLS.gemini,
    body: geminiBody,
    protocol: "gemini",
    expected: [
      "system text* system-secret",
      "user text* summary-secret",
      'assistant thinking* reasoning-secret | text* answer-secret | tool_call read {"path":"a"}',
      "user tool_result* call_1 tool-secret",
    ],
  },
  {
    name: "Gemini generateContent on Vertex",
    url: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent",
    body: geminiBody,
    protocol: "gemini",
    expected: [
      "system text* system-secret",
      "user text* summary-secret",
      'assistant thinking* reasoning-secret | text* answer-secret | tool_call read {"path":"a"}',
      "user tool_result* call_1 tool-secret",
    ],
  },
  {
    name: "Bedrock Converse",
    url: URLS["bedrock-converse"],
    body: bedrockBody,
    protocol: "bedrock-converse",
    expected: [
      "system text* system-secret",
      "user text* summary-secret",
      'assistant text* answer-secret | tool_call read {"path":"a"}',
      'user tool_result* tool_1 tool-secret | tool_result tool_1 {"ok":true}',
    ],
  },
] as const;

function at(value: any, path: readonly (string | number)[]) {
  return path.reduce((current, key) => current?.[key], value);
}

function describeProjection(projection: RequestProjection): string[] {
  const slotted = new Set(
    projection.slots.map((slot) => `${slot.message}:${slot.part}`),
  );
  return projection.messages.map((message, index) => {
    const parts = (message.parts ?? []).map((part, ordinal) => {
      const mark = slotted.has(`${index}:${ordinal}`) ? "*" : "";
      switch (part.type) {
        case "text":
          return `text${mark} ${part.text}`;
        case "thinking":
          return `thinking${mark} ${part.thinking}`;
        case "tool_call":
          return `tool_call${mark} ${part.toolCall.name} ${part.toolCall.inputJSON}`;
        case "tool_result":
          return `tool_result${mark} ${part.toolResult.toolCallId} ${
            part.toolResult.content ?? part.toolResult.contentJSON
          }`;
      }
      return "";
    });
    return `${message.role} ${parts.join(" | ")}`;
  });
}

function redact(value: any): any {
  if (typeof value === "string") return value.replaceAll("secret", "clean");
  if (Array.isArray(value)) return value.map(redact);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [key, redact(child)]),
    );
  return value;
}

function refusal(fn: () => unknown): RequestGuardRefusal {
  try {
    fn();
  } catch (error) {
    if (error instanceof RequestGuardRefusal) return error;
    throw error;
  }
  throw new Error("expected a refusal");
}

describe("compaction request projection", () => {
  it.each(cases)("projects every plaintext field of $name into slots", ({
    url,
    body,
    protocol,
    expected,
  }) => {
    const source = body();
    const before = structuredClone(source);
    const projection = projectHttpBody(url, source);
    expect(projection.protocol).toBe(protocol);
    expect(describeProjection(projection)).toEqual(expected);
    expect(projection).toMatchObject({ stateful: false, sealed: false });
    for (const slot of projection.slots)
      expect(at(source, slot.source)).toBe(
        at(projection.messages[slot.message].parts![slot.part], slot.field),
      );
    expect(source).toEqual(before);
  });

  it.each(
    cases,
  )("rewrites only slot text of $name and leaves everything else in place", ({
    url,
    body,
  }) => {
    const source = body();
    const projection = projectHttpBody(url, source);
    const output = applyRequestTransform(
      source,
      projection,
      redact(projection.messages),
      true,
    );
    expect(output).toEqual(redact(source));
    expect(JSON.stringify(output)).not.toContain("secret");
    expect(
      applyRequestTransform(source, projection, projection.messages, true),
    ).toBeUndefined();
    expect(
      applyRequestTransform(
        source,
        projection,
        structuredClone(projection.messages),
        true,
      ),
    ).toBeUndefined();
  });

  it("accepts the content shorthand for single text messages", () => {
    const source = chatBody();
    const projection = projectHttpBody(URLS["openai-chat"], source);
    const transformed: Message[] = redact(projection.messages).map(
      (message: Message) =>
        message.parts?.length === 1 && message.parts[0].type === "text"
          ? { role: message.role, content: message.parts[0].text }
          : message,
    );
    expect(
      applyRequestTransform(source, projection, transformed, true),
    ).toEqual(redact(source));
  });

  it.each([
    [
      "role",
      (messages: any[]) => {
        messages[1].role = "system";
      },
    ],
    ["drop message", (messages: any[]) => messages.pop()],
    [
      "add part",
      (messages: any[]) => {
        messages[0].parts.push({ type: "text", text: "extra" });
      },
    ],
    [
      "tool arguments",
      (messages: any[]) => {
        messages[2].parts[2].toolCall.inputJSON = '{"path":"b"}';
      },
    ],
    [
      "metadata",
      (messages: any[]) => {
        messages[0].parts[0].metadata = { providerType: "x" };
      },
    ],
    [
      "non-string text",
      (messages: any[]) => {
        messages[4].parts[0].text = 42;
      },
    ],
    [
      "content shorthand over several parts",
      (messages: any[]) => {
        messages[2] = { role: "assistant", content: "answer-clean" };
      },
    ],
  ])("refuses a %s change atomically after valid slot edits", (_name, change) => {
    const source = chatBody();
    const before = structuredClone(source);
    const projection = projectHttpBody(URLS["openai-chat"], source);
    const transformed = redact(projection.messages);
    change(transformed);
    expect(
      refusal(() =>
        applyRequestTransform(source, projection, transformed, true),
      ).reason,
    ).toBe("transform");
    expect(source).toEqual(before);
  });

  it.each([
    [
      "Responses encrypted reasoning",
      "openai-responses",
      (body: any) => {
        body.input[2].encrypted_content = "opaque";
      },
      "sealed",
    ],
    [
      "Responses compaction checkpoint",
      "openai-responses",
      (body: any) => {
        body.input.unshift({ type: "compaction", encrypted_content: "opaque" });
      },
      "sealed",
    ],
    [
      "Responses previous_response_id",
      "openai-responses",
      (body: any) => {
        body.previous_response_id = "resp_1";
      },
      "stateful",
    ],
    [
      "Responses conversation",
      "openai-responses",
      (body: any) => {
        body.conversation = "conv_1";
      },
      "stateful",
    ],
    [
      "Chat encrypted reasoning details",
      "openai-chat",
      (body: any) => {
        body.messages[2].reasoning_details = [
          { type: "reasoning.encrypted", data: "opaque" },
        ];
      },
      "sealed",
    ],
    [
      "Chat Gemini tool-call signature",
      "openai-chat",
      (body: any) => {
        body.messages[2].tool_calls[0].extra_content = {
          google: { thought_signature: "opaque" },
        };
      },
      "sealed",
    ],
    [
      "Anthropic thinking signature",
      "anthropic-messages",
      (body: any) => {
        body.messages[1].content[0].signature = "opaque";
      },
      "sealed",
    ],
    [
      "Anthropic redacted thinking",
      "anthropic-messages",
      (body: any) => {
        body.messages[1].content.unshift({
          type: "redacted_thinking",
          data: "opaque",
        });
      },
      "sealed",
    ],
    [
      "Anthropic compaction block",
      "anthropic-messages",
      (body: any) => {
        body.messages.unshift({
          role: "assistant",
          content: [{ type: "compaction", content: "checkpoint" }],
        });
      },
      "sealed",
    ],
    [
      "Gemini thought signature",
      "gemini",
      (body: any) => {
        body.contents[1].parts[2].thoughtSignature = "opaque";
      },
      "sealed",
    ],
    [
      "Gemini cached content",
      "gemini",
      (body: any) => {
        body.cachedContent = "cachedContents/1";
      },
      "stateful",
    ],
    [
      "Bedrock reasoning signature",
      "bedrock-converse",
      (body: any) => {
        body.messages[1].content.unshift({
          reasoningContent: {
            reasoningText: { text: "reasoning-secret", signature: "opaque" },
          },
        });
      },
      "sealed",
    ],
    [
      "Bedrock redacted reasoning",
      "bedrock-converse",
      (body: any) => {
        body.messages[1].content.unshift({
          reasoningContent: { redactedContent: "opaque" },
        });
      },
      "sealed",
    ],
  ] as const)("refuses rewriting %s but allows it unchanged", (_name, protocol, setup, flag) => {
    const kind = cases.find((entry) => entry.protocol === protocol)!;
    const source = kind.body();
    setup(source);
    const projection = projectHttpBody(kind.url, source);
    expect(projection[flag]).toBe(true);
    expect(
      applyRequestTransform(
        source,
        projection,
        structuredClone(projection.messages),
        true,
      ),
    ).toBeUndefined();
    expect(
      refusal(() =>
        applyRequestTransform(
          source,
          projection,
          redact(projection.messages),
          true,
        ),
      ).reason,
    ).toBe("rewrite");
  });

  it("refuses any rewrite the caller does not allow", () => {
    const source = bedrockBody();
    const projection = projectHttpBody(URLS["bedrock-converse"], source);
    expect(
      refusal(() =>
        applyRequestTransform(
          source,
          projection,
          redact(projection.messages),
          false,
        ),
      ).reason,
    ).toBe("rewrite");
  });

  it("evaluates unknown reasoning fields, signed text and checkpoints without slots", () => {
    const source = chatBody();
    source.messages[2].thinking_trace = "custom-secret";
    const chat = projectHttpBody(URLS["openai-chat"], source);
    expect(describeProjection(chat)[2]).toContain("thinking custom-secret");
    const anthropic = anthropicBody();
    anthropic.messages.unshift({
      role: "assistant",
      content: [{ type: "compaction", content: "checkpoint-secret" }],
    });
    expect(
      describeProjection(
        projectHttpBody(URLS["anthropic-messages"], anthropic),
      )[1],
    ).toBe("assistant text checkpoint-secret");
    const responses = responsesBody();
    responses.input.push({
      type: "mcp_call",
      id: "mcp_1",
      name: "lookup",
      output: "hosted-secret",
    });
    expect(
      describeProjection(
        projectHttpBody(URLS["openai-responses"], responses),
      ).at(-1),
    ).toContain("hosted-secret");
  });

  it.each([
    ["unknown endpoint", "https://api.example.com/v1/complete", chatBody()],
    ["non-object body", URLS["openai-chat"], ["messages"]],
    ["missing conversation", URLS["openai-chat"], { model: "gpt-5" }],
    [
      "unknown chat role",
      URLS["openai-chat"],
      { messages: [{ role: "developer", content: "x" }] },
    ],
    [
      "unknown message key",
      URLS["openai-chat"],
      { messages: [{ role: "user", content: "x", name: "fixture" }] },
    ],
    [
      "non-string text",
      URLS["openai-chat"],
      { messages: [{ role: "user", content: [{ type: "text", text: 1 }] }] },
    ],
    [
      "foreign conversation key",
      URLS["openai-chat"],
      { messages: [], contents: [{ parts: [{ text: "x" }] }] },
    ],
    [
      "unknown Responses item",
      URLS["openai-responses"],
      { input: [{ type: "item_reference", id: "msg_1" }] },
    ],
    [
      "Responses message without type",
      URLS["openai-responses"],
      { input: [{ role: "user", content: "x" }] },
    ],
    [
      "unknown Anthropic block",
      URLS["anthropic-messages"],
      {
        messages: [
          { role: "user", content: [{ type: "search_result", content: [] }] },
        ],
      },
    ],
    [
      "unknown Gemini part",
      URLS.gemini,
      { contents: [{ role: "user", parts: [{ executableCode: {} }] }] },
    ],
    [
      "multi-key Bedrock block",
      URLS["bedrock-converse"],
      { messages: [{ role: "user", content: [{ text: "x", image: {} }] }] },
    ],
  ])("refuses an unknown shape: %s", (_name, url, body) => {
    expect(refusal(() => projectHttpBody(url, body)).reason).toBe("shape");
  });
});

describe("compaction request bodies", () => {
  const text = JSON.stringify(chatBody());
  const zstd = (zlib as any).zstdCompressSync as
    | ((input: string) => Buffer)
    | undefined;

  it.each([
    ["identity", null, (value: string) => Buffer.from(value)],
    ["gzip", "gzip", (value: string) => zlib.gzipSync(value)],
    ...(zstd ? [["zstd", "zstd", zstd] as const] : []),
  ] as const)("round-trips %s bodies and re-encodes rewrites the same way", (_name, header, encode) => {
    const inspected = inspectHttpBody(
      URLS["openai-chat"],
      encode(text),
      header,
    );
    expect(inspected.text).toBe(text);
    const rewritten = rewriteHttpBody(
      inspected,
      redact(inspected.projection.messages),
      true,
    )!;
    const plain =
      header === "gzip"
        ? zlib.gunzipSync(rewritten)
        : header === "zstd"
          ? (zlib as any).zstdDecompressSync(rewritten)
          : rewritten;
    expect(JSON.parse(Buffer.from(plain).toString("utf8"))).toEqual(
      redact(chatBody()),
    );
    expect(
      rewriteHttpBody(inspected, inspected.projection.messages, true),
    ).toBeUndefined();
  });

  it("refuses zstd when the runtime cannot decode it", async () => {
    vi.resetModules();
    vi.doMock("node:zlib", async (original) => ({
      ...(await original<typeof import("node:zlib")>()),
      zstdCompressSync: undefined,
      zstdDecompressSync: undefined,
    }));
    try {
      const mappers = await import("./v2-request-mappers.js");
      const error = (() => {
        try {
          mappers.inspectHttpBody(
            URLS["openai-chat"],
            zstd?.(text) ?? Buffer.from(text),
            "zstd",
          );
        } catch (failure) {
          return failure as RequestGuardRefusal;
        }
      })();
      expect(error).toMatchObject({
        name: "RequestGuardRefusal",
        reason: "encoding",
      });
    } finally {
      vi.doUnmock("node:zlib");
      vi.resetModules();
    }
  });

  it.each([
    ["brotli", Buffer.from(text), "br", "encoding"],
    ["deflate", zlib.deflateSync(text), "deflate", "encoding"],
    [
      "stacked encodings",
      zlib.gzipSync(zlib.gzipSync(text)),
      "gzip, gzip",
      "encoding",
    ],
    ["corrupt gzip", Buffer.from("not gzip"), "gzip", "encoding"],
    ["invalid UTF-8", Buffer.from([0x7b, 0xff, 0x7d]), null, "encoding"],
    ["invalid JSON", Buffer.from('{"messages": [secret'), null, "json"],
    ["empty body", Buffer.alloc(0), null, "json"],
    ["oversized body", Buffer.alloc(MAX_BODY_BYTES + 1, 0x20), null, "size"],
    [
      "decompression past the cap",
      zlib.gzipSync(Buffer.alloc(MAX_BODY_BYTES + 1, 0x20)),
      "gzip",
      "size",
    ],
  ] as const)("refuses %s", (_name, bytes, header, reason) => {
    const error = refusal(() =>
      inspectHttpBody(URLS["openai-chat"], bytes, header),
    );
    expect(error.reason).toBe(reason);
    expect(error.message).not.toContain("secret");
  });

  it("refuses rewriting a body it cannot re-serialize byte for byte", () => {
    const pretty = JSON.stringify(chatBody(), null, 2);
    const inspected = inspectHttpBody(
      URLS["openai-chat"],
      Buffer.from(pretty),
      null,
    );
    expect(
      rewriteHttpBody(inspected, inspected.projection.messages, true),
    ).toBeUndefined();
    expect(
      refusal(() =>
        rewriteHttpBody(inspected, redact(inspected.projection.messages), true),
      ).reason,
    ).toBe("rewrite");
  });
});

describe("compaction WebSocket frames", () => {
  function frame(extra: Record<string, unknown> = {}) {
    const { stream: _stream, ...body } = responsesBody();
    return JSON.stringify({ ...body, type: "response.create", ...extra });
  }

  it("projects response.create frames like the HTTP body", () => {
    expect(describeProjection(projectWebSocketFrame(frame()))).toEqual(
      cases[1].expected,
    );
  });

  it("marks incremental continuation frames stateful", () => {
    expect(
      projectWebSocketFrame(frame({ previous_response_id: "resp_1" })).stateful,
    ).toBe(true);
  });

  it.each([
    ["other frame types", JSON.stringify({ type: "response.cancel" })],
    ["frames without input", JSON.stringify({ type: "response.create" })],
    ["unparseable frames", "{secret"],
  ])("refuses %s", (_name, value) => {
    const error = refusal(() => projectWebSocketFrame(value));
    expect(error.message).not.toContain("secret");
  });
});
