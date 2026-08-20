import type {
  GenerationResult,
  GenerationStart,
  Message,
  MessagePart,
  TokenUsage,
  ToolDefinition,
} from "@grafana/agento11y";
import type {
  DshContentBlock,
  DshFinishReason,
  DshGenerateOptions,
  DshMessage,
  DshSession,
  DshTokenUsage,
  DshToolSchema,
} from "./dsh.js";
import {
  encodeAndRedactToolJSON,
  redactFullText,
  redactTitle,
  redactToolJSON,
} from "./redact.js";

export const DSH_PURPOSE_METADATA_KEY = "dsh.purpose";

/** Title limit in Unicode code points, not UTF-16 code units. */
export const MAX_TITLE_LEN = 100;

export function mapUsage(
  usage: DshTokenUsage | undefined,
): TokenUsage | undefined {
  if (!usage) return undefined;
  const cacheRead = usage.cacheReadTokens ?? 0;
  const cacheWrite = usage.cacheWriteTokens ?? 0;
  const mapped: TokenUsage = {
    inputTokens: usage.inputTokens,
    outputTokens: usage.outputTokens,
    totalTokens:
      usage.totalTokens ??
      usage.inputTokens + cacheRead + cacheWrite + usage.outputTokens,
  };
  if (usage.cacheReadTokens !== undefined) {
    mapped.cacheReadInputTokens = usage.cacheReadTokens;
  }
  if (usage.cacheWriteTokens !== undefined) {
    mapped.cacheWriteInputTokens = usage.cacheWriteTokens;
  }
  if (usage.reasoningTokens !== undefined) {
    mapped.reasoningTokens = usage.reasoningTokens;
  }
  return mapped;
}

export function mapTools(tools: DshToolSchema[] | undefined): ToolDefinition[] {
  if (!tools || tools.length === 0) return [];
  const defs: ToolDefinition[] = [];
  const seen = new Set<string>();
  for (const tool of tools) {
    if (!tool || typeof tool.name !== "string" || tool.name.length === 0) {
      continue;
    }
    if (seen.has(tool.name)) continue;
    seen.add(tool.name);
    const def: ToolDefinition = { name: tool.name };
    if (typeof tool.description === "string" && tool.description.length > 0) {
      def.description = redactFullText(tool.description);
    }
    if (tool.parameters !== undefined) {
      def.inputSchemaJSON = encodeAndRedactToolJSON(tool.parameters);
    }
    defs.push(def);
  }
  return defs;
}

export function mapMessages(messages: DshMessage[] | undefined): Message[] {
  if (!messages || messages.length === 0) return [];
  const out: Message[] = [];
  for (const message of messages) {
    if (!message) continue;
    out.push(...mapMessage(message));
  }
  return out;
}

function mapMessage(message: DshMessage): Message[] {
  const blocks = Array.isArray(message.content) ? message.content : [];
  if (message.role === "system") {
    const parts = blocks
      .filter(
        (block): block is DshContentBlock & { text: string } =>
          block.type === "text" &&
          typeof block.text === "string" &&
          block.text.trim().length > 0,
      )
      .map((block) => ({
        type: "text" as const,
        text: redactFullText(block.text),
      }));
    return parts.length > 0 ? [{ role: "user", parts }] : [];
  }
  if (message.role === "assistant") {
    const parts = blocks
      .filter((block) => block.type !== "tool-result")
      .map(mapContentBlock)
      .filter((part): part is MessagePart => part !== undefined);
    return parts.length > 0 ? [{ role: "assistant", parts }] : [];
  }

  const out: Message[] = [];
  if (message.source?.kind !== "tool") {
    const parts = blocks
      .filter((block) => block.type !== "tool-result")
      .map(mapContentBlock)
      .filter(
        (part): part is MessagePart & { type: "text" } => part?.type === "text",
      );
    if (parts.length > 0) out.push({ role: "user", parts });
  }
  for (const block of blocks) {
    if (block.type !== "tool-result") continue;
    const part = mapContentBlock(block);
    if (part?.type === "tool_result") {
      out.push({ role: "tool", parts: [part] });
    }
  }
  return out;
}

function mapContentBlock(block: DshContentBlock): MessagePart | undefined {
  if (!block || typeof block.type !== "string") return undefined;
  switch (block.type) {
    case "text": {
      const text = block.text ?? "";
      return text.trim().length > 0 ? { type: "text", text } : undefined;
    }
    case "reasoning": {
      const thinking = block.text ?? "";
      return thinking.trim().length > 0
        ? { type: "thinking", thinking }
        : undefined;
    }
    case "tool-call": {
      const name = block.name ?? "";
      if (name.trim().length === 0) return undefined;
      return {
        type: "tool_call",
        toolCall: {
          id: block.id,
          name,
          inputJSON: redactToolJSON(block.arguments ?? ""),
        },
      };
    }
    case "tool-result":
      return {
        type: "tool_result",
        toolResult: {
          toolCallId: block.toolCallId,
          content: redactFullText(contentText(block.content)),
          isError: block.isError === true,
        },
      };
    default:
      return undefined;
  }
}

export function contentText(blocks: DshContentBlock[] | undefined): string {
  if (!blocks || blocks.length === 0) return "";
  return blocks
    .filter(
      (block): block is DshContentBlock & { text: string } =>
        block?.type === "text" && typeof block.text === "string",
    )
    .map((block) => block.text)
    .join("\n");
}

export function agentNameFor(
  baseName: string,
  session: DshSession | undefined,
): string {
  return session?.header?.origin === "subagent"
    ? `${baseName}/subagent`
    : baseName;
}

export interface MapGenerationStartOptions {
  conversationId?: string;
  conversationTitle?: string;
  agentName: string;
  agentVersion?: string;
  startedAt: number;
  tags?: Record<string, string>;
}

export function mapGenerationStart(
  request: DshGenerateOptions,
  opts: MapGenerationStartOptions,
): GenerationStart {
  const tools = mapTools(request.tools);
  const start: GenerationStart = {
    conversationId: opts.conversationId,
    ...(opts.conversationTitle
      ? { conversationTitle: opts.conversationTitle }
      : {}),
    agentName: opts.agentName,
    agentVersion: opts.agentVersion,
    effectiveVersion: opts.agentVersion,
    model: { provider: request.provider, name: request.model },
    startedAt: new Date(opts.startedAt),
    ...(tools.length > 0 ? { tools } : {}),
    ...(opts.tags && Object.keys(opts.tags).length > 0
      ? { tags: opts.tags }
      : {}),
  };
  if (typeof request.maxTokens === "number")
    start.maxTokens = request.maxTokens;
  if (typeof request.temperature === "number") {
    start.temperature = request.temperature;
  }
  const systemPrompt = mapSystemPrompt(request);
  if (systemPrompt !== undefined) start.systemPrompt = systemPrompt;
  if (request.purpose) {
    start.metadata = { [DSH_PURPOSE_METADATA_KEY]: request.purpose };
  }
  return start;
}

export function mapSystemPrompt(
  request: DshGenerateOptions,
): string | undefined {
  return typeof request.system === "string" && request.system.length > 0
    ? request.system
    : undefined;
}

export interface MapGenerationResultOptions {
  request: DshGenerateOptions;
  completedAt: number;
  outputBlocks: DshContentBlock[];
  usage?: DshTokenUsage;
  finishReason?: DshFinishReason;
}

export function mapGenerationResult(
  opts: MapGenerationResultOptions,
): GenerationResult {
  const result: GenerationResult = {
    responseModel: opts.request.model,
    completedAt: new Date(opts.completedAt),
  };
  const usage = mapUsage(opts.usage);
  if (usage) result.usage = usage;
  if (opts.finishReason?.kind) result.stopReason = opts.finishReason.kind;

  const input = mapMessages(opts.request.messages);
  if (input.length > 0) result.input = input;

  const outputParts = opts.outputBlocks
    .filter((block) => block.type !== "tool-result")
    .map(mapContentBlock)
    .filter((part): part is MessagePart => part !== undefined);
  if (outputParts.length > 0) {
    result.output = [{ role: "assistant", parts: outputParts }];
  }
  return result;
}

export interface ResolveConversationTitleOptions {
  sessionTitle?: string;
  conversationId?: string;
}

export function resolveConversationTitle(
  opts: ResolveConversationTitleOptions,
): string | undefined {
  const title = opts.sessionTitle?.trim();
  if (title) return clipTitle(redactTitle(title));
  return opts.conversationId;
}

function clipTitle(value: string): string {
  const codepoints = Array.from(value.trim());
  return codepoints.length > MAX_TITLE_LEN
    ? codepoints.slice(0, MAX_TITLE_LEN).join("")
    : codepoints.join("");
}
