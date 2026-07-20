// Tests system prompt capture via `experimental.chat.system.transform`,
// name-only tool definitions, and host version capture.

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
  emitSessionDeleted,
  makeAgento11yMock,
  makeOpencodeClient,
  type TestHooks,
} from "./hooks.testutil.js";

function userMessage(sessionID: string) {
  return {
    id: "user-1",
    sessionID,
    role: "user",
    time: { created: 1_700_000_000_000 },
    agent: "build",
    model: { providerID: "anthropic", modelID: "claude-sonnet-4" },
    system: "legacy system",
    tools: { legacybash: true, disabled: false },
  } as any;
}

async function makeHooks(
  config = baseConfig(),
  client = makeOpencodeClient(),
): Promise<TestHooks> {
  const hooks = await createAgento11yHooks(config, client);
  if (!hooks) throw new Error("expected hooks");
  return hooks;
}

function emitTransform(
  hooks: TestHooks,
  sessionID: string,
  system: string[],
  modelID = "claude-sonnet-4",
) {
  hooks.systemTransform({ sessionID, model: { id: modelID } }, { system });
}

describe("opencode system prompt capture", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("prefers the transform prompt over the legacy chat.message override", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [] },
    );
    emitTransform(hooks, "sess-1", [
      "composed prompt",
      "<env>cwd: /repo</env>",
    ]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe(
      "composed prompt\n<env>cwd: /repo</env>",
    );
  });

  it("uses the legacy chat.message override when no transform fired", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [] },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe("legacy system");
  });

  it("ignores a transform without a session ID", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    hooks.systemTransform(
      { model: { id: "claude-sonnet-4" } },
      { system: ["no session"] },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBeUndefined();
  });

  it("ignores a transform whose model differs from the chat model", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [] },
    );
    emitTransform(hooks, "sess-1", ["main prompt"]);
    // Concurrent title request on the small model shares the session ID.
    emitTransform(hooks, "sess-1", ["title prompt"], "small-model");
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe("main prompt");
  });

  it("accepts a transform when the session model is unknown", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    emitTransform(hooks, "sess-1", ["prompt without chat.message"]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe(
      "prompt without chat.message",
    );
  });

  it("keeps the latest transform when several fire in one turn", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    emitTransform(hooks, "sess-1", ["first step"]);
    emitTransform(hooks, "sess-1", ["second step"]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe("second step");
  });

  it("keeps the prompt for later turns in the same session", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    emitTransform(hooks, "sess-1", ["session prompt"]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-2"));

    expect(generations).toHaveLength(2);
    expect(generations[1]!.seed.systemPrompt).toBe("session prompt");
  });

  it("drops malformed system entries instead of throwing", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    expect(() =>
      emitTransform(hooks, "sess-1", ["kept", 42, null] as any),
    ).not.toThrow();
    expect(() =>
      hooks.systemTransform(
        { sessionID: "sess-1", model: { id: "claude-sonnet-4" } },
        { system: "not-an-array" as any },
      ),
    ).not.toThrow();
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBe("kept");
  });

  it("omits the prompt in metadata_only", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(
      baseConfig({ contentCapture: "metadata_only" }),
    );

    emitTransform(hooks, "sess-1", ["session prompt"]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBeUndefined();
  });

  it("keeps prompts of concurrent sessions separate", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    emitTransform(hooks, "sess-1", ["prompt one"]);
    emitTransform(hooks, "sess-2", ["prompt two"]);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
    await emitMessageUpdated(hooks, assistantMessage("sess-2", "msg-1"));

    expect(generations).toHaveLength(2);
    expect(generations[0]!.seed.systemPrompt).toBe("prompt one");
    expect(generations[1]!.seed.systemPrompt).toBe("prompt two");
  });

  it("clears the prompt when the session is deleted", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    emitTransform(hooks, "sess-1", ["session prompt"]);
    await emitSessionDeleted(hooks, "sess-1");
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.systemPrompt).toBeUndefined();
  });
});

describe("opencode tool definitions", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("builds name-only definitions from used tools and legacy overrides", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [] },
    );
    await hooks.toolExecuteBefore(
      { tool: "write", sessionID: "sess-1", callID: "tc-1" },
      { args: {} },
    );
    hooks.toolExecuteAfter(
      { tool: "write", sessionID: "sess-1", callID: "tc-1", args: {} },
      { title: "", output: "", metadata: {} },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    // Sorted by name, deduplicated, disabled overrides excluded.
    expect(generations[0]!.seed.tools).toEqual([
      { name: "legacybash", type: "function" },
      { name: "write", type: "function" },
    ]);
  });

  it("keeps tool names in metadata_only from execution records", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(
      baseConfig({ contentCapture: "metadata_only" }),
    );

    await hooks.toolExecuteBefore(
      { tool: "bash", sessionID: "sess-1", callID: "tc-1" },
      { args: {} },
    );
    hooks.toolExecuteAfter(
      { tool: "bash", sessionID: "sess-1", callID: "tc-1", args: {} },
      { title: "", output: "", metadata: {} },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tools).toEqual([
      { name: "bash", type: "function" },
    ]);
  });

  it("includes a started tool that never completed", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    // tool.execute.after never fires for denied or interrupted tools.
    await hooks.toolExecuteBefore(
      { tool: "bash", sessionID: "sess-1", callID: "tc-1" },
      { args: {} },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tools).toEqual([
      { name: "bash", type: "function" },
    ]);
  });

  it("omits tools when nothing was used or declared", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.tools).toBeUndefined();
  });
});

describe("opencode host version", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  async function emitSessionCreatedWithVersion(
    hooks: TestHooks,
    id: string,
    version: string,
  ) {
    await hooks.event({
      event: { type: "session.created", properties: { info: { id, version } } },
    });
  }

  it("uses the OpenCode version as agent and effective version", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: undefined }));

    await emitSessionCreatedWithVersion(hooks, "sess-1", "1.17.20");
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBe("1.17.20");
    expect(generations[0]!.seed.effectiveVersion).toBe("1.17.20");
  });

  it("updates the version from session.updated", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: undefined }));

    await hooks.event({
      event: {
        type: "session.updated",
        properties: { info: { id: "sess-1", version: "1.18.0" } },
      },
    });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBe("1.18.0");
  });

  it("prefers a configured SIGIL_AGENT_VERSION over the host version", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: "my-agent-2" }));

    await emitSessionCreatedWithVersion(hooks, "sess-1", "1.17.20");
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBe("my-agent-2");
    expect(generations[0]!.seed.effectiveVersion).toBe("my-agent-2");
  });

  it("leaves the version unset without config or session events", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: undefined }));

    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBeUndefined();
  });
});
