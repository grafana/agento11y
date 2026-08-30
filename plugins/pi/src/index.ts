import { randomUUID } from "node:crypto";
import type {
  Agento11yClient,
  ContentCaptureMode,
  Message,
} from "@grafana/agento11y";
import { redactSecretText } from "@grafana/agento11y";
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { createAgento11yClient } from "./client.js";
import type { Agento11yPiConfig } from "./config.js";
import { loadConfig } from "./config.js";
import { detectPiVersion } from "./detectPiVersion.js";
import { resolveGitBranch } from "./git.js";
import { runPreflightTransform, runToolCallGuard } from "./guard.js";
import {
  type PiGenerationParent,
  resolvePiGenerationLineage,
  resolvePiSummaryLineage,
  type SessionManagerLike,
} from "./lineage.js";
import { LocalReceiverError } from "./local.js";
import { logger } from "./logger.js";
import {
  applyRedactedText,
  type CachedRequestControls,
  extractRequestControls,
  mapAgentMessagesForHook,
  mapGenerationResult,
  mapGenerationStart,
  mapSummaryGenerationResult,
  mapSummaryGenerationStart,
  mapTools,
  mapUserMessage,
  PI_USAGE_TOKEN_FIELDS,
  type PiAssistantMessage,
  type PiSummaryEntryLike,
  type PiSummaryKind,
  type PiToolInfo,
  type PiToolResult,
  type PiUsageLike,
  type PiUserMessage,
  resolveConversationTitle,
  type ToolTiming,
  toolResultText,
  userMessageText,
} from "./mappers.js";
import {
  type ConversationRecord,
  NOT_FORKED,
  resolveSessionOrigin,
  resolveSessionStart,
  type SessionHeaderSourceLike,
  type SessionOrigin,
} from "./sessionOrigin.js";
import { buildBuiltinTags } from "./tags.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

export default function (pi: ExtensionAPI) {
  let sigil: Agento11yClient | null = null;
  let config: Agento11yPiConfig | null = null;
  let telemetry: TelemetryProviders | null = null;
  // Cached from the latest assistant message. `tool_call` events carry no model
  // metadata, so guards read it from `message_end` before the tool runs.
  let lastSeenModel: { provider: string; name: string } | null = null;

  let turnStartTime = 0;
  // Earliest `message_update` event observed in the current turn. Pi emits
  // `message_update` for streaming text/thinking/toolcall blocks coming back
  // from the provider, so the first one is a faithful TTFT signal.
  // 0 means: no streamed chunk seen yet for this turn.
  let firstTokenTime = 0;
  // Time the assistant `message_end` fired for this turn. Pi's agent loop
  // emits message_end (assistant) immediately after the provider stream's
  // `done`/`error` event, *before* any subsequent tool execution in the
  // same turn, so this is the actual completion time of the model call.
  // 0 means: no assistant message_end seen yet for this turn.
  let assistantMessageEndTime = 0;
  // Cached from the most recent `before_agent_start`. Outlives a single
  // turn so multi-turn tool loops reuse the same prompt; cleared on
  // agent_end and session_shutdown.
  let currentSystemPrompt: string | undefined;
  // Refreshed for every `before_provider_request` and consumed by the
  // matching `turn_end`. Cleared in the per-turn finally so a stale value
  // from turn N never leaks into turn N+1, and also on `agent_end` /
  // session shutdown in case a turn ends without a matching `turn_end`.
  let currentRequestControls: CachedRequestControls = {};

  // Tool execution timing: toolCallId → start timestamp
  const toolStarts = new Map<string, { toolName: string; startedAt: number }>();
  const turnToolTimings: ToolTiming[] = [];

  // User messages observed since the previous turn_end. Consumed at the next
  // turn_end and attached to GenerationResult.input. Filled by the
  // `message_end` handler (user role only). Per pi's agent loop, `turn_start`
  // fires BEFORE the user `message_end` for fresh prompts, so this buffer
  // must NOT be cleared at turn_start — only after consume and on session
  // boundaries.
  const pendingInputMessages: Message[] = [];

  // First user prompt text seen in this session, used to derive a
  // conversation title when pi has no user-set session name. First prompt
  // wins, so it persists across turns and is only cleared on session
  // boundaries (never in resetTurnState).
  let firstUserText: string | undefined;

  // Start timestamps for pi's own summarization LLM calls. Written by the
  // `session_before_*` handler, which is the only start signal pi gives us
  // (those events are `hasHandlers`-gated, so registering the handler is what
  // makes pi emit them), and consumed by the matching terminal event. A new
  // start overwrites any pending one, because an aborted or failed compaction
  // emits no terminal event at all and would otherwise leak its start time
  // into a later export.
  let compactionStartedAt: number | undefined;
  let branchSummaryStartedAt: number | undefined;
  // Summary entry ids already exported, so one entry never produces two
  // generations with the same id. `resolveSummaryEntry` corrects the entry
  // `session_compact` hands back, but only on runtimes that expose
  // `getBranch()`; this set covers the fallback.
  const exportedSummaryEntryIds = new Set<string>();

  // Where each conversation came from. Filled at `session_start` and, for a
  // conversation id that appears without one, on first sight at `turn_end`,
  // so the session header is read once per conversation rather than per
  // turn. Cleared by resetSessionState.
  const sessionOrigins = new Map<string, SessionOrigin>();

  // Conversation the plugin is attributing turns to, and when that
  // conversation began. Set on every `session_start`, and deliberately kept
  // across `resetSessionState` and `session_shutdown`: a fork tears the old
  // session down first (agent-session-runtime.js teardownCurrent), and a
  // `--no-session` fork writes no `parentSession`, so this record is the
  // only reference to the trunk it came from. See sessionOrigin.ts.
  let currentConversation: ConversationRecord | undefined;

  function resetTurnState() {
    turnStartTime = 0;
    firstTokenTime = 0;
    assistantMessageEndTime = 0;
    toolStarts.clear();
    turnToolTimings.length = 0;
  }

  async function resetSessionState() {
    config = null;
    if (telemetry) {
      try {
        await telemetry.shutdown();
      } catch (err) {
        logger.error("telemetry shutdown failed", err);
      }
      telemetry = null;
    }
    resetTurnState();
    pendingInputMessages.length = 0;
    firstUserText = undefined;
    lastSeenModel = null;
    currentSystemPrompt = undefined;
    currentRequestControls = {};
    compactionStartedAt = undefined;
    branchSummaryStartedAt = undefined;
    exportedSummaryEntryIds.clear();
    sessionOrigins.clear();
  }

  /**
   * Resolve the model for a summarization call. Compaction and branch
   * summaries run outside the agent loop and carry no model metadata, so read
   * pi's live `ctx.model` first and fall back to the model cached from the
   * last assistant message.
   */
  function resolveSummaryModel(
    ctx: PiSummaryContext,
  ): { provider: string; name: string } | null {
    const provider = ctx?.model?.provider;
    const id = ctx?.model?.id;
    if (
      typeof provider === "string" &&
      provider.length > 0 &&
      typeof id === "string" &&
      id.length > 0
    ) {
      // Assistant messages carry `model.id` as their `model` field, so use the
      // id (not the display name) to keep turn and summary generations on the
      // same model identity.
      return { provider, name: id };
    }
    return lastSeenModel;
  }

  /**
   * Export one generation for a completed pi summarization call.
   *
   * Skips are logged at debug level. An extension-supplied summary and an
   * event outside an active session are routine; the rest are anomalies that
   * still must not interrupt the host.
   */
  async function exportSummaryGeneration(
    kind: PiSummaryKind,
    eventEntry: unknown,
    ctx: PiSummaryContext,
    extras: {
      startedAt?: number;
      reason?: string;
      willRetry?: boolean;
    },
  ): Promise<void> {
    if (!sigil || !config) {
      logger.debug(`${kind}: skipped, no active session`);
      return;
    }

    const sessionManager = ctx?.sessionManager;
    const entry = resolveSummaryEntry(kind, sessionManager, eventEntry);
    if (!entry) {
      logger.debug(`${kind}: skipped, no summary entry on the event`);
      return;
    }
    if (entry.fromHook === true) {
      logger.debug(`${kind}: skipped, summary came from an extension`);
      return;
    }
    const entryId = entry.id;
    if (!entryId) {
      logger.debug(`${kind}: skipped, summary entry has no id`);
      return;
    }
    if (exportedSummaryEntryIds.has(entryId)) {
      logger.debug(`${kind}: skipped, entry ${entryId} already exported`);
      return;
    }

    const model = resolveSummaryModel(ctx);
    if (!model) {
      logger.debug(`${kind}: skipped, no model could be resolved`);
      return;
    }

    const conversationId = readSessionId(sessionManager);

    // Entry timestamps are ISO strings stamped when the entry is appended,
    // i.e. after the summarization call finished. No pi event carries a
    // timestamp, so this is the only end time available.
    const parsedCompletedAt = entry.timestamp
      ? Date.parse(entry.timestamp)
      : Number.NaN;
    const completedAt = Number.isFinite(parsedCompletedAt)
      ? parsedCompletedAt
      : Date.now();
    // Clamp: a start recorded before a clock adjustment (or an entry
    // timestamp written by another machine) must not invert the window.
    const startedAt = Math.min(extras.startedAt ?? completedAt, completedAt);

    const summaryCwd = process.cwd();
    const tags = buildBuiltinTags({
      cwd: summaryCwd,
      gitBranch: resolveGitBranch(summaryCwd),
    });

    let sessionName: string | undefined;
    try {
      sessionName = sessionManager?.getSessionName?.();
    } catch (err) {
      logger.debug("getSessionName failed", err);
    }

    // A summary taken right after a fork sits on top of an inherited turn,
    // so its parent is subject to the same trunk boundary as a turn's. See
    // sessionOrigin.ts.
    const origin: SessionOrigin = conversationId
      ? sessionOriginFor(conversationId, sessionManager)
      : NOT_FORKED;
    const lineage = resolvePiSummaryLineage(
      sessionManager,
      entryId,
      conversationId,
      { origin },
    );

    const seed = mapSummaryGenerationStart({
      kind,
      conversationId,
      conversationTitle: resolveConversationTitle({
        sessionName,
        firstUserText,
        conversationId,
        contentCapture: config.contentCapture,
      }),
      agentName: config.agentName,
      agentVersion: config.agentVersion,
      model,
      startedAt,
      tags,
      generationId: lineage.generationId,
      parentGenerationIds: parentEdge(lineage.parent),
      metadata: forkMetadata(lineage.parent, origin),
    });

    const result = mapSummaryGenerationResult({
      entry,
      contentCapture: config.contentCapture,
      completedAt,
      responseModel: model.name,
      reason: extras.reason,
      willRetry: extras.willRetry,
    });

    // SYNC, not STREAM: there is no token stream and no TTFT signal for these
    // calls. The SYNC path defaults `operationName` to `generateText`, which
    // also discriminates them from turns (`streamText`).
    await sigil.startGeneration(seed, async (recorder) => {
      recorder.setResult(result);
    });
    // Only mark the entry exported once the recorder accepted it, so a
    // failed export can be retried by a later event for the same entry.
    exportedSummaryEntryIds.add(entryId);
    logger.debug(
      `summary generation queued, kind=${kind} entry=${entryId} tokens=${
        result.usage?.totalTokens ?? "unknown"
      }`,
    );
  }

  /**
   * Origin of `conversationId`, read from the session header on first sight
   * and cached for the rest of the conversation. A session's header never
   * changes; a fork produces a new conversation id and so a fresh lookup.
   */
  function sessionOriginFor(
    conversationId: string,
    sessionManager: SessionHeaderSourceLike | undefined | null,
  ): SessionOrigin {
    const cached = sessionOrigins.get(conversationId);
    if (cached) return cached;
    const origin = resolveSessionOrigin(sessionManager);
    sessionOrigins.set(conversationId, origin);
    return origin;
  }

  /**
   * Current conversation id, or undefined when the runtime cannot report
   * one. Never throws: a failure here must not disable the plugin.
   */
  function readSessionId(
    sessionManager:
      | { getSessionId?: () => string | undefined }
      | undefined
      | null,
  ): string | undefined {
    try {
      return sessionManager?.getSessionId?.() || undefined;
    } catch (err) {
      logger.debug("getSessionId failed", err);
      return undefined;
    }
  }

  function cacheAssistantModel(message: PiAssistantMessage) {
    lastSeenModel = {
      provider: message.provider,
      name: message.model,
    };
  }

  pi.on("session_start", async (event, ctx) => {
    try {
      if (sigil) {
        try {
          await sigil.shutdown();
        } catch (err) {
          logger.error("stale client shutdown failed", err);
        }
      }

      sigil = null;
      const trunk = currentConversation;
      await resetSessionState();

      // Resolve the origin here, while the conversation the session came
      // from is still known: with `--no-session` the header cannot say.
      const startedConversationId = readSessionId(ctx.sessionManager);
      const { startedAt, origin } = resolveSessionStart(ctx.sessionManager, {
        forked: event.reason === "fork",
        trunk,
      });
      if (startedConversationId) {
        currentConversation = {
          id: startedConversationId,
          startedAt: startedAt ?? new Date().toISOString(),
        };
        sessionOrigins.set(startedConversationId, origin);
      } else {
        currentConversation = undefined;
      }

      try {
        config = await loadConfig();
      } catch (err) {
        if (!(err instanceof LocalReceiverError)) throw err;
        // Local mode was chosen for this machine, and no receiver answered.
        // The saved Cloud endpoint is not a fallback: a session told to stay
        // local must not be sent to Cloud because the receiver is down. Pi
        // itself carries on with no capture.
        logger.error("local capture unavailable", err);
        notify(
          ctx,
          `Agent Observability: local capture is off: ${err.message}`,
          "error",
        );
        await resetSessionState();
        return;
      }
      if (!config) return;

      if (!config.agentVersion) {
        config = { ...config, agentVersion: detectPiVersion() };
      }

      // Note: the conversation id a generation carries is read fresh per
      // turn from ctx.sessionManager.getSessionId() so fork/branch
      // reassignments (session-manager.js:927,961) are reflected without
      // restarting the plugin. `currentConversation` above records which
      // conversation this process is on, and is only read when a later fork
      // needs to name its trunk.

      // Set up OTel providers if OTLP is configured.
      // Pass the pi session id as service.instance.id so concurrent pi
      // sessions on the same machine emit distinct OTel metric series.
      if (config.otlp) {
        try {
          const instanceId = startedConversationId ?? randomUUID();
          telemetry = createTelemetryProviders(config.otlp, instanceId);
        } catch (err) {
          logger.error("failed to create OTel providers", err);
        }
      }

      sigil = createAgento11yClient(config, {
        tracer: telemetry?.tracer,
        meter: telemetry?.meter,
      });
      if (!sigil) {
        await resetSessionState();
        return;
      }

      logger.debug(
        `enabled, endpoint=${config.endpoint} auth=${config.auth.mode}${
          config.local ? " local=true" : ""
        }`,
      );
      if (config.local) {
        // Where the session went is not obvious once the endpoint stops being
        // the configured one, so name the receiver the transcript lands in.
        notify(
          ctx,
          `Agent Observability: recording to the local receiver at ${config.endpoint}`,
          "info",
        );
      }
    } catch (err) {
      logger.error("session_start failed", err);
      sigil = null;
      await resetSessionState();
    }
  });

  pi.on("turn_start", async (_event, _ctx) => {
    resetTurnState();
    if (!sigil) return;
    turnStartTime = Date.now();
  });

  pi.on("before_agent_start", async (event, _ctx) => {
    if (!sigil || !config) return;
    try {
      // System prompts encode project conventions (CLAUDE.md, skill text,
      // etc.). Gate on contentCapture the same way `git.branch` is gated.
      if (config.contentCapture === "metadata_only") return;
      const prompt = (event as { systemPrompt?: unknown }).systemPrompt;
      if (typeof prompt === "string" && prompt.length > 0) {
        currentSystemPrompt = prompt;
      }
    } catch (err) {
      logger.error("before_agent_start failed", err);
    }
  });

  pi.on("agent_end", async (_event, _ctx) => {
    // Clear both caches at the agent boundary. `currentRequestControls` is
    // normally cleared in turn_end's finally, but if an agent loop ends
    // without a matching turn_end, those provider settings could otherwise
    // attach to the next agent loop's first exported generation.
    currentSystemPrompt = undefined;
    currentRequestControls = {};
  });

  // Refresh request controls before every provider call. The payload is
  // provider-specific (Anthropic/OpenAI/Gemini), so extraction is purely
  // structural — see `extractRequestControls`. This handler MUST NOT return
  // a value: pi treats a returned value as a payload replacement.
  pi.on("before_provider_request", async (event, _ctx) => {
    if (!sigil) return;
    try {
      const payload = (event as { payload?: unknown }).payload;
      currentRequestControls = extractRequestControls(payload);
    } catch (err) {
      logger.error("before_provider_request failed", err);
      currentRequestControls = {};
    }
  });

  pi.on("message_end", async (event, _ctx) => {
    if (!sigil || !config) return;
    try {
      const message = (event as { message?: unknown }).message;
      const role = (message as { role?: string } | null | undefined)?.role;
      if (role === "assistant") {
        // First write wins: pi emits exactly one assistant message_end per
        // turn, but guard against stray duplicates from extensions so a
        // later (post-tool) timestamp can't displace the real one.
        if (assistantMessageEndTime === 0) {
          assistantMessageEndTime = Date.now();
        }
        if (isAssistantMessage(message)) {
          cacheAssistantModel(message);
        }
        return;
      }
      if (!isUserMessage(message)) return;
      if (firstUserText === undefined) {
        const text = userMessageText(message).trim();
        if (text.length > 0) firstUserText = text;
      }
      const mapped = mapUserMessage(message, config.contentCapture);
      if (mapped) pendingInputMessages.push(mapped);
    } catch (err) {
      logger.error("message_end failed", err);
    }
  });

  // Record the first streamed chunk of an assistant message as the TTFT
  // signal. Pi only emits `message_update` for streamed assistant blocks
  // (text/thinking/toolcall *_start, *_delta, *_end events from
  // AssistantMessageEventStream), so any first occurrence reflects the
  // moment the provider began producing output for this turn.
  pi.on("message_update", async (event, _ctx) => {
    if (!sigil) return;
    if (firstTokenTime !== 0) return;
    const role = (event as { message?: { role?: string } }).message?.role;
    if (role !== "assistant") return;
    firstTokenTime = Date.now();
  });

  pi.on("context", async (event, ctx) => {
    if (!sigil || !config?.guards.enabled) return;
    try {
      const piMessages = event.messages;
      if (!Array.isArray(piMessages) || piMessages.length === 0) return;

      const forward = mapAgentMessagesForHook(piMessages);
      if (forward.length === 0) return;

      const result = await runPreflightTransform({
        client: sigil,
        agentName: config.agentName,
        agentVersion: config.agentVersion,
        model: lastSeenModel ?? { provider: "unknown", name: "unknown" },
        conversationId: readSessionId(ctx.sessionManager),
        messages: forward,
        logger: { warn: (msg: string) => logger.warn(msg) },
      });
      if (!result) return;

      // The server returns one redacted message per forwarded message. If the
      // counts diverge we cannot align the redaction by index, so drop it and
      // forward the originals unchanged.
      if (result.messages.length !== forward.length) {
        logger.debug(
          `preflight transform dropped: got ${result.messages.length} messages, expected ${forward.length}`,
        );
        return;
      }

      const applied = applyRedactedText(piMessages, result.messages);
      if (!applied) {
        logger.debug(
          "preflight transform dropped: could not align redacted messages with the outgoing conversation",
        );
        return;
      }

      // applyRedactedText mutated piMessages (pi's own AgentMessage[]) in
      // place; returning it hands the redacted conversation back to pi.
      return { messages: piMessages };
    } catch (err) {
      // Pi runs `context` handlers serially; any error here would otherwise
      // surface as a turn failure. Drop the transform and let the original
      // messages flow through.
      logger.warn(`context handler failed: ${err}`);
      return;
    }
  });

  pi.on("tool_call", async (event, ctx) => {
    if (!sigil || !config?.guards.enabled) return;
    const res = await runToolCallGuard({
      client: sigil,
      agentName: config.agentName,
      agentVersion: config.agentVersion,
      model: lastSeenModel ?? { provider: "unknown", name: "unknown" },
      conversationId: readSessionId(ctx.sessionManager),
      toolCallId: event.toolCallId,
      toolName: event.toolName,
      input: event.input as Record<string, unknown>,
      failOpen: config.guards.failOpen,
      logger: { warn: (msg: string) => logger.warn(msg) },
    });
    if (!res) return;
    if ("block" in res) return { block: true, reason: res.reason };
    // Postflight transform: the server returns the complete redacted argument
    // set, so replace the tool input wholesale. A plain Object.assign would
    // merge instead, leaving any key the server dropped (an unredacted
    // original) in place. Pi sees the patched arguments at execution time; no
    // re-validation runs after the mutation, so it is the server's
    // responsibility to keep the schema intact.
    //
    // Redaction fails open: if the patch cannot be applied (e.g. event.input
    // is null or frozen), log it and let the original arguments through rather
    // than throwing, which pi would treat as a block.
    try {
      const input = event.input as Record<string, unknown>;
      for (const key of Object.keys(input)) {
        if (!Object.hasOwn(res.transform, key)) {
          delete input[key];
        }
      }
      Object.assign(input, res.transform);
    } catch (err) {
      logger.warn(`tool_call transform apply failed: ${err}`);
    }
    return;
  });

  pi.on("tool_execution_start", async (event, _ctx) => {
    if (!sigil) return;

    try {
      toolStarts.set(event.toolCallId, {
        toolName: event.toolName,
        startedAt: Date.now(),
      });
    } catch (err) {
      logger.error("tool_execution_start failed", err);
    }
  });

  pi.on("tool_execution_end", async (event, _ctx) => {
    if (!sigil) return;

    try {
      const start = toolStarts.get(event.toolCallId);
      if (!start) return;
      toolStarts.delete(event.toolCallId);

      turnToolTimings.push({
        toolCallId: event.toolCallId,
        toolName: start.toolName,
        startedAt: start.startedAt,
        completedAt: Date.now(),
        isError: event.isError,
      });
    } catch (err) {
      logger.error("tool_execution_end failed", err);
    }
  });

  pi.on("turn_end", async (event, ctx) => {
    if (!sigil || !config) return;

    try {
      if (!isAssistantMessage(event.message)) {
        logger.warn(
          "turn_end: assistant message shape did not validate, skipping",
        );
        return;
      }

      const msg = event.message;
      const contentCapture = config.contentCapture;

      // Build the active tool catalog from pi's registry. Prefer the active
      // set (what the model was offered this turn) so evaluators can
      // compute precision/recall. `null` means the active-set API is
      // unavailable (older pi versions); an empty Set means it returned
      // explicitly no tools — those are different cases and `mapTools`
      // treats them differently.
      let toolCatalog: PiToolInfo[] = [];
      try {
        toolCatalog = pi.getAllTools?.() ?? [];
      } catch (err) {
        logger.debug("getAllTools failed", err);
        toolCatalog = [];
      }
      let activeNames: Set<string> | null = null;
      try {
        const active = pi.getActiveTools?.();
        if (Array.isArray(active)) activeNames = new Set(active);
      } catch (err) {
        logger.debug("getActiveTools failed", err);
      }
      if (
        toolCatalog.length === 0 &&
        activeNames !== null &&
        activeNames.size > 0
      ) {
        // getAllTools threw or returned [] but getActiveTools still gave us
        // names — synthesize name-only defs so the seed records the tools
        // pi actually offered the model. Without this, `mapTools` would
        // filter an empty catalog and drop the tool list entirely.
        toolCatalog = Array.from(activeNames).map((name) => ({ name }));
      } else if (activeNames === null && toolCatalog.length === 0) {
        // Neither catalog nor active-set API — fall back to the called-tools
        // subset so older pi versions still emit something useful.
        const calledNames = new Set(turnToolTimings.map((t) => t.toolName));
        toolCatalog = Array.from(calledNames).map((name) => ({ name }));
        activeNames = calledNames;
      }
      const toolDefs = mapTools(toolCatalog, activeNames, contentCapture);

      // Read the current sessionId every turn. SessionManager reassigns
      // sessionId on fork/branch, and extensions that spawn child sessions
      // can share a literal filename (e.g. "session.jsonl") across runs —
      // so file-path-derived ids collapse multiple sessions into one.
      // getSessionId() is the stable unique identifier
      // (session-manager.d.ts: ReadonlySessionManager).
      const conversationId = ctx.sessionManager.getSessionId() || undefined;

      // Prefer the assistant `message_end` timestamp captured above (fires
      // right after the provider stream ends, before tools execute). Fall
      // back to `msg.timestamp` only when no assistant message_end was
      // observed — pi providers set `msg.timestamp` when constructing the
      // assistant message object (before the HTTP request), so it sits near
      // turnStartTime, not at stream completion. The Math.min clamp guards
      // against clock adjustments inverting startedAt and completedAt.
      const completedAtMs =
        assistantMessageEndTime > 0 ? assistantMessageEndTime : msg.timestamp;
      const startedAtMs = Math.min(
        turnStartTime || completedAtMs,
        completedAtMs,
      );

      // Resolved per turn so mid-session checkouts land on the next
      // generation. Always sent, regardless of content capture mode:
      // `git.branch` and `cwd` are low-cardinality session metadata,
      // not message content, matching claude-code/cursor.
      const turnCwd = process.cwd();
      const builtinTags = buildBuiltinTags({
        cwd: turnCwd,
        gitBranch: resolveGitBranch(turnCwd),
      });

      // Resolve lineage at `turn_end`, not `message_end`: pi awaits
      // extension `message_end` callbacks *before* calling
      // `sessionManager.appendMessage` (see agent-session.js `_publish`),
      // so the assistant entry is not yet in the tree at that point. By
      // `turn_end` it has been appended and any subsequent extension
      // mutations have settled.
      //
      // A fork's parent turn may belong to the trunk conversation, in which
      // case no edge is emitted. See sessionOrigin.ts.
      const origin: SessionOrigin = conversationId
        ? sessionOriginFor(conversationId, ctx.sessionManager)
        : NOT_FORKED;
      const { generationId, parent } = resolvePiGenerationLineage(
        ctx.sessionManager,
        msg,
        conversationId,
        { origin },
      );

      // Prefer pi's user-set session name; otherwise derive from the first
      // prompt (suppressed in metadata_only). Resolved per turn so a name
      // set mid-session shows up on the next generation.
      let sessionName: string | undefined;
      try {
        sessionName = ctx.sessionManager.getSessionName?.();
      } catch (err) {
        logger.debug("getSessionName failed", err);
      }
      const conversationTitle = resolveConversationTitle({
        sessionName,
        firstUserText,
        conversationId,
        contentCapture,
      });

      const seed = mapGenerationStart(msg, {
        conversationId,
        conversationTitle,
        agentName: config.agentName,
        agentVersion: config.agentVersion,
        startedAt: startedAtMs,
        tools: toolDefs.length > 0 ? toolDefs : undefined,
        tags: builtinTags,
        systemPrompt: currentSystemPrompt,
        requestControls: currentRequestControls,
        generationId,
        parentGenerationIds: parentEdge(parent),
        metadata: forkMetadata(parent, origin),
      });

      const toolResults = (event.toolResults ?? []) as PiToolResult[];
      // Snapshot the buffer; the finally below clears it in place and
      // GenerationResult.input would otherwise alias the cleared array.
      const result = mapGenerationResult(
        msg,
        toolResults,
        contentCapture,
        pendingInputMessages.slice(),
        completedAtMs,
      );

      try {
        // Pi streams provider responses (see message_update handler above),
        // so generations are exported with mode=STREAM. The SDK only records
        // the gen_ai.client.time_to_first_token histogram when the operation
        // is `streamText`, which `startStreamingGeneration` sets by default.
        await sigil.startStreamingGeneration(seed, async (recorder) => {
          if (firstTokenTime > 0) {
            recorder.setFirstTokenAt(new Date(firstTokenTime));
          }
          recorder.setResult(result);
          if (msg.errorMessage) {
            recorder.setCallError(new Error(msg.errorMessage));
          }

          // sigil and config are guaranteed non-null by the guard at the top of this handler.
          emitToolSpans(
            sigil as Agento11yClient,
            msg,
            toolResults,
            turnToolTimings,
            {
              conversationId,
              conversationTitle,
              agentName: (config as Agento11yPiConfig).agentName,
              agentVersion: (config as Agento11yPiConfig).agentVersion,
              contentCapture,
            },
          );
        });
        logger.debug(
          `generation queued, model=${msg.model} tokens=${msg.usage.totalTokens}`,
        );
      } catch (err) {
        logger.debug("generation export failed", err);
      }
      if (telemetry) {
        void telemetry.forceFlush().catch((err) => {
          logger.debug("telemetry flush failed", err);
        });
      }
    } catch (err) {
      logger.error("turn_end failed", err);
    } finally {
      toolStarts.clear();
      turnToolTimings.length = 0;
      pendingInputMessages.length = 0;
      currentRequestControls = {};
    }
  });

  // Pi runs compaction and branch summarization outside the agent loop, so
  // none of the turn/message events fire for them. The `session_before_*`
  // events are the only start signal, and registering a handler is what makes
  // pi emit them at all (`hasHandlers`-gated). These handlers must not return
  // a value: pi reads a returned object as a cancel/replace instruction.
  pi.on("session_before_compact", async (_event, _ctx) => {
    compactionStartedAt = Date.now();
    // Manual /compact disconnects from the agent before aborting the in-flight
    // turn, so that turn's `turn_end` never reaches us. Drop its buffered
    // state here instead of letting the abandoned prompt and stale timers
    // attach to the next exported generation. Threshold and overflow
    // compaction run after `turn_end`, where this is a no-op: the buffer is
    // already drained and the next `turn_start` resets the timers.
    resetTurnState();
    pendingInputMessages.length = 0;
    currentRequestControls = {};
  });

  pi.on("session_before_tree", async (_event, _ctx) => {
    branchSummaryStartedAt = Date.now();
  });

  pi.on("session_compact", async (event, ctx) => {
    // Consume the pending start unconditionally, including on the skip paths
    // below: a start that produced no export must not be reused by the next
    // compaction.
    const startedAt = compactionStartedAt;
    compactionStartedAt = undefined;
    try {
      if (event.fromExtension === true) {
        logger.debug("compaction: skipped, supplied by an extension");
        return;
      }
      // `reason` and `willRetry` were added to this event after the pinned pi
      // version, so read them structurally.
      const raw = event as { reason?: unknown; willRetry?: unknown };
      await exportSummaryGeneration("compaction", event.compactionEntry, ctx, {
        startedAt,
        reason: typeof raw.reason === "string" ? raw.reason : undefined,
        willRetry:
          typeof raw.willRetry === "boolean" ? raw.willRetry : undefined,
      });
    } catch (err) {
      logger.error(
        `session_compact failed, entry=${entryIdOf(event.compactionEntry)}`,
        err,
      );
    }
  });

  pi.on("session_tree", async (event, ctx) => {
    const startedAt = branchSummaryStartedAt;
    branchSummaryStartedAt = undefined;
    try {
      // `session_tree` fires for every navigation; only the summarizing ones
      // carry a summary entry, and only those involved a model call.
      if (!event.summaryEntry) return;
      if (event.fromExtension === true) {
        logger.debug("branch_summary: skipped, supplied by an extension");
        return;
      }
      await exportSummaryGeneration("branch_summary", event.summaryEntry, ctx, {
        startedAt,
      });
    } catch (err) {
      logger.error(
        `session_tree failed, entry=${entryIdOf(event.summaryEntry)}`,
        err,
      );
    }
  });

  pi.on("session_shutdown", async (_event, _ctx) => {
    if (sigil) {
      try {
        await sigil.shutdown();
      } catch (err) {
        logger.error("session shutdown failed", err);
      }
    }

    sigil = null;
    lastSeenModel = null;
    await resetSessionState();
  });
}

/**
 * Slice of pi's `ExtensionContext` used to reach the user. Structural and
 * fully optional: pi only has a UI in TUI and RPC modes, and `-p` / JSON runs
 * pass a context whose notify would go nowhere.
 */
interface PiNotifyContext {
  hasUI?: boolean;
  ui?: {
    notify?: (message: string, type?: "info" | "warning" | "error") => void;
  };
}

/**
 * Tells the user something the debug log alone would not surface. Silent
 * without a UI, and a failing notify never breaks the handler that called it.
 */
function notify(
  ctx: PiNotifyContext,
  message: string,
  type: "info" | "warning" | "error",
): void {
  if (!ctx?.hasUI) return;
  try {
    ctx.ui?.notify?.(message, type);
  } catch (err) {
    logger.debug("ui notify failed", err);
  }
}

/**
 * Slice of pi's `ExtensionContext` the summary export path reads. Structural
 * so the test fakes stay small and so `ctx.model` (typed as `Model<any>`) can
 * be read without importing pi's model types.
 */
interface PiSummaryContext {
  sessionManager?: SessionManagerLike &
    SessionHeaderSourceLike & {
      getSessionId?: () => string | undefined;
      getSessionName?: () => string | undefined;
    };
  model?: { provider?: unknown; id?: unknown };
}

/**
 * The parent edge to export. Only a parent generation that belongs to this
 * conversation can be one; see `lineage.ts`.
 */
function parentEdge(
  parent: PiGenerationParent | undefined,
): string[] | undefined {
  return parent?.kind === "own" ? parent.generationIds : undefined;
}

/**
 * The trunk link a fork cannot express as an edge.
 *
 * It ships as metadata because the trunk generation only exists in the
 * backend if the trunk itself ran instrumented, and a fork can be taken from
 * a session recorded before this plugin was installed. Metadata, not a tag,
 * because tags are low-cardinality session context (`cwd`, `git.branch`) and
 * a session id is not. The key names follow `codex.parent_session_id`
 * (plugins/agento11y/internal/agents/codex/mapper/mapper.go), which emits the
 * edge when it can resolve the parent generation and falls back to metadata
 * when it cannot.
 */
function forkMetadata(
  parent: PiGenerationParent | undefined,
  origin: SessionOrigin,
): Record<string, string> | undefined {
  if (parent?.kind !== "trunk") return undefined;
  if (!parent.trunkGenerationId || !origin.trunkConversationId) {
    return undefined;
  }
  return {
    "pi.fork.parent_session_id": origin.trunkConversationId,
    "pi.fork.parent_generation_id": parent.trunkGenerationId,
  };
}

/**
 * Pick the entry to export.
 *
 * Only compaction needs a lookup of its own. `session_compact` resolves its
 * entry by summary text and takes the first match in the whole session file,
 * so two byte-identical summaries make the event point at an older entry. The
 * correction is positional, not textual: pi appends the compaction entry and
 * emits the event immediately after, so the newest `compaction` entry on the
 * active branch is the one that was just written. `session_tree` looks its
 * entry up by id (`getEntry(summaryId)`), so its event entry is already exact
 * and is used as-is.
 */
function resolveSummaryEntry(
  kind: PiSummaryKind,
  sessionManager: PiSummaryContext["sessionManager"],
  eventEntry: unknown,
): PiSummaryEntryLike | null {
  if (kind !== "compaction") return toPiSummaryEntry(eventEntry);
  try {
    const branch = sessionManager?.getBranch?.();
    if (Array.isArray(branch)) {
      for (let i = branch.length - 1; i >= 0; i--) {
        const candidate = branch[i] as { type?: unknown } | undefined;
        if (!candidate || candidate.type !== "compaction") continue;
        const mapped = toPiSummaryEntry(candidate);
        if (mapped?.id) return mapped;
        break;
      }
    }
  } catch (err) {
    logger.debug("getBranch failed while resolving the summary entry", err);
  }
  return toPiSummaryEntry(eventEntry);
}

/** Entry id for a log line, or `unknown` when the value carries none. */
function entryIdOf(value: unknown): string {
  return toPiSummaryEntry(value)?.id ?? "unknown";
}

/**
 * Read pi's `CompactionEntry` / `BranchSummaryEntry` structurally. `usage` is
 * absent from the dev-pinned pi types and `tokensBefore` only exists on
 * compaction entries, so nothing here may assume a field is present.
 */
function toPiSummaryEntry(value: unknown): PiSummaryEntryLike | null {
  if (!value || typeof value !== "object") return null;
  const src = value as Record<string, unknown>;
  const entry: PiSummaryEntryLike = {};
  if (typeof src.id === "string" && src.id.length > 0) entry.id = src.id;
  if (typeof src.timestamp === "string") entry.timestamp = src.timestamp;
  if (typeof src.summary === "string") entry.summary = src.summary;
  if (typeof src.tokensBefore === "number") {
    entry.tokensBefore = src.tokensBefore;
  }
  if (src.fromHook === true) entry.fromHook = true;
  const usage = toPiUsage(src.usage);
  if (usage) entry.usage = usage;
  return entry;
}

/**
 * Copy the numeric fields of a pi `Usage` object. Returns `undefined` when the
 * value is not an object or carries no finite number, so a malformed usage
 * object is indistinguishable from an absent one. Whether the token block
 * itself is exported is decided by `mapSummaryGenerationResult`, which needs
 * at least one token count and ignores a cost-only object.
 */
function toPiUsage(value: unknown): PiUsageLike | undefined {
  if (!value || typeof value !== "object") return undefined;
  const src = value as Record<string, unknown>;
  const out: PiUsageLike = {};
  let seen = false;
  for (const key of PI_USAGE_TOKEN_FIELDS) {
    const raw = src[key];
    if (typeof raw === "number" && Number.isFinite(raw)) {
      out[key] = raw;
      seen = true;
    }
  }
  const cost = src.cost;
  if (cost && typeof cost === "object") {
    const total = (cost as Record<string, unknown>).total;
    if (typeof total === "number" && Number.isFinite(total)) {
      out.cost = { total };
      seen = true;
    }
  }
  return seen ? out : undefined;
}

/**
 * Emit one `execute_tool` span per tool call in the turn.
 *
 * Arguments and results are redacted here. This is a second boundary: the SDK's
 * generation sanitizer scrubs the copy of the same bytes that rides in the
 * generation payload, but it does not see a tool-execution span, so the span
 * needs its own pass. Full strength (tier 1 + tier 2), which is what the
 * sanitizer applies to a tool payload.
 *
 * @internal Exported for testing.
 */
export function emitToolSpans(
  client: Agento11yClient,
  msg: PiAssistantMessage,
  toolResults: PiToolResult[],
  timings: ToolTiming[],
  opts: {
    conversationId?: string;
    conversationTitle?: string;
    agentName: string;
    agentVersion?: string;
    contentCapture: ContentCaptureMode;
  },
): void {
  if (timings.length === 0) return;

  const includeContent = opts.contentCapture === "full";

  const argsMap = new Map<string, string>();
  const resultMap = new Map<string, string>();
  if (includeContent) {
    for (const block of msg.content) {
      if (block.type !== "toolCall") continue;
      const encoded = JSON.stringify(block.arguments);
      if (encoded === undefined) continue;
      argsMap.set(block.id, redactSecretText(encoded));
    }
    for (const tr of toolResults) {
      resultMap.set(
        tr.toolCallId,
        redactSecretText(toolResultText(tr.content)),
      );
    }
  }

  for (const timing of timings) {
    try {
      const toolRec = client.startToolExecution({
        toolName: timing.toolName,
        toolCallId: timing.toolCallId,
        toolType: "function",
        conversationId: opts.conversationId,
        conversationTitle: opts.conversationTitle,
        agentName: opts.agentName,
        agentVersion: opts.agentVersion,
        requestModel: msg.model,
        requestProvider: msg.provider,
        startedAt: new Date(timing.startedAt),
        contentCapture: opts.contentCapture,
      });

      const end: {
        arguments?: unknown;
        result?: unknown;
        completedAt: Date;
      } = {
        completedAt: new Date(timing.completedAt),
      };

      if (includeContent) {
        const args = argsMap.get(timing.toolCallId);
        if (args !== undefined) {
          end.arguments = args;
        }
        const trContent = resultMap.get(timing.toolCallId);
        if (trContent !== undefined) {
          end.result = trContent;
        }
      }

      if (timing.isError) {
        toolRec.setCallError(new Error("tool returned error"));
      }

      toolRec.setResult(end);
      toolRec.end();
    } catch (err) {
      logger.error(`failed to emit tool span for ${timing.toolName}`, err);
    }
  }
}

function isAssistantMessage(message: unknown): message is PiAssistantMessage {
  if (!message || typeof message !== "object") return false;
  const candidate = message as Partial<PiAssistantMessage>;

  return (
    candidate.role === "assistant" &&
    typeof candidate.provider === "string" &&
    typeof candidate.model === "string" &&
    typeof candidate.timestamp === "number" &&
    !!candidate.usage &&
    Array.isArray(candidate.content) &&
    typeof candidate.stopReason === "string"
  );
}

function isUserMessage(message: unknown): message is PiUserMessage {
  if (!message || typeof message !== "object") return false;
  const candidate = message as Partial<PiUserMessage>;
  return (
    candidate.role === "user" &&
    (typeof candidate.content === "string" ||
      Array.isArray(candidate.content)) &&
    typeof candidate.timestamp === "number"
  );
}
