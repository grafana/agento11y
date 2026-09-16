import { describe, expect, it } from "vitest";
import type { DshContentBlock, DshStreamChunk } from "./dsh.js";
import { StreamCapture } from "./streamCapture.js";

function captureChunks(
  chunks: DshStreamChunk[],
  now: () => number = () => 1,
): StreamCapture {
  const capture = new StreamCapture(now);
  for (const chunk of chunks) capture.push(chunk);
  return capture;
}

interface AssemblyCase {
  name: string;
  chunks: DshStreamChunk[];
  expected: DshContentBlock[];
}

const ASSEMBLY_CASES: AssemblyCase[] = [
  {
    name: "keeps first-seen order for non-monotonic interleaved delta-only indexes",
    chunks: [
      { type: "text-delta", index: 8, text: "hel" },
      { type: "reasoning-delta", index: 2, text: "plan" },
      { type: "tool-call-delta", index: 5, id: "c5", argumentsDelta: "{" },
      { type: "text-delta", index: 8, text: "lo" },
      {
        type: "tool-call-delta",
        index: 5,
        id: "c5",
        name: "run",
        argumentsDelta: "}",
      },
      { type: "reasoning-delta", index: 2, text: "ning" },
    ],
    expected: [
      { type: "text", text: "hello" },
      { type: "reasoning", text: "planning" },
      { type: "tool-call", id: "c5", name: "run", arguments: "{}" },
    ],
  },
  {
    name: "accepts block-end-only streams and preserves normal blocks",
    chunks: [
      {
        type: "block-end",
        index: 4,
        block: { type: "image", id: "image-1" },
      },
      {
        type: "block-end",
        index: 1,
        block: { type: "text", text: "done" },
      },
    ],
    expected: [
      { type: "image", id: "image-1" },
      { type: "text", text: "done" },
    ],
  },
  {
    name: "uses the first block-end and ignores every later delta and close",
    chunks: [
      { type: "block-start", index: 3, blockType: "text" },
      { type: "text-delta", index: 3, text: "streamed" },
      {
        type: "block-end",
        index: 3,
        block: { type: "text", text: "first close" },
      },
      { type: "text-delta", index: 3, text: " text" },
      { type: "reasoning-delta", index: 3, text: " reasoning" },
      {
        type: "tool-call-delta",
        index: 3,
        id: "late",
        name: "late",
        argumentsDelta: "{}",
      },
      {
        type: "block-end",
        index: 3,
        block: { type: "reasoning", text: "second close" },
      },
    ],
    expected: [{ type: "text", text: "first close" }],
  },
  {
    name: "does not reset a partial on a duplicate block-start",
    chunks: [
      { type: "block-start", index: 0, blockType: "text" },
      { type: "text-delta", index: 0, text: "one" },
      { type: "block-start", index: 0, blockType: "reasoning" },
      { type: "text-delta", index: 0, text: " two" },
    ],
    expected: [{ type: "text", text: "one two" }],
  },
  {
    name: "omits an unknown open block without losing known blocks",
    chunks: [
      { type: "block-start", index: 9, blockType: "future-block" },
      { type: "block-start", index: 1, blockType: "text" },
      { type: "text-delta", index: 1, text: "known" },
    ],
    expected: [{ type: "text", text: "known" }],
  },
  {
    name: "drops open and closed tool calls after a max-tokens finish",
    chunks: [
      {
        type: "block-end",
        index: 7,
        block: {
          type: "tool-call",
          id: "closed",
          name: "read",
          arguments: "{}",
        },
      },
      { type: "text-delta", index: 2, text: "partial" },
      {
        type: "tool-call-delta",
        index: 5,
        id: "open",
        name: "write",
        argumentsDelta: "{",
      },
      { type: "reasoning-delta", index: 1, text: "still useful" },
      { type: "finish", reason: { kind: "max-tokens" } },
    ],
    expected: [
      { type: "text", text: "partial" },
      { type: "reasoning", text: "still useful" },
    ],
  },
];

describe("StreamCapture block assembly", () => {
  it.each(ASSEMBLY_CASES)("$name", ({ chunks, expected }) => {
    expect(captureChunks(chunks).blocks()).toEqual(expected);
  });
});

describe("StreamCapture interrupted blocks", () => {
  it("keeps only non-whitespace text and reasoning in first-seen order", () => {
    const capture = captureChunks([
      { type: "reasoning-delta", index: 8, text: " plan " },
      {
        type: "block-end",
        index: 2,
        block: { type: "text", text: "answer" },
      },
      { type: "text-delta", index: 5, text: " \n\t" },
      {
        type: "block-end",
        index: 3,
        block: { type: "reasoning", text: "   " },
      },
      {
        type: "block-end",
        index: 4,
        block: {
          type: "tool-call",
          id: "closed",
          name: "read",
          arguments: "{}",
        },
      },
      {
        type: "tool-call-delta",
        index: 6,
        id: "open",
        name: "write",
        argumentsDelta: "{",
      },
      { type: "block-start", index: 7, blockType: "future-block" },
      {
        type: "block-end",
        index: 1,
        block: { type: "image", id: "image-1" },
      },
    ]);

    expect(capture.interruptedBlocks()).toEqual([
      { type: "reasoning", text: " plan " },
      { type: "text", text: "answer" },
    ]);
  });

  it("does not apply interrupted filtering to normal output", () => {
    const capture = captureChunks([
      { type: "text-delta", index: 0, text: "  " },
      { type: "reasoning-delta", index: 1, text: "" },
      {
        type: "tool-call-delta",
        index: 2,
        id: "c2",
        argumentsDelta: "{",
      },
    ]);

    expect(capture.blocks()).toEqual([
      { type: "text", text: "  " },
      { type: "reasoning", text: "" },
      { type: "tool-call", id: "c2", name: "", arguments: "{" },
    ]);
    expect(capture.interruptedBlocks()).toEqual([]);
  });
});

interface FirstTokenCase {
  name: string;
  chunk: DshStreamChunk;
  isToken: boolean;
}

const FIRST_TOKEN_CASES: FirstTokenCase[] = [
  {
    name: "non-empty text delta",
    chunk: { type: "text-delta", index: 0, text: " " },
    isToken: true,
  },
  {
    name: "empty text delta",
    chunk: { type: "text-delta", index: 0, text: "" },
    isToken: false,
  },
  {
    name: "non-empty reasoning delta",
    chunk: { type: "reasoning-delta", index: 0, text: "r" },
    isToken: true,
  },
  {
    name: "empty reasoning delta",
    chunk: { type: "reasoning-delta", index: 0, text: "" },
    isToken: false,
  },
  {
    name: "non-empty tool arguments delta",
    chunk: {
      type: "tool-call-delta",
      index: 0,
      id: "c0",
      argumentsDelta: "{",
    },
    isToken: true,
  },
  {
    name: "present tool name",
    chunk: {
      type: "tool-call-delta",
      index: 0,
      id: "c0",
      name: "read",
      argumentsDelta: "",
    },
    isToken: true,
  },
  {
    name: "present empty tool name",
    chunk: {
      type: "tool-call-delta",
      index: 0,
      id: "c0",
      name: "",
      argumentsDelta: "",
    },
    isToken: true,
  },
  {
    name: "empty unnamed tool delta",
    chunk: {
      type: "tool-call-delta",
      index: 0,
      id: "c0",
      argumentsDelta: "",
    },
    isToken: false,
  },
  {
    name: "block start",
    chunk: { type: "block-start", index: 0, blockType: "text" },
    isToken: false,
  },
  {
    name: "non-empty block end",
    chunk: {
      type: "block-end",
      index: 0,
      block: { type: "text", text: "complete" },
    },
    isToken: false,
  },
  {
    name: "usage",
    chunk: { type: "usage", usage: { inputTokens: 1, outputTokens: 1 } },
    isToken: false,
  },
  {
    name: "finish",
    chunk: { type: "finish", reason: { kind: "stop" } },
    isToken: false,
  },
];

describe("StreamCapture first-token timing", () => {
  it.each(FIRST_TOKEN_CASES)("classifies $name", ({ chunk, isToken }) => {
    let calls = 0;
    const capture = new StreamCapture(() => {
      calls += 1;
      return 1234;
    });

    capture.push(chunk);

    expect(capture.firstTokenAt).toBe(isToken ? 1234 : undefined);
    expect(calls).toBe(isToken ? 1 : 0);
  });

  it("keeps the first qualifying delta time and accepts an observed time", () => {
    let calls = 0;
    const capture = new StreamCapture(() => {
      calls += 1;
      return 9999;
    });

    capture.push({ type: "text-delta", index: 0, text: "" }, 10);
    capture.push({ type: "text-delta", index: 0, text: "first" }, 20);
    capture.push({ type: "reasoning-delta", index: 1, text: "later" }, 30);

    expect(capture.firstTokenAt).toBe(20);
    expect(calls).toBe(0);
  });
});

describe("StreamCapture terminal metadata", () => {
  it("starts without a synthetic finish or usage", () => {
    const capture = new StreamCapture();

    expect(capture.finish).toBeUndefined();
    expect(capture.usage).toBeUndefined();
  });

  it("exposes the latest usage and actual latest finish", () => {
    const capture = captureChunks([
      { type: "usage", usage: { inputTokens: 1, outputTokens: 2 } },
      {
        type: "usage",
        usage: {
          inputTokens: 3,
          outputTokens: 4,
          cacheReadTokens: 5,
        },
      },
      { type: "finish", reason: { kind: "max-tokens" } },
      { type: "finish", reason: { kind: "provider-stop" } },
    ]);

    expect(capture.usage).toEqual({
      inputTokens: 3,
      outputTokens: 4,
      cacheReadTokens: 5,
    });
    expect(capture.finish).toEqual({ kind: "provider-stop" });
  });
});
