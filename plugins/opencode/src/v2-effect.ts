import { createRequire } from "node:module";
import type { Plugin as NativePlugin } from "@opencode/plugin/effect";

// Resolve the runtime beside the optional host peer, not from a bundled copy.
// The host's Effects and service context must use the same runtime.
export function effectV2(context: unknown): object {
  const requireHost = createRequire(import.meta.resolve("@opencode/plugin"));
  const { Effect } = requireHost("effect");
  const host = context as Parameters<NativePlugin.Plugin["effect"]>[0];

  return Effect.flatMap(
    Effect.promise(() => import("@opencode/plugin/promise/adapter")),
    ({ fromPromise }: typeof import("@opencode/plugin/promise/adapter")) =>
      Effect.flatMap(Effect.context(), (runtime: unknown) =>
        fromPromise({
          id: "grafana.agento11y",
          setup: async (promiseContext) => {
            const abort = new AbortController();
            const pending = new Set<Promise<unknown>>();
            const drain = async () => {
              abort.abort();
              await Promise.allSettled([...pending]);
            };
            const { setupV2 } = await import("./v2.js");
            let cleanup: Awaited<ReturnType<typeof setupV2>>;
            try {
              cleanup = await setupV2({
                ...promiseContext,
                skill: {
                  ...promiseContext.skill,
                  // OpenCode 2.0.22's Promise adapter ignores API request signals.
                  // Run the native Effect so observation closure interrupts host work.
                  list: (input, options) => {
                    const task = Effect.runPromiseWith(runtime)(
                      host.skill.list(input),
                      {
                        signal: options?.signal
                          ? AbortSignal.any([abort.signal, options.signal])
                          : abort.signal,
                      },
                    ) as ReturnType<typeof promiseContext.skill.list>;
                    pending.add(task);
                    void task.then(
                      () => pending.delete(task),
                      () => pending.delete(task),
                    );
                    return task;
                  },
                },
              });
            } catch (error) {
              await drain();
              throw error;
            }
            return async () => {
              abort.abort();
              try {
                await cleanup?.();
              } finally {
                await drain();
              }
            };
          },
        }).effect(host),
      ),
  );
}
