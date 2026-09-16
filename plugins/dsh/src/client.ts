import type { Agento11yLogger } from "@grafana/agento11y";
import {
  Agento11yClient,
  createSecretRedactionSanitizer,
} from "@grafana/agento11y";
import type { Meter, Tracer } from "@opentelemetry/api";
import type { Agento11yDshConfig } from "./config.js";
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
  config: Agento11yDshConfig,
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
      contentCapture: config.contentCapture,
      // Client tags reach OTel metrics. config.ts omits keys set in
      // AGENTO11Y_TAGS because the SDK gives caller tags precedence.
      ...(config.autoTags ? { tags: config.autoTags } : {}),
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

/** The SDK appends the export path only when the base URL has no path. */
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
