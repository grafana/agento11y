import { randomUUID } from "node:crypto";
import type { ContentCaptureMode, SigilClient } from "@grafana/sigil-sdk-js";
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

export type ToolExecutionRecord = {
  sessionID: string;
  toolName: string;
  toolCallId: string;
  startedAt: number;
  completedAt: number;
  input?: unknown;
  output?: unknown;
  isError?: boolean;
  error?: string;
};

const activeToolExecutions = new Map<string, ToolExecutionRecord>();
const completedToolExecutions = new Map<string, ToolExecutionRecord[]>();

function toolExecutionKey(sessionID: string, callID: string): string {
  return `${sessionID}\x00${callID}`;
}

/** @internal Exported for testing. */
export function _resetToolExecutionState(): void {
  activeToolExecutions.clear();
  completedToolExecutions.clear();
}

/** @internal Exported for testing. */
export function _peekToolExecutionState(): {
  active: ToolExecutionRecord[];
  completed: ToolExecutionRecord[];
} {
  const completed: ToolExecutionRecord[] = [];
  for (const list of completedToolExecutions.values()) {
    completed.push(...list);
  }
  return {
    active: Array.from(activeToolExecutions.values()),
    completed,
  };
}

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

  // Span records prefer terminal `ToolPart.state.time` when assistant parts
  // are already available. `assistantParts` is empty in `metadata_only` (we
  // intentionally skip the REST fetch there), so hook records take over and
  // give us per-tool spans without forcing a body fetch.
  const termRecords = toolSpansFromParts(
    assistantMsg.sessionID,
    assistantParts,
  );
  const hookRecords = completedToolExecutions.get(assistantMsg.sessionID) ?? [];
  const spanRecords = mergeToolSpanRecords(termRecords, hookRecords);
  const spanOpts = {
    conversationId: assistantMsg.sessionID,
    agentName: buildAgentName(config.agentName, assistantMsg.mode),
    agentVersion: config.agentVersion,
    requestProvider: assistantMsg.providerID,
    requestModel: assistantMsg.modelID,
    contentCapture: config.contentCapture,
    redactor,
    debugLog,
  };

  try {
    if (assistantMsg.error) {
      const error = assistantMsg.error;
      await sigil.startGeneration(seed, async (recorder) => {
        recorder.setResult(result);
        recorder.setCallError(mapError(error));
        emitToolSpans(sigil, spanRecords, spanOpts);
      });
    } else {
      await sigil.startGeneration(seed, async (recorder) => {
        recorder.setResult(result);
        emitToolSpans(sigil, spanRecords, spanOpts);
      });
    }
  } catch (err) {
    debugLog("sigil generation export failed", err);
    // Sigil recording failure should never break the plugin
  }

  // Clean up pending generation and per-turn tool records. The completed
  // records were consumed above; clearing them prevents duplicate spans if
  // another export path fires for the same session.
  pendingGenerations.delete(assistantMsg.sessionID);
  completedToolExecutions.delete(assistantMsg.sessionID);
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
      completedToolExecutions.delete(sessionId);
      for (const key of activeToolExecutions.keys()) {
        if (key.startsWith(`${sessionId}\x00`)) {
          activeToolExecutions.delete(key);
        }
      }
    }
  }

  if (type === "global.disposed") {
    activeToolExecutions.clear();
    completedToolExecutions.clear();
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
  const key = toolExecutionKey(input.sessionID, input.callID);
  activeToolExecutions.set(key, {
    sessionID: input.sessionID,
    toolName: input.tool,
    toolCallId: input.callID,
    startedAt: Date.now(),
    completedAt: 0,
    input: output.args,
  });

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
    activeToolExecutions.delete(key);
    throw new Error(res.reason);
  }
}

function handleToolExecuteAfter(
  input: { tool: string; sessionID: string; callID: string; args: unknown },
  output: { title: string; output: string; metadata: unknown },
): void {
  const key = toolExecutionKey(input.sessionID, input.callID);
  const active = activeToolExecutions.get(key);
  if (!active) return;
  activeToolExecutions.delete(key);

  const completed: ToolExecutionRecord = {
    ...active,
    completedAt: Date.now(),
    output: output.output,
  };
  const list = completedToolExecutions.get(input.sessionID) ?? [];
  list.push(completed);
  completedToolExecutions.set(input.sessionID, list);
}

async function handlePermissionAsk(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  debugLog: (msg: string, ...args: unknown[]) => void,
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
    // Log the reason so it's recoverable from the debug log; opencode's
    // permission.ask output API has no field to surface it to the model or
    // the user.
    debugLog(
      `guard denied permission.ask for tool=${input.type} (reason dropped, API has no field): ${res.reason}`,
    );
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

/**
 * Extract completed/error tool execution records from already-fetched
 * terminal assistant parts. Persisted `ToolPart.state.time.start/end` is
 * more accurate than hook wall-clock timing, so prefer this when parts are
 * available.
 *
 * @internal Exported for testing.
 */
export function toolSpansFromParts(
  sessionID: string,
  parts: Part[],
): ToolExecutionRecord[] {
  const records: ToolExecutionRecord[] = [];
  for (const part of parts) {
    if (part.type !== "tool") continue;
    const { state } = part;
    if (state.status === "completed") {
      records.push({
        sessionID,
        toolName: part.tool,
        toolCallId: part.callID,
        startedAt: state.time.start,
        completedAt: state.time.end,
        input: state.input,
        output: state.output,
      });
    } else if (state.status === "error") {
      records.push({
        sessionID,
        toolName: part.tool,
        toolCallId: part.callID,
        startedAt: state.time.start,
        completedAt: state.time.end,
        input: state.input,
        isError: true,
        error: state.error,
      });
    }
  }
  return records;
}

/**
 * Merge tool execution records from terminal `ToolPart` values with
 * hook-recorded records, preferring terminal-part timing and state. Hook
 * records survive only when the terminal parts don't already cover them.
 *
 * @internal Exported for testing.
 */
export function mergeToolSpanRecords(
  termRecords: ToolExecutionRecord[],
  hookRecords: ToolExecutionRecord[],
): ToolExecutionRecord[] {
  const merged: ToolExecutionRecord[] = [...termRecords];
  const seen = new Set(
    termRecords.map((r) => toolExecutionKey(r.sessionID, r.toolCallId)),
  );
  for (const rec of hookRecords) {
    const key = toolExecutionKey(rec.sessionID, rec.toolCallId);
    if (seen.has(key)) continue;
    seen.add(key);
    merged.push(rec);
  }
  return merged;
}

/**
 * Emit Sigil tool execution spans for a set of completed tool records.
 * Errors thrown by the SDK are swallowed so a span failure cannot break
 * the plugin.
 *
 * @internal Exported for testing.
 */
export function emitToolSpans(
  client: SigilClient,
  records: ToolExecutionRecord[],
  opts: {
    conversationId: string;
    agentName: string;
    agentVersion?: string;
    requestProvider: string;
    requestModel: string;
    contentCapture: ContentCaptureMode;
    redactor: Redactor;
    debugLog: (msg: string, ...args: unknown[]) => void;
  },
): void {
  if (records.length === 0) return;
  const includeContent = opts.contentCapture === "full";

  for (const record of records) {
    try {
      const rec = client.startToolExecution({
        toolName: record.toolName,
        toolCallId: record.toolCallId,
        toolType: "function",
        conversationId: opts.conversationId,
        agentName: opts.agentName,
        agentVersion: opts.agentVersion,
        requestProvider: opts.requestProvider,
        requestModel: opts.requestModel,
        startedAt: new Date(record.startedAt),
        contentCapture: opts.contentCapture,
      });

      if (record.isError) {
        rec.setCallError(new Error(record.error || "tool returned error"));
      }

      const end: {
        arguments?: unknown;
        result?: unknown;
        completedAt: Date;
      } = {
        completedAt: new Date(record.completedAt),
      };

      if (includeContent && record.input !== undefined) {
        end.arguments = opts.redactor.redact(JSON.stringify(record.input));
      }
      if (includeContent && record.output !== undefined) {
        end.result = opts.redactor.redact(String(record.output));
      }

      rec.setResult(end);
      rec.end();
    } catch (err) {
      opts.debugLog(`tool span export failed for ${record.toolName}`, err);
    }
  }
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
  toolExecuteAfter: (
    input: { tool: string; sessionID: string; callID: string; args: unknown },
    output: { title: string; output: string; metadata: unknown },
  ) => void;
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
    toolExecuteAfter: (input, output) => {
      handleToolExecuteAfter(input, output);
    },
    permissionAsk: async (input, output) => {
      await handlePermissionAsk(sigil, config, debugLog, input, output);
    },
  };
}
