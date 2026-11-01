import { describe, expect, it } from "vitest";
import type { PiAssistantMessage, PiToolResult } from "./mappers.js";
import {
  ToolExecutions,
  type ToolObservation,
  toolParents,
} from "./toolExecutions.js";

const identity = { toolCallId: "reused", toolName: "bash" };
const proposal = { command: "model proposal" };
const prepared = { command: "prepared input", extra: true };
const mutated = { command: "tool mutated input" };

function message(args: Record<string, unknown> = proposal): PiAssistantMessage {
  return {
    role: "assistant",
    content: [
      {
        type: "toolCall",
        id: identity.toolCallId,
        name: identity.toolName,
        arguments: args,
      },
    ],
  } as PiAssistantMessage;
}

function result(): PiToolResult {
  return {
    role: "toolResult",
    ...identity,
    content: [{ type: "text", text: "finalized transcript" }],
    isError: false,
    timestamp: 100,
  };
}

function node(
  id: string,
  occurrence: number,
  parent?: unknown,
): ToolObservation {
  const state = new ToolExecutions();
  state.start({ toolCallId: id, toolName: "bash", parentToolCallId: parent });
  return { ...state.observations[0]!, occurrence };
}

describe("tool observations", () => {
  it.each([
    {
      name: "unchanged",
      input: proposal,
      post: proposal,
      transformed: false,
      safe: true,
    },
    {
      name: "prepared",
      input: prepared,
      post: prepared,
      transformed: false,
      safe: false,
    },
    {
      name: "transformed",
      input: prepared,
      post: prepared,
      transformed: true,
      safe: false,
    },
    {
      name: "tool mutation",
      input: proposal,
      post: mutated,
      transformed: false,
      safe: false,
    },
    {
      name: "later rewrite",
      input: prepared,
      post: mutated,
      transformed: true,
      safe: false,
    },
    {
      name: "blocked",
      input: proposal,
      post: undefined,
      transformed: true,
      safe: false,
    },
    {
      name: "invalid",
      input: undefined,
      post: undefined,
      transformed: false,
      safe: false,
    },
  ])("keeps proposal, guard-stage and post-execution data separate: $name", ({
    input,
    post,
    transformed,
    safe,
  }) => {
    const state = new ToolExecutions();
    const original = message();
    state.start({ ...identity, args: proposal }, 10);
    if (input) state.guardInput({ ...identity, input }, [], "/tmp");
    if (transformed) state.guardChanged(identity, input);
    if (post) state.resultInput({ ...identity, input: post }, [], "/tmp");
    state.end(
      {
        ...identity,
        result: { content: [{ type: "text", text: "live final result" }] },
        isError: !post,
        durationMs: post ? 2.5 : undefined,
      },
      [],
      100,
    );
    const item = state.completed()[0]!;
    expect(item.proposalJSON).toBe(JSON.stringify(proposal));
    expect(item.guardInputJSON).toBe(input ? JSON.stringify(input) : undefined);
    expect(item.postExecutionInputJSON).toBe(
      post ? JSON.stringify(post) : undefined,
    );
    expect(item.postExecutionInputSafe).toBe(
      !!post && (!transformed || post === input),
    );
    expect(item.resultText).toBe("live final result");
    expect(item.durationMs).toBe(post ? 2.5 : undefined);
    expect(state.safeMessage(original).content[0]).toMatchObject({
      arguments: safe ? proposal : undefined,
    });
    expect(original.content[0]).toMatchObject({ arguments: proposal });
    expect(state.safeResults([result()])[0]).toMatchObject({
      content: [{ type: "text", text: "finalized transcript" }],
      isError: false,
    });
  });

  it.each([
    "reset",
    "same turn",
  ] as const)("does not overwrite reused call IDs: %s", (boundary) => {
    const state = new ToolExecutions();
    for (const args of [proposal, mutated]) {
      if (boundary === "reset") state.reset();
      state.start({ ...identity, args }, 10);
      state.guardInput({ ...identity, input: args }, [], "/tmp");
      state.resultInput({ ...identity, input: args }, [], "/tmp");
      state.end(
        {
          ...identity,
          isError: false,
          result: { content: [{ type: "text", text: args.command }] },
        },
        [],
        20,
      );
    }
    const items = state.completed();
    expect(items.map((item) => item.proposalJSON)).toEqual(
      (boundary === "reset" ? [mutated] : [proposal, mutated]).map((value) =>
        JSON.stringify(value),
      ),
    );
    expect(state.safeMessage(message(mutated)).content[0]).toMatchObject({
      arguments: boundary === "reset" ? mutated : undefined,
    });
    expect(state.safeResults([result()])[0]!.content).toEqual(result().content);
  });

  it.each([
    "blocked",
    "unapplied transform",
  ])("omits unauthorized post-execution input after %s", (outcome) => {
    const state = new ToolExecutions();
    state.start({ ...identity, args: proposal });
    state.guardInput({ ...identity, input: proposal }, [], "/tmp");
    state.guardChanged(identity, outcome === "blocked" ? undefined : prepared);
    state.guardInput({ ...identity, input: proposal }, [], "/tmp", true);
    state.resultInput({ ...identity, input: proposal }, [], "/tmp");
    state.end({ ...identity, isError: false }, []);
    expect(state.completed()[0]).toMatchObject({
      proposalSafe: false,
      guardInputJSON: JSON.stringify(proposal),
      postExecutionInputJSON: JSON.stringify(proposal),
      postExecutionInputSafe: false,
    });
    expect(state.safeMessage(message()).content[0]).toMatchObject({
      arguments: undefined,
    });
  });

  it("omits ambiguous proposals but retains finalized results with duplicate IDs", () => {
    const state = new ToolExecutions();
    state.start({ ...identity, args: proposal });
    state.guardInput({ ...identity, input: proposal }, [], "/tmp");
    state.resultInput({ ...identity, input: proposal }, [], "/tmp");
    state.end(
      {
        ...identity,
        isError: false,
        result: { content: [{ type: "text", text: "live" }] },
      },
      [],
    );
    const original = message();
    original.content.push(original.content[0]!);
    expect(
      state
        .safeMessage(original)
        .content.every(
          (block) => block.type === "toolCall" && block.arguments === undefined,
        ),
    ).toBe(true);
    expect(state.safeResults([result(), result()])).toEqual([
      result(),
      result(),
    ]);
  });

  it.each([
    null,
    0,
    {},
    "",
  ])("keeps malformed-parent observations as roots: %s", (parentToolCallId) => {
    const state = new ToolExecutions();
    const call = { ...identity, parentToolCallId };
    state.start({ ...call, args: proposal });
    state.guardInput({ ...call, input: proposal }, [], "/tmp");
    state.resultInput({ ...call, input: proposal }, [], "/tmp");
    state.end({ ...call, isError: false }, []);
    expect(state.completed()).toHaveLength(1);
    expect(toolParents(state.completed()).size).toBe(0);
  });

  it("does not assign ambiguous concurrent events to either occurrence", () => {
    const state = new ToolExecutions();
    state.start({ ...identity, args: proposal });
    state.start({ ...identity, args: mutated });
    state.guardInput({ ...identity, input: prepared }, [], "/tmp");
    state.resultInput({ ...identity, input: prepared }, [], "/tmp");
    state.end({ ...identity, isError: false }, []);
    expect(state.completed()).toEqual([]);
    expect(
      state.observations.every(
        (item) => item.postExecutionInputJSON === undefined,
      ),
    ).toBe(true);
    expect(state.safeMessage(message()).content[0]).toMatchObject({
      arguments: undefined,
    });
  });

  it("omits unserializable proposals and inputs without using transcript bodies", () => {
    const state = new ToolExecutions();
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    state.start({ ...identity, args: cyclic });
    state.guardInput({ ...identity, input: cyclic }, [], "/tmp");
    state.resultInput({ ...identity, input: cyclic }, [], "/tmp");
    state.end({ ...identity, isError: false }, []);
    expect(state.completed()[0]).toMatchObject({
      proposalJSON: undefined,
      postExecutionInputJSON: undefined,
      proposalSafe: false,
    });
    expect(state.safeMessage(message()).content[0]).toMatchObject({
      arguments: undefined,
    });
  });

  it("snapshots input and final content without retaining mutable objects or result details", () => {
    const state = new ToolExecutions();
    const args = { command: "before" };
    state.start({ ...identity, args });
    args.command = "prepared";
    state.guardInput({ ...identity, input: args }, [], "/tmp");
    args.command = "after execution";
    state.resultInput({ ...identity, input: args }, [], "/tmp");
    const content = [
      { type: "text", text: "final" },
      { type: "image", text: "image data" },
    ];
    state.end(
      {
        ...identity,
        isError: false,
        result: { content, details: { secret: "private" } },
      },
      [],
    );
    args.command = "late";
    content[0]!.text = "late";
    expect(state.completed()[0]).toMatchObject({
      proposalJSON: '{"command":"before"}',
      guardInputJSON: '{"command":"prepared"}',
      postExecutionInputJSON: '{"command":"after execution"}',
      resultText: "final",
    });
  });

  it.each([
    undefined,
    -1,
    Number.NaN,
    Number.POSITIVE_INFINITY,
    0,
    1.5,
  ])("accepts only finite nonnegative host durations: %s", (durationMs) => {
    const state = new ToolExecutions();
    state.start(identity);
    state.end({ ...identity, isError: false, durationMs }, []);
    expect(state.completed()[0]!.durationMs).toBe(
      typeof durationMs === "number" &&
        Number.isFinite(durationMs) &&
        durationMs >= 0
        ? durationMs
        : undefined,
    );
  });

  it.each([
    undefined,
    null,
    { content: null },
    { content: [{ type: "image", data: "secret" }] },
  ])("uses finalized transcript bodies even without live text: %s", (observed) => {
    const state = new ToolExecutions();
    state.start(identity);
    state.end({ ...identity, result: observed, isError: true }, []);
    expect(state.safeResults([result()])[0]).toEqual(result());
  });
});

describe("finalized tool results", () => {
  it.each([
    { text: "final replacement", isError: false },
    { text: "", isError: false },
    { text: "final error", isError: true },
    { text: "glc_abcdefghijklmnopqrstuvwxyz", isError: false },
  ])("preserves message finalization: $text, error=$isError", ({
    text,
    isError,
  }) => {
    const state = new ToolExecutions();
    state.start(identity);
    state.end(
      {
        ...identity,
        result: {
          content: [{ type: "text", text: "removed private content" }],
        },
        isError: !isError,
      },
      [],
    );
    const finalized = {
      ...result(),
      content: [{ type: "text", text }],
      isError,
    };
    const original = structuredClone(finalized);
    expect(state.safeResults([finalized])[0]).toEqual({
      ...finalized,
      content: [
        {
          type: "text",
          text: text.startsWith("glc_")
            ? "[REDACTED:grafana-cloud-token]"
            : text,
        },
      ],
    });
    expect(finalized).toEqual(original);
    expect(state.completed()[0]!.resultText).toBe("removed private content");
  });
});

describe("observed tool parents", () => {
  it.each([
    {
      name: "siblings and grandchild",
      nodes: [
        node("p", 0),
        node("a", 1, "p"),
        node("b", 2, "p"),
        node("g", 3, "a"),
      ],
      edges: [
        ["a", "p"],
        ["b", "p"],
        ["g", "a"],
      ],
    },
    { name: "missing parent", nodes: [node("a", 0, "absent")], edges: [] },
    {
      name: "no prefix inference",
      nodes: [node("a", 0), node("a/1", 1)],
      edges: [],
    },
    {
      name: "duplicate parent IDs",
      nodes: [node("p", 0), node("p", 1), node("a", 2, "p")],
      edges: [],
    },
    {
      name: "duplicate child IDs",
      nodes: [node("p", 0), node("a", 1, "p"), node("a", 2, "p")],
      edges: [],
    },
    { name: "self parent", nodes: [node("p", 0, "p")], edges: [] },
    { name: "cycle", nodes: [node("p", 0, "a"), node("a", 1, "p")], edges: [] },
    {
      name: "parent starts later",
      nodes: [node("a", 0, "p"), node("p", 1)],
      edges: [],
    },
    ...[null, 1, {}, ""].map((parent) => ({
      name: `malformed ${JSON.stringify(parent)}`,
      nodes: [node("p", 0), node("a", 1, parent)],
      edges: [],
    })),
  ])("uses only unique earlier explicit parents: $name", ({ nodes, edges }) => {
    expect(
      [...toolParents(nodes)].map(([child, parent]) => [
        child.toolCallId,
        parent.toolCallId,
      ]),
    ).toEqual(edges);
  });
});
