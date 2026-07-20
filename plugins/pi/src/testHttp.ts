// Local HTTP harness for the pi real-SDK guard tests.
//
// Stands up one server that routes the two endpoints the real Sigil JS SDK
// talks to when guards are enabled: `/api/v1/hooks:evaluate` (preflight /
// postflight evaluation) and `/api/v1/generations:export` (turn export). Hook
// responses are supplied per request via a caller-provided responder so a
// single test can model allow / deny / transform / transport-failure / timeout
// without standing up its own server.

import { createServer, type Server } from "node:http";

const HOOKS_PATH = "/api/v1/hooks:evaluate";
const EXPORT_PATH = "/api/v1/generations:export";

/** One captured `hooks:evaluate` request: the phase plus the parsed body. */
export interface HookCall {
  phase: string;
  body: HookRequestBody;
}

/** Shape of the serialized hook request body the plugin sends (snake_case). */
export interface HookRequestBody {
  phase?: string;
  context?: {
    model?: { provider?: string; name?: string };
    agent_name?: string;
    agent_version?: string;
  };
  input?: {
    messages?: unknown[];
    output?: unknown[];
  };
}

/**
 * What the responder returns for a `hooks:evaluate` request. `json` is the
 * response body (defaults to a bare allow). `status` overrides the HTTP status
 * for the transport-failure path. `delayMs` holds the response open so a test
 * can force a client-side abort/timeout.
 */
export interface HookResponse {
  status?: number;
  json?: unknown;
  delayMs?: number;
}

export interface Agento11yTestServer {
  server: Server;
  baseUrl: string;
  hookCalls: HookCall[];
  /** Number of accepted `generations:export` requests. */
  exportCount: number;
  /** Non-empty when the server saw an unexpected path or invalid JSON. */
  errors: string[];
}

/**
 * Start a server on 127.0.0.1 that captures `hooks:evaluate` requests and acks
 * `generations:export`. `hook()` is called once per hook request to produce the
 * response, so a test can vary behavior across the preflight and postflight
 * calls of a single turn by returning different values on successive calls.
 */
export function startAgento11yTestServer(opts: {
  hook: (call: HookCall) => HookResponse;
}): Promise<Agento11yTestServer> {
  const state: Agento11yTestServer = {
    server: undefined as unknown as Server,
    baseUrl: "",
    hookCalls: [],
    exportCount: 0,
    errors: [],
  };

  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => {
        raw += chunk;
      });
      req.on("end", () => {
        const url = req.url ?? "";

        if (url === EXPORT_PATH) {
          handleExport(raw, res, state);
          return;
        }
        if (url === HOOKS_PATH) {
          handleHook(raw, res, state, opts.hook);
          return;
        }

        state.errors.push(`unexpected path: ${url}`);
        res.statusCode = 404;
        res.end(JSON.stringify({ error: "unexpected path" }));
      });
    });

    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error("expected AddressInfo from server.address()");
      }
      state.server = server;
      state.baseUrl = `http://127.0.0.1:${addr.port}`;
      resolve(state);
    });
  });
}

function handleExport(
  raw: string,
  res: import("node:http").ServerResponse,
  state: Agento11yTestServer,
): void {
  let parsed: { generations?: Array<{ id?: string }> };
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    state.errors.push(`invalid export JSON: ${String(err)}`);
    res.statusCode = 400;
    res.end(JSON.stringify({ error: "invalid export JSON" }));
    return;
  }
  state.exportCount += 1;
  const results = (parsed.generations ?? []).map((g) => ({
    generation_id: g?.id ?? "",
    accepted: true,
  }));
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify({ results }));
}

function handleHook(
  raw: string,
  res: import("node:http").ServerResponse,
  state: Agento11yTestServer,
  hook: (call: HookCall) => HookResponse,
): void {
  let body: HookRequestBody;
  try {
    body = JSON.parse(raw);
  } catch (err) {
    state.errors.push(`invalid hook JSON: ${String(err)}`);
    res.statusCode = 400;
    res.end(JSON.stringify({ error: "invalid hook JSON" }));
    return;
  }

  const call: HookCall = { phase: body.phase ?? "", body };
  state.hookCalls.push(call);
  const response = hook(call);

  // The client aborts on timeout, so a delayed response may write to a closed
  // socket. Swallow those errors instead of crashing the server callback.
  res.on("error", () => {});
  const send = () => {
    try {
      res.statusCode = response.status ?? 200;
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify(response.json ?? { action: "allow", evaluations: [] }),
      );
    } catch {
      /* response already torn down by the aborting client */
    }
  };

  if (response.delayMs && response.delayMs > 0) {
    setTimeout(send, response.delayMs).unref();
    return;
  }
  send();
}

export function closeServer(server: Server): Promise<void> {
  // Drop any sockets still held open by a delayed/aborted response so close()
  // resolves promptly instead of waiting on the timeout responder.
  (
    server as Server & { closeAllConnections?: () => void }
  ).closeAllConnections?.();
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}
