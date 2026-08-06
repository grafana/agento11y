import { randomUUID } from "node:crypto";
import type { Agento11yClient, ContentCaptureMode } from "@grafana/agento11y";
import {
  redactSecretText,
  redactSecretTextLightweight,
} from "@grafana/agento11y-core";
import type { PluginInput } from "@opencode-ai/plugin";
import type {
  AssistantMessage,
  Event,
  Part,
  Permission,
  UserMessage,
} from "@opencode-ai/sdk";
import { createAgento11yClient } from "./client.js";
import type { Agento11yOpencodeConfig } from "./config.js";
import { resolveAutoTagValues } from "./config.js";
import { resolveGitBranch } from "./git.js";
import { runToolCallGuard } from "./guard.js";
import { stableOpencodeGenerationId } from "./lineage.js";
import {
  legacyToolOverrideNames,
  mapError,
  mapGeneration,
  mapToolDefinitions,
  type OpencodeTokens,
  type Redactor,
} from "./mappers.js";
import { buildBuiltinTags } from "./tags.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

type OpencodeClient = PluginInput["client"];

/**
 * Event names opencode declares it can deliver to a plugin. opencode feeds the
 * `event` hook from its own event bus and publishes members of the SDK's
 * generated `Event` union. Typing the hook input against that union keeps a
 * name outside it from compiling.
 */
export type OpencodeEventType = Event["type"];

export type OpencodeEvent = {
  type: OpencodeEventType;
  properties: unknown;
};

/**
 * Builds the redactor the mappers and hooks share.
 *
 * opencode redacts inside its own mappers rather than through the SDK's
 * generation sanitizer, so it calls the shared string helpers from
 * `@grafana/agento11y-core`, which carry the patterns generated from
 * `redaction/patterns.json`.
 *
 * Email redaction is off: agent transcripts routinely carry commit authors and
 * reviewer addresses, and redacting them costs more context than it protects.
 * See `redaction/README.md`.
 */
export function createRedactor(): Redactor {
  return {
    redact: (text: string) =>
      redactSecretText(text, { redactEmailAddresses: false }),
    redactLightweight: (text: string) =>
      redactSecretTextLightweight(text, { redactEmailAddresses: false }),
  };
}

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

// The latest assistant message seen per session, completed or still streaming.
// A subagent is spawned from a tool call inside an assistant turn, so this is
// the turn the child's first generation links to. `lastGenerationIdBySession`
// cannot serve: it is written when a turn is recorded, and the child's
// `session.created` arrives while the spawning turn is still in flight.
const lastSeenAssistantMessageBySession = new Map<string, string>();

// Maps a child (subagent) session id to the parent generation its first
// assistant turn should link to. opencode runs a subagent in a fresh session
// whose `Session.parentID` points at the spawning session, surfaced via the
// `session.created` event. The edge is derived from the latest assistant turn
// seen for the parent, the turn holding the task call, and frozen here. That
// turn is normally still streaming, but a parent that spawns the subagent from
// an already-completed turn resolves the same way.
//
// Freezing at creation (rather than at child-record time) is deliberate: by the
// time the child records, the parent may have started or finished later turns,
// and a lazy resolver would name whichever turn is latest instead of the one
// that spawned the child.
//
// `AssistantMessage.parentID` is a message-level pointer within one session and
// is NOT the same as `Session.parentID`; only the latter crosses the
// parent/subagent boundary.
const parentGenerationByChildSession = new Map<string, string>();

// Maps a child (subagent) session id to its immediate parent session id, set
// from `session.created` whether or not the generation edge resolves. Once an
// edge is frozen for the child, a duplicate event cannot change this, so the
// two always name the same parent. Three readers: the `subagent` tag, the
// exported conversation id (a linked child's turns are reparented onto the
// spawning conversation), and the `opencode.parent_session_id` metadata, which
// is the only lineage signal left when the edge does not resolve.
const parentSessionByChildSession = new Map<string, string>();

// First streamed assistant part time per message, keyed by
// `${sessionID}\x00${messageID}`. Captured from `message.part.updated`
// before the message completes so it survives `metadata_only` (where we
// never fetch the message body). Consumed and cleared when the message is
// recorded.
const firstPartAtByMessage = new Map<string, number>();

// Token counts summed over every provider step of one assistant message, keyed
// by `${sessionID}\x00${messageID}`. opencode adds each step's cost to
// `AssistantMessage.cost` but overwrites `AssistantMessage.tokens` with the
// latest step, so a multi-step message pairs last-step tokens with all-steps
// cost. `StepFinishPart` carries the per-step counts on `message.part.updated`,
// so this also works in `metadata_only`, where message bodies are never
// fetched. Consumed and cleared when the message is recorded.
//
// opencode 1.18.x runs one step per message, except when `Effect.retry` in
// `session/processor.ts` retries a stream attempt. This code still sums,
// because hosts back to `@opencode-ai/plugin` ^1.2.16 are supported.
const stepTokensByMessage = new Map<string, OpencodeTokens>();

function messageKey(sessionID: string, messageID: string): string {
  return `${sessionID}\x00${messageID}`;
}

/** Drop every entry of a message-keyed map that belongs to one session. */
function deleteSessionEntries<V>(map: Map<string, V>, sessionID: string): void {
  const prefix = `${sessionID}\x00`;
  for (const key of map.keys()) {
    if (key.startsWith(prefix)) map.delete(key);
  }
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

// Latest real `Session.title` per session, from the same
// `session.created`/`session.updated` events that carry the host version.
// Latest wins: opencode generates the title asynchronously with its small
// model (so it usually arrives after the first turn has been exported) and a
// user can rename a session at any time. Blank and placeholder titles are
// never stored, so they cannot clear a real one.
const latestSessionTitleBySession = new Map<string, string>();

// At session creation opencode seeds a placeholder rather than an empty title:
// `New session - <ISO>`, or `Child session - <ISO>` for subagents. The ISO
// string is the session's creation time, so sending a placeholder is worse than
// sending nothing: unique per session, and no use as a conversation name. Same
// regex opencode uses on its own placeholders.
const placeholderSessionTitle =
  /^(New session - |Child session - )\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;

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

// Plugin instances that have been created and not yet shut down. One opencode
// server can host an instance per directory, and all the state above is
// module-scoped, so a shutting-down instance must not clear it while a sibling
// is mid-turn. Dropping `pendingGenerations` there would export that turn
// without its input, tools, and system prompt. Only the last instance to shut
// down clears the state, so an earlier instance's sessions stay in the maps
// until then; `session.deleted` is what normally releases them.
let liveInstances = 0;

/**
 * Resets every module-level map: the dedup/generation tracking plus the
 * tool-execution maps.
 *
 * Two callers. Plugin shutdown calls it once the last live plugin instance in
 * the process has shut down, so this runs in production and not only in tests.
 * Integration tests that drive the full record path (`chat.message` ->
 * `message.updated`) also need it between cases: without it, a reused
 * session/message id hits the dedup early-return in `recordAssistantMessage`,
 * silently skips recording, and produces a misleading green.
 *
 * @internal Exported for testing.
 */
export function _resetHookState(): void {
  recordedMessages.clear();
  lastGenerationIdBySession.clear();
  lastSeenAssistantMessageBySession.clear();
  parentGenerationByChildSession.clear();
  parentSessionByChildSession.clear();
  firstPartAtByMessage.clear();
  stepTokensByMessage.clear();
  pendingGenerations.clear();
  latestSystemPromptBySession.clear();
  latestSessionTitleBySession.clear();
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
  sigil: Agento11yClient,
  config: Agento11yOpencodeConfig,
  client: OpencodeClient,
  redactor: Redactor,
  debugLog: (msg: string, ...args: unknown[]) => void,
  projectDir: string,
  event: OpencodeEvent,
): Promise<void> {
  if (event.type === "session.created" || event.type === "session.updated") {
    recordHostVersion(event.properties);
    recordSessionTitle(event.properties, redactor);
    if (event.type === "session.created") {
      recordSessionParent(event.properties, debugLog);
    }
    return;
  }
  if (event.type === "message.part.updated") {
    handleMessagePartUpdated(event.properties);
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

  // Tracked for every assistant update, completed or not, because a subagent
  // spawned inside this turn links to it (see
  // `lastSeenAssistantMessageBySession`).
  if (assistantMsg.sessionID && assistantMsg.id) {
    lastSeenAssistantMessageBySession.set(
      assistantMsg.sessionID,
      assistantMsg.id,
    );
  }

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

/**
 * Consume a streamed part: the time-to-first-token signal and, for
 * `step-finish`, that step's token counts.
 *
 * Does not export. `Session.updateMessage` publishes `message.updated` at every
 * step-finish and again on completion, abort, and error. A host that delivers
 * parts therefore delivers the terminal message update too, so recording waits
 * for that instead.
 */
function handleMessagePartUpdated(properties: unknown): void {
  const part = recordField(properties, "part");
  if (!part) return;
  // `Session.fork` clones a finished message under fresh ids, then republishes
  // its parts. Nothing reads per-message state after the message is recorded,
  // so drop those parts rather than let them fill the maps.
  const sessionID = stringField(part, "sessionID");
  const messageID = stringField(part, "messageID");
  if (!sessionID || !messageID) return;
  if (recordedMessages.get(sessionID)?.has(messageID)) return;
  recordFirstPartTime(part, sessionID, messageID);
  accumulateStepTokens(part, messageKey(sessionID, messageID));
}

/**
 * Add one `step-finish` part's token counts to its message's running totals.
 * A field that is not a finite number contributes nothing, so a malformed
 * payload cannot poison the totals with NaN. opencode floors its own counts at
 * zero in `Session.getUsage`, so only junk reaches that guard.
 */
function accumulateStepTokens(
  part: Record<string, unknown>,
  key: string,
): void {
  if (stringField(part, "type") !== "step-finish") return;
  const tokens = recordField(part, "tokens");
  if (!tokens) return;
  const cache = recordField(tokens, "cache");
  const step = {
    input: numberField(tokens, "input"),
    output: numberField(tokens, "output"),
    reasoning: numberField(tokens, "reasoning"),
    read: numberField(cache, "read"),
    write: numberField(cache, "write"),
  };
  // No parseable count means no observed usage. Storing zeros here would look
  // like a real all-zero step and suppress the `msg.tokens` fallback.
  if (Object.values(step).every((count) => count === undefined)) return;

  const totals = stepTokensByMessage.get(key) ?? {
    input: 0,
    output: 0,
    reasoning: 0,
    cache: { read: 0, write: 0 },
  };
  totals.input += step.input ?? 0;
  totals.output += step.output ?? 0;
  totals.reasoning += step.reasoning ?? 0;
  totals.cache.read += step.read ?? 0;
  totals.cache.write += step.write ?? 0;
  stepTokensByMessage.set(key, totals);
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
function recordFirstPartTime(
  part: Record<string, unknown>,
  sessionID: string,
  messageID: string,
): void {
  const type = stringField(part, "type");
  if (type !== "text" && type !== "reasoning" && type !== "tool") return;
  const key = messageKey(sessionID, messageID);
  if (firstPartAtByMessage.has(key)) return;
  const rawStart = recordField(part, "time")?.start;
  firstPartAtByMessage.set(
    key,
    typeof rawStart === "number" ? rawStart : Date.now(),
  );
}

async function recordAssistantMessage(
  sigil: Agento11yClient,
  config: Agento11yOpencodeConfig,
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

  // Only record terminal messages. `finish` is not terminal: opencode sets it
  // at every `step-finish`, so only `error` or `time.completed` end a message.
  const isTerminal = assistantMsg.error || assistantMsg.time.completed;
  if (!isTerminal) return;

  const msgKey = messageKey(assistantMsg.sessionID, assistantMsg.id);

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
  // `git.branch` is resolved per turn so a mid-session checkout shows up on
  // the next generation. These are sent regardless of content capture mode:
  // they are low-cardinality session metadata, not message content, matching
  // claude-code/cursor.
  const builtinTags = buildBuiltinTags({
    cwd: projectDir,
    gitBranch: resolveGitBranch(projectDir),
    isSubagent: parentSessionByChildSession.has(assistantMsg.sessionID),
  });
  const conversationId = resolveExportedConversationId(assistantMsg.sessionID);
  const reparented = conversationId !== assistantMsg.sessionID;
  const lineageMetadata = subagentLineageMetadata(
    assistantMsg.sessionID,
    reparented,
  );
  // Sent regardless of content capture mode: the title is host-provided
  // session metadata that opencode shows in its own UI, and the SDK drops it
  // from a `metadata_only` export itself. Already redacted by
  // `recordSessionTitle`.
  //
  // A reparented child sends no title at all: the backend keeps the newest
  // title per conversation, so a subagent finishing after the parent's last
  // turn would rename the shared conversation to the subagent's title.
  const conversationTitle = reparented
    ? undefined
    : latestSessionTitleBySession.get(assistantMsg.sessionID);
  const seed = {
    id: genId,
    conversationId,
    agentName: buildAgentName(config.agentName, assistantMsg.mode),
    agentVersion,
    effectiveVersion: agentVersion,
    model: { provider: assistantMsg.providerID, name: assistantMsg.modelID },
    startedAt: new Date(assistantMsg.time.created),
    contentCapture: config.contentCapture,
    ...(conversationTitle && { conversationTitle }),
    ...(parent && { parentGenerationIds: [parent] }),
    ...(tools.length > 0 && { tools }),
    ...(includeMessageBodies && { systemPrompt }),
    ...(builtinTags && { tags: builtinTags }),
    ...(lineageMetadata && { metadata: lineageMetadata }),
  };

  // Without an observed step the export pairs last-step tokens with all-steps
  // cost, the mismatch this accumulator removes. A turn that aborts before its
  // first step-finish hits the fallback legitimately, so log at debug.
  const stepTokens = stepTokensByMessage.get(msgKey);
  if (stepTokens === undefined) {
    debugLog(
      `no step-finish tokens observed for session=${assistantMsg.sessionID} message=${assistantMsg.id}; using the message's own token counts`,
    );
  }

  const result = mapGeneration(
    assistantMsg,
    includeMessageBodies ? (pending?.userParts ?? []) : [],
    assistantParts,
    redactor,
    config.contentCapture,
    stepTokens,
  );

  const spanOpts = {
    conversationId,
    conversationTitle,
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
  const firstAt = firstPartAtByMessage.get(msgKey);

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
    debugLog("agento11y generation export failed", err);
    // agento11y recording failure should never break the plugin
  }

  // Clean up pending generation and per-turn tool records. The completed
  // records were consumed above; clearing them prevents duplicate spans if
  // another export path fires for the same session.
  pendingGenerations.delete(assistantMsg.sessionID);
  completedToolExecutions.delete(assistantMsg.sessionID);
  firstPartAtByMessage.delete(msgKey);
  stepTokensByMessage.delete(msgKey);
}

function isTerminalMessageUpdate(msg: MessageUpdatedInfo): boolean {
  return Boolean(msg.error || msg.time?.completed);
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
 * Remember the latest real `Session.title` from a session event, redacted here so
 * the map only holds exportable text. `plugins/opencode/src/client.ts` installs no
 * SDK `generationSanitizer` the way pi does, so this is the title's only
 * redaction, and opencode's small model writes it from the user's prompt, which
 * may contain a pasted token.
 */
function recordSessionTitle(properties: unknown, redactor: Redactor): void {
  const info = recordField(properties, "info");
  const id = stringField(info, "id");
  const title = stringField(info, "title")?.trim();
  if (!id || !title || placeholderSessionTitle.test(title)) return;
  latestSessionTitleBySession.set(id, redactor.redactLightweight(title));
}

/**
 * Record the parent/subagent link from a `session.created` event. opencode
 * sets `Session.parentID` on a subagent's session to the spawning session;
 * root sessions omit it. The child's parent generation is the latest assistant
 * turn seen for the parent session, the turn holding the task call, and it is
 * frozen here (see `parentGenerationByChildSession`). That turn is usually
 * still streaming, and its deterministic generation id is known before it is
 * recorded.
 *
 * `session.created` fires exactly once, at spawn time, so the frozen edge
 * always points at the parent turn the subagent was launched from. We
 * deliberately do NOT listen on `session.updated`: it fires repeatedly over a
 * session's life, and freezing on a late update could capture a turn that
 * started *after* the spawning one.
 *
 * When no assistant turn has been seen for the parent session, the edge is
 * skipped and the child keeps its own conversation with
 * `opencode.parent_session_id` metadata: an unlinked child is better than a
 * wrong link. The plugin loading mid-run is how that happens in practice. The
 * `subagent` tag comes from `Session.parentID` alone and survives either way.
 *
 * The `has(id)` guard is defensive against a duplicate `session.created`. It
 * covers the parent session too, because a second event naming a different
 * parent would otherwise leave the recorded parent session and the frozen edge
 * pointing at different conversations.
 */
function recordSessionParent(
  properties: unknown,
  debugLog: (msg: string, ...args: unknown[]) => void,
): void {
  const info = recordField(properties, "info");
  if (!info) return;
  const id = stringField(info, "id");
  const parentID = stringField(info, "parentID");
  if (!id || !parentID || id === parentID) return;
  if (parentGenerationByChildSession.has(id)) return;
  parentSessionByChildSession.set(id, parentID);
  const spawningMessage = lastSeenAssistantMessageBySession.get(parentID);
  if (!spawningMessage) {
    debugLog(
      `no assistant turn seen for parent session=${parentID}; subagent session=${id} exports unlinked in its own conversation`,
    );
    return;
  }
  parentGenerationByChildSession.set(
    id,
    stableOpencodeGenerationId(parentID, spawningMessage),
  );
}

/**
 * The conversation id a session's turns are exported under: its own session id,
 * or the conversation of the run that spawned it.
 *
 * A linked subagent's turns belong to the spawning conversation, so the parent's
 * Dependencies view can draw the DAG and the frozen parent edge names a
 * generation in the same conversation. codex
 * (`plugins/agento11y/internal/agents/codex/mapper/mapper.go`) and vibe
 * (`plugins/agento11y/internal/agents/vibe/mapper/mapper.go`) reparent onto the
 * immediate parent session and stop there, so a grandchild of theirs lands in
 * the child's session id while the child went to the root.
 *
 * This walk follows `parentSessionByChildSession` while each ancestor is itself
 * a linked child, so a nested subagent flattens onto the root session and its
 * edge stays inside one conversation. An ancestor whose own edge did not resolve
 * stops the walk, because that ancestor's turns stayed in its own conversation.
 *
 * The visited set both terminates the walk and keeps it defensive: every hop
 * that continues adds a session id not seen before, so a contradictory parent
 * chain from the host stops at the first repeat instead of looping.
 */
function resolveExportedConversationId(sessionID: string): string {
  let current = sessionID;
  const seen = new Set<string>([current]);
  while (parentGenerationByChildSession.has(current)) {
    const parent = parentSessionByChildSession.get(current);
    if (!parent || seen.has(parent)) return current;
    seen.add(parent);
    current = parent;
  }
  return current;
}

/**
 * Session-level lineage for a subagent turn, following
 * `codex.parent_session_id` and `codex.child_session_id`.
 *
 * `opencode.parent_session_id` names the immediate parent session, not the root
 * of the chain, so a nested subagent's depth survives the flattening in
 * `resolveExportedConversationId`. It is emitted whether or not the edge
 * resolved: it is the only lineage signal an unlinked child carries.
 *
 * `opencode.child_session_id` is emitted only for a reparented turn, where
 * `conversation_id` names an ancestor session and the child's own session id
 * would otherwise be unrecoverable. codex emits its key unconditionally
 * (`plugins/agento11y/internal/agents/codex/mapper/mapper.go:161`); the
 * condition here follows vibe
 * (`plugins/agento11y/internal/agents/vibe/mapper/mapper.go:256`), where an
 * unreparented turn already carries its own session id as `conversation_id`.
 */
function subagentLineageMetadata(
  sessionID: string,
  reparented: boolean,
): Record<string, string> | undefined {
  const parentSession = parentSessionByChildSession.get(sessionID);
  if (!parentSession) return undefined;
  return {
    "opencode.parent_session_id": parentSession,
    ...(reparented && { "opencode.child_session_id": sessionID }),
  };
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
  sigil: Agento11yClient,
  telemetry: TelemetryProviders | null,
  debugLog: (msg: string, ...args: unknown[]) => void,
  shutdownOnce: () => Promise<void>,
  event: OpencodeEvent,
): Promise<void> {
  const type = event.type;

  if (type === "session.idle") {
    // Recording happens live on `message.updated`. Idle only flushes
    // already-recorded events; it does not refetch session history.
    //
    // Fire-and-forget: a stuck OTLP endpoint must not block session.idle for
    // up to ~30s (BatchSpanProcessor default) per turn.
    void sigil.flush().catch((err) => debugLog("agento11y flush failed", err));
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
      lastSeenAssistantMessageBySession.delete(sessionId);
      parentGenerationByChildSession.delete(sessionId);
      parentSessionByChildSession.delete(sessionId);
      pendingGenerations.delete(sessionId);
      latestSystemPromptBySession.delete(sessionId);
      latestSessionTitleBySession.delete(sessionId);
      sessionContexts.delete(sessionId);
      completedToolExecutions.delete(sessionId);
      deleteSessionEntries(activeToolExecutions, sessionId);
      deleteSessionEntries(firstPartAtByMessage, sessionId);
      deleteSessionEntries(stepTokensByMessage, sessionId);
    }
  }

  // Covers hosts older than @opencode-ai/plugin 1.15.11, which have no
  // `Hooks.dispose`. On those, the per-instance event bus publishes this to its
  // own subscribers as the instance tears down, so the `event` hook still sees
  // it. On 1.16 and later the plugin's subscription is closed before the
  // instance store publishes the event, and `Hooks.dispose` is the trigger that
  // runs. Both share the same idempotent path.
  if (type === "server.instance.disposed") {
    await shutdownOnce();
  }
}

async function handleToolExecuteBefore(
  sigil: Agento11yClient,
  config: Agento11yOpencodeConfig,
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
  sigil: Agento11yClient,
  config: Agento11yOpencodeConfig,
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
  config: Agento11yOpencodeConfig,
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

function numberField(value: unknown, key: string): number | undefined {
  if (!value || typeof value !== "object") return undefined;
  const field = (value as Record<string, unknown>)[key];
  return typeof field === "number" && Number.isFinite(field)
    ? field
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
 * Emit agento11y tool execution spans for a set of completed tool records.
 * Errors thrown by the SDK are swallowed so a span failure cannot break
 * the plugin.
 *
 * @internal Exported for testing.
 */
export function emitToolSpans(
  client: Agento11yClient,
  records: ToolExecutionRecord[],
  opts: {
    conversationId: string;
    conversationTitle?: string;
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
        conversationTitle: opts.conversationTitle,
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

export type Agento11yHooks = {
  event: (input: { event: OpencodeEvent }) => Promise<void>;
  /**
   * Flushes and shuts down the agento11y client and the OTel providers. Clears
   * the module-level hook state once no other plugin instance in the process
   * is still live. Safe to call more than once: later calls wait on the first
   * shutdown instead of starting another.
   */
  dispose: () => Promise<void>;
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

export async function createAgento11yHooks(
  config: Agento11yOpencodeConfig,
  client: OpencodeClient,
  options: { projectDir?: string } = {},
): Promise<Agento11yHooks | null> {
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

  const sigil = createAgento11yClient(
    // Auto-tags are resolved here rather than in resolveConfig because this is
    // where the project directory is known: the repository and branch must come
    // from the checkout opencode is working in.
    { ...config, autoTags: resolveAutoTagValues(projectDir) },
    {
      tracer: telemetry?.tracer,
      meter: telemetry?.meter,
    },
  );
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

  const redactor = createRedactor();

  liveInstances += 1;

  // Single shutdown path for every teardown trigger, memoized on a promise
  // rather than gated by a boolean so a later trigger waits for the in-flight
  // shutdown instead of returning while the exporters are still draining. The
  // guard is per plugin instance.
  let shutdownPromise: Promise<void> | null = null;
  const shutdownOnce = (): Promise<void> => {
    shutdownPromise ??= (async () => {
      process.off("beforeExit", onBeforeExit);
      liveInstances -= 1;
      try {
        await sigil.shutdown();
      } catch (err) {
        debugLog("agento11y shutdown failed", err);
      }
      if (telemetry) {
        try {
          await telemetry.shutdown();
        } catch (err) {
          debugLog("telemetry shutdown failed", err);
        }
      }
      if (liveInstances <= 0) _resetHookState();
    })();
    return shutdownPromise;
  };

  const onBeforeExit = (): void => {
    void shutdownOnce();
  };

  // Last-resort trigger for hosts that neither invoke `Hooks.dispose` (see the
  // version note in index.ts) nor deliver the disposal event. It is a weak one,
  // because `beforeExit` does not fire on SIGINT, SIGTERM, or `process.exit()`.
  // Deregistering inside `shutdownOnce` keeps a disposed instance from holding a
  // process listener.
  process.on("beforeExit", onBeforeExit);

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
      await handleLifecycle(
        sigil,
        telemetry,
        debugLog,
        shutdownOnce,
        input.event,
      );
    },
    dispose: async () => {
      await shutdownOnce();
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
