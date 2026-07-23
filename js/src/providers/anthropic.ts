import type {
  Message as AnthropicMessage,
  MessageCreateParams,
  RawMessageStreamEvent,
} from '@anthropic-ai/sdk/resources/messages';
import type { Agento11yClient } from '../client.js';
import type { GenerationResult, Message, TokenUsage, ToolDefinition } from '../types.js';

const thinkingBudgetMetadataKey = 'agento11y.gen_ai.request.thinking.budget_tokens';
const usageServerToolUseWebSearchMetadataKey = 'agento11y.gen_ai.usage.server_tool_use.web_search_requests';
const usageServerToolUseWebFetchMetadataKey = 'agento11y.gen_ai.usage.server_tool_use.web_fetch_requests';
const usageServerToolUseTotalMetadataKey = 'agento11y.gen_ai.usage.server_tool_use.total_requests';
type AnyRecord = Record<string, unknown>;

type MessagesCreateRequest = MessageCreateParams & AnyRecord;
type MessagesCreateResponse = AnthropicMessage & AnyRecord;
type MessagesStreamEvent = RawMessageStreamEvent & AnyRecord;

/** Optional Sigil fields applied during Anthropic helper mapping. */
export interface AnthropicOptions {
  conversationId?: string;
  agentName?: string;
  agentVersion?: string;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  rawArtifacts?: boolean;
}

/** Streaming summary accepted by Anthropic messages stream wrapper. */
export interface MessagesStreamSummary {
  events?: MessagesStreamEvent[];
  finalResponse?: MessagesCreateResponse;
  outputText?: string;
  firstChunkAt?: Date | string | number;
}

async function anthropicMessagesCreate(
  client: Agento11yClient,
  request: MessagesCreateRequest,
  providerCall: (request: MessagesCreateRequest) => Promise<MessagesCreateResponse>,
  options: AnthropicOptions = {},
): Promise<MessagesCreateResponse> {
  const mappedRequest = mapAnthropicRequest(request);

  return client.startGeneration(
    {
      conversationId: options.conversationId,
      agentName: options.agentName,
      agentVersion: options.agentVersion,
      model: {
        provider: 'anthropic',
        name: String((request as AnyRecord).model ?? ''),
      },
      systemPrompt: mappedRequest.systemPrompt,
      maxTokens: mappedRequest.maxTokens,
      temperature: mappedRequest.temperature,
      topP: mappedRequest.topP,
      toolChoice: mappedRequest.toolChoice,
      thinkingEnabled: mappedRequest.thinkingEnabled,
      tools: mappedRequest.tools,
      tags: options.tags,
      metadata: metadataWithThinkingBudget(options.metadata, mappedRequest.thinkingBudget),
    },
    async (recorder) => {
      const response = await providerCall(request);
      recorder.setResult(anthropicMessagesFromRequestResponse(request, response, options));
      return response;
    },
  );
}

async function anthropicMessagesStream(
  client: Agento11yClient,
  request: MessagesCreateRequest,
  providerCall: (request: MessagesCreateRequest) => Promise<MessagesStreamSummary>,
  options: AnthropicOptions = {},
): Promise<MessagesStreamSummary> {
  const mappedRequest = mapAnthropicRequest(request);

  return client.startStreamingGeneration(
    {
      conversationId: options.conversationId,
      agentName: options.agentName,
      agentVersion: options.agentVersion,
      model: {
        provider: 'anthropic',
        name: String((request as AnyRecord).model ?? ''),
      },
      systemPrompt: mappedRequest.systemPrompt,
      maxTokens: mappedRequest.maxTokens,
      temperature: mappedRequest.temperature,
      topP: mappedRequest.topP,
      toolChoice: mappedRequest.toolChoice,
      thinkingEnabled: mappedRequest.thinkingEnabled,
      tools: mappedRequest.tools,
      tags: options.tags,
      metadata: metadataWithThinkingBudget(options.metadata, mappedRequest.thinkingBudget),
    },
    async (recorder) => {
      const summary = await providerCall(request);
      const firstChunkAt = asDate(summary.firstChunkAt);
      if (firstChunkAt !== undefined) {
        recorder.setFirstTokenAt(firstChunkAt);
      }
      recorder.setResult(anthropicMessagesFromStream(request, summary, options));
      return summary;
    },
  );
}

function anthropicMessagesFromRequestResponse(
  request: MessagesCreateRequest,
  response: MessagesCreateResponse,
  options: AnthropicOptions = {},
): GenerationResult {
  const mappedRequest = mapAnthropicRequest(request);
  const output = mapAnthropicResponseOutput(response);
  const usageMetadata = anthropicUsageMetadata((response as AnyRecord).usage);

  const result: GenerationResult = {
    responseId: response.id,
    responseModel: response.model ?? String((request as AnyRecord).model ?? ''),
    maxTokens: mappedRequest.maxTokens,
    temperature: mappedRequest.temperature,
    topP: mappedRequest.topP,
    toolChoice: mappedRequest.toolChoice,
    thinkingEnabled: mappedRequest.thinkingEnabled,
    input: mappedRequest.input,
    output,
    tools: mappedRequest.tools,
    usage: mapAnthropicUsage((response as AnyRecord).usage),
    stopReason: normalizeStopReason((response as AnyRecord).stop_reason),
    metadata: mergeMetadata(metadataWithThinkingBudget(options.metadata, mappedRequest.thinkingBudget), usageMetadata),
    tags: options.tags ? { ...options.tags } : undefined,
  };

  if (options.rawArtifacts) {
    result.artifacts = [
      jsonArtifact('request', 'anthropic.messages.request', request),
      jsonArtifact('response', 'anthropic.messages.response', response),
    ];
    if (mappedRequest.tools.length > 0) {
      result.artifacts.push(jsonArtifact('tools', 'anthropic.messages.tools', mappedRequest.tools));
    }
  }

  return result;
}

function anthropicMessagesFromStream(
  request: MessagesCreateRequest,
  summary: MessagesStreamSummary,
  options: AnthropicOptions = {},
): GenerationResult {
  const mappedRequest = mapAnthropicRequest(request);
  const events = summary.events ?? [];

  const outputText = summary.outputText ?? extractAnthropicStreamText(events);
  const fallbackOutput: Message[] = outputText.length > 0 ? [{ role: 'assistant', content: outputText }] : [];
  const streamUsageMetadata = anthropicStreamUsageMetadata(events);

  const result: GenerationResult = summary.finalResponse
    ? {
        ...anthropicMessagesFromRequestResponse(request, summary.finalResponse, options),
        output:
          mapAnthropicResponseOutput(summary.finalResponse).length > 0
            ? mapAnthropicResponseOutput(summary.finalResponse)
            : fallbackOutput,
      }
    : {
        responseModel: String((request as AnyRecord).model ?? ''),
        maxTokens: mappedRequest.maxTokens,
        temperature: mappedRequest.temperature,
        topP: mappedRequest.topP,
        toolChoice: mappedRequest.toolChoice,
        thinkingEnabled: mappedRequest.thinkingEnabled,
        input: mappedRequest.input,
        output: fallbackOutput,
        tools: mappedRequest.tools,
        metadata: mergeMetadata(
          metadataWithThinkingBudget(options.metadata, mappedRequest.thinkingBudget),
          streamUsageMetadata,
        ),
        tags: options.tags ? { ...options.tags } : undefined,
      };

  if (options.rawArtifacts) {
    const existing = result.artifacts ?? [];
    if (!existing.some((artifact) => artifact.type === 'request')) {
      existing.push(jsonArtifact('request', 'anthropic.messages.request', request));
    }
    if (mappedRequest.tools.length > 0 && !existing.some((artifact) => artifact.type === 'tools')) {
      existing.push(jsonArtifact('tools', 'anthropic.messages.tools', mappedRequest.tools));
    }
    existing.push(jsonArtifact('provider_event', 'anthropic.messages.stream_events', events));
    result.artifacts = existing;
  }

  return result;
}

export const messages = {
  create: anthropicMessagesCreate,
  stream: anthropicMessagesStream,
  fromRequestResponse: anthropicMessagesFromRequestResponse,
  fromStream: anthropicMessagesFromStream,
} as const;

function mapAnthropicRequest(request: MessagesCreateRequest): {
  input: Message[];
  systemPrompt?: string;
  tools: ToolDefinition[];
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingEnabled?: boolean;
  thinkingBudget?: number;
} {
  const input: Message[] = [];

  const rawMessages = Array.isArray((request as AnyRecord).messages)
    ? ((request as AnyRecord).messages as unknown[])
    : [];

  for (const rawMessage of rawMessages) {
    if (!isRecord(rawMessage)) {
      continue;
    }

    const roleRaw = typeof rawMessage.role === 'string' ? rawMessage.role.toLowerCase() : '';
    const mappedRole: Message['role'] = roleRaw === 'assistant' || roleRaw === 'tool' ? roleRaw : 'user';

    const contentParts = mapAnthropicContentParts(rawMessage.content, mappedRole);
    const contentText = extractText(rawMessage.content);

    if (contentParts.length > 0) {
      const hasToolResult = contentParts.some((part) => part.type === 'tool_result');
      input.push({
        role: hasToolResult ? 'tool' : mappedRole,
        name: typeof rawMessage.name === 'string' ? rawMessage.name : undefined,
        content: contentText || undefined,
        parts: contentParts,
      });
      continue;
    }

    if (contentText.length > 0) {
      input.push({
        role: mappedRole,
        name: typeof rawMessage.name === 'string' ? rawMessage.name : undefined,
        content: contentText,
      });
    }
  }

  return {
    input,
    systemPrompt: extractAnthropicSystemPrompt((request as AnyRecord).system),
    tools: mapAnthropicTools((request as AnyRecord).tools),
    maxTokens: readIntFromAny((request as AnyRecord).max_tokens),
    temperature: readNumberFromAny((request as AnyRecord).temperature),
    topP: readNumberFromAny((request as AnyRecord).top_p),
    toolChoice: canonicalToolChoice((request as AnyRecord).tool_choice),
    thinkingEnabled: anthropicThinkingEnabled((request as AnyRecord).thinking),
    thinkingBudget: anthropicThinkingBudget((request as AnyRecord).thinking),
  };
}

function mapAnthropicContentParts(content: unknown, role: Message['role']): NonNullable<Message['parts']> {
  if (!Array.isArray(content)) {
    return [];
  }

  const parts: NonNullable<Message['parts']> = [];

  for (const rawBlock of content) {
    if (!isRecord(rawBlock)) {
      continue;
    }

    const blockType = typeof rawBlock.type === 'string' ? rawBlock.type : '';

    if (blockType === 'text' && typeof rawBlock.text === 'string' && rawBlock.text.trim().length > 0) {
      parts.push({
        type: 'text',
        text: rawBlock.text,
        metadata: { providerType: blockType },
      });
      continue;
    }

    if (blockType === 'thinking' || blockType === 'redacted_thinking') {
      const thinking =
        typeof rawBlock.thinking === 'string'
          ? rawBlock.thinking
          : typeof rawBlock.data === 'string'
            ? rawBlock.data
            : typeof rawBlock.text === 'string'
              ? rawBlock.text
              : '';

      if (thinking.trim().length > 0) {
        parts.push({
          type: 'thinking',
          thinking,
          metadata: { providerType: blockType },
        });
      }
      continue;
    }

    if (blockType === 'tool_use' || blockType === 'server_tool_use' || blockType === 'mcp_tool_use') {
      const name = typeof rawBlock.name === 'string' ? rawBlock.name : '';
      if (name.trim().length === 0) {
        continue;
      }

      parts.push({
        type: 'tool_call',
        toolCall: {
          id: typeof rawBlock.id === 'string' ? rawBlock.id : undefined,
          name,
          inputJSON: jsonString(rawBlock.input),
        },
        metadata: { providerType: blockType },
      });
      continue;
    }

    if (blockType === 'tool_result' || role === 'tool') {
      const contentValue = extractText(rawBlock.content);
      parts.push({
        type: 'tool_result',
        toolResult: {
          toolCallId:
            typeof rawBlock.tool_use_id === 'string'
              ? rawBlock.tool_use_id
              : typeof rawBlock.tool_call_id === 'string'
                ? rawBlock.tool_call_id
                : undefined,
          name: typeof rawBlock.name === 'string' ? rawBlock.name : undefined,
          content: contentValue || undefined,
          contentJSON: jsonString(rawBlock.content),
          isError: typeof rawBlock.is_error === 'boolean' ? rawBlock.is_error : undefined,
        },
        metadata: { providerType: blockType || 'tool_result' },
      });
    }
  }

  return parts;
}

function mapAnthropicResponseOutput(response: MessagesCreateResponse): Message[] {
  const content = (response as AnyRecord).content;
  const parts = mapAnthropicContentParts(content, 'assistant');
  const text = extractText(content);

  if (parts.length === 0 && text.length === 0) {
    return [];
  }

  return [
    {
      role: 'assistant',
      content: text || undefined,
      parts: parts.length > 0 ? parts : undefined,
    },
  ];
}

function mapAnthropicTools(rawTools: unknown): ToolDefinition[] {
  if (!Array.isArray(rawTools)) {
    return [];
  }

  const out: ToolDefinition[] = [];
  for (const rawTool of rawTools) {
    if (!isRecord(rawTool)) {
      continue;
    }

    const name = typeof rawTool.name === 'string' ? rawTool.name : '';
    if (name.trim().length === 0) {
      continue;
    }

    out.push({
      name,
      description: typeof rawTool.description === 'string' ? rawTool.description : undefined,
      type: typeof rawTool.type === 'string' ? rawTool.type : 'function',
      inputSchemaJSON: hasValue(rawTool.input_schema) ? jsonString(rawTool.input_schema) : undefined,
    });
  }

  return out;
}

function mapAnthropicUsage(rawUsage: unknown): TokenUsage | undefined {
  if (!isRecord(rawUsage)) {
    return undefined;
  }

  const inputTokens = readIntFromAny(rawUsage.input_tokens);
  const outputTokens = readIntFromAny(rawUsage.output_tokens);
  const totalTokens = readIntFromAny(rawUsage.total_tokens);
  const cacheReadInputTokens = readIntFromAny(rawUsage.cache_read_input_tokens);
  const cacheWriteInputTokens = readIntFromAny(rawUsage.cache_creation_input_tokens);

  const out: TokenUsage = {};
  if (inputTokens !== undefined) {
    out.inputTokens = inputTokens;
  }
  if (outputTokens !== undefined) {
    out.outputTokens = outputTokens;
  }
  if (totalTokens !== undefined) {
    out.totalTokens = totalTokens;
  } else if (inputTokens !== undefined || outputTokens !== undefined) {
    out.totalTokens =
      (inputTokens ?? 0) + (outputTokens ?? 0) + (cacheReadInputTokens ?? 0) + (cacheWriteInputTokens ?? 0);
  }
  if (cacheReadInputTokens !== undefined) {
    out.cacheReadInputTokens = cacheReadInputTokens;
  }
  if (cacheWriteInputTokens !== undefined) {
    out.cacheWriteInputTokens = cacheWriteInputTokens;
  }

  if (Object.keys(out).length === 0) {
    return undefined;
  }
  out.inputIsDisjoint = true;
  return out;
}

function anthropicUsageMetadata(rawUsage: unknown): Record<string, unknown> | undefined {
  if (!isRecord(rawUsage)) {
    return undefined;
  }

  const serverToolUse = isRecord(rawUsage.server_tool_use)
    ? rawUsage.server_tool_use
    : isRecord(rawUsage.serverToolUse)
      ? rawUsage.serverToolUse
      : undefined;
  if (serverToolUse === undefined) {
    return undefined;
  }

  const webSearchRequests = readIntFromAny(serverToolUse.web_search_requests ?? serverToolUse.webSearchRequests);
  const webFetchRequests = readIntFromAny(serverToolUse.web_fetch_requests ?? serverToolUse.webFetchRequests);
  const totalRequests = (webSearchRequests ?? 0) + (webFetchRequests ?? 0);
  if (totalRequests === 0) {
    return undefined;
  }

  const out: Record<string, unknown> = {};
  if (webSearchRequests !== undefined && webSearchRequests > 0) {
    out[usageServerToolUseWebSearchMetadataKey] = webSearchRequests;
  }
  if (webFetchRequests !== undefined && webFetchRequests > 0) {
    out[usageServerToolUseWebFetchMetadataKey] = webFetchRequests;
  }
  out[usageServerToolUseTotalMetadataKey] = totalRequests;
  return out;
}

function extractAnthropicSystemPrompt(system: unknown): string | undefined {
  if (!hasValue(system)) {
    return undefined;
  }

  if (typeof system === 'string') {
    const normalized = system.trim();
    return normalized.length > 0 ? normalized : undefined;
  }

  if (Array.isArray(system)) {
    const chunks: string[] = [];
    for (const block of system) {
      if (!isRecord(block)) {
        continue;
      }

      if (typeof block.text === 'string' && block.text.trim().length > 0) {
        chunks.push(block.text.trim());
      }
    }

    if (chunks.length > 0) {
      return chunks.join('\n');
    }
  }

  const fallback = extractText(system);
  return fallback.length > 0 ? fallback : undefined;
}

function normalizeStopReason(value: unknown): string | undefined {
  if (typeof value !== 'string') {
    return undefined;
  }
  const normalized = value.trim();
  return normalized.length > 0 ? normalized : undefined;
}

function extractAnthropicStreamText(events: MessagesStreamEvent[]): string {
  const chunks: string[] = [];

  for (const event of events) {
    const type = typeof event.type === 'string' ? event.type : '';
    if (type === 'content_block_delta' && isRecord(event.delta)) {
      const deltaType = typeof event.delta.type === 'string' ? event.delta.type : '';
      if (deltaType === 'text_delta' && typeof event.delta.text === 'string' && event.delta.text.length > 0) {
        chunks.push(event.delta.text);
        continue;
      }
      if (typeof event.delta.text === 'string' && event.delta.text.length > 0) {
        chunks.push(event.delta.text);
        continue;
      }
    }

    const fallback = extractText(event);
    if (fallback.length > 0) {
      chunks.push(fallback);
    }
  }

  return chunks.join('');
}

function anthropicStreamUsageMetadata(events: MessagesStreamEvent[]): Record<string, unknown> | undefined {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (event === undefined) {
      continue;
    }
    if ((typeof event.type === 'string' ? event.type : '') !== 'message_delta') {
      continue;
    }
    const metadata = anthropicUsageMetadata((event as AnyRecord).usage);
    if (metadata !== undefined) {
      return metadata;
    }
  }
  return undefined;
}

function anthropicThinkingEnabled(value: unknown): boolean | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value === 'boolean') {
    return value;
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    if (normalized === 'enabled' || normalized === 'adaptive') {
      return true;
    }
    if (normalized === 'disabled') {
      return false;
    }
    return undefined;
  }
  if (isRecord(value)) {
    if (typeof value.enabled === 'boolean') {
      return value.enabled;
    }
    const mode = String(value.type ?? value.mode ?? '')
      .trim()
      .toLowerCase();
    if (mode === 'enabled' || mode === 'adaptive') {
      return true;
    }
    if (mode === 'disabled') {
      return false;
    }
    return undefined;
  }
  return undefined;
}

function anthropicThinkingBudget(value: unknown): number | undefined {
  if (!isRecord(value)) {
    return undefined;
  }
  return readIntFromAny(value.budget_tokens);
}

function canonicalToolChoice(value: unknown): string | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    return normalized.length > 0 ? normalized : undefined;
  }
  if (isRecord(value) && 'value' in value) {
    const normalized = String((value as { value: unknown }).value ?? '')
      .trim()
      .toLowerCase();
    return normalized.length > 0 ? normalized : undefined;
  }
  return jsonString(value);
}

function extractText(value: unknown): string {
  if (!hasValue(value)) {
    return '';
  }

  if (typeof value === 'string') {
    return value.trim();
  }

  if (Array.isArray(value)) {
    const chunks: string[] = [];
    for (const item of value) {
      const chunk = extractText(item);
      if (chunk.length > 0) {
        chunks.push(chunk);
      }
    }
    return chunks.join('\n');
  }

  if (isRecord(value)) {
    if (typeof value.text === 'string' && value.text.trim().length > 0) {
      return value.text.trim();
    }
    if (typeof value.content === 'string' && value.content.trim().length > 0) {
      return value.content.trim();
    }
    if ('content' in value && value.content !== undefined && value.content !== null) {
      return extractText(value.content);
    }
  }

  return String(value).trim();
}

function readIntFromAny(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) {
    const asInt = Math.trunc(value);
    return Number.isNaN(asInt) ? undefined : asInt;
  }
  if (typeof value === 'string') {
    const parsed = Number.parseInt(value.trim(), 10);
    return Number.isNaN(parsed) ? undefined : parsed;
  }
  return undefined;
}

function readNumberFromAny(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === 'string') {
    const parsed = Number.parseFloat(value.trim());
    return Number.isNaN(parsed) ? undefined : parsed;
  }
  return undefined;
}

function metadataWithThinkingBudget(
  metadata: Record<string, unknown> | undefined,
  thinkingBudget: number | undefined,
): Record<string, unknown> | undefined {
  if (thinkingBudget === undefined) {
    return metadata ? { ...metadata } : undefined;
  }
  const out = metadata ? { ...metadata } : {};
  out[thinkingBudgetMetadataKey] = thinkingBudget;
  return out;
}

function mergeMetadata(
  base: Record<string, unknown> | undefined,
  extra: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  if (base === undefined) {
    return extra ? { ...extra } : undefined;
  }
  if (extra === undefined) {
    return { ...base };
  }
  return { ...base, ...extra };
}

function jsonArtifact(type: 'request' | 'response' | 'tools' | 'provider_event', name: string, payload: unknown) {
  return {
    type,
    name,
    payload: jsonString(payload),
    mimeType: 'application/json',
  };
}

function jsonString(value: unknown): string {
  try {
    return JSON.stringify(value, objectKeySorter);
  } catch {
    return String(value ?? '');
  }
}

function objectKeySorter(_key: string, value: unknown): unknown {
  if (!isRecord(value) || Array.isArray(value)) {
    return value;
  }
  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(value).sort()) {
    sorted[key] = value[key];
  }
  return sorted;
}

function hasValue(value: unknown): boolean {
  return value !== undefined && value !== null;
}

function isRecord(value: unknown): value is AnyRecord {
  return typeof value === 'object' && value !== null;
}

function asDate(value: Date | string | number | undefined): Date | undefined {
  if (value === undefined) {
    return undefined;
  }
  const date = value instanceof Date ? new Date(value) : new Date(value);
  if (Number.isNaN(date.getTime())) {
    return undefined;
  }
  return date;
}
