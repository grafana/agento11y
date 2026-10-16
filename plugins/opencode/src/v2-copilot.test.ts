import type { Plugin } from "@opencode/plugin";
import type { AgentEditor } from "@opencode/plugin/promise/agent";
import { describe, expect, it, vi } from "vitest";
import { registerCopilotTitleProtection } from "./v2-copilot.js";

type Agent = Record<string, unknown>;
interface Editor {
  get(id: string): Agent | undefined;
  update(id: string, update: (agent: Agent) => void): void;
  remove(id: string): void;
}
type Transform = (editor: Editor) => void;
const model = { providerID: "github-copilot", id: "gpt-5-mini" };

// Mirrors OpenCode 2.0.29 State/Agent: every read rebuilds from empty by
// running transforms in registration order, and update() creates a default
// agent when the ID is missing (packages/core/src/{state,agent}.ts).
function host(config?: { model?: typeof model; system?: string }) {
  const transforms = new Set<Transform>();
  const register = (callback: Transform) => {
    transforms.add(callback);
    return {
      dispose: vi.fn(async () => {
        transforms.delete(callback);
      }),
    };
  };
  const title = () => {
    const agents = new Map<string, Agent>();
    const editor: Editor = {
      get: (id) => agents.get(id),
      update: (id, update) => {
        const agent = agents.get(id) ?? { id, hidden: false };
        agents.set(id, agent);
        update(agent);
      },
      remove: (id) => agents.delete(id),
    };
    for (const transform of transforms) transform(editor);
    return agents.get("title");
  };
  // Built-in agents load before external plugins (plugin/agent.ts).
  register((editor) =>
    editor.update("title", (agent) => {
      agent.hidden = true;
      agent.system = "built-in title prompt";
    }),
  );
  const transform = vi.fn(async (callback: (editor: AgentEditor) => void) =>
    register(callback as unknown as Transform),
  );
  return {
    context: { agent: { transform } } as unknown as Pick<
      Plugin.Context,
      "agent"
    >,
    transform,
    transforms,
    title,
    // User agent config loads after external plugins (config/plugin/agent.ts).
    loadConfig: () =>
      register((editor) => {
        if (!config) return;
        editor.update("title", (agent) => {
          if (config.model) agent.model = config.model;
          if (config.system) agent.system = config.system;
        });
      }),
  };
}

describe("guarded title-agent removal", () => {
  // Without a title agent SessionContext.selectTitle returns undefined, so no
  // title request reaches Copilot's direct-fetch shortcut.
  it.each([
    { config: undefined, beforeRefresh: undefined, after: undefined },
    {
      config: { model },
      // Config rebuilds a removed agent from defaults, dropping its prompt.
      beforeRefresh: { hidden: false, model },
      after: { hidden: true, system: "built-in title prompt", model },
    },
    {
      config: { system: "user prompt" },
      beforeRefresh: { hidden: false, system: "user prompt" },
      after: undefined,
    },
  ])("keeps no model-less title agent once ordered after config $config", async ({
    config,
    beforeRefresh,
    after,
  }) => {
    const native = host(config);
    const protection = await registerCopilotTitleProtection(native.context);
    native.loadConfig();
    expect(native.title()).toEqual(
      beforeRefresh && { id: "title", ...beforeRefresh },
    );
    await protection.refresh();
    expect(native.title()).toEqual(after && { id: "title", ...after });
    await protection.dispose();
    expect(native.transforms.size).toBe(2);
    expect(native.title()).toBeDefined();
  });

  it("registers before releasing, survives a failed refresh, and stops after dispose", async () => {
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const native = host();
      const protection = await registerCopilotTitleProtection(native.context);
      const first = await native.transform.mock.results[0]?.value;
      native.transform.mockRejectedValueOnce(
        new Error("Authorization: Bearer fixture-secret"),
      );
      await protection.refresh();
      expect(first.dispose).not.toHaveBeenCalled();
      expect(native.title()).toBeUndefined();
      await protection.refresh();
      expect(first.dispose).toHaveBeenCalledOnce();
      expect(native.transform.mock.invocationCallOrder[2]).toBeLessThan(
        first.dispose.mock.invocationCallOrder[0],
      );
      await Promise.all([protection.dispose(), protection.refresh()]);
      const second = await native.transform.mock.results[2]?.value;
      expect(second.dispose).toHaveBeenCalledOnce();
      expect(native.transform).toHaveBeenCalledTimes(3);
      expect(native.transforms.size).toBe(1);
      expect(warning).toHaveBeenCalledOnce();
      expect(JSON.stringify(warning.mock.calls)).not.toContain(
        "fixture-secret",
      );
    } finally {
      warning.mockRestore();
    }
  });
});
