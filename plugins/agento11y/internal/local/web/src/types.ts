// The JSON the daemon sends, as the viewer reads it.
//
// These are hand-written from the Go structs in internal/local (query.go,
// history.go, search.go, settings.go, forward.go) and from the model package
// the SDK shares. Nothing checks the two against each other, so a renamed
// `json:` tag shows up here as a field the viewer reads and never finds.
//
// Optional fields are the ones whose Go tag carries `omitempty`: the daemon
// leaves them out of the object rather than sending a zero. Timestamps are
// RFC 3339 strings.

/** TokenBuckets in query.go: one generation's usage, split into disjoint parts. */
export interface TokenBuckets {
  fresh_input: number;
  cache_read: number;
  cache_write: number;
  output: number;
  reasoning: number;
}

/** One bucket name, for code that indexes a breakdown by key. */
export type TokenBucketKey = keyof TokenBuckets;

/** ConversationSummary in query.go: one row of GET /api/v1/conversations. */
export interface ConversationSummary {
  id: string;
  title?: string;
  started_at: string;
  last_activity: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  token_buckets: TokenBuckets;
  token_buckets_by_model?: Record<string, TokenBuckets>;
  agents: string[];
  models: string[];
  /** "ok", or "err" when a generation in the conversation recorded a call error. */
  status: string;
  workspace?: string;
  branch?: string;
  subagents?: number;
}

export interface ConversationListResponse {
  conversations: ConversationSummary[];
  total_conversations: number;
}

export interface ConversationMetricsAggregate {
  calls: number;
  errored: number;
  agents: number;
  workspaces: number;
  token_buckets: TokenBuckets;
  token_buckets_by_model: Record<string, TokenBuckets>;
  models: string[];
}

export interface ConversationMetricsResponse {
  aggregate: ConversationMetricsAggregate;
  conversations: ConversationSummary[];
  matched_conversations: number;
}

export type MessageRole = 'user' | 'assistant' | 'tool' | 'system' | string;

export type PartKind = 'text' | 'thinking' | 'tool_call' | 'tool_result' | 'media' | string;

export interface ToolCall {
  id?: string;
  name: string;
  /** Raw JSON, as recorded. The viewer renders it, so it is not decoded here. */
  input_json?: unknown;
}

export interface ToolResult {
  tool_call_id?: string;
  name?: string;
  is_error?: boolean;
  content?: string;
  content_json?: unknown;
}

/** go/agento11y/model/message.go:60. The daemon serves it through GenerationView.Messages. */
interface Media {
  kind?: string;
  url?: string;
  mime_type?: string;
  name?: string;
}

export interface Part {
  kind?: PartKind;
  text?: string;
  thinking?: string;
  tool_call?: ToolCall;
  tool_result?: ToolResult;
  media?: Media;
  metadata?: { provider_type?: string };
}

export interface Message {
  role: MessageRole;
  name?: string;
  parts?: Part[];
}

/** SkillView in query.go: one skill a generation loaded. */
interface SkillView {
  name: string;
  id?: string;
  description?: string;
  version?: string;
}

/** GenerationView in query.go: one step of a conversation thread. */
export interface Generation {
  generation_id: string;
  agent_name?: string;
  model?: string;
  provider?: string;
  started_at: string;
  completed_at: string;
  duration_seconds: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  token_buckets: TokenBuckets;
  messages?: Message[];
  input?: Message[];
  output?: Message[];
  tools?: string[];
  tool_preview?: string;
  skills?: SkillView[];
  stop_reason?: string;
  call_error?: string;
  parent_generation_ids?: string[];
  thinking_enabled?: boolean;
}

/** ConversationDetail in query.go: GET /api/v1/conversations/{id}. */
export interface ConversationDetail {
  id: string;
  title?: string;
  generations: Generation[];
}

/** TokenUsagePoint in query.go: the buckets flatten into the object. */
export interface TokenUsagePoint extends TokenBuckets {
  t: string;
  model?: string;
  provider?: string;
  calls: number;
}

export interface TokenUsageResponse {
  points: TokenUsagePoint[];
  interval_seconds: number;
}

export interface ToolUsage {
  name: string;
  calls: number;
  failures: number;
}

export interface ConversationToolUsage {
  id: string;
  tools: ToolUsage[];
}

export interface ToolUsageResponse {
  conversations: ConversationToolUsage[];
}

/** SearchHit in search.go: one row of GET /api/v1/search. */
export interface SearchHit {
  id: string;
  title?: string;
  agents: string[];
  models: string[];
  last_activity: string;
  total_tokens: number;
  calls: number;
  status: string;
  snippet?: string;
  match_count: number;
  generation_id?: string;
  token_buckets: TokenBuckets;
}

export interface SearchResponse {
  hits: SearchHit[];
  mode: string;
}

/** Tag in settings.go: one key/value pair written to config.env. */
export interface Tag {
  key: string;
  value: string;
}

/**
 * Settings in settings.go: the config.env subset the Settings page edits.
 *
 * token and otlpHeaders are write-only. The daemon never sends a stored
 * secret back, so on read they are empty and tokenSet / otlpHeadersSet report
 * whether one exists on disk. On write the pair is tri-state: a value
 * replaces, a `Cleared` flag removes, and neither leaves the stored secret
 * alone.
 */
export interface Settings {
  endpoint: string;
  tenantId: string;
  otlpEndpoint: string;
  tokenSet: boolean;
  token: string;
  tokenCleared: boolean;
  otlpHeaders: string;
  otlpHeadersSet: boolean;
  otlpHeadersCleared: boolean;
  capture: string;
  tags: Tag[];
  guards: string;
  guardTimeout: string;
  debug: boolean;
  autoUpdate: boolean;
  userId: string;
  localForward: boolean;
}

/** forwardFailure in forward.go: one forward attempt that did not deliver. */
interface ForwardFailure {
  at: string;
  label: string;
  detail: string;
}

/** forwardStatus in forward.go: what the daemon would actually send to Cloud. */
export interface ForwardStatus {
  enabled: boolean;
  /** "off", "metadata_only" or "full". */
  mode: string;
  generations: boolean;
  otlp: boolean;
  hooks: boolean;
  reason?: string;
  otlpReason?: string;
  hookReason?: string;
  failures?: ForwardFailure[];
}

/** configResponse in server.go: GET /api/v1/config. */
export interface ConfigResponse {
  settings: Settings;
  preview: string;
  path: string;
  /** The stack `agento11y login` was pointed at. Read-only; a save never writes it. */
  stackUrl: string;
  forwardStatus: ForwardStatus | null;
}

/** One registered importer, from GET /api/v1/history/agents. */
export interface HistoryAgent {
  id: string;
  display_name: string;
  aliases: string[];
}

/** historyOffer in history.go: whether an agent has importable history. */
export interface HistoryOffer {
  agent: string;
  display_name: string;
  sessions: number;
  turns: number;
  approx_turns: boolean;
  show: boolean;
}

/** historySessionJSON in history.go: one discovered session, with no text. */
interface HistorySession {
  session_id: string;
  title: string;
  workspace: string;
  source_path: string;
  turn_count: number;
  approx_turns: boolean;
  size_bytes: number;
  started_at?: string;
  last_activity_at?: string;
  active: boolean;
}

interface HistorySkipped {
  session_id: string;
  reason: string;
}

/** GET /api/v1/history/plan. */
export interface HistoryPlan {
  agent: string;
  since: string;
  until: string;
  sessions: HistorySession[];
  skipped: HistorySkipped[];
  warnings: string[];
}

/** ImportRun in history.go: the state of one backfill. */
export interface ImportRun {
  run_id: string;
  agent: string;
  /** "pending", "running", "completed", "failed" or "cancelled". */
  status: string;
  discovered: number;
  selected: number;
  sessions: number;
  imported: number;
  skipped: number;
  failed: number;
  missing: number;
  started_at: string;
  finished_at?: string;
  error?: string;
}

/**
 * One model's prices, in dollars per million tokens, as models.dev serves
 * them. This is the one shape here that does not come from the daemon.
 */
export interface ModelCost {
  input?: number;
  output?: number;
  cache_read?: number;
  cache_write?: number;
  reasoning?: number;
}

/** The price table the viewer builds from models.dev, keyed by model id. */
export type ModelPrices = Record<string, ModelCost>;
