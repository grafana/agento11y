import { randomUUID } from "node:crypto";
import type { SigilClient } from "@grafana/sigil-sdk-js";
import type { PluginInput } from "@opencode-ai/plugin";
import type { AssistantMessage, Part, UserMessage } from "@opencode-ai/sdk";
import { createSigilClient } from "./client.js";
import type { SigilOpencodeConfig } from "./config.js";
import { mapError, mapGeneration, mapToolDefinitions } from "./mappers.js";
import { Redactor } from "./redact.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

type OpencodeClient = PluginInput["client"];

// Track recorded messages per session for dedup and cleanup
const recordedMessages = new Map<string, Set<string>>();

// Pending generation store: user-side data captured before assistant responds
type PendingGeneration = {
  systemPrompt: string | undefined;
  userParts: Part[];
  tools: Record<string, boolean> | undefined;
};
const pendingGenerations = new Map<string, PendingGeneration>();

function buildAgentName(
  prefix: string | undefined,
  mode: string | undefined,
): string {
  const base = prefix || "opencode";
  return mode ? `${base}:${mode}` : base;
}

/**
 * Called from the chat.message hook. Stores user-side data for later use
 * when the assistant message completes.
 */
function handleChatMessage(
  input: { sessionID: string },
  output: { message: UserMessage; parts: Part[] },
): void {
  pendingGenerations.set(input.sessionID, {
    systemPrompt: output.message.system,
    userParts: output.parts,
    tools: output.message.tools,
  });
}

async function handleEvent(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  event: { type: string; properties: unknown },
): Promise<void> {
  if (event.type !== "message.updated") return;

  const properties = event.properties as
    | { info?: { role?: string } }
    | undefined;
  const msg = properties?.info;
  if (!msg || msg.role !== "assistant") return;

  const assistantMsg = msg as AssistantMessage;

  // Only record terminal messages
  const isTerminal =
    assistantMsg.finish || assistantMsg.error || assistantMsg.time.completed;
  if (!isTerminal) return;

  // Dedup
  const sessionSet =
    recordedMessages.get(assistantMsg.sessionID) ?? new Set<string>();
  if (sessionSet.has(assistantMsg.id)) return;
  sessionSet.add(assistantMsg.id);
  recordedMessages.set(assistantMsg.sessionID, sessionSet);

  // Look up pending generation (user-side data)
  const pending = pendingGenerations.get(assistantMsg.sessionID);

  const includeMessageBodies = config.contentCapture !== "metadata_only";

  // Fetch assistant parts only when the selected mode can export message bodies.
  let assistantParts: Part[] = [];
  if (includeMessageBodies) {
    try {
      const response = await client.session.message({
        path: { id: assistantMsg.sessionID, messageID: assistantMsg.id },
      });
      assistantParts = response.data?.parts ?? [];
    } catch {
      // REST fetch failed — fall back to metadata-only output content.
    }
  }

  const tools = mapToolDefinitions(pending?.tools);
  const seed = {
    conversationId: assistantMsg.sessionID,
    agentName: buildAgentName(config.agentName, assistantMsg.mode),
    agentVersion: config.agentVersion,
    effectiveVersion: config.agentVersion,
    model: { provider: assistantMsg.providerID, name: assistantMsg.modelID },
    startedAt: new Date(assistantMsg.time.created),
    contentCapture: config.contentCapture,
    ...(tools.length > 0 && { tools }),
    ...(includeMessageBodies && { systemPrompt: pending?.systemPrompt }),
  };

  const result = mapGeneration(
    assistantMsg,
    includeMessageBodies ? (pending?.userParts ?? []) : [],
    assistantParts,
    redactor,
    config.contentCapture,
  );

  try {
    if (assistantMsg.error) {
      const error = assistantMsg.error;
      await sigil.startGeneration(seed, async (recorder) => {
        recorder.setResult(result);
        recorder.setCallError(mapError(error));
      });
    } else {
      await sigil.startGeneration(seed, async (recorder) => {
        recorder.setResult(result);
      });
    }
  } catch {
    // Sigil recording failure should never break the plugin
  }

  // Clean up pending generation
  pendingGenerations.delete(assistantMsg.sessionID);
}

async function handleLifecycle(
  sigil: SigilClient,
  telemetry: TelemetryProviders | null,
  debugLog: (msg: string, ...args: unknown[]) => void,
  event: { type: string; properties: unknown },
): Promise<void> {
  const type = event.type as string;

  if (type === "session.idle") {
    // Fire-and-forget: a stuck OTLP endpoint must not block session.idle for
    // up to ~30s (BatchSpanProcessor default) per turn.
    void sigil.flush().catch((err) => debugLog("sigil flush failed", err));
    if (telemetry) {
      void telemetry
        .forceFlush()
        .catch((err) => debugLog("telemetry flush failed", err));
    }
  }

  if (type === "session.deleted") {
    const properties = event.properties as
      | { info?: { id?: string } }
      | undefined;
    const sessionId = properties?.info?.id;
    if (sessionId) {
      recordedMessages.delete(sessionId);
      pendingGenerations.delete(sessionId);
    }
  }

  if (type === "global.disposed") {
    try {
      await sigil.shutdown();
    } catch {
      // shutdown failure is non-fatal
    }
    if (telemetry) {
      try {
        await telemetry.shutdown();
      } catch (err) {
        debugLog("telemetry shutdown failed", err);
      }
    }
  }
}

export type SigilHooks = {
  event: (input: {
    event: { type: string; properties: unknown };
  }) => Promise<void>;
  chatMessage: (
    input: { sessionID: string },
    output: { message: UserMessage; parts: Part[] },
  ) => void;
};

export async function createSigilHooks(
  config: SigilOpencodeConfig,
  client: OpencodeClient,
): Promise<SigilHooks | null> {
  function debugLog(msg: string, ...args: unknown[]) {
    if (config.debug) console.error(`[sigil-opencode] ${msg}`, ...args);
  }

  let telemetry: TelemetryProviders | null = null;
  if (config.otlp) {
    try {
      telemetry = createTelemetryProviders(config.otlp, randomUUID());
    } catch (err) {
      console.warn("[sigil-opencode] failed to create OTel providers:", err);
    }
  }

  const sigil = createSigilClient(config, {
    tracer: telemetry?.tracer,
    meter: telemetry?.meter,
  });
  if (!sigil) {
    if (telemetry) {
      try {
        await telemetry.shutdown();
      } catch (err) {
        debugLog("telemetry shutdown failed", err);
      }
    }
    return null;
  }

  const redactor = new Redactor();

  process.on("beforeExit", () => {
    sigil.shutdown().catch(() => {});
    if (telemetry) {
      telemetry
        .shutdown()
        .catch((err) => debugLog("telemetry shutdown failed", err));
    }
  });

  return {
    event: async (input) => {
      await handleEvent(sigil, config, client, redactor, input.event);
      await handleLifecycle(sigil, telemetry, debugLog, input.event);
    },
    chatMessage: (input, output) => {
      handleChatMessage(input, output);
    },
  };
}
