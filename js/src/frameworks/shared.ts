import { type Span, SpanKind, SpanStatusCode, type Tracer, trace } from '@opentelemetry/api';
import type { SigilClient } from '../client.js';
import type {
  GenerationRecorder,
  GenerationResult,
  Message,
  ModelRef,
  TokenUsage,
  ToolExecutionRecorder,
} from '../types.js';

type AnyRecord = Record<string, unknown>;

type ProviderResolverFn = (context: { modelName: string; serialized?: unknown; invocationParams?: unknown }) => string;

type FrameworkName = 'langchain' | 'langgraph' | 'openai-agents' | 'llamaindex' | 'google-adk' | 'vercel-ai-sdk';

const frameworkInstrumentationName = 'github.com/grafana/sigil/sdks/js/frameworks';
const spanAttrOperationName = 'gen_ai.operation.name';
const spanAttrConversationID = 'gen_ai.conversation.id';
const spanAttrFrameworkName = 'sigil.framework.name';
const spanAttrFrameworkSource = 'sigil.framework.source';
const spanAttrFrameworkLanguage = 'sigil.framework.language';
const spanAttrFrameworkRunID = 'sigil.framework.run_id';
const spanAttrFrameworkThreadID = 'sigil.framework.thread_id';
const spanAttrFrameworkParentRunID = 'sigil.framework.parent_run_id';
const spanAttrFrameworkComponentName = 'sigil.framework.component_name';
const spanAttrFrameworkRunType = 'sigil.framework.run_type';
const spanAttrFrameworkTags = 'sigil.framework.tags';
const spanAttrFrameworkRetryAttempt = 'sigil.framework.retry_attempt';
const spanAttrFrameworkLangGraphNode = 'sigil.framework.langgraph.node';
const spanAttrFrameworkEventID = 'sigil.framework.event_id';
const spanAttrErrorType = 'error.type';
const spanAttrErrorCategory = 'error.category';
const frameworkSource = 'handler';
const maxFrameworkMetadataDepth = 5;

export interface FrameworkHandlerOptions {
  agentName?: string;
  agentVersion?: string;
  providerResolver?: 'auto' | ProviderResolverFn;
  provider?: string;
  captureInputs?: boolean;
  captureOutputs?: boolean;
  extraTags?: Record<string, string>;
  extraMetadata?: Record<string, unknown>;
}

interface RunState {
  recorder: GenerationRecorder;
  input: Message[];
  captureOutputs: boolean;
  requestModel: string;
  requestProvider: string;
  outputChunks: string[];
  firstTokenRecorded: boolean;
}

interface ToolRunState {
  recorder: ToolExecutionRecorder;
  arguments?: unknown;
  captureOutputs: boolean;
}

interface FrameworkContext {
  conversationId: string;
  threadId: string;
  metadata: Record<string, unknown>;
  tags: Record<string, string>;
  componentName: string;
  parentRunId: string;
  runType: string;
  retryAttempt: number | undefined;
  langgraphNode: string;
  eventId: string;
}

export class SigilFrameworkHandler {
  private readonly runs = new Map<string, RunState>();
  private readonly toolRuns = new Map<string, ToolRunState>();
  private readonly chainSpans = new Map<string, Span>();
  private readonly retrieverSpans = new Map<string, Span>();
  private readonly agentName?: string;
  private readonly agentVersion?: string;
  private readonly providerResolver: 'auto' | ProviderResolverFn;
  private readonly provider?: string;
  private readonly captureInputs: boolean;
  private readonly captureOutputs: boolean;
  private readonly extraTags: Record<string, string>;
  private readonly extraMetadata: Record<string, unknown>;

  constructor(
    private readonly client: SigilClient,
    private readonly frameworkName: FrameworkName,
    private readonly frameworkLanguage: 'javascript',
    options: FrameworkHandlerOptions = {},
  ) {
    this.agentName = options.agentName;
    this.agentVersion = options.agentVersion;
    this.providerResolver = options.providerResolver ?? 'auto';
    this.provider = options.provider;
    this.captureInputs = options.captureInputs ?? true;
    this.captureOutputs = options.captureOutputs ?? true;
    this.extraTags = { ...(options.extraTags ?? {}) };
    this.extraMetadata = { ...(options.extraMetadata ?? {}) };
  }

  protected onLLMStart(
    serialized: unknown,
    prompts: unknown,
    runId: string,
    parentRunId?: string,
    extraParams?: AnyRecord,
    callbackTags?: string[],
    callbackMetadata?: AnyRecord,
    runName?: string,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0 || this.runs.has(runKey)) {
      return;
    }

    const invocationParams = asRecord(extraParams?.invocation_params);
    const modelName = resolveModelName(serialized, invocationParams);
    const provider = resolveProvider(this.provider, this.providerResolver, modelName, serialized, invocationParams);
    const stream = isStreaming(invocationParams);
    const input = this.captureInputs ? mapPromptInputs(prompts) : [];
    const context = this.buildFrameworkContext({
      runId: runKey,
      parentRunId,
      runType: 'llm',
      runName,
      serialized,
      invocationParams,
      extraParams,
      callbackTags,
      callbackMetadata,
    });

    const payload = this.startPayload(runKey, provider, modelName, context);
    const recorder = stream ? this.client.startStreamingGeneration(payload) : this.client.startGeneration(payload);

    this.runs.set(runKey, {
      recorder,
      input,
      captureOutputs: this.captureOutputs,
      requestModel: modelName,
      requestProvider: provider,
      outputChunks: [],
      firstTokenRecorded: false,
    });
  }

  protected onChatModelStart(
    serialized: unknown,
    messages: unknown,
    runId: string,
    parentRunId?: string,
    extraParams?: AnyRecord,
    callbackTags?: string[],
    callbackMetadata?: AnyRecord,
    runName?: string,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0 || this.runs.has(runKey)) {
      return;
    }

    const invocationParams = asRecord(extraParams?.invocation_params);
    const modelName = resolveModelName(serialized, invocationParams);
    const provider = resolveProvider(this.provider, this.providerResolver, modelName, serialized, invocationParams);
    const stream = isStreaming(invocationParams);
    const input = this.captureInputs ? mapChatInputs(messages) : [];
    const context = this.buildFrameworkContext({
      runId: runKey,
      parentRunId,
      runType: 'chat',
      runName,
      serialized,
      invocationParams,
      extraParams,
      callbackTags,
      callbackMetadata,
    });

    const payload = this.startPayload(runKey, provider, modelName, context);
    const recorder = stream ? this.client.startStreamingGeneration(payload) : this.client.startGeneration(payload);

    this.runs.set(runKey, {
      recorder,
      input,
      captureOutputs: this.captureOutputs,
      requestModel: modelName,
      requestProvider: provider,
      outputChunks: [],
      firstTokenRecorded: false,
    });
  }

  protected onLLMNewToken(token: string, runId: string): void {
    const runState = this.runs.get(String(runId));
    if (runState === undefined) {
      return;
    }

    if (typeof token !== 'string' || token.trim().length === 0) {
      return;
    }

    if (runState.captureOutputs) {
      runState.outputChunks.push(token);
    }

    if (!runState.firstTokenRecorded) {
      runState.firstTokenRecorded = true;
      runState.recorder.setFirstTokenAt(new Date());
    }
  }

  protected onLLMEnd(output: unknown, runId: string): void {
    const runState = this.runs.get(String(runId));
    if (runState === undefined) {
      return;
    }
    this.runs.delete(String(runId));

    try {
      const llmOutput = asRecord(read(output, 'llm_output'));
      // Streaming (and some integrations, e.g. ChatBedrock) don't populate
      // `llm_output.model_name`/`finish_reason`; the model and stop reason land on the
      // first generation message's `response_metadata` instead. Fall back to it so the
      // backfill below fires for the streaming path, mirroring the Python handler.
      const responseMessage = firstGenerationMessage(output);
      const responseMetadata = asRecord(read(responseMessage, 'response_metadata'));
      const responseModel = asString(read(llmOutput, 'model_name')) || asString(read(responseMetadata, 'model_name'));
      const stopReason =
        asString(read(llmOutput, 'finish_reason')) ||
        asString(read(llmOutput, 'stop_reason')) ||
        asString(read(responseMetadata, 'finish_reason')) ||
        asString(read(responseMetadata, 'stop_reason'));
      // Streaming reports token counts on the message's `usage_metadata`, not on
      // `llm_output.token_usage`. Cost needs tokens, so fall back to the first source
      // that actually yields counts, otherwise a resolved model still yields $0 for
      // streams. `mapUsage` returns undefined for missing/all-zero input, so this also
      // steps past a present-but-empty `token_usage` (which JS treats as truthy),
      // matching the Python `or` chain that falls through an empty dict.
      const usage =
        mapUsage(read(llmOutput, 'token_usage')) ??
        mapUsage(read(llmOutput, 'usage')) ??
        mapUsage(read(responseMessage, 'usage_metadata'));

      let mappedOutput: Message[] | undefined;
      if (runState.captureOutputs) {
        mappedOutput = mapOutputMessages(output);
        if ((mappedOutput?.length ?? 0) === 0 && runState.outputChunks.length > 0) {
          mappedOutput = [{ role: 'assistant', content: runState.outputChunks.join('') }];
        }
      }

      // Some frameworks (e.g. LangChain/LangGraph with Bedrock inference profiles)
      // do not surface the model at request start, so it was recorded as
      // "unknown"/"custom". The real model only arrives on the response. Backfill it
      // here so the generation and, critically, the token-usage metric (which drives
      // cost) carry the resolvable model instead of "unknown". Only override when the
      // request model was not already known, so an explicit request model is never
      // clobbered.
      let backfillModel: ModelRef | undefined;
      if (isUnknownModel(runState.requestModel) && responseModel.length > 0) {
        const provider = isUnknownProvider(runState.requestProvider)
          ? inferProviderFromModelName(responseModel)
          : runState.requestProvider;
        backfillModel = { provider, name: responseModel };
      }

      const result: GenerationResult = {
        input: runState.input,
        output: mappedOutput,
        usage,
        model: backfillModel,
        responseModel: responseModel.length > 0 ? responseModel : undefined,
        stopReason: stopReason.length > 0 ? stopReason : undefined,
      };
      runState.recorder.setResult(result);
    } finally {
      runState.recorder.end();
    }

    const recorderError = runState.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  protected onLLMError(error: unknown, runId: string): void {
    const runState = this.runs.get(String(runId));
    if (runState === undefined) {
      return;
    }
    this.runs.delete(String(runId));

    try {
      runState.recorder.setCallError(error);
      if (runState.captureOutputs && runState.outputChunks.length > 0) {
        runState.recorder.setResult({
          input: runState.input,
          output: [{ role: 'assistant', content: runState.outputChunks.join('') }],
        });
      }
    } finally {
      runState.recorder.end();
    }

    const recorderError = runState.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  protected onToolStart(
    serialized: unknown,
    input: unknown,
    runId: string,
    parentRunId?: string,
    callbackTags?: string[],
    callbackMetadata?: AnyRecord,
    runName?: string,
    extraParams?: AnyRecord,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0 || this.toolRuns.has(runKey)) {
      return;
    }

    const invocationParams = asRecord(extraParams?.invocation_params);
    const context = this.buildFrameworkContext({
      runId: runKey,
      parentRunId,
      runType: 'tool',
      runName,
      serialized,
      invocationParams,
      extraParams,
      callbackTags,
      callbackMetadata,
    });
    const toolName = resolveToolName(serialized, context.componentName);
    const recorder = this.client.startToolExecution({
      toolName,
      toolDescription: resolveToolDescription(serialized),
      conversationId: context.conversationId,
      agentName: this.agentName,
      agentVersion: this.agentVersion,
      includeContent: this.captureInputs || this.captureOutputs,
    });

    this.toolRuns.set(runKey, {
      recorder,
      arguments: this.captureInputs ? resolveToolArguments(input, extraParams) : undefined,
      captureOutputs: this.captureOutputs,
    });
  }

  protected onToolEnd(output: unknown, runId: string): void {
    const runState = this.toolRuns.get(String(runId));
    if (runState === undefined) {
      return;
    }
    this.toolRuns.delete(String(runId));

    try {
      const result: Record<string, unknown> = {};
      if (runState.arguments !== undefined) {
        result.arguments = runState.arguments;
      }
      if (runState.captureOutputs) {
        result.result = output;
      }
      runState.recorder.setResult(result);
    } finally {
      runState.recorder.end();
    }

    const recorderError = runState.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  protected onToolError(error: unknown, runId: string): void {
    const runState = this.toolRuns.get(String(runId));
    if (runState === undefined) {
      return;
    }
    this.toolRuns.delete(String(runId));

    try {
      runState.recorder.setCallError(error);
    } finally {
      runState.recorder.end();
    }

    const recorderError = runState.recorder.getError();
    if (recorderError !== undefined) {
      throw recorderError;
    }
  }

  protected onChainStart(
    serialized: unknown,
    runId: string,
    parentRunId?: string,
    callbackTags?: string[],
    callbackMetadata?: AnyRecord,
    callbackRunType?: string,
    runName?: string,
    extraParams?: AnyRecord,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0 || this.chainSpans.has(runKey)) {
      return;
    }

    const invocationParams = asRecord(extraParams?.invocation_params);
    const context = this.buildFrameworkContext({
      runId: runKey,
      parentRunId,
      runType: notEmpty(callbackRunType) ? String(callbackRunType).trim() : 'chain',
      runName,
      serialized,
      invocationParams,
      extraParams,
      callbackTags,
      callbackMetadata,
    });
    const spanName = notEmpty(context.componentName)
      ? `${this.frameworkName}.chain ${context.componentName}`
      : `${this.frameworkName}.chain`;
    const span = this.getFrameworkTracer().startSpan(spanName, { kind: SpanKind.INTERNAL });
    this.setFrameworkSpanAttributes(span, context, 'framework_chain');
    this.chainSpans.set(runKey, span);
  }

  protected onChainEnd(runId: string): void {
    this.endFrameworkSpan(this.chainSpans, runId, undefined);
  }

  protected onChainError(error: unknown, runId: string): void {
    this.endFrameworkSpan(this.chainSpans, runId, error);
  }

  protected onRetrieverStart(
    serialized: unknown,
    runId: string,
    parentRunId?: string,
    callbackTags?: string[],
    callbackMetadata?: AnyRecord,
    runName?: string,
    extraParams?: AnyRecord,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0 || this.retrieverSpans.has(runKey)) {
      return;
    }

    const invocationParams = asRecord(extraParams?.invocation_params);
    const context = this.buildFrameworkContext({
      runId: runKey,
      parentRunId,
      runType: 'retriever',
      runName,
      serialized,
      invocationParams,
      extraParams,
      callbackTags,
      callbackMetadata,
    });
    const spanName = notEmpty(context.componentName)
      ? `${this.frameworkName}.retriever ${context.componentName}`
      : `${this.frameworkName}.retriever`;
    const span = this.getFrameworkTracer().startSpan(spanName, { kind: SpanKind.INTERNAL });
    this.setFrameworkSpanAttributes(span, context, 'framework_retriever');
    this.retrieverSpans.set(runKey, span);
  }

  protected onRetrieverEnd(runId: string): void {
    this.endFrameworkSpan(this.retrieverSpans, runId, undefined);
  }

  protected onRetrieverError(error: unknown, runId: string): void {
    this.endFrameworkSpan(this.retrieverSpans, runId, error);
  }

  private startPayload(_runId: string, provider: string, modelName: string, context: FrameworkContext) {
    return {
      conversationId: context.conversationId,
      agentName: this.agentName,
      agentVersion: this.agentVersion,
      model: {
        provider,
        name: modelName,
      },
      tags: context.tags,
      metadata: context.metadata,
    };
  }

  private getFrameworkTracer(): Tracer {
    const internalClient = this.client as unknown as { tracer?: Tracer };
    return internalClient.tracer ?? trace.getTracer(frameworkInstrumentationName);
  }

  private setFrameworkSpanAttributes(span: Span, context: FrameworkContext, operationName: string): void {
    span.setAttribute(spanAttrOperationName, operationName);
    span.setAttribute(spanAttrFrameworkName, this.frameworkName);
    span.setAttribute(spanAttrFrameworkSource, frameworkSource);
    span.setAttribute(spanAttrFrameworkLanguage, this.frameworkLanguage);
    span.setAttribute(spanAttrFrameworkRunID, asString(context.metadata[spanAttrFrameworkRunID]));
    if (notEmpty(context.conversationId)) {
      span.setAttribute(spanAttrConversationID, context.conversationId);
    }
    if (notEmpty(context.threadId)) {
      span.setAttribute(spanAttrFrameworkThreadID, context.threadId);
    }
    if (notEmpty(context.parentRunId)) {
      span.setAttribute(spanAttrFrameworkParentRunID, context.parentRunId);
    }
    if (notEmpty(context.componentName)) {
      span.setAttribute(spanAttrFrameworkComponentName, context.componentName);
    }
    if (notEmpty(context.runType)) {
      span.setAttribute(spanAttrFrameworkRunType, context.runType);
    }
    if (context.retryAttempt !== undefined) {
      span.setAttribute(spanAttrFrameworkRetryAttempt, context.retryAttempt);
    }
    if (notEmpty(context.langgraphNode)) {
      span.setAttribute(spanAttrFrameworkLangGraphNode, context.langgraphNode);
    }
    if (notEmpty(context.eventId)) {
      span.setAttribute(spanAttrFrameworkEventID, context.eventId);
    }
  }

  private endFrameworkSpan(spans: Map<string, Span>, runId: string, error: unknown): void {
    const runKey = String(runId);
    const span = spans.get(runKey);
    if (span === undefined) {
      return;
    }
    spans.delete(runKey);
    if (error === undefined) {
      span.setStatus({ code: SpanStatusCode.OK });
      span.end();
      return;
    }

    span.setAttribute(spanAttrErrorType, 'framework_error');
    span.setAttribute(spanAttrErrorCategory, 'sdk_error');
    span.recordException(asError(error));
    span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
    span.end();
  }

  private buildFrameworkContext(params: {
    runId: string;
    parentRunId?: string;
    runType: string;
    runName?: string;
    serialized: unknown;
    invocationParams?: AnyRecord;
    extraParams?: AnyRecord;
    callbackTags?: string[];
    callbackMetadata?: AnyRecord;
  }): FrameworkContext {
    const conversation = resolveFrameworkConversationContext(
      this.frameworkName,
      params.runId,
      params.serialized,
      params.invocationParams,
      params.extraParams,
      params.callbackMetadata,
    );
    const componentName = resolveComponentName(
      params.serialized,
      params.callbackMetadata,
      params.extraParams,
      params.runName,
    );
    const retryAttempt = resolveFrameworkRetryAttempt(
      params.callbackMetadata,
      params.extraParams,
      params.invocationParams,
      params.serialized,
    );
    const parentRunId = normalizeRunID(params.parentRunId);
    const runType = params.runType.trim();
    const frameworkTags = normalizeFrameworkTags(
      params.callbackTags ?? read(params.extraParams, 'tags') ?? read(params.callbackMetadata, 'tags'),
    );
    const langgraphNode =
      this.frameworkName === 'langgraph'
        ? resolveLangGraphNode(params.callbackMetadata, params.extraParams, params.invocationParams, params.serialized)
        : '';
    const eventId = resolveFrameworkEventID(
      params.callbackMetadata,
      params.extraParams,
      params.invocationParams,
      params.serialized,
    );

    const rawMetadata: Record<string, unknown> = {
      ...this.extraMetadata,
      [spanAttrFrameworkRunID]: params.runId,
      [spanAttrFrameworkRunType]: runType,
    };
    if (conversation.threadId.length > 0) {
      rawMetadata[spanAttrFrameworkThreadID] = conversation.threadId;
    }
    if (parentRunId.length > 0) {
      rawMetadata[spanAttrFrameworkParentRunID] = parentRunId;
    }
    if (componentName.length > 0) {
      rawMetadata[spanAttrFrameworkComponentName] = componentName;
    }
    if (frameworkTags.length > 0) {
      rawMetadata[spanAttrFrameworkTags] = frameworkTags;
    }
    if (retryAttempt !== undefined) {
      rawMetadata[spanAttrFrameworkRetryAttempt] = retryAttempt;
    }
    if (langgraphNode.length > 0) {
      rawMetadata[spanAttrFrameworkLangGraphNode] = langgraphNode;
    }
    if (eventId.length > 0) {
      rawMetadata[spanAttrFrameworkEventID] = eventId;
    }
    const metadata = normalizeFrameworkMetadata(rawMetadata);

    const tags: Record<string, string> = {
      ...this.extraTags,
      'sigil.framework.name': this.frameworkName,
      'sigil.framework.source': frameworkSource,
      'sigil.framework.language': this.frameworkLanguage,
    };

    return {
      conversationId: conversation.conversationId,
      threadId: conversation.threadId,
      metadata,
      tags,
      componentName,
      parentRunId,
      runType,
      retryAttempt,
      langgraphNode,
      eventId,
    };
  }
}

function resolveProvider(
  explicitProvider: string | undefined,
  resolver: 'auto' | ProviderResolverFn,
  modelName: string,
  serialized: unknown,
  invocationParams: unknown,
): string {
  const explicit = normalizeProvider(explicitProvider);
  if (explicit.length > 0) {
    return explicit;
  }

  if (typeof resolver === 'function') {
    const resolved = normalizeProvider(
      resolver({
        modelName,
        serialized,
        invocationParams,
      }),
    );
    return resolved.length > 0 ? resolved : 'custom';
  }

  // A framework hint (`provider`/`ls_provider`) wins only when it is a canonical
  // provider. LangChain often reports a non-canonical hint for Bedrock (e.g.
  // `ls_provider: 'amazon_bedrock'`), which `normalizeProvider` collapses to `custom`;
  // treating that as authoritative would short-circuit the model-name inference that
  // can recover the real vendor from a Bedrock id. So a `custom` hint is only kept as a
  // last resort — model-name inference is tried first.
  let hintProvider = '';
  for (const payload of [asRecord(invocationParams), asRecord(serialized)]) {
    for (const key of ['provider', 'ls_provider']) {
      const normalized = normalizeProvider(asString(read(payload, key)));
      if (normalized.length === 0) {
        continue;
      }
      if (normalized !== 'custom') {
        return normalized;
      }
      hintProvider = normalized;
    }
  }

  const inferred = inferProviderFromModelName(modelName);
  if (inferred !== 'custom') {
    return inferred;
  }
  return hintProvider.length > 0 ? hintProvider : inferred;
}

function resolveModelName(serialized: unknown, invocationParams: unknown): string {
  for (const payload of [asRecord(invocationParams), asRecord(serialized)]) {
    for (const key of ['model', 'model_name', 'ls_model_name']) {
      const value = asString(read(payload, key));
      if (value.length > 0) {
        return value;
      }
    }

    const kwargs = asRecord(read(payload, 'kwargs'));
    for (const key of ['model', 'model_name']) {
      const value = asString(read(kwargs, key));
      if (value.length > 0) {
        return value;
      }
    }
  }

  return 'unknown';
}

function isStreaming(invocationParams: AnyRecord | undefined): boolean {
  if (invocationParams === undefined) {
    return false;
  }
  return asBoolean(read(invocationParams, 'stream')) || asBoolean(read(invocationParams, 'streaming'));
}

function resolveFrameworkThreadId(
  serialized: unknown,
  invocationParams: AnyRecord | undefined,
  extraParams: AnyRecord | undefined,
  callbackMetadata: AnyRecord | undefined,
): string {
  for (const payload of [callbackMetadata, extraParams, invocationParams, serialized]) {
    const threadId = threadIdFromPayload(payload);
    if (threadId.length > 0) {
      return threadId;
    }
  }
  return '';
}

function resolveFrameworkConversationContext(
  frameworkName: FrameworkName,
  runId: string,
  serialized: unknown,
  invocationParams: AnyRecord | undefined,
  extraParams: AnyRecord | undefined,
  callbackMetadata: AnyRecord | undefined,
): { conversationId: string; threadId: string } {
  for (const payload of [callbackMetadata, extraParams, invocationParams, serialized]) {
    const conversationId = conversationIdFromPayload(payload);
    if (conversationId.length > 0) {
      const threadId = threadIdFromPayload(payload);
      if (threadId.length > 0) {
        return { conversationId, threadId };
      }
      return {
        conversationId,
        threadId: resolveFrameworkThreadId(serialized, invocationParams, extraParams, callbackMetadata),
      };
    }
  }

  const threadId = resolveFrameworkThreadId(serialized, invocationParams, extraParams, callbackMetadata);
  if (threadId.length > 0) {
    return { conversationId: threadId, threadId };
  }

  // Deterministic fallback when frameworks do not expose session/conversation identity.
  return { conversationId: `sigil:framework:${frameworkName}:${runId}`, threadId: '' };
}

function threadIdFromPayload(payload: unknown): string {
  const candidates = [
    asString(read(payload, 'thread_id')),
    asString(read(payload, 'threadId')),
    asString(read(read(payload, 'metadata'), 'thread_id')),
    asString(read(read(payload, 'metadata'), 'threadId')),
    asString(read(read(payload, 'configurable'), 'thread_id')),
    asString(read(read(payload, 'configurable'), 'threadId')),
    asString(read(read(payload, 'config'), 'thread_id')),
    asString(read(read(payload, 'config'), 'threadId')),
    asString(read(read(read(payload, 'config'), 'metadata'), 'thread_id')),
    asString(read(read(read(payload, 'config'), 'metadata'), 'threadId')),
    asString(read(read(read(payload, 'config'), 'configurable'), 'thread_id')),
    asString(read(read(read(payload, 'config'), 'configurable'), 'threadId')),
  ];
  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return '';
}

function conversationIdFromPayload(payload: unknown): string {
  const candidates = [
    asString(read(payload, 'conversation_id')),
    asString(read(payload, 'conversationId')),
    asString(read(payload, 'session_id')),
    asString(read(payload, 'sessionId')),
    asString(read(payload, 'group_id')),
    asString(read(payload, 'groupId')),
    asString(read(read(payload, 'metadata'), 'conversation_id')),
    asString(read(read(payload, 'metadata'), 'conversationId')),
    asString(read(read(payload, 'metadata'), 'session_id')),
    asString(read(read(payload, 'metadata'), 'sessionId')),
    asString(read(read(payload, 'metadata'), 'group_id')),
    asString(read(read(payload, 'metadata'), 'groupId')),
    asString(read(read(payload, 'configurable'), 'conversation_id')),
    asString(read(read(payload, 'configurable'), 'conversationId')),
    asString(read(read(payload, 'configurable'), 'session_id')),
    asString(read(read(payload, 'configurable'), 'sessionId')),
    asString(read(read(payload, 'configurable'), 'group_id')),
    asString(read(read(payload, 'configurable'), 'groupId')),
    asString(read(read(payload, 'config'), 'conversation_id')),
    asString(read(read(payload, 'config'), 'conversationId')),
    asString(read(read(payload, 'config'), 'session_id')),
    asString(read(read(payload, 'config'), 'sessionId')),
    asString(read(read(payload, 'config'), 'group_id')),
    asString(read(read(payload, 'config'), 'groupId')),
  ];
  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return '';
}

function resolveComponentName(
  serialized: unknown,
  callbackMetadata: AnyRecord | undefined,
  extraParams: AnyRecord | undefined,
  runName: string | undefined,
): string {
  const candidates = [
    asString(read(serialized, 'name')),
    idPath(read(serialized, 'id')),
    idPath(read(serialized, 'lc_id')),
    asString(read(read(serialized, 'kwargs'), 'name')),
    asString(read(callbackMetadata, 'component_name')),
    asString(read(extraParams, 'component_name')),
    asString(runName),
  ];
  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return '';
}

function resolveLangGraphNode(
  callbackMetadata: AnyRecord | undefined,
  extraParams: AnyRecord | undefined,
  invocationParams: AnyRecord | undefined,
  serialized: unknown,
): string {
  for (const payload of [callbackMetadata, extraParams, invocationParams, asRecord(serialized)]) {
    const candidate = langGraphNodeFromPayload(payload);
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return '';
}

function langGraphNodeFromPayload(payload: unknown): string {
  const candidates = [
    asString(read(payload, 'langgraph_node')),
    asString(read(payload, 'langgraph_node_name')),
    asString(read(payload, 'node_name')),
    asString(read(payload, 'node')),
    asString(read(read(payload, 'metadata'), 'langgraph_node')),
    asString(read(read(payload, 'metadata'), 'langgraph_node_name')),
    asString(read(read(payload, 'configurable'), 'langgraph_node')),
    asString(read(read(payload, 'configurable'), 'langgraph_node_name')),
    asString(read(read(read(payload, 'config'), 'metadata'), 'langgraph_node')),
    asString(read(read(read(payload, 'config'), 'configurable'), 'langgraph_node')),
    asString(read(read(read(payload, 'config'), 'configurable'), '__pregel_node')),
  ];
  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return '';
}

function resolveFrameworkRetryAttempt(...payloads: unknown[]): number | undefined {
  for (const payload of payloads) {
    const value = retryAttemptFromPayload(payload);
    if (value !== undefined) {
      return value;
    }
  }
  return undefined;
}

function resolveFrameworkEventID(...payloads: unknown[]): string {
  for (const payload of payloads) {
    const candidates = [
      asString(read(payload, 'event_id')),
      asString(read(payload, 'eventId')),
      asString(read(payload, 'invocation_id')),
      asString(read(payload, 'invocationId')),
      asString(read(read(payload, 'metadata'), 'event_id')),
      asString(read(read(payload, 'metadata'), 'eventId')),
      asString(read(read(payload, 'metadata'), 'invocation_id')),
      asString(read(read(payload, 'metadata'), 'invocationId')),
    ];
    for (const candidate of candidates) {
      if (candidate.length > 0) {
        return candidate;
      }
    }
  }
  return '';
}

function retryAttemptFromPayload(payload: unknown): number | undefined {
  const candidates = [
    read(payload, 'retry_attempt'),
    read(payload, 'retryAttempt'),
    read(payload, 'attempt'),
    read(payload, 'retry'),
    read(read(payload, 'metadata'), 'retry_attempt'),
    read(read(payload, 'metadata'), 'retryAttempt'),
    read(read(payload, 'configurable'), 'retry_attempt'),
    read(read(payload, 'configurable'), 'retryAttempt'),
  ];
  for (const candidate of candidates) {
    const parsed = asMaybeInt(candidate);
    if (parsed !== undefined) {
      return parsed;
    }
  }
  return undefined;
}

function normalizeFrameworkTags(raw: unknown): string[] {
  const values = Array.isArray(raw) ? raw : [raw];
  const seen = new Set<string>();
  const output: string[] = [];
  for (const value of values) {
    if (typeof value !== 'string') {
      continue;
    }
    const trimmed = value.trim();
    if (trimmed.length === 0 || seen.has(trimmed)) {
      continue;
    }
    seen.add(trimmed);
    output.push(trimmed);
  }
  return output;
}

function normalizeFrameworkMetadata(raw: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const seen = new WeakSet<object>();
  for (const [key, value] of Object.entries(raw)) {
    const normalizedKey = key.trim();
    if (normalizedKey.length === 0) {
      continue;
    }
    const normalizedValue = normalizeFrameworkMetadataValue(value, 0, seen);
    if (normalizedValue !== undefined) {
      out[normalizedKey] = normalizedValue;
    }
  }
  return out;
}

function normalizeFrameworkMetadataValue(value: unknown, depth: number, seen: WeakSet<object>): unknown {
  if (depth > maxFrameworkMetadataDepth || value === undefined) {
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
    const normalized = value
      .map((item) => normalizeFrameworkMetadataValue(item, depth + 1, seen))
      .filter((item) => item !== undefined);
    return normalized;
  }
  if (!isRecord(value)) {
    return undefined;
  }
  if (seen.has(value)) {
    return '[circular]';
  }
  seen.add(value);
  try {
    const normalizedObject: Record<string, unknown> = {};
    for (const [key, nestedValue] of Object.entries(value)) {
      const nestedKey = key.trim();
      if (nestedKey.length === 0) {
        continue;
      }
      const normalizedNestedValue = normalizeFrameworkMetadataValue(nestedValue, depth + 1, seen);
      if (normalizedNestedValue !== undefined) {
        normalizedObject[nestedKey] = normalizedNestedValue;
      }
    }
    return normalizedObject;
  } finally {
    seen.delete(value);
  }
}

function normalizeRunID(runId: string | undefined): string {
  if (typeof runId !== 'string') {
    return '';
  }
  return runId.trim();
}

function resolveToolName(serialized: unknown, componentName: string): string {
  const candidates = [
    asString(read(serialized, 'name')),
    asString(read(serialized, 'tool_name')),
    componentName,
    'framework_tool',
  ];
  for (const candidate of candidates) {
    if (candidate.length > 0) {
      return candidate;
    }
  }
  return 'framework_tool';
}

function resolveToolDescription(serialized: unknown): string | undefined {
  const description = asString(read(serialized, 'description'));
  if (description.length > 0) {
    return description;
  }
  return undefined;
}

function resolveToolArguments(input: unknown, extraParams: AnyRecord | undefined): unknown {
  const explicitInputs = read(extraParams, 'inputs');
  if (explicitInputs !== undefined) {
    return explicitInputs;
  }
  if (typeof input === 'string') {
    return input.trim();
  }
  return input;
}

function idPath(value: unknown): string {
  if (!Array.isArray(value)) {
    return '';
  }
  const parts = value
    .filter((item): item is string => typeof item === 'string')
    .map((item) => item.trim())
    .filter((item) => item.length > 0);
  return parts.join('.');
}

function mapPromptInputs(prompts: unknown): Message[] {
  if (!Array.isArray(prompts)) {
    return [];
  }

  const input: Message[] = [];
  for (const prompt of prompts) {
    if (typeof prompt !== 'string') {
      continue;
    }
    const trimmed = prompt.trim();
    if (trimmed.length === 0) {
      continue;
    }
    input.push({ role: 'user', content: trimmed });
  }
  return input;
}

function mapChatInputs(messages: unknown): Message[] {
  if (!Array.isArray(messages)) {
    return [];
  }

  const output: Message[] = [];
  for (const batch of messages) {
    if (!Array.isArray(batch)) {
      continue;
    }

    for (const message of batch) {
      const text = extractMessageText(message);
      if (text.length === 0) {
        continue;
      }

      output.push({
        role: normalizeRole(extractMessageRole(message)),
        content: text,
      });
    }
  }

  return output;
}

function mapOutputMessages(output: unknown): Message[] {
  const generations = read(output, 'generations');
  if (!Array.isArray(generations)) {
    return [];
  }

  const texts: string[] = [];
  for (const candidates of generations) {
    if (!Array.isArray(candidates)) {
      continue;
    }
    for (const candidate of candidates) {
      const text = extractGenerationText(candidate);
      if (text.length > 0) {
        texts.push(text);
      }
    }
  }

  if (texts.length === 0) {
    return [];
  }

  return [{ role: 'assistant', content: texts.join('\n') }];
}

// Returns the first generation's `message` object, mirroring the Python
// `_first_generation_message`. Streaming responses carry the model / stop reason on
// this message's `response_metadata` rather than on `llm_output`.
function firstGenerationMessage(output: unknown): unknown {
  const generations = read(output, 'generations');
  if (!Array.isArray(generations)) {
    return undefined;
  }
  for (const candidates of generations) {
    if (!Array.isArray(candidates)) {
      continue;
    }
    for (const candidate of candidates) {
      const message = read(candidate, 'message');
      if (message !== undefined && message !== null) {
        return message;
      }
    }
  }
  return undefined;
}

function extractGenerationText(candidate: unknown): string {
  const text = asString(read(candidate, 'text'));
  if (text.length > 0) {
    return text;
  }

  return extractMessageText(read(candidate, 'message'));
}

function extractMessageText(message: unknown): string {
  const content = read(message, 'content');

  if (typeof content === 'string') {
    return content.trim();
  }

  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const item of content) {
      if (typeof item === 'string') {
        const trimmed = item.trim();
        if (trimmed.length > 0) {
          parts.push(trimmed);
        }
        continue;
      }

      const text = asString(read(item, 'text'));
      if (text.length > 0) {
        parts.push(text);
      }
    }
    return parts.join(' ').trim();
  }

  if (isRecord(content)) {
    return asString(read(content, 'text'));
  }

  return '';
}

function extractMessageRole(message: unknown): string {
  const role = asString(read(message, 'role'));
  if (role.length > 0) {
    return role;
  }
  return asString(read(message, 'type'));
}

function normalizeRole(role: string): Message['role'] {
  const normalized = role.trim().toLowerCase();
  if (normalized === 'assistant' || normalized === 'ai') {
    return 'assistant';
  }
  if (normalized === 'tool') {
    return 'tool';
  }
  return 'user';
}

function mapUsage(rawUsage: unknown): TokenUsage | undefined {
  const usage = asRecord(rawUsage);
  if (usage === undefined) {
    return undefined;
  }

  const inputTokens = asInt(read(usage, 'prompt_tokens')) || asInt(read(usage, 'input_tokens'));
  const outputTokens = asInt(read(usage, 'completion_tokens')) || asInt(read(usage, 'output_tokens'));
  const totalTokens = asInt(read(usage, 'total_tokens')) || inputTokens + outputTokens;

  if (inputTokens === 0 && outputTokens === 0 && totalTokens === 0) {
    return undefined;
  }

  return {
    inputTokens,
    outputTokens,
    totalTokens,
  };
}

function isUnknownModel(modelName: string): boolean {
  const normalized = modelName.trim().toLowerCase();
  return normalized === '' || normalized === 'unknown';
}

function isUnknownProvider(provider: string): boolean {
  const normalized = provider.trim().toLowerCase();
  return normalized === '' || normalized === 'custom';
}

// Vendor->provider map for Bedrock model IDs. Matches the backend
// `providerFromBedrockVendor` (sigil parseBedrockModelID) so SDK and backend agree.
const BEDROCK_VENDOR_PROVIDERS: Record<string, string> = {
  anthropic: 'anthropic',
  amazon: 'amazon',
  cohere: 'cohere',
  meta: 'meta-llama',
  mistral: 'mistralai',
};

// Longest markers first so a partial marker never matches ahead of the full one.
const BEDROCK_RESOURCE_MARKERS = ['application-inference-profile/', 'inference-profile/', 'foundation-model/'];

const BEDROCK_REGIONAL_PREFIXES = new Set(['us', 'eu', 'apac', 'jp', 'global']);

// Mirror the backend's positional parse (sigil parseBedrockModelID): strip the
// resource-ARN prefix, then read the vendor from a FIXED position — segment 0, or
// segment 1 when segment 0 is a known regional prefix (us/eu/apac/jp/global).
// Scanning every dotted segment would misclassify custom names that merely contain a
// vendor word (e.g. "my-team.anthropic.foo") and diverge from the backend, so we
// match its positional logic exactly.
function inferProviderFromBedrockId(modelName: string): string {
  let trimmed = modelName.trim().toLowerCase();
  for (const marker of BEDROCK_RESOURCE_MARKERS) {
    const idx = trimmed.indexOf(marker);
    if (idx !== -1) {
      trimmed = trimmed.slice(idx + marker.length).trim();
      break;
    }
  }
  const parts = trimmed.split('.');
  if (parts.length < 2) {
    return '';
  }
  let vendorIdx = 0;
  if (parts.length >= 3 && BEDROCK_REGIONAL_PREFIXES.has(parts[0] ?? '')) {
    vendorIdx = 1;
  }
  const vendor = parts[vendorIdx] ?? '';
  return BEDROCK_VENDOR_PROVIDERS[vendor] ?? '';
}

// Called from two places: the request-start provider fallback in `resolveProvider`
// (so a Bedrock-style model surfaced at start resolves to its vendor instead of
// "custom") and the `onLLMEnd` model backfill. Both rely on the Bedrock branch below,
// so keep it consistent with the backend.
function inferProviderFromModelName(modelName: string): string {
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
  // Bedrock inference-profile IDs / ARNs carry the vendor in a fixed dotted position
  // (e.g. "global.anthropic.claude-sonnet-4-6" or an ".../inference-profile/..." ARN),
  // so the plain prefix checks above miss them.
  const bedrockProvider = inferProviderFromBedrockId(normalized);
  if (bedrockProvider !== '') {
    return bedrockProvider;
  }
  return 'custom';
}

function normalizeProvider(value: string | undefined): string {
  const normalized = (value ?? '').trim().toLowerCase();
  if (normalized === 'openai' || normalized === 'anthropic' || normalized === 'gemini') {
    return normalized;
  }
  if (normalized.length === 0) {
    return '';
  }
  return 'custom';
}

function read(value: unknown, key: string): unknown {
  if (isRecord(value)) {
    return value[key];
  }
  return undefined;
}

function asRecord(value: unknown): AnyRecord | undefined {
  return isRecord(value) ? value : undefined;
}

function asString(value: unknown): string {
  if (typeof value !== 'string') {
    return '';
  }
  return value.trim();
}

function asInt(value: unknown): number {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return Math.trunc(value);
  }
  if (typeof value === 'string' && value.trim().length > 0) {
    const parsed = Number.parseInt(value, 10);
    if (Number.isFinite(parsed)) {
      return parsed;
    }
  }
  return 0;
}

function asMaybeInt(value: unknown): number | undefined {
  if (value === undefined || value === null || typeof value === 'boolean') {
    return undefined;
  }
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || !Number.isInteger(value)) {
      return undefined;
    }
    return value;
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return undefined;
    }
    const parsed = Number.parseInt(trimmed, 10);
    if (Number.isNaN(parsed)) {
      return undefined;
    }
    return parsed;
  }
  return undefined;
}

function asBoolean(value: unknown): boolean {
  if (typeof value === 'boolean') {
    return value;
  }
  if (typeof value === 'number') {
    return value !== 0;
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    return normalized === '1' || normalized === 'true' || normalized === 'yes' || normalized === 'on';
  }
  return false;
}

function asError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  if (typeof value === 'string') {
    return new Error(value);
  }
  return new Error('framework callback error');
}

function isRecord(value: unknown): value is AnyRecord {
  return typeof value === 'object' && value !== null;
}

function notEmpty(value: string | undefined): boolean {
  return typeof value === 'string' && value.trim().length > 0;
}
