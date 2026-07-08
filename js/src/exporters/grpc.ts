import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import type {
  Artifact,
  ExportGenerationsRequest,
  ExportGenerationsResponse,
  ExportWorkflowStepsRequest,
  ExportWorkflowStepsResponse,
  Generation,
  GenerationExporter,
  Message,
  MessagePart,
  TokenUsage,
  ToolDefinition,
  WorkflowStep,
} from '../types.js';
import { canonicalEffectiveVersion } from '../utils.js';
import { userAgent } from '../version.js';

type ExportGenerationsMethod = (
  request: unknown,
  metadata: grpc.Metadata,
  callback: (error: grpc.ServiceError | null, response: unknown) => void,
) => void;

type ExportWorkflowStepsMethod = (
  request: unknown,
  metadata: grpc.Metadata,
  callback: (error: grpc.ServiceError | null, response: unknown) => void,
) => void;

type GRPCGenerationServiceClient = grpc.Client & {
  ExportGenerations?: ExportGenerationsMethod;
};

type GRPCWorkflowStepServiceClient = grpc.Client & {
  ExportWorkflowSteps?: ExportWorkflowStepsMethod;
};

type LoadedGRPCPackage = {
  sigil?: {
    v1?: {
      GenerationIngestService?: grpc.ServiceClientConstructor;
      WorkflowStepIngestService?: grpc.ServiceClientConstructor;
    };
  };
};

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const defaultProtoPath = join(__dirname, '../../proto/sigil/v1/generation_ingest.proto');

const protoLoadOptions: protoLoader.Options = {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: false,
  oneofs: true,
};

export class GRPCGenerationExporter implements GenerationExporter {
  private readonly endpoint: string;
  private readonly headers: Record<string, string>;
  private readonly insecure: boolean;
  private readonly userAgent: string;

  private initPromise: Promise<void> | undefined;
  private generationClient: GRPCGenerationServiceClient | undefined;
  private workflowStepClient: GRPCWorkflowStepServiceClient | undefined;

  constructor(endpoint: string, headers?: Record<string, string>, insecure = false) {
    const parsed = parseGRPCEndpoint(endpoint);
    this.endpoint = parsed.host;
    this.insecure = insecure || parsed.insecure;
    // gRPC reserves the user-agent metadata key, so the User-Agent must travel
    // via the channel option rather than per-call metadata. grpc-js appends its
    // own token after this value.
    const split = splitUserAgent(headers);
    this.userAgent = split.userAgent;
    this.headers = split.rest;
  }

  async exportGenerations(request: ExportGenerationsRequest): Promise<ExportGenerationsResponse> {
    await this.ensureClient();
    const client = this.generationClient;
    if (client === undefined || typeof client.ExportGenerations !== 'function') {
      throw new Error('grpc exporter client is unavailable');
    }

    const metadata = new grpc.Metadata();
    for (const [key, value] of Object.entries(this.headers)) {
      metadata.set(key, value);
    }

    const grpcRequest = {
      generations: request.generations.map(mapGenerationToProto),
    };

    const response = await new Promise<unknown>((resolve, reject) => {
      client.ExportGenerations?.(grpcRequest, metadata, (error, result) => {
        if (error !== null) {
          reject(error);
          return;
        }
        resolve(result);
      });
    });

    return parseGRPCExportResponse(response, request);
  }

  async exportWorkflowSteps(request: ExportWorkflowStepsRequest): Promise<ExportWorkflowStepsResponse> {
    await this.ensureClient();
    const client = this.workflowStepClient;
    if (client === undefined || typeof client.ExportWorkflowSteps !== 'function') {
      throw new Error('grpc workflow-step exporter client is unavailable');
    }

    const metadata = new grpc.Metadata();
    for (const [key, value] of Object.entries(this.headers)) {
      metadata.set(key, value);
    }

    const grpcRequest = {
      workflowSteps: request.workflowSteps.map(mapWorkflowStepToProto),
    };

    const response = await new Promise<unknown>((resolve, reject) => {
      client.ExportWorkflowSteps?.(grpcRequest, metadata, (error, result) => {
        if (error !== null) {
          reject(error);
          return;
        }
        resolve(result);
      });
    });

    return parseGRPCWorkflowStepExportResponse(response, request);
  }

  async shutdown(): Promise<void> {
    if (this.generationClient !== undefined) {
      this.generationClient.close();
      this.generationClient = undefined;
    }
    if (this.workflowStepClient !== undefined) {
      this.workflowStepClient.close();
      this.workflowStepClient = undefined;
    }
  }

  private async ensureClient(): Promise<void> {
    if (this.generationClient !== undefined && this.workflowStepClient !== undefined) {
      return;
    }
    if (this.initPromise !== undefined) {
      await this.initPromise;
      return;
    }

    this.initPromise = this.initializeClient();
    await this.initPromise;
    this.initPromise = undefined;
  }

  private async initializeClient(): Promise<void> {
    const packageDefinition = await protoLoader.load(defaultProtoPath, protoLoadOptions);
    const loaded = grpc.loadPackageDefinition(packageDefinition) as unknown as LoadedGRPCPackage;
    const generationClientCtor = loaded.sigil?.v1?.GenerationIngestService;
    if (generationClientCtor === undefined) {
      throw new Error('failed to load sigil.v1.GenerationIngestService from proto');
    }
    const workflowStepClientCtor = loaded.sigil?.v1?.WorkflowStepIngestService;
    if (workflowStepClientCtor === undefined) {
      throw new Error('failed to load sigil.v1.WorkflowStepIngestService from proto');
    }

    const credentials = this.insecure ? grpc.credentials.createInsecure() : grpc.credentials.createSsl();
    this.generationClient = new generationClientCtor(this.endpoint, credentials, {
      'grpc.primary_user_agent': this.userAgent,
    }) as GRPCGenerationServiceClient;
    this.workflowStepClient = new workflowStepClientCtor(this.endpoint, credentials, {
      'grpc.primary_user_agent': this.userAgent,
    }) as GRPCWorkflowStepServiceClient;
  }
}

// splitUserAgent returns the User-Agent to set as the gRPC channel option and
// the remaining headers with any user-agent entry removed. When the caller did
// not supply one, the SDK default (userAgent) is used.
function splitUserAgent(headers?: Record<string, string>): {
  userAgent: string;
  rest: Record<string, string>;
} {
  const rest: Record<string, string> = {};
  let resolved = userAgent();
  for (const [key, value] of Object.entries(headers ?? {})) {
    if (key.toLowerCase() === 'user-agent') {
      if (value.trim().length > 0) {
        resolved = value;
      }
      continue;
    }
    rest[key] = value;
  }
  return { userAgent: resolved, rest };
}

function parseGRPCEndpoint(endpoint: string): { host: string; insecure: boolean } {
  const trimmed = endpoint.trim();
  if (trimmed.length === 0) {
    throw new Error('generation export endpoint is required');
  }

  if (trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
    const parsed = new URL(trimmed);
    return {
      host: parsed.host,
      insecure: parsed.protocol === 'http:',
    };
  }

  if (trimmed.startsWith('grpc://')) {
    return {
      host: trimmed.slice('grpc://'.length),
      insecure: false,
    };
  }

  return {
    host: trimmed,
    insecure: false,
  };
}

function parseGRPCExportResponse(response: unknown, request: ExportGenerationsRequest): ExportGenerationsResponse {
  if (!isObject(response) || !Array.isArray(response.results)) {
    throw new Error('invalid grpc generation export response payload');
  }

  return {
    results: response.results.map((result, index) => {
      if (!isObject(result)) {
        throw new Error('invalid grpc generation export result payload');
      }

      return {
        generationId:
          asString(result.generationId) ?? asString(result.generation_id) ?? request.generations[index]?.id ?? '',
        accepted: Boolean(result.accepted),
        error: asString(result.error),
      };
    }),
  };
}

function parseGRPCWorkflowStepExportResponse(
  response: unknown,
  request: ExportWorkflowStepsRequest,
): ExportWorkflowStepsResponse {
  if (!isObject(response) || !Array.isArray(response.results)) {
    throw new Error('invalid grpc workflow step export response payload');
  }

  return {
    results: response.results.map((result, index) => {
      if (!isObject(result)) {
        throw new Error('invalid grpc workflow step export result payload');
      }

      return {
        stepId: asString(result.stepId) ?? asString(result.step_id) ?? request.workflowSteps[index]?.id ?? '',
        accepted: Boolean(result.accepted),
        error: asString(result.error),
      };
    }),
  };
}

function mapGenerationToProto(generation: Generation): Record<string, unknown> {
  const proto: Record<string, unknown> = {
    id: generation.id,
    conversationId: generation.conversationId,
    operationName: generation.operationName,
    mode: generation.mode === 'STREAM' ? 'GENERATION_MODE_STREAM' : 'GENERATION_MODE_SYNC',
    traceId: generation.traceId,
    spanId: generation.spanId,
    model: {
      provider: generation.model.provider,
      name: generation.model.name,
    },
    responseId: generation.responseId,
    responseModel: generation.responseModel,
    systemPrompt: generation.systemPrompt,
    maxTokens: generation.maxTokens === undefined ? undefined : toInt64String(generation.maxTokens),
    temperature: generation.temperature,
    topP: generation.topP,
    toolChoice: generation.toolChoice,
    thinkingEnabled: generation.thinkingEnabled,
    input: generation.input?.map(mapMessageToProto),
    output: generation.output?.map(mapMessageToProto),
    tools: generation.tools?.map(mapToolToProto),
    usage: mapUsageToProto(generation.usage),
    stopReason: generation.stopReason,
    startedAt: mapTimestamp(generation.startedAt),
    completedAt: mapTimestamp(generation.completedAt),
    tags: generation.tags ?? {},
    metadata: mapStructToProto(generation.metadata),
    rawArtifacts: generation.artifacts?.map(mapArtifactToProto),
    callError: generation.callError,
    agentName: generation.agentName,
    agentVersion: generation.agentVersion,
  };

  const effectiveVersion = canonicalEffectiveVersion(generation.effectiveVersion);
  if (effectiveVersion !== undefined) {
    proto.effectiveVersion = effectiveVersion;
  }

  return proto;
}

function mapWorkflowStepToProto(step: WorkflowStep): Record<string, unknown> {
  return {
    id: step.id,
    conversationId: step.conversationId,
    stepName: step.stepName,
    framework: step.framework ?? '',
    startedAt: step.startedAt === undefined ? undefined : mapTimestamp(step.startedAt),
    completedAt: step.completedAt === undefined ? undefined : mapTimestamp(step.completedAt),
    inputState: mapStructToProto(step.inputState),
    outputState: mapStructToProto(step.outputState),
    error: step.error ?? '',
    tags: step.tags ?? {},
    linkedGenerationIds: step.linkedGenerationIds ?? [],
    parentStepIds: step.parentStepIds ?? [],
    agentName: step.agentName ?? '',
    agentVersion: step.agentVersion ?? '',
    traceId: step.traceId ?? '',
    spanId: step.spanId ?? '',
    metadata: mapStructToProto(step.metadata),
  };
}

function mapMessageToProto(message: Message): Record<string, unknown> {
  const parts = message.parts?.map(mapMessagePartToProto) ?? [];
  if (parts.length === 0 && typeof message.content === 'string') {
    parts.push({
      text: message.content,
    });
  }

  return {
    role: toMessageRoleEnum(message.role),
    name: message.name ?? '',
    parts,
  };
}

function mapMessagePartToProto(part: MessagePart): Record<string, unknown> {
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
          toolCall: {
            id: part.toolCall.id ?? '',
            name: part.toolCall.name,
            inputJson: toBytePayload(part.toolCall.inputJSON),
          },
        },
        part.metadata?.providerType,
      );
    case 'tool_result':
      return withPartMetadata(
        {
          toolResult: {
            toolCallId: part.toolResult.toolCallId ?? '',
            name: part.toolResult.name ?? '',
            content: part.toolResult.content ?? '',
            contentJson: toBytePayload(part.toolResult.contentJSON),
            isError: part.toolResult.isError ?? false,
          },
        },
        part.metadata?.providerType,
      );
  }
}

function mapToolToProto(tool: ToolDefinition): Record<string, unknown> {
  return {
    name: tool.name,
    description: tool.description ?? '',
    type: tool.type ?? '',
    inputSchemaJson: toBytePayload(tool.inputSchemaJSON),
  };
}

function mapUsageToProto(usage: TokenUsage | undefined): Record<string, unknown> | undefined {
  if (usage === undefined) {
    return undefined;
  }

  const inputTokens = usage.inputTokens ?? 0;
  const outputTokens = usage.outputTokens ?? 0;
  const totalTokens = usage.totalTokens ?? inputTokens + outputTokens;

  return {
    inputTokens: toInt64String(inputTokens),
    outputTokens: toInt64String(outputTokens),
    totalTokens: toInt64String(totalTokens),
    cacheReadInputTokens: toInt64String(usage.cacheReadInputTokens),
    cacheWriteInputTokens: toInt64String(usage.cacheWriteInputTokens),
    reasoningTokens: toInt64String(usage.reasoningTokens),
  };
}

function mapArtifactToProto(artifact: Artifact): Record<string, unknown> {
  return {
    kind: toArtifactKindEnum(artifact.type),
    name: artifact.name ?? artifact.type,
    contentType: artifact.mimeType ?? 'application/json',
    payload: toBytePayload(artifact.payload),
    recordId: artifact.recordId ?? '',
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
      providerType,
    },
  };
}

function toBytePayload(value: string | undefined): Buffer {
  if (value === undefined || value.length === 0) {
    return Buffer.from([]);
  }
  return Buffer.from(value, 'utf8');
}

function mapStructToProto(metadata: Record<string, unknown> | undefined): Record<string, unknown> | undefined {
  if (metadata === undefined) {
    return undefined;
  }

  const normalized = normalizeMetadata(metadata);
  if (Object.keys(normalized).length === 0) {
    return undefined;
  }

  return {
    fields: mapStructFields(normalized),
  };
}

function normalizeMetadata(metadata: Record<string, unknown>): Record<string, unknown> {
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
    if (!isObject(decoded)) {
      return {};
    }
    return decoded;
  } catch {
    return {};
  }
}

function mapStructFields(objectValue: Record<string, unknown>): Record<string, unknown> {
  const fields: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(objectValue)) {
    const mappedValue = mapStructValue(value);
    if (mappedValue !== undefined) {
      fields[key] = mappedValue;
    }
  }
  return fields;
}

function mapStructValue(value: unknown): Record<string, unknown> | undefined {
  if (value === null) {
    return { nullValue: 'NULL_VALUE' };
  }

  switch (typeof value) {
    case 'string':
      return { stringValue: value };
    case 'number':
      return Number.isFinite(value) ? { numberValue: value } : { stringValue: String(value) };
    case 'boolean':
      return { boolValue: value };
    case 'object':
      if (Array.isArray(value)) {
        const values = value
          .map((entry) => mapStructValue(entry))
          .filter((entry): entry is Record<string, unknown> => entry !== undefined);
        return { listValue: { values } };
      }
      if (isObject(value)) {
        return { structValue: { fields: mapStructFields(value) } };
      }
      return { nullValue: 'NULL_VALUE' };
    default:
      return undefined;
  }
}

// Resolution is milliseconds by design: JS `Date` only holds millisecond
// precision, so `nanos` is always a multiple of 1e6. Go (`time.Time`, ns) and
// Python (`datetime`, µs) preserve finer precision when the caller supplies it,
// but a JS caller cannot express it in the first place. Shared by generation
// and workflow-step mappers.
function mapTimestamp(date: Date): Record<string, number | string> {
  const milliseconds = date.getTime();
  const seconds = Math.floor(milliseconds / 1_000);
  const nanos = (milliseconds - seconds * 1_000) * 1_000_000;
  return {
    seconds: seconds.toString(),
    nanos,
  };
}

function toInt64String(value: number | undefined): string {
  if (value === undefined || Number.isNaN(value) || !Number.isFinite(value)) {
    return '0';
  }
  return Math.trunc(value).toString();
}

function toMessageRoleEnum(role: string): string {
  const normalized = role.trim().toLowerCase();
  switch (normalized) {
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
  const normalized = kind.trim().toLowerCase();
  switch (normalized) {
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

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}
