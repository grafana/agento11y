import { Buffer } from "node:buffer";
import type { Agento11yClient, Message } from "@grafana/agento11y";
import type { Plugin } from "@opencode/plugin";
import type { Registration } from "@opencode/plugin/promise/registration";
import type {
  SessionHttpRequest,
  SessionWebSocketSend,
} from "@opencode/plugin/promise/session";
import type { Agento11yOpencodeConfig } from "./config.js";
import { runPreflightTransform } from "./guard.js";
import { nativeAgentName } from "./v2-mappers.js";
import {
  applyRequestTransform,
  inspectHttpBody,
  MAX_BODY_BYTES,
  projectWebSocketFrame,
  RequestGuardRefusal,
  rewriteHttpBody,
} from "./v2-request-mappers.js";

export interface FinalCompactionGuardsArgs {
  client: Agento11yClient;
  config: Agento11yOpencodeConfig;
  context: Pick<Plugin.Context, "session">;
  agentVersion?: string;
}

// Bedrock SigV4 signs the exact body before `http.request` runs (bedrock-auth.ts), so a rewrite would not verify.
function signed(request: Request): boolean {
  if (
    /^\s*AWS4-HMAC-SHA256\b/i.test(request.headers.get("authorization") ?? "")
  )
    return true;
  if (
    ["x-amz-content-sha256", "x-amz-date", "x-amz-security-token"].some(
      (name) => request.headers.has(name),
    )
  )
    return true;
  return [...new URL(request.url).searchParams.keys()].some((key) =>
    /^x-amz-(?:signature|credential)$/i.test(key),
  );
}

// Reads a clone, so the host still sends the original stream untouched.
async function readBody(request: Request): Promise<Uint8Array> {
  if (request.method !== "POST" || request.body === null)
    throw new RequestGuardRefusal("shape");
  if (Number(request.headers.get("content-length")) > MAX_BODY_BYTES)
    throw new RequestGuardRefusal("size");
  const reader = request.clone().body?.getReader();
  if (!reader) throw new RequestGuardRefusal("shape");
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return Buffer.concat(chunks);
    size += value.byteLength;
    if (size > MAX_BODY_BYTES) {
      await reader.cancel();
      throw new RequestGuardRefusal("size");
    }
    chunks.push(value);
  }
}

function rebuild(request: Request, body: Uint8Array<ArrayBuffer>): Request {
  const headers = new Headers(request.headers);
  if (headers.has("content-length"))
    headers.set("content-length", String(body.byteLength));
  return new Request(request.url, {
    method: request.method,
    headers,
    body,
    signal: request.signal,
  });
}

/**
 * Checks each compaction attempt as sent. Upstream compaction.ts
 * flattenAndDropOldest rebuilds the previous summary from storage after the
 * `compaction` hook, which can undo that hook's redactions. Shrink and
 * provider retries, the native compact endpoint and the WebSocket fallback
 * all pass through `http.request` or `experimental.ws.send`.
 */
export function createFinalCompactionGuards({
  client,
  config,
  context,
  agentVersion,
}: FinalCompactionGuardsArgs) {
  const enabled = config.guards?.enabled === true;
  const logger = {
    warn: (message: string) => console.warn(`[agento11y-opencode] ${message}`),
  };
  const evaluate = (
    event: SessionHttpRequest | SessionWebSocketSend,
    messages: Message[],
  ) =>
    runPreflightTransform({
      client,
      agentName: nativeAgentName(config.agentName, event.agent),
      agentVersion,
      failOpen: config.guards?.failOpen ?? true,
      strictTransforms: true,
      logger,
      model: { provider: event.model.providerID, name: event.model.id },
      conversationId: event.sessionID,
      messages,
    });
  // Parser and codec errors can quote the body, so every local failure becomes a reason code.
  const locally = async <T>(step: () => T | Promise<T>): Promise<T> => {
    try {
      return await step();
    } catch (error) {
      const refusal =
        error instanceof RequestGuardRefusal
          ? error
          : new RequestGuardRefusal("internal");
      logger.warn(refusal.message);
      throw refusal;
    }
  };

  const http = async (event: SessionHttpRequest) => {
    const request = event.request;
    const inspected = await locally(async () =>
      inspectHttpBody(
        request.url,
        await readBody(request),
        request.headers.get("content-encoding"),
      ),
    );
    const result = await evaluate(event, inspected.projection.messages);
    if (result?.block) throw new Error(result.block);
    const transformed = result?.messages;
    if (!transformed) return;
    const replacement = await locally(() => {
      const body = rewriteHttpBody(inspected, transformed, !signed(request));
      return body && rebuild(request, body);
    });
    if (replacement) event.request = replacement;
  };

  // The socket driver tracks state from what it built, so frames are never rewritten.
  const send = async (event: SessionWebSocketSend) => {
    const frame = event.frame;
    const projection = await locally(() => projectWebSocketFrame(frame));
    const result = await evaluate(event, projection.messages);
    if (result?.block) throw new Error(result.block);
    const transformed = result?.messages;
    if (transformed)
      await locally(() =>
        applyRequestTransform(undefined, projection, transformed, false),
      );
  };

  return {
    async register(): Promise<() => Promise<void>> {
      if (!enabled) return async () => {};
      const registrations: Registration[] = [];
      const active = new Set<Promise<void>>();
      let closed = false;
      const guard =
        <T extends { readonly kind: string }>(
          callback: (event: T) => Promise<void>,
        ) =>
        async (event: T) => {
          if (event.kind !== "compaction") return;
          if (closed)
            throw new Error("agento11y compaction guards are shutting down");
          const task = callback(event);
          active.add(task);
          try {
            await task;
          } finally {
            active.delete(task);
          }
        };
      let cleanup: Promise<void> | undefined;
      const dispose = () =>
        (cleanup ??= (async () => {
          closed = true;
          const results = await Promise.allSettled(
            [...registrations]
              .reverse()
              .map((registration) =>
                Promise.resolve().then(() => registration.dispose()),
              ),
          );
          await Promise.allSettled([...active]);
          if (results.some((result) => result.status === "rejected"))
            throw new Error("agento11y compaction guard cleanup failed");
        })());
      try {
        registrations.push(
          await context.session.hook("http.request", guard(http)),
        );
        registrations.push(
          await context.session.hook("experimental.ws.send", guard(send)),
        );
        return dispose;
      } catch {
        await dispose();
        throw new Error("agento11y compaction guard registration failed");
      }
    },
  };
}
