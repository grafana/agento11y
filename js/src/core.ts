export {
  CACHE_DIAGNOSTICS_MISS_REASON_KEY,
  CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
  CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
  setCacheDiagnostics,
} from './cache-diagnostics.js';
export { SigilClient } from './client.js';
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
  SigilDebugSnapshot,
  SigilLogger,
  SigilSdkConfig,
  SigilSdkConfigInput,
  SubmitConversationRatingResponse,
  TokenUsage,
  ToolCallPart,
  ToolDefinition,
  ToolExecution,
  ToolExecutionRecorder,
  ToolExecutionResult,
  ToolExecutionStart,
  ToolResultPart,
} from './types.js';
export { SDK_VERSION, userAgent } from './version.js';

import { SigilClient } from './client.js';
import type { SigilSdkConfigInput } from './types.js';

/** Convenience factory equivalent to `new SigilClient(config)`. */
export function createSigilClient(config: SigilSdkConfigInput = {}): SigilClient {
  return new SigilClient(config);
}
