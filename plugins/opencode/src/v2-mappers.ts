import type {
  ContentCaptureMode,
  GenerationResult,
  Message,
  MessagePart,
  TokenUsage,
} from "@grafana/agento11y";
import type { Plugin } from "@opencode/plugin";
import type { Redactor } from "./mappers.js";
import type { ToolExecutionRecord } from "./toolSpans.js";

const maxToolInputBytes = 4096;
const maxToolResultBytes = 1 << 20;

// The SDK measures JSON-encoded bytes, including control-character escapes.
function boundText(text: string, limit: number): string {
  let bytes = 0;
  let end = 0;
  for (const character of text) {
    const code = character.charCodeAt(0);
    const escaped =
      code < 0x20 ||
      character === '"' ||
      character === "\\" ||
      (character.length === 1 && code >= 0xd800 && code <= 0xdfff);
    const size = escaped
      ? JSON.stringify(character).length - 2
      : Buffer.byteLength(character);
    if (bytes + size > limit) return `${text.slice(0, end)} [truncated]`;
    bytes += size;
    end += character.length;
  }
  return text;
}

function boundJSON(raw: string, limit: number): string {
  const bytes = Buffer.from(raw);
  if (bytes.length <= limit) return raw;
  let end = limit;
  while ((bytes[end] & 0xc0) === 0x80) end--;
  return JSON.stringify(
    `${bytes.subarray(0, end).toString("utf8")} [truncated]`,
  );
}

// Promise subscriptions yield encoded payloads, not decoded schema values.
export type NativeEvent =
  ReturnType<Plugin.Context["event"]["subscribe"]> extends AsyncIterable<
    infer E
  >
    ? E
    : never;
export type NativeEventOf<T extends NativeEvent["type"]> = Extract<
  NativeEvent,
  { type: T }
>;
export type NativeTokens =
  NativeEventOf<"session.step.ended">["data"]["tokens"];
export type NativeModel =
  NativeEventOf<"session.step.started">["data"]["model"];
export type NativeLocation = NonNullable<
  NativeEventOf<"session.step.started">["location"]
>;

export interface NativeTool {
  type: "tool";
  id: string;
  name: string;
  rawInput?: string;
  input?: Record<string, unknown>;
  calledAt?: number;
  completedAt?: number;
  output?: string;
  isError?: boolean;
  errorType?: string;
}
export type NativeBlock =
  | {
      type: "text" | "reasoning";
      ordinal: number;
      text: string;
      ended: boolean;
    }
  | NativeTool;

/** v1's `<name>:<agent>` identity (`buildAgentName` in hooks.ts), which agent-scoped rules match. */
export function nativeAgentName(
  prefix: string | undefined,
  agent: string | undefined,
): string {
  const base = prefix || "opencode";
  return agent ? `${base}:${agent}` : base;
}

/** Native usage excludes both cache subsets from input and reasoning from output. */
export function mapNativeUsage(tokens?: NativeTokens): TokenUsage | undefined {
  if (tokens === undefined) return undefined;
  const inputTokens = tokens.input + tokens.cache.read + tokens.cache.write;
  const outputTokens = tokens.output + tokens.reasoning;
  return {
    inputTokens,
    outputTokens,
    totalTokens: inputTokens + outputTokens,
    cacheReadInputTokens: tokens.cache.read,
    cacheWriteInputTokens: tokens.cache.write,
    reasoningTokens: tokens.reasoning,
    inputSemantics: "inclusive",
  };
}

/** Observed plaintext only: media and opaque/signed provider state aren't exported. */
export function nativeOutput(blocks: readonly NativeBlock[]): Message[] {
  return blocks.flatMap((block): Message[] => {
    if (block.type === "text") {
      return block.text.trim()
        ? [{ role: "assistant", parts: [{ type: "text", text: block.text }] }]
        : [];
    }
    if (block.type === "reasoning") {
      return block.text.trim()
        ? [
            {
              role: "assistant",
              parts: [{ type: "thinking", thinking: block.text }],
            },
          ]
        : [];
    }
    if (block.type !== "tool") return [];
    const call: Message = {
      role: "assistant",
      parts: [
        {
          type: "tool_call",
          toolCall: {
            id: block.id,
            name: block.name,
            inputJSON:
              block.input === undefined
                ? (block.rawInput ?? "")
                : JSON.stringify(block.input),
          },
        },
      ],
    };
    return block.completedAt === undefined
      ? [call]
      : [
          call,
          {
            role: "tool",
            parts: [
              {
                type: "tool_result",
                toolResult: {
                  toolCallId: block.id,
                  name: block.name,
                  content: block.output ?? "",
                  ...(block.isError && { isError: true }),
                },
              },
            ],
          },
        ];
  });
}

export function mapNativeMessages(
  messages: readonly Message[],
  redactor: Redactor,
  capture: ContentCaptureMode,
  redactInputMessages = true,
): Message[] {
  const bodies = capture !== "metadata_only";
  const tools = capture === "full" || capture === "full_with_metadata_spans";
  return messages.flatMap((message): Message[] => {
    const parts = (message.parts ?? []).flatMap((part): MessagePart[] => {
      switch (part.type) {
        case "text":
          return bodies && part.text.trim()
            ? [
                {
                  ...part,
                  text:
                    message.role === "user"
                      ? redactInputMessages
                        ? redactor.redact(part.text)
                        : part.text
                      : message.role === "system"
                        ? redactor.redact(part.text)
                        : redactor.redactLightweight(part.text),
                },
              ]
            : [];
        case "thinking":
          return bodies && part.thinking.trim()
            ? [{ ...part, thinking: redactor.redactLightweight(part.thinking) }]
            : [];
        case "tool_call":
          return [
            {
              ...part,
              toolCall: {
                ...part.toolCall,
                inputJSON: tools
                  ? boundJSON(
                      redactor.redact(part.toolCall.inputJSON ?? ""),
                      maxToolInputBytes,
                    )
                  : "",
              },
            },
          ];
        case "tool_result":
          return [
            {
              ...part,
              toolResult: {
                ...part.toolResult,
                content: tools
                  ? boundText(
                      redactor.redact(part.toolResult.content ?? ""),
                      maxToolResultBytes,
                    )
                  : "",
                contentJSON: undefined,
              },
            },
          ];
      }
      return [];
    });
    return parts.length ? [{ role: message.role, parts }] : [];
  });
}

export interface NativeGeneration {
  model: NativeModel;
  input: readonly Message[];
  blocks: readonly NativeBlock[];
  tokens?: NativeTokens;
  cost?: number;
  finish?: string;
  streamedAt?: number;
  terminalAt: number;
  settlement: string;
}

export function mapNativeGeneration(
  generation: NativeGeneration,
  redactor: Redactor,
  capture: ContentCaptureMode,
  redactInputMessages = true,
): GenerationResult {
  return {
    input: mapNativeMessages(
      generation.input,
      redactor,
      capture,
      redactInputMessages,
    ),
    output: mapNativeMessages(
      nativeOutput(generation.blocks),
      redactor,
      capture,
    ),
    usage: mapNativeUsage(generation.tokens),
    responseModel: generation.model.id,
    stopReason: generation.finish,
    completedAt: new Date(generation.streamedAt ?? generation.terminalAt),
    metadata: {
      "opencode.input_source": "first_observed_prefix",
      "opencode.completion_time_approximate":
        generation.streamedAt === undefined,
      "opencode.settlement": generation.settlement,
      ...(generation.cost !== undefined && { cost_usd: generation.cost }),
    },
  };
}

export function nativeToolRecords(
  sessionID: string,
  blocks: readonly NativeBlock[],
): ToolExecutionRecord[] {
  return blocks.flatMap((block): ToolExecutionRecord[] => {
    if (
      block.type !== "tool" ||
      block.calledAt === undefined ||
      block.completedAt === undefined
    )
      return [];
    return [
      {
        sessionID,
        toolName: block.name,
        toolCallId: block.id,
        startedAt: block.calledAt,
        completedAt: block.completedAt,
        input: block.input,
        output: block.output,
        isError: block.isError,
        // Never attach a raw host error (which can include tool input or credentials).
        error: block.isError
          ? (block.errorType ?? "tool.execution")
          : undefined,
      },
    ];
  });
}
