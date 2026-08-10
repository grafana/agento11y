import { beforeEach, describe, expect, it, vi } from "vitest";

const { createAgento11yClientMock, createTelemetryProvidersMock } = vi.hoisted(
  () => ({
    createAgento11yClientMock: vi.fn(),
    createTelemetryProvidersMock: vi.fn(),
  }),
);

vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));
vi.mock("./telemetry.js", () => ({
  createTelemetryProviders: createTelemetryProvidersMock,
}));

import { _resetHookState, createAgento11yHooks } from "./hooks.js";
import {
  assistantMessage,
  baseConfig,
  emitMessageUpdated,
  emitPartUpdated,
  emitSessionCreated,
  emitSessionDeleted,
  emitSessionEvent,
  inFlightAssistantMessage,
  makeAgento11yMock,
  makeOpencodeClient,
  type TestHooks,
} from "./hooks.testutil.js";
import { stableOpencodeGenerationId } from "./lineage.js";

function textPart(
  sessionID: string,
  messageID: string,
  start: number,
): unknown {
  return {
    id: "p-1",
    sessionID,
    messageID,
    type: "text",
    text: "hello",
    time: { start },
  };
}

describe("opencode generation lineage and streaming", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("assigns a deterministic opencode- generation id from session and message id", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-det", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.id).toBe(
      stableOpencodeGenerationId("sess-det", "msg-1"),
    );
  });

  it("exports through startStreamingGeneration, not startGeneration", async () => {
    const { sigil, startStreamingGeneration, startGeneration } =
      makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-stream", "msg-1"));

    expect(startStreamingGeneration).toHaveBeenCalledTimes(1);
    expect(startGeneration).not.toHaveBeenCalled();
  });

  it("chains two sequential assistant generations via parentGenerationIds", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-chain", "msg-1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-chain", "msg-2"));

    const idA = stableOpencodeGenerationId("sess-chain", "msg-1");
    const idB = stableOpencodeGenerationId("sess-chain", "msg-2");
    expect(generations).toHaveLength(2);
    expect(generations[0]!.seed.id).toBe(idA);
    expect(generations[0]!.seed.parentGenerationIds).toBeUndefined();
    expect(generations[1]!.seed.id).toBe(idB);
    expect(generations[1]!.seed.parentGenerationIds).toEqual([idA]);
    // A session with no parent keeps its own conversation and no lineage keys.
    expect(generations.map((g) => g.seed.conversationId)).toEqual([
      "sess-chain",
      "sess-chain",
    ]);
    expect(generations.map((g) => g.seed.metadata)).toEqual([
      undefined,
      undefined,
    ]);
  });

  it("re-exporting the same message after a restart keeps the same id and no parent", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-restart", "msg-1"));
    // session.deleted clears the in-process dedup and parent chain, the same
    // way a process restart would, so the next record is a "first" turn.
    await emitSessionDeleted(hooks, "sess-restart");
    await emitMessageUpdated(hooks, assistantMessage("sess-restart", "msg-1"));

    const id = stableOpencodeGenerationId("sess-restart", "msg-1");
    expect(generations).toHaveLength(2);
    expect(generations[0]!.seed.id).toBe(id);
    expect(generations[1]!.seed.id).toBe(id);
    expect(generations[1]!.seed.parentGenerationIds).toBeUndefined();
  });

  it("links a subagent created after the parent's turn completed to that turn", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    // The parent's turn has already completed when the child session is
    // created, so the last assistant message seen for the parent is still the
    // turn that spawned the child.
    await emitMessageUpdated(hooks, assistantMessage("sess-parent", "msg-1"));
    await emitSessionCreated(hooks, "sess-child", "sess-parent");
    await emitMessageUpdated(hooks, assistantMessage("sess-child", "msg-c1"));

    const parentId = stableOpencodeGenerationId("sess-parent", "msg-1");
    const childId = stableOpencodeGenerationId("sess-child", "msg-c1");
    expect(generations).toHaveLength(2);
    expect(generations[0]!.seed.id).toBe(parentId);
    expect(generations[1]!.seed.id).toBe(childId);
    expect(generations[1]!.seed.parentGenerationIds).toEqual([parentId]);
    expect(generations[1]!.seed.conversationId).toBe("sess-parent");
  });

  it("keeps intra-session chaining for a child session's later generations", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-parent-2", "msg-1"));
    await emitSessionCreated(hooks, "sess-child-2", "sess-parent-2");
    await emitMessageUpdated(hooks, assistantMessage("sess-child-2", "msg-c1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-child-2", "msg-c2"));

    const spawningTurn = stableOpencodeGenerationId("sess-parent-2", "msg-1");
    const childId1 = stableOpencodeGenerationId("sess-child-2", "msg-c1");
    const childId2 = stableOpencodeGenerationId("sess-child-2", "msg-c2");
    expect(generations[1]!.seed.parentGenerationIds).toEqual([spawningTurn]);
    // The child's second turn chains to its own first turn, not the parent.
    expect(generations[2]!.seed.id).toBe(childId2);
    expect(generations[2]!.seed.parentGenerationIds).toEqual([childId1]);
    // Every child turn stays in the spawning conversation, not just the first.
    expect(generations[1]!.seed.conversationId).toBe("sess-parent-2");
    expect(generations[2]!.seed.conversationId).toBe("sess-parent-2");
  });

  it("keeps the parent conversation's title off a reparented child", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    // The child exports after the parent's last turn, and the backend keeps the
    // newest title per conversation, so sending the child's title would rename
    // the shared conversation to the subagent's.
    await emitSessionEvent(hooks, "session.created", {
      id: "sess-parent-5",
      title: "Check global identity",
    });
    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-5", "msg-1"),
    );
    await emitSessionEvent(hooks, "session.created", {
      id: "sess-child-5",
      parentID: "sess-parent-5",
      title: "Check global identity (@identity-check subagent)",
    });
    await emitMessageUpdated(hooks, assistantMessage("sess-parent-5", "msg-1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-child-5", "msg-c1"));

    expect(generations[0]!.seed.conversationTitle).toBe(
      "Check global identity",
    );
    expect(generations[1]!.seed.conversationId).toBe("sess-parent-5");
    expect("conversationTitle" in generations[1]!.seed).toBe(false);
    expect(generations[1]!.seed.metadata).toEqual({
      "opencode.parent_session_id": "sess-parent-5",
      "opencode.child_session_id": "sess-child-5",
    });
  });

  it("emits a reparented child's tool spans under the parent conversation", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-6", "msg-1"),
    );
    await emitSessionCreated(hooks, "sess-child-6", "sess-parent-6");
    await hooks.toolExecuteBefore(
      { tool: "bash", sessionID: "sess-child-6", callID: "tc-1" },
      { args: {} },
    );
    hooks.toolExecuteAfter(
      { tool: "bash", sessionID: "sess-child-6", callID: "tc-1", args: {} },
      { title: "bash", output: "ok", metadata: {} },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-child-6", "msg-c1"));

    // Spans nested under a turn must name the conversation the turn was
    // exported into, or the two disagree about where the turn happened.
    expect(generations[0]!.seed.conversationId).toBe("sess-parent-6");
    expect(sigil.startToolExecution).toHaveBeenCalledWith(
      expect.objectContaining({ conversationId: "sess-parent-6" }),
    );
  });

  it("freezes the spawning turn at session.created, not at child-record time", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    // Parent turn 1 completes, then turn 2 starts and spawns the subagent from
    // a task call inside it. Turn 2 and a later turn 3 both complete before the
    // child's turn is recorded. The child must link to turn 2, the turn that was
    // in flight at session.created: not turn 1 (the pre-spawn turn) and not
    // turn 3 (the parent's latest at child-record time, which is what a lazy
    // resolver would pick).
    await emitMessageUpdated(hooks, assistantMessage("sess-parent-3", "msg-1"));
    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-3", "msg-2"),
    );
    await emitSessionCreated(hooks, "sess-child-3", "sess-parent-3");
    await emitMessageUpdated(hooks, assistantMessage("sess-parent-3", "msg-2"));
    await emitMessageUpdated(hooks, assistantMessage("sess-parent-3", "msg-3"));
    await emitMessageUpdated(hooks, assistantMessage("sess-child-3", "msg-c1"));

    const spawningTurn = stableOpencodeGenerationId("sess-parent-3", "msg-2");
    const preSpawnTurn = stableOpencodeGenerationId("sess-parent-3", "msg-1");
    const laterTurn = stableOpencodeGenerationId("sess-parent-3", "msg-3");
    const childId = stableOpencodeGenerationId("sess-child-3", "msg-c1");
    const child = generations.find((g) => g.seed.id === childId);
    expect(child?.seed.parentGenerationIds).toEqual([spawningTurn]);
    expect(child?.seed.parentGenerationIds).not.toEqual([preSpawnTurn]);
    expect(child?.seed.parentGenerationIds).not.toEqual([laterTurn]);
  });

  it("links a subagent spawned on the parent's first turn to that in-flight turn", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    // Event order observed on opencode 1.18.10 for a one-shot run: the parent's
    // assistant message arrives without `time.completed`, the child session is
    // created from a task call inside it, the child runs to completion, and the
    // parent's turn only completes afterwards. The parent has recorded no
    // generation when the child is created, so the edge can only come from the
    // in-flight message.
    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-4", "msg-1"),
    );
    await emitSessionCreated(hooks, "sess-child-4", "sess-parent-4");
    await emitMessageUpdated(hooks, assistantMessage("sess-child-4", "msg-c1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-parent-4", "msg-1"));

    const spawningTurn = stableOpencodeGenerationId("sess-parent-4", "msg-1");
    expect(generations).toHaveLength(2);
    expect(generations[0]!.seed.id).toBe(
      stableOpencodeGenerationId("sess-child-4", "msg-c1"),
    );
    expect(generations[0]!.seed.parentGenerationIds).toEqual([spawningTurn]);
    expect(generations[0]!.seed.conversationId).toBe("sess-parent-4");
    expect(generations[0]!.seed.tags?.subagent).toBe("true");
    // The parent's own turn exports later under the same conversation.
    expect(generations[1]!.seed.id).toBe(spawningTurn);
    expect(generations[1]!.seed.conversationId).toBe("sess-parent-4");
  });

  it("keeps a subagent in its own conversation when the spawning turn was never seen", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    // The plugin loaded mid-run: session.created names the parent, but no
    // assistant message for it was ever observed, so the spawning turn cannot
    // be named. The child keeps its own conversation and carries the parent
    // session as metadata, because an orphan under the parent conversation is
    // worse than a child under its own.
    await emitSessionCreated(hooks, "sess-child-7", "sess-parent-7");
    await emitMessageUpdated(hooks, assistantMessage("sess-child-7", "msg-c1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.parentGenerationIds).toBeUndefined();
    expect(generations[0]!.seed.conversationId).toBe("sess-child-7");
    expect(generations[0]!.seed.metadata).toEqual({
      "opencode.parent_session_id": "sess-parent-7",
    });
    expect(generations[0]!.seed.tags?.subagent).toBe("true");
  });

  // The export says nothing about a dropped edge beyond an absent field, so the
  // log line is the only local signal that a link could not be resolved.
  it("debug-logs a subagent link it cannot resolve and stays quiet otherwise", async () => {
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const unresolved = () =>
      logged.mock.calls.some(
        ([msg]) =>
          typeof msg === "string" && msg.includes("no assistant turn seen"),
      );
    const { sigil } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig({ debug: true }),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitSessionCreated(hooks, "sess-child-9", "sess-parent-9");
    expect(unresolved()).toBe(true);

    logged.mockClear();
    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-10", "msg-1"),
    );
    await emitSessionCreated(hooks, "sess-child-10", "sess-parent-10");
    expect(unresolved()).toBe(false);

    logged.mockRestore();
  });

  it("flattens a nested subagent onto the root conversation", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-root-8", "msg-r1"),
    );
    await emitSessionCreated(hooks, "sess-child-8", "sess-root-8");
    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-child-8", "msg-c1"),
    );
    await emitSessionCreated(hooks, "sess-grand-8", "sess-child-8");
    await emitMessageUpdated(hooks, assistantMessage("sess-grand-8", "msg-g1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-child-8", "msg-c1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-root-8", "msg-r1"));

    expect(generations.map((g) => g.seed.conversationId)).toEqual([
      "sess-root-8",
      "sess-root-8",
      "sess-root-8",
    ]);
    // The grandchild links to the child turn that spawned it, which lives in
    // the root conversation too, so the edge never crosses conversations.
    expect(generations[0]!.seed.parentGenerationIds).toEqual([
      stableOpencodeGenerationId("sess-child-8", "msg-c1"),
    ]);
    // Metadata names the immediate parent, so depth survives the flattening.
    expect(generations[0]!.seed.metadata).toEqual({
      "opencode.parent_session_id": "sess-child-8",
      "opencode.child_session_id": "sess-grand-8",
    });
  });

  const parentChains: Array<{
    name: string;
    setup: (
      hooks: TestHooks,
    ) => Promise<{ sessionID: string; expectedConversationId: string }>;
  }> = [
    {
      name: "a parent chain that forms a cycle",
      setup: async (hooks) => {
        const chain = ["sess-cycle-a", "sess-cycle-b"];
        for (const id of chain) {
          await emitMessageUpdated(hooks, inFlightAssistantMessage(id, "m-1"));
        }
        await emitSessionCreated(hooks, chain[0]!, chain[1]!);
        await emitSessionCreated(hooks, chain[1]!, chain[0]!);
        // The walk leaves `sess-cycle-a`, reaches `sess-cycle-b`, and stops
        // there because the only hop left returns to a session it has visited.
        return { sessionID: chain[0]!, expectedConversationId: chain[1]! };
      },
    },
    {
      name: "a 20-session parent chain",
      setup: async (hooks) => {
        const chain = Array.from({ length: 20 }, (_, i) => `sess-deep-${i}`);
        for (const [index, id] of chain.entries()) {
          await emitMessageUpdated(hooks, inFlightAssistantMessage(id, "m-1"));
          const parent = chain[index - 1];
          if (parent) await emitSessionCreated(hooks, id, parent);
        }
        // Depth is not capped: the deepest session exports into the root of the
        // chain, the only session that is not itself a subagent.
        return { sessionID: chain.at(-1)!, expectedConversationId: chain[0]! };
      },
    },
  ];

  it.each(parentChains)("resolves the conversation id for $name", async ({
    setup,
  }) => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    const { sessionID, expectedConversationId } = await setup(hooks);
    await emitMessageUpdated(hooks, assistantMessage(sessionID, "m-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.conversationId).toBe(expectedConversationId);
  });

  it("does not link or tag a root session with no parentID", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitSessionCreated(hooks, "sess-root");
    await emitMessageUpdated(hooks, assistantMessage("sess-root", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.parentGenerationIds).toBeUndefined();
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
    expect(generations[0]!.seed.conversationId).toBe("sess-root");
    expect(generations[0]!.seed.metadata).toBeUndefined();
  });

  it("tags a child session's generations with subagent=true", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(hooks, assistantMessage("sess-parent-tag", "m-1"));
    await emitSessionCreated(hooks, "sess-child-tag", "sess-parent-tag");
    await emitMessageUpdated(hooks, assistantMessage("sess-child-tag", "m-c1"));

    expect(generations).toHaveLength(2);
    // The spawning session is not a subagent itself.
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
    expect(generations[1]!.seed.tags?.subagent).toBe("true");
    // Agent naming stays mode-based; subagent status lives in the tag only.
    expect(generations[1]!.seed.agentName).toBe("opencode:build");
  });

  it("does not tag a self-parenting session", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitSessionCreated(hooks, "sess-self-tag", "sess-self-tag");
    await emitMessageUpdated(hooks, assistantMessage("sess-self-tag", "m-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
  });

  it("drops subagent classification on session.deleted", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitSessionCreated(hooks, "sess-del-tag", "sess-parent-del");
    await emitSessionDeleted(hooks, "sess-del-tag");
    // The id is reused as a root session; the stale classification must be gone.
    await emitSessionCreated(hooks, "sess-del-tag");
    await emitMessageUpdated(hooks, assistantMessage("sess-del-tag", "m-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
  });

  it("drops subagent classification on _resetHookState", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitSessionCreated(hooks, "sess-reset-tag", "sess-parent-reset");
    _resetHookState();
    await emitSessionCreated(hooks, "sess-reset-tag");
    await emitMessageUpdated(hooks, assistantMessage("sess-reset-tag", "m-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
  });

  it("drops the spawning turn and reparenting on session.deleted", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-del-2", "m-1"),
    );
    await emitSessionCreated(hooks, "sess-del-lineage", "sess-parent-del-2");
    await emitSessionDeleted(hooks, "sess-del-lineage");
    // The id is reused as a root session; no stale lineage may resurface.
    await emitSessionCreated(hooks, "sess-del-lineage");
    await emitMessageUpdated(
      hooks,
      assistantMessage("sess-del-lineage", "m-1"),
    );

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.parentGenerationIds).toBeUndefined();
    expect(generations[0]!.seed.conversationId).toBe("sess-del-lineage");
    expect(generations[0]!.seed.metadata).toBeUndefined();
    expect(generations[0]!.seed.tags?.subagent).toBeUndefined();
  });

  it("does not reuse an in-flight assistant message from before _resetHookState", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitMessageUpdated(
      hooks,
      inFlightAssistantMessage("sess-parent-reset-2", "m-1"),
    );
    await emitSessionCreated(
      hooks,
      "sess-reset-lineage",
      "sess-parent-reset-2",
    );
    _resetHookState();
    // Both ids are reused and the link is re-announced, but the parent turn
    // that was in flight before the reset is gone, so no edge may be named.
    await emitSessionCreated(
      hooks,
      "sess-reset-lineage",
      "sess-parent-reset-2",
    );
    await emitMessageUpdated(
      hooks,
      assistantMessage("sess-reset-lineage", "m-1"),
    );

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.parentGenerationIds).toBeUndefined();
    expect(generations[0]!.seed.conversationId).toBe("sess-reset-lineage");
    expect(generations[0]!.seed.metadata).toEqual({
      "opencode.parent_session_id": "sess-parent-reset-2",
    });
  });

  it("records first-token time from the first streamed part", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await createAgento11yHooks(
      baseConfig(),
      makeOpencodeClient(),
    );
    if (!hooks) throw new Error("expected hooks");

    await emitPartUpdated(
      hooks,
      textPart("sess-ttft", "msg-1", 1_700_000_001_200),
    );
    // A later part for the same message must not overwrite the first.
    await emitPartUpdated(
      hooks,
      textPart("sess-ttft", "msg-1", 1_700_000_001_900),
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-ttft", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.firstTokenAt).toEqual(new Date(1_700_000_001_200));
  });

  it("records TTFT in metadata_only without fetching the message body", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const client = makeOpencodeClient();
    const hooks = await createAgento11yHooks(
      baseConfig({ contentCapture: "metadata_only" }),
      client,
    );
    if (!hooks) throw new Error("expected hooks");

    await emitPartUpdated(
      hooks,
      textPart("sess-ttft-meta", "msg-1", 1_700_000_001_200),
    );
    await emitMessageUpdated(
      hooks,
      assistantMessage("sess-ttft-meta", "msg-1"),
    );

    expect(generations).toHaveLength(1);
    expect(generations[0]!.firstTokenAt).toEqual(new Date(1_700_000_001_200));
    expect(client.session.message).not.toHaveBeenCalled();
  });
});
