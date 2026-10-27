import { createRequire } from "node:module";
import type { Plugin } from "@opencode/plugin";
import type { Plugin as LegacyPlugin, PluginModule } from "@opencode-ai/plugin";
import { beforeEach, describe, expect, expectTypeOf, it, vi } from "vitest";

const { legacy, native, cleanup } = vi.hoisted(() => ({
  legacy: vi.fn(async () => ({})),
  native: vi.fn<Plugin.Plugin["setup"]>(),
  cleanup: vi.fn(async () => {}),
}));
vi.mock("./index.js", () => ({ Agento11yPlugin: legacy }));
vi.mock("./v2.js", () => ({ setupV2: native }));

import server from "./server.js";

const requireHost = createRequire(import.meta.resolve("@opencode/plugin"));
const { Effect } = requireHost("effect");
type Adapter = typeof import("@opencode/plugin/promise/adapter");

function host(list: () => unknown = () => Effect.never) {
  const stub = new Proxy(() => Effect.void, {
    get: (_target, key) => (key === "then" ? undefined : stub),
  });
  return new Proxy(
    {},
    {
      get: (_target, key) => (key === "skill" ? { list } : stub),
    },
  ) as Parameters<ReturnType<Adapter["fromPromise"]>["effect"]>[0];
}

function runScoped(body: () => Promise<void>, context = host()) {
  return Effect.runPromise(
    Effect.scoped(Effect.andThen(server.effect(context), Effect.promise(body))),
  ) as Promise<void>;
}

describe("dual server entry", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    native.mockResolvedValue(cleanup);
  });

  it("keeps the legacy entry and uses only Effect for the native host", () => {
    expect(server.id).toBe("grafana.agento11y");
    expect(server.server).toBe(legacy);
    expect(server).not.toHaveProperty("setup");
    expect(native).not.toHaveBeenCalled();
    expectTypeOf(server.server).toExtend<LegacyPlugin>();
    expectTypeOf(server).toExtend<PluginModule>();
    expectTypeOf(server.effect).parameter(0).toEqualTypeOf<unknown>();
    expectTypeOf(server.effect).returns.toEqualTypeOf<object>();
  });

  it("adapts setup and releases its cleanup once on scope closure", async () => {
    await runScoped(async () => {
      expect(native).toHaveBeenCalledOnce();
      expect(cleanup).not.toHaveBeenCalled();
    });
    expect(cleanup).toHaveBeenCalledOnce();
    expect(legacy).not.toHaveBeenCalled();
  });

  it("accepts an unconfigured setup without a cleanup", async () => {
    native.mockResolvedValueOnce(undefined);
    await runScoped(async () => {});
    expect(cleanup).not.toHaveBeenCalled();
  });

  it("does not fall back to legacy after setup failure", async () => {
    native.mockRejectedValueOnce(new Error("setup failed"));
    await expect(runScoped(async () => {})).rejects.toThrow("setup failed");
    expect(legacy).not.toHaveBeenCalled();
  });

  it.each([
    "observation closure",
    "plugin cleanup",
    "failed cleanup",
    "failed setup",
  ])("interrupts underlying catalog Effects on %s through the real installed adapter", async (kind) => {
    let started = 0;
    let interrupted = 0;
    const operations: Promise<unknown>[] = [];
    const controllers: AbortController[] = [];
    const list = vi.fn(() =>
      Effect.callback(() => {
        started++;
        return Effect.sync(() => {
          interrupted++;
        });
      }),
    );
    native.mockImplementationOnce(async (context) => {
      for (let i = 0; i < 3; i++) {
        const controller = new AbortController();
        controllers.push(controller);
        const operation = context.skill.list(
          { location: { directory: "/repo" } },
          { signal: controller.signal },
        );
        // Observe failures immediately, including setup rollback.
        operations.push(operation.catch(() => undefined));
      }
      if (kind === "failed setup") throw new Error("setup failed");
      return async () => {
        await cleanup();
        if (kind === "failed cleanup") throw new Error("cleanup failed");
      };
    });
    const run = runScoped(async () => {
      expect(started).toBe(3);
      if (kind === "observation closure") {
        controllers.forEach((controller) => {
          controller.abort();
        });
        await Promise.all(operations);
        expect(interrupted).toBe(3);
      }
    }, host(list));
    if (kind.startsWith("failed")) await expect(run).rejects.toThrow(/failed/);
    else await run;
    await Promise.all(operations);
    expect(list).toHaveBeenCalledTimes(3);
    expect(list).toHaveBeenNthCalledWith(1, {
      location: { directory: "/repo" },
    });
    expect(started).toBe(3);
    expect(interrupted).toBe(3);
    expect(legacy).not.toHaveBeenCalled();
  });

  it("passes catalog results and failures without using another host domain", async () => {
    const catalog = { location: { directory: "/repo" }, data: [] };
    const list = vi
      .fn()
      .mockImplementationOnce(() => Effect.succeed(catalog))
      .mockImplementationOnce(() =>
        Effect.fail(new Error("catalog unavailable")),
      );
    native.mockImplementationOnce(async (context) => {
      await expect(context.skill.list({})).resolves.toBe(catalog);
      await expect(context.skill.list({})).rejects.toThrow(
        "catalog unavailable",
      );
    });
    await runScoped(async () => {}, host(list));
    expect(list).toHaveBeenCalledTimes(2);
  });
});
