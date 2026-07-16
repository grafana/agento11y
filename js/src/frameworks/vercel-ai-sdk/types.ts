import type { Message } from '../../types.js';

type HookResult = void | Promise<void>;

export interface StepStartModelRef {
  provider?: unknown;
  modelId?: unknown;
  id?: unknown;
  name?: unknown;
}

export interface StepStartEvent {
  stepNumber?: unknown;
  messages?: unknown;
  model?: StepStartModelRef;
  /**
   * Output configuration the AI SDK passes through (e.g. `Output.object(...)`).
   * Not declared on the public AI SDK type — surfaced at runtime on the step-start
   * event so adapters can capture the response-format schema.
   */
  output?: unknown;
}

export interface PrepareStepEvent extends StepStartEvent {
  steps?: unknown;
  experimental_context?: unknown;
}

export type VercelAiSdkModelMessage =
  | { role: 'system'; content: any; providerOptions?: Record<string, unknown> }
  | { role: 'user'; content: any; providerOptions?: Record<string, unknown> }
  | { role: 'assistant'; content: any; providerOptions?: Record<string, unknown> }
  | { role: 'tool'; content: any; providerOptions?: Record<string, unknown> };

export type PrepareStepResult =
  | {
      messages?: VercelAiSdkModelMessage[];
    }
  | undefined;

type PrepareStepHookResult = PrepareStepResult | PromiseLike<PrepareStepResult>;

export interface StepFinishResponseRef {
  id?: unknown;
  modelId?: unknown;
}

export interface StepFinishUsageTokenDetails {
  cacheReadTokens?: unknown;
  cacheWriteTokens?: unknown;
  cacheCreationTokens?: unknown;
}

export interface StepFinishUsageOutputTokenDetails {
  reasoningTokens?: unknown;
}

export interface StepFinishUsage {
  inputTokens?: unknown;
  outputTokens?: unknown;
  totalTokens?: unknown;
  promptTokens?: unknown;
  completionTokens?: unknown;
  inputTokenDetails?: StepFinishUsageTokenDetails;
  outputTokenDetails?: StepFinishUsageOutputTokenDetails;
}

export interface StepFinishEvent {
  stepNumber?: unknown;
  stepType?: unknown;
  finishReason?: unknown;
  text?: unknown;
  reasoningText?: unknown;
  usage?: StepFinishUsage;
  response?: StepFinishResponseRef;
  modelId?: unknown;
  toolCalls?: unknown;
  toolResults?: unknown;
  error?: unknown;
}

export interface ToolCallRef {
  toolCallId?: unknown;
  toolName?: unknown;
  input?: unknown;
  type?: unknown;
  description?: unknown;
}

export interface ToolCallStartEvent {
  stepNumber?: unknown;
  toolCall?: ToolCallRef;
}

export interface ToolCallFinishEvent {
  stepNumber?: unknown;
  toolCall?: ToolCallRef;
  success?: unknown;
  output?: unknown;
  error?: unknown;
  durationMs?: unknown;
}

export interface StreamChunkEvent {
  stepNumber?: unknown;
  type?: unknown;
  text?: unknown;
  chunk?: {
    type?: unknown;
    text?: unknown;
  };
}

export interface StreamErrorEvent {
  error?: unknown;
}

export interface SigilVercelAiSdkOptions {
  agentName?: string;
  agentVersion?: string;
  captureInputs?: boolean;
  captureOutputs?: boolean;
  extraTags?: Record<string, string>;
  extraMetadata?: Record<string, unknown>;
  resolveConversationId?: (event: StepStartEvent) => string | undefined;
  /**
   * Override client-level hook enablement for this instrumentation.
   *
   * When `true`, preflight hooks run on each LLM step even if
   * `client.config.hooks.enabled` is false. When `false`, hooks never run for
   * calls made through this instrumentation. When `undefined` (default), the
   * client's `hooks.enabled` value decides.
   */
  enableHooks?: boolean;
}

export interface CallOptions {
  conversationId?: string;
  agentName?: string;
  extraMetadata?: Record<string, unknown>;
}

export interface GenerateTextHooks {
  prepareStep?: (event: PrepareStepEvent) => PrepareStepHookResult;
  experimental_onStepStart?: (event: StepStartEvent) => HookResult;
  onStepFinish?: (event: StepFinishEvent) => HookResult;
  experimental_onToolCallStart?: (event: ToolCallStartEvent) => HookResult;
  experimental_onToolCallFinish?: (event: ToolCallFinishEvent) => HookResult;
}

export interface StreamTextHooks extends GenerateTextHooks {
  onChunk?: (event: StreamChunkEvent) => HookResult;
  onError?: (event: StreamErrorEvent | unknown) => HookResult;
  onAbort?: (event?: unknown) => HookResult;
}

export interface ParsedToolCall {
  id?: string;
  name: string;
  inputJSON?: string;
}

export interface ParsedToolResult {
  toolCallId?: string;
  name?: string;
  content?: string;
  contentJSON?: string;
  isError?: boolean;
}

export interface ConversationResolution {
  conversationId: string;
  source: 'explicit' | 'resolver' | 'fallback';
}

export interface StepOutputMapping {
  output: Message[] | undefined;
  reasoningText: string | undefined;
  stepType: string | undefined;
}
