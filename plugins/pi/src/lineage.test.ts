import { describe, expect, it } from "vitest";
import {
  type PiGenerationLineage,
  resolvePiGenerationLineage,
  resolvePiSummaryLineage,
  type SessionEntryLike,
  stablePiGenerationId,
} from "./lineage.js";
import { NOT_FORKED, type SessionOrigin } from "./sessionOrigin.js";

function assistantEntry(
  id: string,
  parentId: string | null,
  message: unknown = { role: "assistant" },
  timestamp?: string,
): SessionEntryLike {
  return {
    type: "message",
    id,
    parentId,
    timestamp,
    message: message as { role?: string },
  };
}

function userEntry(
  id: string,
  parentId: string | null,
  timestamp?: string,
): SessionEntryLike {
  return {
    type: "message",
    id,
    parentId,
    timestamp,
    message: { role: "user" },
  };
}

function toolResultEntry(
  id: string,
  parentId: string | null,
): SessionEntryLike {
  return {
    type: "message",
    id,
    parentId,
    message: { role: "toolResult" },
  };
}

/** The parent edge the caller would export, if any. */
function ownParentIds(lineage: PiGenerationLineage): string[] | undefined {
  return lineage.parent?.kind === "own"
    ? lineage.parent.generationIds
    : undefined;
}

function nonMessageEntry(
  id: string,
  parentId: string | null,
): SessionEntryLike {
  // e.g. thinking_level_change, model_change. Lineage must ignore these as
  // parent candidates and follow the parent chain through them.
  return { type: "model_change", id, parentId };
}

// Compaction and branch_summary entries are never lineage parents, but they
// are lineage subjects: resolvePiSummaryLineage hashes them into their own
// generation id.
function compactionEntry(
  id: string,
  parentId: string | null,
): SessionEntryLike {
  return { type: "compaction", id, parentId };
}

describe("stablePiGenerationId", () => {
  it("produces a pi-prefixed 24-char hex suffix", () => {
    const id = stablePiGenerationId("pi-conv-1", "entry-a");
    expect(id).toMatch(/^pi-[a-f0-9]{24}$/);
  });

  it("is stable for the same inputs", () => {
    const a = stablePiGenerationId("pi-conv-1", "entry-a");
    const b = stablePiGenerationId("pi-conv-1", "entry-a");
    expect(a).toBe(b);
  });

  it("differs when conversationId differs", () => {
    expect(stablePiGenerationId("pi-conv-1", "entry-a")).not.toBe(
      stablePiGenerationId("pi-conv-2", "entry-a"),
    );
  });

  it("differs when entryId differs", () => {
    expect(stablePiGenerationId("pi-conv-1", "entry-a")).not.toBe(
      stablePiGenerationId("pi-conv-1", "entry-b"),
    );
  });

  it("treats conversationId\\0entryId as distinct from entryId\\0conversationId", () => {
    // Sanity: the NUL-separated hash is order-sensitive so we don't end up
    // colliding when ids and conversation ids share a value space.
    expect(stablePiGenerationId("a", "b")).not.toBe(
      stablePiGenerationId("b", "a"),
    );
  });
});

describe("resolvePiGenerationLineage", () => {
  it("returns empty when conversationId is missing", () => {
    const sm = {
      getBranch: () => [assistantEntry("a", null)],
    };
    expect(
      resolvePiGenerationLineage(sm, sm.getBranch()[0]!.message, undefined),
    ).toEqual({});
  });

  it("returns empty when sessionManager is missing", () => {
    expect(
      resolvePiGenerationLineage(undefined, { role: "assistant" }, "conv"),
    ).toEqual({});
    expect(
      resolvePiGenerationLineage(null, { role: "assistant" }, "conv"),
    ).toEqual({});
  });

  it("returns empty when getBranch is not a function (older runtimes)", () => {
    const sm = { getSessionId: () => "x" } as Record<string, unknown>;
    expect(
      resolvePiGenerationLineage(
        sm as unknown as Parameters<typeof resolvePiGenerationLineage>[0],
        { role: "assistant" },
        "conv",
      ),
    ).toEqual({});
  });

  it("returns empty when getBranch throws", () => {
    const sm = {
      getBranch: () => {
        throw new Error("not a real session");
      },
    };
    expect(
      resolvePiGenerationLineage(sm, { role: "assistant" }, "conv"),
    ).toEqual({});
  });

  it("returns empty when getBranch returns []", () => {
    const sm = { getBranch: () => [] };
    expect(
      resolvePiGenerationLineage(sm, { role: "assistant" }, "conv"),
    ).toEqual({});
  });

  it("returns empty when no assistant entry exists on the branch", () => {
    // First turn during turn_end may briefly precede the assistant
    // entry's persistence; degrade gracefully.
    const sm = { getBranch: () => [userEntry("u1", null)] };
    expect(
      resolvePiGenerationLineage(sm, { role: "assistant" }, "conv"),
    ).toEqual({});
  });

  it("matches the current assistant entry by object identity", () => {
    const assistantMsg = { role: "assistant" };
    const otherAssistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", otherAssistantMsg),
      userEntry("u2", "a1"),
      assistantEntry("a2", "u2", assistantMsg),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, assistantMsg, "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "a2"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("falls back to latest assistant entry when message identity does not match", () => {
    // Defensive: an event payload with a different `Message` object should
    // still produce a deterministic ID for the latest assistant entry.
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", { role: "assistant" }),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(
      sm,
      { role: "assistant" }, // distinct object reference
      "conv",
    );
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "a1"));
    expect(ownParentIds(lineage)).toBeUndefined();
  });

  it("omits parentGenerationIds for the first assistant turn", () => {
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", assistantMsg),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, assistantMsg, "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "a1"));
    expect(ownParentIds(lineage)).toBeUndefined();
  });

  it("links a linear second assistant turn to the first one", () => {
    const second = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", { role: "assistant" }),
      userEntry("u2", "a1"),
      assistantEntry("a2", "u2", second),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, second, "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "a2"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("walks through non-message entries when finding the parent assistant", () => {
    // Pi inserts thinking_level_change / model_change entries in the
    // tree; lineage must skip them and follow parentId.
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", { role: "assistant" }),
      nonMessageEntry("mc1", "a1"),
      userEntry("u2", "mc1"),
      assistantEntry("a2", "u2", assistantMsg),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, assistantMsg, "conv");
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("walks through user entries to the previous assistant on the branch", () => {
    // Multiple user messages between two assistant turns must not become
    // the parent — only the nearest previous assistant entry counts.
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", { role: "assistant" }),
      userEntry("u2", "a1"),
      userEntry("u3", "u2"),
      assistantEntry("a2", "u3", assistantMsg),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, assistantMsg, "conv");
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("picks the assistant entry on the active branch, not the most recent chronological one", () => {
    // Branch scenario: branch from a1 to a new path a1 -> a3, even though
    // the underlying session also contains a2 (an abandoned branch). After
    // navigating, getBranch() only returns the active path (u1 -> a1 -> u3 -> a3).
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1", { role: "assistant" }),
      // Note: a2 belongs to a sibling branch (parent u2 -> a1), it is NOT
      // included in getBranch() for the active path.
      userEntry("u3", "a1"),
      assistantEntry("a3", "u3", assistantMsg),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiGenerationLineage(sm, assistantMsg, "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "a3"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
    // Should not link to the abandoned sibling assistant entry.
    expect(ownParentIds(lineage)).not.toContain(
      stablePiGenerationId("conv", "a2"),
    );
  });
});

describe("resolvePiSummaryLineage", () => {
  it("returns empty when conversationId is missing", () => {
    const sm = { getBranch: () => [compactionEntry("c1", "a1")] };
    expect(resolvePiSummaryLineage(sm, "c1", undefined)).toEqual({});
  });

  it("returns empty when the entry id is empty", () => {
    const sm = { getBranch: () => [compactionEntry("c1", "a1")] };
    expect(resolvePiSummaryLineage(sm, "", "conv")).toEqual({});
  });

  it("keeps the deterministic id when getBranch is unavailable", () => {
    // The id only needs conversationId + entryId, both of which pi hands us
    // directly, so an older runtime without getBranch still gets a stable id.
    const sm = { getSessionId: () => "x" } as Record<string, unknown>;
    expect(
      resolvePiSummaryLineage(
        sm as unknown as Parameters<typeof resolvePiSummaryLineage>[0],
        "c1",
        "conv",
      ),
    ).toEqual({ generationId: stablePiGenerationId("conv", "c1") });
    expect(resolvePiSummaryLineage(undefined, "c1", "conv")).toEqual({
      generationId: stablePiGenerationId("conv", "c1"),
    });
    expect(resolvePiSummaryLineage(null, "c1", "conv")).toEqual({
      generationId: stablePiGenerationId("conv", "c1"),
    });
  });

  it("keeps the deterministic id when getBranch throws or is empty", () => {
    const throwing = {
      getBranch: () => {
        throw new Error("not a real session");
      },
    };
    expect(resolvePiSummaryLineage(throwing, "c1", "conv")).toEqual({
      generationId: stablePiGenerationId("conv", "c1"),
    });
    expect(
      resolvePiSummaryLineage({ getBranch: () => [] }, "c1", "conv"),
    ).toEqual({ generationId: stablePiGenerationId("conv", "c1") });
  });

  it("keeps the deterministic id when the entry is not on the branch", () => {
    const sm = { getBranch: () => [assistantEntry("a1", null)] };
    expect(resolvePiSummaryLineage(sm, "c1", "conv")).toEqual({
      generationId: stablePiGenerationId("conv", "c1"),
    });
  });

  it("parents a compaction entry on the previous assistant entry", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      compactionEntry("c1", "a1"),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiSummaryLineage(sm, "c1", "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "c1"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("walks through an intervening toolResult message entry", () => {
    // Compaction is appended as a child of the current leaf, which after a
    // tool-using turn is the toolResult message, not the assistant message.
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      toolResultEntry("t1", "a1"),
      compactionEntry("c1", "t1"),
    ];
    const sm = { getBranch: () => branch };
    expect(ownParentIds(resolvePiSummaryLineage(sm, "c1", "conv"))).toEqual([
      stablePiGenerationId("conv", "a1"),
    ]);
  });

  it("walks through an intervening model_change entry", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      nonMessageEntry("mc1", "a1"),
      compactionEntry("c1", "mc1"),
    ];
    const sm = { getBranch: () => branch };
    expect(ownParentIds(resolvePiSummaryLineage(sm, "c1", "conv"))).toEqual([
      stablePiGenerationId("conv", "a1"),
    ]);
  });

  it("walks through both a toolResult and a model_change entry", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      toolResultEntry("t1", "a1"),
      nonMessageEntry("m1", "t1"),
      compactionEntry("c1", "m1"),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiSummaryLineage(sm, "c1", "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "c1"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("omits parentGenerationIds when no assistant entry precedes the summary", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      compactionEntry("c1", "u1"),
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiSummaryLineage(sm, "c1", "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "c1"));
    expect(ownParentIds(lineage)).toBeUndefined();
  });

  it("resolves a branch_summary entry the same way", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      { type: "branch_summary", id: "b1", parentId: "a1" },
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiSummaryLineage(sm, "b1", "conv");
    expect(lineage.generationId).toBe(stablePiGenerationId("conv", "b1"));
    expect(ownParentIds(lineage)).toEqual([stablePiGenerationId("conv", "a1")]);
  });

  it("differs from the turn generation id for the same conversation", () => {
    const branch: SessionEntryLike[] = [
      userEntry("u1", null),
      assistantEntry("a1", "u1"),
      compactionEntry("c1", "a1"),
    ];
    const sm = { getBranch: () => branch };
    expect(resolvePiSummaryLineage(sm, "c1", "conv").generationId).not.toBe(
      stablePiGenerationId("conv", "a1"),
    );
  });

  it("reports a trunk parent for a summary taken right after a fork", () => {
    // Compacting before the fork's first turn parents the summary on an
    // inherited assistant entry, whose generation belongs to the trunk.
    const branch: SessionEntryLike[] = [
      userEntry("u1", null, "2020-01-01T00:00:30.000Z"),
      assistantEntry(
        "a1",
        "u1",
        { role: "assistant" },
        "2020-01-01T00:00:30.000Z",
      ),
      { ...compactionEntry("c1", "a1"), timestamp: "2020-01-01T00:02:00.000Z" },
    ];
    const sm = { getBranch: () => branch };
    const lineage = resolvePiSummaryLineage(sm, "c1", "fork-conv", {
      origin: {
        forked: true,
        forkedAt: "2020-01-01T00:01:00.000Z",
        trunkConversationId: "trunk-conv",
        trunkStartedAt: "2020-01-01T00:00:00.000Z",
      },
    });

    expect(lineage.generationId).toBe(stablePiGenerationId("fork-conv", "c1"));
    expect(lineage.parent).toEqual({
      kind: "trunk",
      trunkGenerationId: stablePiGenerationId("trunk-conv", "a1"),
    });
  });
});

describe("resolvePiGenerationLineage on a forked session", () => {
  // `a1` below was copied from the trunk, so it keeps its trunk id and a
  // timestamp older than the fork. See sessionOrigin.ts for that boundary.
  const TRUNK = "trunk-conv";
  const FORK = "fork-conv";
  const TRUNK_STARTED_AT = "2020-01-01T00:00:00.000Z";
  const BEFORE_FORK = "2020-01-01T00:00:30.000Z";
  const FORKED_AT = "2020-01-01T00:01:00.000Z";
  const AFTER_FORK = "2020-01-01T00:02:00.000Z";

  const forkOrigin: SessionOrigin = {
    forked: true,
    forkedAt: FORKED_AT,
    trunkConversationId: TRUNK,
    trunkStartedAt: TRUNK_STARTED_AT,
  };

  /**
   * Inherited assistant `a1`, then the fork's own `u2` -> `a2`. `parentAt`
   * stamps the inherited turn; `null` leaves it unstamped.
   */
  function forkBranch(
    assistantMsg: unknown,
    parentAt: string | null = BEFORE_FORK,
  ): SessionEntryLike[] {
    const at = parentAt ?? undefined;
    return [
      userEntry("u1", null, at),
      assistantEntry("a1", "u1", { role: "assistant" }, at),
      userEntry("u2", "a1", AFTER_FORK),
      assistantEntry("a2", "u2", assistantMsg, AFTER_FORK),
    ];
  }

  it.each([
    {
      name: "the parent was copied from the trunk",
      options: { origin: forkOrigin },
      parentAt: BEFORE_FORK,
      wantParent: {
        kind: "trunk",
        trunkGenerationId: stablePiGenerationId(TRUNK, "a1"),
      },
    },
    {
      name: "the parent is stamped exactly at the fork instant",
      options: { origin: forkOrigin },
      parentAt: FORKED_AT,
      wantParent: {
        kind: "trunk",
        trunkGenerationId: stablePiGenerationId(TRUNK, "a1"),
      },
    },
    {
      name: "the trunk conversation id is unknown",
      options: { origin: { forked: true, forkedAt: FORKED_AT } },
      parentAt: BEFORE_FORK,
      wantParent: { kind: "trunk", trunkGenerationId: undefined },
    },
    {
      name: "the trunk's own start time is unknown",
      options: {
        origin: {
          forked: true,
          forkedAt: FORKED_AT,
          trunkConversationId: TRUNK,
        },
      },
      parentAt: BEFORE_FORK,
      wantParent: { kind: "trunk", trunkGenerationId: undefined },
    },
    {
      name: "the parent predates the trunk session, as on a fork of a fork",
      // The trunk inherited `a1` from an older session and never exported
      // it, so no generation for it exists under the trunk either.
      options: {
        origin: { ...forkOrigin, trunkStartedAt: "2020-01-01T00:00:45.000Z" },
      },
      parentAt: BEFORE_FORK,
      wantParent: { kind: "trunk", trunkGenerationId: undefined },
    },
    {
      name: "the fork header carries no timestamp",
      options: { origin: { forked: true, trunkConversationId: TRUNK } },
      parentAt: BEFORE_FORK,
      wantParent: { kind: "unknown" },
    },
    {
      name: "the fork header timestamp is unparseable",
      options: {
        origin: {
          forked: true,
          forkedAt: "whenever",
          trunkConversationId: TRUNK,
        },
      },
      parentAt: BEFORE_FORK,
      wantParent: { kind: "unknown" },
    },
    {
      name: "the parent entry carries no timestamp",
      options: { origin: forkOrigin },
      parentAt: null,
      wantParent: { kind: "unknown" },
    },
    {
      name: "the session is a --session continuation, not a fork",
      // The generation for `a1` exists under this very conversation id.
      // Suppressing here would split one conversation into two roots.
      options: { origin: NOT_FORKED },
      parentAt: BEFORE_FORK,
      wantParent: {
        kind: "own",
        generationIds: [stablePiGenerationId(FORK, "a1")],
      },
    },
    {
      name: "no options are passed at all",
      options: undefined,
      parentAt: BEFORE_FORK,
      wantParent: {
        kind: "own",
        generationIds: [stablePiGenerationId(FORK, "a1")],
      },
    },
  ])("resolves the parent as $wantParent.kind when $name", ({
    options,
    parentAt,
    wantParent,
  }) => {
    const assistantMsg = { role: "assistant" };
    const sm = { getBranch: () => forkBranch(assistantMsg, parentAt) };

    const lineage = resolvePiGenerationLineage(sm, assistantMsg, FORK, options);

    expect(lineage.generationId).toBe(stablePiGenerationId(FORK, "a2"));
    expect(lineage.parent).toEqual(wantParent);
  });

  it("emits the edge when the parent was added after the fork", () => {
    // The fork's second turn. Nothing here depends on what this process
    // exported, so a process that resumed the fork resolves it the same way.
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      ...forkBranch({ role: "assistant" }),
      userEntry("u3", "a2", "2020-01-01T00:03:00.000Z"),
      assistantEntry("a3", "u3", assistantMsg, "2020-01-01T00:03:01.000Z"),
    ];
    const sm = { getBranch: () => branch };

    const lineage = resolvePiGenerationLineage(sm, assistantMsg, FORK, {
      origin: forkOrigin,
    });

    expect(lineage.generationId).toBe(stablePiGenerationId(FORK, "a3"));
    expect(lineage.parent).toEqual({
      kind: "own",
      generationIds: [stablePiGenerationId(FORK, "a2")],
    });
  });

  it("reports no parent for a fork taken before the first turn", () => {
    // A fork with nothing inherited: the first generation is a genuine root
    // and there is no trunk link.
    const assistantMsg = { role: "assistant" };
    const branch: SessionEntryLike[] = [
      userEntry("u1", null, AFTER_FORK),
      assistantEntry("a1", "u1", assistantMsg, AFTER_FORK),
    ];
    const sm = { getBranch: () => branch };

    const lineage = resolvePiGenerationLineage(sm, assistantMsg, FORK, {
      origin: forkOrigin,
    });

    expect(lineage.generationId).toBe(stablePiGenerationId(FORK, "a1"));
    expect(lineage.parent).toBeUndefined();
  });
});
