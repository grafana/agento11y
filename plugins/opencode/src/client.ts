import { SigilClient } from "@grafana/agento11y";
import type { Meter, Tracer } from "@opentelemetry/api";
import { EXPORT_PATH, type SigilOpencodeConfig } from "./config.js";
import { pluginUserAgent } from "./version.js";

export interface SigilClientOptions {
  tracer?: Tracer;
  meter?: Meter;
}

export function createSigilClient(
  config: SigilOpencodeConfig,
  options?: SigilClientOptions,
): SigilClient | null {
  try {
    const guards = config.guards ?? {
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    };
    return new SigilClient({
      generationExport: {
        protocol: "http",
        endpoint: appendExportPath(config.endpoint),
        auth: config.auth,
        headers: { "User-Agent": pluginUserAgent() },
      },
      api: { endpoint: config.endpoint },
      hooks: {
        enabled: guards.enabled,
        phases: ["postflight"],
        timeoutMs: guards.timeoutMs,
        failOpen: guards.failOpen,
      },
      contentCapture: config.contentCapture,
      ...(options?.tracer ? { tracer: options.tracer } : {}),
      ...(options?.meter ? { meter: options.meter } : {}),
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
