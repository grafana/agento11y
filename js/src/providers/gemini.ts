import type { Content, GenerateContentConfig, GenerateContentResponse } from '@google/genai';
import type { Agento11yClient } from '../client.js';
import type { EmbeddingResult, GenerationResult, Message, TokenUsage, ToolDefinition } from '../types.js';

const thinkingBudgetMetadataKey = 'agento11y.gen_ai.request.thinking.budget_tokens';
const thinkingLevelMetadataKey = 'agento11y.gen_ai.request.thinking.level';
const usageToolUsePromptTokensMetadataKey = 'agento11y.gen_ai.usage.tool_use_prompt_tokens';
type AnyRecord = Record<string, unknown>;

type GeminiContent = Content & AnyRecord;
type GeminiContents = Array<GeminiContent | string>;
type GeminiConfig = GenerateContentConfig & AnyRecord;
type GeminiResponse = GenerateContentResponse & AnyRecord;
type GeminiEmbedConfig = AnyRecord;
type GeminiEmbedResponse = AnyRecord;

/** Optional agento11y fields applied during Gemini helper mapping. */
export interface GeminiOptions {
  conversationId?: string;
  agentName?: string;
  agentVersion?: string;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  rawArtifacts?: boolean;
}

/** Streaming summary accepted by Gemini models stream wrapper. */
export interface ModelsStreamSummary {
  responses?: GeminiResponse[];
  finalResponse?: GeminiResponse;
  outputText?: string;
  firstChunkAt?: Date | string | number;
}

async function geminiGenerateContent(
  client: Agento11yClient,
  model: string,
  contents: GeminiContents,
  config: GeminiConfig | undefined,
  providerCall: (model: string, contents: GeminiContents, config: GeminiConfig | undefined) => Promise<GeminiResponse>,
  options: GeminiOptions = {},
): Promise<GeminiResponse> {
  const controls = mapGeminiRequestControls(config);

  return client.startGeneration(
    {
      conversationId: options.conversationId,
      agentName: options.agentName,
      agentVersion: options.agentVersion,
      model: {
        provider: 'gemini',
        name: model,
      },
      systemPrompt: extractGeminiSystemPrompt(config),
      maxTokens: controls.maxTokens,
      temperature: controls.temperature,
      topP: controls.topP,
      toolChoice: controls.toolChoice,
      thinkingEnabled: controls.thinkingEnabled,
      tools: mapGeminiTools(config),
      tags: options.tags,
      metadata: metadataWithThinkingBudget(options.metadata, controls.thinkingBudget, controls.thinkingLevel),
    },
    async (recorder) => {
      const response = await providerCall(model, contents, config);
      recorder.setResult(geminiFromRequestResponse(model, contents, config, response, options));
      return response;
    },
  );
}

async function geminiGenerateContentStream(
  client: Agento11yClient,
  model: string,
  contents: GeminiContents,
  config: GeminiConfig | undefined,
  providerCall: (
    model: string,
    contents: GeminiContents,
    config: GeminiConfig | undefined,
  ) => Promise<ModelsStreamSummary>,
  options: GeminiOptions = {},
): Promise<ModelsStreamSummary> {
  const controls = mapGeminiRequestControls(config);

  return client.startStreamingGeneration(
    {
      conversationId: options.conversationId,
      agentName: options.agentName,
      agentVersion: options.agentVersion,
      model: {
        provider: 'gemini',
        name: model,
      },
      systemPrompt: extractGeminiSystemPrompt(config),
      maxTokens: controls.maxTokens,
      temperature: controls.temperature,
      topP: controls.topP,
      toolChoice: controls.toolChoice,
      thinkingEnabled: controls.thinkingEnabled,
      tools: mapGeminiTools(config),
      tags: options.tags,
      metadata: metadataWithThinkingBudget(options.metadata, controls.thinkingBudget, controls.thinkingLevel),
    },
    async (recorder) => {
      const summary = await providerCall(model, contents, config);
      const firstChunkAt = asDate(summary.firstChunkAt);
      if (firstChunkAt !== undefined) {
        recorder.setFirstTokenAt(firstChunkAt);
      }
      recorder.setResult(geminiFromStream(model, contents, config, summary, options));
      return summary;
    },
  );
}

async function geminiEmbedContent(
  client: Agento11yClient,
  model: string,
  contents: GeminiContents,
  config: GeminiEmbedConfig | undefined,
  providerCall: (
    model: string,
    contents: GeminiContents,
    config: GeminiEmbedConfig | undefined,
  ) => Promise<GeminiEmbedResponse>,
  options: GeminiOptions = {},
): Promise<GeminiEmbedResponse> {
  const requestedDimensions =
    readIntFromAny((config as AnyRecord | undefined)?.outputDimensionality) ??
    readIntFromAny((config as AnyRecord | undefined)?.output_dimensionality);

  return client.startEmbedding(
    {
      agentName: options.agentName,
      agentVersion: options.agentVersion,
      model: {
        provider: 'gemini',
        name: model,
      },
      dimensions: requestedDimensions,
      tags: options.tags,
      metadata: options.metadata,
    },
    async (recorder) => {
      const response = await providerCall(model, contents, config);
      recorder.setResult(geminiEmbeddingFromResponse(model, contents, config, response));
      return response;
    },
  );
}

function geminiEmbeddingFromResponse(
  _model: string,
  contents: GeminiContents,
  config: GeminiEmbedConfig | undefined,
  response: GeminiEmbedResponse | undefined,
): EmbeddingResult {
  const result: EmbeddingResult = {
    inputCount: embeddingInputCount(contents),
    inputTexts: embeddingInputTexts(contents),
  };

  const requestedDimensions =
    readIntFromAny((config as AnyRecord | undefined)?.outputDimensionality) ??
    readIntFromAny((config as AnyRecord | undefined)?.output_dimensionality);

  if (!isRecord(response)) {
    if (requestedDimensions !== undefined && requestedDimensions > 0) {
      result.dimensions = requestedDimensions;
    }
    return result;
  }

  const embeddings = Array.isArray(response.embeddings) ? response.embeddings : [];
  let inputTokens = 0;
  for (const embedding of embeddings) {
    if (!isRecord(embedding)) {
      continue;
    }
    const statistics = isRecord(embedding.statistics) ? embedding.statistics : undefined;
    const tokenCount = readIntFromAny(statistics?.tokenCount) ?? readIntFromAny(statistics?.token_count);
    if (tokenCount !== undefined && tokenCount > 0) {
      inputTokens += tokenCount;
    }
    if (result.dimensions === undefined && Array.isArray(embedding.values) && embedding.values.length > 0) {
      result.dimensions = embedding.values.length;
    }
  }
  if (inputTokens > 0) {
    result.inputTokens = inputTokens;
  }
  if (result.dimensions === undefined && requestedDimensions !== undefined && requestedDimensions > 0) {
    result.dimensions = requestedDimensions;
  }
  return result;
}

function geminiFromRequestResponse(
  model: string,
  contents: GeminiContents,
  config: GeminiConfig | undefined,
  response: GeminiResponse,
  options: GeminiOptions = {},
): GenerationResult {
  const controls = mapGeminiRequestControls(config);
  const output = mapGeminiResponseOutput(response);
  const usageMetadata = geminiUsageMetadata((response as AnyRecord).usageMetadata);

  const result: GenerationResult = {
    responseId: asString((response as AnyRecord).responseId),
    responseModel: asString((response as AnyRecord).modelVersion) || model,
    maxTokens: controls.maxTokens,
    temperature: controls.temperature,
    topP: controls.topP,
    toolChoice: controls.toolChoice,
    thinkingEnabled: controls.thinkingEnabled,
    input: mapGeminiInput(contents),
    output,
    tools: mapGeminiTools(config),
    usage: mapGeminiUsage((response as AnyRecord).usageMetadata),
    stopReason: mapGeminiStopReason(response),
    metadata: mergeMetadata(
      metadataWithThinkingBudget(options.metadata, controls.thinkingBudget, controls.thinkingLevel),
      usageMetadata,
    ),
    tags: options.tags ? { ...options.tags } : undefined,
  };

  if (options.rawArtifacts) {
    result.artifacts = [
      jsonArtifact('request', 'gemini.models.request', { model, contents, config }),
      jsonArtifact('response', 'gemini.models.response', response),
    ];
    if ((result.tools ?? []).length > 0) {
      result.artifacts.push(jsonArtifact('tools', 'gemini.models.tools', result.tools));
    }
  }

  return result;
}

function geminiFromStream(
  model: string,
  contents: GeminiContents,
  config: GeminiConfig | undefined,
  summary: ModelsStreamSummary,
  options: GeminiOptions = {},
): GenerationResult {
  const controls = mapGeminiRequestControls(config);
  const responses = summary.responses ?? [];
  const finalResponse = summary.finalResponse ?? (responses.length > 0 ? responses[responses.length - 1] : undefined);

  const outputText = summary.outputText ?? extractGeminiStreamText(responses);
  const fallbackOutput: Message[] = outputText.length > 0 ? [{ role: 'assistant', content: outputText }] : [];
  const streamUsageMetadata = geminiStreamUsageMetadata(responses);

  const result: GenerationResult = finalResponse
    ? {
        ...geminiFromRequestResponse(model, contents, config, finalResponse, options),
        output:
          mapGeminiResponseOutput(finalResponse).length > 0 ? mapGeminiResponseOutput(finalResponse) : fallbackOutput,
      }
    : {
        responseModel: model,
        maxTokens: controls.maxTokens,
        temperature: controls.temperature,
        topP: controls.topP,
        toolChoice: controls.toolChoice,
        thinkingEnabled: controls.thinkingEnabled,
        input: mapGeminiInput(contents),
        output: fallbackOutput,
        tools: mapGeminiTools(config),
        metadata: mergeMetadata(
          metadataWithThinkingBudget(options.metadata, controls.thinkingBudget, controls.thinkingLevel),
          streamUsageMetadata,
        ),
        tags: options.tags ? { ...options.tags } : undefined,
      };

  if (options.rawArtifacts) {
    const existing = result.artifacts ?? [];
    if (!existing.some((artifact) => artifact.type === 'request')) {
      existing.push(jsonArtifact('request', 'gemini.models.request', { model, contents, config }));
    }
    if ((result.tools ?? []).length > 0 && !existing.some((artifact) => artifact.type === 'tools')) {
      existing.push(jsonArtifact('tools', 'gemini.models.tools', result.tools));
    }
    existing.push(jsonArtifact('provider_event', 'gemini.models.stream_events', responses));
    result.artifacts = existing;
  }

  return result;
}

export const models = {
  generateContent: geminiGenerateContent,
  generateContentStream: geminiGenerateContentStream,
  embedContent: geminiEmbedContent,
  fromRequestResponse: geminiFromRequestResponse,
  fromStream: geminiFromStream,
  embeddingFromResponse: geminiEmbeddingFromResponse,
} as const;

function embeddingInputCount(contents: GeminiContents): number {
  let count = 0;
  for (const content of contents) {
    if (content !== undefined && content !== null) {
      count += 1;
    }
  }
  return count;
}

function embeddingInputTexts(contents: GeminiContents): string[] | undefined {
  const output: string[] = [];
  for (const content of contents) {
    if (typeof content === 'string') {
      const text = content.trim();
      if (text.length > 0) {
        output.push(text);
      }
      continue;
    }
    if (!isRecord(content)) {
      continue;
    }
    const text = extractText(content.parts);
    if (text.length > 0) {
      output.push(text);
    }
  }
  return output.length > 0 ? output : undefined;
}

function mapGeminiInput(contents: GeminiContents): Message[] {
  const input: Message[] = [];

  for (const rawContent of contents) {
    if (typeof rawContent === 'string') {
      const text = rawContent.trim();
      if (text.length > 0) {
        input.push({ role: 'user', content: text });
      }
      continue;
    }

    if (!isRecord(rawContent)) {
      continue;
    }

    const role = normalizeRole(asString(rawContent.role));
    const parts = mapGeminiParts(rawContent.parts, role);
    const text = extractText(rawContent.parts);

    if (parts.length > 0) {
      const hasToolResult = parts.some((part) => part.type === 'tool_result');
      input.push({
        role: hasToolResult ? 'tool' : role,
        content: text || undefined,
        parts,
      });
      continue;
    }

    if (text.length > 0) {
      input.push({ role, content: text });
    }
  }

  return input;
}

function mapGeminiResponseOutput(response: GeminiResponse): Message[] {
  const output: Message[] = [];
  const candidates = Array.isArray((response as AnyRecord).candidates)
    ? ((response as AnyRecord).candidates as unknown[])
    : [];

  for (const rawCandidate of candidates) {
    if (!isRecord(rawCandidate) || !isRecord(rawCandidate.content)) {
      continue;
    }

    const role = normalizeRole(asString(rawCandidate.content.role) || 'assistant');
    const parts = mapGeminiParts(rawCandidate.content.parts, role);
    const text = extractText(rawCandidate.content.parts);

    if (parts.length === 0 && text.length === 0) {
      continue;
    }

    output.push({
      role,
      content: text || undefined,
      parts: parts.length > 0 ? parts : undefined,
    });
  }

  return output;
}

function mapGeminiParts(rawParts: unknown, role: Message['role']): NonNullable<Message['parts']> {
  if (!Array.isArray(rawParts)) {
    return [];
  }

  const parts: NonNullable<Message['parts']> = [];

  for (const rawPart of rawParts) {
    if (!isRecord(rawPart)) {
      continue;
    }

    if (typeof rawPart.text === 'string' && rawPart.text.trim().length > 0) {
      if (rawPart.thought === true && role === 'assistant') {
        parts.push({
          type: 'thinking',
          thinking: rawPart.text,
          metadata: { providerType: 'thought' },
        });
      } else {
        parts.push({
          type: 'text',
          text: rawPart.text,
          metadata: { providerType: 'text' },
        });
      }
    }

    if (isRecord(rawPart.functionCall)) {
      const name = asString(rawPart.functionCall.name);
      if (name.length > 0) {
        parts.push({
          type: 'tool_call',
          toolCall: {
            id: asString(rawPart.functionCall.id) || undefined,
            name,
            inputJSON: jsonString(rawPart.functionCall.args),
          },
          metadata: { providerType: 'function_call' },
        });
      }
    }

    if (isRecord(rawPart.functionResponse)) {
      const responseValue = rawPart.functionResponse.response;
      parts.push({
        type: 'tool_result',
        toolResult: {
          toolCallId: asString(rawPart.functionResponse.id) || undefined,
          name: asString(rawPart.functionResponse.name) || undefined,
          content: extractText(responseValue) || undefined,
          contentJSON: jsonString(responseValue),
          isError: typeof rawPart.functionResponse.isError === 'boolean' ? rawPart.functionResponse.isError : undefined,
        },
        metadata: { providerType: 'function_response' },
      });
    }
  }

  return parts;
}

function mapGeminiTools(config: GeminiConfig | undefined): ToolDefinition[] {
  if (!isRecord(config) || !Array.isArray(config.tools)) {
    return [];
  }

  const out: ToolDefinition[] = [];

  for (const rawTool of config.tools) {
    if (!isRecord(rawTool) || !Array.isArray(rawTool.functionDeclarations)) {
      continue;
    }

    for (const rawDeclaration of rawTool.functionDeclarations) {
      if (!isRecord(rawDeclaration)) {
        continue;
      }

      const name = asString(rawDeclaration.name);
      if (name.length === 0) {
        continue;
      }

      out.push({
        name,
        description: asString(rawDeclaration.description) || undefined,
        type: 'function',
        inputSchemaJSON: hasValue(rawDeclaration.parametersJsonSchema)
          ? jsonString(rawDeclaration.parametersJsonSchema)
          : undefined,
      });
    }
  }

  return out;
}

function mapGeminiUsage(rawUsage: unknown): TokenUsage | undefined {
  if (!isRecord(rawUsage)) {
    return undefined;
  }

  const inputTokens = readIntFromAny(rawUsage.promptTokenCount);
  const outputTokens = readIntFromAny(rawUsage.candidatesTokenCount);
  const totalTokens = readIntFromAny(rawUsage.totalTokenCount);
  const cacheReadInputTokens = readIntFromAny(rawUsage.cachedContentTokenCount);
  const cacheWriteInputTokens = readIntFromAny(rawUsage.cacheCreationInputTokenCount);
  const toolUsePromptTokens = readIntFromAny(rawUsage.toolUsePromptTokenCount);
  const reasoningTokens = readIntFromAny(rawUsage.thoughtsTokenCount);

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
    out.totalTokens = (inputTokens ?? 0) + (outputTokens ?? 0) + (toolUsePromptTokens ?? 0) + (reasoningTokens ?? 0);
  }
  if (cacheReadInputTokens !== undefined) {
    out.cacheReadInputTokens = cacheReadInputTokens;
  }
  if (cacheWriteInputTokens !== undefined) {
    out.cacheWriteInputTokens = cacheWriteInputTokens;
  }
  if (reasoningTokens !== undefined) {
    out.reasoningTokens = reasoningTokens;
  }

  return Object.keys(out).length > 0 ? out : undefined;
}

function geminiUsageMetadata(rawUsage: unknown): Record<string, unknown> | undefined {
  if (!isRecord(rawUsage)) {
    return undefined;
  }

  const toolUsePromptTokens = readIntFromAny(rawUsage.toolUsePromptTokenCount ?? rawUsage.tool_use_prompt_token_count);
  if (toolUsePromptTokens === undefined || toolUsePromptTokens <= 0) {
    return undefined;
  }

  return {
    [usageToolUsePromptTokensMetadataKey]: toolUsePromptTokens,
  };
}

function mapGeminiStopReason(response: GeminiResponse): string | undefined {
  const candidates = Array.isArray((response as AnyRecord).candidates)
    ? ((response as AnyRecord).candidates as unknown[])
    : [];

  let stopReason: string | undefined;
  for (const rawCandidate of candidates) {
    if (!isRecord(rawCandidate)) {
      continue;
    }
    const candidateStopReason = asString(rawCandidate.finishReason);
    if (candidateStopReason.length > 0) {
      stopReason = candidateStopReason.toUpperCase();
    }
  }

  return stopReason;
}

function mapGeminiRequestControls(config: GeminiConfig | undefined): {
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingEnabled?: boolean;
  thinkingBudget?: number;
  thinkingLevel?: string;
} {
  if (!isRecord(config)) {
    return {};
  }

  const toolConfig = isRecord(config.toolConfig) ? config.toolConfig : undefined;
  const functionCallingConfig =
    toolConfig && isRecord(toolConfig.functionCallingConfig) ? toolConfig.functionCallingConfig : undefined;
  const thinkingConfig = isRecord(config.thinkingConfig) ? config.thinkingConfig : undefined;

  return {
    maxTokens: readIntFromAny(config.maxOutputTokens),
    temperature: readNumberFromAny(config.temperature),
    topP: readNumberFromAny(config.topP),
    toolChoice: canonicalToolChoice(functionCallingConfig?.mode),
    thinkingEnabled: typeof thinkingConfig?.includeThoughts === 'boolean' ? thinkingConfig.includeThoughts : undefined,
    thinkingBudget: readIntFromAny(thinkingConfig?.thinkingBudget),
    thinkingLevel: geminiThinkingLevel(thinkingConfig?.thinkingLevel),
  };
}

function extractGeminiSystemPrompt(config: GeminiConfig | undefined): string | undefined {
  if (!isRecord(config)) {
    return undefined;
  }

  const instruction = config.systemInstruction;
  if (!hasValue(instruction)) {
    return undefined;
  }

  if (typeof instruction === 'string') {
    const text = instruction.trim();
    return text.length > 0 ? text : undefined;
  }

  if (isRecord(instruction) && Array.isArray(instruction.parts)) {
    const chunks = instruction.parts
      .map((part) => {
        if (!isRecord(part)) {
          return '';
        }
        return typeof part.text === 'string' ? part.text.trim() : '';
      })
      .filter((chunk) => chunk.length > 0);

    if (chunks.length > 0) {
      return chunks.join('\n');
    }
  }

  const fallback = extractText(instruction);
  return fallback.length > 0 ? fallback : undefined;
}

function extractGeminiStreamText(responses: GeminiResponse[]): string {
  const chunks: string[] = [];

  for (const response of responses) {
    const output = mapGeminiResponseOutput(response);
    for (const message of output) {
      if (typeof message.content === 'string' && message.content.trim().length > 0) {
        chunks.push(message.content.trim());
      }
    }
  }

  return chunks.join('\n');
}

function normalizeRole(value: string): Message['role'] {
  const normalized = value.trim().toLowerCase();
  if (normalized === 'assistant' || normalized === 'model') {
    return 'assistant';
  }
  if (normalized === 'tool') {
    return 'tool';
  }
  return 'user';
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

function metadataWithThinkingBudget(
  metadata: Record<string, unknown> | undefined,
  thinkingBudget: number | undefined,
  thinkingLevel: string | undefined,
): Record<string, unknown> | undefined {
  if (thinkingBudget === undefined && thinkingLevel === undefined) {
    return metadata ? { ...metadata } : undefined;
  }
  const out = metadata ? { ...metadata } : {};
  if (thinkingBudget !== undefined) {
    out[thinkingBudgetMetadataKey] = thinkingBudget;
  }
  if (thinkingLevel !== undefined) {
    out[thinkingLevelMetadataKey] = thinkingLevel;
  }
  return out;
}

function geminiThinkingLevel(value: unknown): string | undefined {
  const raw = asString(value).toLowerCase();
  if (raw.length === 0 || raw === 'thinking_level_unspecified') {
    return undefined;
  }
  if (raw === 'thinking_level_low' || raw === 'low') {
    return 'low';
  }
  if (raw === 'thinking_level_medium' || raw === 'medium') {
    return 'medium';
  }
  if (raw === 'thinking_level_high' || raw === 'high') {
    return 'high';
  }
  if (raw === 'thinking_level_minimal' || raw === 'minimal') {
    return 'minimal';
  }
  return raw;
}

function geminiStreamUsageMetadata(responses: GeminiResponse[]): Record<string, unknown> | undefined {
  for (let index = responses.length - 1; index >= 0; index -= 1) {
    const metadata = geminiUsageMetadata((responses[index] as AnyRecord).usageMetadata);
    if (metadata !== undefined) {
      return metadata;
    }
  }
  return undefined;
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
    if ('content' in value && value.content !== undefined && value.content !== null) {
      return extractText(value.content);
    }
  }

  return String(value).trim();
}

function asString(value: unknown): string {
  if (typeof value === 'string') {
    return value.trim();
  }
  return value === undefined || value === null ? '' : String(value).trim();
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
