import { randomUUID } from "node:crypto";
import type {
  ContentCaptureMode,
  Message,
  SigilClient,
} from "@grafana/sigil-sdk-js";
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { createSigilClient } from "./client.js";
import type { SigilPiConfig } from "./config.js";
import { loadConfig } from "./config.js";
import { detectPiVersion } from "./detectPiVersion.js";
import { resolveGitBranch } from "./git.js";
import { runToolCallGuard } from "./guard.js";
import { resolvePiGenerationLineage } from "./lineage.js";
import { logger } from "./logger.js";
import {
  type CachedRequestControls,
  extractRequestControls,
  mapGenerationResult,
  mapGenerationStart,
  mapTools,
  mapUserMessage,
  type PiAssistantMessage,
  type PiToolInfo,
  type PiToolResult,
  type PiUserMessage,
  resolveConversationTitle,
  type ToolTiming,
  userMessageText,
} from "./mappers.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

export default function (pi: ExtensionAPI) {
  let sigil: SigilClient | null = null;
  let config: SigilPiConfig | null = null;
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
  }

  function cacheAssistantModel(message: PiAssistantMessage) {
    lastSeenModel = {
      provider: message.provider,
      name: message.model,
    };
  }

  pi.on("session_start", async (_event, ctx) => {
    try {
      if (sigil) {
        try {
          await sigil.shutdown();
        } catch (err) {
          logger.error("stale client shutdown failed", err);
        }
      }

      sigil = null;
      await resetSessionState();

      config = await loadConfig();
      if (!config) return;

      if (!config.agentVersion) {
        config = { ...config, agentVersion: detectPiVersion() };
      }

      // Note: conversationId is read fresh per turn from
      // ctx.sessionManager.getSessionId() so fork/branch reassignments
      // (session-manager.js:927,961) are reflected without restarting
      // the plugin. We intentionally do NOT cache it here.

      // Set up OTel providers if OTLP is configured.
      // Pass the pi session id as service.instance.id so concurrent pi
      // sessions on the same machine emit distinct OTel metric series.
      if (config.otlp) {
        try {
          const instanceId = ctx.sessionManager.getSessionId() || randomUUID();
          telemetry = createTelemetryProviders(config.otlp, instanceId);
        } catch (err) {
          logger.error("failed to create OTel providers", err);
        }
      }

      sigil = createSigilClient(config, {
        tracer: telemetry?.tracer,
        meter: telemetry?.meter,
      });
      if (!sigil) {
        await resetSessionState();
        return;
      }

      logger.debug(
        `enabled, endpoint=${config.endpoint} auth=${config.auth.mode}`,
      );
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

  pi.on("tool_call", async (event, _ctx) => {
    if (!sigil || !config?.guards.enabled) return;
    return runToolCallGuard({
      client: sigil,
      agentName: config.agentName,
      agentVersion: config.agentVersion,
      model: lastSeenModel ?? { provider: "unknown", name: "unknown" },
      toolCallId: event.toolCallId,
      toolName: event.toolName,
      input: event.input as Record<string, unknown>,
      failOpen: config.guards.failOpen,
      logger: { warn: (msg: string) => logger.warn(msg) },
    });
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
      // generation. Gated on contentCapture=full because branch names
      // can leak project context (diverges from claude-code/cursor,
      // which always send it).
      const gitBranch =
        config.contentCapture === "full"
          ? resolveGitBranch(process.cwd())
          : undefined;
      const builtinTags = gitBranch ? { "git.branch": gitBranch } : undefined;

      // Resolve lineage at `turn_end`, not `message_end`: pi awaits
      // extension `message_end` callbacks *before* calling
      // `sessionManager.appendMessage` (see agent-session.js `_publish`),
      // so the assistant entry is not yet in the tree at that point. By
      // `turn_end` it has been appended and any subsequent extension
      // mutations have settled.
      const lineage = resolvePiGenerationLineage(
        ctx.sessionManager,
        msg,
        conversationId,
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
        generationId: lineage.generationId,
        parentGenerationIds: lineage.parentGenerationIds,
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
            sigil as SigilClient,
            msg,
            toolResults,
            turnToolTimings,
            {
              conversationId,
              conversationTitle,
              agentName: (config as SigilPiConfig).agentName,
              agentVersion: (config as SigilPiConfig).agentVersion,
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

/** @internal Exported for testing. */
export function emitToolSpans(
  client: SigilClient,
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

  const argsMap = new Map<string, Record<string, unknown>>();
  const resultMap = new Map<string, string>();
  if (includeContent) {
    for (const block of msg.content) {
      if (block.type === "toolCall") {
        argsMap.set(block.id, block.arguments);
      }
    }
    for (const tr of toolResults) {
      const text = tr.content
        .filter(
          (c): c is { type: "text"; text: string } =>
            c.type === "text" && !!c.text,
        )
        .map((c) => c.text)
        .join("\n");
      resultMap.set(tr.toolCallId, text);
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
        if (args) {
          end.arguments = JSON.stringify(args);
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
