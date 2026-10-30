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
  onExportFailure?: (message: string) => void;
}

const EXPORT_FAILURE_WARNING =
  "Agent Observability: some telemetry could not be exported and may be lost.";
const exportFailureReporters = new WeakMap<Agento11yClient, () => void>();

function createExportFailureReporter(
  sink?: (message: string) => void,
): () => void {
  let reported = false;
  return () => {
    if (reported) return;
    reported = true;
    if (sink) {
      try {
        sink(EXPORT_FAILURE_WARNING);
        return;
      } catch {
        // A failed UI notification must not interrupt the host operation.
      }
    }
    try {
      process.stderr.write(`${EXPORT_FAILURE_WARNING}\n`);
    } catch {
      // Reporting is best effort, including when stderr is unavailable.
    }
  };
}

/** Report observed recorder, enqueue, or shutdown loss without exposing errors. */
export function reportExportFailure(client: Agento11yClient): void {
  exportFailureReporters.get(client)?.();
}

const EXPORT_FAILURE_DIAGNOSTICS = [
  "agento11y generation enqueue failed",
  "agento11y generation validation failed",
  "agento11y generation rejected",
  "agento11y generation export failed",
  "agento11y generation export flush on shutdown failed",
  "agento11y generation exporter shutdown failed",
];

function createSdkLogger(reportFailure: () => void): Agento11yLogger {
  function emit(level: "debug" | "warn" | "error", message: string): void {
    const category = EXPORT_FAILURE_DIAGNOSTICS.find(
      (prefix) =>
        message === prefix ||
        message.startsWith(`${prefix}:`) ||
        message.startsWith(`${prefix} `),
    );
    if (category) reportFailure();
    // SDK messages can append raw errors. Only allow fixed diagnostic text;
    // secret redaction alone cannot remove arbitrary evaluator response bodies.
    logger[level](category ?? "agento11y SDK diagnostic");
  }
  return {
    debug: (message) => emit("debug", message),
    warn: (message) => emit("warn", message),
    error: (message) => emit("error", message),
  };
}

export function createAgento11yClient(
  config: Agento11yPiConfig,
  options?: Agento11yClientOptions,
): Agento11yClient | null {
  const reportFailure = createExportFailureReporter(options?.onExportFailure);
  try {
    const client = new Agento11yClient({
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
        // redaction and deny). The `context_with_system` and summary paths pass
        // their own `phases: ["preflight"]` override to `evaluateHook`, which fully
        // replaces this list for that call (see `Agento11yClient.evaluateHook`
        // — `{ ...this.config.hooks, ...override }`), so preflight does not
        // need to be listed here.
        phases: ["postflight"],
        timeoutMs: config.guards.timeoutMs,
        failOpen: config.guards.failOpen,
      },
      contentCapture: config.contentCapture,
      // Client tags, so AGENTO11Y_AUTO_CODING_AGENT_TAGS values reach OTel
      // metrics as well as the export. config.ts already dropped any key AGENTO11Y_TAGS sets,
      // because the SDK lets these caller tags win over the env ones.
      ...(config.autoTags ? { tags: config.autoTags } : {}),
      ...(options?.tracer ? { tracer: options.tracer } : {}),
      ...(options?.meter ? { meter: options.meter } : {}),
      logger: createSdkLogger(reportFailure),
      generationSanitizer: createSecretRedactionSanitizer({
        redactInputMessages: config.redactInputMessages,
      }),
    });
    exportFailureReporters.set(client, reportFailure);
    return client;
  } catch {
    logger.error("failed to create Agento11yClient");
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
