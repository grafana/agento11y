import type { Agento11yClient } from '../../client.js';
import { Agento11yFrameworkHandler, type FrameworkHandlerOptions } from '../shared.js';

export type { FrameworkHandlerOptions };

type CallbackConfig = Record<string, unknown> & { callbacks?: unknown };

export class Agento11yLangGraphHandler extends Agento11yFrameworkHandler {
  name = 'agento11y_langgraph_handler';

  constructor(client: Agento11yClient, options: FrameworkHandlerOptions = {}) {
    super(client, 'langgraph', 'javascript', options);
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

export function createAgento11yLangGraphHandler(
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): Agento11yLangGraphHandler {
  return new Agento11yLangGraphHandler(client, options);
}

export function withAgento11yLangGraphCallbacks<T extends CallbackConfig>(
  config: T | undefined,
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): T & { callbacks: unknown[] } {
  const handler = createAgento11yLangGraphHandler(client, options);
  const base = { ...(config ?? {}) } as CallbackConfig;
  const existingValue = base.callbacks;
  const callbacks = Array.isArray(existingValue)
    ? [...existingValue]
    : existingValue === undefined
      ? []
      : [existingValue];
  if (!callbacks.some((callback) => callback instanceof Agento11yLangGraphHandler)) {
    callbacks.push(handler);
  }
  return {
    ...base,
    callbacks,
  } as T & { callbacks: unknown[] };
}
