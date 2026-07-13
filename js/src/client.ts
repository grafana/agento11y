import {
  context,
  type Histogram,
  type Meter,
  metrics,
  type Span,
  SpanKind,
  SpanStatusCode,
  type Tracer,
  trace,
} from '@opentelemetry/api';
import {
  CACHE_DIAGNOSTICS_MISS_REASON_KEY,
  CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
  CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
} from './cache-diagnostics.js';
import { defaultLogger, mergeConfig } from './config.js';
import {
  callContentCaptureResolver,
  resolveClientContentCaptureMode,
  resolveContentCaptureMode,
  shouldIncludeToolContent,
  stampContentCaptureMetadata,
  stripContent,
} from './content_capture.js';
import {
  agentNameFromContext,
  agentVersionFromContext,
  conversationIdFromContext,
  conversationTitleFromContext,
  userIdFromContext,
} from './context.js';
import { createDefaultGenerationExporter } from './exporters/default.js';
import { evaluateHook as evaluateHookImpl } from './hooks.js';
import type {
  ContentCaptureMode,
  ConversationRating,
  ConversationRatingInput,
  ConversationRatingSummary,
  ConversationRatingValue,
  EmbeddingRecorder,
  EmbeddingResult,
  EmbeddingStart,
  ExecuteToolCallsOptions,
  Generation,
  GenerationExporter,
  GenerationMode,
  GenerationRecorder,
  GenerationResult,
  GenerationStart,
  HookEvaluateRequest,
  HookEvaluateResponse,
  HooksConfig,
  Message,
  RecorderCallback,
  RecorderWithError,
  SigilDebugSnapshot,
  SigilLogger,
  SigilSdkConfig,
  SigilSdkConfigInput,
  SubmitConversationRatingResponse,
  ToolExecution,
  ToolExecutionRecorder,
  ToolExecutionResult,
  ToolExecutionStart,
  WorkflowStep,
} from './types.js';
import {
  asError,
  cloneArtifact,
  cloneEmbeddingResult,
  cloneEmbeddingStart,
  cloneGeneration,
  cloneGenerationResult,
  cloneGenerationStart,
  cloneMessage,
  cloneModelRef,
  cloneToolDefinition,
  cloneToolExecution,
  cloneToolExecutionResult,
  cloneToolExecutionStart,
  cloneWorkflowStep,
  defaultOperationNameForMode,
  defaultSleep,
  encodedSizeBytes,
  maybeUnref,
  newLocalID,
  validateEmbeddingResult,
  validateEmbeddingStart,
  validateGeneration,
  validateToolExecution,
  validateWorkflowStep,
} from './utils.js';

const spanAttrGenerationID = 'sigil.generation.id';
const spanAttrSDKName = 'sigil.sdk.name';
const spanAttrFrameworkRunID = 'sigil.framework.run_id';
const spanAttrFrameworkThreadID = 'sigil.framework.thread_id';
const spanAttrFrameworkParentRunID = 'sigil.framework.parent_run_id';
const spanAttrFrameworkComponentName = 'sigil.framework.component_name';
const spanAttrFrameworkRunType = 'sigil.framework.run_type';
const spanAttrFrameworkRetryAttempt = 'sigil.framework.retry_attempt';
const spanAttrFrameworkLangGraphNode = 'sigil.framework.langgraph.node';
const spanAttrFrameworkEventID = 'sigil.framework.event_id';
const spanAttrConversationID = 'gen_ai.conversation.id';
const spanAttrConversationTitle = 'sigil.conversation.title';
const spanAttrUserID = 'user.id';
const spanAttrAgentName = 'gen_ai.agent.name';
const spanAttrAgentVersion = 'gen_ai.agent.version';
const spanAttrErrorType = 'error.type';
const spanAttrErrorCategory = 'error.category';
const spanAttrOperationName = 'gen_ai.operation.name';
const spanAttrProviderName = 'gen_ai.provider.name';
const spanAttrRequestModel = 'gen_ai.request.model';
const spanAttrRequestMaxTokens = 'gen_ai.request.max_tokens';
const spanAttrRequestTemperature = 'gen_ai.request.temperature';
const spanAttrRequestTopP = 'gen_ai.request.top_p';
const spanAttrRequestToolChoice = 'sigil.gen_ai.request.tool_choice';
const spanAttrRequestThinkingEnabled = 'sigil.gen_ai.request.thinking.enabled';
const spanAttrRequestThinkingBudget = 'sigil.gen_ai.request.thinking.budget_tokens';
const spanAttrResponseID = 'gen_ai.response.id';
const spanAttrResponseModel = 'gen_ai.response.model';
const spanAttrFinishReasons = 'gen_ai.response.finish_reasons';
const spanAttrInputTokens = 'gen_ai.usage.input_tokens';
const spanAttrOutputTokens = 'gen_ai.usage.output_tokens';
const spanAttrEmbeddingInputCount = 'gen_ai.embeddings.input_count';
const spanAttrEmbeddingInputTexts = 'gen_ai.embeddings.input_texts';
const spanAttrEmbeddingDimCount = 'gen_ai.embeddings.dimension.count';
const spanAttrRequestEncodingFormats = 'gen_ai.request.encoding_formats';
const spanAttrCacheReadTokens = 'gen_ai.usage.cache_read_input_tokens';
const spanAttrCacheWriteTokens = 'gen_ai.usage.cache_write_input_tokens';
const spanAttrReasoningTokens = 'gen_ai.usage.reasoning_tokens';
const spanAttrToolName = 'gen_ai.tool.name';
const spanAttrToolCallID = 'gen_ai.tool.call.id';
const spanAttrToolType = 'gen_ai.tool.type';
const spanAttrToolDescription = 'gen_ai.tool.description';
const spanAttrToolCallArguments = 'gen_ai.tool.call.arguments';
const spanAttrToolCallResult = 'gen_ai.tool.call.result';
const spanAttrTagPrefix = 'sigil.tag.';
const maxRatingConversationIdLen = 255;
const maxRatingIdLen = 128;
const maxRatingGenerationIdLen = 255;
const maxRatingActorIdLen = 255;
const maxRatingSourceLen = 64;
const maxRatingCommentBytes = 4096;
const maxRatingMetadataBytes = 16 * 1024;

const metricOperationDuration = 'gen_ai.client.operation.duration';
const metricTokenUsage = 'gen_ai.client.token.usage';
const metricTimeToFirstToken = 'gen_ai.client.time_to_first_token';
const metricToolCallsPerOperation = 'gen_ai.client.tool_calls_per_operation';
const metricAttrTokenType = 'gen_ai.token.type';
const metricTokenTypeInput = 'input';
const metricTokenTypeOutput = 'output';
const metricTokenTypeCacheRead = 'cache_read';
const metricTokenTypeCacheWrite = 'cache_write';
const metricTokenTypeReasoning = 'reasoning';

const durationBucketsSeconds: number[] = [
  0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
];
const tokenUsageBuckets: number[] = [
  1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864,
];
const instrumentationName = 'github.com/grafana/sigil/sdks/js';
const sdkName = 'sdk-js';
const defaultEmbeddingOperationName = 'embeddings';
const metadataUserIDKey = 'sigil.user.id';
const metadataLegacyUserIDKey = 'user.id';

function serializeToolResultPayload(value: unknown): { content: string; contentJSON?: string } {
  if (value == null) {
    return { content: '' };
  }
  if (typeof value === 'string') {
    return { content: value };
  }
  try {
    return { content: '', contentJSON: JSON.stringify(value) };
  } catch {
    return { content: String(value) };
  }
}

function buildToolResultMessage(
  toolName: string,
  toolCallId: string,
  result: unknown,
  isError: boolean,
  errorText: string,
): Message {
  if (isError) {
    return {
      role: 'tool',
      name: toolName,
      parts: [
        {
          type: 'tool_result',
          toolResult: {
            toolCallId,
            name: toolName,
            content: errorText,
            isError: true,
          },
        },
      ],
    };
  }
  const { content, contentJSON } = serializeToolResultPayload(result);
  const toolResult =
    contentJSON !== undefined
      ? { toolCallId, name: toolName, content, contentJSON }
      : { toolCallId, name: toolName, content };
  return {
    role: 'tool',
    name: toolName,
    parts: [{ type: 'tool_result', toolResult }],
  };
}

export class SigilClient {
  private readonly config: SigilSdkConfig;
  private readonly nowFn: () => Date;
  private readonly sleepFn: (durationMs: number) => Promise<void>;
  private readonly logger: SigilLogger;
  private readonly generationExporter: GenerationExporter;
  private readonly tracer: Tracer;
  private readonly meter: Meter;
  private readonly operationDurationHistogram: Histogram;
  private readonly tokenUsageHistogram: Histogram;
  private readonly ttftHistogram: Histogram;
  private readonly toolCallsHistogram: Histogram;
  private readonly generations: Generation[] = [];
  private readonly workflowSteps: WorkflowStep[] = [];
  private readonly toolExecutions: ToolExecution[] = [];
  private readonly pendingGenerations: Generation[] = [];
  private readonly pendingWorkflowSteps: WorkflowStep[] = [];

  private flushPromise: Promise<void> | undefined;
  private flushRequested = false;
  private flushTimer: ReturnType<typeof setInterval> | undefined;
  private shutdownPromise: Promise<void> | undefined;
  private shuttingDown = false;
  private closed = false;

  /**
   * Creates a Sigil SDK client.
   *
   * `inputConfig` is merged with defaults.
   */
  constructor(inputConfig: SigilSdkConfigInput = {}) {
    this.config = mergeConfig(inputConfig);
    this.nowFn = this.config.now ?? (() => new Date());
    this.sleepFn = this.config.sleep ?? defaultSleep;
    this.logger = this.config.logger ?? defaultLogger;
    this.generationExporter =
      this.config.generationExporter ?? createDefaultGenerationExporter(this.config.generationExport);
    this.tracer = this.config.tracer ?? trace.getTracer(instrumentationName);
    this.meter = this.config.meter ?? metrics.getMeter(instrumentationName);
    this.operationDurationHistogram = this.meter.createHistogram(metricOperationDuration, {
      unit: 's',
      advice: { explicitBucketBoundaries: durationBucketsSeconds },
    });
    this.tokenUsageHistogram = this.meter.createHistogram(metricTokenUsage, {
      unit: 'token',
      advice: { explicitBucketBoundaries: tokenUsageBuckets },
    });
    this.ttftHistogram = this.meter.createHistogram(metricTimeToFirstToken, {
      unit: 's',
      advice: { explicitBucketBoundaries: durationBucketsSeconds },
    });
    this.toolCallsHistogram = this.meter.createHistogram(metricToolCallsPerOperation, { unit: 'count' });

    if (this.config.generationExport.flushIntervalMs > 0) {
      this.flushTimer = setInterval(() => {
        this.triggerAsyncFlush();
      }, this.config.generationExport.flushIntervalMs);
      maybeUnref(this.flushTimer);
    }
  }

  /** Enqueues a workflow execution node for export. */
  enqueueWorkflowStep(step: WorkflowStep): void {
    this.assertOpen();
    // Validate the raw input before defaulting timestamps, matching Go and
    // Python. The completedAt < startedAt rule only fires when the caller
    // supplied both; defaulting startedAt to now() first would spuriously
    // reject a step for which the caller only provided completedAt.
    const validationError = validateWorkflowStep(step);
    if (validationError !== undefined) {
      throw new Error(`sigil workflow step validation failed: ${validationError.message}`);
    }
    const normalized = this.normalizeWorkflowStep(step);
    // Enqueue first: internalEnqueueWorkflowStep throws on a payload/queue
    // rejection, and enqueueWorkflowStep propagates it. Recording into the
    // debug buffer only after a successful enqueue keeps debugSnapshot from
    // listing a step the caller was told failed to enqueue.
    this.internalEnqueueWorkflowStep(normalized);
    this.workflowSteps.push(cloneWorkflowStep(normalized));
  }

  /**
   * Starts a generation recorder (`SYNC` mode).
   *
   * Overloads:
   * - returns recorder for manual lifecycle
   * - executes callback and auto-ends recorder
   */
  startGeneration(start: GenerationStart): GenerationRecorder;
  startGeneration<TResult>(
    start: GenerationStart,
    callback: RecorderCallback<GenerationRecorder, TResult>,
  ): Promise<TResult>;
  startGeneration<TResult>(
    start: GenerationStart,
    callback?: RecorderCallback<GenerationRecorder, TResult>,
  ): GenerationRecorder | Promise<TResult> {
    return this.startGenerationWithMode(start, 'SYNC', callback);
  }

  /**
   * Starts a streaming generation recorder (`STREAM` mode).
   *
   * Overloads:
   * - returns recorder for manual lifecycle
   * - executes callback and auto-ends recorder
   */
  startStreamingGeneration(start: GenerationStart): GenerationRecorder;
  startStreamingGeneration<TResult>(
    start: GenerationStart,
    callback: RecorderCallback<GenerationRecorder, TResult>,
  ): Promise<TResult>;
  startStreamingGeneration<TResult>(
    start: GenerationStart,
    callback?: RecorderCallback<GenerationRecorder, TResult>,
  ): GenerationRecorder | Promise<TResult> {
    return this.startGenerationWithMode(start, 'STREAM', callback);
  }

  /**
   * Starts an embeddings recorder.
   *
   * Overloads:
   * - returns recorder for manual lifecycle
   * - executes callback and auto-ends recorder
   */
  startEmbedding(start: EmbeddingStart): EmbeddingRecorder;
  startEmbedding<TResult>(
    start: EmbeddingStart,
    callback: RecorderCallback<EmbeddingRecorder, TResult>,
  ): Promise<TResult>;
  startEmbedding<TResult>(
    start: EmbeddingStart,
    callback?: RecorderCallback<EmbeddingRecorder, TResult>,
  ): EmbeddingRecorder | Promise<TResult> {
    this.assertOpen();
    const seed = cloneEmbeddingStart(start);
    if (!notEmpty(seed.agentName)) {
      seed.agentName = agentNameFromContext();
    }
    if (!notEmpty(seed.agentName)) {
      const fromConfig = this.internalAgentName();
      if (fromConfig !== undefined && fromConfig.length > 0) {
        seed.agentName = fromConfig;
      }
    }
    if (!notEmpty(seed.agentVersion)) {
      seed.agentVersion = agentVersionFromContext();
    }
    if (!notEmpty(seed.agentVersion)) {
      const fromConfig = this.internalAgentVersion();
      if (fromConfig !== undefined && fromConfig.length > 0) {
        seed.agentVersion = fromConfig;
      }
    }
    const recorder = new EmbeddingRecorderImpl(this, seed);
    if (callback === undefined) {
      return recorder;
    }
    return runWithRecorder(recorder, callback);
  }

  /**
   * Starts a tool execution recorder.
   *
   * Empty tool names return a no-op recorder to keep instrumentation safe.
   */
  startToolExecution(start: ToolExecutionStart): ToolExecutionRecorder;
  startToolExecution<TResult>(
    start: ToolExecutionStart,
    callback: RecorderCallback<ToolExecutionRecorder, TResult>,
  ): Promise<TResult>;
  startToolExecution<TResult>(
    start: ToolExecutionStart,
    callback?: RecorderCallback<ToolExecutionRecorder, TResult>,
  ): ToolExecutionRecorder | Promise<TResult> {
    this.assertOpen();
    const recorder: ToolExecutionRecorder =
      start.toolName.trim().length === 0 ? new NoopToolExecutionRecorder() : new ToolExecutionRecorderImpl(this, start);
    if (callback === undefined) {
      return recorder;
    }
    return runWithRecorder(recorder, callback);
  }

  /**
   * Runs each `tool_call` part under `execute_tool` spans and returns tool messages.
   *
   * Walks `messages` (typically `GenerationResult.output`) and invokes `executor`
   * for every tool-call part. Returns `tool` role messages with `tool_result` parts.
   */
  async executeToolCalls(
    messages: Message[],
    executor: (toolName: string, args: unknown) => unknown | Promise<unknown>,
    options: ExecuteToolCallsOptions = {},
  ): Promise<Message[]> {
    this.assertOpen();
    const opts = options;
    const out: Message[] = [];
    const list = messages ?? [];

    for (const msg of list) {
      for (const part of msg.parts ?? []) {
        if (part.type !== 'tool_call') {
          continue;
        }
        const tc = part.toolCall;
        const name = (tc.name ?? '').trim();
        if (name.length === 0) {
          continue;
        }
        const callId = (tc.id ?? '').trim();
        const raw = tc.inputJSON?.trim() ?? '';
        let args: unknown = {};
        if (raw.length > 0) {
          try {
            args = JSON.parse(raw) as unknown;
          } catch {
            args = raw;
          }
        }

        const rec = this.startToolExecution({
          toolName: name,
          toolCallId: callId.length > 0 ? callId : undefined,
          toolType: opts.toolType ?? 'function',
          conversationId: opts.conversationId,
          conversationTitle: opts.conversationTitle,
          agentName: opts.agentName,
          agentVersion: opts.agentVersion,
          requestModel: opts.requestModel,
          requestProvider: opts.requestProvider,
          contentCapture: opts.contentCapture,
        });
        try {
          const result = await executor(name, args);
          rec.setResult({ arguments: args, result });
          out.push(buildToolResultMessage(name, callId, result, false, ''));
        } catch (err) {
          rec.setCallError(err);
          const msgText = err instanceof Error ? err.message : String(err);
          out.push(buildToolResultMessage(name, callId, null, true, msgText));
        } finally {
          rec.end();
        }
      }
    }

    return out;
  }

  /** Submits a user-facing conversation rating through Sigil HTTP API. */
  async submitConversationRating(
    conversationId: string,
    input: ConversationRatingInput,
  ): Promise<SubmitConversationRatingResponse> {
    this.assertOpen();

    const normalizedConversationId = conversationId.trim();
    if (normalizedConversationId.length === 0) {
      throw new Error('sigil conversation rating validation failed: conversationId is required');
    }
    if (normalizedConversationId.length > maxRatingConversationIdLen) {
      throw new Error('sigil conversation rating validation failed: conversationId is too long');
    }

    const normalizedInput = normalizeConversationRatingInput(input);
    const endpoint = buildConversationRatingEndpoint(
      this.config.api.endpoint,
      this.config.generationExport.insecure,
      normalizedConversationId,
    );
    const requestBody = {
      rating_id: normalizedInput.ratingId,
      rating: normalizedInput.rating,
      comment: normalizedInput.comment,
      metadata: normalizedInput.metadata,
      generation_id: normalizedInput.generationId,
      rater_id: normalizedInput.raterId,
      source: normalizedInput.source,
    };

    const response = await fetch(endpoint, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...this.config.generationExport.headers,
      },
      body: JSON.stringify(requestBody),
    });

    const responseText = (await response.text()).trim();
    if (response.status === 400) {
      throw new Error(`sigil conversation rating validation failed: ${ratingErrorText(responseText, response.status)}`);
    }
    if (response.status === 409) {
      throw new Error(`sigil conversation rating conflict: ${ratingErrorText(responseText, response.status)}`);
    }
    if (!response.ok) {
      throw new Error(
        `sigil conversation rating transport failed: status ${response.status}: ${ratingErrorText(responseText, response.status)}`,
      );
    }

    if (responseText.length === 0) {
      throw new Error('sigil conversation rating transport failed: empty response payload');
    }

    let payload: unknown;
    try {
      payload = JSON.parse(responseText);
    } catch (error) {
      throw new Error(`sigil conversation rating transport failed: invalid JSON response: ${asError(error).message}`);
    }

    return parseSubmitConversationRatingResponse(payload);
  }

  /**
   * Returns the resolved hook configuration. Framework adapters use this to
   * decide whether to invoke `evaluateHook` and which phases are configured.
   */
  get hooksConfig(): HooksConfig {
    return this.config.hooks;
  }

  /**
   * Evaluates synchronous hook rules for the given request.
   *
   * Use this to enforce preflight or postflight guardrails (PII, content
   * policy, etc.) on the LLM call's critical path. The server returns
   * `{ action: 'deny' }` to block; framework adapters typically translate that
   * into a `HookDeniedError`.
   *
   * When `hooks.enabled` is false, this short-circuits to `allow`. When
   * `hooks.failOpen` is true (default), network/timeout failures also resolve
   * to `allow` so the LLM call can proceed.
   *
   * Framework adapters can pass `hooksConfigOverride` to override specific
   * fields of the client's hooks config (e.g., force `enabled: true` when the
   * adapter has its own `enableHooks` option).
   */
  async evaluateHook(
    request: HookEvaluateRequest,
    hooksConfigOverride?: Partial<HooksConfig>,
  ): Promise<HookEvaluateResponse> {
    this.assertOpen();
    const effectiveHooks: HooksConfig =
      hooksConfigOverride !== undefined ? { ...this.config.hooks, ...hooksConfigOverride } : this.config.hooks;
    return evaluateHookImpl({
      apiEndpoint: this.config.api.endpoint,
      insecure: this.config.generationExport.insecure,
      extraHeaders: this.config.generationExport.headers,
      hooks: effectiveHooks,
      request,
    });
  }

  /** Forces immediate drain of queued generation exports. */
  async flush(): Promise<void> {
    this.assertOpen();
    await this.flushInternal();
  }

  /** Flushes pending generations and shuts down the generation exporter. */
  async shutdown(): Promise<void> {
    if (this.shutdownPromise !== undefined) {
      await this.shutdownPromise;
      return;
    }

    this.shuttingDown = true;
    this.shutdownPromise = (async () => {
      this.stopFlushTimer();
      try {
        await this.flushInternal();
      } catch (error) {
        this.logWarn('sigil generation export flush on shutdown failed', error);
      }

      try {
        await this.generationExporter.shutdown?.();
      } catch (error) {
        this.logWarn('sigil generation exporter shutdown failed', error);
      }

      this.closed = true;
    })();

    await this.shutdownPromise;
  }

  /** Returns a cloned in-memory snapshot for debugging and tests. */
  debugSnapshot(): SigilDebugSnapshot {
    return {
      generations: this.generations.map(cloneGeneration),
      workflowSteps: this.workflowSteps.map(cloneWorkflowStep),
      toolExecutions: this.toolExecutions.map(cloneToolExecution),
      queueSize: this.pendingGenerations.length,
      workflowStepQueueSize: this.pendingWorkflowSteps.length,
    };
  }

  internalNow(): Date {
    return this.nowFn();
  }

  internalAgentName(): string | undefined {
    return this.config.agentName;
  }

  internalAgentVersion(): string | undefined {
    return this.config.agentVersion;
  }

  internalUserId(): string | undefined {
    return this.config.userId;
  }

  internalTags(): Record<string, string> | undefined {
    return this.config.tags;
  }

  internalRecordGeneration(generation: Generation): void {
    this.generations.push(cloneGeneration(generation));
  }

  internalRecordToolExecution(toolExecution: ToolExecution): void {
    this.toolExecutions.push(cloneToolExecution(toolExecution));
  }

  internalEnqueueGeneration(generation: Generation): void {
    if (this.shuttingDown || this.closed) {
      throw new Error('sigil client is shutdown');
    }

    const payloadMaxBytes = this.config.generationExport.payloadMaxBytes;
    if (payloadMaxBytes > 0) {
      const payloadBytes = encodedSizeBytes(generation);
      if (payloadBytes > payloadMaxBytes) {
        throw new Error(`generation payload exceeds max bytes (${payloadBytes} > ${payloadMaxBytes})`);
      }
    }

    const queueSize = Math.max(1, this.config.generationExport.queueSize);
    if (this.pendingGenerations.length >= queueSize) {
      throw new Error('generation queue is full');
    }

    this.pendingGenerations.push(cloneGeneration(generation));

    const batchSize = Math.max(1, this.config.generationExport.batchSize);
    if (this.pendingGenerations.length >= batchSize) {
      this.triggerAsyncFlush();
    }
  }

  internalEnqueueWorkflowStep(step: WorkflowStep): void {
    if (this.shuttingDown || this.closed) {
      throw new Error('sigil client is shutdown');
    }

    const payloadMaxBytes = this.config.generationExport.payloadMaxBytes;
    if (payloadMaxBytes > 0) {
      const payloadBytes = encodedSizeBytes(step);
      if (payloadBytes > payloadMaxBytes) {
        throw new Error(`workflow step payload exceeds max bytes (${payloadBytes} > ${payloadMaxBytes})`);
      }
    }

    const queueSize = Math.max(1, this.config.generationExport.queueSize);
    if (this.pendingWorkflowSteps.length >= queueSize) {
      throw new Error('workflow step queue is full');
    }

    this.pendingWorkflowSteps.push(cloneWorkflowStep(step));

    const batchSize = Math.max(1, this.config.generationExport.batchSize);
    if (this.pendingWorkflowSteps.length >= batchSize) {
      this.triggerAsyncFlush();
    }
  }

  private normalizeWorkflowStep(step: WorkflowStep): WorkflowStep {
    const normalized = cloneWorkflowStep(step);
    const startedAt = normalized.startedAt ?? this.internalNow();
    normalized.startedAt = new Date(startedAt);
    normalized.completedAt = new Date(normalized.completedAt ?? normalized.startedAt);
    normalized.tags = mergeStringRecords(this.config.tags, normalized.tags);
    return normalized;
  }

  internalLogWarn(message: string, error?: unknown): void {
    this.logWarn(message, error);
  }

  internalResolveGenerationContentCaptureMode(seed: GenerationStart): ContentCaptureMode {
    const resolverMode = callContentCaptureResolver(this.config.contentCaptureResolver, seed.metadata);
    const clientMode = resolveClientContentCaptureMode(
      resolveContentCaptureMode(resolverMode, this.config.contentCapture),
    );
    return resolveContentCaptureMode(seed.contentCapture ?? 'default', clientMode);
  }

  internalResolveEmbeddingContentCaptureMode(seed: EmbeddingStart): ContentCaptureMode {
    // Mirror generation resolution so a per-call resolver can hide
    // gen_ai.embeddings.input_texts without changing the client default.
    const resolverMode = callContentCaptureResolver(this.config.contentCaptureResolver, seed.metadata);
    return resolveClientContentCaptureMode(resolveContentCaptureMode(resolverMode, this.config.contentCapture));
  }

  internalResolveToolContentCaptureMode(seed: ToolExecutionStart): ContentCaptureMode {
    const resolverMode = callContentCaptureResolver(this.config.contentCaptureResolver, undefined);
    const clientMode = resolveClientContentCaptureMode(
      resolveContentCaptureMode(resolverMode, this.config.contentCapture),
    );
    return resolveContentCaptureMode(seed.contentCapture ?? 'default', clientMode);
  }

  internalHasGenerationSanitizer(): boolean {
    return this.config.generationSanitizer !== undefined;
  }

  internalSanitizeGeneration(generation: Generation): Generation {
    const sanitizer = this.config.generationSanitizer;
    if (sanitizer === undefined) {
      return generation;
    }
    const sanitized = sanitizer(cloneGeneration(generation));
    if (sanitized === undefined) {
      throw new Error('generation sanitizer must return a generation');
    }
    return cloneGeneration(sanitized);
  }

  internalStartGenerationSpan(
    seed: GenerationStart,
    mode: GenerationMode,
    startedAt: Date,
    contentCaptureMode: ContentCaptureMode,
  ): Span {
    const operationName = seed.operationName ?? defaultOperationNameForMode(mode);
    const span = this.tracer.startSpan(generationSpanName(operationName, seed.model.name), {
      kind: SpanKind.CLIENT,
      startTime: startedAt,
    });

    // metadata_only and full_with_metadata_spans both drop the title from
    // the span. Under full_with_metadata_spans the proto payload still
    // carries the title — it is rebuilt from `seed.conversationTitle` in
    // end(), so we only zero the value sent to the span here.
    const spanTitle =
      contentCaptureMode === 'metadata_only' || contentCaptureMode === 'full_with_metadata_spans'
        ? undefined
        : seed.conversationTitle;

    setGenerationSpanAttributes(span, {
      id: seed.id,
      conversationId: seed.conversationId,
      conversationTitle: spanTitle,
      userId: seed.userId,
      agentName: seed.agentName,
      agentVersion: seed.agentVersion,
      operationName,
      model: seed.model,
      maxTokens: seed.maxTokens,
      temperature: seed.temperature,
      topP: seed.topP,
      toolChoice: seed.toolChoice,
      thinkingEnabled: seed.thinkingEnabled,
      metadata: seed.metadata,
    });
    setTagSpanAttributes(span, this.config.tags);

    return span;
  }

  internalStartEmbeddingSpan(seed: EmbeddingStart, startedAt: Date): Span {
    const span = this.tracer.startSpan(embeddingSpanName(seed.model.name), {
      kind: SpanKind.CLIENT,
      startTime: startedAt,
    });
    setEmbeddingStartSpanAttributes(span, seed);
    setTagSpanAttributes(span, this.config.tags);
    return span;
  }

  internalStartToolExecutionSpan(seed: ToolExecutionStart, startedAt: Date): Span {
    const span = this.tracer.startSpan(toolSpanName(seed.toolName), {
      kind: SpanKind.INTERNAL,
      startTime: startedAt,
    });

    setToolSpanAttributes(span, seed);
    setTagSpanAttributes(span, this.config.tags);
    return span;
  }

  internalApplyTraceContextFromSpan(span: Span, generation: Generation): void {
    const context = span.spanContext();
    if (context.traceId.length > 0) {
      generation.traceId = context.traceId;
    }
    if (context.spanId.length > 0) {
      generation.spanId = context.spanId;
    }
  }

  internalSyncGenerationSpan(span: Span, generation: Generation): void {
    setGenerationSpanAttributes(span, generation);
  }

  internalClearSpanConversationTitle(span: Span): void {
    span.setAttribute(spanAttrConversationTitle, '');
  }

  internalFinalizeGenerationSpan(
    span: Span,
    generation: Generation,
    callError: string | undefined,
    validationError: Error | undefined,
    enqueueError: Error | undefined,
    firstTokenAt: Date | undefined,
    precomputedCallErrorCategory?: string,
  ): void {
    span.updateName(generationSpanName(generation.operationName, generation.model.name));

    if (callError !== undefined) {
      span.recordException(new Error(callError));
    }
    if (validationError !== undefined) {
      span.recordException(validationError);
    }
    if (enqueueError !== undefined) {
      span.recordException(enqueueError);
    }

    let errorType = '';
    let errorCategory = '';
    if (callError !== undefined) {
      errorType = 'provider_call_error';
      errorCategory = precomputedCallErrorCategory ?? errorCategoryFromError(callError, true);
      span.setAttribute(spanAttrErrorType, errorType);
      span.setAttribute(spanAttrErrorCategory, errorCategory);
      span.setStatus({ code: SpanStatusCode.ERROR, message: callError });
    } else if (validationError !== undefined) {
      errorType = 'validation_error';
      errorCategory = 'sdk_error';
      span.setAttribute(spanAttrErrorType, errorType);
      span.setAttribute(spanAttrErrorCategory, errorCategory);
      span.setStatus({ code: SpanStatusCode.ERROR, message: validationError.message });
    } else if (enqueueError !== undefined) {
      errorType = 'enqueue_error';
      errorCategory = 'sdk_error';
      span.setAttribute(spanAttrErrorType, errorType);
      span.setAttribute(spanAttrErrorCategory, errorCategory);
      span.setStatus({ code: SpanStatusCode.ERROR, message: enqueueError.message });
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }

    const spanCtx = trace.setSpan(context.active(), span);
    context.with(spanCtx, () => {
      this.recordGenerationMetrics(generation, errorType, errorCategory, firstTokenAt);
    });

    span.end(generation.completedAt);
  }

  internalFinalizeEmbeddingSpan(
    span: Span,
    seed: EmbeddingStart,
    result: EmbeddingResult,
    hasResult: boolean,
    callError: Error | undefined,
    localError: Error | undefined,
    startedAt: Date,
    completedAt: Date,
    contentCaptureMode: ContentCaptureMode = 'default',
  ): void {
    span.updateName(embeddingSpanName(seed.model.name));
    setEmbeddingEndSpanAttributes(span, result, hasResult, this.config.embeddingCapture, contentCaptureMode);

    // Redact span-side error text under both stripped modes. Embeddings have
    // no proto export, so the raw provider error never escapes the span
    // path; matches the generation full_with_metadata_spans contract.
    const redactSpanErrors =
      contentCaptureMode === 'metadata_only' || contentCaptureMode === 'full_with_metadata_spans';

    if (callError !== undefined && !redactSpanErrors) {
      span.recordException(callError);
    }
    if (localError !== undefined && !redactSpanErrors) {
      span.recordException(localError);
    }

    let errorType = '';
    let errorCategory = '';
    if (callError !== undefined) {
      errorType = 'provider_call_error';
      errorCategory = errorCategoryFromError(callError, true);
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: redactSpanErrors ? errorCategory : callError.message,
      });
    } else if (localError !== undefined) {
      errorType = 'validation_error';
      errorCategory = 'sdk_error';
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: redactSpanErrors ? errorCategory : localError.message,
      });
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }

    if (errorType.length > 0) {
      span.setAttribute(spanAttrErrorType, errorType);
      span.setAttribute(spanAttrErrorCategory, errorCategory);
    }

    const spanCtx = trace.setSpan(context.active(), span);
    context.with(spanCtx, () => {
      this.recordEmbeddingMetrics(seed, result, startedAt, completedAt, errorType, errorCategory);
    });

    span.end(completedAt);
  }

  internalFinalizeToolExecutionSpan(
    span: Span,
    toolExecution: ToolExecution,
    localError: Error | undefined,
    contentCaptureMode: ContentCaptureMode = 'default',
  ): Error | undefined {
    setToolSpanAttributes(span, toolExecution);

    if (toolExecution.includeContent) {
      const argumentsResult = serializeToolContent(toolExecution.arguments);
      if (argumentsResult.error !== undefined && localError === undefined) {
        localError = argumentsResult.error;
      } else if (argumentsResult.value !== undefined) {
        span.setAttribute(spanAttrToolCallArguments, argumentsResult.value);
      }

      const resultValue = serializeToolContent(toolExecution.result);
      if (resultValue.error !== undefined && localError === undefined) {
        localError = resultValue.error;
      } else if (resultValue.value !== undefined) {
        span.setAttribute(spanAttrToolCallResult, resultValue.value);
      }
    }

    // Tools have no proto export; under both stripped modes the span must
    // not echo raw provider exception text via recordException events or the
    // status description.
    const redactSpanErrors =
      contentCaptureMode === 'metadata_only' || contentCaptureMode === 'full_with_metadata_spans';

    if (toolExecution.callError !== undefined) {
      const errorCategory = errorCategoryFromError(toolExecution.callError, true);
      if (!redactSpanErrors) {
        span.recordException(new Error(toolExecution.callError));
      }
      span.setAttribute(spanAttrErrorType, 'tool_execution_error');
      span.setAttribute(spanAttrErrorCategory, errorCategory);
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: redactSpanErrors ? errorCategory : toolExecution.callError,
      });
    } else if (localError !== undefined) {
      const errorCategory = errorCategoryFromError(localError, true);
      if (!redactSpanErrors) {
        span.recordException(localError);
      }
      span.setAttribute(spanAttrErrorType, 'tool_execution_error');
      span.setAttribute(spanAttrErrorCategory, errorCategory);
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: redactSpanErrors ? errorCategory : localError.message,
      });
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }

    const spanCtx = trace.setSpan(context.active(), span);
    context.with(spanCtx, () => {
      this.recordToolExecutionMetrics(
        toolExecution,
        localError ?? (toolExecution.callError !== undefined ? new Error(toolExecution.callError) : undefined),
      );
    });

    span.end(toolExecution.completedAt);
    return localError;
  }

  private recordGenerationMetrics(
    generation: Generation,
    errorType: string,
    errorCategory: string,
    firstTokenAt: Date | undefined,
  ): void {
    const startedMs = generation.startedAt.getTime();
    const completedMs = generation.completedAt.getTime();
    const durationSeconds = Math.max(0, (completedMs - startedMs) / 1_000);
    const identityAttributes = metricIdentityAttributes(
      generation.model.provider,
      generation.model.name,
      generation.agentName,
      generation.agentVersion,
    );
    const tagAttributes = tagMetricAttributes(this.config.tags);
    this.operationDurationHistogram.record(durationSeconds, {
      [spanAttrOperationName]: generation.operationName,
      ...identityAttributes,
      ...tagAttributes,
      [spanAttrErrorType]: errorType,
      [spanAttrErrorCategory]: errorCategory,
    });

    const usage = generation.usage;
    if (usage !== undefined) {
      this.recordTokenUsage(generation, metricTokenTypeInput, usage.inputTokens);
      this.recordTokenUsage(generation, metricTokenTypeOutput, usage.outputTokens);
      this.recordTokenUsage(generation, metricTokenTypeCacheRead, usage.cacheReadInputTokens);
      this.recordTokenUsage(generation, metricTokenTypeCacheWrite, usage.cacheWriteInputTokens);
      this.recordTokenUsage(generation, metricTokenTypeReasoning, usage.reasoningTokens);
    }

    this.toolCallsHistogram.record(countToolCallParts(generation.output ?? []), {
      ...identityAttributes,
      ...tagAttributes,
    });

    if (generation.operationName === 'streamText' && firstTokenAt !== undefined) {
      const ttftSeconds = (firstTokenAt.getTime() - startedMs) / 1_000;
      if (ttftSeconds >= 0) {
        this.ttftHistogram.record(ttftSeconds, {
          ...identityAttributes,
          ...tagAttributes,
        });
      }
    }
  }

  private recordEmbeddingMetrics(
    seed: EmbeddingStart,
    result: EmbeddingResult,
    startedAt: Date,
    completedAt: Date,
    errorType: string,
    errorCategory: string,
  ): void {
    const durationSeconds = Math.max(0, (completedAt.getTime() - startedAt.getTime()) / 1_000);
    const identityAttributes = metricIdentityAttributes(
      seed.model.provider,
      seed.model.name,
      seed.agentName,
      seed.agentVersion,
    );
    const tagAttributes = tagMetricAttributes(this.config.tags);
    this.operationDurationHistogram.record(durationSeconds, {
      [spanAttrOperationName]: defaultEmbeddingOperationName,
      ...identityAttributes,
      ...tagAttributes,
      [spanAttrErrorType]: errorType,
      [spanAttrErrorCategory]: errorCategory,
    });

    if (result.inputTokens !== undefined && result.inputTokens !== 0) {
      this.tokenUsageHistogram.record(result.inputTokens, {
        [spanAttrOperationName]: defaultEmbeddingOperationName,
        ...identityAttributes,
        ...tagAttributes,
        [metricAttrTokenType]: metricTokenTypeInput,
      });
    }
  }

  private recordTokenUsage(generation: Generation, tokenType: string, value: number | undefined): void {
    if (value === undefined || value === 0) {
      return;
    }
    this.tokenUsageHistogram.record(value, {
      [spanAttrOperationName]: generation.operationName,
      ...metricIdentityAttributes(
        generation.model.provider,
        generation.model.name,
        generation.agentName,
        generation.agentVersion,
      ),
      ...tagMetricAttributes(this.config.tags),
      [metricAttrTokenType]: tokenType,
    });
  }

  private recordToolExecutionMetrics(toolExecution: ToolExecution, finalError: Error | undefined): void {
    const startedMs = toolExecution.startedAt.getTime();
    const completedMs = toolExecution.completedAt.getTime();
    const durationSeconds = Math.max(0, (completedMs - startedMs) / 1_000);
    const errorType = finalError === undefined ? '' : 'tool_execution_error';
    const errorCategory = finalError === undefined ? '' : errorCategoryFromError(finalError, true);
    this.operationDurationHistogram.record(durationSeconds, {
      [spanAttrOperationName]: 'execute_tool',
      [spanAttrToolName]: toolExecution.toolName.trim(),
      ...metricIdentityAttributes(
        toolExecution.requestProvider ?? '',
        toolExecution.requestModel ?? '',
        toolExecution.agentName,
        toolExecution.agentVersion,
      ),
      ...tagMetricAttributes(this.config.tags),
      [spanAttrErrorType]: errorType,
      [spanAttrErrorCategory]: errorCategory,
    });
  }

  private assertOpen(): void {
    if (this.shuttingDown || this.closed) {
      throw new Error('sigil client is shutdown');
    }
  }

  private startGenerationWithMode<TResult>(
    start: GenerationStart,
    mode: GenerationMode,
    callback?: RecorderCallback<GenerationRecorder, TResult>,
  ): GenerationRecorder | Promise<TResult> {
    this.assertOpen();
    const recorder = new GenerationRecorderImpl(this, start, mode);
    if (callback === undefined) {
      return recorder;
    }
    return runWithRecorder(recorder, callback);
  }

  private triggerAsyncFlush(): void {
    void this.flushInternal().catch((error) => {
      this.logWarn('sigil generation export failed', error);
    });
  }

  private flushInternal(): Promise<void> {
    if (this.flushPromise !== undefined) {
      this.flushRequested = true;
      return this.flushPromise;
    }

    this.flushPromise = this.drainPendingExports().finally(() => {
      this.flushPromise = undefined;
    });

    return this.flushPromise;
  }

  private async drainPendingExports(): Promise<void> {
    const errors: Error[] = [];
    do {
      this.flushRequested = false;

      while (this.pendingGenerations.length > 0) {
        const batchSize = Math.max(1, this.config.generationExport.batchSize);
        const batch = this.pendingGenerations.splice(0, batchSize).map(cloneGeneration);
        try {
          await this.exportWithRetry(batch);
        } catch (error) {
          errors.push(asError(error));
        }
      }

      while (this.pendingWorkflowSteps.length > 0) {
        const batchSize = Math.max(1, this.config.generationExport.batchSize);
        const batch = this.pendingWorkflowSteps.splice(0, batchSize).map(cloneWorkflowStep);
        try {
          await this.exportWorkflowStepsWithRetry(batch);
        } catch (error) {
          errors.push(asError(error));
        }
      }
    } while (this.flushRequested || this.pendingGenerations.length > 0 || this.pendingWorkflowSteps.length > 0);

    if (errors.length === 1) {
      throw errors[0];
    }
    if (errors.length > 1) {
      throw new AggregateError(errors, 'sigil export failed');
    }
  }

  private async exportWithRetry(generations: Generation[]): Promise<void> {
    const maxRetries = Math.max(0, this.config.generationExport.maxRetries);
    const attempts = maxRetries + 1;
    const baseBackoffMs =
      this.config.generationExport.initialBackoffMs > 0 ? this.config.generationExport.initialBackoffMs : 100;
    const maxBackoffMs =
      this.config.generationExport.maxBackoffMs > 0 ? this.config.generationExport.maxBackoffMs : baseBackoffMs;

    let backoffMs = baseBackoffMs;
    let lastError: Error | undefined;

    for (let attempt = 0; attempt < attempts; attempt++) {
      try {
        const response = await this.generationExporter.exportGenerations({
          generations: generations.map(cloneGeneration),
        });
        this.logRejectedResults(response.results);
        return;
      } catch (error) {
        lastError = asError(error);
        if (attempt === attempts - 1) {
          break;
        }

        await this.sleepFn(backoffMs);
        backoffMs = Math.min(backoffMs * 2, maxBackoffMs);
      }
    }

    throw lastError ?? new Error('generation export failed');
  }

  private async exportWorkflowStepsWithRetry(workflowSteps: WorkflowStep[]): Promise<void> {
    const maxRetries = Math.max(0, this.config.generationExport.maxRetries);
    const attempts = maxRetries + 1;
    const baseBackoffMs =
      this.config.generationExport.initialBackoffMs > 0 ? this.config.generationExport.initialBackoffMs : 100;
    const maxBackoffMs =
      this.config.generationExport.maxBackoffMs > 0 ? this.config.generationExport.maxBackoffMs : baseBackoffMs;

    let backoffMs = baseBackoffMs;
    let lastError: Error | undefined;

    for (let attempt = 0; attempt < attempts; attempt++) {
      try {
        const response = await this.generationExporter.exportWorkflowSteps({
          workflowSteps: workflowSteps.map(cloneWorkflowStep),
        });
        this.logRejectedWorkflowStepResults(response.results);
        return;
      } catch (error) {
        lastError = asError(error);
        if (attempt === attempts - 1) {
          break;
        }

        await this.sleepFn(backoffMs);
        backoffMs = Math.min(backoffMs * 2, maxBackoffMs);
      }
    }

    throw lastError ?? new Error('workflow step export failed');
  }

  private logRejectedResults(results: Array<{ generationId: string; accepted: boolean; error?: string }>): void {
    for (const result of results) {
      if (!result.accepted) {
        this.logWarn(`sigil generation rejected id=${result.generationId}`, result.error);
      }
    }
  }

  private logRejectedWorkflowStepResults(results: Array<{ stepId: string; accepted: boolean; error?: string }>): void {
    for (const result of results) {
      if (!result.accepted) {
        this.logWarn(`sigil workflow step rejected id=${result.stepId}`, result.error);
      }
    }
  }

  private stopFlushTimer(): void {
    if (this.flushTimer !== undefined) {
      clearInterval(this.flushTimer);
      this.flushTimer = undefined;
    }
  }

  private logWarn(message: string, error?: unknown): void {
    if (error === undefined) {
      this.logger.warn?.(message);
      return;
    }
    this.logger.warn?.(`${message}: ${asError(error).message}`);
  }
}

class GenerationRecorderImpl implements GenerationRecorder {
  private readonly seed: GenerationStart;
  private readonly startedAt: Date;
  private readonly mode: GenerationMode;
  private readonly span: Span;
  private readonly contentCaptureMode: ContentCaptureMode;
  private ended = false;
  private result?: GenerationResult;
  private callError?: string;
  private localError?: Error;
  private firstTokenAt?: Date;
  private extraMetadata?: Record<string, unknown>;

  constructor(
    private readonly client: SigilClient,
    seed: GenerationStart,
    defaultMode: GenerationMode,
  ) {
    this.seed = cloneGenerationStart(seed);
    if (!notEmpty(this.seed.conversationId)) {
      this.seed.conversationId = conversationIdFromContext();
    }
    if (!notEmpty(this.seed.conversationTitle)) {
      this.seed.conversationTitle = conversationTitleFromContext();
    }
    if (!notEmpty(this.seed.userId)) {
      this.seed.userId = userIdFromContext();
    }
    if (!notEmpty(this.seed.userId)) {
      const fromConfig = this.client.internalUserId();
      if (fromConfig !== undefined && fromConfig.length > 0) {
        this.seed.userId = fromConfig;
      }
    }
    if (!notEmpty(this.seed.agentName)) {
      this.seed.agentName = agentNameFromContext();
    }
    if (!notEmpty(this.seed.agentName)) {
      const fromConfig = this.client.internalAgentName();
      if (fromConfig !== undefined && fromConfig.length > 0) {
        this.seed.agentName = fromConfig;
      }
    }
    if (!notEmpty(this.seed.agentVersion)) {
      this.seed.agentVersion = agentVersionFromContext();
    }
    if (!notEmpty(this.seed.agentVersion)) {
      const fromConfig = this.client.internalAgentVersion();
      if (fromConfig !== undefined && fromConfig.length > 0) {
        this.seed.agentVersion = fromConfig;
      }
    }
    const tags = this.client.internalTags();
    if (tags !== undefined && Object.keys(tags).length > 0) {
      this.seed.tags = { ...tags, ...(this.seed.tags ?? {}) };
    }
    if (!notEmpty(this.seed.operationName)) {
      this.seed.operationName = defaultOperationNameForMode(this.seed.mode ?? defaultMode);
    }
    this.mode = this.seed.mode ?? defaultMode;
    this.startedAt = this.seed.startedAt ?? this.client.internalNow();
    this.contentCaptureMode = this.client.internalResolveGenerationContentCaptureMode(this.seed);
    this.span = this.client.internalStartGenerationSpan(this.seed, this.mode, this.startedAt, this.contentCaptureMode);
  }

  setResult(result: GenerationResult): void {
    if (this.ended) {
      return;
    }
    this.result = cloneGenerationResult(result);
  }

  setCallError(error: unknown): void {
    if (this.ended) {
      return;
    }
    this.callError = asError(error).message;
  }

  setFirstTokenAt(firstTokenAt: Date): void {
    if (this.ended) {
      return;
    }
    if (!(firstTokenAt instanceof Date) || Number.isNaN(firstTokenAt.getTime())) {
      return;
    }
    this.firstTokenAt = new Date(firstTokenAt);
  }

  setCacheDiagnostics(missReason: string, opts?: { missedInputTokens?: number; previousMessageId?: string }): void {
    if (this.ended) {
      return;
    }
    const trimmed = missReason.trim();
    if (trimmed.length === 0) {
      return;
    }
    if (this.extraMetadata === undefined) {
      this.extraMetadata = {};
    }
    delete this.extraMetadata[CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY];
    delete this.extraMetadata[CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY];
    this.extraMetadata[CACHE_DIAGNOSTICS_MISS_REASON_KEY] = trimmed;
    if (opts?.missedInputTokens !== undefined) {
      this.extraMetadata[CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY] = String(opts.missedInputTokens);
    }
    const prev = opts?.previousMessageId?.trim();
    if (prev !== undefined && prev.length > 0) {
      this.extraMetadata[CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY] = prev;
    }
  }

  end(): void {
    if (this.ended) {
      return;
    }
    this.ended = true;

    let generation: Generation = {
      id: this.seed.id ?? newLocalID('gen'),
      conversationId: firstNonEmptyString(this.result?.conversationId, this.seed.conversationId),
      conversationTitle: firstNonEmptyString(this.result?.conversationTitle, this.seed.conversationTitle),
      userId: firstNonEmptyString(this.result?.userId, this.seed.userId),
      agentName: firstNonEmptyString(this.result?.agentName, this.seed.agentName),
      agentVersion: firstNonEmptyString(this.result?.agentVersion, this.seed.agentVersion),
      mode: this.mode,
      operationName: this.result?.operationName ?? this.seed.operationName ?? defaultOperationNameForMode(this.mode),
      model: cloneModelRef(this.result?.model ?? this.seed.model),
      systemPrompt: this.seed.systemPrompt,
      responseId: this.result?.responseId,
      responseModel: this.result?.responseModel,
      maxTokens: this.result?.maxTokens ?? this.seed.maxTokens,
      temperature: this.result?.temperature ?? this.seed.temperature,
      topP: this.result?.topP ?? this.seed.topP,
      toolChoice: this.result?.toolChoice ?? this.seed.toolChoice,
      thinkingEnabled: this.result?.thinkingEnabled ?? this.seed.thinkingEnabled,
      parentGenerationIds: this.result?.parentGenerationIds?.length
        ? [...this.result.parentGenerationIds]
        : this.seed.parentGenerationIds?.length
          ? [...this.seed.parentGenerationIds]
          : undefined,
      effectiveVersion: firstNonEmptyString(this.result?.effectiveVersion, this.seed.effectiveVersion),
      input: this.result?.input?.map(cloneMessage),
      output: this.result?.output?.map(cloneMessage),
      tools: this.result?.tools?.map(cloneToolDefinition) ?? this.seed.tools?.map(cloneToolDefinition),
      usage: this.result?.usage ? { ...this.result.usage } : undefined,
      stopReason: this.result?.stopReason,
      startedAt: new Date(this.startedAt),
      completedAt: new Date(this.result?.completedAt ?? this.client.internalNow()),
      tags: mergeStringRecords(this.seed.tags, this.result?.tags),
      metadata: mergeUnknownRecords(mergeUnknownRecords(this.seed.metadata, this.result?.metadata), this.extraMetadata),
      artifacts: this.result?.artifacts?.map(cloneArtifact),
      callError: this.callError,
    };

    generation.conversationTitle = firstNonEmptyString(
      generation.conversationTitle,
      metadataStringValue(generation.metadata, spanAttrConversationTitle),
    )?.trim();
    if (notEmpty(generation.conversationTitle)) {
      if (generation.metadata === undefined) {
        generation.metadata = {};
      }
      generation.metadata[spanAttrConversationTitle] = generation.conversationTitle;
    }

    generation.userId = firstNonEmptyString(
      generation.userId,
      metadataStringValue(generation.metadata, metadataUserIDKey),
      metadataStringValue(generation.metadata, metadataLegacyUserIDKey),
    )?.trim();
    if (notEmpty(generation.userId)) {
      if (generation.metadata === undefined) {
        generation.metadata = {};
      }
      generation.metadata[metadataUserIDKey] = generation.userId;
    }

    if (this.callError !== undefined) {
      if (generation.metadata === undefined) {
        generation.metadata = {};
      }
      generation.metadata.call_error = this.callError;
    }
    if (generation.metadata === undefined) {
      generation.metadata = {};
    }
    generation.metadata[spanAttrSDKName] = sdkName;

    const callErrorCategory = errorCategoryFromError(this.callError, false);

    let effectiveContentCaptureMode = this.contentCaptureMode;
    let validationTarget = cloneGeneration(generation);
    stampContentCaptureMetadata(generation, this.contentCaptureMode);
    if (this.contentCaptureMode === 'metadata_only') {
      stripContent(generation, callErrorCategory);
    } else if (this.client.internalHasGenerationSanitizer()) {
      try {
        generation = this.client.internalSanitizeGeneration(generation);
        validationTarget = cloneGeneration(generation);
      } catch (error) {
        effectiveContentCaptureMode = 'metadata_only';
        stripContent(generation, callErrorCategory);
        stampContentCaptureMetadata(generation, effectiveContentCaptureMode);
        this.client.internalLogWarn('sigil generation sanitization failed; falling back to metadata_only', error);
      }
    }

    const validationError = validateGeneration(validationTarget);

    // full_with_metadata_spans: proto export keeps the title, but the span
    // path must drop it. Pass a shallow copy with the title cleared so the
    // in-memory generation (the proto payload) stays untouched.
    const spanGeneration =
      effectiveContentCaptureMode === 'full_with_metadata_spans'
        ? { ...generation, conversationTitle: '' }
        : generation;
    this.client.internalSyncGenerationSpan(this.span, spanGeneration);
    if (
      effectiveContentCaptureMode === 'metadata_only' &&
      this.contentCaptureMode !== 'metadata_only' &&
      this.contentCaptureMode !== 'full_with_metadata_spans'
    ) {
      // Sanitizer fallback downgrades effective mode to metadata_only.
      // Skipped when the original mode already left the attribute absent at
      // start time (metadata_only / full_with_metadata_spans) so the
      // start-span omission isn't re-emitted here as an empty value.
      this.client.internalClearSpanConversationTitle(this.span);
    }
    this.client.internalApplyTraceContextFromSpan(this.span, generation);
    this.client.internalRecordGeneration(generation);

    let enqueueError: Error | undefined;
    if (validationError !== undefined) {
      this.localError = validationError;
      this.client.internalLogWarn('sigil generation validation failed', validationError);
    } else {
      try {
        this.client.internalEnqueueGeneration(generation);
      } catch (error) {
        enqueueError = asError(error);
        this.localError = enqueueError;
        this.client.internalLogWarn('sigil generation enqueue failed', enqueueError);
      }
    }

    // Under metadata_only stripContent already replaced generation.callError
    // with the category, so the span path can read it back from the
    // generation. Under full_with_metadata_spans generation.callError stays
    // raw for the gRPC export, so we substitute the precomputed category for
    // the span path here.
    let finalCallError: string | undefined;
    if (this.callError === undefined) {
      finalCallError = undefined;
    } else if (effectiveContentCaptureMode === 'metadata_only') {
      finalCallError = generation.callError;
    } else if (effectiveContentCaptureMode === 'full_with_metadata_spans') {
      finalCallError = callErrorCategory.length > 0 ? callErrorCategory : 'sdk_error';
    } else {
      finalCallError = this.callError;
    }

    this.client.internalFinalizeGenerationSpan(
      this.span,
      generation,
      finalCallError,
      validationError,
      enqueueError,
      this.firstTokenAt,
      callErrorCategory.length > 0 ? callErrorCategory : undefined,
    );
  }

  getError(): Error | undefined {
    return this.localError;
  }
}

class EmbeddingRecorderImpl implements EmbeddingRecorder {
  private readonly seed: EmbeddingStart;
  private readonly startedAt: Date;
  private readonly span: Span;
  private readonly contentCaptureMode: ContentCaptureMode;
  private ended = false;
  private callError?: Error;
  private result?: EmbeddingResult;
  private hasResult = false;
  private localError?: Error;

  constructor(
    private readonly client: SigilClient,
    seed: EmbeddingStart,
  ) {
    this.seed = cloneEmbeddingStart(seed);
    this.startedAt = this.seed.startedAt ?? this.client.internalNow();
    this.contentCaptureMode = this.client.internalResolveEmbeddingContentCaptureMode(this.seed);
    this.span = this.client.internalStartEmbeddingSpan(this.seed, this.startedAt);
  }

  setCallError(error: unknown): void {
    if (this.ended) {
      return;
    }
    this.callError = asError(error);
  }

  setResult(result: EmbeddingResult): void {
    if (this.ended) {
      return;
    }
    this.result = cloneEmbeddingResult(result);
    this.hasResult = true;
  }

  end(): void {
    if (this.ended) {
      return;
    }
    this.ended = true;

    const completedAt = this.client.internalNow();
    const normalizedResult = this.result ? cloneEmbeddingResult(this.result) : { inputCount: 0 };
    let localError = validateEmbeddingStart(this.seed);
    if (localError === undefined) {
      localError = validateEmbeddingResult(normalizedResult);
    }

    this.client.internalFinalizeEmbeddingSpan(
      this.span,
      this.seed,
      normalizedResult,
      this.hasResult,
      this.callError,
      localError,
      this.startedAt,
      completedAt,
      this.contentCaptureMode,
    );
    this.localError = localError;
  }

  getError(): Error | undefined {
    return this.localError;
  }
}

class ToolExecutionRecorderImpl implements ToolExecutionRecorder {
  private readonly seed: ToolExecutionStart;
  private readonly startedAt: Date;
  private readonly span: Span;
  private readonly resolvedIncludeContent: boolean;
  private readonly toolMode: ContentCaptureMode;
  private ended = false;
  private result?: ToolExecutionResult;
  private callError?: string;
  private localError?: Error;

  constructor(
    private readonly client: SigilClient,
    seed: ToolExecutionStart,
  ) {
    this.seed = cloneToolExecutionStart(seed);
    if (!notEmpty(this.seed.conversationId)) {
      this.seed.conversationId = conversationIdFromContext();
    }
    if (!notEmpty(this.seed.conversationTitle)) {
      this.seed.conversationTitle = conversationTitleFromContext();
    }
    if (!notEmpty(this.seed.agentName)) {
      this.seed.agentName = agentNameFromContext();
    }
    if (!notEmpty(this.seed.agentName)) {
      const fromConfig = this.client.internalAgentName();
      if (notEmpty(fromConfig)) {
        this.seed.agentName = fromConfig;
      }
    }
    if (!notEmpty(this.seed.agentVersion)) {
      this.seed.agentVersion = agentVersionFromContext();
    }
    if (!notEmpty(this.seed.agentVersion)) {
      const fromConfig = this.client.internalAgentVersion();
      if (notEmpty(fromConfig)) {
        this.seed.agentVersion = fromConfig;
      }
    }
    // Under metadata_only or full_with_metadata_spans, the start-time tool
    // span must not carry any content-bearing seed field. Tools have no
    // proto export, so dropping these on the seed is the only redaction
    // surface.
    this.toolMode = this.client.internalResolveToolContentCaptureMode(this.seed);
    if (this.toolMode === 'metadata_only' || this.toolMode === 'full_with_metadata_spans') {
      this.seed.conversationTitle = undefined;
      this.seed.toolDescription = undefined;
    }
    this.resolvedIncludeContent = shouldIncludeToolContent(this.toolMode, this.seed.includeContent ?? false);
    this.startedAt = this.seed.startedAt ?? this.client.internalNow();
    this.span = this.client.internalStartToolExecutionSpan(this.seed, this.startedAt);
  }

  setResult(result: ToolExecutionResult): void {
    if (this.ended) {
      return;
    }
    this.result = cloneToolExecutionResult(result);
  }

  setCallError(error: unknown): void {
    if (this.ended) {
      return;
    }
    this.localError = asError(error);
    this.callError = this.localError.message;
  }

  end(): void {
    if (this.ended) {
      return;
    }
    this.ended = true;

    const toolExecution: ToolExecution = {
      toolName: this.seed.toolName,
      toolCallId: this.seed.toolCallId,
      toolType: this.seed.toolType,
      toolDescription: this.seed.toolDescription,
      conversationId: this.seed.conversationId,
      conversationTitle: this.seed.conversationTitle,
      agentName: this.seed.agentName,
      agentVersion: this.seed.agentVersion,
      requestModel: this.seed.requestModel,
      requestProvider: this.seed.requestProvider,
      includeContent: this.resolvedIncludeContent,
      startedAt: new Date(this.startedAt),
      completedAt: new Date(this.result?.completedAt ?? this.client.internalNow()),
      arguments: this.result?.arguments,
      result: this.result?.result,
      callError: this.callError,
    };

    const validationError = validateToolExecution(toolExecution);
    if (validationError !== undefined) {
      this.localError = validationError;
      this.client.internalLogWarn('sigil tool execution validation failed', validationError);
    } else {
      this.client.internalRecordToolExecution(toolExecution);
    }
    this.localError = this.client.internalFinalizeToolExecutionSpan(
      this.span,
      toolExecution,
      this.localError,
      this.toolMode,
    );
  }

  getError(): Error | undefined {
    return this.localError;
  }
}

class NoopToolExecutionRecorder implements ToolExecutionRecorder {
  setResult(_result: ToolExecutionResult): void {}

  setCallError(_error: unknown): void {}

  end(): void {}

  getError(): Error | undefined {
    return undefined;
  }
}

async function runWithRecorder<TRecorder extends RecorderWithError, TResult>(
  recorder: TRecorder,
  callback: RecorderCallback<TRecorder, TResult>,
): Promise<TResult> {
  let callbackError: unknown;
  try {
    return await callback(recorder);
  } catch (error) {
    callbackError = error;
    recorder.setCallError(error);
    throw error;
  } finally {
    recorder.end();
    const recorderError = recorder.getError();
    if (callbackError === undefined && recorderError !== undefined) {
      // biome-ignore lint/correctness/noUnsafeFinally: intentional — only throws when callback succeeded but recorder detected an error
      throw recorderError;
    }
  }
}

function generationSpanName(operationName: string, modelName: string): string {
  const operation = operationName.trim();
  const model = modelName.trim();
  if (model.length === 0) {
    return operation;
  }
  return `${operation} ${model}`;
}

function embeddingSpanName(modelName: string): string {
  const model = modelName.trim();
  if (model.length === 0) {
    return defaultEmbeddingOperationName;
  }
  return `${defaultEmbeddingOperationName} ${model}`;
}

function toolSpanName(toolName: string): string {
  const normalized = toolName.trim();
  if (normalized.length === 0) {
    return 'execute_tool unknown';
  }
  return `execute_tool ${normalized}`;
}

function tagMetricAttributes(tags: Record<string, string> | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (tags === undefined) {
    return out;
  }
  const pairs: { key: string; value: string }[] = [];
  for (const [k, v] of Object.entries(tags)) {
    const key = k.trim();
    if (key.length === 0) {
      continue;
    }
    pairs.push({ key, value: (v ?? '').trim() });
  }
  pairs.sort((a, b) => a.key.localeCompare(b.key));
  for (const { key, value } of pairs) {
    out[`${spanAttrTagPrefix}${key}`] = value;
  }
  return out;
}

function setTagSpanAttributes(span: Span, tags: Record<string, string> | undefined): void {
  const attrs = tagMetricAttributes(tags);
  for (const [key, value] of Object.entries(attrs)) {
    span.setAttribute(key, value);
  }
}

function metricIdentityAttributes(
  provider: string,
  model: string,
  agentName: string | undefined,
  agentVersion: string | undefined,
): Record<string, string> {
  const attributes: Record<string, string> = {
    [spanAttrProviderName]: provider.trim(),
    [spanAttrRequestModel]: model.trim(),
    [spanAttrAgentName]: agentName?.trim() ?? '',
  };
  if (notEmpty(agentVersion)) {
    attributes[spanAttrAgentVersion] = agentVersion.trim();
  }
  return attributes;
}

function setGenerationSpanAttributes(
  span: Span,
  generation: {
    id?: string;
    conversationId?: string;
    conversationTitle?: string;
    userId?: string;
    agentName?: string;
    agentVersion?: string;
    operationName: string;
    model: { provider: string; name: string };
    maxTokens?: number;
    temperature?: number;
    topP?: number;
    toolChoice?: string;
    thinkingEnabled?: boolean;
    metadata?: Record<string, unknown>;
    responseId?: string;
    responseModel?: string;
    stopReason?: string;
    usage?: {
      inputTokens?: number;
      outputTokens?: number;
      cacheReadInputTokens?: number;
      cacheWriteInputTokens?: number;
      reasoningTokens?: number;
    };
  },
): void {
  span.setAttribute(spanAttrOperationName, generation.operationName);
  span.setAttribute(spanAttrSDKName, sdkName);

  if (notEmpty(generation.id)) {
    span.setAttribute(spanAttrGenerationID, generation.id);
  }
  if (notEmpty(generation.conversationId)) {
    span.setAttribute(spanAttrConversationID, generation.conversationId);
  }
  if (notEmpty(generation.conversationTitle)) {
    span.setAttribute(spanAttrConversationTitle, generation.conversationTitle);
  }
  if (notEmpty(generation.userId)) {
    span.setAttribute(spanAttrUserID, generation.userId);
  }
  if (notEmpty(generation.agentName)) {
    span.setAttribute(spanAttrAgentName, generation.agentName);
  }
  if (notEmpty(generation.agentVersion)) {
    span.setAttribute(spanAttrAgentVersion, generation.agentVersion);
  }
  if (notEmpty(generation.model.provider)) {
    span.setAttribute(spanAttrProviderName, generation.model.provider);
  }
  if (notEmpty(generation.model.name)) {
    span.setAttribute(spanAttrRequestModel, generation.model.name);
  }
  if (generation.maxTokens !== undefined) {
    span.setAttribute(spanAttrRequestMaxTokens, generation.maxTokens);
  }
  if (generation.temperature !== undefined) {
    span.setAttribute(spanAttrRequestTemperature, generation.temperature);
  }
  if (generation.topP !== undefined) {
    span.setAttribute(spanAttrRequestTopP, generation.topP);
  }
  if (notEmpty(generation.toolChoice)) {
    span.setAttribute(spanAttrRequestToolChoice, generation.toolChoice);
  }
  if (generation.thinkingEnabled !== undefined) {
    span.setAttribute(spanAttrRequestThinkingEnabled, generation.thinkingEnabled);
  }
  const thinkingBudget = thinkingBudgetFromMetadata(generation.metadata);
  if (thinkingBudget !== undefined) {
    span.setAttribute(spanAttrRequestThinkingBudget, thinkingBudget);
  }
  const frameworkRunId = metadataStringValue(generation.metadata, spanAttrFrameworkRunID);
  if (frameworkRunId !== undefined) {
    span.setAttribute(spanAttrFrameworkRunID, frameworkRunId);
  }
  const frameworkThreadId = metadataStringValue(generation.metadata, spanAttrFrameworkThreadID);
  if (frameworkThreadId !== undefined) {
    span.setAttribute(spanAttrFrameworkThreadID, frameworkThreadId);
  }
  const frameworkParentRunId = metadataStringValue(generation.metadata, spanAttrFrameworkParentRunID);
  if (frameworkParentRunId !== undefined) {
    span.setAttribute(spanAttrFrameworkParentRunID, frameworkParentRunId);
  }
  const frameworkComponentName = metadataStringValue(generation.metadata, spanAttrFrameworkComponentName);
  if (frameworkComponentName !== undefined) {
    span.setAttribute(spanAttrFrameworkComponentName, frameworkComponentName);
  }
  const frameworkRunType = metadataStringValue(generation.metadata, spanAttrFrameworkRunType);
  if (frameworkRunType !== undefined) {
    span.setAttribute(spanAttrFrameworkRunType, frameworkRunType);
  }
  const frameworkRetryAttempt = metadataIntValue(generation.metadata, spanAttrFrameworkRetryAttempt);
  if (frameworkRetryAttempt !== undefined) {
    span.setAttribute(spanAttrFrameworkRetryAttempt, frameworkRetryAttempt);
  }
  const frameworkLangGraphNode = metadataStringValue(generation.metadata, spanAttrFrameworkLangGraphNode);
  if (frameworkLangGraphNode !== undefined) {
    span.setAttribute(spanAttrFrameworkLangGraphNode, frameworkLangGraphNode);
  }
  const frameworkEventID = metadataStringValue(generation.metadata, spanAttrFrameworkEventID);
  if (frameworkEventID !== undefined) {
    span.setAttribute(spanAttrFrameworkEventID, frameworkEventID);
  }
  if (notEmpty(generation.responseId)) {
    span.setAttribute(spanAttrResponseID, generation.responseId);
  }
  if (notEmpty(generation.responseModel)) {
    span.setAttribute(spanAttrResponseModel, generation.responseModel);
  }
  if (notEmpty(generation.stopReason)) {
    span.setAttribute(spanAttrFinishReasons, [generation.stopReason]);
  }

  const usage = generation.usage;
  if (usage === undefined) {
    return;
  }
  if ((usage.inputTokens ?? 0) !== 0) {
    span.setAttribute(spanAttrInputTokens, usage.inputTokens ?? 0);
  }
  if ((usage.outputTokens ?? 0) !== 0) {
    span.setAttribute(spanAttrOutputTokens, usage.outputTokens ?? 0);
  }
  if ((usage.cacheReadInputTokens ?? 0) !== 0) {
    span.setAttribute(spanAttrCacheReadTokens, usage.cacheReadInputTokens ?? 0);
  }
  if ((usage.cacheWriteInputTokens ?? 0) !== 0) {
    span.setAttribute(spanAttrCacheWriteTokens, usage.cacheWriteInputTokens ?? 0);
  }
  if ((usage.reasoningTokens ?? 0) !== 0) {
    span.setAttribute(spanAttrReasoningTokens, usage.reasoningTokens ?? 0);
  }
}

function setEmbeddingStartSpanAttributes(span: Span, start: EmbeddingStart): void {
  span.setAttribute(spanAttrOperationName, defaultEmbeddingOperationName);
  span.setAttribute(spanAttrSDKName, sdkName);

  if (notEmpty(start.model.provider)) {
    span.setAttribute(spanAttrProviderName, start.model.provider);
  }
  if (notEmpty(start.model.name)) {
    span.setAttribute(spanAttrRequestModel, start.model.name);
  }
  if (notEmpty(start.agentName)) {
    span.setAttribute(spanAttrAgentName, start.agentName);
  }
  if (notEmpty(start.agentVersion)) {
    span.setAttribute(spanAttrAgentVersion, start.agentVersion);
  }
  if (start.dimensions !== undefined) {
    span.setAttribute(spanAttrEmbeddingDimCount, start.dimensions);
  }
  if (notEmpty(start.encodingFormat)) {
    span.setAttribute(spanAttrRequestEncodingFormats, [start.encodingFormat]);
  }
}

function setEmbeddingEndSpanAttributes(
  span: Span,
  result: EmbeddingResult,
  hasResult: boolean,
  captureConfig: SigilSdkConfig['embeddingCapture'],
  contentCaptureMode: ContentCaptureMode = 'default',
): void {
  if (hasResult) {
    span.setAttribute(spanAttrEmbeddingInputCount, result.inputCount);
  }
  if (result.inputTokens !== undefined && result.inputTokens !== 0) {
    span.setAttribute(spanAttrInputTokens, result.inputTokens);
  }
  if (notEmpty(result.responseModel)) {
    span.setAttribute(spanAttrResponseModel, result.responseModel);
  }
  if (result.dimensions !== undefined) {
    span.setAttribute(spanAttrEmbeddingDimCount, result.dimensions);
  }
  // Embeddings have no proto export; full_with_metadata_spans matches
  // metadata_only for input-text span attributes.
  const omitInputTexts = contentCaptureMode === 'metadata_only' || contentCaptureMode === 'full_with_metadata_spans';
  if (captureConfig.captureInput && result.inputTexts !== undefined && !omitInputTexts) {
    const texts = captureEmbeddingInputTexts(
      result.inputTexts,
      captureConfig.maxInputItems,
      captureConfig.maxTextLength,
    );
    if (texts.length > 0) {
      span.setAttribute(spanAttrEmbeddingInputTexts, texts);
    }
  }
}

function setToolSpanAttributes(
  span: Span,
  tool: {
    toolName: string;
    toolCallId?: string;
    toolType?: string;
    toolDescription?: string;
    conversationId?: string;
    conversationTitle?: string;
    agentName?: string;
    agentVersion?: string;
    requestProvider?: string;
    requestModel?: string;
  },
): void {
  span.setAttribute(spanAttrOperationName, 'execute_tool');
  span.setAttribute(spanAttrToolName, tool.toolName);
  span.setAttribute(spanAttrSDKName, sdkName);

  if (notEmpty(tool.toolCallId)) {
    span.setAttribute(spanAttrToolCallID, tool.toolCallId);
  }
  if (notEmpty(tool.toolType)) {
    span.setAttribute(spanAttrToolType, tool.toolType);
  }
  if (notEmpty(tool.toolDescription)) {
    span.setAttribute(spanAttrToolDescription, tool.toolDescription);
  }
  if (notEmpty(tool.conversationId)) {
    span.setAttribute(spanAttrConversationID, tool.conversationId);
  }
  if (notEmpty(tool.conversationTitle)) {
    span.setAttribute(spanAttrConversationTitle, tool.conversationTitle);
  }
  if (notEmpty(tool.agentName)) {
    span.setAttribute(spanAttrAgentName, tool.agentName);
  }
  if (notEmpty(tool.agentVersion)) {
    span.setAttribute(spanAttrAgentVersion, tool.agentVersion);
  }
  if (notEmpty(tool.requestProvider)) {
    span.setAttribute(spanAttrProviderName, tool.requestProvider);
  }
  if (notEmpty(tool.requestModel)) {
    span.setAttribute(spanAttrRequestModel, tool.requestModel);
  }
}

function serializeToolContent(value: unknown): { value?: string; error?: Error } {
  if (value === undefined || value === null) {
    return {};
  }

  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return {};
    }
    if (isJSON(trimmed)) {
      return { value: trimmed };
    }

    try {
      return { value: JSON.stringify(trimmed) };
    } catch (error) {
      return { error: asError(error) };
    }
  }

  try {
    const encoded = JSON.stringify(value);
    if (encoded === undefined || encoded === 'null') {
      return {};
    }
    return { value: encoded };
  } catch (error) {
    return { error: asError(error) };
  }
}

function normalizeConversationRatingInput(input: ConversationRatingInput): ConversationRatingInput {
  const normalized: ConversationRatingInput = {
    ratingId: input.ratingId.trim(),
    rating: input.rating.trim() as ConversationRatingValue,
    comment: input.comment?.trim(),
    metadata: input.metadata,
    generationId: input.generationId?.trim(),
    raterId: input.raterId?.trim(),
    source: input.source?.trim(),
  };

  if (normalized.ratingId.length === 0) {
    throw new Error('sigil conversation rating validation failed: ratingId is required');
  }
  if (normalized.ratingId.length > maxRatingIdLen) {
    throw new Error('sigil conversation rating validation failed: ratingId is too long');
  }
  if (normalized.rating !== 'CONVERSATION_RATING_VALUE_GOOD' && normalized.rating !== 'CONVERSATION_RATING_VALUE_BAD') {
    throw new Error(
      'sigil conversation rating validation failed: rating must be CONVERSATION_RATING_VALUE_GOOD or CONVERSATION_RATING_VALUE_BAD',
    );
  }
  if (normalized.comment !== undefined && encodedSizeBytes(normalized.comment) > maxRatingCommentBytes) {
    throw new Error('sigil conversation rating validation failed: comment is too long');
  }
  if (normalized.generationId !== undefined && normalized.generationId.length > maxRatingGenerationIdLen) {
    throw new Error('sigil conversation rating validation failed: generationId is too long');
  }
  if (normalized.raterId !== undefined && normalized.raterId.length > maxRatingActorIdLen) {
    throw new Error('sigil conversation rating validation failed: raterId is too long');
  }
  if (normalized.source !== undefined && normalized.source.length > maxRatingSourceLen) {
    throw new Error('sigil conversation rating validation failed: source is too long');
  }
  if (normalized.metadata !== undefined && encodedSizeBytes(normalized.metadata) > maxRatingMetadataBytes) {
    throw new Error('sigil conversation rating validation failed: metadata is too large');
  }

  return normalized;
}

function buildConversationRatingEndpoint(endpoint: string, insecure: boolean, conversationId: string): string {
  const baseURL = baseURLFromAPIEndpoint(endpoint, insecure);
  return `${baseURL}/api/v1/conversations/${encodeURIComponent(conversationId)}/ratings`;
}

function baseURLFromAPIEndpoint(endpoint: string, insecure: boolean): string {
  const trimmed = endpoint.trim();
  if (trimmed.length === 0) {
    throw new Error('sigil conversation rating transport failed: api endpoint is required');
  }

  if (trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
    const parsed = new URL(trimmed);
    // Preserve a path prefix so prefix-mounted Sigil deployments
    // (https://host/sigil) route /api/v1/conversations/... under the prefix.
    const path = parsed.pathname.replace(/\/+$/, '');
    return `${parsed.protocol}//${parsed.host}${path}`;
  }

  const withoutScheme = trimmed.startsWith('grpc://') ? trimmed.slice('grpc://'.length) : trimmed;
  const host = withoutScheme.split('/')[0]?.trim();
  if (host === undefined || host.length === 0) {
    throw new Error('sigil conversation rating transport failed: api endpoint host is required');
  }
  return `${insecure ? 'http' : 'https'}://${host}`;
}

function parseSubmitConversationRatingResponse(payload: unknown): SubmitConversationRatingResponse {
  if (!isObject(payload)) {
    throw new Error('sigil conversation rating transport failed: invalid response payload');
  }
  if (!isObject(payload.rating) || !isObject(payload.summary)) {
    throw new Error('sigil conversation rating transport failed: invalid response payload');
  }

  const rating = mapConversationRating(payload.rating);
  const summary = mapConversationRatingSummary(payload.summary);
  return { rating, summary };
}

function mapConversationRating(payload: Record<string, unknown>): ConversationRating {
  const ratingId = asString(payload.rating_id);
  const conversationId = asString(payload.conversation_id);
  const rating = asString(payload.rating) as ConversationRatingValue;
  const createdAt = asString(payload.created_at);
  if (ratingId === undefined || conversationId === undefined || rating === undefined || createdAt === undefined) {
    throw new Error('sigil conversation rating transport failed: invalid rating payload');
  }

  return {
    ratingId,
    conversationId,
    generationId: asString(payload.generation_id),
    rating,
    comment: asString(payload.comment),
    metadata: asRecordUnknown(payload.metadata),
    raterId: asString(payload.rater_id),
    source: asString(payload.source),
    createdAt,
  };
}

function mapConversationRatingSummary(payload: Record<string, unknown>): ConversationRatingSummary {
  const totalCount = asNumber(payload.total_count);
  const goodCount = asNumber(payload.good_count);
  const badCount = asNumber(payload.bad_count);
  const latestRatedAt = asString(payload.latest_rated_at);
  const hasBadRating = asBoolean(payload.has_bad_rating);
  if (
    totalCount === undefined ||
    goodCount === undefined ||
    badCount === undefined ||
    latestRatedAt === undefined ||
    hasBadRating === undefined
  ) {
    throw new Error('sigil conversation rating transport failed: invalid rating summary payload');
  }

  return {
    totalCount,
    goodCount,
    badCount,
    latestRating: asString(payload.latest_rating) as ConversationRatingValue | undefined,
    latestRatedAt,
    latestBadAt: asString(payload.latest_bad_at),
    hasBadRating,
  };
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function asNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function asBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}

function asRecordUnknown(value: unknown): Record<string, unknown> | undefined {
  return isObject(value) ? value : undefined;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function ratingErrorText(responseText: string, status: number): string {
  if (responseText.length > 0) {
    return responseText;
  }
  return `HTTP ${status}`;
}

function captureEmbeddingInputTexts(inputTexts: string[], maxInputItems: number, maxTextLength: number): string[] {
  if (inputTexts.length === 0) {
    return [];
  }
  const itemLimit = maxInputItems > 0 ? maxInputItems : 20;
  const textLimit = maxTextLength > 0 ? maxTextLength : 1024;
  const output: string[] = [];
  const end = Math.min(itemLimit, inputTexts.length);
  for (let index = 0; index < end; index++) {
    output.push(truncateEmbeddingText(inputTexts[index] ?? '', textLimit));
  }
  return output;
}

function truncateEmbeddingText(text: string, maxTextLength: number): string {
  if (text.length <= maxTextLength) {
    return text;
  }
  if (maxTextLength <= 3) {
    return text.slice(0, maxTextLength);
  }
  return `${text.slice(0, maxTextLength - 3)}...`;
}

function thinkingBudgetFromMetadata(metadata: Record<string, unknown> | undefined): number | undefined {
  if (metadata === undefined) {
    return undefined;
  }
  const raw = metadata[spanAttrRequestThinkingBudget];
  if (raw === undefined || raw === null || typeof raw === 'boolean') {
    return undefined;
  }
  if (typeof raw === 'number') {
    if (!Number.isFinite(raw) || !Number.isInteger(raw)) {
      return undefined;
    }
    return raw;
  }
  if (typeof raw === 'string') {
    const trimmed = raw.trim();
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

function metadataStringValue(metadata: Record<string, unknown> | undefined, key: string): string | undefined {
  if (metadata === undefined) {
    return undefined;
  }
  const value = metadata[key];
  if (typeof value !== 'string') {
    return undefined;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : undefined;
}

function metadataIntValue(metadata: Record<string, unknown> | undefined, key: string): number | undefined {
  if (metadata === undefined) {
    return undefined;
  }
  const value = metadata[key];
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

function firstNonEmptyString(...values: Array<string | undefined>): string | undefined {
  for (const value of values) {
    if (notEmpty(value)) {
      return value;
    }
  }
  return undefined;
}

function mergeStringRecords(
  left: Record<string, string> | undefined,
  right: Record<string, string> | undefined,
): Record<string, string> | undefined {
  if (left === undefined && right === undefined) {
    return undefined;
  }
  return {
    ...(left ?? {}),
    ...(right ?? {}),
  };
}

function mergeUnknownRecords(
  left: Record<string, unknown> | undefined,
  right: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  if (left === undefined && right === undefined) {
    return undefined;
  }
  return {
    ...(left ?? {}),
    ...(right ?? {}),
  };
}

function countToolCallParts(messages: Message[]): number {
  let total = 0;
  for (const message of messages) {
    if (message.parts === undefined) {
      continue;
    }
    for (const part of message.parts) {
      if (part.type === 'tool_call') {
        total += 1;
      }
    }
  }
  return total;
}

function errorCategoryFromError(error: unknown, fallbackSDK: boolean): string {
  if (error === undefined || error === null) {
    return fallbackSDK ? 'sdk_error' : '';
  }
  if (typeof error === 'string') {
    return classifyErrorCategory(extractStatusCodeFromError(error), error, fallbackSDK);
  }
  const typed = error as Record<string, unknown>;
  const statusCode = extractStatusCodeFromObject(typed) ?? extractStatusCodeFromError(asError(error).message);
  const message = asError(error).message;
  return classifyErrorCategory(statusCode, message, fallbackSDK);
}

function classifyErrorCategory(statusCode: number | undefined, message: string, fallbackSDK: boolean): string {
  const lowerMessage = message.toLowerCase();
  if (lowerMessage.includes('timeout') || lowerMessage.includes('deadline exceeded')) {
    return 'timeout';
  }
  if (statusCode === 429) {
    return 'rate_limit';
  }
  if (statusCode === 401 || statusCode === 403) {
    return 'auth_error';
  }
  if (statusCode === 408) {
    return 'timeout';
  }
  if (statusCode !== undefined && statusCode >= 500 && statusCode <= 599) {
    return 'server_error';
  }
  if (statusCode !== undefined && statusCode >= 400 && statusCode <= 499) {
    return 'client_error';
  }
  return fallbackSDK ? 'sdk_error' : '';
}

function extractStatusCodeFromObject(error: Record<string, unknown>): number | undefined {
  const direct = asStatusCode(error.status) ?? asStatusCode(error.statusCode);
  if (direct !== undefined) {
    return direct;
  }
  if (isRecord(error.response)) {
    return asStatusCode(error.response.status) ?? asStatusCode(error.response.statusCode);
  }
  if (isRecord(error.error)) {
    return asStatusCode(error.error.status) ?? asStatusCode(error.error.statusCode);
  }
  return undefined;
}

function extractStatusCodeFromError(message: string): number | undefined {
  const matches = message.match(/\b([1-5]\d\d)\b/g);
  if (matches === null) {
    return undefined;
  }
  for (const match of matches) {
    const parsed = Number.parseInt(match, 10);
    if (!Number.isNaN(parsed) && parsed >= 100 && parsed <= 599) {
      return parsed;
    }
  }
  return undefined;
}

function asStatusCode(value: unknown): number | undefined {
  if (typeof value !== 'number') {
    return undefined;
  }
  if (!Number.isInteger(value) || value < 100 || value > 599) {
    return undefined;
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isJSON(value: string): boolean {
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}

function notEmpty(value: string | undefined): value is string {
  return value !== undefined && value.trim().length > 0;
}
