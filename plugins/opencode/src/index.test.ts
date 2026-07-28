// The plugin object opencode actually receives. The hook tests drive
// `createAgento11yHooks` directly, so this file covers the wiring between the
// two.

import { beforeEach, describe, expect, it, vi } from "vitest";

const { loadConfigMock, createAgento11yClientMock } = vi.hoisted(() => ({
  loadConfigMock: vi.fn(),
  createAgento11yClientMock: vi.fn(),
}));

vi.mock("./config.js", () => ({ loadConfig: loadConfigMock }));
vi.mock("./client.js", () => ({
  createAgento11yClient: createAgento11yClientMock,
}));

import { _resetHookState } from "./hooks.js";
import {
  baseConfig,
  makeAgento11yMock,
  makeOpencodeClient,
} from "./hooks.testutil.js";
import { Agento11yPlugin } from "./index.js";

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
});
