import type { Meter, Tracer } from '@opentelemetry/api';

/** Supported generation export protocols. */
export type GenerationExportProtocol = 'grpc' | 'http' | 'none';
/** Generation execution mode. */
export type GenerationMode = 'SYNC' | 'STREAM';
/** Supported auth modes for transport exports. */
export type ExportAuthMode = 'none' | 'tenant' | 'bearer' | 'basic';

/**
 * Controls what content is included in exported generation payloads and
 * OTel span attributes.
 *
 * - `'default'` — inherits from the parent or client-level default (resolves
 *   to `'no_tool_content'` at the client level for backward compatibility).
 * - `'full'` — exports all content.
 * - `'no_tool_content'` — exports full generation content but excludes tool
 *   execution content (arguments and results) from span attributes unless
 *   explicitly opted in via `includeContent` or a per-tool `contentCapture`
 *   override.
 * - `'metadata_only'` — preserves message structure, tool names, usage, and
 *   timing but strips text, tool arguments, tool results, thinking, system
 *   prompts, and raw artifacts. User-provided `metadata` and `tags` are NOT
 *   stripped — callers are responsible for ensuring these do not contain
 *   sensitive content.
 */
export type ContentCaptureMode = 'default' | 'full' | 'no_tool_content' | 'metadata_only';

/**
 * Callback invoked before each generation or tool execution to dynamically
 * resolve the content capture mode. Receives the recording's metadata (or
 * `undefined` when the recording type has no metadata, e.g. tool executions).
 *
 * Returning `'default'` defers to `Config.contentCapture`. Thrown errors are
 * caught and treated as `'metadata_only'` (fail-closed).
 */
export type ContentCaptureResolver = (metadata: Record<string, unknown> | undefined) => ContentCaptureMode;

/** Per-export auth configuration. */
export interface ExportAuthConfig {
  mode: ExportAuthMode;
  tenantId?: string;
  bearerToken?: string;
  /** Username for basic auth. When empty, tenantId is used. */
  basicUser?: string;
  /** Password/token for basic auth. */
  basicPassword?: string;
}

/** Generation exporter runtime configuration. */
export interface GenerationExportConfig {
  /** Export protocol for generations. */
  protocol: GenerationExportProtocol;
  /** Generation ingest endpoint. */
  endpoint: string;
  /** Optional transport headers/metadata. */
  headers?: Record<string, string>;
  /** Optional auth mode for generation transport. */
  auth: ExportAuthConfig;
  /** Force insecure transport when protocol allows it. */
  insecure: boolean;
  /** Max generations per export request. */
  batchSize: number;
  /** Periodic drain interval for queued generations. */
  flushIntervalMs: number;
  /** Max queued generations before enqueue errors. */
  queueSize: number;
  /** Max retry attempts after first failed export. */
  maxRetries: number;
  /** Initial backoff between retry attempts. */
  initialBackoffMs: number;
  /** Max backoff cap between retry attempts. */
  maxBackoffMs: number;
  /** Optional per-generation max encoded payload size in bytes. */
  payloadMaxBytes: number;
}

/** Sigil HTTP API settings used by non-ingest helper endpoints. */
export interface ApiConfig {
  /** Sigil API base endpoint, for example `http://localhost:8080`. */
  endpoint: string;
}

/** Embedding input-capture settings for span attributes. */
export interface EmbeddingCaptureConfig {
  /** Enables `gen_ai.embeddings.input_texts` capture on spans. */
  captureInput: boolean;
  /** Max number of input texts captured per embedding call. */
  maxInputItems: number;
  /** Max characters captured per text entry before truncation. */
  maxTextLength: number;
}

/** Optional logger hooks used by the SDK runtime. */
export interface SigilLogger {
  debug?: (message: string, ...args: unknown[]) => void;
  warn?: (message: string, ...args: unknown[]) => void;
  error?: (message: string, ...args: unknown[]) => void;
}

/** Per-generation ingest result. */
export interface ExportGenerationResult {
  generationId: string;
  accepted: boolean;
  error?: string;
}

/** Generation export request payload. */
export interface ExportGenerationsRequest {
  generations: Generation[];
}

/** Generation export response payload. */
export interface ExportGenerationsResponse {
  results: ExportGenerationResult[];
}

/** Allowed conversation rating values. */
export type ConversationRatingValue = 'CONVERSATION_RATING_VALUE_GOOD' | 'CONVERSATION_RATING_VALUE_BAD';

/** SDK input for submitting a conversation rating. */
export interface ConversationRatingInput {
  ratingId: string;
  rating: ConversationRatingValue;
  comment?: string;
  metadata?: Record<string, unknown>;
  generationId?: string;
  raterId?: string;
  source?: string;
}

/** Conversation rating event returned by Sigil. */
export interface ConversationRating {
  ratingId: string;
  conversationId: string;
  generationId?: string;
  rating: ConversationRatingValue;
  comment?: string;
  metadata?: Record<string, unknown>;
  raterId?: string;
  source?: string;
  createdAt: string;
}

/** Aggregated rating summary returned by Sigil. */
export interface ConversationRatingSummary {
  totalCount: number;
  goodCount: number;
  badCount: number;
  latestRating?: ConversationRatingValue;
  latestRatedAt: string;
  latestBadAt?: string;
  hasBadRating: boolean;
}

/** Rating create response envelope returned by Sigil. */
export interface SubmitConversationRatingResponse {
  rating: ConversationRating;
  summary: ConversationRatingSummary;
}

/** Pluggable generation exporter interface. */
export interface GenerationExporter {
  exportGenerations(request: ExportGenerationsRequest): Promise<ExportGenerationsResponse>;
  shutdown?(): Promise<void> | void;
}

/**
 * Callback invoked after a generation has been normalized and content-capture
 * policy has been applied, but before span synchronization, validation, debug
 * snapshots, and export. Implementations should return the sanitized
 * generation. Thrown errors are caught and treated as `'metadata_only'`
 * stripping (fail-closed).
 */
export type GenerationSanitizer = (generation: Generation) => Generation;

/** Fully resolved SDK configuration. */
export interface SigilSdkConfig {
  generationExport: GenerationExportConfig;
  api: ApiConfig;
  embeddingCapture: EmbeddingCaptureConfig;
  hooks: HooksConfig;
  /**
   * Default content capture mode for all generations and tool executions.
   * Per-recording overrides take precedence. Defaults to `'no_tool_content'`.
   */
  contentCapture: ContentCaptureMode;
  /**
   * When set, called before each generation and tool execution to dynamically
   * resolve the content capture mode.
   *
   * Resolution precedence (highest to lowest):
   *   1. Per-recording `contentCapture` field
   *   2. `contentCaptureResolver` return value
   *   3. `contentCapture` (static default)
   *
   * Thrown errors are caught and treated as `'metadata_only'` (fail-closed).
   */
  contentCaptureResolver?: ContentCaptureResolver;
  generationSanitizer?: GenerationSanitizer;
  generationExporter?: GenerationExporter;
  tracer?: Tracer;
  meter?: Meter;
  logger?: SigilLogger;
  now?: () => Date;
  sleep?: (durationMs: number) => Promise<void>;
  /**
   * Default agent name applied to GenerationStart / EmbeddingStart /
   * ToolExecutionStart when the per-call value is empty. Read from
   * `SIGIL_AGENT_NAME` automatically by `new SigilClient()`.
   */
  agentName?: string;
  /** Default agent version. Read from `SIGIL_AGENT_VERSION`. */
  agentVersion?: string;
  /** Default user identifier. Read from `SIGIL_USER_ID`. */
  userId?: string;
  /** Default tags merged into every generation; per-call tags win. Read from `SIGIL_TAGS` (CSV). */
  tags?: Record<string, string>;
  /**
   * When true, signals to downstream consumers (plugins, telemetry) that the
   * SDK is running in verbose mode. Read from `SIGIL_DEBUG`. The SDK does not
   * currently change its own logger based on this flag — plugins layer their
   * own log-file plumbing on top of it.
   */
  debug?: boolean;
}

/** Partial SDK configuration passed by callers. */
export interface SigilSdkConfigInput {
  generationExport?: Partial<GenerationExportConfig>;
  api?: Partial<ApiConfig>;
  embeddingCapture?: Partial<EmbeddingCaptureConfig>;
  hooks?: Partial<HooksConfig>;
  contentCapture?: ContentCaptureMode;
  contentCaptureResolver?: ContentCaptureResolver;
  generationSanitizer?: GenerationSanitizer;
  generationExporter?: GenerationExporter;
  tracer?: Tracer;
  meter?: Meter;
  logger?: SigilLogger;
  now?: () => Date;
  sleep?: (durationMs: number) => Promise<void>;
  agentName?: string;
  agentVersion?: string;
  userId?: string;
  tags?: Record<string, string>;
  debug?: boolean;
}

/** Provider/model identity. */
export interface ModelRef {
  provider: string;
  name: string;
}

/** Tool definition visible to the model. */
export interface ToolDefinition {
  name: string;
  description?: string;
  type?: string;
  /** JSON schema string for tool input; mapped to `input_schema_json` on gRPC export. */
  inputSchemaJSON?: string;
}

/** Token usage counters. */
export interface TokenUsage {
  inputTokens?: number;
  outputTokens?: number;
  totalTokens?: number;
  cacheReadInputTokens?: number;
  cacheWriteInputTokens?: number;
  reasoningTokens?: number;
}

/** Provider-specific metadata attached to message parts. */
export interface PartMetadata {
  providerType?: string;
}

/** Tool call part payload. */
export interface ToolCallPart {
  id?: string;
  name: string;
  inputJSON?: string;
}

/** Tool result part payload. */
export interface ToolResultPart {
  toolCallId?: string;
  name?: string;
  content?: string;
  contentJSON?: string;
  isError?: boolean;
}

/** Typed message part union. */
export type MessagePart =
  | { type: 'text'; text: string; metadata?: PartMetadata }
  | { type: 'thinking'; thinking: string; metadata?: PartMetadata }
  | { type: 'tool_call'; toolCall: ToolCallPart; metadata?: PartMetadata }
  | { type: 'tool_result'; toolResult: ToolResultPart; metadata?: PartMetadata };

/** Normalized message payload. */
export interface Message {
  /** Role value: `user`, `assistant`, or `tool`. */
  role: string;
  /** Optional text shorthand; mapped to a text part for gRPC export. */
  content?: string;
  name?: string;
  /** Preferred typed part representation. */
  parts?: MessagePart[];
}

/** Optional raw provider artifact. */
export interface Artifact {
  /** Artifact kind: `request`, `response`, `tools`, or `provider_event`. */
  type: string;
  name?: string;
  payload?: string;
  mimeType?: string;
  recordId?: string;
  uri?: string;
}

/** Generation start seed fields. */
export interface GenerationStart {
  id?: string;
  conversationId?: string;
  conversationTitle?: string;
  userId?: string;
  agentName?: string;
  agentVersion?: string;
  mode?: GenerationMode;
  operationName?: string;
  model: ModelRef;
  systemPrompt?: string;
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingEnabled?: boolean;
  parentGenerationIds?: string[];
  effectiveVersion?: string;
  tools?: ToolDefinition[];
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  startedAt?: Date;
  /** Per-generation content capture override. */
  contentCapture?: ContentCaptureMode;
}

/** Final generation result fields. */
export interface GenerationResult {
  conversationId?: string;
  conversationTitle?: string;
  userId?: string;
  agentName?: string;
  agentVersion?: string;
  operationName?: string;
  responseId?: string;
  responseModel?: string;
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingEnabled?: boolean;
  parentGenerationIds?: string[];
  /** See {@link GenerationStart.effectiveVersion}. */
  effectiveVersion?: string;
  input?: Message[];
  output?: Message[];
  tools?: ToolDefinition[];
  usage?: TokenUsage;
  stopReason?: string;
  completedAt?: Date;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  artifacts?: Artifact[];
}

/** Embedding start seed fields. */
export interface EmbeddingStart {
  model: ModelRef;
  agentName?: string;
  agentVersion?: string;
  dimensions?: number;
  encodingFormat?: string;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  startedAt?: Date;
}

/** Final embedding result fields. */
export interface EmbeddingResult {
  inputCount: number;
  inputTokens?: number;
  inputTexts?: string[];
  responseModel?: string;
  dimensions?: number;
}

/** Fully normalized generation record exported by the SDK. */
export interface Generation {
  id: string;
  conversationId?: string;
  conversationTitle?: string;
  userId?: string;
  agentName?: string;
  agentVersion?: string;
  mode: GenerationMode;
  operationName: string;
  traceId?: string;
  spanId?: string;
  model: ModelRef;
  systemPrompt?: string;
  responseId?: string;
  responseModel?: string;
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingEnabled?: boolean;
  parentGenerationIds?: string[];
  /** See {@link GenerationStart.effectiveVersion}. */
  effectiveVersion?: string;
  input?: Message[];
  output?: Message[];
  tools?: ToolDefinition[];
  usage?: TokenUsage;
  stopReason?: string;
  startedAt: Date;
  completedAt: Date;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
  artifacts?: Artifact[] | null;
  callError?: string;
}

/** Tool execution start seed fields. */
export interface ToolExecutionStart {
  toolName: string;
  toolCallId?: string;
  toolType?: string;
  toolDescription?: string;
  conversationId?: string;
  conversationTitle?: string;
  agentName?: string;
  agentVersion?: string;
  /** The model that requested the tool call (e.g. "gpt-5"). */
  requestModel?: string;
  /** The provider that served the model (e.g. "openai"). */
  requestProvider?: string;
  /**
   * @deprecated Use `contentCapture` instead. When `contentCapture` is
   * `'no_tool_content'` (the default), this field controls whether tool
   * arguments and results are included in span attributes.
   */
  includeContent?: boolean;
  /** Per-tool content capture override. */
  contentCapture?: ContentCaptureMode;
  startedAt?: Date;
}

/** Options for {@link SigilClient.executeToolCalls}. */
export interface ExecuteToolCallsOptions {
  conversationId?: string;
  conversationTitle?: string;
  agentName?: string;
  agentVersion?: string;
  contentCapture?: ContentCaptureMode;
  requestModel?: string;
  requestProvider?: string;
  toolType?: string;
  /** Reserved for forward compatibility; not applied to tool spans in this release. */
  tags?: Record<string, string>;
}

/** Tool execution completion fields. */
export interface ToolExecutionResult {
  arguments?: unknown;
  result?: unknown;
  completedAt?: Date;
}

/** Final tool execution record kept in debug snapshots. */
export interface ToolExecution {
  toolName: string;
  toolCallId?: string;
  toolType?: string;
  toolDescription?: string;
  conversationId?: string;
  conversationTitle?: string;
  agentName?: string;
  agentVersion?: string;
  requestModel?: string;
  requestProvider?: string;
  includeContent: boolean;
  startedAt: Date;
  completedAt: Date;
  arguments?: unknown;
  result?: unknown;
  callError?: string;
}

/** Recorder API for embedding lifecycle. */
export interface EmbeddingRecorder {
  setResult(result: EmbeddingResult): void;
  setCallError(error: unknown): void;
  end(): void;
  getError(): Error | undefined;
}

/** Recorder API for generation lifecycle. */
export interface GenerationRecorder {
  setResult(result: GenerationResult): void;
  setCallError(error: unknown): void;
  setFirstTokenAt(firstTokenAt: Date): void;
  setCacheDiagnostics(missReason: string, opts?: { missedInputTokens?: number; previousMessageId?: string }): void;
  end(): void;
  getError(): Error | undefined;
}

/** Recorder API for tool execution lifecycle. */
export interface ToolExecutionRecorder {
  setResult(result: ToolExecutionResult): void;
  setCallError(error: unknown): void;
  end(): void;
  getError(): Error | undefined;
}

/** In-memory snapshot for tests/debugging. */
export interface SigilDebugSnapshot {
  generations: Generation[];
  toolExecutions: ToolExecution[];
  queueSize: number;
}

/** Callback form used by recorder helper APIs. */
export type RecorderCallback<TRecorder, TResult> = (recorder: TRecorder) => TResult | Promise<TResult>;

/** Shared recorder methods used by callback runtime helpers. */
export interface RecorderWithError {
  setCallError(error: unknown): void;
  end(): void;
  getError(): Error | undefined;
}

/** Phase at which a hook rule is evaluated. */
export type HookPhase = 'preflight' | 'postflight';

/** Hook evaluation action returned by the server. */
export type HookAction = 'allow' | 'deny';

/** Runtime configuration for synchronous hook evaluation. */
export interface HooksConfig {
  /** Master switch. When false, `evaluateHook` short-circuits to `allow`. */
  enabled: boolean;
  /** Phases the SDK is allowed to evaluate. Defaults to `['preflight']`. */
  phases: HookPhase[];
  /** Per-request timeout in milliseconds. Defaults to 15000. */
  timeoutMs: number;
  /**
   * When true, network/timeout failures resolve to `action: 'allow'` so the
   * caller can proceed with the LLM call. Defaults to true.
   */
  failOpen: boolean;
}

/** Identifies the upstream LLM call for hook rule matching. */
export interface HookModel {
  provider: string;
  name: string;
}

/** Routing/matching context attached to a hook evaluation request. */
export interface HookContext {
  agentName?: string;
  agentVersion?: string;
  model: HookModel;
  tags?: Record<string, string>;
}

/**
 * Payload describing the request (preflight) or request+response (postflight)
 * to be evaluated by hook rules.
 */
export interface HookInput {
  messages?: Message[];
  tools?: ToolDefinition[];
  systemPrompt?: string;
  /** Output messages, only meaningful for postflight evaluation. */
  output?: Message[];
  /** Plain-text fallback when structured messages are unavailable. */
  conversationPreview?: string;
}

/** Hook evaluation request body. */
export interface HookEvaluateRequest {
  phase: HookPhase;
  context: HookContext;
  input: HookInput;
}

/** Per-rule outcome reported by the server. */
export interface HookEvaluation {
  ruleId: string;
  evaluatorId: string;
  evaluatorKind: string;
  passed: boolean;
  latencyMs: number;
  explanation?: string;
  reason?: string;
}

/** Hook evaluation response body. */
export interface HookEvaluateResponse {
  action: HookAction;
  ruleId?: string;
  reason?: string;
  /**
   * When the server applied hook transform rules, the redacted/sanitized
   * payload to use instead of the original request input.
   */
  transformedInput?: HookInput;
  evaluations: HookEvaluation[];
}
