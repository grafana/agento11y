import assert from "node:assert/strict";
import { createHook } from "node:async_hooks";
import { readFile } from "node:fs/promises";
import { setImmediate } from "node:timers/promises";

export function observePluginTimers(extensionPath) {
  const paths = new Set([extensionPath]);
  const timers = new Map();
  const hook = createHook({
    init(id, type, _trigger, resource) {
      if (type !== "Timeout") return;
      const stack = new Error().stack ?? "";
      if (![...paths].some((path) => stack.includes(path))) return;
      // Node retains AbortSignal.timeout timers until their deadlines, even after successful requests.
      if (stack.includes("AbortSignal.timeout")) return;
      timers.set(id, { resource, stack });
    },
    destroy(id) {
      timers.delete(id);
    },
  }).enable();
  const active = () =>
    [...timers.values()].filter(({ resource }) => !resource._destroyed);
  return {
    watch(path) {
      paths.add(path);
    },
    assertMetricReader() {
      assert(
        active().some(
          ({ resource }) => resource._repeat === 15_000 && !resource.hasRef(),
        ),
        "Timer observer must see the live unreferenced 15000ms metric reader",
      );
    },
    async assertReleased(boundary) {
      await setImmediate();
      assert.deepEqual(
        active().map(({ resource, stack }) => ({
          delay: resource._idleTimeout,
          repeat: resource._repeat,
          referenced: resource.hasRef(),
          stack,
        })),
        [],
        `Plugin timers remain after ${boundary}`,
      );
    },
    close() {
      hook.disable();
    },
  };
}

export async function assertPluginDiagnostics(logPath, expected) {
  let log = "";
  try {
    log = await readFile(logPath, "utf8");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  const diagnostics = [
    ...log.matchAll(/^agento11y\[pi\]: \S+ (warn|error) (.*)$/gm),
  ]
    .map(([, level, message]) => `${level} ${message}`)
    .sort();
  assert.deepEqual(
    diagnostics,
    [...expected].sort(),
    "Unexpected plugin diagnostics (or missing intended diagnostic)",
  );
}
