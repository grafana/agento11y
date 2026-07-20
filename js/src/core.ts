export {
  CACHE_DIAGNOSTICS_MISS_REASON_KEY,
  CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
  CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
  setCacheDiagnostics,
} from './cache-diagnostics.js';
export { Agento11yClient } from './client.js';
export { configFromEnv, defaultConfig, mergeConfig } from './config.js';
export {
  agentNameFromContext,
  agentVersionFromContext,
  conversationIdFromContext,
  conversationTitleFromContext,
  userIdFromContext,
  withAgentName,
  withAgentVersion,
  withConversationId,
  withConversationTitle,
  withUserId,
} from './context.js';
export { HookDeniedError } from './hooks.js';
export type { SecretRedactionOptions } from './redaction.js';
export { createSecretRedactionSanitizer } from './redaction.js';
export type {
  Agento11yDebugSnapshot,
  Agento11yLogger,
  Agento11ySdkConfig,
  Agento11ySdkConfigInput,
  ApiConfig,
  Artifact,
  ContentCaptureMode,
  ContentCaptureResolver,
  ConversationRating,
  ConversationRatingInput,
  ConversationRatingSummary,
  ConversationRatingValue,
  EmbeddingCaptureConfig,
  EmbeddingRecorder,
  EmbeddingResult,
  EmbeddingStart,
  ExecuteToolCallsOptions,
  ExportGenerationResult,
  ExportGenerationsRequest,
  ExportGenerationsResponse,
  ExportWorkflowStepResult,
  ExportWorkflowStepsRequest,
  ExportWorkflowStepsResponse,
  Generation,
  GenerationExportConfig,
  GenerationExporter,
  GenerationExportProtocol,
  GenerationMode,
  GenerationRecorder,
  GenerationResult,
  GenerationSanitizer,
  GenerationStart,
  HookAction,
  HookContext,
  HookEvaluateRequest,
  HookEvaluateResponse,
  HookEvaluation,
  HookInput,
  HookModel,
  HookPhase,
  HooksConfig,
  Message,
  MessagePart,
  ModelRef,
  PartMetadata,
  RecorderCallback,
  SubmitConversationRatingResponse,
  TokenUsage,
  ToolCallPart,
  ToolDefinition,
  ToolExecution,
  ToolExecutionRecorder,
  ToolExecutionResult,
  ToolExecutionStart,
  ToolResultPart,
  WorkflowStep,
} from './types.js';
export { SDK_VERSION, userAgent } from './version.js';

import { Agento11yClient } from './client.js';
import type { Agento11ySdkConfigInput } from './types.js';

/** Convenience factory equivalent to `new Agento11yClient(config)`. */
export function createAgento11yClient(config: Agento11ySdkConfigInput = {}): Agento11yClient {
  return new Agento11yClient(config);
}
