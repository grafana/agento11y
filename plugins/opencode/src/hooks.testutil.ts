// Shared factories and event helpers for hook tests. Each test file keeps its
// own vi.mock calls because Vitest hoists them.

import { vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import type { createAgento11yHooks } from "./hooks.js";

export type TestHooks = NonNullable<
  Awaited<ReturnType<typeof createAgento11yHooks>>
>;

export type CapturedGeneration = {
  seed: any;
  firstTokenAt: Date | undefined;
  result: unknown;
  callError: unknown;
};

// The explicit return type avoids TS2883 when declarations are generated.
export function makeAgento11yMock(): {
  sigil: any;
  generations: CapturedGeneration[];
  startStreamingGeneration: any;
  startGeneration: any;
} {
  const generations: CapturedGeneration[] = [];
  const startStreamingGeneration = vi.fn(async (seed: any, run: any) => {
    const entry: CapturedGeneration = {
      seed,
      firstTokenAt: undefined,
      result: undefined,
      callError: undefined,
    };
    generations.push(entry);
    await run({
      setResult: (r: unknown) => {
        entry.result = r;
      },
      setCallError: (e: unknown) => {
        entry.callError = e;
      },
      setFirstTokenAt: (d: Date) => {
        entry.firstTokenAt = d;
      },
      setCacheDiagnostics: vi.fn(),
      end: vi.fn(),
      getError: () => undefined,
    });
  });
  const startGeneration = vi.fn();
  const sigil = {
    startStreamingGeneration,
    startGeneration,
    startToolExecution: vi.fn(() => ({
      setResult: vi.fn(),
      setCallError: vi.fn(),
      end: vi.fn(),
      getError: vi.fn(),
    })),
    flush: vi.fn(async () => {}),
    shutdown: vi.fn(async () => {}),
  };
  return { sigil, generations, startStreamingGeneration, startGeneration };
}

export function makeOpencodeClient(parts: any[] = []) {
  return {
    session: { message: vi.fn(async () => ({ data: { parts } })) },
  } as any;
}

export function baseConfig(
  overrides: Partial<Agento11yOpencodeConfig> = {},
): Agento11yOpencodeConfig {
  return {
    endpoint: "http://127.0.0.1:1/api/v1/generations:export",
    auth: { mode: "none" },
    agentName: "opencode",
    agentVersion: "test-version",
    contentCapture: "full",
    debug: false,
    ...overrides,
  };
}

export function assistantMessage(sessionID: string, messageID: string) {
  return {
    id: messageID,
    sessionID,
    role: "assistant",
    time: { created: 1_700_000_001_000, completed: 1_700_000_002_500 },
    parentID: "user-1",
    modelID: "claude-sonnet-4",
    providerID: "anthropic",
    mode: "build",
    path: { cwd: "/repo", root: "/repo" },
    cost: 0.001,
    tokens: {
      input: 10,
      output: 5,
      reasoning: 0,
      cache: { read: 0, write: 0 },
    },
    finish: "end_turn",
  } as const;
}

export async function emitMessageUpdated(
  hooks: TestHooks,
  msg: unknown,
): Promise<void> {
  await hooks.event({
    event: { type: "message.updated", properties: { info: msg } },
  });
}

export async function emitPartUpdated(
  hooks: TestHooks,
  part: unknown,
): Promise<void> {
  await hooks.event({
    event: { type: "message.part.updated", properties: { part } },
  });
}

export async function emitSessionDeleted(
  hooks: TestHooks,
  sessionID: string,
): Promise<void> {
  await hooks.event({
    event: { type: "session.deleted", properties: { info: { id: sessionID } } },
  });
}

export async function emitSessionCreated(
  hooks: TestHooks,
  id: string,
  parentID?: string,
): Promise<void> {
  await hooks.event({
    event: { type: "session.created", properties: { info: { id, parentID } } },
  });
}
