// The plugin object opencode actually receives. The hook tests drive
// `createAgento11yHooks` directly, so this file covers the wiring between the
// two.

import { beforeEach, describe, expect, it, vi } from "vitest";

const { loadConfigMock, createAgento11yClientMock, resolveAutoTagValuesMock } =
  vi.hoisted(() => ({
    loadConfigMock: vi.fn(),
    createAgento11yClientMock: vi.fn(),
    // hooks.ts resolves auto-tags for the project directory; this file only
    // covers the plugin wiring, so the switch stays off.
    resolveAutoTagValuesMock: vi.fn(() => undefined),
  }));

vi.mock("./config.js", () => ({
  loadConfig: loadConfigMock,
  resolveAutoTagValues: resolveAutoTagValuesMock,
}));
vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));

import { _resetHookState, _setGuardToastDelayMs } from "./hooks.js";
import {
  baseConfig,
  makeAgento11yMock,
  makeOpencodeClient,
} from "./hooks.testutil.js";
import { Agento11yPlugin } from "./index.js";
import { LocalReceiverError } from "./local.js";

// `dispose` is not in the pinned `Hooks` type (see the version note in
// index.ts), so the plugin's return type does not declare it either.
type PluginHooks = Awaited<ReturnType<typeof Agento11yPlugin>> & {
  dispose?: () => Promise<void>;
};

function pluginInput() {
  return { client: makeOpencodeClient(), directory: "/repo" } as any;
}

describe("Agento11yPlugin", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    _resetHookState();
    _setGuardToastDelayMs(0);
  });

  it("wires opencode's dispose hook to the plugin shutdown path", async () => {
    const { sigil } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    loadConfigMock.mockResolvedValue(baseConfig());

    const hooks = (await Agento11yPlugin(pluginInput())) as PluginHooks;

    expect(hooks.dispose).toBeTypeOf("function");
    await hooks.dispose?.();

    expect(sigil.shutdown).toHaveBeenCalledTimes(1);
  });

  it("registers no hooks when no config is resolved", async () => {
    loadConfigMock.mockResolvedValue(null);

    const hooks = (await Agento11yPlugin(pluginInput())) as PluginHooks;

    expect(Object.keys(hooks)).toEqual([]);
    expect(createAgento11yClientMock).not.toHaveBeenCalled();
  });

  it("lets a guard deny reject out of the chat.message hook", async () => {
    // The throw is the whole enforcement mechanism: opencode's plugin
    // dispatcher has no error handling, so a rejection here stops the turn.
    // Dropping the `await` in the wrapper would leave the rejection floating
    // and opencode would send the prompt anyway, which no other test catches.
    const { sigil } = makeAgento11yMock();
    sigil.evaluateHook = vi.fn(async () => ({
      action: "deny",
      reason: "policy says no",
      evaluations: [],
    }));
    createAgento11yClientMock.mockReturnValue(sigil);
    loadConfigMock.mockResolvedValue(
      baseConfig({
        guards: { enabled: true, timeoutMs: 1500, failOpen: true },
      }),
    );

    const input = pluginInput();
    const hooks = (await Agento11yPlugin(input)) as PluginHooks;

    await expect(
      hooks["chat.message"]?.({ sessionID: "sess-1", agent: "build" } as any, {
        message: { id: "m1", sessionID: "sess-1", role: "user" } as any,
        parts: [
          {
            id: "p1",
            sessionID: "sess-1",
            messageID: "m1",
            type: "text",
            text: "key=abc",
          } as any,
        ],
      }),
    ).rejects.toThrow("policy says no");

    await new Promise((resolve) => setTimeout(resolve, 5));
    expect(input.client.tui.showToast).toHaveBeenCalledTimes(1);

    await hooks.dispose?.();
  });

  // A saved AGENTO11Y_LOCAL=true reaches the plugin as a config whose endpoint
  // is the machine's own receiver, or as a LocalReceiverError when no receiver
  // answered. Neither case may end with a Cloud client.
  it("names the receiver the session records to", async () => {
    const { sigil } = makeAgento11yMock();
    createAgento11yClientMock.mockReturnValue(sigil);
    loadConfigMock.mockResolvedValue(
      baseConfig({ endpoint: "http://127.0.0.1:8768", local: true }),
    );

    const input = pluginInput();
    const hooks = (await Agento11yPlugin(input)) as PluginHooks;

    expect(input.client.tui.showToast).toHaveBeenCalledWith({
      body: {
        title: "Agent Observability",
        message: expect.stringContaining("http://127.0.0.1:8768"),
        variant: "info",
      },
    });
    expect(hooks.dispose).toBeTypeOf("function");

    await hooks.dispose?.();
  });

  it("registers no hooks and warns when no local receiver answers", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    loadConfigMock.mockRejectedValue(
      new LocalReceiverError("no local receiver is running"),
    );

    const input = pluginInput();
    const hooks = (await Agento11yPlugin(input)) as PluginHooks;

    expect(Object.keys(hooks)).toEqual([]);
    expect(createAgento11yClientMock).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no local receiver is running"),
    );
    expect(input.client.tui.showToast).toHaveBeenCalledWith({
      body: {
        title: "Agent Observability",
        message: expect.stringContaining("no local receiver is running"),
        variant: "error",
      },
    });
    warn.mockRestore();
  });

  it("loads even when the failure toast cannot be shown", async () => {
    // opencode loads plugins before the TUI is necessarily attached, and a
    // rejected toast must not turn a disabled capture into a broken host.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    loadConfigMock.mockRejectedValue(new LocalReceiverError("nothing running"));

    const input = pluginInput();
    input.client.tui.showToast = vi.fn(async () => {
      throw new Error("no tui");
    });

    await expect(Agento11yPlugin(input)).resolves.toEqual({});
    warn.mockRestore();
  });

  it("lets an unrelated config failure out", async () => {
    // Only a missing receiver is contained here. Anything else is a bug the
    // host should surface rather than a capture decision.
    loadConfigMock.mockRejectedValue(new Error("disk on fire"));

    await expect(Agento11yPlugin(pluginInput())).rejects.toThrow(
      "disk on fire",
    );
  });
});
