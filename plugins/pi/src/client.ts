import type { Agento11yLogger } from "@grafana/agento11y";
import {
  Agento11yClient,
  createSecretRedactionSanitizer,
} from "@grafana/agento11y";
import type { Meter, Tracer } from "@opentelemetry/api";
import type { Agento11yPiConfig } from "./config.js";
import { EXPORT_PATH } from "./config.js";
import { logger } from "./logger.js";
import { pluginUserAgent } from "./version.js";

export interface Agento11yClientOptions {
  tracer?: Tracer;
  meter?: Meter;
}

function createSdkLogger(): Agento11yLogger {
  return {
    debug: (message: string, ...args: unknown[]) => {
      logger.debug(message, ...args);
    },
    warn: (message: string, ...args: unknown[]) => {
      // Best-effort export failures are expected when the endpoint is
      // unreachable; keep them at debug level so they don't read as warnings.
      if (isBestEffortExportLog(message)) {
        logger.debug(message, ...args);
        return;
      }
      logger.warn(message, ...args);
    },
    error: (message: string, ...args: unknown[]) => {
      logger.error(message, ...args);
    },
  };
}

function isBestEffortExportLog(message: string): boolean {
  return (
    message.startsWith("agento11y generation export failed") ||
    message.startsWith("agento11y generation rejected")
  );
}

export function createAgento11yClient(
  config: Agento11yPiConfig,
  options?: Agento11yClientOptions,
): Agento11yClient | null {
  try {
    return new Agento11yClient({
      generationExport: {
        protocol: "http",
        endpoint: appendExportPath(config.endpoint),
        auth: config.auth,
        headers: { "User-Agent": pluginUserAgent() },
      },
      api: { endpoint: config.endpoint },
      hooks: {
        enabled: config.guards.enabled,
        // The pi plugin's default hook evaluations are postflight (tool-arg
        // redaction and deny). The preflight `context` path passes its own
        // `phases: ["preflight"]` override to `evaluateHook`, which fully
        // replaces this list for that call (see `Agento11yClient.evaluateHook`
        // — `{ ...this.config.hooks, ...override }`), so preflight does not
        // need to be listed here.
        phases: ["postflight"],
        timeoutMs: config.guards.timeoutMs,
        failOpen: config.guards.failOpen,
      },
      contentCapture: config.contentCapture,
      ...(options?.tracer ? { tracer: options.tracer } : {}),
      ...(options?.meter ? { meter: options.meter } : {}),
      logger: createSdkLogger(),
      generationSanitizer: createSecretRedactionSanitizer({
        redactInputMessages: config.redactInputMessages,
      }),
    });
  } catch (err) {
    logger.error("failed to create Agento11yClient", err);
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
