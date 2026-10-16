import type { Agento11yClient, Message } from "@grafana/agento11y";
import type { Agento11yOpencodeConfig } from "./config.js";
import { resolveGitBranch } from "./git.js";
import { stableOpencodeGenerationId } from "./lineage.js";
import type { Redactor } from "./mappers.js";
import { buildBuiltinTags } from "./tags.js";
import { emitToolSpans } from "./toolSpans.js";
import {
  mapNativeGeneration,
  type NativeBlock,
  type NativeEvent,
  type NativeEventOf,
  type NativeLocation,
  type NativeModel,
  type NativeTool,
  nativeAgentName,
  nativeOutput,
  nativeToolRecords,
} from "./v2-mappers.js";

export type { NativeEvent } from "./v2-mappers.js";

type InboxItem = NativeEventOf<"session.inbox.enqueued">["data"]["item"];
type Terminal = NativeEventOf<
  "session.step.ended" | "session.step.failed"
>["data"];
interface Assistant {
  id: string;
  model: NativeModel;
  agent: string;
  startedAt: number;
  firstAt?: number;
  streamedAt?: number;
  input: Message[];
  systemPrompt?: string;
  blocks: NativeBlock[];
  parent?: string;
  subagent: boolean;
  conversationId: string;
  title?: string;
  lineage?: Record<string, string>;
}
interface SessionState {
  history: Message[];
  instructions: string[];
  inbox: Map<string, InboxItem>;
  assistant?: Assistant;
  lastGeneration?: string;
  title?: string;
  parentSession?: string;
  spawningGeneration?: string;
  conversationId: string;
}

export interface NativeCaptureOptions {
  client: Agento11yClient;
  config: Agento11yOpencodeConfig;
  redactor: Redactor;
  directory: string;
  workspaceID?: string;
  /** Running OpenCode version, the default when no agent version is configured. */
  hostVersion?: string;
  debugLog: (message: string, ...args: unknown[]) => void;
}

/** Setup-local, synchronous event fold. Export promises never block host guard callbacks. */
export function createNativeCapture({
  client,
  config,
  redactor,
  directory,
  workspaceID,
  hostVersion,
  debugLog,
}: NativeCaptureOptions): {
  handleEvent(event: NativeEvent): void;
  shutdown(): Promise<void>;
  drain(): Promise<void>;
} {
  const sessions = new Map<string, SessionState>();
  // Ownership comes from events, never from a later session lookup.
  const locations = new Map<string, NativeLocation>();
  const locationSince = new Map<string, number>();
  const settled = new Set<string>();
  const seen = new Set<string>();
  const tasks = new Set<Promise<void>>();
  const agentVersion = config.agentVersion || hostVersion;
  let closed = false;
  // After location.shutdown a replacement instance owns new work; running steps finish here.
  let draining = false;
  let shutdownTask: Promise<void> | undefined;
  const owns = (location?: NativeLocation) =>
    location !== undefined &&
    location.directory === directory &&
    location.workspaceID === workspaceID;
  const enqueue = (work: () => Promise<void>) => {
    const task = Promise.resolve()
      .then(work)
      .catch(() => {
        debugLog("native telemetry task failed");
      })
      .finally(() => tasks.delete(task));
    tasks.add(task);
  };
  const getSession = (id: string): SessionState => {
    let session = sessions.get(id);
    if (!session) {
      session = {
        history: [],
        instructions: [],
        inbox: new Map(),
        conversationId: id,
      };
      sessions.set(id, session);
    }
    return session;
  };
  const settle = (
    sessionID: string,
    session: SessionState,
    at: number,
    reason: string,
    terminal?: Terminal,
  ) => {
    const assistant = session.assistant;
    if (!assistant) return;
    session.assistant = undefined;
    if (draining) sessions.delete(sessionID);
    const id = stableOpencodeGenerationId(sessionID, assistant.id);
    if (settled.has(id)) return;
    settled.add(id); // Commit before scheduling any asynchronous export.
    session.lastGeneration = id;
    session.history.push(...nativeOutput(assistant.blocks));
    const result = mapNativeGeneration(
      {
        ...assistant,
        terminalAt: at,
        settlement: reason,
        tokens: terminal?.tokens,
        cost: terminal?.cost,
        finish:
          terminal?.finish ?? (reason === "step.failed" ? "error" : undefined),
      },
      redactor,
      config.contentCapture,
      config.redactInputMessages,
    );
    const records = nativeToolRecords(sessionID, assistant.blocks);
    const seed = {
      id,
      conversationId: assistant.conversationId,
      conversationTitle: assistant.title,
      parentGenerationIds: assistant.parent ? [assistant.parent] : undefined,
      agentName: nativeAgentName(config.agentName, assistant.agent),
      agentVersion,
      effectiveVersion: agentVersion,
      model: { provider: assistant.model.providerID, name: assistant.model.id },
      startedAt: new Date(assistant.startedAt),
      contentCapture: config.contentCapture,
      systemPrompt:
        config.contentCapture !== "metadata_only" &&
        assistant.systemPrompt !== undefined
          ? redactor.redact(assistant.systemPrompt)
          : undefined,
      // Per turn, like v1, so a mid-session checkout reaches the next turn.
      tags: buildBuiltinTags({
        cwd: directory,
        gitBranch: resolveGitBranch(directory),
        isSubagent: assistant.subagent,
      }),
      metadata: assistant.lineage,
      tools: [
        ...new Set(
          assistant.blocks.flatMap((block) =>
            block.type === "tool" ? [block.name] : [],
          ),
        ),
      ]
        .sort()
        .map((name) => ({ name, type: "function" })),
    };
    // Host failure messages may contain secrets; retain only the structured error type.
    const error =
      terminal && "error" in terminal
        ? terminal.error.type
        : reason === "step.ended" || reason === "execution.succeeded"
          ? undefined
          : `opencode.${reason}`;
    enqueue(async () => {
      await client.startStreamingGeneration(seed, async (recorder) => {
        if (assistant.firstAt !== undefined)
          recorder.setFirstTokenAt(new Date(assistant.firstAt));
        recorder.setResult(result);
        if (error) recorder.setCallError(new Error(error));
        emitToolSpans(client, records, {
          conversationId: assistant.conversationId,
          conversationTitle: assistant.title,
          agentName: seed.agentName,
          agentVersion,
          requestProvider: assistant.model.providerID,
          requestModel: assistant.model.id,
          contentCapture: config.contentCapture,
          redactor,
          debugLog,
        });
      });
    });
  };
  const textMessage = (text: string): Message => ({
    role: "user",
    parts: [{ type: "text", text }],
  });
  const deliver = (item: InboxItem): Message[] => {
    if (item.type === "synthetic") return [textMessage(item.payload.text)];
    if (item.type !== "user") return [];
    // Skills are observed plaintext; binary attachments/signatures aren't reconstructed.
    const texts = [
      ...(item.payload.skills ?? []).flatMap((skill) =>
        skill.text === undefined ? [] : [skill.text],
      ),
      item.payload.text,
    ];
    return [
      {
        role: "user",
        parts: texts.filter(Boolean).map((text) => ({ type: "text", text })),
      },
    ];
  };
  const tool = (assistant: Assistant, id: string): NativeTool | undefined =>
    assistant.blocks.find(
      (block): block is NativeTool => block.type === "tool" && block.id === id,
    );

  function handleEvent(event: NativeEvent): void {
    if (closed || seen.has(event.id)) return;
    if (event.type === "location.shutdown") {
      if (owns(event.location)) {
        draining = true;
        for (const [id, session] of sessions)
          if (!session.assistant) sessions.delete(id);
      }
      return;
    }
    if (
      !("sessionID" in event.data) ||
      typeof event.data.sessionID !== "string"
    )
      return;
    const sessionID = event.data.sessionID;
    if (draining && !sessions.get(sessionID)?.assistant) return;
    const since = locationSince.get(sessionID);
    if (
      since !== undefined &&
      "durable" in event &&
      event.durable.seq < since &&
      (!event.location ||
        event.type === "session.moved" ||
        event.type === "session.created")
    )
      return;
    if (event.type === "session.moved") {
      const previous = locations.get(sessionID) ?? event.location;
      if (owns(previous)) {
        const session = sessions.get(sessionID);
        if (session) settle(sessionID, session, event.created, "moved");
      }
      // Neither side imports the other location's retained history or queued inbox.
      sessions.delete(sessionID);
      if (owns(previous) || owns(event.data.location)) seen.add(event.id);
      locations.set(sessionID, { ...event.data.location });
      locationSince.set(sessionID, event.durable.seq);
      return;
    }
    const location =
      event.type === "session.created"
        ? event.data.location
        : (event.location ?? locations.get(sessionID));
    if (
      event.type === "session.created" &&
      event.location &&
      !owns(event.location)
    )
      return;
    if (!owns(location)) return; // No content retained, even temporarily, before this check.
    // A queued envelope from before a move must not restore the old owner.
    const known = locations.get(sessionID);
    if (known && !owns(known) && event.type !== "session.created") return;
    seen.add(event.id);
    if (location && !known) {
      locations.set(sessionID, { ...location });
      if ("durable" in event) locationSince.set(sessionID, event.durable.seq);
    }
    const session = getSession(sessionID);
    switch (event.type) {
      case "session.created": {
        session.title = event.data.title
          ? redactor.redact(event.data.title)
          : undefined;
        session.parentSession = event.data.parentID;
        const parent = event.data.parentID
          ? sessions.get(event.data.parentID)
          : undefined;
        // Only an observed spawning response establishes an edge; parentID alone doesn't.
        if (parent?.assistant && !session.spawningGeneration) {
          session.spawningGeneration = stableOpencodeGenerationId(
            event.data.parentID as string,
            parent.assistant.id,
          );
          session.conversationId = parent.assistant.conversationId;
        }
        return;
      }
      case "session.renamed":
        session.title = redactor.redact(event.data.title);
        return;
      case "session.inbox.enqueued":
        session.inbox.set(event.data.inboxID, structuredClone(event.data.item));
        return;
      case "session.inbox.cancelled":
        session.inbox.delete(event.data.inboxID);
        return;
      case "session.inbox.delivered": {
        const item = session.inbox.get(event.data.inboxID);
        if (item) session.history.push(...deliver(item));
        session.inbox.delete(event.data.inboxID);
        return;
      }
      case "session.synthetic":
      case "session.skill.activated":
        session.history.push(textMessage(event.data.text));
        return;
      case "session.instructions.updated":
        // These are chronological updates, not a complete effective system snapshot.
        if (event.data.text?.trim()) session.instructions.push(event.data.text);
        return;
      case "session.compaction.ended":
        // This is an observed checkpoint, not a claim of the final provider request.
        // Opaque native providerContext is deliberately not decoded/reconstructed.
        session.history = [textMessage(event.data.text)];
        if (event.data.recent !== undefined)
          session.history.push(textMessage(event.data.recent));
        session.instructions = [];
        return;
      case "session.step.started": {
        const id = stableOpencodeGenerationId(
          sessionID,
          event.data.assistantMessageID,
        );
        if (settled.has(id)) return;
        if (session.assistant?.id === event.data.assistantMessageID) {
          // Transparent retries refresh instructions upstream. Keep FIRST observed prefix/time.
          session.assistant.streamedAt = undefined;
          return;
        }
        settle(sessionID, session, event.created, "superseded");
        if (draining) return;
        session.assistant = {
          id: event.data.assistantMessageID,
          agent: event.data.agent,
          model: { ...event.data.model },
          startedAt: event.data.started,
          input: structuredClone(session.history),
          systemPrompt: session.instructions.length
            ? session.instructions.join("\n\n")
            : undefined,
          blocks: [],
          parent: session.lastGeneration ?? session.spawningGeneration,
          subagent: session.parentSession !== undefined,
          conversationId: session.conversationId,
          title:
            session.conversationId === sessionID ? session.title : undefined,
          lineage: session.parentSession
            ? {
                "opencode.parent_session_id": session.parentSession,
                ...(session.conversationId !== sessionID && {
                  "opencode.child_session_id": sessionID,
                }),
              }
            : undefined,
        };
        return;
      }
      case "session.execution.succeeded":
      case "session.execution.failed":
      case "session.execution.interrupted":
        settle(
          sessionID,
          session,
          event.created,
          event.type.slice("session.".length),
        );
        return;
      case "session.deleted":
        settle(sessionID, session, event.created, "deleted");
        sessions.delete(sessionID);
        locations.delete(sessionID);
        return;
    }
    if (!("assistantMessageID" in event.data)) return;
    const assistant = session.assistant;
    if (!assistant || assistant.id !== event.data.assistantMessageID) return;
    switch (event.type) {
      case "session.step.streamed":
        assistant.streamedAt = event.created;
        return;
      case "session.step.ended":
      case "session.step.failed":
        settle(
          sessionID,
          session,
          event.created,
          event.type.slice("session.".length),
          event.data,
        );
        return;
      case "session.text.started":
      case "session.text.delta":
      case "session.text.ended":
      case "session.reasoning.started":
      case "session.reasoning.delta":
      case "session.reasoning.ended": {
        const type = event.type.startsWith("session.text.")
          ? "text"
          : "reasoning";
        let block = assistant.blocks.find(
          (entry) =>
            entry.type === type && entry.ordinal === event.data.ordinal,
        );
        if (!block) {
          block = { type, ordinal: event.data.ordinal, text: "", ended: false };
          assistant.blocks.push(block);
        }
        if (block.type === "tool") return;
        if ("text" in event.data) {
          block.text = event.data.text;
          block.ended = true;
        } else if ("delta" in event.data && !block.ended)
          block.text += event.data.delta;
        if (
          ("text" in event.data || "delta" in event.data) &&
          assistant.firstAt === undefined
        )
          assistant.firstAt = event.created;
        return;
      }
      case "session.tool.input.started":
        if (!tool(assistant, event.data.id))
          assistant.blocks.push({
            type: "tool",
            id: event.data.id,
            name: event.data.name,
          });
        if (assistant.firstAt === undefined) assistant.firstAt = event.created;
        return;
      case "session.tool.input.ended": {
        const call = tool(assistant, event.data.id);
        if (call) call.rawInput = event.data.text;
        return;
      }
      case "session.tool.called": {
        const call = tool(assistant, event.data.id);
        if (call && call.calledAt === undefined) {
          call.input = structuredClone(event.data.input);
          call.calledAt = event.created;
        }
        return;
      }
      case "session.tool.success":
      case "session.tool.failed": {
        const call = tool(assistant, event.data.id);
        if (!call || call.completedAt !== undefined) return;
        call.completedAt = event.created;
        call.output = (event.data.content ?? [])
          .flatMap((content) => (content.type === "text" ? [content.text] : []))
          .join("\n");
        if (event.type === "session.tool.failed") {
          call.isError = true;
          call.errorType = event.data.error.type;
          call.output ||= event.data.error.message;
        } else if (
          call.name === "invalid" ||
          (call.name === "bash" &&
            typeof event.data.metadata?.exit === "number" &&
            event.data.metadata.exit !== 0)
        ) {
          call.isError = true;
          call.errorType = "tool.exit";
        }
        return;
      }
    }
  }
  async function drain(): Promise<void> {
    while (tasks.size) await Promise.all([...tasks]);
  }
  function shutdown(): Promise<void> {
    if (shutdownTask) return shutdownTask;
    closed = true;
    const at = Date.now();
    for (const [id, session] of sessions) settle(id, session, at, "shutdown");
    sessions.clear();
    locations.clear();
    seen.clear();
    shutdownTask = drain();
    return shutdownTask;
  }
  return { handleEvent, shutdown, drain };
}
