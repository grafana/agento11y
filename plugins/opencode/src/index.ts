import type { Hooks, Plugin } from "@opencode-ai/plugin";
import { loadConfig } from "./config.js";
import { createAgento11yHooks } from "./hooks.js";

// opencode calls `dispose` on every plugin as a scope finalizer when it tears
// the instance down (packages/opencode/src/plugin/index.ts). Both that call and
// the field on the published `Hooks` type arrived in @opencode-ai/plugin
// 1.15.11, and this package pins 1.3.13 for development against a ^1.2.16 peer
// range, so the type has to be widened locally. Older hosts ignore the extra
// property. Remove this alias once the pin reaches 1.15.11 or later.
//
// `dispose` is required here, unlike upstream, so dropping the wiring below
// fails to compile instead of silently losing the primary shutdown trigger.
type HooksWithDispose = Hooks & { dispose: () => Promise<void> };

export const Agento11yPlugin: Plugin = async ({ client, directory }) => {
  const config = await loadConfig();
  if (!config) return {};

  const hooks = await createAgento11yHooks(config, client, {
    projectDir: directory,
  });
  if (!hooks) return {};

  const pluginHooks: HooksWithDispose = {
    "chat.message": async (input, output) => {
      hooks.chatMessage(input, output);
    },
    "experimental.chat.system.transform": async (input, output) => {
      hooks.systemTransform(input, output);
    },
    event: async ({ event }) => {
      await hooks.event({ event });
    },
    "tool.execute.before": async (input, output) => {
      await hooks.toolExecuteBefore(input, output);
    },
    "tool.execute.after": async (input, output) => {
      hooks.toolExecuteAfter(input, output);
    },
    "permission.ask": async (input, output) => {
      await hooks.permissionAsk(input, output);
    },
    dispose: async () => {
      await hooks.dispose();
    },
  };
  return pluginHooks;
};
