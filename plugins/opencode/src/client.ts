import { Agento11yClient } from "@grafana/agento11y";
import type { Meter, Tracer } from "@opentelemetry/api";
import { type Agento11yOpencodeConfig, EXPORT_PATH } from "./config.js";
import { pluginUserAgent } from "./version.js";

export interface Agento11yClientOptions {
  tracer?: Tracer;
  meter?: Meter;
}

export function createAgento11yClient(
  config: Agento11yOpencodeConfig,
  options?: Agento11yClientOptions,
): Agento11yClient | null {
  try {
    const guards = config.guards ?? {
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    };
    return new Agento11yClient({
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
    console.warn("[sigil-opencode] failed to create Agento11yClient:", err);
    return null;
  }
}

/**
 * Append `/api/v1/generations:export` to a bare Agent Observability base URL. The SDK's
 * own normalizer only appends when the URL has no path, which breaks
 * prefix-mounted Agent Observability deployments (`https://host/prefix`) — so we do it
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
