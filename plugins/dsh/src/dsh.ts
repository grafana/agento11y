/**
 * Structural mirrors of dsh 0.2.1-rc.2 (687f282e8625).
 * The managed bundle cannot resolve dsh profile dependencies at runtime.
 * Local mirrors avoid runtime imports; real-host tests check the LLM boundary.
 */

/**
 * Mirrors `TokenUsage` in `packages/llm/llm/src/types.ts`.
 * Input counters are disjoint: `inputTokens` is uncached input. Aggregate
 * input is `inputTokens + cacheReadTokens + cacheWriteTokens`. `totalTokens`
 * is the provider's full-call total when available.
 */
export interface DshTokenUsage {
  inputTokens: number;
  outputTokens: number;
  totalTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
  reasoningTokens?: number;
}

/**
 * Mirrors `FinishReason` in `packages/llm/llm/src/types.ts`.
 * Finish kinds are extensible upstream, so unknown `kind` strings pass through.
 */
export interface DshFinishReason {
  kind: string;
  failure?: { message?: string; code?: string };
}

/**
 * Mirrors `StreamChunk` in `packages/llm/llm/src/types.ts`; only fields read
 * by the plugin are modeled. Optional variant fields allow unknown chunk
 * types. On `block-end`, `block` contains the assembled block.
 */
export interface DshStreamChunk {
  type: string;
  index?: number;
  text?: string;
  /** `block-start`: the type of the block the following deltas belong to. */
  blockType?: string;
  /** Provider-issued call ID on `tool-call-delta` chunks. */
  id?: string;
  name?: string;
  argumentsDelta?: string;
  block?: DshContentBlock;
  usage?: DshTokenUsage;
  reason?: DshFinishReason;
}

/**
 * Mirrors `ContentBlock` in `packages/llm/llm/src/types.ts`, flattened into
 * one optional-field shape. The upstream union is extensible, so unknown block
 * types are ignored.
 */
export interface DshContentBlock {
  type: string;
  text?: string;
  /** Provider-issued call ID on `tool-call` blocks. */
  id?: string;
  name?: string;
  /** `tool-call`: raw JSON string exactly as the model produced it. */
  arguments?: string;
  toolCallId?: string;
  readonly content?: readonly DshContentBlock[];
  isError?: boolean;
}

/**
 * Mirrors `Message` in `packages/llm/llm/src/message.ts`.
 * Tool results use role `tool` with top-level call identity and error status.
 * Legacy `user` messages can contain nested `tool-result` blocks.
 */
export interface DshMessage {
  id?: string;
  role: string;
  readonly content?: readonly DshContentBlock[];
  toolCallId?: string;
  isError?: boolean;
  source?: { kind?: string };
}

/** Mirrors `ToolSchema` in `packages/llm/llm/src/types.ts`. */
export interface DshToolSchema {
  name: string;
  description?: string;
  parameters?: Record<string, unknown>;
}

/**
 * Mirrors `GenerateOptions` in `packages/llm/llm/src/types.ts`, the payload of
 * the `llm/stream` waterfall.
 */
export interface DshGenerateOptions {
  provider: string;
  model: string;
  messages?: readonly DshMessage[];
  system?: string;
  tools?: readonly DshToolSchema[];
  temperature?: number;
  maxTokens?: number;
  signal?: AbortSignal;
  sessionId?: string;
  purpose?: "compaction" | "session-title";
}

/** Mirrors `Agent` in `packages/core/agent/src/types.ts`. */
export interface DshAgent {
  id: string;
  session?: DshSession;
}

/**
 * Mirrors `ToolDispatchExecution` in `packages/core/tools/src/index.ts`, the
 * payload of the `tools/execute` waterfall.
 */
export interface DshToolDispatchExecution {
  callId: string;
  rootCallId?: string;
  name: string;
  arguments: unknown;
  agent?: DshAgent;
}

/**
 * Mirrors `ToolExecutionResult` (`ToolExecutionSuccess | ToolExecutionFailure`)
 * in `packages/core/tools/src/index.ts`. The plugin reads only text blocks
 * from the result's content.
 */
export interface DshToolExecutionResult {
  isError: boolean;
  readonly content?: readonly DshContentBlock[];
  error?: { message?: string; code?: string };
}

/** Mirrors `SessionHeader` in `packages/core/session/src/types.ts`. */
export interface DshSessionHeader {
  id: string;
  createdAt?: number;
  cwd?: string;
  parentSession?: string;
  origin?: string;
}

/**
 * Mirrors the `Session` surface this plugin reads
 * (`packages/core/session/src/index.ts`): its id and its immutable header.
 */
export interface DshSession {
  id: string;
  header?: DshSessionHeader;
}

/** The session store cordis provides under the `sessions` service name. */
export interface DshSessionStore {
  get?(id: string): DshSession | undefined;
}

/** Mirrors the title snapshot returned by `SessionTitleService.get`. */
export interface DshSessionTitleSnapshot {
  title: string;
}

/** Optional enrichment service provided under the `sessionTitle` name. */
export interface DshSessionTitleService {
  get?(session: DshSession): DshSessionTitleSnapshot | undefined;
}

/**
 * The slice of cordis' `Context` the plugin uses. `on` returns a disposer
 * upstream; the plugin ignores it because cordis tears every listener down
 * when the plugin unloads.
 */
export interface DshContext {
  on(
    event: "llm/stream",
    listener: (
      options: DshGenerateOptions,
      next: () => AsyncIterable<DshStreamChunk>,
    ) => AsyncIterable<DshStreamChunk>,
  ): unknown;
  on(
    event: "tools/execute",
    listener: (
      exec: DshToolDispatchExecution,
      next: () => Promise<DshToolExecutionResult>,
    ) => Promise<DshToolExecutionResult>,
  ): unknown;
  /** Cordis awaits promised disposers; optional for test contexts. */
  effect?(setup: () => () => void | Promise<void>): unknown;
  /**
   * Reads undeclared services; absent services return `undefined`
   * (`vendor/cordis/src/reflect.ts`), so optional enrichment cannot stop capture.
   */
  get?(name: string): unknown;
}
