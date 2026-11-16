import { type Span, SpanKind, SpanStatusCode, type Tracer, trace } from '@opentelemetry/api';
import type { Agento11yClient } from '../client.js';
import type {
  GenerationRecorder,
  GenerationResult,
  GenerationStart,
  Message,
  ModelRef,
  TokenUsage,
  ToolDefinition,
  ToolExecutionRecorder,
  WorkflowStep,
} from '../types.js';
import { newLocalID } from '../utils.js';

type AnyRecord = Record<string, unknown>;

type ProviderResolverFn = (context: { modelName: string; serialized?: unknown; invocationParams?: unknown }) => string;

type FrameworkName = 'langchain' | 'langgraph' | 'openai-agents' | 'llamaindex' | 'google-adk' | 'vercel-ai-sdk';

const frameworkInstrumentationName = 'github.com/grafana/sigil/sdks/js/frameworks';
const spanAttrOperationName = 'gen_ai.operation.name';
const spanAttrConversationID = 'gen_ai.conversation.id';
const spanAttrFrameworkName = 'agento11y.framework.name';
const spanAttrFrameworkSource = 'agento11y.framework.source';
const spanAttrFrameworkLanguage = 'agento11y.framework.language';
const spanAttrFrameworkRunID = 'agento11y.framework.run_id';
const spanAttrFrameworkThreadID = 'agento11y.framework.thread_id';
const spanAttrFrameworkParentRunID = 'agento11y.framework.parent_run_id';
const spanAttrFrameworkComponentName = 'agento11y.framework.component_name';
const spanAttrFrameworkRunType = 'agento11y.framework.run_type';
const spanAttrFrameworkTags = 'agento11y.framework.tags';
const spanAttrFrameworkRetryAttempt = 'agento11y.framework.retry_attempt';
const spanAttrFrameworkLangGraphNode = 'agento11y.framework.langgraph.node';
const spanAttrFrameworkEventID = 'agento11y.framework.event_id';
const spanAttrErrorType = 'error.type';
const spanAttrErrorCategory = 'error.category';
const metadataThinkingBudgetTokens = 'agento11y.gen_ai.request.thinking.budget_tokens';
const metadataThinkingLevel = 'agento11y.gen_ai.request.thinking.level';
const frameworkSource = 'handler';
const maxFrameworkMetadataDepth = 5;
// LangGraph JS sends no chain end for the root of a stream the caller stops
// early, so a long-lived handler keeps at most this many graph runs.
const maxTrackedGraphRoots = 1000;

export interface FrameworkHandlerOptions {
  agentName?: string;
  agentVersion?: string;
  providerResolver?: 'auto' | ProviderResolverFn;
  provider?: string;
  captureInputs?: boolean;
  captureOutputs?: boolean;
  /**
   * Fallback conversation id for frameworks that expose no session key.
   * Per-invocation identity (a `conversation_id` in callback metadata, or a
   * LangGraph `thread_id`) still wins, so a handler built once per process
   * cannot override a checkpointed thread.
   */
  conversationId?: string;
  /**
   * Records each node of a graph run as a workflow step, with its input and
   * output state and the generations that ran inside it. The LangGraph and
   * LangChain handlers support it. While it is on, chain runs produce workflow
   * steps instead of framework chain spans.
   */
  captureWorkflowSteps?: boolean;
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

/** Chat-start request parameters lifted out of `invocation_params`. */
interface ChatRequestParams {
  tools: ToolDefinition[];
  temperature: number | undefined;
  maxTokens: number | undefined;
  topP: number | undefined;
  toolChoice: string;
  thinkingEnabled: boolean | undefined;
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

interface FrameworkSpan {
  span: Span;
  // The run's metadata, kept so an error can resolve its capture mode.
  metadata: Record<string, unknown>;
}

interface WorkflowStepState {
  stepId: string;
  conversationId: string;
  stepName: string;
  startedAt: Date;
  tags: Record<string, string>;
  metadata: Record<string, unknown>;
  parentStepIds: string[];
  graphRootKey: string;
  captureOutputs: boolean;
  inputState: Record<string, unknown>;
  linkedGenerationIds: string[];
}

export class Agento11yFrameworkHandler {
  private readonly runs = new Map<string, RunState>();
  private readonly toolRuns = new Map<string, ToolRunState>();
  private readonly chainSpans = new Map<string, FrameworkSpan>();
  private readonly retrieverSpans = new Map<string, FrameworkSpan>();
  private readonly workflowStepRuns = new Map<string, WorkflowStepState>();
  // Graph-level state for workflow step wiring, keyed by the run key of each
  // graph root so concurrent graph invocations on one handler stay isolated.
  private readonly graphRootRunKeys = new Set<string>();
  private readonly graphRunConversationId = new Map<string, string>();
  private readonly graphRunLastStepId = new Map<string, string>();
  private readonly graphRunLastGenerationId = new Map<string, string>();
  // Parent run of every active chain or LLM run. Walked upwards from an LLM
  // run to find the enclosing graph root and workflow step.
  private readonly runToGraphKey = new Map<string, string>();
  private readonly agentName?: string;
  private readonly agentVersion?: string;
  private readonly providerResolver: 'auto' | ProviderResolverFn;
  private readonly provider?: string;
  private readonly captureInputs: boolean;
  private readonly captureOutputs: boolean;
  private readonly captureWorkflowSteps: boolean;
  private readonly conversationId: string;
  private readonly extraTags: Record<string, string>;
  private readonly extraMetadata: Record<string, unknown>;

  constructor(
    private readonly client: Agento11yClient,
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
    // The graph logic relies on LangChain chain callbacks. The other adapters
    // share these options but send chain events with different meanings.
    this.captureWorkflowSteps =
      (options.captureWorkflowSteps ?? false) && (frameworkName === 'langgraph' || frameworkName === 'langchain');
    this.conversationId = (options.conversationId ?? '').trim();
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
      invocationControls: true,
      extraParams,
      callbackTags,
      callbackMetadata,
    });

    const payload = this.startPayload(runKey, provider, modelName, context);
    this.linkGraphGeneration(runKey, parentRunId, payload);
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
    const { input, systemPrompt: messageSystemPrompt } = this.captureInputs
      ? mapChatInputs(messages)
      : { input: [] as Message[], systemPrompt: '' };
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
      invocationControls: true,
    });

    const systemPrompt = asString(read(invocationParams, 'system_prompt')) || messageSystemPrompt;
    const requestParams: ChatRequestParams = {
      tools: resolveToolDefinitions(invocationParams),
      temperature: asMaybeFloat(read(invocationParams, 'temperature')),
      maxTokens: asMaybeInt(read(invocationParams, 'max_tokens')),
      topP: asMaybeFloat(read(invocationParams, 'top_p')),
      toolChoice: asString(read(invocationParams, 'tool_choice')),
      thinkingEnabled: asMaybeBoolean(read(invocationParams, 'thinking_enabled')),
    };
    const payload = this.startPayload(runKey, provider, modelName, context, systemPrompt, requestParams);
    this.linkGraphGeneration(runKey, parentRunId, payload);
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
    this.runToGraphKey.delete(String(runId));
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
    this.runToGraphKey.delete(String(runId));
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
    inputs?: unknown,
  ): void {
    const runKey = String(runId);
    if (runKey.length === 0) {
      return;
    }

    const parentRunKey = normalizeRunID(parentRunId);
    const runType = notEmpty(callbackRunType) ? String(callbackRunType).trim() : 'chain';
    const buildContext = () =>
      this.buildFrameworkContext({
        runId: runKey,
        parentRunId,
        runType,
        runName,
        serialized,
        invocationParams: asRecord(extraParams?.invocation_params),
        extraParams,
        callbackTags,
        callbackMetadata,
      });

    if (this.captureWorkflowSteps) {
      // Register the parent so LLM runs below this chain can walk up to
      // their graph root and enclosing workflow step.
      if (parentRunKey.length > 0) {
        this.runToGraphKey.set(runKey, parentRunKey);
      }
      // A top-level run becomes a graph root with its own conversation. A set
      // of roots keeps concurrent graph invocations on the same handler
      // isolated. Unlike the Python handler, a new root does not take the
      // conversation of another active root: a concurrent run, or a root that
      // never ended, would then share it.
      if (parentRunKey.length === 0) {
        if (this.graphRootRunKeys.size >= maxTrackedGraphRoots) {
          const oldestRootKey = this.graphRootRunKeys.values().next().value;
          if (oldestRootKey !== undefined) {
            this.client.internalLogWarn(
              `agento11y: dropping the state of graph run ${oldestRootKey}: more than ${maxTrackedGraphRoots} graph runs have not ended`,
            );
            this.cleanupGraphStateForRoot(oldestRootKey);
          }
        }
        this.graphRootRunKeys.add(runKey);
        return;
      }
      // Only direct children of a graph root become workflow steps. Sub-chains
      // inside a node, such as conditional edge routers, are skipped.
      if (!this.graphRootRunKeys.has(parentRunKey) || this.workflowStepRuns.has(runKey)) {
        return;
      }
      // LangGraph JS runs its `__start__` input node as a hidden child run,
      // which the Python handler never receives. Skipping hidden runs gives
      // the same steps in both SDKs.
      if (callbackTags?.includes('langsmith:hidden') === true) {
        return;
      }
      this.startWorkflowStep(runKey, parentRunKey, buildContext(), {
        serialized,
        callbackMetadata,
        extraParams,
        runName,
        inputs,
      });
      return;
    }

    if (this.chainSpans.has(runKey)) {
      return;
    }
    const context = buildContext();
    const spanName = notEmpty(context.componentName)
      ? `${this.frameworkName}.chain ${context.componentName}`
      : `${this.frameworkName}.chain`;
    const span = this.getFrameworkTracer().startSpan(spanName, { kind: SpanKind.INTERNAL });
    this.setFrameworkSpanAttributes(span, context, 'framework_chain');
    this.chainSpans.set(runKey, { span, metadata: context.metadata });
  }

  protected onChainEnd(runId: string, outputs?: unknown): void {
    const runKey = String(runId);
    if (this.graphRootRunKeys.has(runKey)) {
      this.cleanupGraphStateForRoot(runKey);
      return;
    }
    if (this.workflowStepRuns.has(runKey)) {
      this.endWorkflowStep(runKey, outputs, undefined);
      return;
    }
    this.runToGraphKey.delete(runKey);
    this.endFrameworkSpan(this.chainSpans, runId, undefined);
  }

  protected onChainError(error: unknown, runId: string): void {
    const runKey = String(runId);
    if (this.graphRootRunKeys.has(runKey)) {
      this.cleanupGraphStateForRoot(runKey);
      return;
    }
    if (this.workflowStepRuns.has(runKey)) {
      this.endWorkflowStep(runKey, undefined, error ?? new Error('framework callback error'));
      return;
    }
    this.runToGraphKey.delete(runKey);
    this.endFrameworkSpan(this.chainSpans, runId, error);
  }

  private startWorkflowStep(
    runKey: string,
    parentRunKey: string,
    context: FrameworkContext,
    run: {
      serialized: unknown;
      callbackMetadata: AnyRecord | undefined;
      extraParams: AnyRecord | undefined;
      runName: string | undefined;
      inputs: unknown;
    },
  ): void {
    // The LangChain handler does not resolve the node for its framework
    // context, but a LangChain handler on a LangGraph graph still names steps
    // by node, as in the Python SDK.
    const langgraphNode =
      context.langgraphNode ||
      resolveLangGraphNode(
        run.callbackMetadata,
        run.extraParams,
        asRecord(run.extraParams?.invocation_params),
        run.serialized,
      );
    // LangChain JS serializes runnables without a `name`, so the component
    // name falls back to the class path. The run name gives a short name, as
    // the Python handler has for most runnables.
    const stepName = langgraphNode || asString(run.runName).trim() || context.componentName || context.runType;
    const metadata =
      langgraphNode.length > 0 && context.langgraphNode.length === 0
        ? { ...context.metadata, [spanAttrFrameworkLangGraphNode]: langgraphNode }
        : context.metadata;
    const graphRootKey = this.findGraphRootKey(runKey);
    const lastStepId = graphRootKey.length > 0 ? this.graphRunLastStepId.get(graphRootKey) : undefined;

    this.workflowStepRuns.set(runKey, {
      stepId: newLocalID('wfs'),
      conversationId: this.resolveGraphConversationId(context.conversationId, parentRunKey),
      stepName,
      startedAt: this.client.internalNow(),
      tags: { ...context.tags, 'agento11y.workflow_step.type': context.runType },
      metadata,
      parentStepIds: lastStepId !== undefined ? [lastStepId] : [],
      graphRootKey,
      captureOutputs: this.captureOutputs,
      inputState: this.captureInputs ? toWorkflowState(run.inputs) : {},
      linkedGenerationIds: [],
    });
  }

  private endWorkflowStep(runKey: string, outputs: unknown, error: unknown): void {
    const state = this.workflowStepRuns.get(runKey);
    if (state === undefined) {
      return;
    }
    this.workflowStepRuns.delete(runKey);

    const step: WorkflowStep = {
      id: state.stepId,
      conversationId: state.conversationId,
      stepName: state.stepName,
      framework: this.frameworkName,
      agentName: this.agentName,
      agentVersion: this.agentVersion,
      startedAt: state.startedAt,
      completedAt: this.client.internalNow(),
      inputState: state.inputState,
      outputState: state.captureOutputs ? toWorkflowState(outputs) : {},
      error: error === undefined ? '' : errorText(error),
      tags: state.tags,
      metadata: state.metadata,
      linkedGenerationIds: [...state.linkedGenerationIds],
      parentStepIds: state.parentStepIds,
    };
    try {
      this.client.enqueueWorkflowStep(step);
    } catch (enqueueError) {
      this.client.internalLogWarn('agento11y: failed to enqueue workflow step', enqueueError);
      return;
    }
    if (state.graphRootKey.length > 0) {
      this.graphRunLastStepId.set(state.graphRootKey, state.stepId);
    }
  }

  /**
   * Wires a generation into its graph run when workflow-step capture is on:
   * one conversation per graph run, the previous generation of the run as
   * parent, and a link from the enclosing workflow step.
   */
  private linkGraphGeneration(runKey: string, parentRunId: string | undefined, payload: GenerationStart): void {
    if (!this.captureWorkflowSteps) {
      return;
    }
    const parentRunKey = normalizeRunID(parentRunId);
    payload.conversationId = this.resolveGraphConversationId(payload.conversationId ?? '', parentRunKey);
    if (parentRunKey.length > 0) {
      this.runToGraphKey.set(runKey, parentRunKey);
    }

    const generationId = newLocalID('gen');
    payload.id = generationId;
    const graphRootKey = this.findGraphRootKey(runKey);
    if (graphRootKey.length > 0) {
      const lastGenerationId = this.graphRunLastGenerationId.get(graphRootKey);
      if (lastGenerationId !== undefined) {
        payload.parentGenerationIds = [lastGenerationId];
      }
      this.graphRunLastGenerationId.set(graphRootKey, generationId);
    }
    this.findEnclosingWorkflowStep(runKey)?.linkedGenerationIds.push(generationId);
  }

  /**
   * Walks the parent chain from runKey to a known graph root. Returns "" when
   * there is none. The visited set guards against a cycle in the parent map.
   */
  private findGraphRootKey(runKey: string): string {
    const visited = new Set<string>();
    let current: string | undefined = runKey;
    while (current !== undefined && current.length > 0 && !visited.has(current)) {
      if (this.graphRootRunKeys.has(current)) {
        return current;
      }
      visited.add(current);
      current = this.runToGraphKey.get(current);
    }
    return '';
  }

  /**
   * Walks the parent chain to the closest ancestor that is a workflow step.
   * An LLM run is often inside a sub-chain (LCEL composition, conditional
   * edges), so the step can be one or two levels above its direct parent.
   */
  private findEnclosingWorkflowStep(runKey: string): WorkflowStepState | undefined {
    const visited = new Set<string>();
    let current = this.runToGraphKey.get(runKey);
    while (current !== undefined && current.length > 0 && !visited.has(current)) {
      visited.add(current);
      const state = this.workflowStepRuns.get(current);
      if (state !== undefined) {
        return state;
      }
      current = this.runToGraphKey.get(current);
    }
    return undefined;
  }

  /** Gives every run under one graph invocation the same conversation id. */
  private resolveGraphConversationId(conversationId: string, parentRunKey: string): string {
    if (!this.captureWorkflowSteps || parentRunKey.length === 0) {
      return conversationId;
    }
    const rootKey = this.findGraphRootKey(parentRunKey);
    if (rootKey.length === 0) {
      return conversationId;
    }
    const cached = this.graphRunConversationId.get(rootKey);
    if (cached !== undefined && cached.length > 0) {
      return cached;
    }
    this.graphRunConversationId.set(rootKey, conversationId);
    return conversationId;
  }

  /**
   * Removes all state of a graph run when its root ends, including parent
   * entries of runs below it, so the maps of a long-lived handler do not grow without bound.
   */
  private cleanupGraphStateForRoot(rootRunKey: string): void {
    const descendants = [...this.runToGraphKey.keys()].filter((runKey) => this.reachesRun(runKey, rootRunKey));
    for (const runKey of descendants) {
      this.runToGraphKey.delete(runKey);
      const orphan = this.workflowStepRuns.get(runKey);
      if (orphan !== undefined) {
        this.workflowStepRuns.delete(runKey);
        this.client.internalLogWarn(
          `agento11y: dropping in-flight workflow step ${orphan.stepId} (node ${orphan.stepName}): the chain end callback never fired`,
        );
      }
    }
    this.graphRootRunKeys.delete(rootRunKey);
    this.graphRunConversationId.delete(rootRunKey);
    this.graphRunLastStepId.delete(rootRunKey);
    this.graphRunLastGenerationId.delete(rootRunKey);
  }

  /** Reports whether walking the parent chain from startKey reaches targetKey. */
  private reachesRun(startKey: string, targetKey: string): boolean {
    const visited = new Set<string>();
    let current = this.runToGraphKey.get(startKey);
    while (current !== undefined && current.length > 0 && !visited.has(current)) {
      if (current === targetKey) {
        return true;
      }
      visited.add(current);
      current = this.runToGraphKey.get(current);
    }
    return false;
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
    this.retrieverSpans.set(runKey, { span, metadata: context.metadata });
  }

  protected onRetrieverEnd(runId: string): void {
    this.endFrameworkSpan(this.retrieverSpans, runId, undefined);
  }

  protected onRetrieverError(error: unknown, runId: string): void {
    this.endFrameworkSpan(this.retrieverSpans, runId, error);
  }

  private startPayload(
    _runId: string,
    provider: string,
    modelName: string,
    context: FrameworkContext,
    systemPrompt?: string,
    requestParams?: ChatRequestParams,
  ): GenerationStart {
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
      ...(notEmpty(systemPrompt ?? '') ? { systemPrompt } : {}),
      ...(requestParams !== undefined && requestParams.tools.length > 0 ? { tools: requestParams.tools } : {}),
      ...(requestParams?.temperature !== undefined ? { temperature: requestParams.temperature } : {}),
      ...(requestParams?.maxTokens !== undefined ? { maxTokens: requestParams.maxTokens } : {}),
      ...(requestParams?.topP !== undefined ? { topP: requestParams.topP } : {}),
      ...(notEmpty(requestParams?.toolChoice) ? { toolChoice: requestParams?.toolChoice } : {}),
      ...(requestParams?.thinkingEnabled !== undefined ? { thinkingEnabled: requestParams.thinkingEnabled } : {}),
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

  private endFrameworkSpan(spans: Map<string, FrameworkSpan>, runId: string, error: unknown): void {
    const runKey = String(runId);
    const entry = spans.get(runKey);
    if (entry === undefined) {
      return;
    }
    spans.delete(runKey);
    const { span, metadata } = entry;
    if (error === undefined) {
      span.setStatus({ code: SpanStatusCode.OK });
      span.end();
      return;
    }

    span.setAttribute(spanAttrErrorType, 'framework_error');
    span.setAttribute(spanAttrErrorCategory, 'sdk_error');
    // A chain error is often the provider error, secrets included, so its
    // text stays off when this run's capture mode hides content or a
    // sanitizer is configured.
    if (this.client.internalShouldRedactFrameworkSpanErrors(metadata)) {
      span.setStatus({ code: SpanStatusCode.ERROR, message: 'sdk_error' });
    } else {
      span.recordException(asError(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
    }
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
    /** Only the LLM and chat-model paths carry sampling controls. */
    invocationControls?: boolean;
  }): FrameworkContext {
    const conversation = resolveFrameworkConversationContext(
      this.frameworkName,
      params.runId,
      params.serialized,
      params.invocationParams,
      params.extraParams,
      params.callbackMetadata,
      this.conversationId,
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
    if (params.invocationControls === true) {
      addInvocationControlMetadata(rawMetadata, params.invocationParams);
    }
    const metadata = normalizeFrameworkMetadata(rawMetadata);

    const tags: Record<string, string> = {
      ...this.extraTags,
      'agento11y.framework.name': this.frameworkName,
      'agento11y.framework.source': frameworkSource,
      'agento11y.framework.language': this.frameworkLanguage,
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
  handlerConversationId: string,
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

  if (handlerConversationId.length > 0) {
    return { conversationId: handlerConversationId, threadId: '' };
  }

  // Deterministic fallback when frameworks do not expose session/conversation identity.
  return { conversationId: `agento11y:framework:${frameworkName}:${runId}`, threadId: '' };
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

// The wire format has no SYSTEM role, so system/developer messages are pulled out
// of the message list and joined into the dedicated `systemPrompt` field rather
// than being mapped as USER messages.
function mapChatInputs(messages: unknown): { input: Message[]; systemPrompt: string } {
  if (!Array.isArray(messages)) {
    return { input: [], systemPrompt: '' };
  }

  const output: Message[] = [];
  const systemChunks: string[] = [];
  for (const batch of messages) {
    if (!Array.isArray(batch)) {
      continue;
    }

    for (const message of batch) {
      const text = extractMessageText(message);
      if (text.length === 0) {
        continue;
      }

      if (isSystemRole(extractMessageRole(message))) {
        systemChunks.push(text);
        continue;
      }

      output.push({
        role: normalizeRole(extractMessageRole(message)),
        content: text,
      });
    }
  }

  return { input: output, systemPrompt: systemChunks.join('\n\n') };
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

function isSystemRole(role: string): boolean {
  const normalized = role.trim().toLowerCase();
  return normalized === 'system' || normalized === 'developer';
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

function addInvocationControlMetadata(metadata: AnyRecord, invocationParams: AnyRecord | undefined): void {
  const thinkingBudget = asMaybeInt(read(invocationParams, 'thinking_budget'));
  if (thinkingBudget !== undefined && !(metadataThinkingBudgetTokens in metadata)) {
    metadata[metadataThinkingBudgetTokens] = thinkingBudget;
  }

  const thinkingLevel = asString(read(invocationParams, 'thinking_level'));
  if (thinkingLevel.length > 0 && !(metadataThinkingLevel in metadata)) {
    metadata[metadataThinkingLevel] = thinkingLevel;
  }
}

function resolveToolDefinitions(invocationParams: AnyRecord | undefined): ToolDefinition[] {
  const raw = read(invocationParams, 'tools');
  if (!Array.isArray(raw)) {
    return [];
  }

  const out: ToolDefinition[] = [];
  for (const item of raw) {
    const toolType = asString(read(item, 'type'));
    // OpenAI-style tools nest the definition under `function`. Anything else, and
    // already-shaped definitions that only carry `type`, describe themselves.
    const nested = toolType === 'function' ? read(item, 'function') : undefined;
    const definition = isRecord(nested) ? nested : item;

    const name = asString(read(definition, 'name'));
    if (name.length === 0) {
      continue;
    }

    const schema = read(definition, 'parameters') ?? read(definition, 'parameters_json_schema');
    const description = asString(read(definition, 'description'));
    out.push({
      name,
      ...(description.length > 0 ? { description } : {}),
      type: toolType.length > 0 ? toolType : 'function',
      ...(schema === undefined || schema === null ? {} : { inputSchemaJSON: stableJSONString(schema) }),
    });
  }
  return out;
}

/** Key order is sorted so a given schema encodes identically here and in the Python handler. */
function stableJSONString(value: unknown): string | undefined {
  try {
    return JSON.stringify(value, (_key, entry: unknown) => {
      if (isRecord(entry) && !Array.isArray(entry)) {
        return Object.fromEntries(Object.entries(entry).sort(([left], [right]) => (left < right ? -1 : 1)));
      }
      return entry;
    });
  } catch {
    return undefined;
  }
}

function asMaybeFloat(value: unknown): number | undefined {
  if (value === undefined || value === null || typeof value === 'boolean') {
    return undefined;
  }
  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : undefined;
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return undefined;
    }
    const parsed = Number.parseFloat(trimmed);
    return Number.isFinite(parsed) ? parsed : undefined;
  }
  return undefined;
}

function asMaybeBoolean(value: unknown): boolean | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value === 'boolean') {
    return value;
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    if (normalized === '1' || normalized === 'true' || normalized === 'yes' || normalized === 'on') {
      return true;
    }
    if (normalized === '0' || normalized === 'false' || normalized === 'no' || normalized === 'off') {
      return false;
    }
    return undefined;
  }
  if (typeof value === 'number') {
    return value !== 0;
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

/**
 * Converts graph state to a JSON-safe record for a workflow step's Struct
 * fields. The JSON round trip turns class instances, such as LangChain
 * messages, into their `toJSON` form. Like the Python SDK, a value that is not
 * an object goes under `raw`, and one value JSON cannot encode does not hide
 * the rest of the state.
 */
function toWorkflowState(value: unknown): Record<string, unknown> {
  if (value === undefined || value === null) {
    return {};
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(JSON.stringify(value, workflowStateReplacer()));
  } catch {
    return { raw: String(value) };
  }
  if (isRecord(parsed) && !Array.isArray(parsed)) {
    return parsed;
  }
  return { raw: parsed };
}

/**
 * JSON replacer for workflow state: BigInt becomes text, Map and Set become
 * an object and an array, and a reference back to an enclosing object becomes
 * "[Circular]".
 */
function workflowStateReplacer(): (this: unknown, key: string, value: unknown) => unknown {
  const ancestors: unknown[] = [];
  return function (this: unknown, _key: string, value: unknown): unknown {
    if (typeof value === 'bigint') {
      return value.toString();
    }
    if (typeof value !== 'object' || value === null) {
      return value;
    }
    // `this` is the object that holds the current key, so popping down to it
    // leaves only the path from the root to the current value.
    while (ancestors.length > 0 && ancestors[ancestors.length - 1] !== this) {
      ancestors.pop();
    }
    if (ancestors.includes(value)) {
      return '[Circular]';
    }
    const converted = value instanceof Map ? Object.fromEntries(value) : value instanceof Set ? [...value] : value;
    ancestors.push(converted);
    return converted;
  };
}

/** Returns the text of an error, as `str(error)` does in the Python SDK. */
function errorText(error: unknown): string {
  if (error instanceof Error) {
    return error.message;
  }
  if (typeof error === 'string') {
    return error;
  }
  const message = read(error, 'message');
  return typeof message === 'string' ? message : String(error);
}

function notEmpty(value: string | undefined): boolean {
  return typeof value === 'string' && value.trim().length > 0;
}
