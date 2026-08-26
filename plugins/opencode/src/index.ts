import type { Hooks, Plugin, PluginInput } from "@opencode-ai/plugin";
import { loadConfig } from "./config.js";
import { createAgento11yHooks } from "./hooks.js";
import { LocalReceiverError } from "./local.js";

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

// Best effort: opencode's TUI may not be attached yet while plugins load, and
// a failed toast must not take the plugin down with it.
//
// Never await this from plugin initialization. opencode serves the call
// in-process, and the handler needs the instance whose bootstrap is waiting on
// this plugin's own init, so an awaited toast deadlocks the host at startup
// (opencode 1.18.20). Left floating, it settles as soon as bootstrap finishes.
async function toast(
  client: PluginInput["client"],
  message: string,
  variant: "info" | "error",
): Promise<void> {
  try {
    await client.tui.showToast({
      body: { title: "Agent Observability", message, variant },
    });
  } catch {
    // The notice is not worth failing plugin initialization for.
  }
}

export const Agento11yPlugin: Plugin = async ({ client, directory }) => {
  let config: Awaited<ReturnType<typeof loadConfig>>;
  try {
    config = await loadConfig();
  } catch (err) {
    if (!(err instanceof LocalReceiverError)) throw err;
    // Local mode was chosen for this machine, and no receiver answered. The
    // saved Cloud endpoint is not a fallback: a session told to stay local
    // must not be sent to Cloud because the receiver is down. opencode keeps
    // running with no hooks registered.
    console.warn(`[sigil-opencode] local capture is off: ${err.message}`);
    void toast(client, `Local capture is off: ${err.message}`, "error");
    return {};
  }
  if (!config) return {};

  const hooks = await createAgento11yHooks(config, client, {
    projectDir: directory,
  });
  if (!hooks) return {};

  if (config.local) {
    // Where the session went is not obvious once the endpoint stops being the
    // configured one, so name the receiver the transcript lands in.
    void toast(
      client,
      `Recording to the local receiver at ${config.endpoint}`,
      "info",
    );
  }

  const pluginHooks: HooksWithDispose = {
    // Awaited, and the rejection is not caught: a guard deny refuses the turn
    // by throwing, and opencode's plugin dispatcher has no error handling of
    // its own, so the throw is what stops the prompt.
    "chat.message": async (input, output) => {
      await hooks.chatMessage(input, output);
    },
    "experimental.chat.system.transform": async (input, output) => {
      hooks.systemTransform(input, output);
    },
    // The hook input is `{}`, so only the output is forwarded. Awaited, and the
    // rejection is not caught, for the same reason as `chat.message`: throwing
    // is what refuses a denied turn.
    "experimental.chat.messages.transform": async (_input, output) => {
      await hooks.messagesTransform(output);
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
