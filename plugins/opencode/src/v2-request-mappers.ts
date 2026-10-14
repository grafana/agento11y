import { Buffer } from "node:buffer";
import { isDeepStrictEqual } from "node:util";
import * as zlib from "node:zlib";
import type { Message, MessagePart } from "@grafana/agento11y";

export type RequestProtocol =
  | "openai-chat"
  | "openai-responses"
  | "anthropic-messages"
  | "gemini"
  | "bedrock-converse";

export type RefusalReason =
  | "size"
  | "encoding"
  | "json"
  | "shape"
  | "transform"
  | "rewrite"
  | "internal";

/** Carries only a fixed reason code, never request content. */
export class RequestGuardRefusal extends Error {
  readonly reason: RefusalReason;
  constructor(reason: RefusalReason) {
    super(
      `agento11y could not verify a compaction request against the guard, so it was not sent (${reason}).`,
    );
    this.name = "RequestGuardRefusal";
    this.reason = reason;
  }
}

function refuse(reason: RefusalReason): never {
  throw new RequestGuardRefusal(reason);
}

/**
 * Anthropic Messages caps a request at 32 MB. The cap applies to the encoded
 * and the decoded body, so a compressed body cannot expand past it, and the
 * decoded text, its parse and a rewritten copy stay bounded in the host.
 */
export const MAX_BODY_BYTES = 32 * 1024 * 1024;

export type BodyEncoding = "identity" | "gzip" | "zstd";

type ZstdCodec = {
  zstdCompressSync?: (input: string) => Uint8Array<ArrayBuffer>;
  zstdDecompressSync?: (
    input: Uint8Array,
    options: { maxOutputLength: number },
  ) => Uint8Array;
};

// Not every runtime ships zstd in node:zlib; upstream shared.ts compressBody probes it the same way.
function zstd() {
  const { zstdCompressSync: compress, zstdDecompressSync: decompress } =
    zlib as unknown as ZstdCodec;
  return compress && decompress ? { compress, decompress } : undefined;
}

export function bodyEncoding(header: string | null): BodyEncoding {
  const value = (header ?? "").trim().toLowerCase();
  if (value === "" || value === "identity") return "identity";
  if (value === "gzip") return "gzip";
  if (value === "zstd" && zstd()) return "zstd";
  return refuse("encoding");
}

export function decodeBody(bytes: Uint8Array, encoding: BodyEncoding): string {
  if (bytes.byteLength > MAX_BODY_BYTES) refuse("size");
  const limit = { maxOutputLength: MAX_BODY_BYTES };
  let plain: Uint8Array;
  try {
    if (encoding === "gzip") plain = zlib.gunzipSync(bytes, limit);
    else if (encoding === "zstd")
      plain = (zstd() ?? refuse("encoding")).decompress(bytes, limit);
    else plain = bytes;
  } catch (error) {
    if (error instanceof RequestGuardRefusal) throw error;
    return refuse(
      (error as { code?: unknown } | undefined)?.code === "ERR_BUFFER_TOO_LARGE"
        ? "size"
        : "encoding",
    );
  }
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(plain);
  } catch {
    return refuse("encoding");
  }
}

export function encodeBody(
  text: string,
  encoding: BodyEncoding,
): Uint8Array<ArrayBuffer> {
  if (encoding === "gzip") return zlib.gzipSync(text);
  if (encoding === "zstd") return (zstd() ?? refuse("encoding")).compress(text);
  return new TextEncoder().encode(text);
}

function parse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return refuse("json");
  }
}

type Path = readonly (string | number)[];
type Json = Record<string, unknown>;

export interface RequestSlot {
  readonly message: number;
  readonly part: number;
  readonly field: Path;
  readonly source: Path;
}

export interface RequestProjection {
  readonly protocol: RequestProtocol;
  readonly messages: Message[];
  readonly slots: readonly RequestSlot[];
  /** The provider holds conversation state that this body continues. */
  readonly stateful: boolean;
  /** Signed or encrypted state may be bound to the plaintext around it. */
  readonly sealed: boolean;
}

interface Entry {
  readonly part: MessagePart;
  readonly field?: Path;
  readonly source?: Path;
}
type Entries = (Entry | undefined)[];
type Wrap = (value: unknown, source: Path) => Entry | undefined;

function present(value: unknown): boolean {
  return (
    value !== undefined && value !== null && value !== "" && value !== false
  );
}

class Projector {
  readonly messages: Message[] = [];
  readonly slots: RequestSlot[] = [];
  stateful = false;
  sealed = false;

  add(role: string, entries: Entries) {
    const kept = entries.filter((entry) => entry !== undefined);
    if (kept.length === 0) return;
    const message = this.messages.length;
    this.messages.push({ role, parts: kept.map((entry) => entry.part) });
    kept.forEach((entry, part) => {
      if (entry.field && entry.source)
        this.slots.push({
          message,
          part,
          field: entry.field,
          source: entry.source,
        });
    });
  }

  seal(value: unknown) {
    if (present(value)) this.sealed = true;
  }

  continues(value: unknown) {
    if (present(value)) this.stateful = true;
  }

  result(protocol: RequestProtocol): RequestProjection {
    const { messages, slots, stateful, sealed } = this;
    return { protocol, messages, slots, stateful, sealed };
  }
}

function record(value: unknown, allowed?: readonly string[]): Json {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return refuse("shape");
  if (allowed && Object.keys(value).some((key) => !allowed.includes(key)))
    return refuse("shape");
  return value as Json;
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : refuse("shape");
}

function string(value: unknown): string {
  return typeof value === "string" ? value : refuse("shape");
}

function optional(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined;
  return string(value) || undefined;
}

function absent(value: unknown): boolean {
  return value === undefined || value === null;
}

function json(value: unknown): string {
  const encoded = JSON.stringify(value);
  return encoded === undefined ? refuse("shape") : encoded;
}

// The hook wire embeds tool arguments as parsed JSON, so only the canonical form survives the round trip.
function args(value: unknown): string {
  const raw = string(value);
  try {
    return JSON.stringify(JSON.parse(raw));
  } catch {
    return raw;
  }
}

// The hook wire drops empty text, so empty strings are never projected.
function text(value: unknown, source?: Path): Entry | undefined {
  const content = string(value);
  if (!content) return undefined;
  return {
    part: { type: "text", text: content },
    ...(source ? { field: ["text"], source } : {}),
  };
}

function thinking(value: unknown, source?: Path): Entry | undefined {
  const content = string(value);
  if (!content) return undefined;
  return {
    part: { type: "thinking", thinking: content },
    ...(source ? { field: ["thinking"], source } : {}),
  };
}

interface Identity {
  readonly toolCallId?: string;
  readonly name?: string;
  readonly isError?: true;
}

function identity(id: unknown, name?: unknown, isError = false): Identity {
  const toolCallId = optional(id);
  const toolName = optional(name);
  return {
    ...(toolCallId ? { toolCallId } : {}),
    ...(toolName ? { name: toolName } : {}),
    ...(isError ? { isError: true } : {}),
  };
}

function toolResult(who: Identity, value: unknown, source?: Path): Entry {
  const content = string(value);
  return {
    part: {
      type: "tool_result",
      toolResult: { ...who, ...(content ? { content } : {}) },
    },
    ...(content && source ? { field: ["toolResult", "content"], source } : {}),
  };
}

function toolResultJSON(who: Identity, value: unknown): Entry {
  return {
    part: {
      type: "tool_result",
      toolResult: { ...who, contentJSON: json(value) },
    },
  };
}

function toolCall(id: unknown, name: unknown, inputJSON: string): Entry {
  const callID = optional(id);
  return {
    part: {
      type: "tool_call",
      toolCall: {
        ...(callID ? { id: callID } : {}),
        name: string(name),
        inputJSON,
      },
    },
  };
}

const CONVERSATION_KEYS = [
  "messages",
  "input",
  "contents",
  "system",
  "instructions",
  "systemInstruction",
  "prompt",
];

function owns(body: Json, keys: readonly string[]) {
  if (
    CONVERSATION_KEYS.some((key) => !keys.includes(key) && !absent(body[key]))
  )
    refuse("shape");
}

function chatContent(
  value: unknown,
  at: Path,
  wrap: Wrap,
  media: boolean,
): Entries {
  if (typeof value === "string") return [wrap(value, at)];
  return list(value).map((raw, index) => {
    const item = record(raw);
    if (item.type === "text") {
      record(item, ["type", "text", "cache_control"]);
      return wrap(item.text, [...at, index, "text"]);
    }
    if (media && item.type === "image_url") {
      record(item, ["type", "image_url"]);
      return undefined;
    }
    if (media && item.type === "file") {
      record(item, ["type", "file"]);
      return undefined;
    }
    return refuse("shape");
  });
}

const CHAT_REASONING = ["reasoning_content", "reasoning", "reasoning_text"];
const CHAT_ASSISTANT = [
  "role",
  "content",
  "tool_calls",
  "reasoning_details",
  "thought_signature",
  ...CHAT_REASONING,
];

// openai-chat.ts: OpenRouter `reasoning.*` details replay unmodified; Kimi uses bare `summary`/`encrypted`.
function chatReasoningDetail(raw: unknown, p: Projector): Entry | undefined {
  const detail = record(raw, [
    "type",
    "text",
    "summary",
    "data",
    "encrypted",
    "id",
    "format",
    "index",
    "signature",
  ]);
  p.seal(detail.signature);
  p.seal(detail.data);
  p.seal(detail.encrypted);
  switch (detail.type) {
    case "reasoning.text":
      return thinking(detail.text ?? "");
    case "reasoning.summary":
    case "summary":
      return thinking(detail.summary ?? "");
    case "reasoning.encrypted":
    case "encrypted":
      return undefined;
    default:
      return refuse("shape");
  }
}

function chatAssistant(message: Json, at: Path, p: Projector): Entries {
  p.seal(message.thought_signature);
  const entries: Entries = [];
  for (const [key, value] of Object.entries(message)) {
    if (CHAT_REASONING.includes(key)) {
      if (!absent(value)) entries.push(thinking(value, [...at, key]));
    } else if (!CHAT_ASSISTANT.includes(key)) {
      // `compatibility.reasoningField` may name any key: evaluated, never rewritten.
      entries.push(thinking(value));
    }
  }
  if (!absent(message.reasoning_details))
    for (const detail of list(message.reasoning_details))
      entries.push(chatReasoningDetail(detail, p));
  if (!absent(message.content))
    entries.push(
      ...chatContent(message.content, [...at, "content"], text, false),
    );
  if (!absent(message.tool_calls))
    for (const raw of list(message.tool_calls)) {
      const call = record(raw, [
        "id",
        "type",
        "function",
        "extra_content",
        "thought_signature",
      ]);
      if (call.type !== "function") refuse("shape");
      p.seal(call.extra_content);
      p.seal(call.thought_signature);
      const fn = record(call.function, ["name", "arguments"]);
      entries.push(toolCall(call.id, fn.name, args(fn.arguments)));
    }
  return entries;
}

function chat(body: Json, p: Projector) {
  owns(body, ["messages"]);
  list(body.messages).forEach((raw, index) => {
    const at: Path = ["messages", index];
    const message = record(raw);
    switch (message.role) {
      case "system":
      case "user":
        record(message, ["role", "content"]);
        p.add(
          message.role,
          chatContent(message.content, [...at, "content"], text, true),
        );
        return;
      case "assistant":
        p.add("assistant", chatAssistant(message, at, p));
        return;
      case "tool": {
        record(message, ["role", "tool_call_id", "content"]);
        const who = identity(message.tool_call_id);
        p.add(
          "tool",
          chatContent(
            message.content,
            [...at, "content"],
            (value, source) => toolResult(who, value, source),
            false,
          ),
        );
        return;
      }
      default:
        refuse("shape");
    }
  });
}

const RESPONSES_MEDIA: Record<string, readonly string[]> = {
  input_image: ["type", "image_url", "detail"],
  input_file: ["type", "filename", "detail", "file_data", "file_url"],
  input_video: ["type", "video_url"],
};

// open-responses.ts InputItem plus the openai-responses.ts and xai-responses.ts hosted items.
const RESPONSES_HOSTED = new Set([
  "web_search_call",
  "web_search_preview_call",
  "file_search_call",
  "code_interpreter_call",
  "mcp_call",
  "computer_call",
  "image_generation_call",
  "x_search_call",
]);

function responsesContent(
  value: unknown,
  at: Path,
  wrap: Wrap,
  textType: string,
  media: readonly string[],
): Entries {
  if (typeof value === "string") return [wrap(value, at)];
  return list(value).map((raw, index) => {
    const item = record(raw);
    if (item.type === textType) {
      record(item, ["type", "text"]);
      return wrap(item.text, [...at, index, "text"]);
    }
    if (typeof item.type === "string" && media.includes(item.type)) {
      record(item, RESPONSES_MEDIA[item.type]);
      return undefined;
    }
    return refuse("shape");
  });
}

function responsesMessage(item: Json, at: Path, p: Projector) {
  const content: Path = [...at, "content"];
  switch (item.role) {
    case "system":
    case "developer":
      record(item, ["type", "role", "content"]);
      p.add(
        "system",
        responsesContent(item.content, content, text, "input_text", []),
      );
      return;
    case "user":
      record(item, ["type", "role", "content", "id", "status"]);
      p.add(
        "user",
        responsesContent(item.content, content, text, "input_text", [
          "input_image",
          "input_file",
        ]),
      );
      return;
    case "assistant":
      record(item, ["type", "id", "role", "content", "phase", "status"]);
      p.add(
        "assistant",
        responsesContent(item.content, content, text, "output_text", []),
      );
      return;
    default:
      refuse("shape");
  }
}

function responses(body: Json, p: Projector) {
  owns(body, ["input", "instructions"]);
  p.continues(body.previous_response_id);
  p.continues(body.conversation);
  if (!absent(body.instructions))
    p.add("system", [text(body.instructions, ["instructions"])]);
  list(body.input).forEach((raw, index) => {
    const at: Path = ["input", index];
    const item = record(raw);
    switch (item.type) {
      case "message":
        responsesMessage(item, at, p);
        return;
      case "reasoning":
        record(item, ["type", "id", "summary", "encrypted_content"]);
        p.seal(item.encrypted_content);
        p.add(
          "assistant",
          list(item.summary).map((raw, part) => {
            const summary = record(raw, ["type", "text"]);
            if (summary.type !== "summary_text") refuse("shape");
            return thinking(summary.text, [...at, "summary", part, "text"]);
          }),
        );
        return;
      case "function_call":
        record(item, [
          "type",
          "id",
          "call_id",
          "name",
          "namespace",
          "arguments",
        ]);
        p.add("assistant", [
          toolCall(item.call_id, item.name, args(item.arguments)),
        ]);
        return;
      case "function_call_output": {
        record(item, ["type", "call_id", "output"]);
        const who = identity(item.call_id);
        p.add(
          "tool",
          responsesContent(
            item.output,
            [...at, "output"],
            (value, source) => toolResult(who, value, source),
            "input_text",
            Object.keys(RESPONSES_MEDIA),
          ),
        );
        return;
      }
      case "compaction":
        record(item, ["type", "id", "encrypted_content"]);
        p.seal(true);
        return;
      case "compaction_trigger":
        record(item, ["type"]);
        return;
      case "configuration_update":
        record(item, ["type", "reasoning"]);
        return;
      default:
        if (typeof item.type === "string" && RESPONSES_HOSTED.has(item.type)) {
          p.add("assistant", [
            toolResultJSON(identity(item.id, item.type), item),
          ]);
          return;
        }
        refuse("shape");
    }
  });
}

const ANTHROPIC_SERVER_RESULTS = [
  "web_search_tool_result",
  "code_execution_tool_result",
  "web_fetch_tool_result",
];

function anthropicDocument(block: Json, at: Path, wrap: Wrap): Entries {
  record(block, [
    "type",
    "source",
    "cache_control",
    "title",
    "context",
    "citations",
  ]);
  const source = record(block.source);
  const entries: Entries = [];
  if (source.type === "text") {
    record(source, ["type", "media_type", "data"]);
    entries.push(wrap(source.data, [...at, "source", "data"]));
  }
  for (const key of ["title", "context"])
    if (!absent(block[key])) entries.push(wrap(block[key], [...at, key]));
  return entries;
}

function anthropicBlock(raw: unknown, at: Path, p: Projector): Entries {
  const block = record(raw);
  switch (block.type) {
    case "text":
      record(block, ["type", "text", "cache_control", "citations"]);
      return [text(block.text, [...at, "text"])];
    case "image":
      record(block, ["type", "source", "cache_control", "transformations"]);
      return [];
    case "document":
      return anthropicDocument(block, at, text);
    case "tool_result": {
      record(block, [
        "type",
        "tool_use_id",
        "content",
        "is_error",
        "cache_control",
      ]);
      const who = identity(
        block.tool_use_id,
        undefined,
        block.is_error === true,
      );
      const wrap: Wrap = (value, source) => toolResult(who, value, source);
      const content: Path = [...at, "content"];
      if (typeof block.content === "string")
        return [wrap(block.content, content)];
      return list(block.content).flatMap((raw, index) => {
        const item = record(raw);
        const source: Path = [...content, index];
        if (item.type === "text") {
          record(item, ["type", "text", "cache_control"]);
          return [wrap(item.text, [...source, "text"])];
        }
        if (item.type === "image") {
          record(item, ["type", "source", "cache_control", "transformations"]);
          return [];
        }
        if (item.type === "document")
          return anthropicDocument(item, source, wrap);
        return refuse("shape");
      });
    }
    case "thinking":
      record(block, ["type", "thinking", "signature", "cache_control"]);
      p.seal(block.signature);
      return [thinking(block.thinking, [...at, "thinking"])];
    case "redacted_thinking":
      record(block, ["type", "data", "cache_control"]);
      p.seal(true);
      return [];
    case "tool_use":
    case "server_tool_use":
      record(block, ["type", "id", "name", "input", "cache_control"]);
      return [toolCall(block.id, block.name, json(block.input ?? {}))];
    // A replayed provider checkpoint: evaluated, but it pins the window it summarizes.
    case "compaction":
      record(block, ["type", "content"]);
      p.seal(true);
      return [absent(block.content) ? undefined : text(block.content)];
    default:
      if (
        typeof block.type === "string" &&
        ANTHROPIC_SERVER_RESULTS.includes(block.type)
      ) {
        record(block, ["type", "tool_use_id", "content", "cache_control"]);
        return [
          toolResultJSON(
            identity(block.tool_use_id, block.type),
            block.content,
          ),
        ];
      }
      return refuse("shape");
  }
}

function anthropic(body: Json, p: Projector) {
  owns(body, ["messages", "system"]);
  if (typeof body.system === "string")
    p.add("system", [text(body.system, ["system"])]);
  else if (!absent(body.system))
    list(body.system).forEach((raw, index) => {
      const block = record(raw, ["type", "text", "cache_control"]);
      if (block.type !== "text") refuse("shape");
      p.add("system", [text(block.text, ["system", index, "text"])]);
    });
  list(body.messages).forEach((raw, index) => {
    const message = record(raw);
    if (message.role === "system")
      record(message, ["role", "content", "output_config"]);
    else if (message.role === "user" || message.role === "assistant")
      record(message, ["role", "content"]);
    else refuse("shape");
    const role = string(message.role);
    const content: Path = ["messages", index, "content"];
    if (typeof message.content === "string")
      p.add(role, [text(message.content, content)]);
    else
      p.add(
        role,
        list(message.content).flatMap((raw, part) =>
          anthropicBlock(raw, [...content, part], p),
        ),
      );
  });
}

function geminiPart(raw: unknown, at: Path, p: Projector): Entries {
  const part = record(raw);
  p.seal(part.thoughtSignature);
  if ("text" in part) {
    record(part, ["text", "thought", "thoughtSignature"]);
    const source: Path = [...at, "text"];
    return [
      part.thought === true
        ? thinking(part.text, source)
        : text(part.text, source),
    ];
  }
  if ("inlineData" in part) {
    record(part, ["inlineData", "thoughtSignature"]);
    return [];
  }
  if ("fileData" in part) {
    record(part, ["fileData"]);
    return [];
  }
  if ("functionCall" in part) {
    record(part, ["functionCall", "thoughtSignature"]);
    const call = record(part.functionCall, ["id", "name", "args"]);
    return [toolCall(call.id, call.name, json(call.args ?? {}))];
  }
  if ("functionResponse" in part) {
    record(part, ["functionResponse"]);
    const response = record(part.functionResponse, [
      "id",
      "name",
      "response",
      "parts",
    ]);
    if (!absent(response.parts))
      for (const media of list(response.parts)) {
        const inline = record(media, ["inlineData", "thoughtSignature"]);
        p.seal(inline.thoughtSignature);
      }
    const who = identity(response.id, response.name);
    const output = response.response;
    // gemini.ts lowers text results to `{ name, content }`; anything else stays structured.
    if (
      output !== null &&
      typeof output === "object" &&
      !Array.isArray(output) &&
      typeof (output as Json).content === "string" &&
      Object.keys(output).every((key) => key === "name" || key === "content")
    )
      return [
        toolResult(who, (output as Json).content, [
          ...at,
          "functionResponse",
          "response",
          "content",
        ]),
      ];
    return [toolResultJSON(who, output ?? {})];
  }
  return refuse("shape");
}

function gemini(body: Json, p: Projector) {
  owns(body, ["contents", "systemInstruction"]);
  p.continues(body.cachedContent);
  p.continues(body.continuationToken);
  if (!absent(body.systemInstruction)) {
    const instruction = record(body.systemInstruction, ["parts", "role"]);
    list(instruction.parts).forEach((raw, index) => {
      const part = record(raw, ["text"]);
      p.add("system", [
        text(part.text, ["systemInstruction", "parts", index, "text"]),
      ]);
    });
  }
  list(body.contents).forEach((raw, index) => {
    const content = record(raw, ["role", "parts"]);
    const role =
      content.role === "model"
        ? "assistant"
        : content.role === "user" || absent(content.role)
          ? "user"
          : refuse("shape");
    const parts = absent(content.parts) ? [] : list(content.parts);
    p.add(
      role,
      parts.flatMap((raw, part) =>
        geminiPart(raw, ["contents", index, "parts", part], p),
      ),
    );
  });
}

// Converse content blocks are unions keyed by their single member name.
function single(value: unknown): [string, unknown] {
  const entries = Object.entries(record(value));
  return entries.length === 1 ? entries[0] : refuse("shape");
}

function bedrockBlock(raw: unknown, at: Path, p: Projector): Entries {
  const [key, value] = single(raw);
  switch (key) {
    case "text":
      return [text(value, [...at, "text"])];
    case "image":
    case "document":
    case "cachePoint":
      record(value);
      return [];
    case "toolUse": {
      const use = record(value, ["toolUseId", "name", "input"]);
      return [toolCall(use.toolUseId, use.name, json(use.input ?? {}))];
    }
    case "toolResult": {
      const result = record(value, ["toolUseId", "content", "status"]);
      const who = identity(
        result.toolUseId,
        undefined,
        result.status === "error",
      );
      return list(result.content).flatMap((raw, index): Entries => {
        const [kind, content] = single(raw);
        if (kind === "text")
          return [
            toolResult(who, content, [
              ...at,
              "toolResult",
              "content",
              index,
              "text",
            ]),
          ];
        if (kind === "json") return [toolResultJSON(who, content)];
        if (kind === "image" || kind === "document") return [];
        return refuse("shape");
      });
    }
    case "reasoningContent": {
      const [variant, reasoning] = single(value);
      if (variant === "redactedContent") {
        p.seal(true);
        return [];
      }
      if (variant !== "reasoningText") return refuse("shape");
      const block = record(reasoning, ["text", "signature"]);
      p.seal(block.signature);
      return [
        thinking(block.text, [
          ...at,
          "reasoningContent",
          "reasoningText",
          "text",
        ]),
      ];
    }
    default:
      return refuse("shape");
  }
}

function bedrock(body: Json, p: Projector) {
  owns(body, ["messages", "system"]);
  if (!absent(body.system))
    list(body.system).forEach((raw, index) => {
      const [key, value] = single(raw);
      if (key === "cachePoint") return;
      if (key !== "text") refuse("shape");
      p.add("system", [text(value, ["system", index, "text"])]);
    });
  list(body.messages).forEach((raw, index) => {
    const message = record(raw, ["role", "content"]);
    if (message.role !== "user" && message.role !== "assistant")
      refuse("shape");
    p.add(
      message.role,
      list(message.content).flatMap((raw, part) =>
        bedrockBlock(raw, ["messages", index, "content", part], p),
      ),
    );
  });
}

interface Route {
  readonly protocol: RequestProtocol;
  readonly path: RegExp;
  readonly conversation: string;
  readonly project: (body: Json, p: Projector) => void;
}

// Shapes follow the encoders in opencode packages/ai/src/protocols at 2.0.29.
const ROUTES: readonly Route[] = [
  {
    protocol: "openai-chat",
    path: /\/chat\/completions$/,
    conversation: "messages",
    project: chat,
  },
  {
    protocol: "openai-responses",
    path: /\/responses(?:\/compact)?$/,
    conversation: "input",
    project: responses,
  },
  {
    protocol: "anthropic-messages",
    path: /\/messages$/,
    conversation: "messages",
    project: anthropic,
  },
  {
    protocol: "gemini",
    path: /:(?:streamGenerateContent|generateContent)$/,
    conversation: "contents",
    project: gemini,
  },
  {
    protocol: "bedrock-converse",
    path: /\/model\/[^/]+\/converse(?:-stream)?$/,
    conversation: "messages",
    project: bedrock,
  },
];

export function projectHttpBody(url: string, body: unknown): RequestProjection {
  let path: string;
  try {
    path = new URL(url).pathname;
  } catch {
    return refuse("shape");
  }
  const json = record(body);
  const route = ROUTES.find(
    (candidate) =>
      candidate.path.test(path) && Array.isArray(json[candidate.conversation]),
  );
  if (!route) return refuse("shape");
  const projector = new Projector();
  route.project(json, projector);
  return projector.result(route.protocol);
}

/** open-responses-channel.ts sends the HTTP body as a `response.create` frame. */
export function projectWebSocketFrame(frame: string): RequestProjection {
  if (Buffer.byteLength(frame, "utf8") > MAX_BODY_BYTES) refuse("size");
  const body = record(parse(frame));
  if (body.type !== "response.create" || !Array.isArray(body.input))
    refuse("shape");
  const projector = new Projector();
  responses(body, projector);
  return projector.result("openai-responses");
}

export interface InspectedBody {
  readonly text: string;
  readonly body: unknown;
  readonly encoding: BodyEncoding;
  readonly projection: RequestProjection;
}

export function inspectHttpBody(
  url: string,
  bytes: Uint8Array,
  contentEncoding: string | null,
): InspectedBody {
  const encoding = bodyEncoding(contentEncoding);
  const text = decodeBody(bytes, encoding);
  const body = parse(text);
  return { text, body, encoding, projection: projectHttpBody(url, body) };
}

function at(value: unknown, path: Path): unknown {
  let current = value;
  for (const key of path) {
    if (current === null || typeof current !== "object") return undefined;
    current = (current as Record<string | number, unknown>)[key];
  }
  return current;
}

function replace(value: unknown, path: Path, text: string): unknown {
  if (path.length === 0) return text;
  if (value === null || typeof value !== "object") return refuse("rewrite");
  const [key, ...rest] = path;
  const copy = Array.isArray(value) ? [...value] : { ...value };
  (copy as Record<string | number, unknown>)[key] = replace(
    (value as Record<string | number, unknown>)[key],
    rest,
    text,
  );
  return copy;
}

function comparable(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(comparable);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .filter(([, child]) => child !== undefined)
        .map(([key, child]) => [key, comparable(child)]),
    );
  return value;
}

function normalize(message: Message, original: Message | undefined): Message {
  if (!message || typeof message !== "object") return refuse("transform");
  if (message.content === undefined) return message;
  const parts = original?.parts;
  if (
    message.parts !== undefined ||
    typeof message.content !== "string" ||
    parts?.length !== 1 ||
    parts[0].type !== "text"
  )
    return refuse("transform");
  const { content, ...rest } = message;
  return { ...rest, parts: [{ type: "text", text: content }] };
}

/**
 * Validates the whole transform before touching the body: only slot text may
 * change. Returns undefined when nothing changed, so the caller keeps the
 * original bytes.
 */
export function applyRequestTransform(
  body: unknown,
  projection: RequestProjection,
  transformed: Message[],
  allowRewrite: boolean,
): unknown {
  if (
    !Array.isArray(transformed) ||
    transformed.length !== projection.messages.length
  )
    return refuse("transform");
  const normalized = transformed.map((message, index) =>
    normalize(message, projection.messages[index]),
  );
  let expected: unknown = projection.messages;
  const changes: { source: Path; text: string }[] = [];
  for (const slot of projection.slots) {
    const path = [slot.message, "parts", slot.part, ...slot.field];
    const value = at(normalized, path);
    if (typeof value !== "string") return refuse("transform");
    if (value === at(projection.messages, path)) continue;
    expected = replace(expected, path, value);
    changes.push({ source: slot.source, text: value });
  }
  if (!isDeepStrictEqual(comparable(expected), comparable(normalized)))
    return refuse("transform");
  if (changes.length === 0) return undefined;
  if (!allowRewrite || projection.stateful || projection.sealed)
    return refuse("rewrite");
  return changes.reduce<unknown>(
    (output, change) => replace(output, change.source, change.text),
    body,
  );
}

/** Undefined when the transform changed nothing. */
export function rewriteHttpBody(
  inspected: InspectedBody,
  transformed: Message[],
  allowRewrite: boolean,
): Uint8Array<ArrayBuffer> | undefined {
  const body = applyRequestTransform(
    inspected.body,
    inspected.projection,
    transformed,
    allowRewrite,
  );
  if (body === undefined) return undefined;
  // Re-serializing must leave every byte outside the slots as the host built it.
  if (JSON.stringify(inspected.body) !== inspected.text) refuse("rewrite");
  return encodeBody(JSON.stringify(body), inspected.encoding);
}
