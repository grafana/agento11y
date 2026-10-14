import { Agento11yPlugin } from "./index.js";

// Optional host APIs must not appear in this entry's public declaration.
type LegacyServer = (
  context: unknown,
  options?: Record<string, unknown>,
) => Promise<object>;
type Cleanup = () => void | Promise<void>;

const setupV2 = async (context: unknown): Promise<Cleanup | undefined> => {
  const { setupV2 } = await import("./v2.js");
  return (await setupV2(context as Parameters<typeof setupV2>[0])) ?? undefined;
};

export default {
  id: "grafana.agento11y",
  server: Agento11yPlugin as LegacyServer,
  setup: setupV2,
};
