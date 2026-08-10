// Shared factories and event helpers for hook tests. Each test file keeps its
// own vi.mock calls because Vitest hoists them.

import type { AssistantMessage, StepFinishPart } from "@opencode-ai/sdk";
import { vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import type { createAgento11yHooks, OpencodeEventType } from "./hooks.js";

export type TestHooks = NonNullable<
  Awaited<ReturnType<typeof createAgento11yHooks>>
>;

/**
 * Dispatches an event through the plugin's `event` hook. `type` is constrained
 * to the `@opencode-ai/sdk` `Event` union, so a name outside opencode's
 * declared event surface fails to compile.
 */
export async function emitEvent(
  hooks: TestHooks,
  type: OpencodeEventType,
  properties: unknown = {},
): Promise<void> {
  await hooks.event({ event: { type, properties } });
}

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
    // Allows by default, so a test that enables guards only states the verdict
    // it cares about. Guard paths that need a real HTTP round trip live in
    // hooks.guard.test.ts, which builds a client instead of mocking one.
    evaluateHook: vi.fn(async () => ({ action: "allow" })),
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
    app: { log: vi.fn(async () => ({ data: true })) },
    tui: { showToast: vi.fn(async () => ({ data: true })) },
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

export function assistantMessage(
  sessionID: string,
  messageID: string,
  overrides: Partial<AssistantMessage> = {},
): AssistantMessage {
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
    ...overrides,
  };
}

/**
 * An assistant message mid-turn: opencode sets `finish` at every `step-finish`
 * and only sets `time.completed` when the turn actually ends, so this shape is
 * what the plugin sees between steps.
 */
export function inFlightAssistantMessage(
  sessionID: string,
  messageID: string,
  overrides: Omit<Partial<AssistantMessage>, "time"> = {},
): AssistantMessage {
  return assistantMessage(sessionID, messageID, {
    ...overrides,
    time: { created: 1_700_000_001_000 },
  });
}

/**
 * A `step-finish` part carrying one provider step's usage. Token fields
 * default to 0 so a case only states the numbers it cares about.
 */
export function stepFinishPart(
  sessionID: string,
  messageID: string,
  tokens: {
    input?: number;
    output?: number;
    reasoning?: number;
    cache?: { read?: number; write?: number };
  } = {},
  overrides: Omit<Partial<StepFinishPart>, "tokens"> = {},
): StepFinishPart {
  return {
    id: `prt-step-${messageID}`,
    sessionID,
    messageID,
    type: "step-finish",
    reason: "tool-calls",
    cost: 0.002,
    tokens: {
      input: tokens.input ?? 0,
      output: tokens.output ?? 0,
      reasoning: tokens.reasoning ?? 0,
      cache: {
        read: tokens.cache?.read ?? 0,
        write: tokens.cache?.write ?? 0,
      },
    },
    ...overrides,
  };
}

export async function emitMessageUpdated(
  hooks: TestHooks,
  msg: unknown,
): Promise<void> {
  await emitEvent(hooks, "message.updated", { info: msg });
}

export async function emitPartUpdated(
  hooks: TestHooks,
  part: unknown,
): Promise<void> {
  await emitEvent(hooks, "message.part.updated", { part });
}

export async function emitSessionDeleted(
  hooks: TestHooks,
  sessionID: string,
): Promise<void> {
  await emitEvent(hooks, "session.deleted", { info: { id: sessionID } });
}

/** Emit a session lifecycle event carrying an arbitrary `Session` subset. */
export async function emitSessionEvent(
  hooks: TestHooks,
  type: "session.created" | "session.updated",
  info: Record<string, unknown>,
): Promise<void> {
  await emitEvent(hooks, type, { info });
}

export async function emitSessionCreated(
  hooks: TestHooks,
  id: string,
  parentID?: string,
): Promise<void> {
  await emitSessionEvent(hooks, "session.created", { id, parentID });
}

/** Dispatches opencode's instance-teardown event. */
export async function emitServerInstanceDisposed(
  hooks: TestHooks,
): Promise<void> {
  await emitEvent(hooks, "server.instance.disposed", { directory: "/repo" });
}
