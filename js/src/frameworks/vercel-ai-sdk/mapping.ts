import type { Message, MessagePart, TokenUsage, ToolDefinition } from '../../types.js';
import type {
  Agento11yVercelAiSdkOptions,
  ConversationResolution,
  ParsedToolCall,
  ParsedToolResult,
  StepFinishEvent,
  StepOutputMapping,
  StepStartEvent,
  StreamChunkEvent,
  ToolCallFinishEvent,
  ToolCallStartEvent,
} from './types.js';

const frameworkConversationPrefix = 'agento11y:framework:vercel-ai-sdk:';
const frameworkName = 'vercel-ai-sdk';
const frameworkSource = 'framework';
const frameworkLanguage = 'typescript';
const stepTypeMetadataKey = 'agento11y.framework.step_type';
const reasoningTextMetadataKey = 'agento11y.framework.reasoning_text';
const maxMetadataDepth = 5;

type AnyRecord = Record<string, unknown>;

export function frameworkIdentity(): { name: string; source: string; language: string } {
  return {
    name: frameworkName,
    source: frameworkSource,
    language: frameworkLanguage,
  };
}

export function buildFrameworkTags(extraTags: Record<string, string> | undefined): Record<string, string> {
  return {
    ...(extraTags ?? {}),
    'agento11y.framework.name': frameworkName,
    'agento11y.framework.source': frameworkSource,
    'agento11y.framework.language': frameworkLanguage,
  };
}

export function buildFrameworkMetadata(
  extraMetadata: Record<string, unknown> | undefined,
  stepType: string | undefined,
  reasoningText: string | undefined,
): Record<string, unknown> {
  const raw: Record<string, unknown> = {
    ...(extraMetadata ?? {}),
    'agento11y.framework.name': frameworkName,
    'agento11y.framework.source': frameworkSource,
    'agento11y.framework.language': frameworkLanguage,
  };
  if (stepType !== undefined) {
    raw[stepTypeMetadataKey] = stepType;
  }
  if (reasoningText !== undefined) {
    raw[reasoningTextMetadataKey] = reasoningText;
  }
  return normalizeMetadata(raw);
}

export function fallbackConversationId(suffix: string): string {
  return `${frameworkConversationPrefix}${suffix}`;
}

export function resolveConversationId(params: {
  explicitConversationId?: string;
  resolver?: Agento11yVercelAiSdkOptions['resolveConversationId'];
  stepStartEvent: StepStartEvent;
  fallbackSeed: string;
}): ConversationResolution {
  const explicit = asString(params.explicitConversationId);
  if (explicit.length > 0) {
    return { conversationId: explicit, source: 'explicit' };
  }

  const fromResolver = asString(params.resolver?.(params.stepStartEvent));
  if (fromResolver.length > 0) {
    return { conversationId: fromResolver, source: 'resolver' };
  }

  return {
    conversationId: fallbackConversationId(params.fallbackSeed),
    source: 'fallback',
  };
}

export function extractStepNumber(event: { stepNumber?: unknown }, fallback: number): number {
  const stepNumber = asNonNegativeInt(event.stepNumber);
  if (stepNumber !== undefined) {
    return stepNumber;
  }
  return fallback;
}

/**
 * Best-effort extraction of an output-schema tool from the AI SDK step-start
 * `output` reference (set via `generateText({ output: Output.object({ schema }) })`).
 *
 * Fail-open: any shape mismatch, schema resolution failure, or serialization
 * error resolves to `undefined` so schema capture never breaks the generation.
 */
export async function extractOutputSchemaTool(output: unknown): Promise<ToolDefinition | undefined> {
  try {
    if (!isRecord(output)) {
      return undefined;
    }
    const responseFormatRef = (output as { responseFormat?: unknown }).responseFormat;
    if (responseFormatRef === undefined || responseFormatRef === null) {
      return undefined;
    }
    const responseFormat = await Promise.resolve(responseFormatRef as unknown);
    if (!isRecord(responseFormat)) {
      return undefined;
    }
    const formatType = asString((responseFormat as { type?: unknown }).type);
    if (formatType !== 'json') {
      return undefined;
    }
    const schema = (responseFormat as { schema?: unknown }).schema;
    if (schema === undefined || schema === null) {
      return undefined;
    }
    let inputSchemaJSON: string;
    try {
      const encoded = JSON.stringify(schema);
      if (encoded === undefined || encoded === 'null') {
        return undefined;
      }
      inputSchemaJSON = encoded;
    } catch {
      return undefined;
    }
    const formatName = asString((responseFormat as { name?: unknown }).name);
    const outputName = asString((output as { name?: unknown }).name);
    const description = asString((responseFormat as { description?: unknown }).description);
    const tool: ToolDefinition = {
      name: formatName.length > 0 ? formatName : outputName.length > 0 ? outputName : 'output',
      type: 'output_schema',
      inputSchemaJSON,
    };
    if (description.length > 0) {
      tool.description = description;
    }
    return tool;
  } catch {
    return undefined;
  }
}

export function mapModelFromStepStart(event: StepStartEvent): { provider: string; modelName: string } {
  const model = asRecord(event.model);
  const modelName = asString(model?.modelId) || asString(model?.id) || asString(model?.name) || 'unknown';
  const provider = normalizeProvider(asString(model?.provider), modelName);
  return {
    provider,
    modelName,
  };
}

export function mapResponseFromStepFinish(event: StepFinishEvent): {
  responseId?: string;
  responseModel?: string;
  finishReason?: string;
} {
  const response = asRecord(event.response);
  const responseId = asString(response?.id);
  const responseModel = asString(response?.modelId) || asString(event.modelId);
  const finishReason = asString(event.finishReason);

  return {
    responseId: responseId.length > 0 ? responseId : undefined,
    responseModel: responseModel.length > 0 ? responseModel : undefined,
    finishReason: finishReason.length > 0 ? finishReason : undefined,
  };
}

export function shouldTreatStepAsError(event: StepFinishEvent): boolean {
  const finishReason = asString(event.finishReason).toLowerCase();
  if (finishReason === 'error') {
    return true;
  }
  return event.error !== undefined;
}

export function mapUsageFromStepFinish(event: StepFinishEvent): TokenUsage | undefined {
  const usage = asRecord(event.usage);
  if (usage === undefined) {
    return undefined;
  }

  const inputTokens = numberFromCandidates([usage.inputTokens, usage.promptTokens]);
  const outputTokens = numberFromCandidates([usage.outputTokens, usage.completionTokens]);
  const totalTokens = numberFromCandidates([usage.totalTokens]);

  const inputDetails = asRecord(usage.inputTokenDetails);
  const outputDetails = asRecord(usage.outputTokenDetails);
  const cacheReadTokens = numberFromCandidates([inputDetails?.cacheReadTokens]);
  const cacheWriteFromUpstream = numberFromCandidates([inputDetails?.cacheWriteTokens]);
  const cacheCreationFromUpstream = numberFromCandidates([inputDetails?.cacheCreationTokens]);
  const cacheWriteTokens = cacheWriteFromUpstream ?? cacheCreationFromUpstream;
  const reasoningTokens = numberFromCandidates([outputDetails?.reasoningTokens]);

  const hasUsagePayload =
    inputTokens !== undefined ||
    outputTokens !== undefined ||
    totalTokens !== undefined ||
    inputDetails !== undefined ||
    outputDetails !== undefined;
  if (!hasUsagePayload) {
    return undefined;
  }

  // Vercel AI SDK reports cache-inclusive inputTokens: the total input includes both
  // cacheReadTokens and cacheWriteTokens (totalTokens === inputTokens + outputTokens).
  // Subtract both to recover fresh, non-cached input for the disjoint TokenUsage contract.
  const resolvedCacheRead = cacheReadTokens ?? 0;
  const resolvedCacheWrite = cacheWriteTokens ?? 0;
  const resolvedInput = Math.max((inputTokens ?? 0) - resolvedCacheRead - resolvedCacheWrite, 0);
  const resolvedOutput = outputTokens ?? 0;
  const resolvedTotal = totalTokens ?? resolvedInput + resolvedOutput + resolvedCacheRead + resolvedCacheWrite;

  return {
    inputTokens: resolvedInput,
    outputTokens: resolvedOutput,
    totalTokens: resolvedTotal,
    cacheReadInputTokens: resolvedCacheRead,
    cacheWriteInputTokens: resolvedCacheWrite,
    reasoningTokens: reasoningTokens ?? 0,
    inputIsDisjoint: true,
  };
}

export function mapInputMessages(messages: unknown): Message[] {
  if (!Array.isArray(messages)) {
    return [];
  }

  const output: Message[] = [];
  for (const rawMessage of messages) {
    const message = mapSingleMessage(rawMessage);
    if (message !== undefined) {
      output.push(message);
    }
  }
  return output;
}

export function mapStepOutput(event: StepFinishEvent): StepOutputMapping {
  const text = asString(event.text);
  const reasoningText = asString(event.reasoningText);
  const parsedToolCalls = parseToolCalls(event.toolCalls);
  const parsedToolResults = parseToolResults(event.toolResults);
  const stepType = deriveStepType({
    stepType: event.stepType,
    stepNumber: event.stepNumber,
    hasToolResults: parsedToolResults.length > 0,
  });

  const assistantParts: MessagePart[] = [];
  if (text.length > 0) {
    assistantParts.push({
      type: 'text',
      text,
    });
  }

  for (const toolCall of parsedToolCalls) {
    assistantParts.push({
      type: 'tool_call',
      toolCall: {
        id: toolCall.id,
        name: toolCall.name,
        inputJSON: toolCall.inputJSON,
      },
    });
  }

  const outputMessages: Message[] = [];
  if (assistantParts.length > 0) {
    outputMessages.push({
      role: 'assistant',
      content: text.length > 0 ? text : undefined,
      parts: assistantParts,
    });
  }

  for (const toolResult of parsedToolResults) {
    outputMessages.push({
      role: 'tool',
      name: toolResult.name,
      content: toolResult.content,
      parts: [
        {
          type: 'tool_result',
          toolResult: {
            toolCallId: toolResult.toolCallId,
            name: toolResult.name,
            content: toolResult.content,
            contentJSON: toolResult.contentJSON,
            isError: toolResult.isError,
          },
        },
      ],
    });
  }

  return {
    output: outputMessages.length > 0 ? outputMessages : undefined,
    stepType,
    reasoningText: reasoningText.length > 0 ? reasoningText : undefined,
  };
}

export function parseToolCallStart(event: ToolCallStartEvent):
  | {
      toolCallId: string;
      toolName: string;
      input: unknown;
      toolType?: string;
      description?: string;
    }
  | undefined {
  const toolCall = asRecord(event.toolCall);
  const toolCallId = asString(toolCall?.toolCallId);
  if (toolCallId.length === 0) {
    return undefined;
  }

  const toolName = asString(toolCall?.toolName) || 'framework_tool';
  const toolType = asString(toolCall?.type);
  const description = asString(toolCall?.description);
  const input = toolCall?.input;

  return {
    toolCallId,
    toolName,
    input,
    toolType: toolType.length > 0 ? toolType : undefined,
    description: description.length > 0 ? description : undefined,
  };
}

export function parseToolCallFinish(event: ToolCallFinishEvent):
  | {
      toolCallId: string;
      success: boolean;
      output: unknown;
      error: unknown;
      durationMs?: number;
    }
  | undefined {
  const toolCall = asRecord(event.toolCall);
  const toolCallId = asString(toolCall?.toolCallId);
  if (toolCallId.length === 0) {
    return undefined;
  }

  const success = asBoolean(event.success, event.error === undefined);
  const durationMs = asNonNegativeInt(event.durationMs);

  return {
    toolCallId,
    success,
    output: event.output,
    error: event.error,
    durationMs,
  };
}

export function isTextChunk(event: StreamChunkEvent): boolean {
  const directType = asString(event.type);
  if (isTextChunkType(directType)) {
    return true;
  }
  const chunk = asRecord(event.chunk);
  return isTextChunkType(asString(chunk?.type));
}

function isTextChunkType(type: string): boolean {
  return type === 'text' || type === 'text-delta';
}

export function normalizeMetadata(raw: Record<string, unknown>): Record<string, unknown> {
  const output: Record<string, unknown> = {};
  const seen = new WeakSet<object>();
  for (const [key, value] of Object.entries(raw)) {
    const normalizedKey = key.trim();
    if (normalizedKey.length === 0) {
      continue;
    }
    const normalizedValue = normalizeMetadataValue(value, 0, seen);
    if (normalizedValue !== undefined) {
      output[normalizedKey] = normalizedValue;
    }
  }
  return output;
}

function normalizeMetadataValue(value: unknown, depth: number, seen: WeakSet<object>): unknown {
  if (depth > maxMetadataDepth || value === undefined) {
    return undefined;
  }

  if (value === null || typeof value === 'boolean' || typeof value === 'string') {
    return value;
  }

  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : undefined;
  }

  if (value instanceof Date) {
    return Number.isFinite(value.getTime()) ? value.toISOString() : undefined;
  }

  if (typeof value === 'function' || typeof value === 'symbol' || typeof value === 'bigint') {
    return undefined;
  }

  if (Array.isArray(value)) {
    const normalizedItems: unknown[] = [];
    for (const item of value) {
      const normalized = normalizeMetadataValue(item, depth + 1, seen);
      if (normalized !== undefined) {
        normalizedItems.push(normalized);
      }
    }
    return normalizedItems;
  }

  if (!isRecord(value)) {
    return undefined;
  }

  if (seen.has(value)) {
    return '[circular]';
  }

  seen.add(value);
  try {
    const output: Record<string, unknown> = {};
    for (const [key, nestedValue] of Object.entries(value)) {
      const normalizedKey = key.trim();
      if (normalizedKey.length === 0) {
        continue;
      }
      const normalizedValue = normalizeMetadataValue(nestedValue, depth + 1, seen);
      if (normalizedValue !== undefined) {
        output[normalizedKey] = normalizedValue;
      }
    }
    return output;
  } finally {
    seen.delete(value);
  }
}

function normalizeProvider(explicitProvider: string, modelName: string): string {
  const normalizedProvider = explicitProvider.trim().toLowerCase();
  const canonicalProvider = canonicalizeProvider(normalizedProvider);
  if (canonicalProvider !== undefined) {
    return canonicalProvider;
  }
  if (normalizedProvider.length > 0) {
    return 'custom';
  }
  return inferProviderFromModel(modelName);
}

function canonicalizeProvider(normalizedProvider: string): string | undefined {
  if (matchesProvider(normalizedProvider, 'openai')) {
    return 'openai';
  }
  if (matchesProvider(normalizedProvider, 'anthropic')) {
    return 'anthropic';
  }
  if (matchesProvider(normalizedProvider, 'gemini') || matchesProvider(normalizedProvider, 'google')) {
    return 'gemini';
  }
  return undefined;
}

function matchesProvider(value: string, providerPrefix: string): boolean {
  if (value === providerPrefix) {
    return true;
  }
  if (!value.startsWith(providerPrefix)) {
    return false;
  }
  const separator = value.charAt(providerPrefix.length);
  return separator === '.' || separator === ':' || separator === '/' || separator === '_' || separator === '-';
}

function inferProviderFromModel(modelName: string): string {
  const normalized = modelName.trim().toLowerCase();
  if (
    normalized.startsWith('gpt-') ||
    normalized.startsWith('o1') ||
    normalized.startsWith('o3') ||
    normalized.startsWith('o4')
  ) {
    return 'openai';
  }
  if (normalized.startsWith('claude-')) {
    return 'anthropic';
  }
  if (normalized.startsWith('gemini-')) {
    return 'gemini';
  }
  return 'custom';
}

function normalizeStepType(value: unknown): string | undefined {
  const normalized = asString(value).toLowerCase();
  if (normalized === 'initial' || normalized === 'continue' || normalized === 'tool-result') {
    return normalized;
  }
  return undefined;
}

function deriveStepType(params: {
  stepType: unknown;
  stepNumber: unknown;
  hasToolResults: boolean;
}): string | undefined {
  const explicit = normalizeStepType(params.stepType);
  if (explicit !== undefined) {
    return explicit;
  }
  if (params.hasToolResults) {
    return 'tool-result';
  }
  const stepNumber = asNonNegativeInt(params.stepNumber);
  if (stepNumber === 0) {
    return 'initial';
  }
  if (stepNumber !== undefined) {
    return 'continue';
  }
  return undefined;
}

function parseToolCalls(value: unknown): ParsedToolCall[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const output: ParsedToolCall[] = [];
  for (const rawToolCall of value) {
    const toolCall = asRecord(rawToolCall);
    if (toolCall === undefined) {
      continue;
    }
    const id = asString(toolCall.toolCallId) || asString(toolCall.callId) || asString(toolCall.id);
    const name = asString(toolCall.toolName) || asString(toolCall.name);
    if (name.length === 0) {
      continue;
    }
    output.push({
      id: id.length > 0 ? id : undefined,
      name,
      inputJSON: maybeJSONStringify(toolCall.input ?? toolCall.arguments),
    });
  }
  return output;
}

function parseToolResults(value: unknown): ParsedToolResult[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const output: ParsedToolResult[] = [];
  for (const rawToolResult of value) {
    const toolResult = asRecord(rawToolResult);
    if (toolResult === undefined) {
      continue;
    }

    const toolCallId = asString(toolResult.toolCallId) || asString(toolResult.callId) || asString(toolResult.id);
    const name = asString(toolResult.toolName) || asString(toolResult.name);
    const isError = asBoolean(toolResult.isError, false);
    const rawContent = toolResult.output ?? toolResult.result ?? toolResult.content;

    const content = typeof rawContent === 'string' ? rawContent.trim() : undefined;
    const contentJSON = content === undefined ? maybeJSONStringify(rawContent) : undefined;

    output.push({
      toolCallId: toolCallId.length > 0 ? toolCallId : undefined,
      name: name.length > 0 ? name : undefined,
      content,
      contentJSON,
      isError,
    });
  }
  return output;
}

function mapSingleMessage(rawMessage: unknown): Message | undefined {
  if (!isRecord(rawMessage)) {
    const text = asString(rawMessage);
    if (text.length === 0) {
      return undefined;
    }
    return {
      role: 'user',
      content: text,
    };
  }

  const role = normalizeRole(rawMessage.role ?? rawMessage.type);
  const name = asString(rawMessage.name);
  const messageName = name.length > 0 ? name : undefined;
  const content = rawMessage.content;

  if (typeof content === 'string') {
    const text = content.trim();
    if (text.length === 0) {
      return undefined;
    }
    return {
      role,
      name: messageName,
      content: text,
    };
  }

  const parts = mapMessageParts(content);
  const fallbackText = extractFallbackText(content);
  const textFromParts = extractTextFromParts(parts);
  const messageContent = fallbackText.length > 0 ? fallbackText : textFromParts;

  if (parts.length === 0 && messageContent.length === 0) {
    return undefined;
  }

  return {
    role,
    name: messageName,
    content: messageContent.length > 0 ? messageContent : undefined,
    parts: parts.length > 0 ? parts : undefined,
  };
}

function mapMessageParts(content: unknown): MessagePart[] {
  const rawParts = Array.isArray(content) ? content : isRecord(content) ? [content] : [];
  const parts: MessagePart[] = [];
  for (const rawPart of rawParts) {
    const mapped = mapSinglePart(rawPart);
    if (mapped !== undefined) {
      parts.push(mapped);
    }
  }
  return parts;
}

function mapSinglePart(rawPart: unknown): MessagePart | undefined {
  if (typeof rawPart === 'string') {
    const text = rawPart.trim();
    if (text.length === 0) {
      return undefined;
    }
    return {
      type: 'text',
      text,
    };
  }

  const part = asRecord(rawPart);
  if (part === undefined) {
    return undefined;
  }

  const type = asString(part.type).toLowerCase();
  const text = asString(part.text);
  if (type === 'text' || (type.length === 0 && text.length > 0)) {
    if (text.length === 0) {
      return undefined;
    }
    return {
      type: 'text',
      text,
    };
  }

  if (type === 'reasoning' || type === 'thinking') {
    const thinking = text;
    if (thinking.length === 0) {
      return undefined;
    }
    return {
      type: 'thinking',
      thinking,
    };
  }

  if (type === 'tool-call' || type === 'tool_call') {
    const name = asString(part.toolName) || asString(part.name);
    if (name.length === 0) {
      return undefined;
    }
    const id = asString(part.toolCallId) || asString(part.callId) || asString(part.id);
    const inputJSON = maybeJSONStringify(part.input ?? part.arguments);
    return {
      type: 'tool_call',
      toolCall: {
        id: id.length > 0 ? id : undefined,
        name,
        inputJSON,
      },
    };
  }

  if (type === 'tool-result' || type === 'tool_result') {
    const toolCallId = asString(part.toolCallId) || asString(part.callId) || asString(part.id);
    const name = asString(part.toolName) || asString(part.name);
    const rawResult = part.result ?? part.output ?? part.content;
    const content = typeof rawResult === 'string' ? rawResult.trim() : undefined;
    const contentJSON = content === undefined ? maybeJSONStringify(rawResult) : undefined;
    const isError = asBoolean(part.isError, false);
    return {
      type: 'tool_result',
      toolResult: {
        toolCallId: toolCallId.length > 0 ? toolCallId : undefined,
        name: name.length > 0 ? name : undefined,
        content,
        contentJSON,
        isError,
      },
    };
  }

  return undefined;
}

function extractFallbackText(content: unknown): string {
  if (!isRecord(content)) {
    return '';
  }
  return asString(content.text);
}

function extractTextFromParts(parts: MessagePart[]): string {
  const textParts = parts
    .filter((part): part is Extract<MessagePart, { type: 'text' }> => part.type === 'text')
    .map((part) => part.text.trim())
    .filter((part) => part.length > 0);
  return textParts.join(' ').trim();
}

function normalizeRole(value: unknown): string {
  const normalized = asString(value).toLowerCase();
  if (normalized === 'assistant' || normalized === 'ai') {
    return 'assistant';
  }
  if (normalized === 'tool') {
    return 'tool';
  }
  return 'user';
}

function maybeJSONStringify(value: unknown): string | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return undefined;
    }
    if (isJSON(trimmed)) {
      return trimmed;
    }
    return JSON.stringify(trimmed);
  }
  try {
    const encoded = JSON.stringify(value);
    if (encoded === undefined || encoded === 'null') {
      return undefined;
    }
    return encoded;
  } catch {
    return undefined;
  }
}

function numberFromCandidates(values: unknown[]): number | undefined {
  for (const value of values) {
    const parsed = asNonNegativeInt(value);
    if (parsed !== undefined) {
      return parsed;
    }
  }
  return undefined;
}

function asRecord(value: unknown): AnyRecord | undefined {
  return isRecord(value) ? value : undefined;
}

function isRecord(value: unknown): value is AnyRecord {
  return typeof value === 'object' && value !== null;
}

function asString(value: unknown): string {
  if (typeof value !== 'string') {
    return '';
  }
  return value.trim();
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  if (typeof value === 'boolean') {
    return value;
  }
  return fallback;
}

function asNonNegativeInt(value: unknown): number | undefined {
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) {
      return undefined;
    }
    if (value < 0) {
      return undefined;
    }
    return Math.trunc(value);
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return undefined;
    }
    const parsed = Number.parseInt(trimmed, 10);
    if (Number.isNaN(parsed) || parsed < 0) {
      return undefined;
    }
    return parsed;
  }
  return undefined;
}

function isJSON(value: string): boolean {
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}
