import { randomUUID } from "node:crypto";
import type { Plugin } from "@opencode/plugin";
import { createAgento11yClient } from "./client.js";
import { loadConfig, resolveAutoTagValues } from "./config.js";
import { LocalReceiverError } from "./local.js";
import { createRedactor } from "./redaction.js";
import {
  createOwnedTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";
import { registerCopilotTitleProtection } from "./v2-copilot.js";
import { createNativeCapture } from "./v2-events.js";
import { createNativeGuards } from "./v2-guards.js";
import { createFinalCompactionGuards } from "./v2-request-guards.js";

export const setupV2: Plugin.Plugin["setup"] = async (context) => {
  let config: Awaited<ReturnType<typeof loadConfig>>;
  try {
    config = await loadConfig();
  } catch (error) {
    if (!(error instanceof LocalReceiverError)) throw error;
    console.warn(
      "[agento11y-opencode] local capture is off: receiver unavailable",
    );
    return;
  }
  if (!config) return;

  let telemetry: TelemetryProviders | undefined;
  let client: ReturnType<typeof createAgento11yClient>;
  let capture: ReturnType<typeof createNativeCapture> | undefined;
  let intake: Promise<void> | undefined;
  const registrations: (() => Promise<void>)[] = [];
  const abort = new AbortController();
  let stopped = false;
  let cleanupPromise: Promise<void> | undefined;
  const cleanup = (): Promise<void> => {
    cleanupPromise ??= (async () => {
      stopped = true;
      abort.abort();
      await intake;
      try {
        await Promise.allSettled(
          registrations
            .splice(0)
            .reverse()
            .map((dispose) => Promise.resolve().then(dispose)),
        );
        await capture?.shutdown();
      } finally {
        const [, telemetryDrain] = await Promise.allSettled([
          Promise.resolve().then(() => client?.shutdown()),
          Promise.resolve().then(() => telemetry?.shutdown()),
        ]);
        if (telemetryDrain.status === "rejected") {
          console.warn(
            "[agento11y-opencode] final OTLP drain failed; telemetry may be lost",
          );
        }
      }
    })();
    return cleanupPromise;
  };

  try {
    if (config.otlp) {
      telemetry = await createOwnedTelemetryProviders(
        config.otlp,
        randomUUID(),
      );
    }
    client = createAgento11yClient(
      {
        ...config,
        autoTags: resolveAutoTagValues(context.location.directory),
      },
      { tracer: telemetry?.tracer, meter: telemetry?.meter },
    );
    if (!client) {
      await cleanup();
      return;
    }
    capture = createNativeCapture({
      client,
      config,
      redactor: createRedactor(),
      directory: context.location.directory,
      workspaceID: context.location.workspaceID,
      hostVersion: context.app.version,
      debugLog: (message) => {
        if (config.debug) console.error(`[agento11y-opencode] ${message}`);
      },
    });

    const titles = config.guards?.enabled
      ? await registerCopilotTitleProtection(context)
      : undefined;
    if (titles) registrations.push(() => titles.dispose());
    const guards = createNativeGuards({ client, config, context });
    registrations.push(await guards.register());
    registrations.push(
      await createFinalCompactionGuards({
        client,
        config,
        context,
        agentVersion: config.agentVersion || context.app.version,
      }).register(),
    );

    const events = context.event.subscribe({ signal: abort.signal });
    // The host finishes initialization before serving the subscription.
    intake = (async () => {
      try {
        for await (const event of events) {
          if (stopped) break;
          if (event.type === "plugin.updated") void titles?.refresh();
          if (event.type === "session.deleted")
            guards.forget(event.data.sessionID);
          capture?.handleEvent(event);
        }
      } catch {
        if (!stopped) {
          console.warn(
            "[agento11y-opencode] native event subscription stopped",
          );
        }
      }
    })();
    return cleanup;
  } catch (error) {
    await cleanup();
    throw error;
  }
};
