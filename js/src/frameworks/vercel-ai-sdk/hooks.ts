import type { Agento11yClient } from '../../client.js';
import { HookDeniedError } from '../../hooks.js';
import type {
  GenerationMode,
  GenerationRecorder,
  HookEvaluateResponse,
  Message,
  ToolDefinition,
  ToolExecutionRecorder,
} from '../../types.js';
import {
  buildFrameworkMetadata,
  buildFrameworkTags,
  extractOutputSchemaTool,
  extractStepNumber,
  isTextChunk,
  mapInputMessages,
  mapModelFromStepStart,
  mapResponseFromStepFinish,
  mapStepOutput,
  mapUsageFromStepFinish,
  parseToolCallFinish,
  parseToolCallStart,
  resolveConversationId,
  shouldTreatStepAsError,
} from './mapping.js';
import type {
  Agento11yVercelAiSdkOptions,
  CallOptions,
  ConversationResolution,
  GenerateTextHooks,
  PrepareStepEvent,
  PrepareStepResult,
  StepStartEvent,
  StreamTextHooks,
  ToolCallFinishEvent,
  ToolCallStartEvent,
  VercelAiSdkModelMessage,
} from './types.js';

interface StepState {
  recorder?: GenerationRecorder;
  startedAt: Date;
  inputMessages: Message[];
  conversation: ConversationResolution;
  fallbackSeed: string;
  firstTokenRecorded: boolean;
  firstTokenAt?: Date;
  stepStartEvent: StepStartEvent;
  toolCallIds: Set<string>;
  hasToolCalls: boolean;
  outputSchemaPromise?: Promise<ToolDefinition | undefined>;
  outputSchemaTool?: ToolDefinition;
  outputSchemaResolved: boolean;
}

interface ToolState {
  recorder: ToolExecutionRecorder;
  startedAt: Date;
  stepNumber: number;
  input: unknown;
}

interface CallState {
  stepStates: Map<number, StepState>;
  toolStates: Map<string, ToolState>;
  completedToolCallIds: Set<string>;
  nextSyntheticStepNumber: number;
}

export class Agento11yVercelAiSdkInstrumentation {
  private readonly agentName?: string;
  private readonly agentVersion?: string;
  private readonly captureInputs: boolean;
  private readonly captureOutputs: boolean;
  private readonly extraTags: Record<string, string>;
  private readonly extraMetadata: Record<string, unknown>;
  private readonly resolveConversationIdFn: Agento11yVercelAiSdkOptions['resolveConversationId'];
  private readonly hooksOverride: boolean | undefined;
  private callSequence = 0;

  constructor(
    private readonly client: Agento11yClient,
    options: Agento11yVercelAiSdkOptions = {},
  ) {
    this.agentName = normalizeOptionalString(options.agentName);
    this.agentVersion = normalizeOptionalString(options.agentVersion);
    this.captureInputs = options.captureInputs ?? true;
    this.captureOutputs = options.captureOutputs ?? true;
    this.extraTags = { ...(options.extraTags ?? {}) };
    this.extraMetadata = { ...(options.extraMetadata ?? {}) };
    this.resolveConversationIdFn = options.resolveConversationId;
    this.hooksOverride = options.enableHooks;
  }

  private hooksEnabled(): boolean {
    if (this.hooksOverride !== undefined) {
      return this.hooksOverride;
    }
    return this.client.hooksConfig.enabled;
  }

  private preflightEnabled(): boolean {
    if (!this.hooksEnabled()) {
      return false;
    }
    const phases = this.client.hooksConfig.phases;
    return phases.length === 0 || phases.includes('preflight');
  }

  generateTextHooks(callOptions: CallOptions = {}): GenerateTextHooks {
    return this.createHooks('SYNC', 'generateText', callOptions, false) as GenerateTextHooks;
  }

  streamTextHooks(callOptions: CallOptions = {}): StreamTextHooks {
    return this.createHooks('STREAM', 'streamText', callOptions, true) as StreamTextHooks;
  }

  private createHooks(
    mode: GenerationMode,
    operationName: 'generateText' | 'streamText',
    callOptions: CallOptions,
    includeStreamHandlers: boolean,
  ): GenerateTextHooks | StreamTextHooks {
    const callID = `call-${++this.callSequence}`;
    const callStartedAt = new Date();
    const state: CallState = {
      stepStates: new Map<number, StepState>(),
      toolStates: new Map<string, ToolState>(),
      completedToolCallIds: new Set<string>(),
      nextSyntheticStepNumber: 0,
    };

    const explicitConversationId = normalizeOptionalString(callOptions.conversationId);
    const callAgentName = normalizeOptionalString(callOptions.agentName) ?? this.agentName;
    const mergedCallMetadata = {
      ...this.extraMetadata,
      ...(callOptions.extraMetadata ?? {}),
    };
    const tags = buildFrameworkTags(this.extraTags);
    let streamObservedStartAt: Date | undefined;
    let hasCreatedStreamStepState = false;
    const noteStreamObservedStartAt = (timestamp: Date): void => {
      if (mode !== 'STREAM') {
        return;
      }
      if (streamObservedStartAt === undefined || timestamp.getTime() < streamObservedStartAt.getTime()) {
        streamObservedStartAt = timestamp;
      }
    };
    const createGenerationRecorder = (params: {
      stepStartEvent: StepStartEvent;
      conversationId: string;
      startedAt: Date;
    }): GenerationRecorder => {
      const model = mapModelFromStepStart(params.stepStartEvent);
      return mode === 'STREAM'
        ? this.client.startStreamingGeneration({
            conversationId: params.conversationId,
            agentName: callAgentName,
            agentVersion: this.agentVersion,
            mode,
            operationName,
            model: {
              provider: model.provider,
              name: model.modelName,
            },
            tags,
            metadata: buildFrameworkMetadata(mergedCallMetadata, undefined, undefined),
            startedAt: params.startedAt,
          })
        : this.client.startGeneration({
            conversationId: params.conversationId,
            agentName: callAgentName,
            agentVersion: this.agentVersion,
            mode,
            operationName,
            model: {
              provider: model.provider,
              name: model.modelName,
            },
            tags,
            metadata: buildFrameworkMetadata(mergedCallMetadata, undefined, undefined),
            startedAt: params.startedAt,
          });
    };
    const ensureStepRecorder = (stepState: StepState, conversationId: string): GenerationRecorder => {
      if (stepState.recorder !== undefined) {
        return stepState.recorder;
      }
      const recorder = createGenerationRecorder({
        stepStartEvent: stepState.stepStartEvent,
        conversationId,
        startedAt: stepState.startedAt,
      });
      stepState.recorder = recorder;
      return recorder;
    };
    const createStepState = (params: {
      stepNumber: number;
      stepStartEvent: StepStartEvent;
      startedAt: Date;
      inputMessages: Message[];
    }): StepState => {
      const conversation = resolveConversationId({
        explicitConversationId,
        resolver: this.resolveConversationIdFn,
        stepStartEvent: params.stepStartEvent,
        fallbackSeed: `${callID}:step-${params.stepNumber}`,
      });

      const stepState: StepState = {
        recorder:
          mode === 'STREAM' && hasStepStartModelRef(params.stepStartEvent)
            ? createGenerationRecorder({
                stepStartEvent: params.stepStartEvent,
                conversationId: conversation.conversationId,
                startedAt: params.startedAt,
              })
            : undefined,
        startedAt: params.startedAt,
        inputMessages: params.inputMessages,
        conversation,
        fallbackSeed: `${callID}:step-${params.stepNumber}`,
        firstTokenRecorded: false,
        firstTokenAt: undefined,
        stepStartEvent: params.stepStartEvent,
        toolCallIds: new Set<string>(),
        hasToolCalls: false,
        outputSchemaResolved: false,
      };
      state.stepStates.set(params.stepNumber, stepState);
      if (mode === 'STREAM') {
        hasCreatedStreamStepState = true;
      }
      kickOffOutputSchemaExtraction(stepState);
      return stepState;
    };
    const kickOffOutputSchemaExtraction = (stepState: StepState): void => {
      const outputRef = stepState.stepStartEvent.output;
      if (outputRef === undefined || outputRef === null) {
        stepState.outputSchemaResolved = true;
        return;
      }
      const promise = extractOutputSchemaTool(outputRef).then(
        (tool) => {
          stepState.outputSchemaTool = tool;
          stepState.outputSchemaResolved = true;
          return tool;
        },
        () => {
          stepState.outputSchemaResolved = true;
          return undefined;
        },
      );
      stepState.outputSchemaPromise = promise;
    };
    const resolveOrCreateStepStateForFinish = (params: {
      eventStepNumber: unknown;
      responseModel: string | undefined;
    }): { stepNumber: number; stepState: StepState } => {
      const resolvedStepNumber = resolveStepNumberForEvent(state, params.eventStepNumber);
      if (resolvedStepNumber !== undefined) {
        const resolvedState = state.stepStates.get(resolvedStepNumber);
        if (resolvedState !== undefined) {
          return { stepNumber: resolvedStepNumber, stepState: resolvedState };
        }
      }

      const syntheticStepNumber = extractStepNumber(
        { stepNumber: params.eventStepNumber },
        state.nextSyntheticStepNumber,
      );
      state.nextSyntheticStepNumber = Math.max(state.nextSyntheticStepNumber + 1, syntheticStepNumber + 1);

      const syntheticStepStartEvent: StepStartEvent = {
        stepNumber: syntheticStepNumber,
        model: params.responseModel !== undefined ? { modelId: params.responseModel } : undefined,
      };
      const now = new Date();
      const syntheticStartedAt =
        mode === 'STREAM' ? (hasCreatedStreamStepState ? now : (streamObservedStartAt ?? callStartedAt)) : now;
      const syntheticStepState = createStepState({
        stepNumber: syntheticStepNumber,
        stepStartEvent: syntheticStepStartEvent,
        startedAt: syntheticStartedAt,
        inputMessages: [],
      });
      return { stepNumber: syntheticStepNumber, stepState: syntheticStepState };
    };
    const resolveOrCreateStepStateForObservedEvent = (params: {
      eventStepNumber: unknown;
      observedAt: Date;
    }): { stepNumber: number; stepState: StepState } => {
      const resolvedStepNumber = resolveStepNumberForEvent(state, params.eventStepNumber);
      if (resolvedStepNumber !== undefined) {
        const resolvedState = state.stepStates.get(resolvedStepNumber);
        if (resolvedState !== undefined) {
          return { stepNumber: resolvedStepNumber, stepState: resolvedState };
        }
      }

      const syntheticStepNumber = extractStepNumber(
        { stepNumber: params.eventStepNumber },
        state.nextSyntheticStepNumber,
      );
      state.nextSyntheticStepNumber = Math.max(state.nextSyntheticStepNumber + 1, syntheticStepNumber + 1);

      const syntheticStepStartEvent: StepStartEvent = {
        stepNumber: syntheticStepNumber,
      };
      const syntheticStepState = createStepState({
        stepNumber: syntheticStepNumber,
        stepStartEvent: syntheticStepStartEvent,
        startedAt: params.observedAt,
        inputMessages: [],
      });
      return { stepNumber: syntheticStepNumber, stepState: syntheticStepState };
    };
    const parseJSONIfPossible = (value: string | undefined): unknown => {
      if (typeof value !== 'string') {
        return undefined;
      }
      const trimmed = value.trim();
      if (trimmed.length === 0) {
        return undefined;
      }
      try {
        return JSON.parse(trimmed);
      } catch {
        return trimmed;
      }
    };
    const extractToolPartsFromStepOutput = (
      output: Message[] | undefined,
    ): {
      toolCalls: Array<{ id?: string; name: string; inputJSON?: string }>;
      toolResults: Array<{
        toolCallId?: string;
        name?: string;
        content?: string;
        contentJSON?: string;
        isError?: boolean;
      }>;
    } => {
      const toolCalls: Array<{
        id?: string;
        name: string;
        inputJSON?: string;
      }> = [];
      const toolResults: Array<{
        toolCallId?: string;
        name?: string;
        content?: string;
        contentJSON?: string;
        isError?: boolean;
      }> = [];

      if (!Array.isArray(output)) {
        return { toolCalls, toolResults };
      }

      for (const message of output) {
        if (!Array.isArray(message.parts)) {
          continue;
        }
        for (const part of message.parts) {
          if (part.type === 'tool_call') {
            toolCalls.push(part.toolCall);
            continue;
          }
          if (part.type === 'tool_result') {
            toolResults.push(part.toolResult);
          }
        }
      }

      return { toolCalls, toolResults };
    };
    const recordToolExecutionFromStepFinish = (params: {
      stepNumber: number;
      stepState: StepState;
      conversationId: string;
      output: Message[] | undefined;
    }): Error | undefined => {
      const { toolCalls, toolResults } = extractToolPartsFromStepOutput(params.output);
      if (toolResults.length === 0) {
        return undefined;
      }

      const toolCallsByID = new Map<string, { id?: string; name: string; inputJSON?: string }>();
      const toolCallsByName = new Map<string, Array<{ id?: string; name: string; inputJSON?: string }>>();
      for (const toolCall of toolCalls) {
        if (toolCall.id !== undefined) {
          toolCallsByID.set(toolCall.id, toolCall);
        }
        const existing = toolCallsByName.get(toolCall.name) ?? [];
        existing.push(toolCall);
        toolCallsByName.set(toolCall.name, existing);
      }

      let firstError: Error | undefined;
      for (const [index, toolResult] of toolResults.entries()) {
        let toolCallID = toolResult.toolCallId;
        let matchedToolCall = toolCallID !== undefined ? toolCallsByID.get(toolCallID) : undefined;
        if (matchedToolCall === undefined && toolResult.name !== undefined) {
          const matchingByName = toolCallsByName.get(toolResult.name);
          matchedToolCall = matchingByName?.shift();
        }
        if (toolCallID === undefined || toolCallID.length === 0) {
          toolCallID = matchedToolCall?.id ?? `${callID}:step-${params.stepNumber}:tool-${index}`;
        }

        if (state.completedToolCallIds.has(toolCallID)) {
          continue;
        }

        const liveToolState = state.toolStates.get(toolCallID);
        if (liveToolState !== undefined) {
          try {
            const toolResultPayload: {
              arguments?: unknown;
              result?: unknown;
              completedAt: Date;
            } = {
              completedAt: new Date(),
            };
            if (this.captureInputs) {
              toolResultPayload.arguments = liveToolState.input;
            }
            if (this.captureOutputs) {
              toolResultPayload.result = parseJSONIfPossible(toolResult.contentJSON) ?? toolResult.content;
            }
            if (toolResult.isError) {
              liveToolState.recorder.setCallError(toolResult.content ?? new Error('tool call failed'));
            }
            liveToolState.recorder.setResult(toolResultPayload);
          } finally {
            liveToolState.recorder.end();
          }

          const recorderError = liveToolState.recorder.getError();
          if (firstError === undefined && recorderError !== undefined && !toolResult.isError) {
            firstError = recorderError;
          }
          state.toolStates.delete(toolCallID);
          const liveStepState = state.stepStates.get(liveToolState.stepNumber);
          liveStepState?.toolCallIds.delete(toolCallID);
          state.completedToolCallIds.add(toolCallID);
          continue;
        }

        const resolvedToolName = toolResult.name ?? matchedToolCall?.name ?? 'framework_tool';
        const recorder = this.client.startToolExecution({
          toolName: resolvedToolName,
          toolCallId: toolCallID,
          conversationId: params.conversationId,
          agentName: callAgentName,
          agentVersion: this.agentVersion,
          includeContent: this.captureInputs || this.captureOutputs,
          startedAt: params.stepState.startedAt,
        });

        try {
          const toolResultPayload: {
            arguments?: unknown;
            result?: unknown;
            completedAt: Date;
          } = {
            completedAt: new Date(),
          };
          if (this.captureInputs) {
            toolResultPayload.arguments = parseJSONIfPossible(matchedToolCall?.inputJSON);
          }
          if (this.captureOutputs) {
            toolResultPayload.result = parseJSONIfPossible(toolResult.contentJSON) ?? toolResult.content;
          }
          if (toolResult.isError) {
            recorder.setCallError(toolResult.content ?? new Error('tool call failed'));
          }
          recorder.setResult(toolResultPayload);
        } finally {
          recorder.end();
        }

        const recorderError = recorder.getError();
        if (firstError === undefined && recorderError !== undefined && !toolResult.isError) {
          firstError = recorderError;
        }
        state.completedToolCallIds.add(toolCallID);
      }

      return firstError;
    };

    const runPreflight = async (
      event: StepStartEvent,
      inputMessages: Message[],
    ): Promise<{ messages: Message[]; providerMessages?: VercelAiSdkModelMessage[]; transformed: boolean }> => {
      const model = mapModelFromStepStart(event);
      // evaluateHook honors fail-open internally; an exception here means
      // fail_open=false and the LLM call should be aborted.
      // Pass enabled: true to override the client's hooks.enabled since we've
      // already verified hooks are enabled via hooksEnabled() / preflightEnabled().
      const response: HookEvaluateResponse = await this.client.evaluateHook(
        {
          phase: 'preflight',
          context: {
            agentName: callAgentName,
            agentVersion: this.agentVersion,
            model: { provider: model.provider, name: model.modelName },
            tags,
          },
          input: {
            messages: inputMessages.length > 0 ? inputMessages : undefined,
          },
        },
        { enabled: true },
      );
      if (response.action === 'deny') {
        throw new HookDeniedError(
          response.reason ?? 'request blocked by Sigil hook rule',
          response.ruleId,
          response.evaluations,
        );
      }
      const ti = response.transformedInput?.messages;
      if (ti !== undefined && ti.length > 0) {
        return {
          messages: ti,
          providerMessages: toVercelAiSdkModelMessages(ti),
          transformed: true,
        };
      }
      return { messages: inputMessages, transformed: false };
    };

    const handleStepStart = (
      event: StepStartEvent,
      returnPreparedMessages: boolean,
    ): PrepareStepResult | PromiseLike<PrepareStepResult> => {
      noteStreamObservedStartAt(new Date());
      const fallbackStep = state.nextSyntheticStepNumber;
      const stepNumber = extractStepNumber(event, fallbackStep);
      if (state.stepStates.has(stepNumber)) {
        state.nextSyntheticStepNumber = Math.max(state.nextSyntheticStepNumber, stepNumber + 1);
        return undefined;
      }
      state.nextSyntheticStepNumber = Math.max(state.nextSyntheticStepNumber + 1, stepNumber + 1);

      const inputMessages = mapInputMessages(event.messages);
      const finalize = (
        resolvedMessages: Message[],
        providerMessages: VercelAiSdkModelMessage[] | undefined,
        transformed: boolean,
      ): PrepareStepResult => {
        if (transformed) {
          if (!returnPreparedMessages) {
            throw new Error(
              'agento11y preflight transformed_input.messages cannot be applied by experimental_onStepStart; use prepareStep-capable AI SDK instrumentation',
            );
          }
          if (providerMessages === undefined) {
            throw new Error(
              'agento11y preflight transformed_input.messages could not be converted to Vercel AI SDK model messages',
            );
          }
        }

        createStepState({
          stepNumber,
          stepStartEvent: event,
          startedAt: new Date(),
          inputMessages: this.captureInputs ? resolvedMessages : [],
        });
        if (returnPreparedMessages && providerMessages !== undefined) {
          return { messages: providerMessages };
        }
        return undefined;
      };

      if (!this.preflightEnabled()) {
        return finalize(inputMessages, undefined, false);
      }

      // Preflight evaluates against the actual prompt/messages even when
      // captureInputs is disabled — the hook server only sees them in
      // memory and never persists them.
      return runPreflight(event, inputMessages).then(({ messages, providerMessages, transformed }) =>
        finalize(messages, providerMessages, transformed),
      );
    };

    const hooks: StreamTextHooks = {
      prepareStep: (event: PrepareStepEvent) => handleStepStart(event, true),
      // Returns Promise<void> when preflight is enabled, void otherwise.
      // The Vercel AI SDK awaits both shapes; keeping the sync path avoids
      // microtask scheduling for callers that don't use hooks.
      experimental_onStepStart: (event) => {
        const result = handleStepStart(event, false);
        if (isPromiseLike(result)) {
          return Promise.resolve(result).then(() => undefined);
        }
        return undefined;
      },
      onStepFinish: (event) => {
        const response = mapResponseFromStepFinish(event);
        const { stepNumber, stepState } = resolveOrCreateStepStateForFinish({
          eventStepNumber: event.stepNumber,
          responseModel: response.responseModel,
        });
        const conversation = stepState.conversation;
        if (stepState.recorder === undefined && response.responseModel !== undefined) {
          stepState.stepStartEvent = {
            ...stepState.stepStartEvent,
            model: {
              ...(stepState.stepStartEvent.model !== null && typeof stepState.stepStartEvent.model === 'object'
                ? stepState.stepStartEvent.model
                : {}),
              modelId: response.responseModel,
            },
          };
        }
        let recorder: GenerationRecorder;
        try {
          recorder = ensureStepRecorder(stepState, conversation.conversationId);
        } catch (error) {
          state.stepStates.delete(stepNumber);
          if (stepState.toolCallIds.size > 0) {
            closeStepTools(state, stepState, error);
          }
          throw error;
        }
        if (stepState.firstTokenAt !== undefined) {
          recorder.setFirstTokenAt(stepState.firstTokenAt);
        }

        const outputMapping = mapStepOutput(event);
        const metadata = buildFrameworkMetadata(
          mergedCallMetadata,
          outputMapping.stepType,
          this.captureOutputs ? outputMapping.reasoningText : undefined,
        );
        const usage = mapUsageFromStepFinish(event);
        const isError = shouldTreatStepAsError(event);

        const completeFinish = (schemaTool: ToolDefinition | undefined): void => {
          const tools = schemaTool !== undefined ? [schemaTool] : undefined;
          try {
            recorder.setResult({
              conversationId: conversation.conversationId,
              agentName: callAgentName,
              agentVersion: this.agentVersion,
              operationName,
              input: this.captureInputs ? stepState.inputMessages : undefined,
              output: this.captureOutputs ? outputMapping.output : undefined,
              usage,
              stopReason: response.finishReason,
              responseId: response.responseId,
              responseModel: response.responseModel,
              tags,
              metadata,
              tools,
              completedAt: new Date(),
            });
            if (isError) {
              recorder.setCallError(event.error ?? new Error('step finished with error'));
            }
          } finally {
            recorder.end();
          }

          const recorderError = recorder.getError();
          const fallbackToolRecorderError = recordToolExecutionFromStepFinish({
            stepNumber,
            stepState,
            conversationId: conversation.conversationId,
            output: outputMapping.output,
          });
          state.stepStates.delete(stepNumber);
          if (stepState.toolCallIds.size > 0) {
            const closeError = isError
              ? (event.error ?? new Error('parent step failed'))
              : new Error('tool call did not finish before step completion');
            closeStepTools(state, stepState, closeError);
          }
          if (recorderError !== undefined) {
            throw recorderError;
          }
          if (fallbackToolRecorderError !== undefined) {
            throw fallbackToolRecorderError;
          }
        };

        if (stepState.outputSchemaResolved || stepState.outputSchemaPromise === undefined) {
          completeFinish(stepState.outputSchemaTool);
          return;
        }
        return stepState.outputSchemaPromise.then(
          (tool) => {
            completeFinish(tool);
          },
          () => {
            completeFinish(undefined);
          },
        );
      },
      experimental_onToolCallStart: (event: ToolCallStartEvent) => {
        const parsed = parseToolCallStart(event);
        if (parsed === undefined) {
          return;
        }
        state.completedToolCallIds.delete(parsed.toolCallId);
        if (state.toolStates.has(parsed.toolCallId)) {
          return;
        }

        const startedAt = new Date();
        if (mode === 'STREAM') {
          noteStreamObservedStartAt(startedAt);
        }
        const { stepNumber, stepState } = resolveOrCreateStepStateForObservedEvent({
          eventStepNumber: event.stepNumber,
          observedAt: startedAt,
        });

        const recorder = this.client.startToolExecution({
          toolName: parsed.toolName,
          toolCallId: parsed.toolCallId,
          toolType: parsed.toolType,
          toolDescription: parsed.description,
          conversationId: stepState.conversation.conversationId,
          agentName: callAgentName,
          agentVersion: this.agentVersion,
          includeContent: this.captureInputs || this.captureOutputs,
          startedAt,
        });
        state.toolStates.set(parsed.toolCallId, {
          recorder,
          startedAt,
          stepNumber,
          input: parsed.input,
        });
        stepState.hasToolCalls = true;
        stepState.toolCallIds.add(parsed.toolCallId);
      },
      experimental_onToolCallFinish: (event: ToolCallFinishEvent) => {
        const parsed = parseToolCallFinish(event);
        if (parsed === undefined) {
          return;
        }

        const toolState = state.toolStates.get(parsed.toolCallId);
        if (toolState === undefined) {
          return;
        }
        const completedAt =
          parsed.durationMs !== undefined ? new Date(toolState.startedAt.getTime() + parsed.durationMs) : new Date();

        try {
          if (parsed.success) {
            const result: {
              arguments?: unknown;
              result?: unknown;
              completedAt: Date;
            } = {
              completedAt,
            };
            if (this.captureInputs) {
              result.arguments = toolState.input;
            }
            if (this.captureOutputs) {
              result.result = parsed.output;
            }
            toolState.recorder.setResult(result);
          } else {
            toolState.recorder.setCallError(parsed.error ?? new Error('tool call failed'));
            toolState.recorder.setResult({ completedAt });
          }
        } finally {
          toolState.recorder.end();
        }

        const recorderError = toolState.recorder.getError();
        state.toolStates.delete(parsed.toolCallId);
        const stepState = state.stepStates.get(toolState.stepNumber);
        stepState?.toolCallIds.delete(parsed.toolCallId);
        state.completedToolCallIds.add(parsed.toolCallId);
        if (parsed.success && recorderError !== undefined) {
          throw recorderError;
        }
      },
    };

    if (includeStreamHandlers) {
      const finalizeStreamFailure = (error: unknown): void => {
        const observedAt = new Date();
        const fallbackStreamStartedAt =
          streamObservedStartAt ?? (hasCreatedStreamStepState ? observedAt : callStartedAt);
        noteStreamObservedStartAt(observedAt);
        if (state.stepStates.size === 0) {
          resolveOrCreateStepStateForObservedEvent({
            eventStepNumber: undefined,
            observedAt: fallbackStreamStartedAt,
          });
        }
        const stepEntries = [...state.stepStates.entries()];
        let firstError: Error | undefined;

        for (const [, stepState] of stepEntries) {
          let recorder = stepState.recorder;
          if (recorder === undefined) {
            try {
              recorder = ensureStepRecorder(stepState, stepState.conversation.conversationId);
            } catch (error) {
              if (firstError === undefined) {
                firstError = error instanceof Error ? error : new Error(String(error));
              }
              if (stepState.toolCallIds.size > 0) {
                closeStepTools(state, stepState, error);
              }
              continue;
            }
          }
          if (stepState.firstTokenAt !== undefined) {
            recorder.setFirstTokenAt(stepState.firstTokenAt);
          }
          try {
            recorder.setCallError(error);
          } finally {
            recorder.end();
          }
          const recorderError = recorder.getError();
          if (firstError === undefined && recorderError !== undefined) {
            firstError = recorderError;
          }
          if (stepState.toolCallIds.size > 0) {
            closeStepTools(state, stepState, error);
          }
        }

        state.stepStates.clear();
        if (state.toolStates.size > 0) {
          closeAllTools(state, error);
        }

        if (firstError !== undefined) {
          throw firstError;
        }
      };
      hooks.onChunk = (event) => {
        const observedAt = new Date();
        noteStreamObservedStartAt(observedAt);
        const { stepState } = resolveOrCreateStepStateForObservedEvent({
          eventStepNumber: event.stepNumber,
          observedAt,
        });
        if (!isTextChunk(event)) {
          return;
        }
        const recorder = stepState.recorder;
        if (recorder === undefined || stepState.firstTokenRecorded) {
          if (!stepState.firstTokenRecorded) {
            stepState.firstTokenRecorded = true;
            stepState.firstTokenAt = observedAt;
          }
          return;
        }
        stepState.firstTokenRecorded = true;
        stepState.firstTokenAt = undefined;
        recorder.setFirstTokenAt(observedAt);
      };

      hooks.onError = (event) => {
        finalizeStreamFailure(unwrapStreamError(event));
      };

      hooks.onAbort = (event) => {
        const abortError = unwrapStreamAbortError(event);
        finalizeStreamFailure(abortError);
      };
    }

    return hooks;
  }
}

function unwrapStreamError(event: unknown): unknown {
  if (event === null || typeof event !== 'object' || !('error' in event)) {
    return event;
  }
  return (event as { error?: unknown }).error;
}

function unwrapStreamAbortError(event: unknown): unknown {
  const unwrapped = unwrapStreamError(event);
  if (unwrapped === undefined || unwrapped === null) {
    return new Error('stream aborted');
  }
  if (unwrapped instanceof Error) {
    return unwrapped;
  }
  if (typeof unwrapped === 'string') {
    const trimmed = unwrapped.trim();
    return new Error(trimmed.length > 0 ? trimmed : 'stream aborted');
  }
  return new Error('stream aborted');
}

function closeStepTools(state: CallState, stepState: StepState, error: unknown): void {
  for (const toolCallId of stepState.toolCallIds) {
    const toolState = state.toolStates.get(toolCallId);
    if (toolState === undefined) {
      continue;
    }
    try {
      toolState.recorder.setCallError(error);
    } finally {
      toolState.recorder.end();
    }
    state.toolStates.delete(toolCallId);
    state.completedToolCallIds.add(toolCallId);
  }
  stepState.toolCallIds.clear();
}

function closeAllTools(state: CallState, error: unknown): void {
  const entries = [...state.toolStates.entries()];
  for (const [toolCallId, toolState] of entries) {
    try {
      toolState.recorder.setCallError(error);
    } finally {
      toolState.recorder.end();
    }
    state.toolStates.delete(toolCallId);
    state.completedToolCallIds.add(toolCallId);
    const stepState = state.stepStates.get(toolState.stepNumber);
    stepState?.toolCallIds.delete(toolCallId);
  }
}

function resolveStepNumberForEvent(state: CallState, eventStepNumber: unknown): number | undefined {
  const parsed = parseOptionalStepNumber(eventStepNumber);
  if (parsed !== undefined) {
    if (state.stepStates.has(parsed)) {
      return parsed;
    }
    return undefined;
  }

  if (state.stepStates.size !== 1) {
    return undefined;
  }

  const [only] = state.stepStates.keys();
  return only;
}

function parseOptionalStepNumber(value: unknown): number | undefined {
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || value < 0) {
      return undefined;
    }
    return Math.trunc(value);
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      return undefined;
    }
    const parsed = Number.parseInt(trimmed, 10);
    if (Number.isNaN(parsed) || parsed < 0) {
      return undefined;
    }
    return parsed;
  }
  return undefined;
}

function hasStepStartModelRef(event: StepStartEvent): boolean {
  const model = event.model;
  if (model === null || typeof model !== 'object') {
    return false;
  }
  const modelRef = model as Record<string, unknown>;
  return hasNonEmptyString(modelRef.modelId) || hasNonEmptyString(modelRef.id) || hasNonEmptyString(modelRef.name);
}

function hasNonEmptyString(value: unknown): boolean {
  return typeof value === 'string' && value.trim().length > 0;
}

function normalizeOptionalString(value: string | undefined): string | undefined {
  if (typeof value !== 'string') {
    return undefined;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : undefined;
}

function isPromiseLike<T>(value: unknown): value is PromiseLike<T> {
  return value !== null && typeof value === 'object' && typeof (value as { then?: unknown }).then === 'function';
}

type VercelAiSdkRole = VercelAiSdkModelMessage['role'];

function toVercelAiSdkModelMessages(messages: Message[]): VercelAiSdkModelMessage[] | undefined {
  const output: VercelAiSdkModelMessage[] = [];

  for (const message of messages) {
    const role = toVercelAiSdkRole(message.role);
    if (role === undefined) {
      return undefined;
    }
    const content = toVercelAiSdkMessageContent(role, message);
    if (content === undefined) {
      return undefined;
    }
    output.push({ role, content } as VercelAiSdkModelMessage);
  }

  return output.length > 0 ? output : undefined;
}

function toVercelAiSdkRole(role: string): VercelAiSdkRole | undefined {
  const normalized = role.trim().toLowerCase();
  if (normalized === 'system' || normalized === 'user' || normalized === 'assistant' || normalized === 'tool') {
    return normalized;
  }
  return undefined;
}

function toVercelAiSdkMessageContent(role: VercelAiSdkRole, message: Message): unknown | undefined {
  const textContent = normalizeOptionalString(message.content);
  if (message.parts === undefined || message.parts.length === 0) {
    return role === 'tool' ? undefined : textContent;
  }

  const parts: unknown[] = [];
  for (const part of message.parts) {
    if (part.type === 'text') {
      if (role === 'tool') {
        return undefined;
      }
      parts.push({ type: 'text', text: part.text });
      continue;
    }

    if (part.type === 'thinking') {
      if (role !== 'assistant') {
        return undefined;
      }
      parts.push({ type: 'reasoning', text: part.thinking });
      continue;
    }

    if (part.type === 'tool_call') {
      if (role !== 'assistant') {
        return undefined;
      }
      const toolCallId = normalizeOptionalString(part.toolCall.id);
      if (toolCallId === undefined) {
        return undefined;
      }
      parts.push({
        type: 'tool-call',
        toolCallId,
        toolName: part.toolCall.name,
        input: parseOptionalJSON(part.toolCall.inputJSON) ?? {},
      });
      continue;
    }

    if (part.type === 'tool_result') {
      if (role !== 'assistant' && role !== 'tool') {
        return undefined;
      }
      const toolCallId = normalizeOptionalString(part.toolResult.toolCallId);
      const toolName = normalizeOptionalString(part.toolResult.name);
      if (toolCallId === undefined || toolName === undefined) {
        return undefined;
      }
      parts.push({
        type: 'tool-result',
        toolCallId,
        toolName,
        output: toVercelAiSdkToolOutput(part.toolResult.content, part.toolResult.contentJSON, part.toolResult.isError),
      });
    }
  }

  if (role === 'system') {
    const text = parts
      .map((part) => (isTextPart(part) ? part.text.trim() : ''))
      .filter((text) => text.length > 0)
      .join(' ')
      .trim();
    return text.length > 0 ? text : textContent;
  }

  return parts.length > 0 ? parts : textContent;
}

function toVercelAiSdkToolOutput(
  content: string | undefined,
  contentJSON: string | undefined,
  isError: boolean | undefined,
): unknown {
  const parsedJSON = parseOptionalJSON(contentJSON);
  if (parsedJSON !== undefined) {
    return {
      type: isError ? 'error-json' : 'json',
      value: parsedJSON,
    };
  }

  return {
    type: isError ? 'error-text' : 'text',
    value: content ?? '',
  };
}

function parseOptionalJSON(value: string | undefined): unknown | undefined {
  if (value === undefined) {
    return undefined;
  }
  try {
    return JSON.parse(value);
  } catch {
    return undefined;
  }
}

function isTextPart(value: unknown): value is { type: 'text'; text: string } {
  if (value === null || typeof value !== 'object') {
    return false;
  }
  const part = value as { type?: unknown; text?: unknown };
  return part.type === 'text' && typeof part.text === 'string';
}
