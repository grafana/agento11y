// Tests system prompt capture via `experimental.chat.system.transform`,
// name-only tool definitions, host version capture, and session title capture.

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

import type { Agento11yOpencodeConfig } from "./config.js";
import { _resetHookState, createAgento11yHooks } from "./hooks.js";
import {
  assistantMessage,
  baseConfig,
  emitMessageUpdated,
  emitSessionDeleted,
  emitSessionEvent,
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

  it("uses the OpenCode version as agent and effective version", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: undefined }));

    await emitSessionEvent(hooks, "session.created", {
      id: "sess-1",
      version: "1.17.20",
    });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBe("1.17.20");
    expect(generations[0]!.seed.effectiveVersion).toBe("1.17.20");
  });

  it("updates the version from session.updated", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: undefined }));

    await emitSessionEvent(hooks, "session.updated", {
      id: "sess-1",
      version: "1.18.0",
    });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect(generations[0]!.seed.agentVersion).toBe("1.18.0");
  });

  it("prefers a configured SIGIL_AGENT_VERSION over the host version", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ agentVersion: "my-agent-2" }));

    await emitSessionEvent(hooks, "session.created", {
      id: "sess-1",
      version: "1.17.20",
    });
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

describe("opencode conversation title", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  const title = "Fix flaky auth test";
  // opencode's own creation defaults, replaced only once its small model
  // produces a real title.
  const newPlaceholder = "New session - 2026-01-12T13:07:53.510Z";
  const childPlaceholder = "Child session - 2026-01-12T13:07:53.510Z";

  function emitTitle(
    hooks: TestHooks,
    type: "session.created" | "session.updated",
    sessionTitle: string | undefined,
    id = "sess-1",
  ): Promise<void> {
    return emitSessionEvent(hooks, type, { id, title: sessionTitle });
  }

  const cases: {
    name: string;
    config?: Agento11yOpencodeConfig;
    run: (hooks: TestHooks) => Promise<void>;
    // Expected seed.conversationTitle per exported generation, in order.
    want: (string | undefined)[];
  }[] = [
    {
      name: "uses the title from session.created",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [title],
    },
    {
      name: "keeps the title for later turns in the same session",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-2"));
      },
      want: [title, title],
    },
    {
      name: "replaces the stored title on a later session.updated",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", "Investigate auth failure");
        await emitTitle(hooks, "session.updated", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [title],
    },
    {
      name: "keeps the previous title when an update is blank",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitTitle(hooks, "session.updated", "");
        await emitTitle(hooks, "session.updated", "   ");
        await emitTitle(hooks, "session.updated", undefined);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [title],
    },
    {
      name: "ignores an empty title",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", "");
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [undefined],
    },
    {
      name: "ignores the placeholder opencode sets at session creation",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", newPlaceholder);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
        await emitTitle(hooks, "session.updated", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-2"));
      },
      want: [undefined, title],
    },
    {
      name: "ignores the placeholder opencode sets on a subagent session",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", childPlaceholder);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [undefined],
    },
    {
      name: "keeps a real title when a placeholder update arrives",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitTitle(hooks, "session.updated", newPlaceholder);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [title],
    },
    {
      name: "keeps a title that only looks like a placeholder",
      run: async (hooks) => {
        await emitTitle(
          hooks,
          "session.created",
          "New session - what changed?",
        );
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: ["New session - what changed?"],
    },
    {
      name: "applies a late title prospectively, leaving the first turn untitled",
      run: async (hooks) => {
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
        await emitTitle(hooks, "session.updated", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-2"));
      },
      want: [undefined, title],
    },
    {
      name: "keeps titles of concurrent sessions separate",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", "title one", "sess-1");
        await emitTitle(hooks, "session.created", "title two", "sess-2");
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
        await emitMessageUpdated(hooks, assistantMessage("sess-2", "msg-1"));
      },
      want: ["title one", "title two"],
    },
    {
      name: "clears the title when the session is deleted",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitSessionDeleted(hooks, "sess-1");
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [undefined],
    },
    {
      name: "clears the title on hook state reset",
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        _resetHookState();
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [undefined],
    },
    {
      name: "still sets the title in metadata_only",
      config: baseConfig({ contentCapture: "metadata_only" }),
      run: async (hooks) => {
        await emitTitle(hooks, "session.created", title);
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: [title],
    },
    {
      name: "redacts a secret the titling model copied from the prompt",
      run: async (hooks) => {
        await emitTitle(
          hooks,
          "session.created",
          "Rotate glc_abcdefghijklmnopqrstuvwxyz1234",
        );
        await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));
      },
      want: ["Rotate [REDACTED:grafana-cloud-token]"],
    },
  ];

  it.each(cases)("$name", async ({ config, run, want }) => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(config);

    await run(hooks);

    expect(generations.map((g) => g.seed.conversationTitle)).toEqual(want);
  });

  it("omits the field instead of sending an empty title", async () => {
    const { sigil, generations } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    await emitTitle(hooks, "session.created", newPlaceholder);
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(generations).toHaveLength(1);
    expect("conversationTitle" in generations[0]!.seed).toBe(false);
  });

  it("carries the title on the tool spans nested under the turn", async () => {
    const { sigil } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks();

    await emitTitle(hooks, "session.created", title);
    await hooks.toolExecuteBefore(
      { tool: "bash", sessionID: "sess-1", callID: "tc-1" },
      { args: {} },
    );
    hooks.toolExecuteAfter(
      { tool: "bash", sessionID: "sess-1", callID: "tc-1", args: {} },
      { title: "bash", output: "ok", metadata: {} },
    );
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect(sigil.startToolExecution).toHaveBeenCalledWith(
      expect.objectContaining({ conversationTitle: title }),
    );
  });
});

describe("opencode preflight redaction in the export", () => {
  const guards = { enabled: true, timeoutMs: 1500, failOpen: true };

  /** A stored text part, the shape opencode hands `chat.message`. */
  function storedPart(id: string, text: string): any {
    return {
      id,
      sessionID: "sess-1",
      messageID: "user-1",
      type: "text",
      text,
    };
  }

  /**
   * The outgoing entry for the same message. opencode reads it back from its
   * message store, so the parts carry the ids of the stored ones in new
   * objects, which is what the export substitution matches on.
   */
  function outgoingCopy(parts: any[]) {
    return {
      info: { id: "user-1", role: "user", sessionID: "sess-1" },
      parts: parts.map((part) => ({ ...part })),
    };
  }

  function transformOf(...texts: string[]) {
    return {
      action: "allow",
      transformedInput: {
        messages: texts.map((text) => ({
          role: "user",
          parts: [{ type: "text", text }],
        })),
      },
    };
  }

  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
  });

  it("exports the text the provider received, not what the user typed", async () => {
    const { sigil, generations } = makeAgento11yMock();
    sigil.evaluateHook.mockResolvedValue(transformOf("token=[REDACTED]"));
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ guards }));

    const stored = storedPart("prt-1", "token=alpha");
    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [stored] },
    );
    await hooks.messagesTransform({ messages: [outgoingCopy([stored])] });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect((generations[0]!.result as any).input).toEqual([
      { role: "user", parts: [{ type: "text", text: "token=[REDACTED]" }] },
    ]);
    // opencode's own part is untouched: a redact rule governs what leaves the
    // machine, not the user's transcript.
    expect(stored.text).toBe("token=alpha");
  });

  it("exports a collapsed message once", async () => {
    const { sigil, generations } = makeAgento11yMock();
    sigil.evaluateHook.mockResolvedValue(transformOf("keep\ntoken=[R]"));
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ guards }));

    // A transform returns one string per message, so two text parts come back
    // joined. Replaying it into both parts would export the text twice.
    const first = storedPart("prt-1", "keep");
    const second = storedPart("prt-2", "token=alpha");
    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [first, second] },
    );
    await hooks.messagesTransform({
      messages: [outgoingCopy([first, second])],
    });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect((generations[0]!.result as any).input).toEqual([
      { role: "user", parts: [{ type: "text", text: "keep\ntoken=[R]" }] },
    ]);
  });

  it("exports the original when no rule rewrote anything", async () => {
    const { sigil, generations } = makeAgento11yMock();
    sigil.evaluateHook.mockResolvedValue({ action: "allow" });
    createAgento11yClientMock.mockReturnValue(sigil);
    const hooks = await makeHooks(baseConfig({ guards }));

    const stored = storedPart("prt-1", "token=alpha");
    hooks.chatMessage(
      { sessionID: "sess-1" },
      { message: userMessage("sess-1"), parts: [stored] },
    );
    await hooks.messagesTransform({ messages: [outgoingCopy([stored])] });
    await emitMessageUpdated(hooks, assistantMessage("sess-1", "msg-1"));

    expect((generations[0]!.result as any).input).toEqual([
      { role: "user", parts: [{ type: "text", text: "token=alpha" }] },
    ]);
  });
});
