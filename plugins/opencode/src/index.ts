import type { Plugin } from "@opencode-ai/plugin";
import { loadConfig } from "./config.js";
import { createSigilHooks } from "./hooks.js";

export const SigilPlugin: Plugin = async ({ client }) => {
  const config = await loadConfig();
  if (!config) return {};

  const hooks = await createSigilHooks(config, client);
  if (!hooks) return {};

  return {
    "chat.message": async (input, output) => {
      hooks.chatMessage(input, output);
    },
    event: async ({ event }) => {
      await hooks.event({
        event: event as { type: string; properties: unknown },
      });
    },
  };
};
