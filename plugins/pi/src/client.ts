import type { SigilLogger } from "@grafana/sigil-sdk-js";
import {
  createSecretRedactionSanitizer,
  SigilClient,
} from "@grafana/sigil-sdk-js";
import type { Meter, Tracer } from "@opentelemetry/api";
import type { SigilPiConfig } from "./config.js";
import { EXPORT_PATH } from "./config.js";
import { logger } from "./logger.js";
import { pluginUserAgent } from "./version.js";

export interface SigilClientOptions {
  tracer?: Tracer;
  meter?: Meter;
}

function createSdkLogger(): SigilLogger {
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
    message.startsWith("sigil generation export failed") ||
    message.startsWith("sigil generation rejected")
  );
}

export function createSigilClient(
  config: SigilPiConfig,
  options?: SigilClientOptions,
): SigilClient | null {
  try {
    return new SigilClient({
      generationExport: {
        protocol: "http",
        endpoint: appendExportPath(config.endpoint),
        auth: config.auth,
        headers: { "User-Agent": pluginUserAgent() },
      },
      api: { endpoint: config.endpoint },
      hooks: {
        enabled: config.guards.enabled,
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
    logger.error("failed to create SigilClient", err);
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
