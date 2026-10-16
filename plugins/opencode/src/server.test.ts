import type { Plugin } from "@opencode/plugin";
import type { Plugin as LegacyPlugin, PluginModule } from "@opencode-ai/plugin";
import { beforeEach, describe, expect, expectTypeOf, it, vi } from "vitest";

const { legacy, native, cleanup } = vi.hoisted(() => {
  const cleanup = vi.fn(async () => {});
  return {
    legacy: vi.fn(async () => ({})),
    native: vi.fn(
      async (_context: unknown): Promise<void | (() => void | Promise<void>)> =>
        cleanup,
    ),
    cleanup,
  };
});
vi.mock("./index.js", () => ({ Agento11yPlugin: legacy }));
vi.mock("./v2.js", () => ({ setupV2: native }));

import server from "./server.js";

describe("dual server entry", () => {
  beforeEach(() => vi.clearAllMocks());

  it("has a stable plugin ID", () => {
    expect(server.id).toBe("grafana.agento11y");
  });

  it("retains the legacy plugin function's identity without activating setup", () => {
    expect(server.server).toBe(legacy);
    expect(legacy).not.toHaveBeenCalled();
    expect(native).not.toHaveBeenCalled();
  });

  it("forwards the setup context and returns its cleanup without activating legacy", async () => {
    const context = { location: { directory: "/repo" } };
    const result = await server.setup(context);

    expect(native).toHaveBeenCalledExactlyOnceWith(context);
    expect(native.mock.calls[0]?.[0]).toBe(context);
    expect(legacy).not.toHaveBeenCalled();
    expect(result).toBe(cleanup);
    expect(cleanup).not.toHaveBeenCalled();

    if (typeof result !== "function") throw new Error("Missing setup cleanup");
    await result();
    expect(cleanup).toHaveBeenCalledTimes(1);
  });

  it("also forwards setup's void result", async () => {
    native.mockResolvedValueOnce(undefined);
    await expect(server.setup({})).resolves.toBeUndefined();
    expect(legacy).not.toHaveBeenCalled();
    expect(cleanup).not.toHaveBeenCalled();
  });

  it("preserves setup failures without falling back to legacy", async () => {
    const error = new Error("setup failed");
    native.mockRejectedValueOnce(error);
    await expect(server.setup({})).rejects.toBe(error);
    expect(legacy).not.toHaveBeenCalled();
  });

  it("is structurally assignable to both installed host contracts", () => {
    expectTypeOf(server.server).toExtend<LegacyPlugin>();
    expectTypeOf(server).toExtend<PluginModule>();
    expectTypeOf(server).toExtend<Plugin.Plugin>();
    expectTypeOf(server.server).parameter(0).toEqualTypeOf<unknown>();
    expectTypeOf(server.setup).parameter(0).toEqualTypeOf<unknown>();
    expectTypeOf(server.server).returns.toEqualTypeOf<Promise<object>>();
    expectTypeOf(server.setup).returns.toEqualTypeOf<
      Promise<undefined | (() => void | Promise<void>)>
    >();
  });
});
