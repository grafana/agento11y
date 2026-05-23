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
import {
  mapGenerationResult,
  mapGenerationStart,
  mapToolNames,
  mapUserMessage,
  type PiAssistantMessage,
  type PiToolResult,
  type PiUserMessage,
  type ToolTiming,
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

  function debugLog(msg: string, ...args: unknown[]) {
    if (config?.debug) console.error(`[sigil-pi] ${msg}`, ...args);
  }

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
        console.warn("[sigil-pi] telemetry shutdown failed:", err);
      }
      telemetry = null;
    }
    resetTurnState();
    pendingInputMessages.length = 0;
    lastSeenModel = null;
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
          console.warn("[sigil-pi] stale client shutdown failed:", err);
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
          console.warn("[sigil-pi] failed to create OTel providers:", err);
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

      debugLog(`enabled, endpoint=${config.endpoint} auth=${config.auth.mode}`);
    } catch (err) {
      console.warn("[sigil-pi] session_start failed:", err);
      sigil = null;
      await resetSessionState();
    }
  });

  pi.on("turn_start", async (_event, _ctx) => {
    resetTurnState();
    if (!sigil) return;
    turnStartTime = Date.now();
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
      const mapped = mapUserMessage(message, config.contentCapture);
      if (mapped) pendingInputMessages.push(mapped);
    } catch (err) {
      console.warn("[sigil-pi] message_end failed:", err);
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
      logger: { warn: (msg: string) => console.warn(msg) },
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
      console.warn("[sigil-pi] tool_execution_start failed:", err);
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
      console.warn("[sigil-pi] tool_execution_end failed:", err);
    }
  });

  pi.on("turn_end", async (event, ctx) => {
    if (!sigil || !config) return;

    try {
      if (!isAssistantMessage(event.message)) {
        console.warn(
          "[sigil-pi] turn_end: assistant message shape did not validate, skipping",
        );
        return;
      }

      const msg = event.message;
      const contentCapture = config.contentCapture;
      const toolDefs = mapToolNames(turnToolTimings);

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

      const seed = mapGenerationStart(
        msg,
        conversationId,
        config.agentName,
        config.agentVersion,
        startedAtMs,
        toolDefs.length > 0 ? toolDefs : undefined,
        builtinTags,
      );

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
              agentName: (config as SigilPiConfig).agentName,
              agentVersion: (config as SigilPiConfig).agentVersion,
              contentCapture,
            },
          );
        });
        debugLog(
          `generation queued, model=${msg.model} tokens=${msg.usage.totalTokens}`,
        );
      } catch (err) {
        debugLog("generation export failed", err);
      }
      if (telemetry) {
        void telemetry.forceFlush().catch((err) => {
          debugLog("telemetry flush failed", err);
        });
      }
    } catch (err) {
      console.warn("[sigil-pi] turn_end failed:", err);
    } finally {
      toolStarts.clear();
      turnToolTimings.length = 0;
      pendingInputMessages.length = 0;
    }
  });

  pi.on("session_shutdown", async (_event, _ctx) => {
    if (sigil) {
      try {
        await sigil.shutdown();
      } catch (err) {
        console.warn("[sigil-pi] session shutdown failed:", err);
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
      console.warn(
        `[sigil-pi] failed to emit tool span for ${timing.toolName}:`,
        err,
      );
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
