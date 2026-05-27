import type {
  Artifact,
  ExportGenerationResult,
  ExportGenerationsRequest,
  ExportGenerationsResponse,
  Generation,
  GenerationExporter,
  Message,
  MessagePart,
  TokenUsage,
  ToolDefinition,
} from '../types.js';
import { canonicalEffectiveVersion, isRecord } from '../utils.js';

export class HTTPGenerationExporter implements GenerationExporter {
  private readonly endpoint: string;
  private readonly headers: Record<string, string>;

  constructor(endpoint: string, headers?: Record<string, string>) {
    this.endpoint = normalizeHTTPGenerationEndpoint(endpoint);
    this.headers = headers ? { ...headers } : {};
  }

  async exportGenerations(request: ExportGenerationsRequest): Promise<ExportGenerationsResponse> {
    const response = await fetch(this.endpoint, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...this.headers,
      },
      body: JSON.stringify({
        generations: request.generations.map(mapGenerationToProtoJSON),
      }),
    });

    if (!response.ok) {
      const responseText = (await response.text()).trim();
      throw new Error(`http generation export status ${response.status}: ${responseText}`);
    }

    const payload = (await response.json()) as unknown;
    return parseExportGenerationsResponse(payload, request);
  }
}

function parseExportGenerationsResponse(
  payload: unknown,
  request: ExportGenerationsRequest,
): ExportGenerationsResponse {
  if (!isRecord(payload) || !Array.isArray(payload.results)) {
    throw new Error('invalid generation export response payload');
  }

  const results: ExportGenerationResult[] = payload.results.map((result, index) => {
    if (!isRecord(result)) {
      throw new Error('invalid generation export result payload');
    }

    const fallbackGenerationID = request.generations[index]?.id ?? '';
    return {
      generationId:
        typeof result.generationId === 'string'
          ? result.generationId
          : typeof result.generation_id === 'string'
            ? result.generation_id
            : fallbackGenerationID,
      accepted: Boolean(result.accepted),
      error: typeof result.error === 'string' ? result.error : undefined,
    };
  });

  return { results };
}

const HTTP_GENERATION_EXPORT_PATH = '/api/v1/generations:export';

/** @internal Exported for tests; not part of the public API. */
export function normalizeHTTPGenerationEndpoint(endpoint: string): string {
  const trimmed = endpoint.trim();
  if (trimmed.length === 0) {
    throw new Error('generation export endpoint is required');
  }

  const lowerPrefix = trimmed.slice(0, 8).toLowerCase();
  const hasScheme = lowerPrefix.startsWith('http://') || lowerPrefix.startsWith('https://');
  const withScheme = hasScheme ? trimmed : `http://${trimmed}`;

  let url: URL;
  try {
    url = new URL(withScheme);
  } catch (e) {
    const detail = e instanceof Error ? e.message : String(e);
    throw new Error(`parse generation export endpoint "${endpoint}": ${detail}`);
  }
  if (url.pathname === '' || url.pathname === '/') {
    url.pathname = HTTP_GENERATION_EXPORT_PATH;
  }
  return url.toString();
}

function mapGenerationToProtoJSON(generation: Generation): Record<string, unknown> {
  const proto: Record<string, unknown> = {
    id: generation.id,
    conversation_id: generation.conversationId ?? '',
    operation_name: generation.operationName,
    mode: generation.mode === 'STREAM' ? 'GENERATION_MODE_STREAM' : 'GENERATION_MODE_SYNC',
    trace_id: generation.traceId ?? '',
    span_id: generation.spanId ?? '',
    model: {
      provider: generation.model.provider,
      name: generation.model.name,
    },
    response_id: generation.responseId ?? '',
    response_model: generation.responseModel ?? '',
    system_prompt: generation.systemPrompt ?? '',
    input: (generation.input ?? []).map(mapMessageToProtoJSON),
    output: (generation.output ?? []).map(mapMessageToProtoJSON),
    tools: (generation.tools ?? []).map(mapToolToProtoJSON),
    usage: mapUsageToProtoJSON(generation.usage),
    stop_reason: generation.stopReason ?? '',
    started_at: generation.startedAt.toISOString(),
    completed_at: generation.completedAt.toISOString(),
    tags: generation.tags ?? {},
    metadata: normalizeMetadata(generation.metadata),
    raw_artifacts: (generation.artifacts ?? []).map(mapArtifactToProtoJSON),
    call_error: generation.callError ?? '',
    agent_name: generation.agentName ?? '',
    agent_version: generation.agentVersion ?? '',
  };

  if (generation.maxTokens !== undefined) {
    proto.max_tokens = toInt64String(generation.maxTokens);
  }
  if (generation.temperature !== undefined) {
    proto.temperature = generation.temperature;
  }
  if (generation.topP !== undefined) {
    proto.top_p = generation.topP;
  }
  if (generation.toolChoice !== undefined) {
    proto.tool_choice = generation.toolChoice;
  }
  if (generation.thinkingEnabled !== undefined) {
    proto.thinking_enabled = generation.thinkingEnabled;
  }
  if (generation.parentGenerationIds?.length) {
    proto.parent_generation_ids = generation.parentGenerationIds;
  }
  const effectiveVersion = canonicalEffectiveVersion(generation.effectiveVersion);
  if (effectiveVersion !== undefined) {
    proto.effective_version = effectiveVersion;
  }

  return proto;
}

function mapMessageToProtoJSON(message: Message): Record<string, unknown> {
  const parts = (message.parts ?? []).map(mapMessagePartToProtoJSON);
  if (parts.length === 0 && typeof message.content === 'string') {
    parts.push({ text: message.content });
  }

  return {
    role: toMessageRoleEnum(message.role),
    name: message.name ?? '',
    parts,
  };
}

function mapMessagePartToProtoJSON(part: MessagePart): Record<string, unknown> {
  switch (part.type) {
    case 'text':
      return withPartMetadata(
        {
          text: part.text,
        },
        part.metadata?.providerType,
      );
    case 'thinking':
      return withPartMetadata(
        {
          thinking: part.thinking,
        },
        part.metadata?.providerType,
      );
    case 'tool_call':
      return withPartMetadata(
        {
          tool_call: {
            id: part.toolCall.id ?? '',
            name: part.toolCall.name,
            input_json: toBase64Payload(part.toolCall.inputJSON),
          },
        },
        part.metadata?.providerType,
      );
    case 'tool_result':
      return withPartMetadata(
        {
          tool_result: {
            tool_call_id: part.toolResult.toolCallId ?? '',
            name: part.toolResult.name ?? '',
            content: part.toolResult.content ?? '',
            content_json: toBase64Payload(part.toolResult.contentJSON),
            is_error: part.toolResult.isError ?? false,
          },
        },
        part.metadata?.providerType,
      );
  }
}

function mapToolToProtoJSON(tool: ToolDefinition): Record<string, unknown> {
  return {
    name: tool.name,
    description: tool.description ?? '',
    type: tool.type ?? '',
    input_schema_json: toBase64Payload(tool.inputSchemaJSON),
  };
}

function mapUsageToProtoJSON(usage: TokenUsage | undefined): Record<string, unknown> | undefined {
  if (usage === undefined) {
    return undefined;
  }

  const inputTokens = usage.inputTokens ?? 0;
  const outputTokens = usage.outputTokens ?? 0;
  const totalTokens = usage.totalTokens ?? inputTokens + outputTokens;

  return {
    input_tokens: toInt64String(inputTokens),
    output_tokens: toInt64String(outputTokens),
    total_tokens: toInt64String(totalTokens),
    cache_read_input_tokens: toInt64String(usage.cacheReadInputTokens),
    cache_write_input_tokens: toInt64String(usage.cacheWriteInputTokens),
    reasoning_tokens: toInt64String(usage.reasoningTokens),
  };
}

function mapArtifactToProtoJSON(artifact: Artifact): Record<string, unknown> {
  return {
    kind: toArtifactKindEnum(artifact.type),
    name: artifact.name ?? artifact.type,
    content_type: artifact.mimeType ?? 'application/json',
    payload: toBase64Payload(artifact.payload),
    record_id: artifact.recordId ?? '',
    uri: artifact.uri ?? '',
  };
}

function withPartMetadata(part: Record<string, unknown>, providerType: string | undefined): Record<string, unknown> {
  if (providerType === undefined || providerType.trim().length === 0) {
    return part;
  }
  return {
    ...part,
    metadata: {
      provider_type: providerType,
    },
  };
}

function normalizeMetadata(metadata: Record<string, unknown> | undefined): Record<string, unknown> {
  if (metadata === undefined) {
    return {};
  }

  try {
    const encoded = JSON.stringify(metadata, (_key, value) => {
      if (value instanceof Date) {
        return value.toISOString();
      }
      if (typeof value === 'bigint') {
        return value.toString();
      }
      return value;
    });
    if (encoded === undefined) {
      return {};
    }
    const decoded = JSON.parse(encoded) as unknown;
    if (!isRecord(decoded)) {
      return {};
    }
    return decoded;
  } catch {
    return {};
  }
}

function toBase64Payload(value: string | undefined): string {
  if (value === undefined || value.length === 0) {
    return '';
  }
  const bytes = new TextEncoder().encode(value);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

function toInt64String(value: number | undefined): string {
  if (value === undefined || Number.isNaN(value) || !Number.isFinite(value)) {
    return '0';
  }
  return Math.trunc(value).toString();
}

function toMessageRoleEnum(role: string): string {
  switch (String(role).trim().toLowerCase()) {
    case 'assistant':
      return 'MESSAGE_ROLE_ASSISTANT';
    case 'tool':
      return 'MESSAGE_ROLE_TOOL';
    case 'user':
    default:
      return 'MESSAGE_ROLE_USER';
  }
}

function toArtifactKindEnum(kind: string): string {
  switch (String(kind).trim().toLowerCase()) {
    case 'request':
      return 'ARTIFACT_KIND_REQUEST';
    case 'response':
      return 'ARTIFACT_KIND_RESPONSE';
    case 'tools':
      return 'ARTIFACT_KIND_TOOLS';
    case 'provider_event':
      return 'ARTIFACT_KIND_PROVIDER_EVENT';
    default:
      return 'ARTIFACT_KIND_UNSPECIFIED';
  }
}
