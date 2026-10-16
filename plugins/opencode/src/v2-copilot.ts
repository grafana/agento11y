import type { Plugin } from "@opencode/plugin";
import type { Registration } from "@opencode/plugin/promise/registration";

const TITLE_AGENT = "title";

/**
 * OpenCode's built-in Copilot `title` hook runs before plugin hooks and, when
 * the title agent has no model, sends stored conversation text by direct fetch.
 * Title regeneration skips the prompt hook. Without a title agent OpenCode makes
 * no title request, so guarded sessions get no automatic titles until the user
 * sets `agent.title.model`.
 */
export async function registerCopilotTitleProtection(
  context: Pick<Plugin.Context, "agent">,
): Promise<{ refresh(): Promise<void>; dispose(): Promise<void> }> {
  const register = () =>
    context.agent.transform((editor) => {
      if (!editor.get(TITLE_AGENT)?.model) editor.remove(TITLE_AGENT);
    });
  let current: Registration | undefined = await register();
  let queue: Promise<void> = Promise.resolve();
  const serial = (work: () => Promise<void>) => {
    const run = queue.then(work);
    queue = run.catch(() => {});
    return run;
  };
  return {
    // Transforms apply in registration order, and OpenCode registers its config
    // agent transform (agent.title.model) after external plugins. Re-registering
    // after each plugin activation puts this one last again; removing the title
    // agent earlier would make the config rebuild it without its built-in prompt.
    refresh: () =>
      serial(async () => {
        if (!current) return;
        try {
          const next = await register();
          const previous = current;
          current = next;
          await previous.dispose();
        } catch {
          console.warn(
            "[agento11y-opencode] could not reorder the title-agent guard transform",
          );
        }
      }),
    dispose: () =>
      serial(async () => {
        const previous = current;
        current = undefined;
        await previous?.dispose();
      }),
  };
}
