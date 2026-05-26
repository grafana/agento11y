import type {
  ContentCaptureMode,
  GenerationResult,
  GenerationStart,
  Message,
  ToolDefinition,
} from "@grafana/sigil-sdk-js";

/**
 * Pi's ToolInfo shape from @mariozechner/pi-coding-agent.
 * Declared here to avoid a hard import of pi types (treated as external at
 * runtime); the structural fields match `ToolInfo` in pi's `ExtensionAPI`.
 */
export interface PiToolInfo {
  name: string;
  description?: string;
  parameters?: unknown;
}

/**
 * Request controls extracted from a `before_provider_request` payload.
 * Defensive shape: every field is optional because providers differ in
 * which controls they accept and what names they use.
 */
export interface CachedRequestControls {
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingBudgetTokens?: number;
}

/**
 * Pi's AssistantMessage shape from @mariozechner/pi-ai.
 * Declared here to avoid hard import (pi types are external at runtime).
 */
export interface PiAssistantMessage {
  role: "assistant";
  content: PiContentBlock[];
  provider: string;
  model: string;
  responseId?: string;
  usage: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
    totalTokens: number;
    cost?: {
      input: number;
      output: number;
      cacheRead: number;
      cacheWrite: number;
      total: number;
    };
  };
  stopReason: string;
  errorMessage?: string;
  timestamp: number;
}

export type PiContentBlock =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string; redacted?: boolean }
  | {
      type: "toolCall";
      id: string;
      name: string;
      arguments: Record<string, unknown>;
    };

/** Pi's TextContent / ImageContent / UserMessage shapes from @mariozechner/pi-ai. */
export interface PiTextContent {
  type: "text";
  text: string;
}

export interface PiImageContent {
  type: "image";
  data: string;
  mimeType: string;
}

export interface PiUserMessage {
  role: "user";
  content: string | (PiTextContent | PiImageContent)[];
  timestamp: number;
}

export interface PiToolResult {
  role: "toolResult";
  toolCallId: string;
  toolName: string;
  content: Array<{ type: string; text?: string }>;
  details?: unknown;
  isError: boolean;
  timestamp: number;
}

export interface ToolTiming {
  toolCallId: string;
  toolName: string;
  startedAt: number;
  completedAt: number;
  isError: boolean;
}

/** Optional context for building a GenerationStart seed. */
export interface MapGenerationStartOptions {
  conversationId?: string;
  agentName: string;
  agentVersion?: string;
  startedAt: number;
  tools?: ToolDefinition[];
  tags?: Record<string, string>;
  systemPrompt?: string;
  requestControls?: CachedRequestControls;
}

/** Build the GenerationStart seed from an assistant message and context. */
export function mapGenerationStart(
  msg: PiAssistantMessage,
  opts: MapGenerationStartOptions,
): GenerationStart {
  const {
    conversationId,
    agentName,
    agentVersion,
    startedAt,
    tools,
    tags,
    systemPrompt,
    requestControls,
  } = opts;
  // Tags on the seed override client-level SIGIL_TAGS (the SDK merges
  // `{...clientTags, ...seedTags}`), matching claude-code/cursor.
  const start: GenerationStart = {
    conversationId,
    agentName,
    agentVersion,
    effectiveVersion: agentVersion,
    model: { provider: msg.provider, name: msg.model },
    startedAt: new Date(startedAt),
    ...(tools && tools.length > 0 ? { tools } : {}),
    ...(tags && Object.keys(tags).length > 0 ? { tags } : {}),
  };
  if (msg.content.some((b) => b.type === "thinking")) {
    start.thinkingEnabled = true;
  }
  if (systemPrompt && systemPrompt.length > 0) {
    start.systemPrompt = systemPrompt;
  }
  if (requestControls) {
    if (typeof requestControls.maxTokens === "number") {
      start.maxTokens = requestControls.maxTokens;
    }
    if (typeof requestControls.temperature === "number") {
      start.temperature = requestControls.temperature;
    }
    if (typeof requestControls.topP === "number") {
      start.topP = requestControls.topP;
    }
    if (typeof requestControls.toolChoice === "string") {
      start.toolChoice = requestControls.toolChoice;
    }
    if (typeof requestControls.thinkingBudgetTokens === "number") {
      // The SDK reads `sigil.gen_ai.request.thinking.budget_tokens` from
      // generation metadata and surfaces it as the matching span attribute.
      start.metadata = {
        "sigil.gen_ai.request.thinking.budget_tokens":
          requestControls.thinkingBudgetTokens,
      };
    }
  }
  return start;
}

/**
 * Build the GenerationResult from an assistant message.
 *
 * `completedAtMs` should be the time the provider stream finished
 * (assistant `message_end`). `msg.timestamp` is unreliable as a completion
 * marker: pi providers set it via `Date.now()` when constructing the
 * assistant message object — i.e. *before* the API request is sent — so
 * it is closer to a start timestamp than an end timestamp. Falls back to
 * `msg.timestamp` only when no end timestamp was observed.
 */
export function mapGenerationResult(
  msg: PiAssistantMessage,
  toolResults: PiToolResult[],
  contentCapture: ContentCaptureMode,
  input?: Message[],
  completedAtMs?: number,
): GenerationResult {
  const result: GenerationResult = {
    responseId: msg.responseId,
    responseModel: msg.model,
    usage: {
      inputTokens: msg.usage.input,
      outputTokens: msg.usage.output,
      totalTokens: msg.usage.totalTokens,
      cacheReadInputTokens: msg.usage.cacheRead,
      cacheWriteInputTokens: msg.usage.cacheWrite,
    },
    stopReason: mapStopReason(msg.stopReason),
    completedAt: new Date(completedAtMs ?? msg.timestamp),
    metadata:
      msg.usage.cost !== undefined ? { cost_usd: msg.usage.cost.total } : {},
  };

  if (input && input.length > 0) {
    result.input = input;
  }

  // Always emit structural tool_call / tool_result parts so the SDK can count
  // them for the `gen_ai.client.tool_calls_per_operation` histogram. Body
  // content (assistant text, tool args, tool results) is included per
  // contentCapture; in `metadata_only` the SDK strips content before export.
  const output: Message[] = [
    ...mapAssistantOutput(msg, contentCapture),
    ...mapToolResultsOutput(toolResults, contentCapture),
  ];
  if (output.length > 0) {
    result.output = output;
  }

  return result;
}

/**
 * Map a pi user message to a Sigil input Message. Returns null in
 * `metadata_only` (mirrors how assistant text/thinking is dropped) and for
 * empty/whitespace-only content. Image parts are skipped because Sigil's
 * `MessagePart` union has no image type; multiple text parts are joined with
 * a newline.
 */
export function mapUserMessage(
  msg: PiUserMessage,
  contentCapture: ContentCaptureMode,
): Message | null {
  if (contentCapture === "metadata_only") return null;

  let text: string;
  if (typeof msg.content === "string") {
    text = msg.content;
  } else {
    text = msg.content
      .filter((c): c is PiTextContent => c.type === "text")
      .map((c) => c.text)
      .join("\n");
  }

  if (text.trim().length === 0) return null;

  return {
    role: "user",
    parts: [{ type: "text", text }],
  };
}

/**
 * Map the active tool catalog to ToolDefinition[]. Filters `toolCatalog` by
 * `activeNames` so the seed reflects what was offered to the model, not the
 * full registry. `activeNames === null` means "no filter" (the active-set
 * API is unavailable); an empty Set means "no tools offered this turn" and
 * produces an empty result. `description` and `inputSchemaJSON` are body
 * content and are only emitted under `contentCapture === "full"`; otherwise
 * the definitions are name-only, matching how `git.branch` is gated.
 */
export function mapTools(
  toolCatalog: PiToolInfo[],
  activeNames: Set<string> | null,
  contentCapture: ContentCaptureMode,
): ToolDefinition[] {
  const defs: ToolDefinition[] = [];
  const seen = new Set<string>();
  const includeBody = contentCapture === "full";

  for (const tool of toolCatalog) {
    if (!tool || typeof tool.name !== "string") continue;
    if (activeNames !== null && !activeNames.has(tool.name)) continue;
    if (seen.has(tool.name)) continue;
    seen.add(tool.name);

    const def: ToolDefinition = { name: tool.name };
    if (includeBody) {
      if (typeof tool.description === "string" && tool.description.length > 0) {
        def.description = tool.description;
      }
      if (tool.parameters !== undefined) {
        try {
          def.inputSchemaJSON = JSON.stringify(tool.parameters);
        } catch {
          // Non-serializable schema (cycles, BigInt, etc.) — skip silently.
        }
      }
    }
    defs.push(def);
  }
  return defs;
}

/**
 * Read provider-specific request controls from a `before_provider_request`
 * payload. Pi emits provider-shaped payloads:
 *   - Anthropic / OpenAI Chat / OpenAI Responses: fields at the top level
 *     (`max_tokens`, `temperature`, `top_p`, `tool_choice`, `thinking`).
 *   - Gemini (`@google/genai` SDK): wrapped in `config` (`config.temperature`,
 *     `config.maxOutputTokens`, `config.toolConfig.functionCallingConfig.mode`,
 *     `config.thinkingConfig.thinkingBudget`).
 * Unknown shapes degrade to `{}`.
 */
export function extractRequestControls(
  payload: unknown,
): CachedRequestControls {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) {
    return {};
  }
  const obj = payload as Record<string, unknown>;
  const out: CachedRequestControls = {};

  const asObject = (v: unknown): Record<string, unknown> | undefined =>
    v && typeof v === "object" && !Array.isArray(v)
      ? (v as Record<string, unknown>)
      : undefined;

  // Gemini wraps controls in `config`. The legacy REST shape used
  // `generationConfig`; accept both so older pi versions or compat layers
  // still work.
  const geminiConfig = asObject(obj.config) ?? asObject(obj.generationConfig);

  const readNumber = (...candidates: unknown[]): number | undefined => {
    for (const c of candidates) {
      if (typeof c === "number" && Number.isFinite(c)) return c;
    }
    return undefined;
  };

  const maxTokens = readNumber(
    obj.max_tokens,
    obj.max_completion_tokens,
    obj.max_output_tokens,
    geminiConfig?.maxOutputTokens,
  );
  if (maxTokens !== undefined) out.maxTokens = maxTokens;

  const temperature = readNumber(obj.temperature, geminiConfig?.temperature);
  if (temperature !== undefined) out.temperature = temperature;

  const topP = readNumber(obj.top_p, obj.topP, geminiConfig?.topP);
  if (topP !== undefined) out.topP = topP;

  // `tool_choice` is a string for some providers and an object for others.
  // Anthropic forced-tool uses `{type: "tool", name: "<tool>"}` — encode as
  // `"tool:<name>"` so the forced tool isn't lost.
  // Gemini uses `config.toolConfig.functionCallingConfig.mode`.
  const tc = obj.tool_choice ?? obj.toolChoice;
  if (typeof tc === "string") {
    out.toolChoice = tc;
  } else {
    const tcObj = asObject(tc);
    if (tcObj) {
      const t = tcObj.type;
      const name = tcObj.name;
      if (typeof t === "string") {
        out.toolChoice =
          t === "tool" && typeof name === "string" && name.length > 0
            ? `tool:${name}`
            : t;
      }
    }
  }
  if (out.toolChoice === undefined && geminiConfig) {
    const fc = asObject(geminiConfig.toolConfig)?.functionCallingConfig;
    const mode = asObject(fc)?.mode;
    if (typeof mode === "string") out.toolChoice = mode;
  }

  const thinking = asObject(obj.thinking);
  if (thinking && typeof thinking.budget_tokens === "number") {
    out.thinkingBudgetTokens = thinking.budget_tokens;
  }
  if (out.thinkingBudgetTokens === undefined && geminiConfig) {
    const thinkingConfig = asObject(geminiConfig.thinkingConfig);
    if (thinkingConfig && typeof thinkingConfig.thinkingBudget === "number") {
      out.thinkingBudgetTokens = thinkingConfig.thinkingBudget;
    }
  }

  return out;
}

/**
 * Map assistant message content blocks to Sigil output messages.
 * - text/thinking parts: only when contentCapture allows body content.
 * - tool_call parts: always emitted (structure needed for the SDK's
 *   tool_calls_per_operation metric); inputJSON is only filled in `full` mode.
 */
function mapAssistantOutput(
  msg: PiAssistantMessage,
  contentCapture: ContentCaptureMode,
): Message[] {
  const messages: Message[] = [];
  const includeBodies = contentCapture !== "metadata_only";

  for (const block of msg.content) {
    switch (block.type) {
      case "text": {
        if (includeBodies && block.text.trim().length > 0) {
          messages.push({
            role: "assistant",
            parts: [{ type: "text", text: block.text }],
          });
        }
        break;
      }
      case "thinking": {
        if (block.redacted) break;
        if (includeBodies && block.thinking.trim().length > 0) {
          messages.push({
            role: "assistant",
            parts: [{ type: "thinking", thinking: block.thinking }],
          });
        }
        break;
      }
      case "toolCall": {
        messages.push({
          role: "assistant",
          parts: [
            {
              type: "tool_call",
              toolCall: {
                id: block.id,
                name: block.name,
                inputJSON:
                  contentCapture === "full"
                    ? JSON.stringify(block.arguments)
                    : "",
              },
            },
          ],
        });
        break;
      }
    }
  }

  return messages;
}

/**
 * Map pi tool results to Sigil tool result messages. Always emits the
 * structural part; body content is included only in `full` mode.
 */
function mapToolResultsOutput(
  toolResults: PiToolResult[],
  contentCapture: ContentCaptureMode,
): Message[] {
  const messages: Message[] = [];
  const includeBody = contentCapture === "full";

  for (const tr of toolResults) {
    let content = "";
    if (includeBody) {
      content = tr.content
        .filter(
          (c): c is { type: "text"; text: string } =>
            c.type === "text" && !!c.text,
        )
        .map((c) => c.text)
        .join("\n");
    }

    messages.push({
      role: "tool",
      parts: [
        {
          type: "tool_result",
          toolResult: {
            toolCallId: tr.toolCallId,
            name: tr.toolName,
            content,
            isError: tr.isError,
          },
        },
      ],
    });
  }

  return messages;
}

function mapStopReason(reason: string): string {
  switch (reason) {
    case "stop":
      return "end_turn";
    case "length":
      return "max_tokens";
    case "toolUse":
      return "tool_use";
    case "error":
      return "error";
    case "aborted":
      return "aborted";
    default:
      return reason;
  }
}
