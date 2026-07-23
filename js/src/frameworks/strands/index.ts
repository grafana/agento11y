import { createRequire } from 'node:module';
import type { LocalAgent, Plugin } from '@strands-agents/sdk';
import type { Agento11yClient } from '../../client.js';
import type {
  GenerationRecorder,
  GenerationStart,
  Message,
  MessagePart,
  TokenUsage,
  ToolDefinition,
  ToolExecutionRecorder,
} from '../../types.js';

type AnyRecord = Record<string, unknown>;
type HookCleanup = () => void;

export type StrandsProviderResolver =
  | 'auto'
  | 'none'
  | ((params: { modelName: string; model: unknown; agent: unknown; modelConfig: unknown }) => string | undefined);

export interface Agento11yStrandsOptions {
  agentName?: string;
  agentVersion?: string;
  conversationId?: string;
  conversationTitle?: string;
  userId?: string;
  provider?: string;
  providerResolver?: StrandsProviderResolver;
  captureInputs?: boolean;
  captureOutputs?: boolean;
  extraTags?: Record<string, string>;
  extraMetadata?: Record<string, unknown>;
  resolveConversationId?: (event: unknown, agent: unknown) => string | undefined;
}

type StrandsAgentConfig = Record<string, unknown> & { plugins?: unknown };
type HookableAgent = AnyRecord & {
  addHook(eventType: unknown, callback: (event: unknown) => unknown): HookCleanup;
};
type StrandsRuntime = {
  BeforeInvocationEvent: unknown;
  AfterInvocationEvent: unknown;
  BeforeModelCallEvent: unknown;
  ModelStreamUpdateEvent: unknown;
  AfterModelCallEvent: unknown;
  BeforeToolCallEvent: unknown;
  AfterToolCallEvent: unknown;
};

interface ModelRunState {
  recorder: GenerationRecorder;
  inputMessages: Message[];
  captureOutputs: boolean;
  outputChunks: string[];
  firstTokenRecorded: boolean;
  usage?: TokenUsage;
}

interface ToolRunState {
  recorder: ToolExecutionRecorder;
  input: unknown;
}

const frameworkName = 'strands';
const frameworkSource = 'hooks';
const frameworkLanguage = 'typescript';
const agento11yPluginMarker = '__agento11yStrandsPlugin';
const instrumentedAgents = new WeakSet<object>();

export class Agento11yStrandsHookProvider {
  private readonly agentName?: string;
  private readonly agentVersion?: string;
  private readonly conversationId?: string;
  private readonly conversationTitle?: string;
  private readonly userId?: string;
  private readonly provider?: string;
  private readonly providerResolver: StrandsProviderResolver;
  private readonly captureInputs: boolean;
  private readonly captureOutputs: boolean;
  private readonly extraTags: Record<string, string>;
  private readonly extraMetadata: Record<string, unknown>;
  private readonly resolveConversationIdFn?: Agento11yStrandsOptions['resolveConversationId'];
  private readonly invocationRunIds = new WeakMap<object, string[]>();
  private readonly modelRunIds = new WeakMap<object, string[]>();
  private readonly modelRuns = new Map<string, ModelRunState>();
  private readonly toolRunIds = new Map<string, string>();
  private readonly fallbackToolRunIds = new WeakMap<object, string[]>();
  private readonly toolRuns = new Map<string, ToolRunState>();
  private sequence = 0;

  constructor(
    private readonly client: Agento11yClient,
    options: Agento11yStrandsOptions = {},
  ) {
    this.agentName = normalizeOptionalString(options.agentName);
    this.agentVersion = normalizeOptionalString(options.agentVersion);
    this.conversationId = normalizeOptionalString(options.conversationId);
    this.conversationTitle = normalizeOptionalString(options.conversationTitle);
    this.userId = normalizeOptionalString(options.userId);
    this.provider = normalizeOptionalString(options.provider);
    this.providerResolver = options.providerResolver ?? 'auto';
    this.captureInputs = options.captureInputs ?? true;
    this.captureOutputs = options.captureOutputs ?? true;
    this.extraTags = { ...(options.extraTags ?? {}) };
    this.extraMetadata = { ...(options.extraMetadata ?? {}) };
    this.resolveConversationIdFn = options.resolveConversationId;
  }

  registerHooks(agent: HookableAgent): HookCleanup[] {
    const strands = loadStrandsRuntime();
    return [
      agent.addHook(strands.BeforeInvocationEvent, (event) => this.beforeInvocation(event)),
      agent.addHook(strands.AfterInvocationEvent, (event) => this.afterInvocation(event)),
      agent.addHook(strands.BeforeModelCallEvent, (event) => this.beforeModelCall(event)),
      agent.addHook(strands.ModelStreamUpdateEvent, (event) => this.modelStreamUpdate(event)),
      agent.addHook(strands.AfterModelCallEvent, (event) => this.afterModelCall(event)),
      agent.addHook(strands.BeforeToolCallEvent, (event) => this.beforeToolCall(event)),
      agent.addHook(strands.AfterToolCallEvent, (event) => this.afterToolCall(event)),
    ];
  }

  beforeInvocation(event: unknown): void {
    const runId = this.nextRunId('invocation');
    this.stackFor(this.invocationRunIds, event).push(runId);
  }

  afterInvocation(event: unknown): void {
    this.popStack(this.invocationRunIds, event);
  }

  beforeModelCall(event: unknown): void {
    const agent = read(event, 'agent');
    const model = read(event, 'model') ?? read(agent, 'model');
    const modelConfig = modelConfigFromModel(model);
    const modelName = modelNameFrom(model, modelConfig);
    const provider = this.resolveProvider(modelName, model, agent, modelConfig);
    const runId = this.nextRunId('model');
    this.stackFor(this.modelRunIds, event).push(runId);

    const context = this.buildContext(event, agent, runId, 'chat', this.peekStack(this.invocationRunIds, event));
    const start: GenerationStart = {
      conversationId: context.conversationId,
      conversationTitle: this.conversationTitle,
      userId: this.userId,
      agentName: this.agentName ?? agentName(agent),
      agentVersion: this.agentVersion,
      mode: 'STREAM',
      operationName: 'strands.invoke',
      model: { provider, name: modelName },
      systemPrompt: systemPromptText(read(agent, 'systemPrompt')),
      tools: toolDefinitions(agent),
      maxTokens: optionalNumber(read(modelConfig, 'maxTokens')),
      temperature: optionalNumber(read(modelConfig, 'temperature')),
      topP: optionalNumber(read(modelConfig, 'topP')),
      toolChoice: toolChoiceString(read(modelConfig, 'toolChoice')),
      tags: this.frameworkTags(),
      metadata: context.metadata,
    };
    const recorder = this.client.startStreamingGeneration(start);
    this.modelRuns.set(runId, {
      recorder,
      inputMessages: this.captureInputs ? mapMessages(read(agent, 'messages')) : [],
      captureOutputs: this.captureOutputs,
      outputChunks: [],
      firstTokenRecorded: false,
    });
  }

  modelStreamUpdate(event: unknown): void {
    const runId = this.peekStack(this.modelRunIds, event);
    if (runId === undefined) {
      return;
    }
    const state = this.modelRuns.get(runId);
    if (state === undefined) {
      return;
    }

    const innerEvent = read(event, 'event');
    const usage = mapUsage(read(innerEvent, 'usage'));
    if (usage !== undefined) {
      state.usage = usage;
    }

    const text = textDelta(innerEvent);
    if (text.length === 0) {
      return;
    }
    if (state.captureOutputs) {
      state.outputChunks.push(text);
    }
    if (!state.firstTokenRecorded) {
      state.firstTokenRecorded = true;
      state.recorder.setFirstTokenAt(new Date());
    }
  }

  afterModelCall(event: unknown): void {
    const runId = this.popStack(this.modelRunIds, event);
    if (runId === undefined) {
      return;
    }
    const state = this.modelRuns.get(runId);
    if (state === undefined) {
      return;
    }
    this.modelRuns.delete(runId);

    try {
      const error = read(event, 'error');
      if (error !== undefined) {
        state.recorder.setCallError(error);
      }

      const stopData = read(event, 'stopData');
      const message = read(stopData, 'message');
      let output = this.captureOutputs ? mapMessages([message]) : undefined;
      if ((output?.length ?? 0) === 0 && state.captureOutputs && state.outputChunks.length > 0) {
        output = [{ role: 'assistant', content: state.outputChunks.join('') }];
      }
      const usage = mapUsage(read(read(message, 'metadata'), 'usage')) ?? state.usage;
      state.recorder.setResult({
        input: state.inputMessages,
        output,
        usage,
        stopReason: asString(read(stopData, 'stopReason')) || undefined,
        responseModel: modelNameFrom(read(event, 'model'), modelConfigFromModel(read(event, 'model'))) || undefined,
      });
    } finally {
      state.recorder.end();
    }

    const recorderError = state.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  beforeToolCall(event: unknown): void {
    const agent = read(event, 'agent');
    const toolUse = asRecord(read(event, 'toolUse')) ?? {};
    const tool = read(event, 'tool');
    const toolUseId = asString(read(toolUse, 'toolUseId'));
    const runId = this.nextRunId('tool');

    if (toolUseId.length > 0) {
      this.toolRunIds.set(this.toolKey(event, toolUseId), runId);
    } else {
      this.stackFor(this.fallbackToolRunIds, event).push(runId);
    }

    const model = read(agent, 'model');
    const modelConfig = modelConfigFromModel(model);
    const context = this.buildContext(event, agent, runId, 'tool', this.peekStack(this.modelRunIds, event));
    const toolName = firstNonEmpty(asString(read(toolUse, 'name')), asString(read(tool, 'name')), 'framework_tool');
    const recorder = this.client.startToolExecution({
      toolName,
      toolCallId: toolUseId || undefined,
      toolType: 'strands',
      toolDescription: firstNonEmpty(
        asString(read(tool, 'description')),
        asString(read(read(tool, 'toolSpec'), 'description')),
      ),
      conversationId: context.conversationId,
      conversationTitle: this.conversationTitle,
      agentName: this.agentName ?? agentName(agent),
      agentVersion: this.agentVersion,
      requestModel: modelNameFrom(model, modelConfig),
      requestProvider: this.resolveProvider(modelNameFrom(model, modelConfig), model, agent, modelConfig),
    });
    this.toolRuns.set(runId, {
      recorder,
      input: read(toolUse, 'input'),
    });
  }

  afterToolCall(event: unknown): void {
    const toolUse = asRecord(read(event, 'toolUse')) ?? {};
    const toolUseId = asString(read(toolUse, 'toolUseId'));
    const runId =
      toolUseId.length > 0
        ? this.toolRunIds.get(this.toolKey(event, toolUseId))
        : this.popStack(this.fallbackToolRunIds, event);
    if (runId === undefined) {
      return;
    }
    if (toolUseId.length > 0) {
      this.toolRunIds.delete(this.toolKey(event, toolUseId));
    }

    const state = this.toolRuns.get(runId);
    if (state === undefined) {
      return;
    }
    this.toolRuns.delete(runId);

    try {
      const error = read(event, 'error');
      if (error !== undefined) {
        state.recorder.setCallError(error);
      }
      state.recorder.setResult({
        arguments: toJSONSafe(state.input),
        result: toolResultToJSON(read(event, 'result')),
      });
    } finally {
      state.recorder.end();
    }

    const recorderError = state.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  private nextRunId(prefix: string): string {
    this.sequence += 1;
    return `${prefix}:${this.sequence}`;
  }

  private buildContext(
    event: unknown,
    agent: unknown,
    runId: string,
    runType: string,
    parentRunId: string | undefined,
  ): { conversationId: string; metadata: Record<string, unknown> } {
    const conversationId = this.resolveConversationId(event, agent, runId);
    const metadata: Record<string, unknown> = normalizeMetadata({
      ...this.extraMetadata,
      conversation_id: conversationId,
      'agento11y.framework.run_id': runId,
      'agento11y.framework.run_type': runType,
      'agento11y.framework.component_name': agentName(agent),
      ...metadataFromAgentState(agent),
    });
    if (parentRunId !== undefined) {
      metadata['agento11y.framework.parent_run_id'] = parentRunId;
    }
    const eventId = firstNonEmpty(asString(read(read(event, 'toolUse'), 'toolUseId')), asString(read(event, 'id')));
    if (eventId.length > 0) {
      metadata['agento11y.framework.event_id'] = eventId;
    }
    return { conversationId, metadata };
  }

  private resolveConversationId(event: unknown, agent: unknown, runId: string): string {
    const resolved = normalizeOptionalString(this.resolveConversationIdFn?.(event, agent));
    return firstNonEmpty(
      this.conversationId ?? '',
      resolved ?? '',
      conversationIdFromPayload(metadataFromAgentState(agent)),
      conversationIdFromPayload(this.extraMetadata),
      `agento11y:framework:${frameworkName}:${runId}`,
    );
  }

  private resolveProvider(modelName: string, model: unknown, agent: unknown, modelConfig: unknown): string {
    if (this.provider !== undefined) {
      return this.provider;
    }
    const configProvider = firstNonEmpty(asString(read(modelConfig, 'provider')), asString(read(model, 'provider')));
    if (configProvider.length > 0) {
      return normalizeProvider(configProvider);
    }
    if (typeof this.providerResolver === 'function') {
      return normalizeProvider(this.providerResolver({ modelName, model, agent, modelConfig }) ?? '') || 'custom';
    }
    if (this.providerResolver === 'none') {
      return '';
    }
    return inferProviderFromModelName(modelName);
  }

  private frameworkTags(): Record<string, string> {
    return {
      ...this.extraTags,
      'agento11y.framework.name': frameworkName,
      'agento11y.framework.source': frameworkSource,
      'agento11y.framework.language': frameworkLanguage,
    };
  }

  private stackFor(store: WeakMap<object, string[]>, event: unknown): string[] {
    const source = eventSource(event);
    const existing = store.get(source);
    if (existing !== undefined) {
      return existing;
    }
    const created: string[] = [];
    store.set(source, created);
    return created;
  }

  private peekStack(store: WeakMap<object, string[]>, event: unknown): string | undefined {
    const stack = store.get(eventSource(event)) ?? [];
    return stack[stack.length - 1];
  }

  private popStack(store: WeakMap<object, string[]>, event: unknown): string | undefined {
    const source = eventSource(event);
    const stack = store.get(source) ?? [];
    const runId = stack.pop();
    if (stack.length === 0) {
      store.delete(source);
    }
    return runId;
  }

  private toolKey(event: unknown, toolUseId: string): string {
    return `${sourceId(eventSource(event))}:${toolUseId}`;
  }
}

export class Agento11yStrandsPlugin implements Plugin {
  readonly name = 'agento11y-strands-plugin';
  readonly [agento11yPluginMarker] = true;

  constructor(
    private readonly client: Agento11yClient,
    private readonly options: Agento11yStrandsOptions = {},
  ) {}

  initAgent(agent: LocalAgent): void {
    const target = agent as unknown as HookableAgent;
    if (instrumentedAgents.has(target)) {
      return;
    }
    const provider = createAgento11yStrandsHookProvider(this.client, this.options);
    provider.registerHooks(target);
    instrumentedAgents.add(target);
  }
}

export function createAgento11yStrandsHookProvider(
  client: Agento11yClient,
  options: Agento11yStrandsOptions = {},
): Agento11yStrandsHookProvider {
  return new Agento11yStrandsHookProvider(client, options);
}

export function createAgento11yStrandsPlugin(
  client: Agento11yClient,
  options: Agento11yStrandsOptions = {},
): Agento11yStrandsPlugin {
  return new Agento11yStrandsPlugin(client, options);
}

export function withAgento11yStrandsHooks<T extends StrandsAgentConfig | HookableAgent | undefined>(
  configOrAgent: T,
  client: Agento11yClient,
  options: Agento11yStrandsOptions = {},
): T extends undefined ? StrandsAgentConfig : T {
  const plugin = createAgento11yStrandsPlugin(client, options);
  if (configOrAgent === undefined || !isHookableAgent(configOrAgent)) {
    const config = { ...(configOrAgent ?? {}) } as StrandsAgentConfig;
    const plugins = asArray(config.plugins);
    if (!plugins.some(isAgento11yPlugin)) {
      plugins.push(plugin);
    }
    config.plugins = plugins;
    return config as T extends undefined ? StrandsAgentConfig : T;
  }

  if (!instrumentedAgents.has(configOrAgent)) {
    createAgento11yStrandsHookProvider(client, options).registerHooks(configOrAgent);
    instrumentedAgents.add(configOrAgent);
  }
  return configOrAgent as T extends undefined ? StrandsAgentConfig : T;
}

function mapMessages(messages: unknown): Message[] {
  if (!Array.isArray(messages)) {
    return [];
  }
  const output: Message[] = [];
  for (const message of messages) {
    const mapped = mapMessage(message);
    if (mapped !== undefined) {
      output.push(mapped);
    }
  }
  return output;
}

function mapMessage(message: unknown): Message | undefined {
  if (message === undefined || message === null) {
    return undefined;
  }
  const parts: MessagePart[] = [];
  let containsToolResult = false;
  const content = read(message, 'content');

  for (const block of Array.isArray(content) ? content : [content]) {
    const mappedParts = mapContentBlock(block);
    for (const part of mappedParts) {
      if (part.type === 'tool_result') {
        containsToolResult = true;
      }
      parts.push(part);
    }
  }

  if (parts.length === 0) {
    return undefined;
  }
  if (containsToolResult) {
    return { role: 'tool', parts: parts.filter((part) => part.type === 'tool_result') };
  }
  return {
    role: normalizeRole(asString(read(message, 'role'))),
    parts,
  };
}

function mapContentBlock(block: unknown): MessagePart[] {
  if (typeof block === 'string') {
    const text = block.trim();
    return text.length > 0 ? [{ type: 'text', text }] : [];
  }
  const text = asString(read(block, 'text'));
  if (text.length > 0) {
    return [{ type: 'text', text }];
  }
  const reasoning =
    read(block, 'reasoning') ?? (asString(read(block, 'type')) === 'reasoningBlock' ? block : undefined);
  const thinking = firstNonEmpty(asString(read(reasoning, 'text')), asString(read(block, 'thinking')));
  if (thinking.length > 0) {
    return [{ type: 'thinking', thinking }];
  }
  const toolUse = read(block, 'toolUse') ?? (asString(read(block, 'type')) === 'toolUseBlock' ? block : undefined);
  const toolName = asString(read(toolUse, 'name'));
  if (toolName.length > 0) {
    return [
      {
        type: 'tool_call',
        toolCall: {
          id: asString(read(toolUse, 'toolUseId')) || undefined,
          name: toolName,
          inputJSON: jsonString(read(toolUse, 'input')),
        },
      },
    ];
  }
  const toolResult =
    read(block, 'toolResult') ?? (asString(read(block, 'type')) === 'toolResultBlock' ? block : undefined);
  if (toolResult !== undefined) {
    return [
      {
        type: 'tool_result',
        toolResult: {
          toolCallId: asString(read(toolResult, 'toolUseId')) || undefined,
          content: toolResultText(read(toolResult, 'content')),
          contentJSON: jsonString(read(toolResult, 'content')),
          isError: asString(read(toolResult, 'status')).toLowerCase() === 'error',
        },
      },
    ];
  }
  return [];
}

function toolDefinitions(agent: unknown): ToolDefinition[] {
  const tools = Array.isArray(read(agent, 'tools')) ? (read(agent, 'tools') as unknown[]) : registryTools(agent);
  const definitions: ToolDefinition[] = [];
  for (const tool of tools) {
    const spec = read(tool, 'toolSpec') ?? tool;
    const name = firstNonEmpty(asString(read(spec, 'name')), asString(read(tool, 'name')));
    if (name.length === 0) {
      continue;
    }
    definitions.push({
      name,
      description: firstNonEmpty(asString(read(spec, 'description')), asString(read(tool, 'description'))) || undefined,
      type: 'strands',
      inputSchemaJSON: jsonString(read(spec, 'inputSchema')),
    });
  }
  return definitions;
}

function registryTools(agent: unknown): unknown[] {
  const registry = read(agent, 'toolRegistry');
  const list = read(registry, 'list');
  if (typeof list !== 'function') {
    return [];
  }
  try {
    const tools = list.call(registry);
    return Array.isArray(tools) ? tools : [];
  } catch {
    return [];
  }
}

function modelConfigFromModel(model: unknown): unknown {
  const getConfig = read(model, 'getConfig');
  if (typeof getConfig === 'function') {
    try {
      return getConfig.call(model);
    } catch {
      return {};
    }
  }
  return read(model, 'config') ?? {};
}

function modelNameFrom(model: unknown, modelConfig: unknown): string {
  return firstNonEmpty(
    asString(read(modelConfig, 'modelId')),
    asString(read(modelConfig, 'model')),
    asString(read(modelConfig, 'modelName')),
    asString(read(model, 'modelId')),
    asString(read(model, 'model')),
    asString(read(model, 'modelName')),
    asString(read(model, 'name')),
    'unknown',
  );
}

function mapUsage(rawUsage: unknown): TokenUsage | undefined {
  const usage = asRecord(rawUsage);
  if (usage === undefined) {
    return undefined;
  }
  const inputTokens = optionalNumber(read(usage, 'inputTokens')) ?? optionalNumber(read(usage, 'input_tokens'));
  const outputTokens = optionalNumber(read(usage, 'outputTokens')) ?? optionalNumber(read(usage, 'output_tokens'));
  const totalTokens =
    optionalNumber(read(usage, 'totalTokens')) ??
    optionalNumber(read(usage, 'total_tokens')) ??
    ((inputTokens ?? 0) + (outputTokens ?? 0) || undefined);
  if (inputTokens === undefined && outputTokens === undefined && totalTokens === undefined) {
    return undefined;
  }
  return {
    inputTokens,
    outputTokens,
    totalTokens,
    cacheReadInputTokens:
      optionalNumber(read(usage, 'cacheReadInputTokens')) ?? optionalNumber(read(usage, 'cache_read_input_tokens')),
    cacheWriteInputTokens:
      optionalNumber(read(usage, 'cacheWriteInputTokens')) ?? optionalNumber(read(usage, 'cache_write_input_tokens')),
    // Bedrock reports additive (non-cached) input, already disjoint-compliant.
    inputIsDisjoint: true,
  };
}

function metadataFromAgentState(agent: unknown): Record<string, unknown> {
  const state = read(agent, 'appState');
  const getAll = read(state, 'getAll');
  if (typeof getAll === 'function') {
    try {
      const values = getAll.call(state);
      return asRecord(values) ?? {};
    } catch {
      return {};
    }
  }
  return {};
}

function conversationIdFromPayload(payload: unknown): string {
  return firstNonEmpty(
    asString(read(payload, 'conversation_id')),
    asString(read(payload, 'conversationId')),
    asString(read(payload, 'session_id')),
    asString(read(payload, 'sessionId')),
    asString(read(payload, 'group_id')),
    asString(read(payload, 'groupId')),
  );
}

function systemPromptText(systemPrompt: unknown): string | undefined {
  if (typeof systemPrompt === 'string') {
    return systemPrompt.trim() || undefined;
  }
  const content = read(systemPrompt, 'content');
  if (typeof content === 'string') {
    return content.trim() || undefined;
  }
  if (Array.isArray(content)) {
    const text = content
      .map((block) => asString(read(block, 'text')))
      .filter((item) => item.length > 0)
      .join('\n');
    return text || undefined;
  }
  return undefined;
}

function textDelta(event: unknown): string {
  if (asString(read(event, 'type')) !== 'modelContentBlockDeltaEvent') {
    return '';
  }
  const delta = read(event, 'delta');
  if (asString(read(delta, 'type')) !== 'textDelta') {
    return '';
  }
  return asString(read(delta, 'text'));
}

function toolResultToJSON(result: unknown): unknown {
  const safe = toJSONSafe(result);
  return safe === undefined ? undefined : safe;
}

function toolResultText(content: unknown): string {
  if (typeof content === 'string') {
    return content.trim();
  }
  if (!Array.isArray(content)) {
    return '';
  }
  return content
    .map((item) => {
      const text = asString(read(item, 'text'));
      if (text.length > 0) {
        return text;
      }
      const json = read(item, 'json');
      return json === undefined ? '' : (jsonString(json) ?? '');
    })
    .filter((item) => item.length > 0)
    .join(' ')
    .trim();
}

function toJSONSafe(value: unknown): unknown {
  if (value instanceof Error) {
    return { name: value.name, message: value.message };
  }
  if (value === undefined) {
    return undefined;
  }
  try {
    return JSON.parse(JSON.stringify(value));
  } catch {
    return String(value);
  }
}

function jsonString(value: unknown): string | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  try {
    return JSON.stringify(value);
  } catch {
    return undefined;
  }
}

function normalizeMetadata(raw: Record<string, unknown>): Record<string, unknown> {
  const metadata: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(raw)) {
    if (key.trim().length === 0 || value === undefined) {
      continue;
    }
    metadata[key] = toJSONSafe(value);
  }
  return metadata;
}

function agentName(agent: unknown): string {
  return firstNonEmpty(asString(read(agent, 'name')), asString(read(agent, 'id')), className(agent), 'strands-agent');
}

function eventSource(event: unknown): object {
  const agent = read(event, 'agent');
  if (isRecord(agent)) {
    return agent;
  }
  return isRecord(event) ? event : sourceBox;
}

const sourceBox = {};
const sourceIds = new WeakMap<object, number>();
let sourceIdSequence = 0;
function sourceId(source: object): number {
  const existing = sourceIds.get(source);
  if (existing !== undefined) {
    return existing;
  }
  sourceIdSequence += 1;
  sourceIds.set(source, sourceIdSequence);
  return sourceIdSequence;
}

function toolChoiceString(value: unknown): string | undefined {
  if (typeof value === 'string') {
    return value.trim() || undefined;
  }
  return jsonString(value);
}

function inferProviderFromModelName(modelName: string): string {
  const normalized = modelName.toLowerCase();
  if (
    normalized.startsWith('gpt-') ||
    normalized.startsWith('o1') ||
    normalized.startsWith('o3') ||
    normalized.startsWith('o4')
  ) {
    return 'openai';
  }
  if (normalized.includes('claude')) {
    return 'anthropic';
  }
  if (normalized.includes('gemini')) {
    return 'gemini';
  }
  return normalized.length > 0 && normalized !== 'unknown' ? 'custom' : '';
}

function normalizeProvider(value: string): string {
  const normalized = value.trim().toLowerCase();
  if (normalized === 'openai' || normalized === 'anthropic' || normalized === 'gemini') {
    return normalized;
  }
  return normalized.length > 0 ? 'custom' : '';
}

function normalizeRole(role: string): string {
  const normalized = role.trim().toLowerCase();
  return normalized === 'assistant' || normalized === 'ai' ? 'assistant' : 'user';
}

function optionalNumber(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === 'string' && value.trim().length > 0) {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : undefined;
  }
  return undefined;
}

function asArray(value: unknown): unknown[] {
  if (Array.isArray(value)) {
    return [...value];
  }
  return value === undefined || value === null ? [] : [value];
}

function isAgento11yPlugin(value: unknown): boolean {
  return isRecord(value) && value[agento11yPluginMarker] === true;
}

function isHookableAgent(value: unknown): value is HookableAgent {
  return isRecord(value) && typeof value.addHook === 'function';
}

function loadStrandsRuntime(): StrandsRuntime {
  const errors: string[] = [];
  const requireFromCwd = createRequire(`${process.cwd()}/package.json`);
  try {
    return validateStrandsRuntime(requireFromCwd('@strands-agents/sdk'));
  } catch (error) {
    errors.push(error instanceof Error ? error.message : String(error));
  }

  const requireFromPackage = createRequire(import.meta.url);
  try {
    return validateStrandsRuntime(requireFromPackage('@strands-agents/sdk'));
  } catch (error) {
    errors.push(error instanceof Error ? error.message : String(error));
  }

  throw new Error(`@grafana/agento11y/strands requires @strands-agents/sdk to be installed: ${errors.join('; ')}`);
}

function validateStrandsRuntime(value: unknown): StrandsRuntime {
  const runtime = asRecord(value);
  const required = [
    'BeforeInvocationEvent',
    'AfterInvocationEvent',
    'BeforeModelCallEvent',
    'ModelStreamUpdateEvent',
    'AfterModelCallEvent',
    'BeforeToolCallEvent',
    'AfterToolCallEvent',
  ];
  if (runtime === undefined || required.some((key) => typeof runtime[key] !== 'function')) {
    throw new Error('@strands-agents/sdk did not expose the expected hook event constructors');
  }
  return runtime as StrandsRuntime;
}

function normalizeOptionalString(value: unknown): string | undefined {
  const normalized = asString(value);
  return normalized.length > 0 ? normalized : undefined;
}

function asRecord(value: unknown): AnyRecord | undefined {
  return isRecord(value) ? value : undefined;
}

function isRecord(value: unknown): value is AnyRecord {
  return typeof value === 'object' && value !== null;
}

function read(value: unknown, key: string): unknown {
  if (!isRecord(value)) {
    return undefined;
  }
  return value[key];
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function className(value: unknown): string {
  if (!isRecord(value)) {
    return '';
  }
  const ctor = read(value, 'constructor');
  return isRecord(ctor) ? asString(read(ctor, 'name')) : '';
}

function firstNonEmpty(...values: string[]): string {
  for (const value of values) {
    if (value.trim().length > 0) {
      return value.trim();
    }
  }
  return '';
}
