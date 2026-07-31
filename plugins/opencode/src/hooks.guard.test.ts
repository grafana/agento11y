import { createServer, type Server } from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";
import { createAgento11yHooks } from "./hooks.js";
import { emitServerInstanceDisposed } from "./hooks.testutil.js";

type HookServer = {
  server: Server;
  baseUrl: string;
  captures: Array<Record<string, any>>;
};

function startHookServer(
  response: Record<string, unknown>,
): Promise<HookServer> {
  const captures: Array<Record<string, any>> = [];
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        captures.push(JSON.parse(body));
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(response));
      });
    });
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        throw new Error("expected AddressInfo from server.address()");
      }
      resolve({
        server,
        baseUrl: `http://127.0.0.1:${addr.port}`,
        captures,
      });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}

/** Encodes a transformed tool payload the way the server does. */
function base64Json(value: unknown): string {
  return Buffer.from(JSON.stringify(value), "utf8").toString("base64");
}

function config(endpoint: string): Agento11yOpencodeConfig {
  return {
    endpoint,
    auth: { mode: "none" },
    agentName: "opencode",
    agentVersion: "test-version",
    contentCapture: "full",
    debug: false,
    guards: { enabled: true, timeoutMs: 1500, failOpen: true },
  };
}

describe("opencode guards", () => {
  const servers: Server[] = [];

  afterEach(async () => {
    await Promise.all(servers.splice(0).map(closeServer));
  });

  it("blocks denied tool.execute.before calls", async () => {
    const hookServer = await startHookServer({
      action: "deny",
      reason: "blocked demo tool",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    hooks.chatMessage(
      {
        sessionID: "sess-1",
        agent: "build",
        model: { providerID: "anthropic", modelID: "claude-sonnet-4" },
      },
      {
        message: {
          id: "msg-user",
          sessionID: "sess-1",
          role: "user",
          system: "",
          tools: {},
        } as any,
        parts: [],
      },
    );

    await expect(
      hooks.toolExecuteBefore(
        {
          sessionID: "sess-1",
          callID: "call-1",
          tool: "third-party-test-mcp_third_party_test_mcp_leak_fake_credential",
        },
        { args: { demo: true } },
      ),
    ).rejects.toThrow("blocked demo tool");

    expect(hookServer.captures).toHaveLength(1);
    expect(hookServer.captures[0]).toMatchObject({
      phase: "postflight",
      context: {
        agent_name: "opencode:build",
        agent_version: "test-version",
        model: { provider: "anthropic", name: "claude-sonnet-4" },
      },
      input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "third-party-test-mcp_third_party_test_mcp_leak_fake_credential",
                  input_json: { demo: true },
                },
              },
            ],
          },
        ],
      },
    });

    await emitServerInstanceDisposed(hooks);
  });

  it("replaces frozen tool.execute.before args with the redacted set", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  // The server base64-encodes response payloads, so a stub
                  // sending raw JSON here would exercise a shape it never
                  // emits. conformance/hooks/README.md.
                  input_json: base64Json({ command: "echo [REDACTED]" }),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    // opencode >=1.14 freezes output.args. A secret key sits alongside the
    // command; the server drops it and rewrites command. The redacted set must
    // replace output.args even though the original is frozen — an in-place
    // mutation would throw and silently leak the original.
    const output = {
      args: Object.freeze({
        command: "echo sonia@grafana.com",
        apiKey: "sk-secret",
      }) as Record<string, unknown>,
    };

    // Must not throw (allow + transform, not a block).
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      output,
    );

    // Wholesale replacement: dropped key gone, command redacted.
    expect(output.args).toEqual({ command: "echo [REDACTED]" });
    expect(output.args.apiKey).toBeUndefined();

    await emitServerInstanceDisposed(hooks);
  });

  it("leaves tool.execute.before args unchanged when Agent Observability allows without a transform", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const args: Record<string, unknown> = { command: "ls" };
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      { args },
    );

    expect(args).toEqual({ command: "ls" });

    await emitServerInstanceDisposed(hooks);
  });

  it("strips all args when Agent Observability returns an empty-object transform", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  input_json: base64Json({}),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const output: { args: Record<string, unknown> } = {
      args: { command: "secret", apiKey: "x" },
    };
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      output,
    );

    // An intentional "strip all arguments" transform empties the object.
    expect(output.args).toEqual({});

    await emitServerInstanceDisposed(hooks);
  });

  it("fails open and leaves args untouched when args are not a plain object", async () => {
    const hookServer = await startHookServer({
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                kind: "tool_call",
                tool_call: {
                  id: "call-1",
                  name: "bash",
                  input_json: base64Json({ 0: "x" }),
                },
              },
            ],
          },
        ],
      },
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    // opencode hands tool args as an object; an array here stands in for any
    // non-plain-object value the apply path must refuse to mutate.
    const args: unknown[] = ["ls", "-la"];
    await hooks.toolExecuteBefore(
      { sessionID: "sess-1", callID: "call-1", tool: "bash" },
      { args },
    );

    // Fail open: the original (unmutated) args are preserved.
    expect(args).toEqual(["ls", "-la"]);

    await emitServerInstanceDisposed(hooks);
  });

  it("sets permission.ask output to deny when Agent Observability denies", async () => {
    const hookServer = await startHookServer({
      action: "deny",
      reason: "blocked permission",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createAgento11yHooks(config(hookServer.baseUrl), {
      session: { message: async () => ({ data: { parts: [] } }) },
    } as any);
    if (!hooks) throw new Error("expected hooks");

    const output: { status: "ask" | "deny" | "allow" } = { status: "ask" };
    await hooks.permissionAsk(
      {
        id: "perm-1",
        sessionID: "sess-1",
        messageID: "msg-1",
        callID: "call-1",
        type: "bash",
        pattern: "rm *",
        title: "Run shell command",
        metadata: { command: "rm -rf /tmp/demo" },
        time: { created: Date.now() },
      },
      output,
    );

    expect(output.status).toBe("deny");
    expect(
      hookServer.captures[0]?.input?.output?.[0]?.parts?.[0],
    ).toMatchObject({
      kind: "tool_call",
      tool_call: {
        id: "call-1",
        name: "bash",
        // Tool arguments travel as embedded JSON, which is what an
        // argument-level rule matches on. conformance/hooks/README.md.
        input_json: {
          pattern: "rm *",
          title: "Run shell command",
          metadata: { command: "rm -rf /tmp/demo" },
        },
      },
    });

    await emitServerInstanceDisposed(hooks);
  });
});
