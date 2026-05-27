import type {
  ContentCaptureMode,
  GenerationResult,
  Message,
  ToolDefinition,
} from "@grafana/sigil-sdk-js";
import type { AssistantMessage, Part } from "@opencode-ai/sdk";
import type { Redactor } from "./redact.js";

export type { GenerationResult };

function includesMessageBodies(contentCapture: ContentCaptureMode): boolean {
  return contentCapture !== "metadata_only";
}

function includesToolBodies(contentCapture: ContentCaptureMode): boolean {
  return (
    contentCapture === "full" || contentCapture === "full_with_metadata_spans"
  );
}

/**
 * Map user-side parts to Sigil input messages. No redaction applied — user text is the
 * user's own data and Sigil needs it for prompt analysis when content capture allows it.
 */
export function mapInputMessages(
  parts: Part[],
  contentCapture: ContentCaptureMode = "full",
): Message[] {
  if (!includesMessageBodies(contentCapture)) return [];

  const messages: Message[] = [];
  for (const part of parts) {
    if (part.type === "text" && part.text.trim().length > 0) {
      messages.push({
        role: "user",
        parts: [{ type: "text", text: part.text }],
      });
    }
  }
  return messages;
}

/** Map assistant-side parts to Sigil output messages with redaction. */
export function mapOutputMessages(
  parts: Part[],
  redactor: Redactor,
  contentCapture: ContentCaptureMode = "full",
): Message[] {
  const messages: Message[] = [];
  const includeBodies = includesMessageBodies(contentCapture);
  const includeToolBodies = includesToolBodies(contentCapture);

  for (const part of parts) {
    switch (part.type) {
      case "text": {
        if (includeBodies) {
          const text = redactor.redactLightweight(part.text);
          if (text.trim().length > 0) {
            messages.push({
              role: "assistant",
              parts: [{ type: "text", text }],
            });
          }
        }
        break;
      }
      case "reasoning": {
        if (includeBodies) {
          const thinking = redactor.redactLightweight(part.text);
          if (thinking.trim().length > 0) {
            messages.push({
              role: "assistant",
              parts: [{ type: "thinking", thinking }],
            });
          }
        }
        break;
      }
      case "tool": {
        const { state } = part;
        if (state.status === "completed") {
          messages.push({
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                toolCall: {
                  id: part.callID,
                  name: part.tool,
                  inputJSON: includeToolBodies
                    ? redactor.redact(JSON.stringify(state.input ?? {}))
                    : "",
                },
              },
            ],
          });
          messages.push({
            role: "tool",
            parts: [
              {
                type: "tool_result",
                toolResult: {
                  toolCallId: part.callID,
                  name: part.tool,
                  content: includeToolBodies
                    ? redactor.redact(state.output ?? "")
                    : "",
                },
              },
            ],
          });
        } else if (state.status === "error") {
          messages.push({
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                toolCall: {
                  id: part.callID,
                  name: part.tool,
                  inputJSON: includeToolBodies
                    ? redactor.redact(JSON.stringify(state.input ?? {}))
                    : "",
                },
              },
            ],
          });
          messages.push({
            role: "tool",
            parts: [
              {
                type: "tool_result",
                toolResult: {
                  toolCallId: part.callID,
                  name: part.tool,
                  content: includeToolBodies
                    ? redactor.redact(state.error ?? "unknown error")
                    : "",
                  isError: true,
                },
              },
            ],
          });
        }
        break;
      }
    }
  }
  return messages;
}

/** Convert opencode tool name map to Sigil ToolDefinition array. Only includes enabled tools. */
export function mapToolDefinitions(
  tools: Record<string, boolean> | undefined,
): ToolDefinition[] {
  if (!tools) return [];
  return Object.entries(tools)
    .filter(([, enabled]) => enabled)
    .map(([name]) => ({ name }));
}

/** Map an AssistantMessage + parts to a Sigil GenerationResult with content. */
export function mapGeneration(
  msg: AssistantMessage,
  userParts: Part[],
  assistantParts: Part[],
  redactor: Redactor,
  contentCapture: ContentCaptureMode = "full",
): GenerationResult {
  return {
    input: mapInputMessages(userParts, contentCapture),
    output: mapOutputMessages(assistantParts, redactor, contentCapture),
    usage: {
      inputTokens: msg.tokens.input,
      outputTokens: msg.tokens.output,
      reasoningTokens: msg.tokens.reasoning,
      cacheReadInputTokens: msg.tokens.cache.read,
      cacheWriteInputTokens: msg.tokens.cache.write,
    },
    responseModel: msg.modelID,
    stopReason: msg.finish,
    completedAt: msg.time.completed ? new Date(msg.time.completed) : undefined,
    metadata: {
      cost: msg.cost,
    },
  };
}

export function mapError(error: NonNullable<AssistantMessage["error"]>): Error {
  switch (error.name) {
    case "ProviderAuthError":
      return new Error("provider_auth");
    case "APIError":
      return new Error(`api_error: ${error.data.statusCode ?? "unknown"}`);
    case "MessageOutputLengthError":
      return new Error("output_length_exceeded");
    case "MessageAbortedError":
      return new Error("aborted");
    case "UnknownError":
      return new Error("unknown_error");
    default: {
      const _exhaustive: never = error;
      return new Error("unknown_error");
    }
  }
}
