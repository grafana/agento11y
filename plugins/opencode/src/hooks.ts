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
import { resolveGitBranch } from "./git.js";
import { runToolCallGuard } from "./guard.js";
import { stableOpencodeGenerationId } from "./lineage.js";
import {
  legacyToolOverrideNames,
  mapError,
  mapGeneration,
  mapToolDefinitions,
} from "./mappers.js";
import { Redactor } from "./redact.js";
import { buildBuiltinTags } from "./tags.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

type OpencodeClient = PluginInput["client"];

// Track recorded messages per session for dedup and cleanup
const recordedMessages = new Map<string, Set<string>>();

// Last assistant generation id recorded per session, used as the parent for
// the next assistant generation. opencode assistant messages all share a
// `parentID` pointing at the user message, so they cannot express
// assistant-to-assistant lineage themselves. Message ids are monotonic and
// recording is sequential, so the previous recorded generation is the
// correct parent. The chain is in-memory and resets per process: the first
// turn after a restart loses its parent edge, but its deterministic id still
// dedups.
const lastGenerationIdBySession = new Map<string, string>();

// Maps a child (subagent) session id to the parent generation its first
// assistant turn should link to. opencode runs a subagent in a fresh session
// whose `Session.parentID` points at the spawning session, surfaced via the
// `session.created` event. We resolve the parent generation *at creation time*
// — the spawning session's latest recorded generation — and freeze it here.
//
// Freezing at creation (rather than at child-record time) is deliberate: a
// subagent is launched from a tool call inside the parent's assistant turn, so
// the parent's *current* turn is still in flight and unrecorded when the child
// runs. The already-recorded prior parent generation is the turn the subagent
// was spawned from, which is the meaningful link. Resolving lazily at
// child-record time would usually find no parent generation yet and drop the
// edge.
//
// `AssistantMessage.parentID` is a message-level pointer within one session and
// is NOT the same as `Session.parentID`; only the latter crosses the
// parent/subagent boundary.
const parentGenerationByChildSession = new Map<string, string>();

// First streamed assistant part time per message, keyed by
// `${sessionID}\x00${messageID}`. Captured from `message.part.updated`
// before the message completes so it survives `metadata_only` (where we
// never fetch the message body). Consumed and cleared when the message is
// recorded.
const firstPartAtByMessage = new Map<string, number>();

function messageKey(sessionID: string, messageID: string): string {
  return `${sessionID}\x00${messageID}`;
}

// Pending generation store: user-side data captured before assistant responds
type PendingGeneration = {
  systemPrompt: string | undefined;
  userParts: Part[];
  tools: Record<string, boolean> | undefined;
};
const pendingGenerations = new Map<string, PendingGeneration>();

// Effective system prompt per session, captured from OpenCode's
// `experimental.chat.system.transform` hook. The hook fires during request
// preparation, after OpenCode composes the agent, runtime, and override
// prompts, so this is what the model receives. The latest value wins and
// stays until the session is deleted, because the prompt normally repeats
// across turns. Title requests share the session ID; they are filtered out
// by comparing the request model against the session's chat model.
const latestSystemPromptBySession = new Map<string, string>();

// OpenCode host version from `session.created`/`session.updated` events.
// Used as the default agent and effective version, matching claude-code.
let hostVersion: string | undefined;

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

/**
 * Resets every module-level map: the dedup/generation tracking plus the
 * tool-execution maps. Integration tests that drive the full record path
 * (`chat.message` -> `message.updated`) need this between cases. Without it, a
 * reused session/message id hits the dedup early-return in
 * `recordAssistantMessage`, silently skips recording, and produces a
 * misleading green.
 *
 * @internal Exported for testing.
 */
export function _resetHookState(): void {
  recordedMessages.clear();
  lastGenerationIdBySession.clear();
  parentGenerationByChildSession.clear();
  firstPartAtByMessage.clear();
  pendingGenerations.clear();
  latestSystemPromptBySession.clear();
  hostVersion = undefined;
  sessionContexts.clear();
  _resetToolExecutionState();
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

/**
 * Called from the `experimental.chat.system.transform` hook. Stores the
 * composed system prompt for the session. Read-only: this transform hook is
 * shared with the host and other plugins, so `output.system` is never
 * mutated here.
 *
 * The hook also fires for auxiliary requests such as title generation, which
 * share the session ID but use OpenCode's small model. When the request
 * model differs from the session's chat model (stored by `chat.message`),
 * the prompt is skipped. If both requests use the same model the title
 * prompt can win the race; this only affects a session's first turn.
 */
function handleSystemTransform(
  input: { sessionID?: string; model?: { id?: string } },
  output: { system: string[] },
  debugLog: (msg: string, ...args: unknown[]) => void,
): void {
  const sessionID = input.sessionID;
  if (!sessionID || !Array.isArray(output.system)) return;

  const sessionModel = sessionContexts.get(sessionID)?.model?.name;
  const requestModel = input.model?.id;
  if (sessionModel && requestModel && sessionModel !== requestModel) {
    debugLog(
      `ignoring system prompt for session=${sessionID}: request model ${requestModel} differs from chat model ${sessionModel}`,
    );
    return;
  }

  const prompt = joinSystemPrompt(
    output.system.filter((entry): entry is string => typeof entry === "string"),
  );
  if (prompt === undefined) return;
  latestSystemPromptBySession.set(sessionID, prompt);
}

async function handleEvent(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  projectDir: string,
  event: { type: string; properties: unknown },
): Promise<void> {
  if (event.type === "session.created" || event.type === "session.updated") {
    recordHostVersion(event.properties);
    if (event.type === "session.created") {
      recordSessionParent(event.properties);
    }
    return;
  }
  if (event.type === "message.part.updated") {
    await handleMessagePartUpdated(
      sigil,
      config,
      client,
      redactor,
      debugLog,
      projectDir,
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
    projectDir,
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
  projectDir: string,
  properties: unknown,
): Promise<void> {
  const part = recordField(properties, "part");
  recordFirstPartTime(part);
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
      projectDir,
      response.data.info as AssistantMessage,
      response.data.parts ?? [],
    );
  } catch (err) {
    debugLog("failed to export terminal message part", err);
  }
}

/**
 * Record the time of the first streamed text/reasoning/tool part for a
 * message. This is the time-to-first-token signal: opencode emits
 * `message.part.updated` as the provider streams output, so the first such
 * part marks when the model began producing the response. Keyed by message
 * id, so a user message's parts never displace the assistant turn we read
 * back in `recordAssistantMessage`.
 *
 * Prefer the part's own `time.start`; fall back to `Date.now()` so tool
 * parts (whose timestamp lives under `state.time`) and any part lacking a
 * `time` field still yield a signal. First write wins.
 */
function recordFirstPartTime(part: Record<string, unknown> | undefined): void {
  if (!part) return;
  const type = stringField(part, "type");
  if (type !== "text" && type !== "reasoning" && type !== "tool") return;
  const sessionID = stringField(part, "sessionID");
  const messageID = stringField(part, "messageID");
  if (!sessionID || !messageID) return;
  const key = messageKey(sessionID, messageID);
  if (firstPartAtByMessage.has(key)) return;
  const rawStart = recordField(part, "time")?.start;
  firstPartAtByMessage.set(
    key,
    typeof rawStart === "number" ? rawStart : Date.now(),
  );
}

async function recordAssistantMessage(
  sigil: SigilClient,
  config: SigilOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  projectDir: string,
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

  // Deterministic id + parent link. The id makes re-exporting this message a
  // backend no-op; the parent is the previous assistant generation recorded
  // for this session in this process. Update the chain before exporting so a
  // failed export still parents the next turn correctly.
  const genId = stableOpencodeGenerationId(
    assistantMsg.sessionID,
    assistantMsg.id,
  );
  const parent = resolveParentGenerationId(assistantMsg.sessionID);
  lastGenerationIdBySession.set(assistantMsg.sessionID, genId);

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

  // Prefer the composed prompt from the system transform hook; fall back to
  // the optional legacy override from chat.message.
  const systemPrompt =
    latestSystemPromptBySession.get(assistantMsg.sessionID) ??
    pending?.systemPrompt;
  if (systemPrompt === undefined) {
    debugLog(
      `no system prompt captured for session=${assistantMsg.sessionID} message=${assistantMsg.id}`,
    );
  }

  // Tool spans double as the tool catalog source. Prefer terminal
  // `ToolPart.state.time` when assistant parts are available; hook records
  // cover metadata_only, where parts are never fetched.
  const termRecords = toolSpansFromParts(
    assistantMsg.sessionID,
    assistantParts,
  );
  const hookRecords = completedToolExecutions.get(assistantMsg.sessionID) ?? [];
  // Tools that started but never fired `tool.execute.after` (errored, denied,
  // or interrupted) become error records here, so they surface as spans even
  // in metadata_only and stop leaking from activeToolExecutions. termRecords
  // win on key collision, so a native tool with an error part keeps the
  // accurate terminal record while the active entry is still deleted.
  const drainedRecords = drainActiveToolExecutions(assistantMsg.sessionID);
  const spanRecords = mergeToolSpanRecords(termRecords, [
    ...hookRecords,
    ...drainedRecords,
  ]);

  // Name-only tool definitions, like claude-code: the tools this generation
  // used plus any legacy overrides. OpenCode does not tell the plugin which
  // tools were offered to the model.
  const tools = mapToolDefinitions([
    ...spanRecords.map((record) => record.toolName),
    ...legacyToolOverrideNames(pending?.tools),
  ]);

  const agentVersion = config.agentVersion || hostVersion;
  // Resolved per turn so a mid-session checkout shows up on the next
  // generation. Always sent regardless of content capture mode:
  // `git.branch` and `cwd` are low-cardinality session metadata, not
  // message content, matching claude-code/cursor.
  const builtinTags = buildBuiltinTags({
    cwd: projectDir,
    gitBranch: resolveGitBranch(projectDir),
  });
  const seed = {
    id: genId,
    conversationId: assistantMsg.sessionID,
    agentName: buildAgentName(config.agentName, assistantMsg.mode),
    agentVersion,
    effectiveVersion: agentVersion,
    model: { provider: assistantMsg.providerID, name: assistantMsg.modelID },
    startedAt: new Date(assistantMsg.time.created),
    contentCapture: config.contentCapture,
    ...(parent && { parentGenerationIds: [parent] }),
    ...(tools.length > 0 && { tools }),
    ...(includeMessageBodies && { systemPrompt }),
    ...(builtinTags && { tags: builtinTags }),
  };

  const result = mapGeneration(
    assistantMsg,
    includeMessageBodies ? (pending?.userParts ?? []) : [],
    assistantParts,
    redactor,
    config.contentCapture,
  );

  const spanOpts = {
    conversationId: assistantMsg.sessionID,
    agentName: buildAgentName(config.agentName, assistantMsg.mode),
    agentVersion,
    requestProvider: assistantMsg.providerID,
    requestModel: assistantMsg.modelID,
    contentCapture: config.contentCapture,
    redactor,
    debugLog,
  };

  // opencode streams provider responses, so generations are exported with
  // mode=STREAM. The SDK only records the gen_ai.client.time_to_first_token
  // histogram for streaming generations.
  const firstAt = firstPartAtByMessage.get(
    messageKey(assistantMsg.sessionID, assistantMsg.id),
  );

  try {
    if (assistantMsg.error) {
      const error = assistantMsg.error;
      await sigil.startStreamingGeneration(seed, async (recorder) => {
        if (firstAt !== undefined) recorder.setFirstTokenAt(new Date(firstAt));
        recorder.setResult(result);
        recorder.setCallError(mapError(error));
        emitToolSpans(sigil, spanRecords, spanOpts);
      });
    } else {
      await sigil.startStreamingGeneration(seed, async (recorder) => {
        if (firstAt !== undefined) recorder.setFirstTokenAt(new Date(firstAt));
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
  firstPartAtByMessage.delete(
    messageKey(assistantMsg.sessionID, assistantMsg.id),
  );
}

function isTerminalMessageUpdate(msg: MessageUpdatedInfo): boolean {
  return Boolean(msg.finish || msg.error || msg.time?.completed);
}

/** Join OpenCode's system entries. Omit the field when all are empty. */
function joinSystemPrompt(system: string[]): string | undefined {
  const joined = system.filter((entry) => entry.length > 0).join("\n");
  return joined.length > 0 ? joined : undefined;
}

/** Remember the OpenCode host version from a session event. */
function recordHostVersion(properties: unknown): void {
  const info = recordField(properties, "info");
  const version = stringField(info, "version");
  if (version) hostVersion = version;
}

/**
 * Record the parent/subagent link from a `session.created` event. opencode
 * sets `Session.parentID` on a subagent's session to the spawning session;
 * root sessions omit it. We resolve the spawning session's latest
 * already-recorded generation now and freeze it as the child's parent (see
 * `parentGenerationByChildSession`).
 *
 * `session.created` fires exactly once, at spawn time, so the frozen edge
 * always points at the parent turn the subagent was launched from. We
 * deliberately do NOT listen on `session.updated`: it fires repeatedly over a
 * session's life, and freezing on a late update could capture a parent turn
 * recorded *after* the spawning one. If the parent has no recorded generation
 * yet at creation (rare — the spawning turn's predecessor is normally already
 * recorded), we skip: an unlinked child is better than a wrong link. The
 * `has(id)` guard is defensive against a duplicate `session.created`.
 */
function recordSessionParent(properties: unknown): void {
  const info = recordField(properties, "info");
  if (!info) return;
  const id = stringField(info, "id");
  const parentID = stringField(info, "parentID");
  if (!id || !parentID || id === parentID) return;
  if (parentGenerationByChildSession.has(id)) return;
  const parentGeneration = lastGenerationIdBySession.get(parentID);
  if (!parentGeneration) return;
  parentGenerationByChildSession.set(id, parentGeneration);
}

/**
 * Resolve the parent generation id for a session's next assistant generation.
 * Prefer the previous assistant generation recorded for this same session
 * (intra-session chain). When this is the session's first generation and the
 * session is a subagent child, fall back to the parent generation frozen at
 * `session.created`, linking the subagent run to the turn it was spawned from.
 */
function resolveParentGenerationId(sessionID: string): string | undefined {
  const intra = lastGenerationIdBySession.get(sessionID);
  if (intra) return intra;
  return parentGenerationByChildSession.get(sessionID);
}

async function handleLifecycle(
  sigil: SigilClient,
  telemetry: TelemetryProviders | null,
  debugLog: (msg: string, ...args: unknown[]) => void,
  event: { type: string; properties: unknown },
): Promise<void> {
  const type = event.type as string;

  if (type === "session.idle") {
    // Recording happens live on `message.updated` and `message.part.updated`
    // (step-finish). Idle only flushes already-recorded events; it does not
    // refetch session history.
    //
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
      lastGenerationIdBySession.delete(sessionId);
      parentGenerationByChildSession.delete(sessionId);
      pendingGenerations.delete(sessionId);
      latestSystemPromptBySession.delete(sessionId);
      sessionContexts.delete(sessionId);
      completedToolExecutions.delete(sessionId);
      for (const key of activeToolExecutions.keys()) {
        if (key.startsWith(`${sessionId}\x00`)) {
          activeToolExecutions.delete(key);
        }
      }
      for (const key of firstPartAtByMessage.keys()) {
        if (key.startsWith(`${sessionId}\x00`)) {
          firstPartAtByMessage.delete(key);
        }
      }
    }
  }

  if (type === "global.disposed") {
    activeToolExecutions.clear();
    completedToolExecutions.clear();
    latestSystemPromptBySession.clear();
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
  debugLog: (msg: string, ...args: unknown[]) => void,
  input: { tool: string; sessionID: string; callID: string },
  output: { args: unknown },
): Promise<void> {
  const key = toolExecutionKey(input.sessionID, input.callID);
  const record: ToolExecutionRecord = {
    sessionID: input.sessionID,
    toolName: input.tool,
    toolCallId: input.callID,
    startedAt: Date.now(),
    completedAt: 0,
    input: output.args,
  };
  activeToolExecutions.set(key, record);

  const guards = config.guards;
  if (guards?.enabled !== true) return;
  const res = await runToolCallGuard({
    client: sigil,
    agentName: agentNameForSession(config, input.sessionID),
    agentVersion: config.agentVersion || hostVersion,
    model: modelForSession(input.sessionID),
    toolCallId: input.callID,
    toolName: input.tool,
    input: output.args ?? {},
    failOpen: guards.failOpen,
    logger: { warn: (msg: string) => debugLog(msg) },
  });
  if (!res) return;
  if ("block" in res) {
    activeToolExecutions.delete(key);
    throw new Error(res.reason);
  }
  // Postflight transform: the server returned the complete redacted argument
  // set. Replace `output.args` with a fresh object rather than mutating the
  // existing one in place: opencode freezes `output.args` on newer versions
  // (>=1.14), so an in-place `delete`/`Object.assign` would throw and, caught
  // below, silently run the ORIGINAL unredacted arguments. Reassigning the
  // property on the (unfrozen) `output` container sidesteps that and still
  // gives opencode the redacted set at execution time. A fresh object also
  // enforces wholesale replacement — keys the server dropped do not survive.
  //
  // Redaction fails open: if the args aren't a plain object or reassignment
  // throws, log and let the original arguments through rather than throwing,
  // which opencode would treat as a tool failure. Because a silently-skipped
  // redaction is indistinguishable from a leak, only log success once the
  // replacement has actually happened (not when the transform was parsed).
  const args = output.args;
  if (!args || typeof args !== "object" || Array.isArray(args)) {
    debugLog(
      `tool-call transform for ${input.callID} dropped: args are not a plain object`,
    );
    return;
  }
  try {
    const redacted = { ...res.transform };
    output.args = redacted;
    // Keep the recorded span consistent with what actually runs.
    record.input = redacted;
    debugLog(`tool-call transform for ${input.callID} applied`);
  } catch (err) {
    debugLog(`tool-call transform apply failed for ${input.callID}`, err);
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
    agentVersion: config.agentVersion || hostVersion,
    model: modelForSession(input.sessionID),
    toolCallId: input.callID,
    toolName: input.type,
    input: {
      pattern: input.pattern,
      title: input.title,
      metadata: input.metadata,
    },
    failOpen: guards.failOpen,
    logger: { warn: (msg: string) => debugLog(msg) },
  });
  // permission.ask carries no tool arguments to rewrite, so only a block is
  // actionable here; a transform result (if any) is ignored.
  if (res && "block" in res) {
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
 * Convert tool executions that started but never completed for this session
 * into error records, removing them from the active map. opencode skips
 * `tool.execute.after` when a tool throws or a permission deny aborts it, so
 * an entry still active when the assistant message goes terminal is a failed,
 * denied, or interrupted call. Without this it produces no span (in
 * `metadata_only`, where terminal parts aren't fetched) and leaks from
 * `activeToolExecutions` until `session.deleted`.
 *
 * `startedAt` is the real value from the before hook; `completedAt` is
 * approximate (we have no real end time) and the reason is generic because the
 * hook can't tell an error from a deny from an interrupt.
 *
 * @internal Exported for testing.
 */
export function drainActiveToolExecutions(
  sessionID: string,
): ToolExecutionRecord[] {
  const drained: ToolExecutionRecord[] = [];
  const now = Date.now();
  for (const [key, record] of activeToolExecutions) {
    if (record.sessionID !== sessionID) continue;
    activeToolExecutions.delete(key);
    drained.push({
      ...record,
      completedAt: now,
      isError: true,
      error: "tool did not complete (errored, denied, or interrupted)",
    });
  }
  return drained;
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
  systemTransform: (
    input: { sessionID?: string; model?: { id?: string } },
    output: { system: string[] },
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
  options: { projectDir?: string } = {},
): Promise<SigilHooks | null> {
  // Prefer the opencode plugin's project directory (PluginInput.directory)
  // because the opencode server can run from a directory different from the
  // project root. Fall back to `process.cwd()` for older callers and tests.
  const projectDir = options.projectDir || process.cwd();
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
      await handleEvent(
        sigil,
        config,
        client,
        redactor,
        debugLog,
        projectDir,
        input.event,
      );
      await handleLifecycle(sigil, telemetry, debugLog, input.event);
    },
    chatMessage: (input, output) => {
      handleChatMessage(input, output);
    },
    systemTransform: (input, output) => {
      handleSystemTransform(input, output, debugLog);
    },
    toolExecuteBefore: async (input, output) => {
      await handleToolExecuteBefore(sigil, config, debugLog, input, output);
    },
    toolExecuteAfter: (input, output) => {
      handleToolExecuteAfter(input, output);
    },
    permissionAsk: async (input, output) => {
      await handlePermissionAsk(sigil, config, debugLog, input, output);
    },
  };
}
