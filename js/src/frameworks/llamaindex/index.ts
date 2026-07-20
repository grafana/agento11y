import { CallbackManager } from 'llamaindex';

import type { Agento11yClient } from '../../client.js';
import { Agento11yFrameworkHandler, type FrameworkHandlerOptions } from '../shared.js';

export type { FrameworkHandlerOptions };

type AnyRecord = Record<string, unknown>;

type CallbackConfig = Record<string, unknown> & { callbackManager?: unknown };

type LlamaIndexEvent = {
  detail?: unknown;
  reason?: unknown;
};

type LlamaIndexCallbackHandler = (event: LlamaIndexEvent) => void;

type LlamaIndexCallbackManager = {
  on(event: string, handler: LlamaIndexCallbackHandler): unknown;
  off(event: string, handler: LlamaIndexCallbackHandler): unknown;
};

export interface LlamaIndexCallbackRegistration {
  handler: Agento11yLlamaIndexHandler;
  detach: () => void;
}

const registrations = new WeakMap<LlamaIndexCallbackManager, LlamaIndexCallbackRegistration>();

export class Agento11yLlamaIndexHandler extends Agento11yFrameworkHandler {
  name = 'agento11y_llamaindex_handler';

  constructor(client: Agento11yClient, options: FrameworkHandlerOptions = {}) {
    super(client, 'llamaindex', 'javascript', options);
  }

  async handleLLMStart(
    serialized: unknown,
    prompts: unknown,
    runId: string,
    parentRunId?: string,
    extraParams?: Record<string, unknown>,
    tags?: string[],
    metadata?: Record<string, unknown>,
    runName?: string,
  ): Promise<void> {
    this.onLLMStart(serialized, prompts, runId, parentRunId, extraParams, tags, metadata, runName);
  }

  async handleChatModelStart(
    serialized: unknown,
    messages: unknown,
    runId: string,
    parentRunId?: string,
    extraParams?: Record<string, unknown>,
    tags?: string[],
    metadata?: Record<string, unknown>,
    runName?: string,
  ): Promise<void> {
    this.onChatModelStart(serialized, messages, runId, parentRunId, extraParams, tags, metadata, runName);
  }

  async handleLLMNewToken(token: string, _idx: unknown, runId: string): Promise<void> {
    this.onLLMNewToken(token, runId);
  }

  async handleLLMEnd(output: unknown, runId: string): Promise<void> {
    this.onLLMEnd(output, runId);
  }

  async handleLLMError(error: unknown, runId: string): Promise<void> {
    this.onLLMError(error, runId);
  }

  async handleToolStart(
    serialized: unknown,
    input: unknown,
    runId: string,
    parentRunId?: string,
    tags?: string[],
    metadata?: Record<string, unknown>,
    runName?: string,
  ): Promise<void> {
    this.onToolStart(serialized, input, runId, parentRunId, tags, metadata, runName);
  }

  async handleToolEnd(output: unknown, runId: string): Promise<void> {
    this.onToolEnd(output, runId);
  }

  async handleToolError(error: unknown, runId: string): Promise<void> {
    this.onToolError(error, runId);
  }

  async handleChainStart(
    serialized: unknown,
    _inputs: unknown,
    runId: string,
    parentRunId?: string,
    tags?: string[],
    metadata?: Record<string, unknown>,
    runType?: string,
    runName?: string,
  ): Promise<void> {
    this.onChainStart(serialized, runId, parentRunId, tags, metadata, runType, runName);
  }

  async handleChainEnd(_outputs: unknown, runId: string): Promise<void> {
    this.onChainEnd(runId);
  }

  async handleChainError(error: unknown, runId: string): Promise<void> {
    this.onChainError(error, runId);
  }

  async handleRetrieverStart(
    serialized: unknown,
    _query: string,
    runId: string,
    parentRunId?: string,
    tags?: string[],
    metadata?: Record<string, unknown>,
    runName?: string,
  ): Promise<void> {
    this.onRetrieverStart(serialized, runId, parentRunId, tags, metadata, runName);
  }

  async handleRetrieverEnd(_documents: unknown, runId: string): Promise<void> {
    this.onRetrieverEnd(runId);
  }

  async handleRetrieverError(error: unknown, runId: string): Promise<void> {
    this.onRetrieverError(error, runId);
  }
}

export function createAgento11yLlamaIndexHandler(
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): Agento11yLlamaIndexHandler {
  return new Agento11yLlamaIndexHandler(client, options);
}

export function attachAgento11yLlamaIndexCallbacks(
  callbackManager: LlamaIndexCallbackManager,
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): LlamaIndexCallbackRegistration {
  const existing = registrations.get(callbackManager);
  if (existing !== undefined) {
    return existing;
  }

  const handler = createAgento11yLlamaIndexHandler(client, options);
  const llmRunIds = new Map<string, string>();
  const llmFallbackRunIds: string[] = [];
  const toolRunIds = new Map<string, string>();
  const toolFallbackRunIds: string[] = [];
  let sequence = 0;

  const nextRunId = (prefix: string): string => `${prefix}:${++sequence}`;

  const onLlmStart: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      const runId = eventId.length > 0 ? `llama_llm:${eventId}` : nextRunId('llama_llm');
      if (eventId.length > 0) {
        llmRunIds.set(eventId, runId);
      } else {
        llmFallbackRunIds.push(runId);
      }

      const messages = normalizeLlamaMessages(read(detail, 'messages'));
      const stream = resolveStreamFlag(detail);

      await handler.handleChatModelStart(
        { name: 'llamaindex.llm' },
        [messages],
        runId,
        undefined,
        {
          invocation_params: {
            stream,
          },
        },
        undefined,
        buildMetadata(event, detail),
        'llamaindex.llm',
      );
    });
  };

  const onLlmStream: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      const runId =
        eventId.length > 0
          ? (llmRunIds.get(eventId) ?? `llama_llm:${eventId}`)
          : llmFallbackRunIds[llmFallbackRunIds.length - 1];
      if (runId === undefined) {
        return;
      }
      const chunkText = normalizeMessageText(read(read(detail, 'chunk'), 'delta'));
      if (chunkText.length === 0) {
        return;
      }
      await handler.handleLLMNewToken(chunkText, undefined, runId);
    });
  };

  const onLlmEnd: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      const runId = eventId.length > 0 ? (llmRunIds.get(eventId) ?? `llama_llm:${eventId}`) : llmFallbackRunIds.pop();
      if (runId === undefined) {
        return;
      }
      if (eventId.length > 0) {
        llmRunIds.delete(eventId);
      }
      const explicitError = read(detail, 'error');
      if (explicitError !== undefined) {
        await handler.handleLLMError(explicitError, runId);
        return;
      }

      const response = asRecord(read(detail, 'response'));
      const text = normalizeMessageText(read(read(response, 'message'), 'content'));
      const usage = resolveUsage(response);

      await handler.handleLLMEnd(
        {
          generations: text.length > 0 ? [[{ text }]] : [],
          llm_output: {
            model_name: resolveModelName(response),
            finish_reason: resolveStopReason(response),
            token_usage: usage,
          },
        },
        runId,
      );
    });
  };

  const onLlmError: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      const runId = eventId.length > 0 ? (llmRunIds.get(eventId) ?? `llama_llm:${eventId}`) : llmFallbackRunIds.pop();
      if (runId === undefined) {
        return;
      }
      if (eventId.length > 0) {
        llmRunIds.delete(eventId);
      }
      await handler.handleLLMError(read(detail, 'error') ?? new Error('llamaindex llm error'), runId);
    });
  };

  const onToolCall: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const toolCall = asRecord(read(detail, 'toolCall'));
      const callId = asString(read(toolCall, 'id'));
      const runId = callId.length > 0 ? `llama_tool:${callId}` : nextRunId('llama_tool');
      if (callId.length > 0) {
        toolRunIds.set(callId, runId);
      } else {
        toolFallbackRunIds.push(runId);
      }

      const toolName = asString(read(toolCall, 'name')) || 'framework_tool';
      await handler.handleToolStart(
        {
          name: toolName,
        },
        read(toolCall, 'input'),
        runId,
        undefined,
        undefined,
        buildMetadata(event, detail, { event_id: callId }),
        toolName,
      );
    });
  };

  const onToolResult: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const toolCall = asRecord(read(detail, 'toolCall'));
      const callId = asString(read(toolCall, 'id'));
      const runId = callId.length > 0 ? toolRunIds.get(callId) : toolFallbackRunIds.pop();
      if (runId === undefined) {
        return;
      }
      if (callId.length > 0) {
        toolRunIds.delete(callId);
      }
      await handler.handleToolEnd(read(detail, 'toolResult'), runId);
    });
  };

  const onQueryStart: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() =>
      runChainStart(handler, event, {
        runPrefix: 'llama_query',
        runType: 'query',
        name: 'llamaindex.query',
        queryField: 'query',
      }),
    );
  };

  const onQueryEnd: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() => runChainEnd(handler, event, 'llama_query'));
  };

  const onSynthesizeStart: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() =>
      runChainStart(handler, event, {
        runPrefix: 'llama_synthesize',
        runType: 'synthesize',
        name: 'llamaindex.synthesize',
        queryField: 'query',
      }),
    );
  };

  const onSynthesizeEnd: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() => runChainEnd(handler, event, 'llama_synthesize'));
  };

  const onRetrieveStart: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      if (eventId.length === 0) {
        return;
      }

      const runId = `llama_retrieve:${eventId}`;
      await handler.handleRetrieverStart(
        { name: 'llamaindex.retrieve' },
        normalizeMessageText(read(read(detail, 'query'), 'query')),
        runId,
        undefined,
        undefined,
        buildMetadata(event, detail),
        'llamaindex.retrieve',
      );
    });
  };

  const onRetrieveEnd: LlamaIndexCallbackHandler = (event) => {
    safelyRun(async () => {
      const detail = asRecord(event.detail);
      const eventId = resolveEventId(detail);
      if (eventId.length === 0) {
        return;
      }
      await handler.handleRetrieverEnd(undefined, `llama_retrieve:${eventId}`);
    });
  };

  const onAgentStart: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() =>
      runChainStart(handler, event, {
        runPrefix: 'llama_agent',
        runType: 'agent',
        name: 'llamaindex.agent',
        idPath: ['startStep', 'id'],
      }),
    );
  };

  const onAgentEnd: LlamaIndexCallbackHandler = (event) => {
    safelyRun(() => runChainEnd(handler, event, 'llama_agent', ['endStep', 'id']));
  };

  const listeners: Array<[string, LlamaIndexCallbackHandler]> = [
    ['llm-start', onLlmStart],
    ['llm-stream', onLlmStream],
    ['llm-end', onLlmEnd],
    ['llm-error', onLlmError],
    ['llm-tool-call', onToolCall],
    ['llm-tool-result', onToolResult],
    ['query-start', onQueryStart],
    ['query-end', onQueryEnd],
    ['synthesize-start', onSynthesizeStart],
    ['synthesize-end', onSynthesizeEnd],
    ['retrieve-start', onRetrieveStart],
    ['retrieve-end', onRetrieveEnd],
    ['agent-start', onAgentStart],
    ['agent-end', onAgentEnd],
  ];

  for (const [eventName, listener] of listeners) {
    callbackManager.on(eventName, listener);
  }

  const registration: LlamaIndexCallbackRegistration = {
    handler,
    detach: () => {
      for (const [eventName, listener] of listeners) {
        callbackManager.off(eventName, listener);
      }
      llmRunIds.clear();
      llmFallbackRunIds.length = 0;
      toolRunIds.clear();
      toolFallbackRunIds.length = 0;
      registrations.delete(callbackManager);
    },
  };

  registrations.set(callbackManager, registration);
  return registration;
}

export function withAgento11yLlamaIndexCallbacks<T extends CallbackConfig>(
  config: T | undefined,
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): T & { callbackManager: LlamaIndexCallbackManager } {
  const base = { ...(config ?? {}) } as CallbackConfig;
  const existingManager = base.callbackManager;

  let callbackManager: LlamaIndexCallbackManager;
  if (existingManager === undefined) {
    callbackManager = new CallbackManager();
  } else if (isCallbackManager(existingManager)) {
    callbackManager = existingManager;
  } else {
    throw new Error('withAgento11yLlamaIndexCallbacks expects config.callbackManager to implement on/off methods.');
  }

  attachAgento11yLlamaIndexCallbacks(callbackManager, client, options);

  return {
    ...base,
    callbackManager,
  } as T & { callbackManager: LlamaIndexCallbackManager };
}

type ChainStartConfig = {
  runPrefix: string;
  runType: string;
  name: string;
  queryField?: string;
  idPath?: string[];
};

async function runChainStart(
  handler: Agento11yLlamaIndexHandler,
  event: LlamaIndexEvent,
  config: ChainStartConfig,
): Promise<void> {
  const detail = asRecord(event.detail);
  const eventId = resolveEventId(detail, config.idPath);
  if (eventId.length === 0) {
    return;
  }

  await handler.handleChainStart(
    { name: config.name },
    undefined,
    `${config.runPrefix}:${eventId}`,
    undefined,
    undefined,
    buildMetadata(event, detail),
    config.runType,
    config.name,
  );
}

async function runChainEnd(
  handler: Agento11yLlamaIndexHandler,
  event: LlamaIndexEvent,
  runPrefix: string,
  idPath?: string[],
): Promise<void> {
  const detail = asRecord(event.detail);
  const eventId = resolveEventId(detail, idPath);
  if (eventId.length === 0) {
    return;
  }

  await handler.handleChainEnd(undefined, `${runPrefix}:${eventId}`);
}

function buildMetadata(event: LlamaIndexEvent, detail: AnyRecord | undefined, extras?: AnyRecord): AnyRecord {
  const eventId = resolveEventId(detail);
  const conversationId = resolveConversationId(detail);

  return {
    event_id: eventId,
    conversation_id: conversationId,
    session_id: conversationId,
    thread_id: asString(read(detail, 'thread_id')) || asString(read(detail, 'threadId')),
    reason_id: asString(read(event.reason, 'id')),
    ...extras,
  };
}

function resolveConversationId(detail: AnyRecord | undefined): string {
  const candidates = [
    asString(read(detail, 'conversation_id')),
    asString(read(detail, 'conversationId')),
    asString(read(detail, 'session_id')),
    asString(read(detail, 'sessionId')),
    asString(read(detail, 'group_id')),
    asString(read(detail, 'groupId')),
    asString(read(read(detail, 'query'), 'conversation_id')),
    asString(read(read(detail, 'query'), 'conversationId')),
    asString(read(read(detail, 'query'), 'session_id')),
    asString(read(read(detail, 'query'), 'sessionId')),
  ];

  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }

  return '';
}

function resolveEventId(detail: AnyRecord | undefined, idPath?: string[]): string {
  if (Array.isArray(idPath) && idPath.length > 0) {
    let current: unknown = detail;
    for (const key of idPath) {
      current = read(current, key);
    }
    const explicit = asString(current);
    if (explicit.length > 0) {
      return explicit;
    }
  }

  const candidates = [
    asString(read(detail, 'id')),
    asString(read(read(detail, 'startStep'), 'id')),
    asString(read(read(detail, 'endStep'), 'id')),
    asString(read(read(detail, 'toolCall'), 'id')),
  ];

  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }

  return '';
}

function resolveModelName(response: AnyRecord | undefined): string {
  const raw = read(response, 'raw');
  const candidates = [
    asString(read(response, 'model')),
    asString(read(response, 'model_name')),
    asString(read(raw, 'model')),
    asString(read(raw, 'model_name')),
  ];

  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }

  return '';
}

function resolveStopReason(response: AnyRecord | undefined): string | undefined {
  const raw = read(response, 'raw');
  const finishReason =
    asString(read(response, 'finish_reason')) ||
    asString(read(response, 'finishReason')) ||
    asString(read(raw, 'finish_reason')) ||
    asString(read(raw, 'finishReason'));
  return finishReason.length > 0 ? finishReason : undefined;
}

function resolveUsage(response: AnyRecord | undefined): AnyRecord | undefined {
  const raw = read(response, 'raw');
  const usage = asRecord(read(raw, 'usage')) ?? asRecord(read(response, 'usage'));
  if (!usage) {
    return undefined;
  }

  const promptTokens = asNumber(read(usage, 'prompt_tokens')) ?? asNumber(read(usage, 'promptTokens'));
  const completionTokens = asNumber(read(usage, 'completion_tokens')) ?? asNumber(read(usage, 'completionTokens'));
  const totalTokens = asNumber(read(usage, 'total_tokens')) ?? asNumber(read(usage, 'totalTokens'));

  if (promptTokens === undefined && completionTokens === undefined && totalTokens === undefined) {
    return undefined;
  }

  return {
    prompt_tokens: promptTokens,
    completion_tokens: completionTokens,
    total_tokens: totalTokens,
  };
}

function resolveStreamFlag(detail: AnyRecord | undefined): boolean {
  const additional = asRecord(read(detail, 'additional_kwargs'));
  const stream = read(detail, 'stream') ?? read(detail, 'streaming');
  const additionalStream = read(additional, 'stream') ?? read(additional, 'streaming');
  return asBoolean(stream) || asBoolean(additionalStream);
}

function normalizeLlamaMessages(messages: unknown): Array<{ role: string; content: string }> {
  if (!Array.isArray(messages)) {
    return [];
  }

  const output: Array<{ role: string; content: string }> = [];
  for (const message of messages) {
    const role = asString(read(message, 'role')) || 'user';
    const content = normalizeMessageText(read(message, 'content'));
    if (content.length === 0) {
      continue;
    }
    output.push({ role, content });
  }

  return output;
}

function normalizeMessageText(value: unknown): string {
  if (typeof value === 'string') {
    return value.trim();
  }

  if (Array.isArray(value)) {
    const parts = value
      .map((entry) => normalizeMessageText(read(entry, 'text') ?? read(entry, 'data') ?? entry))
      .filter((entry) => entry.length > 0);
    return parts.join(' ').trim();
  }

  if (isRecord(value)) {
    const text = asString(read(value, 'text'));
    if (text.length > 0) {
      return text;
    }
    const nested = asString(read(value, 'content'));
    if (nested.length > 0) {
      return nested;
    }
  }

  return '';
}

function isCallbackManager(value: unknown): value is LlamaIndexCallbackManager {
  if (!isRecord(value)) {
    return false;
  }
  return typeof value.on === 'function' && typeof value.off === 'function';
}

function safelyRun(task: () => Promise<void>): void {
  void task().catch((error) => {
    queueMicrotask(() => {
      throw toError(error);
    });
  });
}

function read(value: unknown, key: string): unknown {
  if (!isRecord(value)) {
    return undefined;
  }
  return value[key];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function asRecord(value: unknown): AnyRecord | undefined {
  if (!isRecord(value)) {
    return undefined;
  }
  return value;
}

function asString(value: unknown): string {
  if (typeof value !== 'string') {
    return '';
  }
  return value.trim();
}

function asBoolean(value: unknown): boolean {
  return value === true;
}

function asNumber(value: unknown): number | undefined {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return undefined;
  }
  return value;
}

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  return new Error(typeof value === 'string' ? value : 'framework callback failed');
}
