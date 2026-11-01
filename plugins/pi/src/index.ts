import { randomUUID } from "node:crypto";
import type {
  Agento11yClient,
  ContentCaptureMode,
  ToolExecutionRecorder,
  ToolExecutionStart,
} from "@grafana/agento11y";
import { redactSecretText } from "@grafana/agento11y";
import type {
  ExtensionAPI,
  ExtensionContext,
  SessionBeforeCompactEvent,
  SessionBeforeTreeEvent,
} from "@mariozechner/pi-coding-agent";
import { createAgento11yClient, reportExportFailure } from "./client.js";
import type { Agento11yPiConfig, GuardsFeatureConfig } from "./config.js";
import { loadConfig, loadGuardIntent } from "./config.js";
import { detectPiVersion } from "./detectPiVersion.js";
import { resolveGitBranch } from "./git.js";
import {
  evaluatePreflight,
  formatEvalFailure,
  runToolCallGuard,
} from "./guard.js";
import {
  applyGuardTransform,
  projectGuardMessages,
} from "./guardProjection.js";
import {
  type PiGenerationParent,
  resolvePiGenerationLineage,
  resolvePiSummaryLineage,
  type SessionManagerLike,
} from "./lineage.js";
import { LocalReceiverError } from "./local.js";
import { logger } from "./logger.js";
import {
  mapGenerationResult,
  mapGenerationStart,
  mapSummaryGenerationResult,
  mapSummaryGenerationStart,
  PI_USAGE_TOKEN_FIELDS,
  type PiAssistantMessage,
  type PiSummaryEntryLike,
  type PiSummaryKind,
  type PiToolResult,
  type PiUsageLike,
  redactSharedWords,
  redactSpans,
  removedSpans,
  resolveConversationTitle,
  toolResultText,
} from "./mappers.js";
import { RequestObservation } from "./requestObservation.js";
import {
  NOT_FORKED,
  resolveSessionStart,
  type SessionHeaderSourceLike,
  type SessionOrigin,
} from "./sessionOrigin.js";
import { type SkillIdentity, snapshotSkills } from "./skills.js";
import { guardCompaction, guardTreeSummary } from "./summaryGuards.js";
import { buildBuiltinTags, effectiveTags } from "./tags.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";
import {
  ToolExecutions,
  type ToolObservation,
  toolParents,
} from "./toolExecutions.js";

type GuardReadiness =
  | { kind: "disabled" }
  | { kind: "unavailable"; failOpen: boolean; reason: string }
  | { kind: "ready"; client: Agento11yClient };

export default function (pi: ExtensionAPI) {
  let sigil: Agento11yClient | null = null;
  let config: Agento11yPiConfig | null = null;
  let telemetry: TelemetryProviders | null = null;
  let guardIntent: GuardsFeatureConfig | undefined;
  let guardReadiness: GuardReadiness = { kind: "disabled" };
  let guardUnavailableWarned = false;

  function invalidateGuards(reason: string) {
    guardReadiness = guardIntent?.enabled
      ? { kind: "unavailable", failOpen: guardIntent.failOpen, reason }
      : { kind: "disabled" };
  }
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

  // Keep the mutable prompt options only until agent_start sees the final catalog.
  let pendingSkillOptions: { value: unknown } | undefined;
  let skillCatalog: SkillIdentity[] = [];

  const toolExecutions = new ToolExecutions();

  const requestObservation = new RequestObservation();

  // Text a redact guard removed from this session's prompts. The title can
  // come from pi's session name, which no guard sees, so title sources are
  // redacted with these too. Cleared on session boundaries, like
  // firstUserText.
  const guardSpans = new Set<string>();
  // Prompts a guard redacted whole, so their sensitive part is unknown.
  const wholeRedactions = new Set<string>();

  /** Redacts what a guard removed this session from a prompt's text. */
  const guardSafe = (value: string | undefined): string | undefined =>
    value === undefined || guardSpans.size === 0
      ? value
      : redactSpans(value, guardSpans);

  /**
   * Redacts what a guard removed this session from pi's session name. The
   * name can quote part of a prompt the guard redacted whole, so its words
   * that appear in such a prompt are redacted too.
   */
  const guardSafeName = (value: string | undefined): string | undefined => {
    const out = guardSafe(value);
    return out === undefined ? out : redactSharedWords(out, wholeRedactions);
  };

  // Pi exposes no tree-failure event. New starts overwrite pending timing
  // so an unobserved failure cannot time a later operation.
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
  let sessionOrigins = new WeakMap<
    SessionHeaderSourceLike,
    { id: string; origin: SessionOrigin }
  >();

  function resetTurnState() {
    turnStartTime = 0;
    firstTokenTime = 0;
    assistantMessageEndTime = 0;
    toolExecutions.reset();
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
    requestObservation.reset({});
    guardSpans.clear();
    wholeRedactions.clear();
    lastSeenModel = null;
    pendingSkillOptions = undefined;
    skillCatalog = [];
    compactionStartedAt = undefined;
    branchSummaryStartedAt = undefined;
    exportedSummaryEntryIds.clear();
    sessionOrigins = new WeakMap();
  }

  // The selected model is context only; Pi exposes no physical summary identity.
  function resolveSummaryModel(
    ctx: PiSummaryContext,
  ): { provider: string; name: string } | undefined {
    const provider = ctx?.model?.provider;
    const id = ctx?.model?.id;
    if (
      typeof provider === "string" &&
      provider.length > 0 &&
      typeof id === "string" &&
      id.length > 0
    ) {
      // Display names can differ from the selected model's stable id.
      return { provider, name: id };
    }
    return undefined;
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

    const summaryCwd = ctx.cwd;
    const tags = buildBuiltinTags({
      cwd: summaryCwd,
      gitBranch: summaryCwd ? resolveGitBranch(summaryCwd) : undefined,
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
        sessionName: guardSafeName(sessionName),
        firstUserText: guardSafe(requestObservation.firstUserText),
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
      reason: extras.reason,
      willRetry: extras.willRetry,
    });

    // SYNC, not STREAM: there is no token stream and no TTFT signal for these
    // calls. The SYNC path defaults `operationName` to `generateText`, which
    // also discriminates them from turns (`streamText`).
    try {
      await sigil.startGeneration(seed, async (recorder) => {
        recorder.setResult(result);
      });
    } catch (err) {
      reportExportFailure(sigil);
      throw err;
    }
    // Dedup tracks local enqueue, not delivery acknowledgment.
    // A later event can retry only a failed enqueue.
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
    const cached = sessionManager && sessionOrigins.get(sessionManager);
    if (cached && cached.id === conversationId) return cached.origin;
    const { origin } = resolveSessionStart(sessionManager, { forked: false });
    if (sessionManager)
      sessionOrigins.set(sessionManager, { id: conversationId, origin });
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
    guardUnavailableWarned = false;
    invalidateGuards("initialization incomplete");
    try {
      if (sigil) {
        try {
          await sigil.shutdown();
        } catch (err) {
          reportExportFailure(sigil);
          logger.error("stale client shutdown failed", err);
        }
      }

      sigil = null;
      await resetSessionState();
      requestObservation.reset(ctx.sessionManager);
      guardIntent = loadGuardIntent();
      invalidateGuards("initialization incomplete");

      const startedConversationId = readSessionId(ctx.sessionManager);
      const { origin } = resolveSessionStart(ctx.sessionManager, {
        forked: event.reason === "fork",
      });
      if (startedConversationId) {
        sessionOrigins.set(ctx.sessionManager, {
          id: startedConversationId,
          origin,
        });
      }

      try {
        config = await loadConfig(ctx.cwd, guardIntent);
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
      guardIntent = config.guards ?? guardIntent;
      invalidateGuards("client unavailable");

      if (!config.agentVersion) {
        config = { ...config, agentVersion: detectPiVersion() };
      }

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
        ...(ctx.hasUI && typeof ctx.ui?.notify === "function"
          ? {
              onExportFailure: (message: string) =>
                ctx.ui.notify(message, "warning"),
            }
          : {}),
      });
      if (!sigil) {
        await resetSessionState();
        return;
      }

      if (guardIntent?.enabled)
        guardReadiness = { kind: "ready", client: sigil };

      logger.debug(
        `enabled, auth=${config.auth.mode}${config.local ? " local=true" : ""}`,
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
      invalidateGuards("initialization failed");
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
    pendingSkillOptions = { value: undefined };
    try {
      pendingSkillOptions.value = (
        event as { systemPromptOptions?: unknown }
      ).systemPromptOptions;
    } catch (err) {
      logger.error("before_agent_start failed", err);
    }
  });

  pi.on("agent_start", async (_event, ctx) => {
    if (!pendingSkillOptions) return;
    const options = pendingSkillOptions.value;
    pendingSkillOptions = undefined;
    skillCatalog = [];
    skillCatalog = snapshotSkills(options, ctx.cwd);
  });

  pi.on("agent_settled", async () => {
    // agent_end can precede a retry without another before_agent_start.
    skillCatalog = [];
    pendingSkillOptions = undefined;
    resetTurnState();
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

  pi.on("context_with_system", async (event, ctx) => {
    if (!config) return;
    let observed = event.messages;
    requestObservation.observe(ctx.sessionManager, []);
    try {
      if (guardReadiness.kind === "ready") {
        const projection = projectGuardMessages(event.messages);
        if (projection.messages.length > 0) {
          const result = await evaluatePreflight({
            client: guardReadiness.client,
            agentName: config.agentName,
            agentVersion: config.agentVersion,
            model: currentGuardModel(ctx.model),
            conversationId: readSessionId(ctx.sessionManager),
            tags: effectiveTags(
              config,
              buildBuiltinTags({
                cwd: ctx.cwd,
                gitBranch: resolveGitBranch(ctx.cwd),
              }),
            ),
            messages: projection.messages,
          });
          if (result.kind === "transform") {
            observed = applyGuardTransform(
              event.messages,
              projection,
              result.input.messages,
            );
            const transformed = projectGuardMessages(observed);
            for (const slot of projection.slots) {
              const before =
                projection.messages[slot.message]?.parts?.[slot.part];
              const after =
                transformed.messages[slot.message]?.parts?.[slot.part];
              if (before?.type !== "text" || after?.type !== "text") continue;
              for (const span of removedSpans(before.text, after.text)) {
                guardSpans.add(span);
                if (span === before.text.trim()) wholeRedactions.add(span);
              }
            }
          } else if (result.kind !== "allow") {
            logger.warn(
              "Agent Observability: ordinary preflight unavailable or refused; Pi cannot cancel at this hook.",
            );
            return;
          }
        }
      }
      requestObservation.observe(ctx.sessionManager, observed);
      return observed === event.messages ? undefined : { messages: observed };
    } catch {
      logger.warn(
        "Agent Observability: preflight transform dropped because its source could not be validated.",
      );
      return;
    }
  });

  pi.on("tool_call", async (event, ctx) => {
    if (guardReadiness.kind === "unavailable") {
      if (!guardReadiness.failOpen) {
        return {
          block: true,
          reason: formatEvalFailure(event.toolName, undefined),
        };
      }
      if (!guardUnavailableWarned) {
        guardUnavailableWarned = true;
        const warning =
          "Agent Observability: guards are unavailable; tool calls are allowed by fail-open configuration.";
        logger.warn(warning);
        if (ctx.hasUI) notify(ctx, warning, "warning");
        else {
          try {
            process.stderr.write(`${warning}\n`);
          } catch {}
        }
      }
    }
    if (!sigil) return;
    toolExecutions.guardInput(event, skillCatalog, ctx.cwd);
    if (guardReadiness.kind !== "ready" || !config) return;
    const res = await runToolCallGuard({
      client: guardReadiness.client,
      agentName: config.agentName,
      agentVersion: config.agentVersion,
      model: lastSeenModel ?? { provider: "unknown", name: "unknown" },
      conversationId: readSessionId(ctx.sessionManager),
      toolCallId: event.toolCallId,
      toolName: event.toolName,
      input: event.input as Record<string, unknown>,
      tags: effectiveTags(
        config,
        buildBuiltinTags({
          cwd: ctx.cwd,
          gitBranch: resolveGitBranch(ctx.cwd),
        }),
      ),
      failOpen: guardIntent?.failOpen ?? true,
      logger: { warn: (msg: string) => logger.warn(msg) },
    });
    if (!res) return;
    toolExecutions.guardChanged(
      event,
      "transform" in res ? res.transform : undefined,
    );
    if ("block" in res) return { block: true, reason: res.reason };
    // Postflight transform: the server returns the complete redacted argument
    // set, so replace the tool input wholesale. A plain Object.assign would
    // merge instead, leaving any key the server dropped (an unredacted
    // original) in place. Pi sees the patched arguments at execution time; no
    // re-validation runs after the mutation, so it is the server's
    // responsibility to keep the schema intact.
    //
    // If the patch cannot be applied (e.g. event.input is null or frozen),
    // log it and follow the configured failure policy rather than throwing,
    // which pi would treat as a block.
    try {
      const input = event.input as Record<string, unknown>;
      for (const key of Object.keys(input)) {
        if (!Object.hasOwn(res.transform, key)) {
          delete input[key];
        }
      }
      Object.assign(input, res.transform);
    } catch {
      logger.warn("tool_call transform apply failed");
      if (guardIntent?.failOpen === false) {
        return {
          block: true,
          reason:
            "Agent Observability could not apply a required guard transformation.",
        };
      }
    }
    toolExecutions.guardInput(event, skillCatalog, ctx.cwd, true);
    return;
  });

  pi.on("tool_result", async (event, ctx) => {
    if (!sigil || !("input" in event)) return;
    toolExecutions.resultInput(event, skillCatalog, ctx.cwd);
  });

  pi.on("tool_execution_start", async (event, _ctx) => {
    if (!sigil) return;

    try {
      toolExecutions.start(event);
    } catch (err) {
      logger.error("tool_execution_start failed", err);
    }
  });

  pi.on("tool_execution_end", async (event, _ctx) => {
    if (!sigil) return;

    try {
      toolExecutions.end(event, skillCatalog);
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

      // Pi exposes no final offered catalog; these names describe observed proposals only.
      const calledNames = new Set(
        msg.content.flatMap((part) =>
          part.type === "toolCall" ? [part.name] : [],
        ),
      );
      const toolDefs = Array.from(calledNames, (name) => ({ name }));

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
      const turnCwd = ctx.cwd;
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
      const observedInput = requestObservation.consume(
        ctx.sessionManager,
        contentCapture,
      );
      const conversationTitle = resolveConversationTitle({
        sessionName: guardSafeName(sessionName),
        firstUserText: guardSafe(requestObservation.firstUserText),
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
        generationId,
        parentGenerationIds: parentEdge(parent),
        metadata: forkMetadata(parent, origin),
      });

      const toolResults = toolExecutions.safeResults(
        (event.toolResults ?? []) as PiToolResult[],
      );
      const result = mapGenerationResult(
        toolExecutions.safeMessage(msg),
        toolResults,
        contentCapture,
        observedInput,
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
            (start) => recorder.startToolExecution(start),
            msg,
            toolResults,
            toolExecutions.completed(),
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
        reportExportFailure(sigil);
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
      toolExecutions.reset();
    }
  });

  async function protectSummary(
    event: SessionBeforeCompactEvent | SessionBeforeTreeEvent,
    ctx: ExtensionContext,
  ) {
    if (
      event.type === "session_before_tree" &&
      !event.preparation.userWantsSummary
    )
      return;
    if (event.signal?.aborted) return { cancel: true };
    if (guardReadiness.kind === "disabled") return;
    const warn = (message: string) => {
      logger.warn(message);
      if (ctx.hasUI) notify(ctx, message, "warning");
      else {
        try {
          process.stderr.write(`${message}\n`);
        } catch {}
      }
    };
    if (guardReadiness.kind === "unavailable" || !config) {
      const failOpen =
        guardReadiness.kind === "unavailable" && guardReadiness.failOpen;
      warn(
        failOpen
          ? "Agent Observability: summary guards are unavailable; continuing with fail-open enabled."
          : "Agent Observability: summary cancelled because guards are unavailable.",
      );
      return failOpen ? undefined : { cancel: true };
    }
    const args = {
      client: guardReadiness.client,
      agentName: config.agentName,
      agentVersion: config.agentVersion,
      model: currentGuardModel(ctx.model),
      conversationId: readSessionId(ctx.sessionManager),
      tags: effectiveTags(config, {
        ...buildBuiltinTags({
          cwd: ctx.cwd,
          gitBranch: resolveGitBranch(ctx.cwd),
        }),
        "pi.call_kind":
          event.type === "session_before_compact"
            ? "compaction"
            : "branch_summary",
      }),
      failOpen: guardIntent?.failOpen ?? true,
      logger: { warn },
    };
    return event.type === "session_before_compact"
      ? guardCompaction(event, args)
      : guardTreeSummary(event, args);
  }

  // Pi runs compaction and branch summarization outside the agent loop, so
  // none of the turn/message events fire for them. The `session_before_*`
  // events are the only start signal, and registering a handler is what makes
  // pi emit them at all (`hasHandlers`-gated).
  pi.on("session_before_compact", async (event, ctx) => {
    compactionStartedAt = Date.now();
    // Legacy manual compaction can abandon a turn. Automatic compaction
    // also runs after turn_start and must retain that turn's observations.
    if (event.reason === "manual") {
      resetTurnState();
    }
    const result = await protectSummary(event, ctx);
    if (result?.cancel) compactionStartedAt = undefined;
    return result;
  });

  pi.on("session_before_tree", async (event, ctx) => {
    branchSummaryStartedAt = Date.now();
    const result = await protectSummary(event, ctx);
    if (result?.cancel) branchSummaryStartedAt = undefined;
    return result;
  });

  pi.on("session_compact_failed", async () => {
    compactionStartedAt = undefined;
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
      // `reason` and `willRetry` can be absent on summary events.
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
    invalidateGuards("session ended");
    if (sigil) {
      try {
        await sigil.shutdown();
      } catch (err) {
        reportExportFailure(sigil);
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
  cwd?: string;
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
 * Older summary entries can omit usage; only compaction entries have tokensBefore.
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
  for (const key of [...PI_USAGE_TOKEN_FIELDS, "cacheWrite1h"] as const) {
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
 * Generation sanitizers do not process tool spans. Redact live input and final
 * results independently, using both secret-redaction tiers.
 *
 * @internal Exported for testing.
 */
export function emitToolSpans(
  startToolExecution: (start: ToolExecutionStart) => ToolExecutionRecorder,
  msg: PiAssistantMessage,
  toolResults: PiToolResult[],
  observations: ToolObservation[],
  opts: {
    conversationId?: string;
    conversationTitle?: string;
    agentName: string;
    agentVersion?: string;
    contentCapture: ContentCaptureMode;
  },
): void {
  const includeContent = opts.contentCapture === "full";
  const finalizedById = new Map<string, PiToolResult[]>();
  for (const result of toolResults) {
    const matches = finalizedById.get(result.toolCallId) ?? [];
    matches.push(result);
    finalizedById.set(result.toolCallId, matches);
  }
  const observationCounts = new Map<string, number>();
  for (const item of observations) {
    observationCounts.set(
      item.toolCallId,
      (observationCounts.get(item.toolCallId) ?? 0) + 1,
    );
  }
  const parents = toolParents(observations);
  const children = new Map<ToolObservation, ToolObservation[]>();
  for (const [child, parent] of parents) {
    const siblings = children.get(parent) ?? [];
    siblings.push(child);
    children.set(parent, siblings);
  }

  function emit(
    timing: ToolObservation,
    start: (value: ToolExecutionStart) => ToolExecutionRecorder,
  ): void {
    try {
      const toolRec = start({
        ...(timing.skillName
          ? { executionKind: "load_skill", skillName: timing.skillName }
          : {}),
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
        // Host duration excludes guards and result hooks; placement uses the observed start.
        completedAt: new Date(
          timing.durationMs === undefined
            ? timing.completedAt
            : timing.startedAt + Math.round(timing.durationMs),
        ),
      };

      if (includeContent) {
        if (
          timing.postExecutionInputSafe &&
          timing.postExecutionInputJSON !== undefined
        ) {
          // Tools can mutate input. This is not an execution-entry snapshot.
          end.arguments = redactSecretText(
            `{"observation":"tool_result.input","input":${timing.postExecutionInputJSON}}`,
          );
        }
        const matches = finalizedById.get(timing.toolCallId);
        const finalized =
          matches?.length === 1 &&
          !timing.duplicateId &&
          observationCounts.get(timing.toolCallId) === 1 &&
          matches[0]?.toolName === timing.toolName
            ? matches[0]
            : undefined;
        // Nested calls have no transcript entry. Top-level bodies must use finalization.
        const text =
          timing.parentToolCallId !== undefined
            ? timing.resultText
            : finalized === undefined
              ? undefined
              : toolResultText(finalized.content);
        if (text !== undefined) end.result = redactSecretText(text);
      }

      if (timing.isError) {
        toolRec.setCallError(new Error("tool returned error"));
      }

      toolRec.setResult(end);
      for (const child of children.get(timing) ?? []) {
        emit(child, (value) => toolRec.startToolExecution(value));
      }
      toolRec.end();
    } catch (err) {
      logger.error("failed to emit tool span", err);
    }
  }
  for (const observation of observations) {
    if (!parents.has(observation)) emit(observation, startToolExecution);
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

function currentGuardModel(
  model: { provider?: string; id?: string; api?: string } | undefined,
) {
  return model?.provider && model.id && model.api !== "pi-virtual"
    ? { provider: model.provider, name: model.id }
    : { provider: "unknown", name: "unknown" };
}
