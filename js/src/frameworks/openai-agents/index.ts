import type { Agento11yClient } from '../../client.js';
import type { Message, MessagePart, TokenUsage, ToolDefinition } from '../../types.js';
import {
  Agento11yFrameworkHandler,
  type FrameworkHandlerOptions,
  type MappedGenerationEnd,
  type MappedGenerationStart,
  splitProviderPrefix,
} from '../shared.js';

export type { FrameworkHandlerOptions };

type AnyRecord = Record<string, unknown>;

type OpenAIAgentsHookTarget = {
  on(event: string, listener: (...args: unknown[]) => void): unknown;
  off(event: string, listener: (...args: unknown[]) => void): unknown;
};

export interface OpenAIAgentsHookRegistration {
  handler: Agento11yOpenAIAgentsHandler;
  detach: () => void;
}

export class Agento11yOpenAIAgentsHandler extends Agento11yFrameworkHandler {
  name = 'agento11y_openai_agents_handler';

  constructor(client: Agento11yClient, options: FrameworkHandlerOptions = {}) {
    super(client, 'openai-agents', 'javascript', options);
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

  /** Starts a generation for one model call (`Model.getResponse` / `getStreamedResponse`). */
  handleModelCallStart(runId: string, start: MappedGenerationStart): void {
    this.onMappedGenerationStart(runId, start);
  }

  /** Records a streamed text delta; the first one sets time to first token. */
  handleModelCallDelta(runId: string, delta: string): void {
    this.onLLMNewToken(delta, runId);
  }

  handleModelCallEnd(runId: string, end: MappedGenerationEnd): void {
    this.onMappedGenerationEnd(runId, end);
  }

  handleModelCallError(error: unknown, runId: string): void {
    this.onLLMError(error, runId);
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

export function createAgento11yOpenAIAgentsHandler(
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): Agento11yOpenAIAgentsHandler {
  return new Agento11yOpenAIAgentsHandler(client, options);
}

export function withAgento11yOpenAIAgentsHooks(
  target: OpenAIAgentsHookTarget,
  client: Agento11yClient,
  options: FrameworkHandlerOptions = {},
): OpenAIAgentsHookRegistration {
  if (!isHookTarget(target)) {
    throw new Error(
      'withAgento11yOpenAIAgentsHooks expects an OpenAI Agents Runner or Agent (RunHooks/AgentHooks emitter).',
    );
  }

  const handler = createAgento11yOpenAIAgentsHandler(client, options);
  const contextStacks = new WeakMap<object, string[]>();
  const contextIds = new WeakMap<object, number>();
  const toolRunIds = new Map<string, string>();
  const fallbackToolRunIds = new Map<number, string[]>();

  let sequence = 0;

  const nextRunId = (prefix: string): string => `${prefix}:${++sequence}`;

  const getContextId = (context: unknown): number | undefined => {
    if (!isRecord(context)) {
      return undefined;
    }
    const existing = contextIds.get(context);
    if (existing !== undefined) {
      return existing;
    }
    const assigned = ++sequence;
    contextIds.set(context, assigned);
    return assigned;
  };

  const getStack = (context: unknown): string[] => {
    if (!isRecord(context)) {
      return [];
    }
    const existing = contextStacks.get(context);
    if (existing !== undefined) {
      return existing;
    }
    const created: string[] = [];
    contextStacks.set(context, created);
    return created;
  };

  const makeContextMetadata = (context: unknown, extras?: Record<string, unknown>): Record<string, unknown> => {
    const conversationId = resolveConversationId(context);
    return {
      conversation_id: conversationId,
      group_id: conversationId,
      ...extras,
    };
  };

  // Model calls carry no run context, so each one is attributed to the run that
  // emitted the latest hook event. Runs on one registration are expected to be
  // sequential; a conversation id set with the SDK's context helpers still
  // applies per call.
  let lastContext: unknown;
  let lastAgentName = '';
  const noteContext = (context: unknown, agent?: unknown): void => {
    lastContext = context;
    if (agent !== undefined) {
      lastAgentName = resolveAgentName(agent);
    }
  };

  // Runs whose model calls are not intercepted keep one generation per agent,
  // recorded from agent_start/agent_end.
  const legacyRuns = new Set<string>();
  const restorers: Array<() => void> = [];
  const wrappedModels = new WeakMap<object, unknown>();
  const modelProxies = new WeakSet<object>();

  const recordStart = (runId: string, request: unknown, modelName: string, stream: boolean): void => {
    const context = lastContext;
    const stack = getStack(context);
    try {
      handler.handleModelCallStart(runId, {
        ...mapModelRequest(request, modelName),
        stream,
        parentRunId: stack.length > 0 ? stack[stack.length - 1] : undefined,
        runName: lastAgentName || undefined,
        callbackMetadata: makeContextMetadata(context),
      });
    } catch {
      // Recording must never break the model call.
    }
  };
  const recordEnd = (runId: string, response: unknown): void => {
    try {
      handler.handleModelCallEnd(runId, mapModelResponse(response));
    } catch {
      // Recording must never break the model call.
    }
  };
  const recordError = (runId: string, error: unknown): void => {
    try {
      handler.handleModelCallError(error, runId);
    } catch {
      // Recording must never break the model call.
    }
  };

  const wrapModel = (model: unknown, requestedName: string): unknown => {
    if (!isModel(model) || modelProxies.has(model)) {
      return model;
    }
    const existing = wrappedModels.get(model);
    if (existing !== undefined) {
      return existing;
    }
    const modelName = requestedName.trim() || resolveModelObjectName(model);
    const getResponse = async (request: unknown): Promise<unknown> => {
      const runId = nextRunId('openai_generation');
      recordStart(runId, request, modelName, false);
      let response: unknown;
      try {
        response = await (model.getResponse as (request: unknown) => Promise<unknown>).call(model, request);
      } catch (error) {
        recordError(runId, error);
        throw error;
      }
      recordEnd(runId, response);
      return response;
    };
    const getStreamedResponse = async function* (request: unknown): AsyncGenerator<unknown> {
      const runId = nextRunId('openai_generation');
      recordStart(runId, request, modelName, true);
      let finalResponse: unknown;
      let settled = false;
      try {
        const events = (model.getStreamedResponse as (request: unknown) => AsyncIterable<unknown>).call(model, request);
        for await (const event of events) {
          const eventType = asString(read(event, 'type'));
          if (eventType === 'output_text_delta') {
            const delta = read(event, 'delta');
            if (typeof delta === 'string' && delta.length > 0) {
              try {
                handler.handleModelCallDelta(runId, delta);
              } catch {
                // Recording must never break the model call.
              }
            }
          } else if (eventType === 'response_done') {
            finalResponse = read(event, 'response');
          }
          yield event;
        }
        settled = true;
        recordEnd(runId, finalResponse);
      } catch (error) {
        settled = true;
        recordError(runId, error);
        throw error;
      } finally {
        if (!settled) {
          // The consumer stopped reading before the stream finished.
          recordError(runId, new Error('model stream closed before completion'));
        }
      }
    };
    const proxy = new Proxy(model, {
      get(target, property) {
        if (property === 'getResponse') {
          return getResponse;
        }
        if (property === 'getStreamedResponse') {
          return getStreamedResponse;
        }
        const value = Reflect.get(target, property, target);
        // Bind to the real model so class private fields keep working.
        return typeof value === 'function' ? value.bind(target) : value;
      },
    });
    wrappedModels.set(model, proxy);
    modelProxies.add(proxy);
    return proxy;
  };

  // A Runner resolves string model names through its model provider.
  let runnerResolvesModels = false;
  const runnerConfig = read(target, 'config');
  if (isRecord(runnerConfig)) {
    const provider = read(runnerConfig, 'modelProvider');
    if (isRecord(provider) && typeof provider.getModel === 'function') {
      const getModel = provider.getModel as (name?: string) => unknown;
      const wrappedProvider = new Proxy(provider, {
        get(providerTarget, property) {
          if (property === 'getModel') {
            return async (name?: string) => wrapModel(await getModel.call(providerTarget, name), name ?? '');
          }
          const value = Reflect.get(providerTarget, property, providerTarget);
          return typeof value === 'function' ? value.bind(providerTarget) : value;
        },
      });
      runnerConfig.modelProvider = wrappedProvider;
      restorers.push(() => {
        if (runnerConfig.modelProvider === wrappedProvider) {
          runnerConfig.modelProvider = provider;
        }
      });
      runnerResolvesModels = true;
    }
    const configModel = read(runnerConfig, 'model');
    if (isModel(configModel)) {
      const wrapped = wrapModel(configModel, '');
      runnerConfig.model = wrapped;
      restorers.push(() => {
        if (runnerConfig.model === wrapped) {
          runnerConfig.model = configModel;
        }
      });
    }
  }

  // Runs synchronously inside the agent_start emit, before the runner resolves
  // the agent's model for its first call.
  const interceptAgentModel = (agent: unknown): boolean => {
    if (!isRecord(agent)) {
      return false;
    }
    const model = agent.model;
    if (isModel(model)) {
      if (!modelProxies.has(model)) {
        const wrapped = wrapModel(model, '');
        agent.model = wrapped;
        restorers.push(() => {
          if (agent.model === wrapped) {
            agent.model = model;
          }
        });
      }
      return true;
    }
    return runnerResolvesModels;
  };

  const onAgentStart = async (
    context: unknown,
    agent: unknown,
    turnInput: unknown,
    intercepted: boolean,
  ): Promise<void> => {
    const stack = getStack(context);
    const runId = nextRunId('openai_agent');
    const parentRunId = stack.length > 0 ? stack[stack.length - 1] : undefined;
    stack.push(runId);
    if (intercepted) {
      return;
    }
    legacyRuns.add(runId);

    const modelName = resolveModelName(agent);
    const agentName = resolveAgentName(agent);

    await handler.handleChatModelStart(
      {
        name: agentName,
        kwargs: modelName.length > 0 ? { model: modelName } : undefined,
      },
      [mapTurnInputToMessages(turnInput)],
      runId,
      parentRunId,
      {
        invocation_params: {
          model: modelName,
        },
      },
      undefined,
      makeContextMetadata(context),
      agentName,
    );
  };

  const onAgentEnd = async (context: unknown, agent: unknown, output: unknown): Promise<void> => {
    const stack = getStack(context);
    const runId = stack.pop();
    if (runId === undefined) {
      return;
    }
    const contextId = getContextId(context);
    if (contextId !== undefined) {
      fallbackToolRunIds.delete(contextId);
    }
    if (!legacyRuns.delete(runId)) {
      return;
    }

    const modelName = resolveModelName(agent);
    const outputText = normalizeOutputText(output);
    const usage = readUsage(output) ?? readUsage(context);

    await handler.handleLLMEnd(
      {
        generations: outputText.length > 0 ? [[{ text: outputText }]] : [],
        llm_output: {
          model_name: modelName.length > 0 ? modelName : undefined,
          token_usage: usage,
        },
      },
      runId,
    );
  };

  const onAgentError = async (context: unknown, error: unknown): Promise<void> => {
    const stack = getStack(context);
    const runId = stack.pop();
    if (runId === undefined) {
      return;
    }

    const contextId = getContextId(context);
    if (contextId !== undefined) {
      for (const key of toolRunIds.keys()) {
        if (key.startsWith(`${contextId}:`)) {
          toolRunIds.delete(key);
        }
      }
      fallbackToolRunIds.delete(contextId);
    }

    if (!legacyRuns.delete(runId)) {
      return;
    }
    await handler.handleLLMError(error, runId);
  };

  const onAgentHandoff = async (context: unknown, fromAgent: unknown, toAgent: unknown): Promise<void> => {
    const stack = getStack(context);
    const parentRunId = stack.length > 0 ? stack[stack.length - 1] : undefined;
    const handoffRunId = nextRunId('openai_handoff');

    await handler.handleChainStart(
      {
        name: 'agent_handoff',
        source: resolveAgentName(fromAgent),
        destination: resolveAgentName(toAgent),
      },
      undefined,
      handoffRunId,
      parentRunId,
      undefined,
      makeContextMetadata(context),
      'handoff',
      'agent_handoff',
    );
    await handler.handleChainEnd(undefined, handoffRunId);
  };

  const onToolStart = async (context: unknown, _agent: unknown, tool: unknown, details: unknown): Promise<void> => {
    const callId = resolveToolCallId(details);
    const contextId = getContextId(context);
    const runId = callId.length > 0 ? callId : nextRunId('openai_tool');
    if (contextId !== undefined) {
      if (callId.length > 0) {
        toolRunIds.set(`${contextId}:${callId}`, runId);
      } else {
        const stack = fallbackToolRunIds.get(contextId) ?? [];
        stack.push(runId);
        fallbackToolRunIds.set(contextId, stack);
      }
    }

    const stack = getStack(context);
    const parentRunId = stack.length > 0 ? stack[stack.length - 1] : undefined;
    const toolName = resolveToolName(tool, details);

    await handler.handleToolStart(
      {
        name: toolName,
      },
      resolveToolArguments(details),
      runId,
      parentRunId,
      undefined,
      makeContextMetadata(context, {
        event_id: callId,
      }),
      toolName,
    );
  };

  const onToolEnd = async (context: unknown, result: unknown, details: unknown): Promise<void> => {
    const callId = resolveToolCallId(details);
    const contextId = getContextId(context);

    let runId: string | undefined;
    if (contextId !== undefined) {
      if (callId.length > 0) {
        runId = toolRunIds.get(`${contextId}:${callId}`);
        toolRunIds.delete(`${contextId}:${callId}`);
      } else {
        const stack = fallbackToolRunIds.get(contextId);
        runId = stack?.pop();
        if (stack !== undefined && stack.length === 0) {
          fallbackToolRunIds.delete(contextId);
        }
      }
    }

    if (runId === undefined && callId.length > 0) {
      runId = callId;
    }
    if (runId === undefined) {
      return;
    }

    await handler.handleToolEnd(result, runId);
  };

  const onToolError = async (context: unknown, error: unknown, details: unknown): Promise<void> => {
    const callId = resolveToolCallId(details);
    const contextId = getContextId(context);

    let runId: string | undefined;
    if (contextId !== undefined) {
      if (callId.length > 0) {
        runId = toolRunIds.get(`${contextId}:${callId}`);
        toolRunIds.delete(`${contextId}:${callId}`);
      } else {
        const stack = fallbackToolRunIds.get(contextId);
        runId = stack?.pop();
        if (stack !== undefined && stack.length === 0) {
          fallbackToolRunIds.delete(contextId);
        }
      }
    }

    if (runId === undefined && callId.length > 0) {
      runId = callId;
    }
    if (runId === undefined) {
      return;
    }

    await handler.handleToolError(error, runId);
  };

  const listeners: Array<[string, (...args: unknown[]) => void]> = [
    [
      'agent_start',
      (...args: unknown[]) => {
        if (args.length < 2) {
          return;
        }
        noteContext(args[0], args[1]);
        const intercepted = interceptAgentModel(args[1]);
        safelyRun(onAgentStart(args[0], args[1], args[2], intercepted));
      },
    ],
    [
      'agent_end',
      (...args: unknown[]) => {
        if (args.length < 2) {
          return;
        }

        if (args.length >= 3) {
          safelyRun(onAgentEnd(args[0], args[1], args[2]));
          return;
        }

        safelyRun(onAgentEnd(args[0], undefined, args[1]));
      },
    ],
    [
      'agent_error',
      (...args: unknown[]) => {
        if (args.length < 2) {
          return;
        }

        if (args.length >= 3) {
          safelyRun(onAgentError(args[0], args[2]));
          return;
        }

        safelyRun(onAgentError(args[0], args[1]));
      },
    ],
    [
      'agent_handoff',
      (...args: unknown[]) => {
        if (args.length < 2) {
          return;
        }

        noteContext(args[0], args.length >= 3 ? args[2] : args[1]);
        if (args.length >= 3) {
          safelyRun(onAgentHandoff(args[0], args[1], args[2]));
          return;
        }

        safelyRun(onAgentHandoff(args[0], undefined, args[1]));
      },
    ],
    [
      'agent_tool_start',
      (...args: unknown[]) => {
        if (args.length < 3) {
          return;
        }

        noteContext(args[0], args.length >= 4 ? args[1] : undefined);
        if (args.length >= 4) {
          safelyRun(onToolStart(args[0], args[1], args[2], args[3]));
          return;
        }

        safelyRun(onToolStart(args[0], undefined, args[1], args[2]));
      },
    ],
    [
      'agent_tool_end',
      (...args: unknown[]) => {
        if (args.length < 4) {
          return;
        }

        noteContext(args[0], args.length >= 5 ? args[1] : undefined);
        if (args.length >= 5) {
          safelyRun(onToolEnd(args[0], args[3], args[4]));
          return;
        }

        safelyRun(onToolEnd(args[0], args[2], args[3]));
      },
    ],
    [
      'agent_tool_error',
      (...args: unknown[]) => {
        if (args.length < 4) {
          return;
        }

        if (args.length >= 5) {
          safelyRun(onToolError(args[0], args[3], args[4]));
          return;
        }

        safelyRun(onToolError(args[0], args[2], args[3]));
      },
    ],
  ];

  for (const [event, listener] of listeners) {
    target.on(event, listener);
  }

  return {
    handler,
    detach: () => {
      for (const [event, listener] of listeners) {
        target.off(event, listener);
      }
      for (const restore of restorers.splice(0).reverse()) {
        restore();
      }
    },
  };
}

export const attachAgento11yOpenAIAgentsHooks = withAgento11yOpenAIAgentsHooks;

function isHookTarget(value: unknown): value is OpenAIAgentsHookTarget {
  if (!isRecord(value)) {
    return false;
  }
  return typeof value.on === 'function' && typeof value.off === 'function';
}

function mapTurnInputToMessages(turnInput: unknown): Array<{ role: string; content: string }> {
  if (!Array.isArray(turnInput)) {
    const text = normalizeOutputText(turnInput);
    if (text.length === 0) {
      return [];
    }
    return [{ role: 'user', content: text }];
  }

  const messages: Array<{ role: string; content: string }> = [];
  for (const item of turnInput) {
    const role = asString(read(item, 'role')) || 'user';
    const content = normalizeOutputText(read(item, 'content') ?? item);
    if (content.length === 0) {
      continue;
    }
    messages.push({ role, content });
  }
  return messages;
}

function normalizeOutputText(value: unknown): string {
  if (typeof value === 'string') {
    return value.trim();
  }

  if (Array.isArray(value)) {
    const parts = value.map((entry) => normalizeOutputText(entry)).filter((entry) => entry.length > 0);
    return parts.join(' ').trim();
  }

  if (isRecord(value)) {
    const directText = asString(read(value, 'text'));
    if (directText.length > 0) {
      return directText;
    }

    const outputText = asString(read(value, 'output_text'));
    if (outputText.length > 0) {
      return outputText;
    }

    const content = read(value, 'content');
    const contentText = normalizeOutputText(content);
    if (contentText.length > 0) {
      return contentText;
    }

    const message = read(value, 'message');
    const messageText = normalizeOutputText(message);
    if (messageText.length > 0) {
      return messageText;
    }

    const finalOutput = read(value, 'finalOutput');
    const finalOutputText = normalizeOutputText(finalOutput);
    if (finalOutputText.length > 0) {
      return finalOutputText;
    }

    const maybeStringified = safeStringify(value);
    if (maybeStringified.length > 0 && maybeStringified !== '{}') {
      return maybeStringified;
    }
  }

  return '';
}

function resolveConversationId(context: unknown): string {
  if (!isRecord(context)) {
    return '';
  }

  const candidates = [
    asString(read(context, 'conversationId')),
    asString(read(context, 'conversation_id')),
    asString(read(context, 'sessionId')),
    asString(read(context, 'session_id')),
    asString(read(context, 'groupId')),
    asString(read(context, 'group_id')),
  ];

  const nestedContext = read(context, 'context');
  if (isRecord(nestedContext)) {
    candidates.push(
      asString(read(nestedContext, 'conversationId')),
      asString(read(nestedContext, 'conversation_id')),
      asString(read(nestedContext, 'sessionId')),
      asString(read(nestedContext, 'session_id')),
      asString(read(nestedContext, 'groupId')),
      asString(read(nestedContext, 'group_id')),
    );
  }

  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }

  return '';
}

function resolveModelName(agent: unknown): string {
  const direct = asString(read(agent, 'model'));
  if (direct.length > 0) {
    return direct;
  }

  const nestedModel = read(agent, 'model');
  if (isRecord(nestedModel)) {
    const nested =
      asString(read(nestedModel, 'model')) || asString(read(nestedModel, 'name')) || asString(read(nestedModel, 'id'));
    if (nested.length > 0) {
      return nested;
    }
  }

  return '';
}

function resolveAgentName(agent: unknown): string {
  return asString(read(agent, 'name')) || 'openai_agent';
}

function resolveToolName(tool: unknown, details: unknown): string {
  const fromTool = asString(read(tool, 'name'));
  if (fromTool.length > 0) {
    return fromTool;
  }

  const toolCall = read(details, 'toolCall');
  const fromCall = asString(read(toolCall, 'name'));
  if (fromCall.length > 0) {
    return fromCall;
  }

  return 'framework_tool';
}

function resolveToolCallId(details: unknown): string {
  const toolCall = read(details, 'toolCall');
  return asString(read(toolCall, 'callId')) || asString(read(toolCall, 'id'));
}

function resolveToolArguments(details: unknown): unknown {
  const toolCall = read(details, 'toolCall');
  const raw = read(toolCall, 'arguments');
  if (typeof raw !== 'string') {
    return raw;
  }

  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

function readUsage(context: unknown): AnyRecord | undefined {
  if (!isRecord(context)) {
    return undefined;
  }

  const usage = read(context, 'usage');
  if (!isRecord(usage)) {
    return undefined;
  }

  const promptTokens = asNumber(read(usage, 'input_tokens')) ?? asNumber(read(usage, 'inputTokens'));
  const completionTokens = asNumber(read(usage, 'output_tokens')) ?? asNumber(read(usage, 'outputTokens'));
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

function isModel(value: unknown): value is AnyRecord {
  return isRecord(value) && typeof value.getResponse === 'function' && typeof value.getStreamedResponse === 'function';
}

// OpenAIResponsesModel keeps its model id on `_model`. Models that hide it
// (the AI SDK adapter) record "unknown" until the response names the model.
function resolveModelObjectName(model: AnyRecord): string {
  return asString(model._model) || asString(model.modelId) || asString(model.model);
}

type MappedRequest = Omit<MappedGenerationStart, 'stream'>;

function mapModelRequest(request: unknown, modelName: string): MappedRequest {
  const [prefixProvider, name] = splitProviderPrefix(modelName.trim());
  const systemChunks: string[] = [];
  const instructions = asString(read(request, 'systemInstructions'));
  if (instructions.length > 0) {
    systemChunks.push(instructions);
  }

  const rawInput = read(request, 'input');
  const items: unknown[] = typeof rawInput === 'string' ? [{ role: 'user', content: rawInput }] : asArray(rawInput);
  const toolNames = new Map<string, string>();
  for (const item of items) {
    if (asString(read(item, 'type')) === 'function_call') {
      toolNames.set(asString(read(item, 'callId')), asString(read(item, 'name')));
    }
  }
  const input = mapResponseItems(items, 'user', toolNames, systemChunks);

  const settings = read(request, 'modelSettings');
  const toolChoice = read(settings, 'toolChoice');
  return {
    modelName: name,
    provider: prefixProvider,
    input,
    systemPrompt: systemChunks.length > 0 ? systemChunks.join('\n\n') : undefined,
    tools: mapToolDefinitions(read(request, 'tools')),
    temperature: asNumber(read(settings, 'temperature')),
    maxTokens: asNumber(read(settings, 'maxTokens')),
    topP: asNumber(read(settings, 'topP')),
    toolChoice: typeof toolChoice === 'string' ? toolChoice : undefined,
  };
}

function mapModelResponse(response: unknown): MappedGenerationEnd {
  const items = asArray(read(response, 'output'));
  const responseId = asString(read(response, 'responseId')) || asString(read(response, 'id'));
  const [responseProvider, responseModel] = resolveResponseModel(response, items);
  return {
    output: mapResponseItems(items, 'assistant', new Map(), []),
    usage: mapAgentsUsage(read(response, 'usage')),
    responseId: responseId === 'FAKE_ID' ? undefined : responseId,
    responseModel,
    responseProvider,
    stopReason: stopReasonFromItems(items),
  };
}

// AI SDK provider ids (`anthropic.messages`, `google.generative-ai`, ...) that
// name a provider agento11y records directly.
const AI_SDK_PROVIDERS: Record<string, string> = {
  openai: 'openai',
  anthropic: 'anthropic',
  google: 'gemini',
  'amazon-bedrock': 'bedrock',
};

// The provider's model: `providerData.model` from the Responses and Chat
// Completions models, or the `<provider>.<api>:<modelId>` tag the AI SDK
// adapter puts on each output item.
function resolveResponseModel(response: unknown, items: unknown[]): [string, string] {
  const providerData = read(response, 'providerData');
  const direct = asString(read(providerData, 'model')) || asString(read(read(providerData, 'response'), 'modelId'));
  for (const item of items) {
    const tagged = asString(read(read(item, 'providerData'), 'model'));
    const colon = tagged.indexOf(':');
    if (colon > 0 && colon < tagged.length - 1) {
      const vendor = (tagged.slice(0, colon).split('.')[0] ?? '').trim().toLowerCase();
      return [AI_SDK_PROVIDERS[vendor] ?? '', direct || tagged.slice(colon + 1).trim()];
    }
  }
  const [provider, name] = splitProviderPrefix(direct);
  return [provider, name];
}

// Output items that end the turn so the runner can execute a client-side tool.
const CLIENT_TOOL_CALL_TYPES = new Set([
  'function_call',
  'computer_call',
  'shell_call',
  'apply_patch_call',
  'local_shell_call',
  'custom_tool_call',
]);

// ModelResponse drops the provider's finish reason, but the SDK's models turn it
// into output items: a tool call means `tool_calls`, a completed message `stop`.
function stopReasonFromItems(items: unknown[]): string | undefined {
  if (items.some((item) => CLIENT_TOOL_CALL_TYPES.has(asString(read(item, 'type'))))) {
    return 'tool_calls';
  }
  const statuses = items
    .filter((item) => asString(read(item, 'type')) === 'message' || asString(read(item, 'role')) === 'assistant')
    .map((item) => asString(read(item, 'status')));
  if (statuses.length > 0 && statuses.every((status) => status === '' || status === 'completed')) {
    return 'stop';
  }
  return undefined;
}

// Maps Responses-style items (messages, function calls, call results).
// Consecutive function calls share one assistant message, as the model
// emitted them in one turn. System and developer messages go to systemChunks.
function mapResponseItems(
  items: unknown[],
  defaultRole: 'user' | 'assistant',
  toolNames: Map<string, string>,
  systemChunks: string[],
): Message[] {
  const messages: Message[] = [];
  for (const item of items) {
    const itemType = asString(read(item, 'type'));
    if (itemType === 'function_call') {
      const name = asString(read(item, 'name'));
      if (name.length === 0) {
        continue;
      }
      const part: MessagePart = {
        type: 'tool_call',
        toolCall: {
          id: asString(read(item, 'callId')) || undefined,
          name,
          inputJSON: toolArgumentsJSON(read(item, 'arguments')),
        },
      };
      const last = messages[messages.length - 1];
      if (
        last !== undefined &&
        last.role === 'assistant' &&
        (last.parts ?? []).length > 0 &&
        (last.parts ?? []).every((existing) => existing.type === 'tool_call')
      ) {
        last.parts?.push(part);
      } else {
        messages.push({ role: 'assistant', parts: [part] });
      }
      continue;
    }
    if (itemType === 'function_call_result') {
      const callId = asString(read(item, 'callId'));
      const name = asString(read(item, 'name')) || toolNames.get(callId) || '';
      messages.push({
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            toolResult: {
              ...(callId.length > 0 ? { toolCallId: callId } : {}),
              ...(name.length > 0 ? { name } : {}),
              content: toolResultText(read(item, 'output')),
            },
          },
        ],
      });
      continue;
    }
    if (itemType.length > 0 && itemType !== 'message') {
      // Reasoning, hosted tool calls and other item kinds carry no chat text.
      continue;
    }
    const role = asString(read(item, 'role')).toLowerCase();
    const text = contentText(read(item, 'content'));
    if (text.length === 0) {
      continue;
    }
    if (role === 'system' || role === 'developer') {
      systemChunks.push(text);
      continue;
    }
    messages.push({
      role: role === 'assistant' || role === 'user' ? role : defaultRole,
      parts: [{ type: 'text', text }],
    });
  }
  return messages;
}

function contentText(content: unknown): string {
  if (typeof content === 'string') {
    return content.trim();
  }
  const parts: string[] = [];
  for (const entry of asArray(content)) {
    const kind = asString(read(entry, 'type'));
    const text =
      kind === 'refusal'
        ? asString(read(entry, 'refusal'))
        : typeof entry === 'string'
          ? entry.trim()
          : asString(read(entry, 'text'));
    if (text.length > 0) {
      parts.push(text);
    }
  }
  return parts.join(' ').trim();
}

function toolArgumentsJSON(raw: unknown): string | undefined {
  if (typeof raw === 'string') {
    return raw.length > 0 ? raw : undefined;
  }
  if (raw === undefined || raw === null) {
    return undefined;
  }
  const encoded = safeStringify(raw);
  return encoded.length > 0 ? encoded : undefined;
}

function toolResultText(output: unknown): string {
  if (typeof output === 'string') {
    return output;
  }
  if (Array.isArray(output)) {
    const texts = output.map((entry) => toolResultText(entry)).filter((entry) => entry.length > 0);
    return texts.join('\n');
  }
  if (isRecord(output)) {
    const text = read(output, 'text');
    if (typeof text === 'string') {
      return text;
    }
  }
  return output === undefined || output === null ? '' : safeStringify(output);
}

function mapToolDefinitions(raw: unknown): ToolDefinition[] {
  const definitions: ToolDefinition[] = [];
  for (const tool of asArray(raw)) {
    const name = asString(read(tool, 'name'));
    if (name.length === 0) {
      continue;
    }
    const description = asString(read(tool, 'description'));
    const parameters = read(tool, 'parameters');
    const toolType = asString(read(tool, 'type'));
    definitions.push({
      name,
      ...(description.length > 0 ? { description } : {}),
      // Function tools carry a parameter schema. Hosted tools have none; their
      // name is the API tool kind (web_search, file_search, ...).
      type: isRecord(parameters) ? 'function' : toolType || name,
      ...(isRecord(parameters) ? { inputSchemaJSON: sortedJSONString(parameters) } : {}),
    });
  }
  return definitions;
}

// Agents SDK usage follows the OpenAI convention for every model it wraps:
// input tokens include the cached ones, which are reported as details.
function mapAgentsUsage(raw: unknown): TokenUsage | undefined {
  if (!isRecord(raw)) {
    return undefined;
  }
  const inputTokens = asNumber(read(raw, 'inputTokens')) ?? asNumber(read(raw, 'input_tokens')) ?? 0;
  const outputTokens = asNumber(read(raw, 'outputTokens')) ?? asNumber(read(raw, 'output_tokens')) ?? 0;
  const totalTokens =
    asNumber(read(raw, 'totalTokens')) ?? asNumber(read(raw, 'total_tokens')) ?? inputTokens + outputTokens;
  const inputDetails = read(raw, 'inputTokensDetails') ?? read(raw, 'input_tokens_details');
  const outputDetails = read(raw, 'outputTokensDetails') ?? read(raw, 'output_tokens_details');
  const cacheRead = sumDetail(inputDetails, 'cached_tokens');
  const cacheWrite = sumDetail(inputDetails, 'cache_write_tokens');
  const reasoning = sumDetail(outputDetails, 'reasoning_tokens');
  if (inputTokens === 0 && outputTokens === 0 && totalTokens === 0) {
    return undefined;
  }
  return {
    inputTokens,
    outputTokens,
    totalTokens,
    ...(cacheRead > 0 ? { cacheReadInputTokens: cacheRead } : {}),
    ...(cacheWrite > 0 ? { cacheWriteInputTokens: cacheWrite } : {}),
    ...(reasoning > 0 ? { reasoningTokens: reasoning } : {}),
    inputSemantics: 'inclusive',
  };
}

// `Usage` keeps one details record per request; a raw stream event has one object.
function sumDetail(details: unknown, key: string): number {
  const entries = Array.isArray(details) ? details : [details];
  let total = 0;
  for (const entry of entries) {
    total += asNumber(read(entry, key)) ?? 0;
  }
  return total;
}

/** Key order is sorted so a schema encodes the same here and in the Python adapter. */
function sortedJSONString(value: unknown): string | undefined {
  try {
    return JSON.stringify(value, (_key, entry: unknown) => {
      if (isRecord(entry) && !Array.isArray(entry)) {
        return Object.fromEntries(
          Object.keys(entry)
            .sort()
            .map((key) => [key, entry[key]]),
        );
      }
      return entry;
    });
  } catch {
    return undefined;
  }
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function safelyRun(task: Promise<unknown>): void {
  void task.catch((error) => {
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

function asString(value: unknown): string {
  if (typeof value !== 'string') {
    return '';
  }
  return value.trim();
}

function asNumber(value: unknown): number | undefined {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return undefined;
  }
  return value;
}

function safeStringify(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return '';
  }
}

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  return new Error(typeof value === 'string' ? value : 'framework callback failed');
}
