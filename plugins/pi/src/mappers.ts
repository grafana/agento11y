import type {
  ContentCaptureMode,
  GenerationResult,
  GenerationStart,
  Message,
  TokenUsage,
  ToolDefinition,
} from "@grafana/agento11y";

// includesToolBodies decides whether tool argument JSON, tool result content,
// and tool description/schema are included in the proto export.
//
// Both `full` and `full_with_metadata_spans` ship full content in the proto
// export per the SDK contract (see go/agento11y/content_capture.go on
// ContentCaptureModeFullWithMetadataSpans). The two modes only differ on the
// OTel span side, which is handled inside the SDK, not in this mapper.
function includesToolBodies(contentCapture: ContentCaptureMode): boolean {
  return (
    contentCapture === "full" || contentCapture === "full_with_metadata_spans"
  );
}

/**
 * Tag key marking a generation that pi's host made outside a user turn.
 * Values: `compaction` (context compaction) and `branch_summary` (tree
 * navigation summary). Prefixed with `pi.` because no cross-launcher
 * convention exists for host-initiated model calls.
 */
const PI_CALL_KIND_TAG = "pi.call_kind";

/**
 * Pi's ToolInfo shape from @mariozechner/pi-coding-agent.
 * Declared here to avoid a hard import of pi types (treated as external at
 * runtime); the structural fields match `ToolInfo` in pi's `ExtensionAPI`.
 */
export interface PiToolInfo {
  name: string;
  description?: string;
  parameters?: unknown;
}

/**
 * Request controls extracted from a `before_provider_request` payload.
 * Defensive shape: every field is optional because providers differ in
 * which controls they accept and what names they use.
 */
export interface CachedRequestControls {
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  toolChoice?: string;
  thinkingBudgetTokens?: number;
}

/**
 * Pi's AssistantMessage shape from @mariozechner/pi-ai.
 * Declared here to avoid hard import (pi types are external at runtime).
 */
export interface PiAssistantMessage {
  role: "assistant";
  content: PiContentBlock[];
  provider: string;
  model: string;
  responseId?: string;
  usage: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
    totalTokens: number;
    cost?: {
      input: number;
      output: number;
      cacheRead: number;
      cacheWrite: number;
      total: number;
    };
  };
  stopReason: string;
  errorMessage?: string;
  timestamp: number;
}

export type PiContentBlock =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string; redacted?: boolean }
  | {
      type: "toolCall";
      id: string;
      name: string;
      arguments: Record<string, unknown>;
    };

/** Pi's TextContent / ImageContent / UserMessage shapes from @mariozechner/pi-ai. */
export interface PiTextContent {
  type: "text";
  text: string;
}

export interface PiImageContent {
  type: "image";
  data: string;
  mimeType: string;
}

export interface PiUserMessage {
  role: "user";
  content: string | (PiTextContent | PiImageContent)[];
  timestamp: number;
}

export interface PiToolResult {
  role: "toolResult";
  toolCallId: string;
  toolName: string;
  content: Array<{ type: string; text?: string }>;
  details?: unknown;
  isError: boolean;
  timestamp: number;
}

export interface ToolTiming {
  toolCallId: string;
  toolName: string;
  startedAt: number;
  completedAt: number;
  isError: boolean;
}

/**
 * Cap for the conversation title. Counts code points, not UTF-16 units, so a
 * trailing surrogate pair (emoji) is never split.
 */
export const MAX_TITLE_LEN = 100;

function clipTitle(value: string): string {
  const trimmed = value.trim();
  const codepoints = Array.from(trimmed);
  return codepoints.length > MAX_TITLE_LEN
    ? codepoints.slice(0, MAX_TITLE_LEN).join("")
    : trimmed;
}

/** Inputs for {@link resolveConversationTitle}. */
export interface ResolveConversationTitleOptions {
  /** User-defined session name from pi's `SessionManager.getSessionName()`. */
  sessionName?: string;
  /** First user prompt text seen in the session (first prompt wins). */
  firstUserText?: string;
  /** Session id, used as the last-resort fallback. */
  conversationId?: string;
  contentCapture: ContentCaptureMode;
}

/**
 * Resolve the conversation title shown in Agent Observability.
 *
 * Pi exposes a real, user-defined session name via `getSessionName()`; prefer
 * it whenever set. Otherwise derive a title from the first user prompt, the
 * same approach the Claude Code and Cursor plugins take since neither host
 * exposes a name. The derived title is suppressed in `metadata_only` because
 * the prompt body is dropped from the export in that mode; a user-set session
 * name is metadata rather than content, so it survives. Falls back to the
 * session id when nothing else is available.
 *
 * The returned title is not redacted here: the SDK's generation sanitizer
 * runs `redactLightweight` over `conversationTitle` on export.
 */
export function resolveConversationTitle(
  opts: ResolveConversationTitleOptions,
): string | undefined {
  const name = opts.sessionName?.trim();
  if (name) return clipTitle(name);

  if (opts.contentCapture !== "metadata_only") {
    const derived = opts.firstUserText?.trim();
    if (derived) return clipTitle(derived);
  }

  return opts.conversationId;
}

/** Optional context for building a GenerationStart seed. */
export interface MapGenerationStartOptions {
  conversationId?: string;
  conversationTitle?: string;
  agentName: string;
  agentVersion?: string;
  startedAt: number;
  tools?: ToolDefinition[];
  tags?: Record<string, string>;
  systemPrompt?: string;
  requestControls?: CachedRequestControls;
  /**
   * Deterministic generation ID. When set, overrides the SDK's random
   * `gen-*` ID so Agent Observability can link this generation in the dependency graph.
   * Resolved from the active Pi session branch in `index.ts`.
   */
  generationId?: string;
  /**
   * Producer-supplied parent generation IDs. Pi uses this to point at the
   * previous assistant turn on the same branch.
   */
  parentGenerationIds?: string[];
  /**
   * Extra generation metadata. Merged with the metadata this mapper derives
   * from `requestControls`; derived keys win on collision.
   */
  metadata?: Record<string, unknown>;
}

/** Build the GenerationStart seed from an assistant message and context. */
export function mapGenerationStart(
  msg: PiAssistantMessage,
  opts: MapGenerationStartOptions,
): GenerationStart {
  const {
    conversationId,
    conversationTitle,
    agentName,
    agentVersion,
    startedAt,
    tools,
    tags,
    systemPrompt,
    requestControls,
    generationId,
    parentGenerationIds,
    metadata,
  } = opts;
  // Tags on the seed override client-level SIGIL_TAGS (the SDK merges
  // `{...clientTags, ...seedTags}`), matching claude-code/cursor.
  const start: GenerationStart = {
    conversationId,
    ...(conversationTitle ? { conversationTitle } : {}),
    agentName,
    agentVersion,
    effectiveVersion: agentVersion,
    model: { provider: msg.provider, name: msg.model },
    startedAt: new Date(startedAt),
    ...(tools && tools.length > 0 ? { tools } : {}),
    ...(tags && Object.keys(tags).length > 0 ? { tags } : {}),
  };
  if (generationId) {
    start.id = generationId;
  }
  if (parentGenerationIds && parentGenerationIds.length > 0) {
    start.parentGenerationIds = parentGenerationIds;
  }
  if (msg.content.some((b) => b.type === "thinking")) {
    start.thinkingEnabled = true;
  }
  if (systemPrompt && systemPrompt.length > 0) {
    start.systemPrompt = systemPrompt;
  }
  const startMetadata: Record<string, unknown> = { ...metadata };
  if (requestControls) {
    if (typeof requestControls.maxTokens === "number") {
      start.maxTokens = requestControls.maxTokens;
    }
    if (typeof requestControls.temperature === "number") {
      start.temperature = requestControls.temperature;
    }
    if (typeof requestControls.topP === "number") {
      start.topP = requestControls.topP;
    }
    if (typeof requestControls.toolChoice === "string") {
      start.toolChoice = requestControls.toolChoice;
    }
    if (typeof requestControls.thinkingBudgetTokens === "number") {
      // The SDK reads `agento11y.gen_ai.request.thinking.budget_tokens` from
      // generation metadata and surfaces it as the matching span attribute.
      startMetadata["agento11y.gen_ai.request.thinking.budget_tokens"] =
        requestControls.thinkingBudgetTokens;
    }
  }
  if (Object.keys(startMetadata).length > 0) {
    start.metadata = startMetadata;
  }
  return start;
}

/**
 * Build the GenerationResult from an assistant message.
 *
 * `completedAtMs` should be the time the provider stream finished
 * (assistant `message_end`). `msg.timestamp` is unreliable as a completion
 * marker: pi providers set it via `Date.now()` when constructing the
 * assistant message object — i.e. *before* the API request is sent — so
 * it is closer to a start timestamp than an end timestamp. Falls back to
 * `msg.timestamp` only when no end timestamp was observed.
 */
export function mapGenerationResult(
  msg: PiAssistantMessage,
  toolResults: PiToolResult[],
  contentCapture: ContentCaptureMode,
  input?: Message[],
  completedAtMs?: number,
): GenerationResult {
  const result: GenerationResult = {
    responseId: msg.responseId,
    responseModel: msg.model,
    usage: mapPiUsage(msg.usage),
    stopReason: mapStopReason(msg.stopReason),
    completedAt: new Date(completedAtMs ?? msg.timestamp),
    metadata:
      msg.usage.cost !== undefined ? { cost_usd: msg.usage.cost.total } : {},
  };

  if (input && input.length > 0) {
    result.input = input;
  }

  // Always emit structural tool_call / tool_result parts so the SDK can count
  // them for the `gen_ai.client.tool_calls_per_operation` histogram. Body
  // content (assistant text, tool args, tool results) is included per
  // contentCapture; in `metadata_only` the SDK strips content before export.
  const output: Message[] = [
    ...mapAssistantOutput(msg, contentCapture),
    ...mapToolResultsOutput(toolResults, contentCapture),
  ];
  if (output.length > 0) {
    result.output = output;
  }

  return result;
}

/**
 * Pi's `Usage` shape, read structurally. Every field is optional because the
 * plugin also reads usage off persisted session entries written by older pi
 * versions, where the field may be missing entirely.
 */
export interface PiUsageLike {
  input?: number;
  output?: number;
  cacheRead?: number;
  cacheWrite?: number;
  totalTokens?: number;
  cost?: { total?: number };
}

/**
 * Map pi usage to the SDK's `TokenUsage`. Shared by the turn path and the
 * summary path so the two cannot drift.
 *
 * The field set is narrower than pi's `Usage`. `reasoning` and `cacheWrite1h`
 * are not forwarded, and `totalTokens` is passed through as pi computes it
 * (`input + output + cacheRead + cacheWrite`) instead of being recomputed to
 * the Go launchers' `input + output`. Either change would move existing golden
 * fixtures, so both stay as they are.
 */
export function mapPiUsage(usage: PiUsageLike): TokenUsage {
  return {
    inputTokens: usage.input ?? 0,
    outputTokens: usage.output ?? 0,
    totalTokens: usage.totalTokens ?? 0,
    cacheReadInputTokens: usage.cacheRead ?? 0,
    cacheWriteInputTokens: usage.cacheWrite ?? 0,
  };
}

/** Token-count fields on pi's `Usage`. The `cost` block is not one of them. */
export const PI_USAGE_TOKEN_FIELDS = [
  "input",
  "output",
  "cacheRead",
  "cacheWrite",
  "totalTokens",
] as const;

/**
 * True when the host reported at least one token count. A usage object that
 * carries only a cost says nothing about tokens, and every token field is
 * serialized on the exported record, so mapping it would report "0 tokens"
 * where the truth is "unknown".
 */
function hasPiTokenCounts(usage: PiUsageLike): boolean {
  return PI_USAGE_TOKEN_FIELDS.some((key) => Number.isFinite(usage[key]));
}

/**
 * Which host summarization call produced a generation. `compaction` covers
 * manual `/compact`, `ctx.compact()`, threshold auto-compaction, and
 * context-overflow recovery; `branch_summary` covers tree-navigation
 * summaries.
 */
export type PiSummaryKind = "compaction" | "branch_summary";

/**
 * Fields read off pi's persisted `CompactionEntry` / `BranchSummaryEntry`.
 * Structural and fully optional: `usage` was added to those entries after the
 * dev-pinned pi version, so it must degrade to `undefined` rather than fail
 * type checking.
 */
export interface PiSummaryEntryLike {
  id?: string;
  timestamp?: string;
  summary?: string;
  /** Pre-compaction context estimate. Compaction entries only. */
  tokensBefore?: number;
  usage?: PiUsageLike;
  /** True when an extension supplied the summary and no model call happened. */
  fromHook?: boolean;
}

/** Inputs for {@link mapSummaryGenerationStart}. */
export interface MapSummaryGenerationStartOptions {
  kind: PiSummaryKind;
  conversationId?: string;
  conversationTitle?: string;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  startedAt: number;
  tags?: Record<string, string>;
  generationId?: string;
  parentGenerationIds?: string[];
  /** Extra generation metadata, e.g. the fork's trunk link. */
  metadata?: Record<string, unknown>;
}

/**
 * Build the `GenerationStart` seed for a host summarization call.
 *
 * No `operationName` is set on purpose: the SYNC path (`startGeneration`)
 * defaults it to `generateText`, which discriminates these calls from turns
 * (`streamText`) without adding a new `gen_ai.operation.name` label value.
 */
export function mapSummaryGenerationStart(
  opts: MapSummaryGenerationStartOptions,
): GenerationStart {
  const start: GenerationStart = {
    conversationId: opts.conversationId,
    ...(opts.conversationTitle
      ? { conversationTitle: opts.conversationTitle }
      : {}),
    agentName: opts.agentName,
    agentVersion: opts.agentVersion,
    effectiveVersion: opts.agentVersion,
    model: opts.model,
    startedAt: new Date(opts.startedAt),
    tags: { ...opts.tags, [PI_CALL_KIND_TAG]: opts.kind },
  };
  if (opts.generationId) {
    start.id = opts.generationId;
  }
  if (opts.parentGenerationIds && opts.parentGenerationIds.length > 0) {
    start.parentGenerationIds = opts.parentGenerationIds;
  }
  if (opts.metadata && Object.keys(opts.metadata).length > 0) {
    start.metadata = { ...opts.metadata };
  }
  return start;
}

/** Inputs for {@link mapSummaryGenerationResult}. */
export interface MapSummaryGenerationResultOptions {
  entry: PiSummaryEntryLike;
  contentCapture: ContentCaptureMode;
  completedAt: number;
  responseModel?: string;
  /** `manual` | `threshold` | `overflow`, compaction only. */
  reason?: string;
  /** True when the aborted turn is retried after this compaction. */
  willRetry?: boolean;
}

/**
 * Build the `GenerationResult` for a host summarization call.
 *
 * The summary text goes in `output` as a single assistant text part, never in
 * metadata or tags: content capture strips `output` but never touches
 * `metadata`/`tags`, so a summary in metadata would leak in `metadata_only`.
 * The gate is `contentCapture !== "metadata_only"`, matching assistant text on
 * the turn path — the summary is assistant text, not a tool body, so the
 * `no_tool_content` split does not apply to it.
 *
 * `usage` is omitted entirely when the entry reports no token counts. The
 * exported record always serializes every token field, so a zeroed block
 * reads as "this call used 0 tokens" rather than "the host did not say".
 * `tokensBefore` is a context estimate for the whole conversation, not this
 * call's input token count, so it stays in metadata.
 *
 * `cost_usd` is set whenever pi priced the call, including a cost of `0`,
 * which is what the turn path does in {@link mapGenerationResult}.
 */
export function mapSummaryGenerationResult(
  opts: MapSummaryGenerationResultOptions,
): GenerationResult {
  const { entry, contentCapture, completedAt } = opts;

  const metadata: Record<string, unknown> = {};
  const cost = entry.usage?.cost?.total;
  if (typeof cost === "number") {
    metadata.cost_usd = cost;
  }
  if (typeof entry.tokensBefore === "number") {
    metadata["pi.tokens_before"] = entry.tokensBefore;
  }
  if (typeof opts.reason === "string" && opts.reason.length > 0) {
    metadata["pi.compaction.reason"] = opts.reason;
  }
  if (typeof opts.willRetry === "boolean") {
    metadata["pi.compaction.will_retry"] = opts.willRetry;
  }

  const result: GenerationResult = {
    stopReason: "end_turn",
    completedAt: new Date(completedAt),
    metadata,
  };
  if (opts.responseModel) {
    result.responseModel = opts.responseModel;
  }
  if (entry.usage && hasPiTokenCounts(entry.usage)) {
    result.usage = mapPiUsage(entry.usage);
  }

  const summary = entry.summary;
  if (
    contentCapture !== "metadata_only" &&
    typeof summary === "string" &&
    summary.trim().length > 0
  ) {
    result.output = [
      { role: "assistant", parts: [{ type: "text", text: summary }] },
    ];
  }

  return result;
}

/**
 * Pi's AgentMessage union as exposed in the `context` event. Declared
 * structurally to avoid hard-importing pi-ai/pi-agent-core types at runtime.
 * Matches `UserMessage | AssistantMessage | ToolResultMessage` from
 * @mariozechner/pi-ai, plus any custom AgentMessage extensions, which we
 * pass through without touching.
 */
export type PiAgentMessage =
  | PiUserMessage
  | PiAssistantMessage
  | PiToolResult
  | { role?: string };

/**
 * Map pi's `ContextEvent.messages` (the full conversation pi is about to
 * send to the model) to agento11y `Message[]` for a preflight hook evaluation.
 *
 * Differences from `mapUserMessage`/`mapAssistantOutput`:
 * - All roles (user, assistant, tool) are mapped 1:1 so the redacted
 *   round-trip can be aligned by index. Returning fewer messages would
 *   break that alignment.
 * - `contentCapture` is intentionally NOT applied. The hook server only
 *   sees these messages in memory and never persists them; redacting them
 *   here would defeat the point of running transforms (Vercel adapter
 *   takes the same approach for the same reason).
 * - Provider-signed `thinking` parts are dropped from the forward payload:
 *   they carry an opaque provider signature, are not transformable, and
 *   would needlessly inflate the hook request. The original pi message is
 *   kept by the caller, so the `thinking` block survives untouched on the
 *   write-back side.
 * - Image content is skipped (agento11y's `MessagePart` union has no image
 *   type), mirroring `mapUserMessage`.
 */
export function mapAgentMessagesForHook(messages: PiAgentMessage[]): Message[] {
  return messages.map(mapAgentMessageForHook);
}

function mapAgentMessageForHook(msg: PiAgentMessage): Message {
  // Null / non-object slot. Emit a placeholder so the 1:1 index alignment
  // between forward and write-back arrays is preserved; dropping it here
  // would shrink `forward` below `piMessages` and force the whole transform
  // to be discarded on write-back. The write-back side skips slots it cannot
  // touch.
  if (!msg || typeof msg !== "object") {
    return { role: "unknown", parts: [] };
  }
  const role = (msg as { role?: unknown }).role;

  if (role === "user") {
    return mapUserMessageForHook(msg as PiUserMessage);
  }
  if (role === "assistant") {
    return mapAssistantMessageForHook(msg as PiAssistantMessage);
  }
  if (role === "toolResult") {
    return mapToolResultForHook(msg as PiToolResult);
  }
  // Unknown / custom AgentMessage subtype. Emit a placeholder so the
  // index alignment between forward and write-back arrays is preserved.
  return { role: String(role ?? "unknown"), parts: [] };
}

function mapUserMessageForHook(msg: PiUserMessage): Message {
  const text = userMessageText(msg);
  return {
    role: "user",
    parts: text.length > 0 ? [{ type: "text", text }] : [],
  };
}

function mapAssistantMessageForHook(msg: PiAssistantMessage): Message {
  // Concatenate all assistant text parts into a single text part. `thinking`
  // blocks carry an opaque provider signature that must round-trip unchanged,
  // so they are dropped from the forward payload. `toolCall` blocks are
  // dropped here too: postflight tool-arg redaction has its own path through
  // `runToolCallGuard`, and forwarding the call here would duplicate it.
  const textParts: string[] = [];
  const partList: NonNullable<Message["parts"]> = [];
  for (const block of msg.content) {
    if (block.type === "text" && block.text.length > 0) {
      textParts.push(block.text);
    }
  }
  if (textParts.length > 0) {
    partList.push({ type: "text", text: textParts.join("\n") });
  }
  return { role: "assistant", parts: partList };
}

/**
 * Join a pi tool result's text content into a single string, dropping
 * non-text parts (e.g. images). Shared by the hook mappers and the tool-span
 * emitter so tool output is flattened the same way everywhere.
 */
export function toolResultText(
  content: Array<{ type: string; text?: string }>,
): string {
  return content
    .filter(
      (c): c is { type: "text"; text: string } => c.type === "text" && !!c.text,
    )
    .map((c) => c.text)
    .join("\n");
}

function mapToolResultForHook(tr: PiToolResult): Message {
  const text = toolResultText(tr.content);
  return {
    role: "tool",
    parts: [
      {
        type: "tool_result",
        toolResult: {
          toolCallId: tr.toolCallId,
          name: tr.toolName,
          content: text,
          isError: tr.isError,
        },
      },
    ],
  };
}

/**
 * Write redacted text from a server-side preflight transform back into the
 * original pi messages, in place. Returns true on a clean apply, false when
 * any safety check fails (counts diverge, role mismatch, no text part where
 * one is expected). On a false return, callers must NOT replace pi's
 * outgoing `messages` — the transform is dropped and the original text
 * flows through unchanged.
 *
 * `thinking` parts on assistant messages are never touched: their opaque
 * provider signature must round-trip unchanged. Only the assistant text
 * blocks are overwritten, and only when the redacted message exposes a
 * single text part covering them all.
 */
export function applyRedactedText(
  piMessages: PiAgentMessage[],
  redactedAgento11yMessages: Message[],
): boolean {
  if (piMessages.length !== redactedAgento11yMessages.length) {
    return false;
  }
  for (let i = 0; i < piMessages.length; i++) {
    const pi = piMessages[i];
    const sig = redactedAgento11yMessages[i];
    if (!sig) return false;
    // Null / non-object pi slot: nothing writable here. The mapper emitted a
    // placeholder to keep alignment, so skip it rather than dropping the
    // redaction for every other message in the turn.
    if (typeof pi !== "object" || pi === null) continue;
    const ok = applyRedactedToMessage(pi, sig);
    if (!ok) return false;
  }
  return true;
}

function applyRedactedToMessage(pi: PiAgentMessage, sig: Message): boolean {
  const role = (pi as { role?: unknown }).role;
  if (role === "user") {
    return applyRedactedToUser(pi as PiUserMessage, sig);
  }
  if (role === "assistant") {
    return applyRedactedToAssistant(pi as PiAssistantMessage, sig);
  }
  if (role === "toolResult") {
    return applyRedactedToToolResult(pi as PiToolResult, sig);
  }
  // Unknown role: leave untouched, but accept (alignment preserved).
  return true;
}

function extractTextFromAgento11yMessage(sig: Message): string | null {
  // Accept both shapes: legacy `content` shorthand and the typed `parts`
  // array. The Agent Observability server emits typed parts on the transformed payload,
  // but tolerate either to keep round-tripping robust to wire changes.
  if (typeof sig.content === "string") return sig.content;
  if (!sig.parts) return null;
  let acc = "";
  let seenText = false;
  for (const p of sig.parts) {
    if (p.type === "text") {
      if (seenText) acc += "\n";
      acc += p.text;
      seenText = true;
    }
  }
  return seenText ? acc : null;
}

function applyRedactedToUser(pi: PiUserMessage, sig: Message): boolean {
  const redacted = extractTextFromAgento11yMessage(sig);
  if (redacted === null) {
    // Server stripped the message to nothing (e.g. image-only original).
    // Leave pi untouched — there is nothing meaningful to write back.
    return true;
  }
  if (typeof pi.content === "string") {
    pi.content = redacted;
    return true;
  }
  // Array-shaped content: collapse all text parts into the first text
  // slot and empty the rest. We do not attempt to split the redacted text
  // back across multiple slots because the regex transform may have
  // changed or added newlines, and a misaligned split would silently
  // misrepresent message structure. Image parts are left untouched.
  return collapseTextParts(
    pi.content as Array<PiTextContent | PiImageContent>,
    redacted,
  );
}

function applyRedactedToAssistant(
  pi: PiAssistantMessage,
  sig: Message,
): boolean {
  const redacted = extractTextFromAgento11yMessage(sig);
  if (redacted === null) {
    // No text in the redacted payload (e.g. assistant turn had only tool
    // calls / thinking originally). Leave pi untouched.
    return true;
  }
  // Collapse text blocks but never touch `thinking` or `toolCall` blocks:
  // their provider signatures must round-trip unchanged.
  return collapseTextParts(
    pi.content as Array<{ type: string; text?: string }>,
    redacted,
  );
}

function applyRedactedToToolResult(pi: PiToolResult, sig: Message): boolean {
  // For tool-result messages the server keeps the `tool_result` part shape
  // and redacts `toolResult.content` in place (grafana/sigil
  // `internal/eval/hooks/transform.go`). The text part path is intentionally
  // ignored here: the SDK does not synthesize a standalone text part for
  // redacted tool results, so falling back to `extractTextFromAgento11yMessage`
  // would silently miss the redaction.
  const redacted = extractToolResultContent(sig);
  if (redacted === null) return true;
  return collapseTextParts(
    pi.content as Array<{ type: string; text?: string }>,
    redacted,
  );
}

function extractToolResultContent(sig: Message): string | null {
  if (!sig.parts) return null;
  for (const p of sig.parts) {
    if (p.type !== "tool_result") continue;
    const c = p.toolResult?.content;
    if (typeof c === "string") return c;
  }
  return null;
}

/**
 * Walk a content array, replace the first `text` block's text with
 * `redacted`, and clear text on any other `text` blocks. Non-text blocks
 * (`thinking`, `toolCall`, `image`) are left untouched. Returns true if at
 * least one text block was found and updated, or if there were no text
 * blocks at all (nothing to redact). Mutates `parts` in place.
 */
function collapseTextParts(
  parts: Array<{ type: string; text?: string }>,
  redacted: string,
): boolean {
  let firstText: { type: string; text?: string } | undefined;
  let firstTextIndex = -1;
  for (let i = 0; i < parts.length; i++) {
    const part = parts[i];
    if (part?.type === "text") {
      firstText = part;
      firstTextIndex = i;
      break;
    }
  }
  if (!firstText) return true;
  firstText.text = redacted;
  for (let i = firstTextIndex + 1; i < parts.length; i++) {
    const part = parts[i];
    if (part?.type === "text") part.text = "";
  }
  return true;
}

/**
 * Map a pi user message to an agento11y input Message. Returns null in
 * `metadata_only` (mirrors how assistant text/thinking is dropped) and for
 * empty/whitespace-only content. Image parts are skipped because agento11y's
 * `MessagePart` union has no image type; multiple text parts are joined with
 * a newline.
 */
export function mapUserMessage(
  msg: PiUserMessage,
  contentCapture: ContentCaptureMode,
): Message | null {
  if (contentCapture === "metadata_only") return null;

  const text = userMessageText(msg);
  if (text.trim().length === 0) return null;

  return {
    role: "user",
    parts: [{ type: "text", text }],
  };
}

/**
 * Flatten a pi user message to plain text. String content passes through;
 * a content array keeps text parts (joined with a newline) and drops images,
 * which agento11y's `MessagePart` union cannot represent.
 */
export function userMessageText(msg: PiUserMessage): string {
  if (typeof msg.content === "string") return msg.content;
  return msg.content
    .filter((c): c is PiTextContent => c.type === "text")
    .map((c) => c.text)
    .join("\n");
}

/**
 * Map the active tool catalog to ToolDefinition[]. Filters `toolCatalog` by
 * `activeNames` so the seed reflects what was offered to the model, not the
 * full registry. `activeNames === null` means "no filter" (the active-set
 * API is unavailable); an empty Set means "no tools offered this turn" and
 * produces an empty result. `description` and `inputSchemaJSON` are body
 * content and are only emitted when the mode includes tool bodies (`full` or
 * `full_with_metadata_spans`); otherwise the definitions are name-only,
 * matching how `git.branch` is gated.
 */
export function mapTools(
  toolCatalog: PiToolInfo[],
  activeNames: Set<string> | null,
  contentCapture: ContentCaptureMode,
): ToolDefinition[] {
  const defs: ToolDefinition[] = [];
  const seen = new Set<string>();
  const includeBody = includesToolBodies(contentCapture);

  for (const tool of toolCatalog) {
    if (!tool || typeof tool.name !== "string") continue;
    if (activeNames !== null && !activeNames.has(tool.name)) continue;
    if (seen.has(tool.name)) continue;
    seen.add(tool.name);

    const def: ToolDefinition = { name: tool.name };
    if (includeBody) {
      if (typeof tool.description === "string" && tool.description.length > 0) {
        def.description = tool.description;
      }
      if (tool.parameters !== undefined) {
        try {
          def.inputSchemaJSON = JSON.stringify(tool.parameters);
        } catch {
          // Non-serializable schema (cycles, BigInt, etc.) — skip silently.
        }
      }
    }
    defs.push(def);
  }
  return defs;
}

/**
 * Read provider-specific request controls from a `before_provider_request`
 * payload. Pi emits provider-shaped payloads:
 *   - Anthropic / OpenAI Chat / OpenAI Responses: fields at the top level
 *     (`max_tokens`, `temperature`, `top_p`, `tool_choice`, `thinking`).
 *   - Gemini (`@google/genai` SDK): wrapped in `config` (`config.temperature`,
 *     `config.maxOutputTokens`, `config.toolConfig.functionCallingConfig.mode`,
 *     `config.thinkingConfig.thinkingBudget`).
 * Unknown shapes degrade to `{}`.
 */
export function extractRequestControls(
  payload: unknown,
): CachedRequestControls {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) {
    return {};
  }
  const obj = payload as Record<string, unknown>;
  const out: CachedRequestControls = {};

  const asObject = (v: unknown): Record<string, unknown> | undefined =>
    v && typeof v === "object" && !Array.isArray(v)
      ? (v as Record<string, unknown>)
      : undefined;

  // Gemini wraps controls in `config`. The legacy REST shape used
  // `generationConfig`; accept both so older pi versions or compat layers
  // still work.
  const geminiConfig = asObject(obj.config) ?? asObject(obj.generationConfig);

  const readNumber = (...candidates: unknown[]): number | undefined => {
    for (const c of candidates) {
      if (typeof c === "number" && Number.isFinite(c)) return c;
    }
    return undefined;
  };

  const maxTokens = readNumber(
    obj.max_tokens,
    obj.max_completion_tokens,
    obj.max_output_tokens,
    geminiConfig?.maxOutputTokens,
  );
  if (maxTokens !== undefined) out.maxTokens = maxTokens;

  const temperature = readNumber(obj.temperature, geminiConfig?.temperature);
  if (temperature !== undefined) out.temperature = temperature;

  const topP = readNumber(obj.top_p, obj.topP, geminiConfig?.topP);
  if (topP !== undefined) out.topP = topP;

  // `tool_choice` is a string for some providers and an object for others.
  // Anthropic forced-tool uses `{type: "tool", name: "<tool>"}` — encode as
  // `"tool:<name>"` so the forced tool isn't lost.
  // Gemini uses `config.toolConfig.functionCallingConfig.mode`.
  const tc = obj.tool_choice ?? obj.toolChoice;
  if (typeof tc === "string") {
    out.toolChoice = tc;
  } else {
    const tcObj = asObject(tc);
    if (tcObj) {
      const t = tcObj.type;
      const name = tcObj.name;
      if (typeof t === "string") {
        out.toolChoice =
          t === "tool" && typeof name === "string" && name.length > 0
            ? `tool:${name}`
            : t;
      }
    }
  }
  if (out.toolChoice === undefined && geminiConfig) {
    const fc = asObject(geminiConfig.toolConfig)?.functionCallingConfig;
    const mode = asObject(fc)?.mode;
    if (typeof mode === "string") out.toolChoice = mode;
  }

  const thinking = asObject(obj.thinking);
  if (thinking && typeof thinking.budget_tokens === "number") {
    out.thinkingBudgetTokens = thinking.budget_tokens;
  }
  if (out.thinkingBudgetTokens === undefined && geminiConfig) {
    const thinkingConfig = asObject(geminiConfig.thinkingConfig);
    if (thinkingConfig && typeof thinkingConfig.thinkingBudget === "number") {
      out.thinkingBudgetTokens = thinkingConfig.thinkingBudget;
    }
  }

  return out;
}

/**
 * Map assistant message content blocks to agento11y output messages.
 * - text/thinking parts: only when contentCapture allows body content.
 * - tool_call parts: always emitted (structure needed for the SDK's
 *   tool_calls_per_operation metric); inputJSON is only filled when the mode
 *   includes tool bodies (`full` or `full_with_metadata_spans`).
 */
function mapAssistantOutput(
  msg: PiAssistantMessage,
  contentCapture: ContentCaptureMode,
): Message[] {
  const messages: Message[] = [];
  const includeBodies = contentCapture !== "metadata_only";

  for (const block of msg.content) {
    switch (block.type) {
      case "text": {
        if (includeBodies && block.text.trim().length > 0) {
          messages.push({
            role: "assistant",
            parts: [{ type: "text", text: block.text }],
          });
        }
        break;
      }
      case "thinking": {
        if (block.redacted) break;
        if (includeBodies && block.thinking.trim().length > 0) {
          messages.push({
            role: "assistant",
            parts: [{ type: "thinking", thinking: block.thinking }],
          });
        }
        break;
      }
      case "toolCall": {
        messages.push({
          role: "assistant",
          parts: [
            {
              type: "tool_call",
              toolCall: {
                id: block.id,
                name: block.name,
                inputJSON: includesToolBodies(contentCapture)
                  ? JSON.stringify(block.arguments)
                  : "",
              },
            },
          ],
        });
        break;
      }
    }
  }

  return messages;
}

/**
 * Map pi tool results to agento11y tool result messages. Always emits the
 * structural part; body content is included only when the mode includes tool
 * bodies (`full` or `full_with_metadata_spans`).
 */
function mapToolResultsOutput(
  toolResults: PiToolResult[],
  contentCapture: ContentCaptureMode,
): Message[] {
  const messages: Message[] = [];
  const includeBody = includesToolBodies(contentCapture);

  for (const tr of toolResults) {
    let content = "";
    if (includeBody) {
      content = toolResultText(tr.content);
    }

    messages.push({
      role: "tool",
      parts: [
        {
          type: "tool_result",
          toolResult: {
            toolCallId: tr.toolCallId,
            name: tr.toolName,
            content,
            isError: tr.isError,
          },
        },
      ],
    });
  }

  return messages;
}

function mapStopReason(reason: string): string {
  switch (reason) {
    case "stop":
      return "end_turn";
    case "length":
      return "max_tokens";
    case "toolUse":
      return "tool_use";
    case "error":
      return "error";
    case "aborted":
      return "aborted";
    default:
      return reason;
  }
}
