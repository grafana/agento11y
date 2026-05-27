import { SigilClient } from "@grafana/sigil-sdk-js";
import { EXPORT_PATH, type SigilOpencodeConfig } from "./config.js";

export function createSigilClient(
  config: SigilOpencodeConfig,
): SigilClient | null {
  try {
    return new SigilClient({
      generationExport: {
        protocol: "http",
        endpoint: appendExportPath(config.endpoint),
        auth: config.auth,
      },
      contentCapture: config.contentCapture,
    });
  } catch (err) {
    console.warn("[sigil-opencode] failed to create SigilClient:", err);
    return null;
  }
}

/**
 * Append `/api/v1/generations:export` to a bare Sigil base URL. The SDK's
 * own normalizer only appends when the URL has no path, which breaks
 * prefix-mounted Sigil deployments (`https://host/prefix`) — so we do it
 * here unconditionally.
 */
function appendExportPath(endpoint: string): string {
  if (!endpoint) return "";
  try {
    const url = new URL(endpoint);
    url.pathname = url.pathname.replace(/\/+$/, "") + EXPORT_PATH;
    return url.toString();
  } catch {
    return endpoint.replace(/\/+$/, "") + EXPORT_PATH;
  }
}
