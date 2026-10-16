import {
  Agento11yClient,
  type ContentCaptureMode,
  type Generation,
  type GenerationRecorder,
  type GenerationResult,
  type GenerationStart,
} from "@grafana/agento11y";
import {
  AggregationTemporality,
  DataPointType,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BasicTracerProvider,
  InMemorySpanExporter,
  SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import { resolveGitBranch } from "./git.js";
import {
  stableOpencodeCompactionGenerationId,
  stableOpencodeGenerationId,
} from "./lineage.js";
import { createRedactor } from "./redaction.js";
import { createNativeCapture } from "./v2-events.js";
import type {
  NativeEvent,
  NativeEventOf,
  NativeLocation,
} from "./v2-mappers.js";
import { nativeFixture } from "./v2-testdata.js";

vi.mock("./git.js", () => ({
  resolveGitBranch: vi.fn((cwd: string) =>
    cwd === "/repo" ? "feature/native" : undefined,
  ),
}));

const home: NativeLocation = { directory: "/repo", workspaceID: "w1" };
const away: NativeLocation = { directory: "/other", workspaceID: "w1" };
let serial = 0;
function event<T extends NativeEvent["type"]>(
  type: T,
  data: NativeEventOf<T>["data"],
  location: NativeLocation | undefined = home,
  created = 100,
): NativeEventOf<T> {
  return {
    type,
    data,
    location,
    created,
    id: `event-${++serial}`,
    ...(!type.endsWith(".delta") &&
      !type.endsWith(".progress") &&
      type !== "location.shutdown" &&
      type !== "session.usage.updated" && {
        durable: {
          aggregateID: "sessionID" in data ? data.sessionID : "",
          seq: serial,
          version: [
            "session.deleted",
            "session.instructions.updated",
            "session.tool.success",
            "session.tool.failed",
          ].includes(type)
            ? 2
            : 1,
        },
      }),
  } as NativeEventOf<T>;
}
function start(
  sessionID = "s",
  assistantMessageID = "a",
  location: NativeLocation | undefined = home,
  agent = "build",
) {
  return event(
    "session.step.started",
    {
      sessionID,
      assistantMessageID,
      agent,
      model: { id: "model", providerID: "provider" },
      started: 50,
    },
    location,
  );
}
function end(
  sessionID = "s",
  assistantMessageID = "a",
  location: NativeLocation | undefined = home,
) {
  return event(
    "session.step.ended",
    {
      sessionID,
      assistantMessageID,
      finish: "stop",
      cost: 0.5,
      tokens: {
        input: 10,
        output: 20,
        reasoning: 3,
        cache: { read: 4, write: 5 },
      },
    },
    location,
    300,
  );
}
function enqueue(
  text: string,
  inboxID = "i",
  sessionID = "s",
  location: NativeLocation | undefined = home,
) {
  return event(
    "session.inbox.enqueued",
    {
      sessionID,
      inboxID,
      item: { type: "user", delivery: "queue", payload: { text } },
    },
    location,
  );
}
function created(sessionID: string, parentID?: string, location = home) {
  return event(
    "session.created",
    {
      sessionID,
      parentID,
      projectID: "p",
      location,
      slug: sessionID,
      version: "2.0.22",
      title: "Title",
    },
    location,
  );
}
function move(
  sessionID = "s",
  location = away,
  previous: NativeLocation | undefined = home,
) {
  return event(
    "session.moved",
    { sessionID, location, projectID: "p" },
    previous,
    200,
  );
}
const compactionTokens = {
  input: 100,
  output: 20,
  reasoning: 3,
  cache: { read: 4, write: 5 },
};
function compactionStart(
  sessionID = "s",
  reason: "auto" | "manual" = "auto",
  location = home,
) {
  return event(
    "session.compaction.started",
    {
      sessionID,
      reason,
      recent: "not generated output",
      inputID: "compact-inbox",
    },
    location,
    120,
  );
}
function compactionEnd(
  sessionID = "s",
  overrides: Partial<NativeEventOf<"session.compaction.ended">["data"]> = {},
  location = home,
) {
  return event(
    "session.compaction.ended",
    {
      sessionID,
      reason: "auto",
      model: { id: "compaction-model", providerID: "compaction-provider" },
      text: "checkpoint",
      recent: "retained recent context",
      tokens: compactionTokens,
      cost: 0.001,
      ...overrides,
    },
    location,
    220,
  );
}
function compactionFailure(
  sessionID = "s",
  overrides: Partial<NativeEventOf<"session.compaction.failed">["data"]> = {},
) {
  return event(
    "session.compaction.failed",
    {
      sessionID,
      reason: "manual",
      inputID: "compact-inbox",
      error: {
        type: "compaction.failed",
        message: `failed ${nativeFixture.secret}; DATABASE_PASSWORD=${nativeFixture.envSecret}`,
      },
      ...overrides,
    },
    home,
    220,
  );
}
const config: Agento11yOpencodeConfig = {
  endpoint: "http://127.0.0.1:1",
  auth: { mode: "none" },
  agentName: "opencode",
  agentVersion: "2.0.22",
  contentCapture: "full",
  redactInputMessages: true,
  debug: false,
};
interface Recorded {
  seed: GenerationStart;
  result?: GenerationResult;
  error?: unknown;
  firstAt?: Date;
}
function harness(
  location = home,
  overrides: Partial<Agento11yOpencodeConfig> = {},
) {
  const generations: Recorded[] = [];
  const toolResults: unknown[] = [];
  const debugLog = vi.fn();
  const recordGeneration = async (
    seed: GenerationStart,
    run: (recorder: GenerationRecorder) => Promise<void>,
  ) => {
    const entry: Recorded = { seed };
    generations.push(entry);
    await run({
      setResult: (result) => {
        entry.result = result;
      },
      setCallError: (error) => {
        entry.error = error;
      },
      setFirstTokenAt: (firstAt) => {
        entry.firstAt = firstAt;
      },
      setCacheDiagnostics: vi.fn(),
      end: vi.fn(),
      getError: () => undefined,
    });
  };
  const exportGeneration = vi.fn(recordGeneration);
  const exportCompaction = vi.fn(recordGeneration);
  const evaluateHook = vi.fn(() => {
    throw new Error("capture must not evaluate guards");
  });
  const client = {
    startStreamingGeneration: exportGeneration,
    startGeneration: exportCompaction,
    evaluateHook,
    startToolExecution: vi.fn(() => ({
      setResult: (result: unknown) => toolResults.push(result),
      setCallError: vi.fn(),
      end: vi.fn(),
    })),
  } as unknown as Agento11yClient;
  const capture = createNativeCapture({
    client,
    config: { ...config, ...overrides },
    redactor: createRedactor(),
    ...location,
    hostVersion: "2.0.29",
    debugLog,
  });
  return {
    capture,
    generations,
    client,
    toolResults,
    debugLog,
    exportGeneration,
    exportCompaction,
    evaluateHook,
  };
}
function replay(
  capture: ReturnType<typeof createNativeCapture>,
  events: NativeEvent[],
) {
  events.forEach(capture.handleEvent);
  return capture.drain();
}

const compactionReloads = ["ended", "failed"].flatMap((kind) =>
  ["old first", "replacement first", "replacement only"].map((delivery) => ({
    kind,
    delivery,
  })),
);
const lifecycleTerminals: Array<{
  name: string;
  make: () => NativeEvent;
  usage: boolean;
}> = [
  { name: "step ended", make: () => end(), usage: true },
  {
    name: "step failed",
    make: () =>
      event("session.step.failed", {
        sessionID: "s",
        assistantMessageID: "a",
        error: { type: "provider.unknown", message: "secret" },
      }),
    usage: false,
  },
  {
    name: "execution completed",
    make: () => event("session.execution.succeeded", { sessionID: "s" }),
    usage: false,
  },
  {
    name: "execution failed",
    make: () =>
      event("session.execution.failed", {
        sessionID: "s",
        error: { type: "unknown", message: "secret" },
      }),
    usage: false,
  },
  ...(["user", "shutdown", "superseded", "inactivity"] as const).map(
    (reason) => ({
      name: `execution interrupted ${reason}`,
      make: () =>
        event("session.execution.interrupted", { sessionID: "s", reason }),
      usage: false,
    }),
  ),
  {
    name: "deletion",
    make: () => event("session.deleted", { sessionID: "s" }),
    usage: false,
  },
];

describe("native lifecycle fold", () => {
  it.each(lifecycleTerminals)("settles at most once through $name", async ({
    make,
    usage,
  }) => {
    const { capture, generations } = harness();
    const terminal = make();
    await replay(capture, [start(), terminal, terminal, end(), start(), end()]);
    await capture.shutdown();
    expect(generations).toHaveLength(1);
    expect(generations[0]?.seed.id).toBe(stableOpencodeGenerationId("s", "a"));
    expect(generations[0]?.result?.usage !== undefined).toBe(usage);
    expect(JSON.stringify(generations)).not.toContain("secret");
  });

  it.each([
    "old terminal before new start",
    "old terminal while new active",
    "old terminal after new terminal",
  ])("handles %s without cross-assistant settlement", async (order) => {
    const { capture, generations } = harness();
    const a = end();
    const b = start("s", "b");
    const bEnd = end("s", "b");
    const sequence =
      order === "old terminal before new start"
        ? [a, b, bEnd]
        : order === "old terminal while new active"
          ? [b, a, bEnd]
          : [b, bEnd, a];
    await replay(capture, [start(), ...sequence]);
    expect(generations.map((entry) => entry.seed.id)).toEqual([
      stableOpencodeGenerationId("s", "a"),
      stableOpencodeGenerationId("s", "b"),
    ]);
    expect(generations[0]?.result?.usage !== undefined).toBe(
      order === "old terminal before new start",
    );
    expect(generations[1]?.result?.usage?.inputTokens).toBe(19);
    await capture.shutdown();
  });

  it.each([
    { cost: undefined, tokens: undefined },
    { cost: 0, tokens: undefined },
    { cost: undefined, tokens: end().data.tokens },
    { cost: 0.5, tokens: end().data.tokens },
  ])("keeps independently optional failed usage/cost: %j", async ({
    cost,
    tokens,
  }) => {
    const { capture, generations } = harness();
    await replay(capture, [
      start(),
      event("session.step.failed", {
        sessionID: "s",
        assistantMessageID: "a",
        cost,
        tokens,
        error: { type: "provider.unknown", message: "not exported" },
      }),
    ]);
    expect(generations[0]?.result?.usage?.inputTokens).toBe(
      tokens ? 19 : undefined,
    );
    expect(generations[0]?.result?.metadata?.cost_usd).toBe(cost);
    expect(generations[0]?.result?.stopReason).toBe("error");
    await capture.shutdown();
  });

  it("shutdown is idempotent, stops intake and settles pending steps", async () => {
    const { capture, generations } = harness();
    capture.handleEvent(start());
    const first = capture.shutdown();
    expect(capture.shutdown()).toBe(first);
    capture.handleEvent(start("s", "b"));
    await first;
    expect(generations).toHaveLength(1);
    expect(generations[0]?.result?.metadata).toMatchObject({
      "opencode.settlement": "shutdown",
      "opencode.completion_time_approximate": true,
    });
    expect(generations[0]?.result?.usage).toBeUndefined();
  });

  it("supersession after successful overflow compaction settles abandoned identity without invented usage", async () => {
    const { capture, generations, exportGeneration, exportCompaction } =
      harness();
    const begun = compactionStart();
    const terminal = compactionEnd();
    await replay(capture, [
      start(),
      begun,
      terminal,
      start("s", "b"),
      end("s", "b"),
      end(),
    ]);
    await capture.shutdown();
    expect(generations).toHaveLength(3);
    expect(exportGeneration).toHaveBeenCalledTimes(2);
    expect(exportCompaction).toHaveBeenCalledTimes(1);
    const abandoned = generations.find(
      (entry) => entry.seed.id === stableOpencodeGenerationId("s", "a"),
    );
    const retried = generations.find(
      (entry) => entry.seed.id === stableOpencodeGenerationId("s", "b"),
    );
    const compacted = generations.find(
      (entry) => entry.seed.tags?.["opencode.call_kind"] === "compaction",
    );
    expect(abandoned?.result?.usage).toBeUndefined();
    expect(abandoned?.result?.metadata?.["opencode.settlement"]).toBe(
      "superseded",
    );
    expect(JSON.stringify(retried?.result?.input)).toContain("checkpoint");
    expect(retried?.result?.usage?.inputTokens).toBe(19);
    expect(retried?.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("s", "a"),
    ]);
    expect(compacted?.seed.id).toBe(
      stableOpencodeCompactionGenerationId("s", terminal.id),
    );
    expect(compacted?.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("s", "a"),
    ]);
    expect(compacted?.result?.usage).toMatchObject({
      inputTokens: 109,
      outputTokens: 23,
    });
    expect(compacted?.result?.metadata?.cost_usd).toBe(0.001);
  });

  it("excludes queued/cancelled inbox and freezes the FIRST observed prefix across retries", async () => {
    const { capture, generations } = harness();
    const prompt = enqueue("delivered");
    capture.handleEvent(prompt);
    // A host/plugin mutating its event after dispatch cannot mutate retained capture.
    if (prompt.data.item.type === "user")
      prompt.data.item.payload.text = "mutated";
    await replay(capture, [
      event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
      enqueue("never delivered", "queued"),
      enqueue("cancelled", "cancelled"),
      event("session.inbox.cancelled", {
        sessionID: "s",
        inboxID: "cancelled",
      }),
      start(),
      event("session.retry.scheduled", {
        sessionID: "s",
        assistantMessageID: "a",
        attempt: 1,
        at: 500,
        error: { type: "retry", message: "transient" },
      }),
      event("session.instructions.updated", {
        sessionID: "s",
        delta: { instructions: "changed" },
        text: "retry-time instructions",
      }),
      event("session.inbox.delivered", { sessionID: "s", inboxID: "queued" }),
      start(),
      end(),
      start("s", "b"),
      end("s", "b"),
    ]);
    const firstInput = JSON.stringify(generations[0]?.result?.input);
    expect(firstInput).toContain("delivered");
    expect(firstInput).not.toMatch(
      /mutated|never delivered|cancelled|retry-time/,
    );
    expect(generations[0]?.seed.systemPrompt).toBeUndefined();
    expect(generations[1]?.seed.systemPrompt).toBe("retry-time instructions");
    const nextInput = JSON.stringify(generations[1]?.result?.input);
    expect(nextInput).not.toContain("retry-time instructions");
    expect(nextInput).toContain("never delivered");
    expect(generations[0]?.seed.startedAt).toEqual(new Date(50));
  });

  it.each([
    "text",
    "reasoning",
  ] as const)("%s ended replaces accumulated deltas, ignores late deltas", async (kind) => {
    const { capture, generations } = harness();
    const data = { sessionID: "s", assistantMessageID: "a", ordinal: 0 };
    await replay(capture, [
      start(),
      event(`session.${kind}.started`, data),
      event(`session.${kind}.delta`, { ...data, delta: "partial" }, home, 110),
      event(`session.${kind}.ended`, { ...data, text: "canonical" }, home, 120),
      event(`session.${kind}.delta`, { ...data, delta: "late" }),
      event(
        "session.step.streamed",
        { sessionID: "s", assistantMessageID: "a" },
        home,
        150,
      ),
      end(),
    ]);
    expect(JSON.stringify(generations[0]?.result?.output)).toContain(
      "canonical",
    );
    expect(JSON.stringify(generations[0]?.result?.output)).not.toMatch(
      /partial|late/,
    );
    expect(generations[0]?.firstAt).toEqual(new Date(110));
    expect(generations[0]?.result?.completedAt).toEqual(new Date(150));
  });

  it.each([
    away,
    { directory: "/repo", workspaceID: "w2" },
    { directory: "/repo" },
  ])("checks full location before retaining any content (%j)", async (foreign) => {
    const { capture, generations } = harness();
    await replay(capture, [
      enqueue("foreign", "i", "s", foreign),
      start("s", "a", foreign),
      end("s", "a", foreign),
      start("s", "b"),
      end("s", "b"),
    ]);
    expect(generations).toHaveLength(1);
    expect(JSON.stringify(generations)).not.toContain("foreign");
  });

  it("uses only previously observed ownership for unlocated events, never later lookup", async () => {
    const { capture, generations } = harness();
    const old = enqueue("unlocated old");
    delete old.location;
    const locatedStart = start();
    const noLocationEnd = end();
    delete noLocationEnd.location;
    await replay(capture, [old, locatedStart, noLocationEnd]);
    expect(generations).toHaveLength(1);
    expect(JSON.stringify(generations)).not.toContain("unlocated old");
    const orphanStart = start("orphan");
    delete orphanStart.location;
    const orphanEnd = end("orphan");
    delete orphanEnd.location;
    await replay(capture, [orphanStart, created("orphan"), orphanEnd]);
    expect(generations).toHaveLength(1);
  });

  it.each([
    true,
    false,
  ])("moves notify old and new owners; envelope present=%s", async (envelope) => {
    const source = harness();
    const destination = harness(away);
    const both = (e: NativeEvent) => {
      source.capture.handleEvent(e);
      destination.capture.handleEvent(e);
    };
    both(enqueue("old owner input"));
    both(event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }));
    both(start());
    const old = enqueue("stale old");
    delete old.location;
    const oldDelivery = event("session.inbox.delivered", {
      sessionID: "s",
      inboxID: "i",
    });
    delete oldDelivery.location;
    const moved = move();
    if (!envelope) delete moved.location;
    both(moved);
    both(moved); // duplicate transition must not settle destination later
    both(old);
    both(oldDelivery);
    both(start("s", "b", away));
    both(moved);
    both(end("s", "b", away));
    await Promise.all([
      source.capture.shutdown(),
      destination.capture.shutdown(),
    ]);
    expect(source.generations).toHaveLength(1);
    expect(destination.generations).toHaveLength(1);
    expect(JSON.stringify(destination.generations)).not.toMatch(
      /old owner|stale old/,
    );
    expect(destination.generations[0]?.seed.conversationId).toBe("s");
  });

  it("moving away and back cannot resurrect late old-owner content", async () => {
    const { capture, generations } = harness();
    await replay(capture, [
      start(),
      move(),
      enqueue("stale"),
      move("s", home, away),
      start("s", "b"),
      end("s", "b"),
    ]);
    expect(generations).toHaveLength(2);
    expect(JSON.stringify(generations[1]?.result?.input)).not.toContain(
      "stale",
    );
  });

  it.each([
    {
      overrides: {},
      parent: undefined,
      agent: "build",
      name: "opencode:build",
      version: "2.0.22",
      tags: { cwd: "/repo", "git.branch": "feature/native" },
    },
    {
      overrides: { agentName: "acme", agentVersion: undefined },
      parent: "root",
      agent: "plan",
      name: "acme:plan",
      version: "2.0.29",
      tags: { cwd: "/repo", "git.branch": "feature/native", subagent: "true" },
    },
    {
      overrides: { agentVersion: "" },
      parent: undefined,
      agent: "explore",
      name: "opencode:explore",
      version: "2.0.29",
      tags: { cwd: "/repo", "git.branch": "feature/native" },
    },
  ])("exports v1 identity $name@$version with built-in tags", async ({
    overrides,
    parent,
    agent,
    name,
    version,
    tags,
  }) => {
    const { capture, generations, client } = harness(home, overrides);
    const data = { sessionID: "s", assistantMessageID: "a", id: "c" };
    await replay(capture, [
      created("s", parent),
      start("s", "a", home, agent),
      event("session.tool.input.started", { ...data, name: "read" }),
      event("session.tool.called", { ...data, input: {}, executed: false }),
      event("session.tool.success", {
        ...data,
        executed: false,
        content: [{ type: "text", text: "ok" }],
      }),
      end(),
    ]);
    expect(generations[0]?.seed).toMatchObject({
      agentName: name,
      agentVersion: version,
      effectiveVersion: version,
      tags,
    });
    expect(client.startToolExecution).toHaveBeenCalledWith(
      expect.objectContaining({ agentName: name, agentVersion: version }),
    );
    expect(resolveGitBranch).toHaveBeenCalledWith("/repo");
    await capture.shutdown();
  });

  it.each([
    true,
    false,
  ])("lineage uses observed spawning generation only; parent known=%s", async (known) => {
    const { capture, generations } = harness();
    if (known) capture.handleEvent(start("parent", "spawn"));
    await replay(capture, [
      created("child", "parent"),
      start("child", "reply"),
      end("child", "reply"),
    ]);
    const child = generations.find(
      (entry) => entry.seed.id === stableOpencodeGenerationId("child", "reply"),
    );
    expect(child?.seed.conversationId).toBe(known ? "parent" : "child");
    expect(child?.seed.parentGenerationIds).toEqual(
      known ? [stableOpencodeGenerationId("parent", "spawn")] : undefined,
    );
    expect(child?.seed.metadata).toMatchObject({
      "opencode.parent_session_id": "parent",
    });
    if (known) expect(child?.seed.conversationTitle).toBeUndefined();
    await capture.shutdown();
  });

  it("nested lineage stays in spawning conversation, while intra-session chaining takes precedence", async () => {
    const { capture, generations } = harness();
    await replay(capture, [
      start("root", "spawn"),
      created("child", "root"),
      start("child", "first"),
      created("grandchild", "child"),
      start("grandchild", "reply"),
      end("grandchild", "reply"),
      end("child", "first"),
      start("child", "second"),
      end("child", "second"),
    ]);
    const grandchild = generations.find(
      (g) => g.seed.id === stableOpencodeGenerationId("grandchild", "reply"),
    );
    expect(grandchild?.seed.conversationId).toBe("root");
    expect(grandchild?.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("child", "first"),
    ]);
    expect(generations.at(-1)?.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("child", "first"),
    ]);
    await capture.shutdown();
  });

  it("independent setups and concurrent session identities never share state", async () => {
    const a = harness();
    const b = harness();
    a.capture.handleEvent(enqueue("only a"));
    a.capture.handleEvent(
      event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
    );
    await Promise.all([
      replay(a.capture, [start(), start("other"), end(), end("other")]),
      replay(b.capture, [start(), end()]),
    ]);
    expect(a.generations).toHaveLength(2);
    expect(b.generations).toHaveLength(1);
    expect(JSON.stringify(b.generations)).not.toContain("only a");
  });

  it("telemetry tasks are independent; rejection is content-free and does not delay later exports", async () => {
    const { capture, generations, exportGeneration, debugLog } = harness();
    let release!: () => void;
    const stuck = new Promise<void>((resolve) => {
      release = resolve;
    });
    exportGeneration.mockImplementationOnce(async () => {
      await stuck;
      throw new Error("secret provider content");
    });
    capture.handleEvent(start());
    capture.handleEvent(end());
    capture.handleEvent(start("s", "b"));
    capture.handleEvent(end("s", "b"));
    await Promise.resolve();
    await Promise.resolve();
    expect(generations).toHaveLength(1);
    release();
    await capture.shutdown();
    expect(debugLog).toHaveBeenCalledWith("native telemetry task failed");
    expect(JSON.stringify(debugLog.mock.calls)).not.toContain("secret");
  });

  // OpenCode detaches routing on location.shutdown, but a running step keeps
  // its graph and still emits text and its terminal before setup cleanup.
  it.each([
    { shutdown: home, terminal: true, settlement: "step.ended" },
    { shutdown: home, terminal: false, settlement: "shutdown" },
    { shutdown: away, terminal: true, settlement: "step.ended" },
  ])("drains a running step after shutdown at $shutdown.directory (terminal=$terminal); only the owner of new work captures it", async ({
    shutdown,
    terminal,
    settlement,
  }) => {
    const { capture, generations } = harness();
    const text = (delta: string) =>
      event("session.text.delta", {
        sessionID: "s",
        assistantMessageID: "a",
        ordinal: 0,
        delta,
      });
    await replay(capture, [
      start(),
      text("before "),
      event("location.shutdown", {}, shutdown),
      text("after"),
      ...(terminal ? [end(), start("s", "b"), end("s", "b")] : []),
      created("fresh"),
      start("fresh", "c"),
      end("fresh", "c"),
    ]);
    await capture.shutdown();
    expect(generations.map((entry) => entry.seed.id)).toEqual([
      stableOpencodeGenerationId("s", "a"),
      ...(shutdown === home
        ? []
        : [
            stableOpencodeGenerationId("s", "b"),
            stableOpencodeGenerationId("fresh", "c"),
          ]),
    ]);
    const drained = generations[0];
    expect(drained?.result?.metadata?.["opencode.settlement"]).toBe(settlement);
    expect(drained?.result?.usage?.inputTokens).toBe(terminal ? 19 : undefined);
    expect((drained?.error as Error | undefined)?.message).toBe(
      terminal ? undefined : "opencode.shutdown",
    );
    expect(drained?.result?.output).toEqual([
      { role: "assistant", parts: [{ type: "text", text: "before after" }] },
    ]);
  });
});

describe("native compaction lifecycle", () => {
  const compactions = (generations: Recorded[]) =>
    generations.filter(
      (entry) => entry.seed.tags?.["opencode.call_kind"] === "compaction",
    );
  const findCompaction = (generations: Recorded[], terminal: NativeEvent) =>
    generations.find(
      (entry) =>
        entry.seed.id ===
        stableOpencodeCompactionGenerationId(
          "sessionID" in terminal.data ? terminal.data.sessionID : "",
          terminal.id,
        ),
    );

  it.each([
    "auto",
    "manual",
  ] as const)("exports a started/%s ended compaction with independent usage and timing", async (reason) => {
    const { capture, generations, exportCompaction, exportGeneration, client } =
      harness();
    const begun = compactionStart("s", reason);
    const terminal = compactionEnd("s", { reason });
    await replay(capture, [
      enqueue("observed prefix"),
      event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
      start(),
      begun,
      event("session.synthetic", { sessionID: "s", text: "after snapshot" }),
      event("session.compaction.delta", {
        sessionID: "s",
        text: "discarded summary attempt",
      }),
      terminal,
    ]);
    expect(exportCompaction).toHaveBeenCalledTimes(1);
    expect(exportGeneration).not.toHaveBeenCalled();
    const compacted = findCompaction(generations, terminal);
    expect(compacted?.seed).toMatchObject({
      mode: "SYNC",
      operationName: "generateText",
      conversationId: "s",
      parentGenerationIds: [stableOpencodeGenerationId("s", "a")],
      agentName: "opencode",
      agentVersion: "2.0.22",
      model: { provider: "compaction-provider", name: "compaction-model" },
      startedAt: new Date(begun.created),
      tags: {
        "opencode.call_kind": "compaction",
        cwd: "/repo",
        "git.branch": "feature/native",
      },
    });
    expect(compacted?.result).toMatchObject({
      input: [
        { role: "user", parts: [{ type: "text", text: "observed prefix" }] },
      ],
      output: [
        { role: "assistant", parts: [{ type: "text", text: "checkpoint" }] },
      ],
      usage: {
        inputTokens: 109,
        outputTokens: 23,
        totalTokens: 132,
        cacheReadInputTokens: 4,
        cacheWriteInputTokens: 5,
        reasoningTokens: 3,
        inputSemantics: "inclusive",
      },
      completedAt: new Date(terminal.created),
      metadata: {
        cost_usd: 0.001,
        "opencode.usage_scope": "logical_compaction",
        "opencode.compaction.reason": reason,
        "opencode.settlement": "compaction.ended",
        "opencode.model_source": "event",
        "opencode.input_source": "first_observed_prefix",
        "opencode.start_time_approximate": false,
        "opencode.completion_time_approximate": true,
      },
    });
    expect(compacted?.firstAt).toBeUndefined();
    expect(compacted?.error).toBeUndefined();
    expect(client.startToolExecution).not.toHaveBeenCalled();
    expect(JSON.stringify(compacted)).not.toMatch(
      /after snapshot|discarded summary|retained recent/,
    );
    await capture.shutdown();
  });

  it.each([
    false,
    true,
  ])("end-only has no invented input, parent or start time (active primary=%s)", async (primary) => {
    const { capture, generations } = harness();
    const terminal = compactionEnd();
    await replay(capture, [
      enqueue("available but not observed at compaction start"),
      event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
      ...(primary ? [start()] : []),
      terminal,
    ]);
    const compacted = findCompaction(generations, terminal);
    expect(compacted?.seed.parentGenerationIds).toBeUndefined();
    expect(compacted?.seed.startedAt).toEqual(compacted?.result?.completedAt);
    expect(compacted?.seed.startedAt).toEqual(new Date(terminal.created));
    expect(compacted?.result?.input).toEqual([]);
    expect(compacted?.result?.metadata).toMatchObject({
      "opencode.input_source": "unavailable",
      "opencode.start_time_approximate": true,
    });
    await capture.shutdown();
  });

  it.each(
    [false, true].flatMap((started) =>
      [false, true].map((usage) => ({ started, usage })),
    ),
  )("exports failure without leaking its message (started=$started, usage=$usage)", async ({
    started,
    usage,
  }) => {
    const { capture, generations, exportCompaction } = harness();
    const begun = started ? compactionStart("s", "manual") : undefined;
    const terminal = compactionFailure(
      "s",
      usage ? { tokens: compactionTokens, cost: 0.001 } : {},
    );
    await replay(capture, [...(begun ? [begun] : []), terminal]);
    expect(exportCompaction).toHaveBeenCalledTimes(1);
    const compacted = findCompaction(generations, terminal);
    expect(compacted?.seed.model).toEqual({
      provider: "unknown",
      name: "unknown",
    });
    expect(compacted?.result?.metadata).toMatchObject({
      "opencode.model_source": "unavailable",
      "opencode.compaction.reason": "manual",
      "opencode.settlement": "compaction.failed",
      "opencode.start_time_approximate": !started,
    });
    expect(compacted?.result?.stopReason).toBe("error");
    expect(compacted?.result?.output).toEqual([]);
    expect((compacted?.error as Error)?.message).toBe("compaction.failed");
    if (usage) {
      expect(compacted?.result?.usage).toMatchObject({
        inputTokens: 109,
        outputTokens: 23,
      });
      expect(compacted?.result?.metadata?.cost_usd).toBe(0.001);
    } else {
      expect(compacted?.result).not.toHaveProperty("usage");
      expect(compacted?.result?.metadata).not.toHaveProperty("cost_usd");
    }
    expect(JSON.stringify(generations)).not.toContain(nativeFixture.secret);
    expect(JSON.stringify(generations)).not.toContain(nativeFixture.envSecret);
    await capture.shutdown();
  });

  it.each([
    "ended",
    "failed",
  ] as const)("deduplicates %s before and after drain, and after shutdown", async (kind) => {
    const { capture, generations, exportCompaction } = harness();
    capture.handleEvent(compactionStart());
    const terminal = kind === "ended" ? compactionEnd() : compactionFailure();
    capture.handleEvent(terminal);
    capture.handleEvent(terminal);
    await capture.drain();
    await replay(capture, [terminal, terminal]);
    await capture.shutdown();
    capture.handleEvent(terminal);
    expect(compactions(generations)).toHaveLength(1);
    expect(exportCompaction).toHaveBeenCalledTimes(1);
    expect(findCompaction(generations, terminal)).toBeDefined();
  });

  it.each(
    compactionReloads,
  )("exports one $kind compaction through overlapping instances ($delivery)", async ({
    kind,
    delivery,
  }) => {
    const old = harness();
    await replay(old.capture, [
      event("session.synthetic", { sessionID: "s", text: "old prefix" }),
      start(),
      compactionStart(),
      event("location.shutdown", {}),
    ]);
    const replacement = harness();
    const terminal =
      kind === "ended"
        ? compactionEnd()
        : compactionFailure("s", { tokens: compactionTokens, cost: 0.001 });
    try {
      const instances =
        delivery === "old first"
          ? [old, replacement]
          : delivery === "replacement first"
            ? [replacement, old]
            : [replacement];
      for (const { capture } of instances) await replay(capture, [terminal]);
      await old.capture.shutdown();
      await replay(replacement.capture, [terminal]);
      expect(compactions(old.generations)).toHaveLength(1);
      expect(replacement.exportCompaction).not.toHaveBeenCalled();
      const compacted = findCompaction(old.generations, terminal);
      expect(compacted?.seed.startedAt).toEqual(new Date(120));
      expect(compacted?.seed.parentGenerationIds).toEqual([
        stableOpencodeGenerationId("s", "a"),
      ]);
      expect(JSON.stringify(compacted?.result?.input)).toContain("old prefix");
      expect(compacted?.result?.usage?.inputTokens).toBe(109);
      expect(compacted?.result?.usage?.outputTokens).toBe(23);
      await replay(replacement.capture, [start("s", "b"), end("s", "b")]);
      const next = replacement.generations[0];
      expect(JSON.stringify(next?.result?.input).includes("checkpoint")).toBe(
        kind === "ended",
      );
      expect(next?.seed.parentGenerationIds).toBeUndefined();
    } finally {
      await Promise.all([
        old.capture.shutdown(),
        replacement.capture.shutdown(),
      ]);
    }
  });

  it.each(
    ["ended", "failed"].flatMap((kind) =>
      [
        "delayed start while old open",
        "delayed start after old closes",
        "next start before old terminal",
        "forwarded terminal before next start",
      ].map((delivery) => ({ kind, delivery })),
    ),
  )("keeps sequential $kind compactions with $delivery", async ({
    kind,
    delivery,
  }) => {
    const old = harness();
    const replacement = harness();
    const firstStart = compactionStart();
    const first = kind === "ended" ? compactionEnd() : compactionFailure();
    const nextStart = compactionStart();
    nextStart.created = 230;
    const next = kind === "ended" ? compactionEnd() : compactionFailure();
    next.created = 330;
    try {
      if (delivery.startsWith("delayed start")) {
        await replay(replacement.capture, [first]);
        await replay(old.capture, [
          firstStart,
          event("location.shutdown", {}),
          first,
        ]);
        if (delivery === "delayed start after old closes")
          await old.capture.shutdown();
      } else {
        await replay(old.capture, [firstStart, event("location.shutdown", {})]);
        if (delivery === "forwarded terminal before next start")
          await replay(replacement.capture, [first]);
      }
      await replay(replacement.capture, [
        event("session.synthetic", {
          sessionID: "s",
          text: "next compaction prefix",
        }),
        nextStart,
        next,
      ]);
      if (delivery === "next start before old terminal")
        await replay(old.capture, [next]);
      await replay(old.capture, [first]);
      const recorded = [...old.generations, ...replacement.generations];
      expect(compactions(recorded)).toHaveLength(2);
      expect(findCompaction(recorded, first)?.seed.startedAt).toEqual(
        new Date(
          delivery.startsWith("delayed start")
            ? first.created
            : firstStart.created,
        ),
      );
      const compacted = findCompaction(replacement.generations, next);
      expect(compacted?.seed.startedAt).toEqual(new Date(230));
      expect(compacted?.result?.metadata?.["opencode.input_source"]).toBe(
        "first_observed_prefix",
      );
      expect(JSON.stringify(compacted?.result?.input)).toContain(
        "next compaction prefix",
      );
    } finally {
      await Promise.all([
        old.capture.shutdown(),
        replacement.capture.shutdown(),
      ]);
    }
  });

  it.each([
    "no start",
    "shared start",
    "owner shutdown",
    "owner moved",
    "owner deleted",
  ])("coordinates overlapping captures with %s", async (transition) => {
    const old = harness();
    const replacement = harness();
    const started = compactionStart();
    try {
      if (transition !== "no start") old.capture.handleEvent(started);
      if (transition === "shared start")
        replacement.capture.handleEvent(started);
      if (transition === "owner shutdown") await old.capture.shutdown();
      if (transition === "owner moved") old.capture.handleEvent(move());
      if (transition === "owner deleted")
        old.capture.handleEvent(event("session.deleted", { sessionID: "s" }));
      const terminal = compactionEnd();
      for (const { capture } of [replacement, old])
        await replay(capture, [terminal]);
      const recorded = [...old.generations, ...replacement.generations];
      expect(compactions(recorded)).toHaveLength(1);
      expect(old.exportCompaction).toHaveBeenCalledTimes(
        transition === "shared start" ? 1 : 0,
      );
      expect(replacement.exportCompaction).toHaveBeenCalledTimes(
        transition === "shared start" ? 0 : 1,
      );
      expect(
        findCompaction(recorded, terminal)?.result?.metadata?.[
          "opencode.input_source"
        ],
      ).toBe(
        transition === "shared start" ? "first_observed_prefix" : "unavailable",
      );
      const begun = compactionStart();
      const next = compactionEnd();
      await replay(replacement.capture, [begun, next]);
      expect(
        findCompaction(replacement.generations, next)?.result?.metadata?.[
          "opencode.input_source"
        ],
      ).toBe("first_observed_prefix");
    } finally {
      await Promise.all([
        old.capture.shutdown(),
        replacement.capture.shutdown(),
      ]);
    }
  });

  it.each([
    away,
    { directory: home.directory, workspaceID: "other-workspace" },
  ])("keeps compaction reservations separate at %j", async (otherLocation) => {
    const old = harness();
    const other = harness(otherLocation);
    const terminal = compactionEnd();
    try {
      await replay(old.capture, [compactionStart()]);
      await replay(other.capture, [{ ...terminal, location: otherLocation }]);
      await replay(old.capture, [terminal]);
      expect(old.exportCompaction).toHaveBeenCalledTimes(1);
      expect(other.exportCompaction).toHaveBeenCalledTimes(1);
      expect(
        compactions(other.generations)[0]?.result?.metadata?.[
          "opencode.input_source"
        ],
      ).toBe("unavailable");
    } finally {
      await Promise.all([old.capture.shutdown(), other.capture.shutdown()]);
    }
  });

  it.each([
    "active",
    "last",
    "spawning",
    "none",
  ] as const)("freezes the %s parent and history at start without changing ordinary lineage", async (parentSource) => {
    const { capture, generations } = harness();
    if (parentSource === "spawning") {
      capture.handleEvent(start("root", "spawn"));
      capture.handleEvent(created("s", "root"));
    }
    if (parentSource === "active" || parentSource === "last")
      capture.handleEvent(start());
    if (parentSource === "last") capture.handleEvent(end());
    const prefix = event("session.synthetic", {
      sessionID: "s",
      text: "frozen history",
    });
    capture.handleEvent(prefix);
    capture.handleEvent(compactionStart());
    prefix.data.text = "mutated after dispatch";
    if (parentSource === "active") capture.handleEvent(end());
    capture.handleEvent(start("s", "b"));
    capture.handleEvent(end("s", "b"));
    const terminal = compactionEnd();
    await replay(capture, [terminal, start("s", "c"), end("s", "c")]);
    const compacted = findCompaction(generations, terminal);
    const expectedParent =
      parentSource === "spawning"
        ? stableOpencodeGenerationId("root", "spawn")
        : parentSource === "none"
          ? undefined
          : stableOpencodeGenerationId("s", "a");
    expect(compacted?.seed.parentGenerationIds).toEqual(
      expectedParent ? [expectedParent] : undefined,
    );
    expect(compacted?.seed.conversationId).toBe(
      parentSource === "spawning" ? "root" : "s",
    );
    expect(compacted?.result?.input).toEqual([
      { role: "user", parts: [{ type: "text", text: "frozen history" }] },
    ]);
    const next = generations.find(
      (entry) => entry.seed.id === stableOpencodeGenerationId("s", "c"),
    );
    expect(next?.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("s", "b"),
    ]);
    await capture.shutdown();
  });

  it("keeps sequential compactions independent across two interleaved sessions", async () => {
    const { capture, generations, exportCompaction } = harness();
    const firstStart = compactionStart();
    const otherStart = compactionStart("other");
    const other = compactionEnd("other", {
      text: "other checkpoint",
      cost: 0.002,
    });
    const first = compactionEnd("s", { text: "first checkpoint" });
    const secondStart = compactionStart();
    const second = compactionEnd("s", { text: "second checkpoint" });
    await replay(capture, [
      event("session.synthetic", { sessionID: "s", text: "s prefix" }),
      event("session.synthetic", { sessionID: "other", text: "other prefix" }),
      start(),
      end(),
      start("other"),
      end("other"),
      firstStart,
      otherStart,
      other,
      first,
      secondStart,
      second,
      start("s", "b"),
      end("s", "b"),
      start("other", "b"),
      end("other", "b"),
    ]);
    expect(exportCompaction).toHaveBeenCalledTimes(3);
    expect(
      new Set(compactions(generations).map((entry) => entry.seed.id)).size,
    ).toBe(3);
    for (const [terminal, input, parent] of [
      [first, "s prefix", stableOpencodeGenerationId("s", "a")],
      [other, "other prefix", stableOpencodeGenerationId("other", "a")],
      [second, "first checkpoint", stableOpencodeGenerationId("s", "a")],
    ] as const) {
      const compacted = findCompaction(generations, terminal);
      expect(JSON.stringify(compacted?.result?.input)).toContain(input);
      expect(compacted?.seed.parentGenerationIds).toEqual([parent]);
    }
    expect(JSON.stringify(findCompaction(generations, first))).not.toContain(
      "other prefix",
    );
    expect(JSON.stringify(findCompaction(generations, other))).not.toContain(
      "s prefix",
    );
    for (const sessionID of ["s", "other"]) {
      const turn = generations.find(
        (entry) => entry.seed.id === stableOpencodeGenerationId(sessionID, "b"),
      );
      expect(turn?.seed.parentGenerationIds).toEqual([
        stableOpencodeGenerationId(sessionID, "a"),
      ]);
    }
    await capture.shutdown();
  });

  it.each([
    { name: "started only", tail: () => [] },
    {
      name: "delta",
      tail: () => [
        event("session.compaction.delta", {
          sessionID: "s",
          text: nativeFixture.secret,
        }),
      ],
    },
    { name: "location shutdown", tail: () => [event("location.shutdown", {})] },
    { name: "move", tail: () => [move()] },
    ...lifecycleTerminals.map(({ name, make }) => ({
      name,
      tail: () => [make()],
    })),
    { name: "primary supersession", tail: () => [start("s", "b")] },
  ])("does not synthesize a terminal on $name or capture.shutdown", async ({
    tail,
  }) => {
    const { capture, generations, exportCompaction } = harness();
    await replay(capture, [start(), compactionStart(), ...tail()]);
    expect(exportCompaction).not.toHaveBeenCalled();
    await capture.shutdown();
    expect(exportCompaction).not.toHaveBeenCalled();
    expect(compactions(generations)).toEqual([]);
  });

  it("never exports a standalone compaction delta", async () => {
    const { capture, generations } = harness();
    await replay(capture, [
      event("session.compaction.delta", {
        sessionID: "s",
        text: nativeFixture.secret,
      }),
    ]);
    await capture.shutdown();
    expect(generations).toEqual([]);
  });

  it.each([
    "move",
    "delete",
  ] as const)("removes stale snapshots on %s before a later end-only compaction", async (transition) => {
    const { capture, generations } = harness();
    await replay(capture, [
      event("session.synthetic", {
        sessionID: "s",
        text: "stale compaction prefix",
      }),
      start(),
      compactionStart(),
      ...(transition === "move"
        ? [move(), move("s", home, away)]
        : [event("session.deleted", { sessionID: "s" }), created("s")]),
    ]);
    const terminal = compactionEnd();
    await replay(capture, [terminal]);
    const compacted = findCompaction(generations, terminal);
    expect(compactions(generations)).toHaveLength(1);
    expect(compacted?.result?.input).toEqual([]);
    expect(compacted?.seed.parentGenerationIds).toBeUndefined();
    expect(compacted?.seed.startedAt).toEqual(new Date(terminal.created));
    expect(JSON.stringify(compacted)).not.toContain("stale compaction prefix");
    await capture.shutdown();
  });

  it.each([
    {
      name: "compaction only",
      primary: false,
      primaryFirst: false,
      failed: false,
    },
    {
      name: "primary settles first",
      primary: true,
      primaryFirst: true,
      failed: false,
    },
    {
      name: "compaction settles first",
      primary: true,
      primaryFirst: false,
      failed: false,
    },
    {
      name: "pending failure after primary",
      primary: true,
      primaryFirst: true,
      failed: true,
    },
  ])("retains pending work through location shutdown: $name", async ({
    primary,
    primaryFirst,
    failed,
  }) => {
    const { capture, generations, exportCompaction } = harness();
    await replay(capture, [
      ...(primary ? [start()] : []),
      compactionStart(),
      event("location.shutdown", {}),
      ...(primary && primaryFirst ? [end()] : []),
    ]);
    expect(exportCompaction).not.toHaveBeenCalled();
    const terminal = failed ? compactionFailure() : compactionEnd();
    // Terminal events can omit location.
    delete terminal.location;
    await replay(capture, [
      terminal,
      ...(primary && !primaryFirst ? [end()] : []),
      terminal,
    ]);
    await capture.shutdown();
    expect(compactions(generations)).toHaveLength(1);
    const compacted = findCompaction(generations, terminal);
    expect(compacted?.seed.startedAt).toEqual(new Date(120));
    expect(compacted?.seed.parentGenerationIds).toEqual(
      primary ? [stableOpencodeGenerationId("s", "a")] : undefined,
    );
    expect(compacted?.result?.usage?.inputTokens).toBe(
      failed ? undefined : 109,
    );
    expect(generations).toHaveLength(primary ? 2 : 1);
  });

  it.each(
    [false, true].flatMap((primary) =>
      ["ended", "failed"].map((kind) => ({ primary, kind })),
    ),
  )("rejects new compactions after draining, even with an existing primary (primary=$primary, $kind)", async ({
    primary,
    kind,
  }) => {
    const { capture, generations, exportCompaction } = harness();
    await replay(capture, [
      ...(primary ? [start()] : []),
      event("location.shutdown", {}),
      compactionStart(),
      kind === "ended" ? compactionEnd() : compactionFailure(),
      ...(primary ? [end()] : []),
    ]);
    await capture.shutdown();
    expect(exportCompaction).not.toHaveBeenCalled();
    expect(compactions(generations)).toEqual([]);
    expect(generations).toHaveLength(primary ? 1 : 0);
  });

  it.each([
    away,
    { directory: "/repo", workspaceID: "w2" },
  ])("does not retain or export foreign compactions at %j", async (foreign) => {
    const { capture, generations } = harness();
    await replay(capture, [
      compactionStart("s", "auto", foreign),
      compactionEnd("s", { text: "foreign summary" }, foreign),
      compactionEnd(),
    ]);
    expect(compactions(generations)).toHaveLength(1);
    expect(JSON.stringify(generations)).not.toContain("foreign summary");
    expect(
      compactions(generations)[0]?.result?.metadata?.["opencode.input_source"],
    ).toBe("unavailable");
    await capture.shutdown();
  });

  it("ignores cumulative usage.updated without changing primary or compaction usage", async () => {
    const { capture, generations } = harness();
    const terminal = compactionEnd();
    await replay(capture, [start(), compactionStart(), terminal]);
    const before = JSON.stringify(findCompaction(generations, terminal));
    await replay(capture, [
      event("session.usage.updated", {
        sessionID: "s",
        tokens: { ...compactionTokens, input: 99999 },
        cost: 999,
      }),
      end(),
      event("session.usage.updated", {
        sessionID: "s",
        tokens: { ...compactionTokens, input: 999999 },
        cost: 9999,
      }),
    ]);
    expect(generations).toHaveLength(2);
    expect(JSON.stringify(findCompaction(generations, terminal))).toBe(before);
    const turn = generations.find(
      (entry) => entry.seed.id === stableOpencodeGenerationId("s", "a"),
    );
    expect(turn?.result?.usage?.inputTokens).toBe(19);
    expect(turn?.result?.metadata?.cost_usd).toBe(0.5);
    await capture.shutdown();
  });
});

describe("native compaction reload through the real SDK", () => {
  it.each(
    compactionReloads,
  )("does not double $kind exports or token metrics ($delivery)", async ({
    kind,
    delivery,
  }) => {
    const exporter = new InMemoryMetricExporter(
      AggregationTemporality.CUMULATIVE,
    );
    const reader = new PeriodicExportingMetricReader({
      exporter,
      exportIntervalMillis: 60_000,
    });
    const provider = new MeterProvider({ readers: [reader] });
    const exported: Generation[] = [];
    const instances: Array<{
      client: Agento11yClient;
      capture: ReturnType<typeof createNativeCapture>;
    }> = [];
    const make = (name: string, createCapture = createNativeCapture) => {
      const client = new Agento11yClient({
        hooks: { enabled: false },
        meter: provider.getMeter(name),
        generationExport: { protocol: "none" },
        generationExporter: {
          exportGenerations: async ({ generations }) => {
            exported.push(...generations);
            return {
              results: generations.map((generation) => ({
                generationId: generation.id,
                accepted: true,
              })),
            };
          },
          exportWorkflowSteps: async () => ({ results: [] }),
        },
      });
      const instance = {
        client,
        capture: createCapture({
          client,
          config,
          redactor: createRedactor(),
          ...home,
          debugLog: vi.fn(),
        }),
      };
      instances.push(instance);
      return instance;
    };
    try {
      const old = make("old");
      await replay(old.capture, [
        compactionStart(),
        event("location.shutdown", {}),
      ]);
      vi.resetModules();
      const reloaded = await import("./v2-events.js");
      expect(reloaded.createNativeCapture).not.toBe(createNativeCapture);
      const replacement = make("replacement", reloaded.createNativeCapture);
      const usage = {
        tokens: {
          input: 100,
          output: 20,
          reasoning: 0,
          cache: { read: 0, write: 0 },
        },
        cost: 0.001,
      };
      const terminal =
        kind === "ended"
          ? compactionEnd("s", usage)
          : compactionFailure("s", usage);
      const ordered =
        delivery === "old first"
          ? [old, replacement]
          : delivery === "replacement first"
            ? [replacement, old]
            : [replacement];
      for (const { capture } of ordered) await replay(capture, [terminal]);
      await old.capture.shutdown();
      await replay(replacement.capture, [terminal]);
      for (const { client } of instances) await client.flush();
      await reader.forceFlush();
      expect(exported).toHaveLength(1);
      expect(exported[0]?.id).toBe(
        stableOpencodeCompactionGenerationId("s", terminal.id),
      );
      expect(exported[0]?.usage).toMatchObject({
        inputTokens: 100,
        outputTokens: 20,
      });
      expect(exported[0]?.metadata?.["opencode.input_source"]).toBe(
        "first_observed_prefix",
      );
      const points = exporter
        .getMetrics()
        .flatMap((resource) => resource.scopeMetrics)
        .flatMap((scope) => scope.metrics)
        .flatMap((metric) =>
          metric.descriptor.name === "gen_ai.client.token.usage" &&
          metric.dataPointType === DataPointType.HISTOGRAM
            ? metric.dataPoints
            : [],
        );
      expect(
        points.map((point) => ({
          type: point.attributes["gen_ai.token.type"],
          count: point.value.count,
          sum: point.value.sum,
        })),
      ).toEqual([
        { type: "input", count: 1, sum: 100 },
        { type: "output", count: 1, sum: 20 },
      ]);
    } finally {
      for (const { capture, client } of instances) {
        await capture.shutdown();
        await client.shutdown();
      }
      await provider.shutdown();
    }
  });
});

describe("native history through the real SDK exporter", () => {
  const modes = [
    "full",
    "no_tool_content",
    "metadata_only",
    "full_with_metadata_spans",
  ] satisfies ContentCaptureMode[];
  const policies = modes.flatMap((contentCapture) =>
    [true, false].map((redactInputMessages) => ({
      contentCapture,
      redactInputMessages,
    })),
  );
  const secret = "glc_abcdefghijklmnopqrstuvwxyz1234";
  const envSecret = "DATABASE_PASSWORD=hunter2secret123";
  const redactor = createRedactor();

  async function exportEvents(
    events: NativeEvent[],
    contentCapture: ContentCaptureMode,
    redactInputMessages = true,
  ) {
    const exported: Generation[] = [];
    const diagnostics: string[] = [];
    const client = new Agento11yClient({
      contentCapture,
      hooks: { enabled: false },
      generationExport: { protocol: "none" },
      generationExporter: {
        exportGenerations: async ({ generations }) => {
          exported.push(...generations);
          return {
            results: generations.map((generation) => ({
              generationId: generation.id,
              accepted: true,
            })),
          };
        },
        exportWorkflowSteps: async () => ({ results: [] }),
      },
    });
    const capture = createNativeCapture({
      client,
      config: { ...config, contentCapture, redactInputMessages },
      redactor,
      ...home,
      debugLog: (message) => diagnostics.push(message),
    });
    try {
      await replay(capture, events);
      await capture.shutdown();
      await client.flush();
      return { exported, diagnostics };
    } finally {
      await capture.shutdown();
      await client.shutdown();
    }
  }
  function texts(messages: Generation["input"]) {
    return (messages ?? []).flatMap((message) =>
      (message.parts ?? []).flatMap((part) =>
        part.type === "text" ? [part.text] : [],
      ),
    );
  }
  function instructions(text?: string) {
    return event("session.instructions.updated", {
      sessionID: "s",
      delta: { instructions: "changed" },
      ...(text !== undefined && { text }),
    });
  }
  function compaction(text: string, recent: string) {
    return event("session.compaction.ended", {
      sessionID: "s",
      reason: "auto",
      model: { id: "model", providerID: "provider" },
      text,
      recent,
      cost: 1,
      tokens: {
        input: 100,
        output: 10,
        reasoning: 0,
        cache: { read: 0, write: 0 },
      },
    });
  }

  it.each(
    policies,
  )("exports frozen chronological instruction updates under $contentCapture, redactInputMessages=$redactInputMessages", async ({
    contentCapture,
    redactInputMessages,
  }) => {
    const initial = `  first update ${secret}; ${envSecret}\n`;
    const retry = `retry update ${secret}`;
    const later = "later update";
    const user = `prompt ${secret}`;
    const { exported, diagnostics } = await exportEvents(
      [
        enqueue(user),
        event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
        instructions(initial),
        start(),
        instructions(retry),
        start(),
        end(),
        start("s", "b"),
        end("s", "b"),
        instructions(),
        instructions(" \n\t"),
        start("s", "c"),
        end("s", "c"),
        instructions(later),
        start("s", "d"),
        end("s", "d"),
      ],
      contentCapture,
      redactInputMessages,
    );
    expect(exported).toHaveLength(4);
    expect(diagnostics).toEqual([]);
    expect(exported.map((generation) => generation.systemPrompt)).toEqual(
      contentCapture === "metadata_only"
        ? ["", "", "", ""]
        : [
            initial,
            `${initial}\n\n${retry}`,
            `${initial}\n\n${retry}`,
            `${initial}\n\n${retry}\n\n${later}`,
          ].map((text) => redactor.redact(text)),
    );
    for (const generation of exported) {
      expect(texts(generation.input)).toEqual(
        contentCapture === "metadata_only"
          ? []
          : [redactInputMessages ? redactor.redact(user) : user],
      );
      expect(
        generation.input?.every((message) => message.role === "user"),
      ).toBe(true);
      expect(generation.systemPrompt).not.toContain(secret);
      expect(generation.systemPrompt).not.toContain("hunter2secret123");
    }
  });

  it.each(
    policies,
  )("exports compaction summary and recent context in later generations under $contentCapture, redactInputMessages=$redactInputMessages", async ({
    contentCapture,
    redactInputMessages,
  }) => {
    const summary = `  summary ${secret}; ${envSecret}\n`;
    const recent = `\n recent ${secret}; ${envSecret}  `;
    const checkpoint = compaction(summary, recent);
    const { exported, diagnostics } = await exportEvents(
      [
        enqueue("old prompt"),
        event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
        start(),
        end(),
        checkpoint,
        start("s", "b"),
        event("session.text.ended", {
          sessionID: "s",
          assistantMessageID: "b",
          ordinal: 0,
          text: "after checkpoint",
        }),
        end("s", "b"),
        start("s", "c"),
        end("s", "c"),
      ],
      contentCapture,
      redactInputMessages,
    );
    expect(exported).toHaveLength(4);
    expect(diagnostics).toEqual([]);
    const auxiliary = exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", checkpoint.id),
    );
    expect(auxiliary).toMatchObject({
      mode: "SYNC",
      operationName: "generateText",
      tags: { "opencode.call_kind": "compaction" },
      usage: { inputTokens: 100, outputTokens: 10 },
      metadata: {
        cost_usd: 1,
        "opencode.usage_scope": "logical_compaction",
        "opencode.settlement": "compaction.ended",
        "opencode.input_source": "unavailable",
      },
    });
    expect(auxiliary?.input).toEqual([]);
    expect(texts(auxiliary?.output)).toEqual(
      contentCapture === "metadata_only" ? [] : [redactor.redact(summary)],
    );
    expect(JSON.stringify(auxiliary)).not.toContain(secret);
    expect(JSON.stringify(auxiliary)).not.toContain(nativeFixture.envSecret);
    expect(JSON.stringify(auxiliary?.output)).not.toContain("recent");
    const turns = exported.filter(
      (generation) => generation.tags?.["opencode.call_kind"] !== "compaction",
    );
    expect(turns).toHaveLength(3);
    const after = turns.find(
      (generation) => generation.id === stableOpencodeGenerationId("s", "b"),
    );
    const later = turns.find(
      (generation) => generation.id === stableOpencodeGenerationId("s", "c"),
    );
    const expected =
      contentCapture === "metadata_only"
        ? []
        : [summary, recent].map((text) =>
            redactInputMessages ? redactor.redact(text) : text,
          );
    expect(texts(after?.input)).toEqual(expected);
    expect(texts(later?.input)).toEqual(
      contentCapture === "metadata_only"
        ? []
        : [...expected, "after checkpoint"],
    );
    for (const generation of [after, later]) {
      expect(JSON.stringify(generation?.input)).not.toContain("old prompt");
      expect(
        generation?.input?.every((message) =>
          ["user", "assistant"].includes(message.role),
        ),
      ).toBe(true);
    }
  });

  it.each(
    modes.flatMap((contentCapture) =>
      [false, true].map((blankInput) => ({ contentCapture, blankInput })),
    ),
  )("exports useful content and later history despite blank text/thinking under $contentCapture, blankInput=$blankInput", async ({
    contentCapture,
    blankInput,
  }) => {
    const { exported, diagnostics } = await exportEvents(
      [
        enqueue("  prompt\n"),
        event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
        ...(blankInput
          ? [
              enqueue(" \n\t", "blank"),
              event("session.inbox.delivered", {
                sessionID: "s",
                inboxID: "blank",
              }),
              event("session.synthetic", { sessionID: "s", text: " \t\n" }),
              event("session.skill.activated", {
                sessionID: "s",
                id: "blank-skill",
                name: "blank",
                text: "\t\n",
              }),
            ]
          : []),
        start(),
        event("session.text.ended", {
          sessionID: "s",
          assistantMessageID: "a",
          ordinal: 0,
          text: " \n\t",
        }),
        event("session.reasoning.ended", {
          sessionID: "s",
          assistantMessageID: "a",
          ordinal: 1,
          text: "\t\n",
        }),
        event("session.text.ended", {
          sessionID: "s",
          assistantMessageID: "a",
          ordinal: 2,
          text: "  reply\n",
        }),
        event("session.reasoning.ended", {
          sessionID: "s",
          assistantMessageID: "a",
          ordinal: 3,
          text: "\n thought  ",
        }),
        end(),
        start("s", "b"),
        end("s", "b"),
      ],
      contentCapture,
    );
    expect(exported).toHaveLength(2);
    expect(diagnostics).toEqual([]);
    expect(texts(exported[0]?.input)).toEqual(
      contentCapture === "metadata_only" ? [] : ["  prompt\n"],
    );
    expect(exported[0]?.output).toEqual(
      contentCapture === "metadata_only"
        ? []
        : [
            {
              role: "assistant",
              parts: [{ type: "text", text: "  reply\n", metadata: undefined }],
            },
            {
              role: "assistant",
              parts: [
                {
                  type: "thinking",
                  thinking: "\n thought  ",
                  metadata: undefined,
                },
              ],
            },
          ],
    );
    expect(exported[1]?.input).toEqual([
      ...(exported[0]?.input ?? []),
      ...(exported[0]?.output ?? []),
    ]);
  });

  it.each(
    modes,
  )("freezes instructions across compaction and captures only new updates afterward under %s", async (contentCapture) => {
    const checkpoint = compaction("checkpoint", "recent");
    const { exported, diagnostics } = await exportEvents(
      [
        instructions("before checkpoint"),
        start(),
        instructions("update during active assistant"),
        checkpoint,
        start("s", "b"),
        end("s", "b"),
        instructions("after checkpoint"),
        start("s", "c"),
        end("s", "c"),
      ],
      contentCapture,
    );
    expect(exported).toHaveLength(4);
    expect(diagnostics).toEqual([]);
    const auxiliary = exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", checkpoint.id),
    );
    expect(auxiliary?.tags?.["opencode.call_kind"]).toBe("compaction");
    const turns = ["a", "b", "c"].map((id) =>
      exported.find(
        (generation) => generation.id === stableOpencodeGenerationId("s", id),
      ),
    );
    expect(turns.map((generation) => generation?.systemPrompt ?? "")).toEqual(
      contentCapture === "metadata_only"
        ? ["", "", ""]
        : ["before checkpoint", "", "after checkpoint"],
    );
    const next = exported.find(
      (generation) => generation.id === stableOpencodeGenerationId("s", "b"),
    );
    expect(texts(next?.input)).toEqual(
      contentCapture === "metadata_only" ? [] : ["checkpoint", "recent"],
    );
  });

  it.each(
    policies,
  )("captures compaction input and fully redacts its summary under $contentCapture, redactInputMessages=$redactInputMessages", async ({
    contentCapture,
    redactInputMessages,
  }) => {
    const user = `user prefix ${secret}`;
    const summary = `summary ${secret}; ${envSecret}`;
    const begun = compactionStart();
    const terminal = compactionEnd("s", {
      text: summary,
      recent: "RECENT_NOT_GENERATED",
    });
    const data = {
      sessionID: "s",
      assistantMessageID: "a",
      id: "history-tool",
    };
    const { exported, diagnostics } = await exportEvents(
      [
        enqueue(user),
        event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
        start(),
        event("session.tool.input.started", { ...data, name: "read" }),
        event("session.tool.called", {
          ...data,
          input: { path: `file ${secret}` },
          executed: false,
        }),
        event("session.tool.success", {
          ...data,
          content: [
            { type: "text", text: `tool result ${secret}; ${envSecret}` },
          ],
          executed: false,
        }),
        end(),
        begun,
        terminal,
      ],
      contentCapture,
      redactInputMessages,
    );
    expect(diagnostics).toEqual([]);
    expect(exported).toHaveLength(2);
    const compacted = exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", terminal.id),
    );
    expect(compacted?.tags?.["opencode.call_kind"]).toBe("compaction");
    expect(compacted?.usage?.inputTokens).toBe(109);
    expect(compacted?.metadata).toMatchObject({
      "opencode.input_source": "first_observed_prefix",
      "opencode.start_time_approximate": false,
    });
    expect(texts(compacted?.output)).toEqual(
      contentCapture === "metadata_only" ? [] : [redactor.redact(summary)],
    );
    expect(JSON.stringify(compacted?.output)).not.toContain(secret);
    expect(JSON.stringify(compacted?.output)).not.toContain(
      nativeFixture.envSecret,
    );
    expect(JSON.stringify(compacted?.output)).not.toContain(
      "RECENT_NOT_GENERATED",
    );
    if (contentCapture === "metadata_only") {
      expect(compacted?.input).toEqual([]);
    } else {
      expect(texts(compacted?.input)).toEqual([
        redactInputMessages ? redactor.redact(user) : user,
      ]);
      const parts = compacted?.input?.flatMap((message) => message.parts ?? []);
      const call = parts?.find((part) => part.type === "tool_call");
      const result = parts?.find((part) => part.type === "tool_result");
      expect(call?.type).toBe("tool_call");
      expect(result?.type).toBe("tool_result");
      const captureTools = contentCapture !== "no_tool_content";
      if (call?.type === "tool_call") {
        expect(call.toolCall.inputJSON).toBe(
          captureTools
            ? redactor.redact(JSON.stringify({ path: `file ${secret}` }))
            : "",
        );
      }
      if (result?.type === "tool_result") {
        expect(result.toolResult.content).toBe(
          captureTools
            ? redactor.redact(`tool result ${secret}; ${envSecret}`)
            : "",
        );
      }
    }
  });

  it.each(
    policies,
  )("failure preserves ordinary history and instructions under $contentCapture, redactInputMessages=$redactInputMessages", async ({
    contentCapture,
    redactInputMessages,
  }) => {
    const prefix = [
      enqueue(`original prompt ${secret}`),
      event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
      instructions(`original instruction ${secret}; ${envSecret}`),
      start(),
      event("session.text.ended", {
        sessionID: "s",
        assistantMessageID: "a",
        ordinal: 0,
        text: "original reply",
      }),
      end(),
      instructions("instruction after first turn"),
    ];
    const next = [start("s", "b"), end("s", "b")];
    const terminal = compactionFailure("s", {
      tokens: compactionTokens,
      cost: 0.001,
    });
    const baseline = await exportEvents(
      [...prefix, ...next],
      contentCapture,
      redactInputMessages,
    );
    const failed = await exportEvents(
      [...prefix, compactionStart("s", "manual"), terminal, ...next],
      contentCapture,
      redactInputMessages,
    );
    expect(baseline.exported).toHaveLength(2);
    expect(failed.exported).toHaveLength(3);
    expect(failed.diagnostics).toEqual([]);
    const compacted = failed.exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", terminal.id),
    );
    expect(compacted?.tags?.["opencode.call_kind"]).toBe("compaction");
    for (const assistantID of ["a", "b"]) {
      const id = stableOpencodeGenerationId("s", assistantID);
      const before = baseline.exported.find(
        (generation) => generation.id === id,
      );
      const after = failed.exported.find((generation) => generation.id === id);
      expect(after?.input).toEqual(before?.input);
      expect(after?.systemPrompt).toBe(before?.systemPrompt);
      expect(after?.parentGenerationIds).toEqual(before?.parentGenerationIds);
      expect(after?.usage).toEqual(before?.usage);
    }
    const following = failed.exported.find(
      (generation) => generation.id === stableOpencodeGenerationId("s", "b"),
    );
    expect(texts(following?.input)).toEqual(
      contentCapture === "metadata_only"
        ? []
        : [
            redactInputMessages
              ? redactor.redact(`original prompt ${secret}`)
              : `original prompt ${secret}`,
            "original reply",
          ],
    );
    expect(following?.systemPrompt ?? "").toBe(
      contentCapture === "metadata_only"
        ? ""
        : redactor.redact(
            `original instruction ${secret}; ${envSecret}\n\ninstruction after first turn`,
          ),
    );
  });

  it.each(
    modes,
  )("exports native empty summary usage without opaque provider state under %s", async (contentCapture) => {
    const terminal = compactionEnd("s", {
      text: "",
      recent: "",
      model: undefined,
    });
    Object.assign(terminal.data, {
      providerState: { secret: "OPAQUE_PROVIDER_STATE" },
      providerContext: { secret: "OPAQUE_PROVIDER_CONTEXT" },
    });
    const { exported, diagnostics } = await exportEvents(
      [compactionStart(), terminal],
      contentCapture,
    );
    expect(diagnostics).toEqual([]);
    expect(exported).toHaveLength(1);
    const compacted = exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", terminal.id),
    );
    expect(compacted?.output).toEqual([]);
    expect(compacted?.usage).toMatchObject({
      inputTokens: 109,
      outputTokens: 23,
    });
    expect(compacted?.metadata).toMatchObject({
      cost_usd: 0.001,
      "opencode.model_source": "unavailable",
    });
    expect(compacted?.model).toEqual({ provider: "unknown", name: "unknown" });
    expect(JSON.stringify(exported)).not.toMatch(
      /OPAQUE_PROVIDER|providerState|providerContext/,
    );
  });

  it.each([
    ["", "recent only"],
    ["summary only", " \n\t"],
    ["\t", "\n"],
  ])("exports compaction with summary=%j and recent=%j", async (summary, recent) => {
    const checkpoint = compaction(summary, recent);
    const { exported, diagnostics } = await exportEvents(
      [checkpoint, start(), end()],
      "full",
    );
    expect(exported).toHaveLength(2);
    expect(diagnostics).toEqual([]);
    const auxiliary = exported.find(
      (generation) =>
        generation.id ===
        stableOpencodeCompactionGenerationId("s", checkpoint.id),
    );
    expect(auxiliary?.tags?.["opencode.call_kind"]).toBe("compaction");
    expect(texts(auxiliary?.output)).toEqual(summary.trim() ? [summary] : []);
    const turn = exported.find(
      (generation) => generation.id === stableOpencodeGenerationId("s", "a"),
    );
    expect(texts(turn?.input)).toEqual(
      [summary, recent].filter((text) => text.trim().length > 0),
    );
  });
});

describe("native tools and real SDK capture modes", () => {
  function tools(sessionID = "s", assistantMessageID = "a"): NativeEvent[] {
    const data = { sessionID, assistantMessageID, id: "outer-code-call" };
    return [
      event("session.tool.input.started", { ...data, name: "code" }, home, 110),
      event(
        "session.tool.called",
        {
          ...data,
          input: { code: "secret = ghp_abcdefghijklmnopqrstuvwxyz1234567890" },
          executed: false,
        },
        home,
        120,
      ),
      // Inner Code Mode identities in UI metadata are NOT independently identified executions.
      event("session.tool.progress", {
        ...data,
        metadata: {
          calls: [
            { id: "repeat", name: "read" },
            { id: "repeat", name: "read" },
          ],
        },
      }),
      event(
        "session.tool.success",
        {
          ...data,
          content: [
            {
              type: "text",
              text: "result ghp_abcdefghijklmnopqrstuvwxyz1234567890",
            },
          ],
          executed: false,
        },
        home,
        180,
      ),
    ];
  }
  it.each([
    "full",
    "no_tool_content",
    "metadata_only",
    "full_with_metadata_spans",
  ] satisfies ContentCaptureMode[])("exports through real SDK under %s", async (contentCapture) => {
    const exporter = new InMemorySpanExporter();
    const provider = new BasicTracerProvider({
      spanProcessors: [new SimpleSpanProcessor(exporter)],
    });
    const exported: Generation[] = [];
    const client = new Agento11yClient({
      contentCapture,
      hooks: { enabled: false },
      tracer: provider.getTracer("native-test"),
      generationExport: { protocol: "none" },
      generationExporter: {
        exportGenerations: async ({ generations }) => {
          exported.push(...generations);
          return {
            results: generations.map((g) => ({
              generationId: g.id,
              accepted: true,
            })),
          };
        },
        exportWorkflowSteps: async () => ({ results: [] }),
      },
    });
    const capture = createNativeCapture({
      client,
      config: {
        ...config,
        contentCapture,
        guards: { enabled: true, timeoutMs: 1, failOpen: false },
      },
      redactor: createRedactor(),
      ...home,
      debugLog: vi.fn(),
    });
    try {
      await replay(capture, [
        enqueue("prompt ghp_abcdefghijklmnopqrstuvwxyz1234567890"),
        event("session.inbox.delivered", { sessionID: "s", inboxID: "i" }),
        start(),
        ...tools(),
        event("session.text.ended", {
          sessionID: "s",
          assistantMessageID: "a",
          ordinal: 0,
          text: "reply",
        }),
        event(
          "session.step.streamed",
          { sessionID: "s", assistantMessageID: "a" },
          home,
          190,
        ),
        end(),
        end(),
      ]);
      await capture.shutdown();
      await client.flush();
      await provider.forceFlush();
      const generation = client.debugSnapshot().generations[0];
      expect(client.debugSnapshot().generations).toHaveLength(1);
      expect(generation?.id).toBe(stableOpencodeGenerationId("s", "a"));
      expect(generation?.mode).toBe("STREAM");
      expect(generation?.model).toEqual({
        name: "model",
        provider: "provider",
      });
      expect(generation?.usage).toMatchObject({
        inputTokens: 19,
        outputTokens: 23,
        totalTokens: 42,
        inputSemantics: "inclusive",
      });
      expect(generation?.completedAt).toEqual(new Date(190));
      expect(JSON.stringify(generation)).not.toContain(
        "ghp_abcdefghijklmnopqrstuvwxyz1234567890",
      );
      const snapshotTools = client.debugSnapshot().toolExecutions;
      expect(snapshotTools).toHaveLength(1);
      expect(snapshotTools[0]?.toolCallId).toBe("outer-code-call");
      expect(snapshotTools[0]?.startedAt).toEqual(new Date(120));
      expect(snapshotTools[0]?.completedAt).toEqual(new Date(180));
      const toolSpans = exporter
        .getFinishedSpans()
        .filter((span) => span.attributes["gen_ai.tool.name"] === "code");
      expect(toolSpans).toHaveLength(1);
      const spanContent = JSON.stringify(toolSpans[0]?.attributes);
      expect(spanContent).not.toContain(
        "ghp_abcdefghijklmnopqrstuvwxyz1234567890",
      );
      expect(spanContent.includes("gen_ai.tool.call.arguments")).toBe(
        contentCapture === "full",
      );
      expect(JSON.stringify(generation?.input).includes("prompt")).toBe(
        contentCapture !== "metadata_only",
      );
      const resultContent = generation?.output
        ?.flatMap((message) => message.parts ?? [])
        .find((part) => part.type === "tool_result");
      expect(
        resultContent?.type === "tool_result" &&
          Boolean(resultContent.toolResult.content),
      ).toBe(
        contentCapture === "full" ||
          contentCapture === "full_with_metadata_spans",
      );
      expect(exported).toHaveLength(1);
    } finally {
      await capture.shutdown();
      await client.shutdown();
      await provider.shutdown();
    }
  });

  it.each([
    { name: "ASCII input", field: "input", value: "x".repeat(5 << 20) },
    { name: "ASCII output", field: "output", value: "x".repeat(5 << 20) },
    { name: "NUL output", field: "output", value: "\0".repeat(5 << 20) },
    {
      name: "NUL output below the raw cap",
      field: "output",
      value: "\0".repeat(1 << 20),
    },
  ])("exports tool $name and the following turn through the SDK", async ({
    field,
    value,
  }) => {
    const exported: Generation[] = [];
    const client = new Agento11yClient({
      contentCapture: "full_with_metadata_spans",
      hooks: { enabled: false },
      generationExport: { protocol: "none" },
      generationExporter: {
        exportGenerations: async ({ generations }) => {
          exported.push(...generations);
          return {
            results: generations.map((generation) => ({
              generationId: generation.id,
              accepted: true,
            })),
          };
        },
        exportWorkflowSteps: async () => ({ results: [] }),
      },
    });
    const debugLog = vi.fn();
    const capture = createNativeCapture({
      client,
      config: { ...config, contentCapture: "full_with_metadata_spans" },
      redactor: createRedactor(),
      ...home,
      debugLog,
    });
    const data = { sessionID: "s", assistantMessageID: "a", id: "c" };
    try {
      await replay(capture, [
        start(),
        event("session.tool.input.started", { ...data, name: "read" }),
        event("session.tool.called", {
          ...data,
          input: { path: field === "input" ? value : "file" },
          executed: false,
        }),
        event("session.tool.success", {
          ...data,
          content: [
            { type: "text", text: field === "output" ? value : "result" },
          ],
          executed: false,
        }),
        end(),
        start("s", "b"),
        end("s", "b"),
      ]);
      await capture.shutdown();
      await client.flush();
      expect(exported).toHaveLength(2);
      expect(JSON.stringify(exported[0]?.output)).toContain("[truncated]");
      expect(exported[1]?.input).toEqual(exported[0]?.output);
      expect(debugLog).not.toHaveBeenCalled();
    } finally {
      await capture.shutdown();
      await client.shutdown();
    }
  });

  it.each([
    true,
    false,
  ])("capture never awaits or calls guard evaluation (enabled=%s)", async (enabled) => {
    const { capture, evaluateHook, generations } = harness(home, {
      guards: { enabled, timeoutMs: 1, failOpen: false },
    });
    await replay(capture, [start(), ...tools(), end()]);
    expect(generations).toHaveLength(1);
    expect(evaluateHook).not.toHaveBeenCalled();
  });

  it.each([
    "native failure",
    "bash nonzero exit",
  ])("maps %s and ignores duplicate tool terminals", async (failure) => {
    const { capture, generations, client, toolResults } = harness();
    const data = { sessionID: "s", assistantMessageID: "a", id: "c" };
    const terminal =
      failure === "native failure"
        ? event(
            "session.tool.failed",
            {
              ...data,
              executed: false,
              error: {
                type: "tool.execution",
                message: "failed ghp_abcdefghijklmnopqrstuvwxyz1234567890",
              },
            },
            home,
            200,
          )
        : event(
            "session.tool.success",
            {
              ...data,
              executed: false,
              content: [{ type: "text", text: "failed" }],
              metadata: { exit: 1 },
            },
            home,
            200,
          );
    await replay(capture, [
      start(),
      event("session.tool.input.started", { ...data, name: "bash" }),
      event("session.tool.called", { ...data, input: {}, executed: false }),
      terminal,
      terminal,
      end(),
    ]);
    expect(client.startToolExecution).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(generations[0]?.result?.output)).toContain(
      '"isError":true',
    );
    expect(JSON.stringify(toolResults)).not.toContain(
      "ghp_abcdefghijklmnopqrstuvwxyz1234567890",
    );
  });
});
