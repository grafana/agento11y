import { randomUUID } from "node:crypto";
import type { SigilClient } from "@grafana/sigil-sdk-js";
import type { PluginInput } from "@opencode-ai/plugin";
import type {
  AssistantMessage,
  Part,
  Permission,
  UserMessage,
} from "@opencode-ai/sdk";
import { createSigilClient } from "./client.js";
import type { SigilOpencodeConfig } from "./config.js";
import { runToolCallGuard } from "./guard.js";
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

type SessionContext = {
  agent: string | undefined;
  model: { provider: string; name: string } | undefined;
};
const sessionContexts = new Map<string, SessionContext>();

type MessageUpdatedInfo = Partial<AssistantMessage> & {
  id?: string;
  sessionID?: string;
};

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
  input: {
    sessionID: string;
    agent?: string;
    model?: { providerID: string; modelID: string };
  },
  output: { message: UserMessage; parts: Part[] },
): void {
  pendingGenerations.set(input.sessionID, {
    systemPrompt: output.message.system,
    userParts: output.parts,
    tools: output.message.tools,
  });
  sessionContexts.set(input.sessionID, {
    agent: input.agent ?? stringField(output.message, "agent"),
    model: resolveModel(input.model, output.message),
  });
}

async function handleEvent(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  event: { type: string; properties: unknown },
): Promise<void> {
  if (event.type === "message.part.updated") {
    await handleMessagePartUpdated(
      sigil,
      config,
      client,
      redactor,
      debugLog,
      event.properties,
    );
    return;
  }
  if (event.type !== "message.updated") return;

  const properties = event.properties as
    | { info?: MessageUpdatedInfo }
    | undefined;
  const msg = properties?.info;
  if (!msg) return;

  let assistantMsg: AssistantMessage | undefined =
    msg.role === "assistant" ? (msg as AssistantMessage) : undefined;
  let fetchedParts: Part[] | undefined;
  if (
    !assistantMsg &&
    isTerminalMessageUpdate(msg) &&
    msg.sessionID &&
    msg.id
  ) {
    try {
      const response = await client.session.message({
        path: { id: msg.sessionID, messageID: msg.id },
      });
      if (response.data?.info?.role === "assistant") {
        assistantMsg = response.data.info as AssistantMessage;
        fetchedParts = response.data.parts ?? [];
      }
    } catch (err) {
      debugLog("failed to hydrate partial assistant message", err);
      return;
    }
  }
  if (!assistantMsg) return;

  await recordAssistantMessage(
    sigil,
    config,
    client,
    redactor,
    debugLog,
    assistantMsg,
    fetchedParts,
  );
}

async function handleMessagePartUpdated(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  properties: unknown,
): Promise<void> {
  const part = recordField(properties, "part");
  if (stringField(part, "type") !== "step-finish") return;
  const sessionID = stringField(part, "sessionID");
  const messageID = stringField(part, "messageID");
  if (!sessionID || !messageID) return;

  try {
    const response = await client.session.message({
      path: { id: sessionID, messageID },
    });
    if (response.data?.info?.role !== "assistant") return;
    await recordAssistantMessage(
      sigil,
      config,
      client,
      redactor,
      debugLog,
      response.data.info as AssistantMessage,
      response.data.parts ?? [],
    );
  } catch (err) {
    debugLog("failed to export terminal message part", err);
  }
}

async function recordAssistantMessage(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  assistantMsg: AssistantMessage,
  fetchedParts?: Part[],
): Promise<void> {
  sessionContexts.set(assistantMsg.sessionID, {
    agent: assistantMsg.mode,
    model: {
      provider: assistantMsg.providerID,
      name: assistantMsg.modelID,
    },
  });

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
    if (fetchedParts !== undefined) {
      assistantParts = fetchedParts;
    } else {
      try {
        const response = await client.session.message({
          path: { id: assistantMsg.sessionID, messageID: assistantMsg.id },
        });
        assistantParts = response.data?.parts ?? [];
      } catch (err) {
        debugLog("failed to fetch assistant message parts", err);
        // REST fetch failed — fall back to metadata-only output content.
      }
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
  } catch (err) {
    debugLog("sigil generation export failed", err);
    // Sigil recording failure should never break the plugin
  }

  // Clean up pending generation
  pendingGenerations.delete(assistantMsg.sessionID);
}

async function sweepTerminalAssistantMessages(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  sessionID: string,
): Promise<void> {
  try {
    const response = await client.session.messages({
      path: { id: sessionID },
    });
    for (const message of response.data ?? []) {
      if (message.info.role !== "assistant") continue;
      await recordAssistantMessage(
        sigil,
        config,
        client,
        redactor,
        debugLog,
        message.info as AssistantMessage,
        message.parts ?? [],
      );
    }
  } catch (err) {
    debugLog("failed to sweep terminal assistant messages", err);
  }
}

function isTerminalMessageUpdate(msg: MessageUpdatedInfo): boolean {
  return Boolean(msg.finish || msg.error || msg.time?.completed);
}

async function handleLifecycle(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  telemetry: TelemetryProviders | null,
  debugLog: (msg: string, ...args: unknown[]) => void,
  event: { type: string; properties: unknown },
): Promise<void> {
  const type = event.type as string;

  if (type === "session.idle") {
    const properties = event.properties as
      | { info?: { id?: string } }
      | undefined;
    const sessionIds = properties?.info?.id
      ? [properties.info.id]
      : Array.from(pendingGenerations.keys());
    for (const sessionId of sessionIds) {
      await sweepTerminalAssistantMessages(
        sigil,
        config,
        client,
        redactor,
        debugLog,
        sessionId,
      );
    }
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
      sessionContexts.delete(sessionId);
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

async function handleToolExecuteBefore(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  input: { tool: string; sessionID: string; callID: string },
  output: { args: unknown },
): Promise<void> {
  const guards = config.guards;
  if (guards?.enabled !== true) return;
  const res = await runToolCallGuard({
    client: sigil,
    agentName: agentNameForSession(config, input.sessionID),
    agentVersion: config.agentVersion,
    model: modelForSession(input.sessionID),
    toolCallId: input.callID,
    toolName: input.tool,
    input: output.args ?? {},
    failOpen: guards.failOpen,
  });
  if (res?.block) {
    throw new Error(res.reason);
  }
}

async function handlePermissionAsk(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  input: Permission,
  output: { status: "ask" | "deny" | "allow" },
): Promise<void> {
  const guards = config.guards;
  if (guards?.enabled !== true) return;
  const res = await runToolCallGuard({
    client: sigil,
    agentName: agentNameForSession(config, input.sessionID),
    agentVersion: config.agentVersion,
    model: modelForSession(input.sessionID),
    toolCallId: input.callID,
    toolName: input.type,
    input: {
      pattern: input.pattern,
      title: input.title,
      metadata: input.metadata,
    },
    failOpen: guards.failOpen,
  });
  if (res?.block) {
    output.status = "deny";
  }
}

function agentNameForSession(
  config: SigilOpencodeConfig,
  sessionID: string,
): string {
  return buildAgentName(
    config.agentName,
    sessionContexts.get(sessionID)?.agent,
  );
}

function modelForSession(sessionID: string): {
  provider: string;
  name: string;
} {
  return (
    sessionContexts.get(sessionID)?.model ?? {
      provider: "unknown",
      name: "unknown",
    }
  );
}

function resolveModel(
  inputModel: { providerID: string; modelID: string } | undefined,
  message: UserMessage,
): { provider: string; name: string } | undefined {
  if (inputModel) {
    return { provider: inputModel.providerID, name: inputModel.modelID };
  }
  const rawModel = recordField(message, "model");
  if (!rawModel) return undefined;
  const provider = stringField(rawModel, "providerID");
  const name = stringField(rawModel, "modelID");
  if (!provider && !name) return undefined;
  return {
    provider: provider || "unknown",
    name: name || "unknown",
  };
}

function recordField(
  value: unknown,
  key: string,
): Record<string, unknown> | undefined {
  if (!value || typeof value !== "object") return undefined;
  const field = (value as Record<string, unknown>)[key];
  return field && typeof field === "object"
    ? (field as Record<string, unknown>)
    : undefined;
}

function stringField(value: unknown, key: string): string | undefined {
  if (!value || typeof value !== "object") return undefined;
  const field = (value as Record<string, unknown>)[key];
  return typeof field === "string" && field.trim().length > 0
    ? field
    : undefined;
}

export type SigilHooks = {
  event: (input: {
    event: { type: string; properties: unknown };
  }) => Promise<void>;
  chatMessage: (
    input: {
      sessionID: string;
      agent?: string;
      model?: { providerID: string; modelID: string };
    },
    output: { message: UserMessage; parts: Part[] },
  ) => void;
  toolExecuteBefore: (
    input: { tool: string; sessionID: string; callID: string },
    output: { args: unknown },
  ) => Promise<void>;
  permissionAsk: (
    input: Permission,
    output: { status: "ask" | "deny" | "allow" },
  ) => Promise<void>;
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
      await handleEvent(sigil, config, client, redactor, debugLog, input.event);
      await handleLifecycle(
        sigil,
        config,
        client,
        redactor,
        telemetry,
        debugLog,
        input.event,
      );
    },
    chatMessage: (input, output) => {
      handleChatMessage(input, output);
    },
    toolExecuteBefore: async (input, output) => {
      await handleToolExecuteBefore(sigil, config, input, output);
    },
    permissionAsk: async (input, output) => {
      await handlePermissionAsk(sigil, config, input, output);
    },
  };
}
