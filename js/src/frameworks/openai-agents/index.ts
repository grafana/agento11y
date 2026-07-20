import type { Agento11yClient } from '../../client.js';
import { Agento11yFrameworkHandler, type FrameworkHandlerOptions } from '../shared.js';

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

  const onAgentStart = async (context: unknown, agent: unknown, turnInput?: unknown): Promise<void> => {
    const stack = getStack(context);
    const runId = nextRunId('openai_agent');
    const parentRunId = stack.length > 0 ? stack[stack.length - 1] : undefined;
    stack.push(runId);

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

    const contextId = getContextId(context);
    if (contextId !== undefined) {
      fallbackToolRunIds.delete(contextId);
    }
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
        safelyRun(onAgentStart(args[0], args[1], args[2]));
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
