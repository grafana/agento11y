import type {
  Artifact,
  EmbeddingResult,
  EmbeddingStart,
  Generation,
  GenerationMode,
  GenerationResult,
  GenerationStart,
  Message,
  MessagePart,
  ModelRef,
  ToolDefinition,
  ToolExecution,
  ToolExecutionResult,
  ToolExecutionStart,
} from './types.js';

const textEncoder = new TextEncoder();

// Returns "sha256:<hex>" of the trimmed input, or undefined when the trimmed
// input is empty (so the proto field stays absent on the wire).
export function canonicalEffectiveVersion(value: string | undefined): string | undefined {
  if (value === undefined) {
    return undefined;
  }
  const trimmed = value.trim();
  if (trimmed.length === 0) {
    return undefined;
  }
  return `sha256:${sha256Hex(trimmed)}`;
}

export function encodedSizeBytes(value: unknown): number {
  return textEncoder.encode(JSON.stringify(value)).byteLength;
}

export function defaultOperationNameForMode(mode: GenerationMode): string {
  return mode === 'STREAM' ? 'streamText' : 'generateText';
}

export function validateGeneration(generation: Generation): Error | undefined {
  if (generation.id.trim().length === 0) {
    return new Error('generation id is required');
  }
  if (generation.mode !== 'SYNC' && generation.mode !== 'STREAM') {
    return new Error('generation.mode must be one of SYNC|STREAM');
  }
  if (generation.model.provider.trim().length === 0) {
    return new Error('generation model provider is required');
  }
  if (generation.model.name.trim().length === 0) {
    return new Error('generation model name is required');
  }
  for (let index = 0; index < (generation.input ?? []).length; index++) {
    const error = validateMessage('generation.input', index, generation.input?.[index]);
    if (error !== undefined) {
      return error;
    }
  }
  for (let index = 0; index < (generation.output ?? []).length; index++) {
    const error = validateMessage('generation.output', index, generation.output?.[index]);
    if (error !== undefined) {
      return error;
    }
  }
  for (let index = 0; index < (generation.tools ?? []).length; index++) {
    const tool = generation.tools?.[index];
    if (tool === undefined || tool.name.trim().length === 0) {
      return new Error(`generation.tools[${index}].name is required`);
    }
  }
  for (let index = 0; index < (generation.artifacts ?? []).length; index++) {
    const error = validateArtifact(index, generation.artifacts?.[index]);
    if (error !== undefined) {
      return error;
    }
  }
  if (generation.completedAt.getTime() < generation.startedAt.getTime()) {
    return new Error('generation completedAt must not be earlier than startedAt');
  }
  return undefined;
}

export function validateToolExecution(toolExecution: ToolExecution): Error | undefined {
  if (toolExecution.toolName.trim().length === 0) {
    return new Error('tool execution name is required');
  }
  if (toolExecution.completedAt.getTime() < toolExecution.startedAt.getTime()) {
    return new Error('tool execution completedAt must not be earlier than startedAt');
  }
  return undefined;
}

export function validateEmbeddingStart(start: EmbeddingStart): Error | undefined {
  if (start.model.provider.trim().length === 0) {
    return new Error('embedding.model.provider is required');
  }
  if (start.model.name.trim().length === 0) {
    return new Error('embedding.model.name is required');
  }
  if (start.dimensions !== undefined && start.dimensions <= 0) {
    return new Error('embedding.dimensions must be > 0');
  }
  if (start.encodingFormat !== undefined && start.encodingFormat.trim().length === 0) {
    return new Error('embedding.encoding_format must not be blank');
  }
  return undefined;
}

export function validateEmbeddingResult(result: EmbeddingResult): Error | undefined {
  if (result.inputCount < 0) {
    return new Error('embedding.input_count must be >= 0');
  }
  if (result.inputTokens !== undefined && result.inputTokens < 0) {
    return new Error('embedding.input_tokens must be >= 0');
  }
  if (result.dimensions !== undefined && result.dimensions <= 0) {
    return new Error('embedding.dimensions must be > 0');
  }
  return undefined;
}

export function asError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  return new Error(String(value));
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

export function maybeUnref(timer: ReturnType<typeof setInterval>): void {
  if (typeof timer === 'object' && timer !== null && 'unref' in timer) {
    const maybeTimer = timer as { unref?: () => void };
    maybeTimer.unref?.();
  }
}

export function defaultSleep(durationMs: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, durationMs);
  });
}

type NodeHash = {
  update(input: string): NodeHash;
  digest(encoding: 'hex'): string;
};

type NodeCrypto = {
  createHash(algorithm: 'sha256'): NodeHash;
};

function sha256Hex(input: string): string {
  const nodeCrypto = resolveNodeCrypto();
  if (nodeCrypto === undefined) {
    throw new Error('sha256 hashing is unavailable in this JavaScript runtime');
  }
  return nodeCrypto.createHash('sha256').update(input).digest('hex');
}

function resolveNodeCrypto(): NodeCrypto | undefined {
  const processWithBuiltins = (globalThis as { process?: { getBuiltinModule?: (id: string) => unknown } }).process;
  const module = processWithBuiltins?.getBuiltinModule?.('crypto') as NodeCrypto | undefined;
  return typeof module?.createHash === 'function' ? module : undefined;
}

export function cloneGeneration(generation: Generation): Generation {
  return {
    ...generation,
    model: cloneModelRef(generation.model),
    input: generation.input?.map(cloneMessage),
    output: generation.output?.map(cloneMessage),
    tools: generation.tools?.map(cloneToolDefinition),
    usage: generation.usage ? { ...generation.usage } : undefined,
    startedAt: new Date(generation.startedAt),
    completedAt: new Date(generation.completedAt),
    tags: generation.tags ? { ...generation.tags } : undefined,
    metadata: generation.metadata ? { ...generation.metadata } : undefined,
    artifacts: generation.artifacts === null ? null : generation.artifacts?.map(cloneArtifact),
  };
}

export function cloneGenerationResult(result: GenerationResult): GenerationResult {
  return {
    ...result,
    input: result.input?.map(cloneMessage),
    output: result.output?.map(cloneMessage),
    tools: result.tools?.map(cloneToolDefinition),
    usage: result.usage ? { ...result.usage } : undefined,
    completedAt: result.completedAt ? new Date(result.completedAt) : undefined,
    tags: result.tags ? { ...result.tags } : undefined,
    metadata: result.metadata ? { ...result.metadata } : undefined,
    artifacts: result.artifacts?.map(cloneArtifact),
  };
}

export function cloneGenerationStart(start: GenerationStart): GenerationStart {
  return {
    ...start,
    model: cloneModelRef(start.model),
    tools: start.tools?.map(cloneToolDefinition),
    tags: start.tags ? { ...start.tags } : undefined,
    metadata: start.metadata ? { ...start.metadata } : undefined,
    startedAt: start.startedAt ? new Date(start.startedAt) : undefined,
  };
}

export function cloneEmbeddingStart(start: EmbeddingStart): EmbeddingStart {
  return {
    ...start,
    model: cloneModelRef(start.model),
    tags: start.tags ? { ...start.tags } : undefined,
    metadata: start.metadata ? { ...start.metadata } : undefined,
    startedAt: start.startedAt ? new Date(start.startedAt) : undefined,
  };
}

export function cloneEmbeddingResult(result: EmbeddingResult): EmbeddingResult {
  return {
    ...result,
    inputTexts: result.inputTexts ? [...result.inputTexts] : undefined,
  };
}

export function cloneToolExecution(toolExecution: ToolExecution): ToolExecution {
  return {
    ...toolExecution,
    startedAt: new Date(toolExecution.startedAt),
    completedAt: new Date(toolExecution.completedAt),
  };
}

export function cloneToolExecutionStart(start: ToolExecutionStart): ToolExecutionStart {
  return {
    ...start,
    startedAt: start.startedAt ? new Date(start.startedAt) : undefined,
  };
}

export function cloneToolExecutionResult(result: ToolExecutionResult): ToolExecutionResult {
  return {
    ...result,
    completedAt: result.completedAt ? new Date(result.completedAt) : undefined,
  };
}

export function cloneModelRef(model: ModelRef): ModelRef {
  return { ...model };
}

export function cloneMessage(message: Message): Message {
  return {
    ...message,
    parts: message.parts?.map(cloneMessagePart),
  };
}

export function cloneToolDefinition(tool: ToolDefinition): ToolDefinition {
  return { ...tool };
}

export function cloneArtifact(artifact: Artifact): Artifact {
  return { ...artifact };
}

export function newLocalID(prefix: string): string {
  const now = Date.now().toString(36);
  const rand = Math.random().toString(36).slice(2, 10);
  return `${prefix}-${now}-${rand}`;
}

function cloneMessagePart(part: MessagePart): MessagePart {
  switch (part.type) {
    case 'text':
      return {
        type: 'text',
        text: part.text,
        metadata: part.metadata ? { ...part.metadata } : undefined,
      };
    case 'thinking':
      return {
        type: 'thinking',
        thinking: part.thinking,
        metadata: part.metadata ? { ...part.metadata } : undefined,
      };
    case 'tool_call':
      return {
        type: 'tool_call',
        toolCall: { ...part.toolCall },
        metadata: part.metadata ? { ...part.metadata } : undefined,
      };
    case 'tool_result':
      return {
        type: 'tool_result',
        toolResult: { ...part.toolResult },
        metadata: part.metadata ? { ...part.metadata } : undefined,
      };
  }
}

function validateMessage(path: string, index: number, message: Message | undefined): Error | undefined {
  if (message === undefined) {
    return new Error(`${path}[${index}] is required`);
  }

  const role = normalizeRole(message.role);
  if (role !== 'user' && role !== 'assistant' && role !== 'tool') {
    return new Error(`${path}[${index}].role must be one of user|assistant|tool`);
  }

  const hasContent = typeof message.content === 'string' && message.content.trim().length > 0;
  const parts = message.parts ?? [];
  if (parts.length === 0 && !hasContent) {
    return new Error(`${path}[${index}].parts must not be empty`);
  }

  for (let partIndex = 0; partIndex < parts.length; partIndex++) {
    const part = parts[partIndex];
    if (part === undefined) {
      return new Error(`${path}[${index}].parts[${partIndex}] is required`);
    }
    const error = validatePart(path, index, partIndex, role, part);
    if (error !== undefined) {
      return error;
    }
  }

  return undefined;
}

function validatePart(
  path: string,
  messageIndex: number,
  partIndex: number,
  role: 'user' | 'assistant' | 'tool',
  part: MessagePart,
): Error | undefined {
  const payloadFieldCount = payloadFieldsCount(part);
  if (payloadFieldCount !== 1) {
    return new Error(`${path}[${messageIndex}].parts[${partIndex}] must set exactly one payload field`);
  }

  switch (part.type) {
    case 'text':
      if (part.text.trim().length === 0) {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].text is required`);
      }
      return undefined;
    case 'thinking':
      if (role !== 'assistant') {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].thinking only allowed for assistant role`);
      }
      if (part.thinking.trim().length === 0) {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].thinking is required`);
      }
      return undefined;
    case 'tool_call':
      if (role !== 'assistant') {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].tool_call only allowed for assistant role`);
      }
      if (part.toolCall.name.trim().length === 0) {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].tool_call.name is required`);
      }
      return undefined;
    case 'tool_result':
      if (role !== 'tool') {
        return new Error(`${path}[${messageIndex}].parts[${partIndex}].tool_result only allowed for tool role`);
      }
      return undefined;
    default:
      return new Error(`${path}[${messageIndex}].parts[${partIndex}].kind is invalid`);
  }
}

function payloadFieldsCount(part: MessagePart): number {
  let count = 0;
  if ('text' in part && typeof part.text === 'string' && part.text.trim().length > 0) {
    count++;
  }
  if ('thinking' in part && typeof part.thinking === 'string' && part.thinking.trim().length > 0) {
    count++;
  }
  if ('toolCall' in part && isRecord(part.toolCall)) {
    count++;
  }
  if ('toolResult' in part && isRecord(part.toolResult)) {
    count++;
  }
  return count;
}

function validateArtifact(index: number, artifact: Artifact | undefined): Error | undefined {
  if (artifact === undefined) {
    return new Error(`generation.artifacts[${index}] is required`);
  }

  const kind = artifact.type.trim().toLowerCase();
  switch (kind) {
    case 'request':
    case 'response':
    case 'tools':
    case 'provider_event':
      break;
    default:
      return new Error(`generation.artifacts[${index}].kind is invalid`);
  }

  const payload = artifact.payload?.trim() ?? '';
  const recordID = artifact.recordId?.trim() ?? '';
  if (payload.length === 0 && recordID.length === 0) {
    return new Error(`generation.artifacts[${index}] must provide payload or record_id`);
  }

  return undefined;
}

function normalizeRole(role: string): 'user' | 'assistant' | 'tool' | '' {
  const normalized = role.trim().toLowerCase();
  if (normalized === 'user' || normalized === 'assistant' || normalized === 'tool') {
    return normalized;
  }
  return '';
}
