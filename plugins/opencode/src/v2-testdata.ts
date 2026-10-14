import type { Plugin } from "@opencode/plugin";

// Promise subscription ENCODED payloads, checked against the installed v2 SDK.
// Source: opencode 13afea6a1a, packages/client/src/promise/generated/types.ts
// and packages/core/src/session/{message-updater,runner/step}.ts. No v1 properties/info,
// Effect Schema brands, REST snapshots, or reconstructed provider requests.
export type NativeFixtureEvent =
  ReturnType<Plugin.Context["event"]["subscribe"]> extends AsyncIterable<
    infer Event
  >
    ? Event
    : never;
type EventOf<T extends NativeFixtureEvent["type"]> = Extract<
  NativeFixtureEvent,
  { type: T }
>;

export const nativeFixture = {
  sessionID: "native-session-1",
  assistantMessageID: "native-assistant-1",
  location: { directory: "/repo/oc-native", workspaceID: "native-workspace-1" },
  model: { providerID: "anthropic", id: "claude-sonnet-4-opencode" },
  title: "List the Go files in the repo",
  started: 1_700_000_001_000,
  streamed: 1_700_000_002_000,
  terminal: 1_700_000_002_500,
  // Deliberately synthetic secrets, never real credentials.
  secret: "glc_abcdefghijklmnopqrstuvwxyz1234",
  envSecret: "hunter2secret123",
  toolID: "native-call-1",
} as const;

export function nativeTranscript(): NativeFixtureEvent[] {
  const {
    sessionID,
    assistantMessageID,
    location,
    model,
    title,
    secret,
    envSecret,
    toolID,
  } = nativeFixture;
  const step = { sessionID, assistantMessageID };
  let seq = 0;
  function event<T extends NativeFixtureEvent["type"]>(
    type: T,
    offset: number,
    data: EventOf<T>["data"],
    version: number = 1,
  ): EventOf<T> {
    seq += 1;
    return {
      id: `native-event-${seq}`,
      created: 1_700_000_000_000 + offset,
      type,
      durable: { aggregateID: sessionID, seq, version },
      location,
      data,
    } as EventOf<T>;
  }
  return [
    event("session.created", 0, {
      sessionID,
      projectID: "native-project-1",
      location,
      slug: "native-golden",
      title,
      agent: "build",
      model,
      version: "2.0.22",
    }),
    event("session.compaction.ended", 50, {
      sessionID,
      reason: "auto",
      model,
      text: `Earlier summary; token: ${secret}`,
      recent: `Recent context: inspect Go files; DATABASE_PASSWORD=${envSecret}`,
      cost: 0.001,
      tokens: {
        input: 100,
        output: 20,
        reasoning: 0,
        cache: { read: 0, write: 0 },
      },
    }),
    event(
      "session.instructions.updated",
      60,
      {
        sessionID,
        delta: { instructions: "changed" },
        text: `Prefer concise answers; token: ${secret}`,
      },
      2,
    ),
    event(
      "session.instructions.updated",
      75,
      {
        sessionID,
        delta: { instructions: "changed-again" },
        text: `Use the observed repository context; DATABASE_PASSWORD=${envSecret}`,
      },
      2,
    ),
    event("session.inbox.enqueued", 100, {
      sessionID,
      inboxID: "native-user-1",
      item: {
        type: "user",
        delivery: "queue",
        payload: {
          text: `list go files; token: ${secret}; DATABASE_PASSWORD=${envSecret}`,
        },
      },
    }),
    event("session.inbox.delivered", 200, {
      sessionID,
      inboxID: "native-user-1",
    }),
    event("session.step.started", 1000, {
      ...step,
      agent: "build",
      model,
      started: nativeFixture.started,
    }),
    event("session.reasoning.started", 1100, { ...step, ordinal: 0 }),
    event("session.reasoning.ended", 1150, {
      ...step,
      ordinal: 0,
      text: `I should inspect the repo; token: ${secret}`,
      state: { opaque: "DO_NOT_EXPORT_PROVIDER_STATE" },
    }),
    event("session.text.started", 1200, { ...step, ordinal: 1 }),
    // Ended is authoritative, not delta + ended concatenated together.
    event("session.text.delta", 1300, {
      ...step,
      ordinal: 1,
      delta: "I will list",
    }),
    event("session.text.ended", 1400, {
      ...step,
      ordinal: 1,
      text: `I will list the Go files; token: ${secret}`,
    }),
    event("session.tool.input.started", 1600, {
      ...step,
      id: toolID,
      name: "bash",
    }),
    event("session.tool.input.ended", 1700, {
      ...step,
      id: toolID,
      text: JSON.stringify({ command: `ls *.go; echo ${secret}` }),
    }),
    event("session.tool.called", 1800, {
      ...step,
      id: toolID,
      input: { command: `ls *.go; echo ${secret}` },
      executed: false,
    }),
    // Native streaming can finish before tools settle. Completion is streamed,
    // not the later terminal timestamp; the tool span still includes its runtime.
    event("session.step.streamed", 2000, step),
    event(
      "session.tool.success",
      2200,
      {
        ...step,
        id: toolID,
        executed: false,
        content: [
          {
            type: "text",
            text: `main.go\nutil.go\ntoken: ${secret}\nDATABASE_PASSWORD=${envSecret}`,
          },
        ],
      },
      2,
    ),
    event("session.step.ended", 2500, {
      ...step,
      finish: "stop",
      rawFinish: "end_turn",
      cost: 0.005,
      tokens: {
        input: 200,
        output: 50,
        reasoning: 10,
        cache: { read: 30, write: 20 },
      },
      providerState: { opaque: "DO_NOT_EXPORT_PROVIDER_STATE" },
    }),
    event("session.execution.succeeded", 2600, { sessionID }),
  ];
}
