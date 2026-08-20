import { readFileSync } from "node:fs";
import { userAgent } from "@grafana/agento11y";

function readPluginVersion(): string {
  try {
    const pkg = JSON.parse(
      readFileSync(new URL("../package.json", import.meta.url), "utf-8"),
    ) as { version?: string };
    return pkg.version ?? "dev";
  } catch {
    return "dev";
  }
}

export const PLUGIN_VERSION = readPluginVersion();

export function pluginUserAgent(): string {
  return `agento11y-plugin-dsh/${PLUGIN_VERSION} ${userAgent()}`;
}
