import type { Plugin } from "@opencode-ai/plugin";
import { loadConfig } from "./config.js";
import { createAgento11yHooks } from "./hooks.js";

export const Agento11yPlugin: Plugin = async ({ client, directory }) => {
  const config = await loadConfig();
  if (!config) return {};

  const hooks = await createAgento11yHooks(config, client, {
    projectDir: directory,
  });
  if (!hooks) return {};

  return {
    "chat.message": async (input, output) => {
      hooks.chatMessage(input, output);
    },
    "experimental.chat.system.transform": async (input, output) => {
      hooks.systemTransform(input, output);
    },
    event: async ({ event }) => {
      await hooks.event({
        event: event as { type: string; properties: unknown },
      });
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
  };
};
