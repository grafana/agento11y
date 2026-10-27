import { Agento11yPlugin } from "./index.js";
import { effectV2 } from "./v2-effect.js";

// Optional host APIs must not appear in this entry's public declaration.
type LegacyServer = (
  context: unknown,
  options?: Record<string, unknown>,
) => Promise<object>;
export default {
  id: "grafana.agento11y",
  server: Agento11yPlugin as LegacyServer,
  effect: effectV2,
};
