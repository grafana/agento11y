import { createServer, type Server } from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import type { SigilOpencodeConfig } from "./config.js";
import { createSigilHooks } from "./hooks.js";

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

function config(endpoint: string): SigilOpencodeConfig {
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

    const hooks = await createSigilHooks(config(hookServer.baseUrl), {
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
                type: "tool_call",
                toolCall: {
                  id: "call-1",
                  name: "third-party-test-mcp_third_party_test_mcp_leak_fake_credential",
                  inputJSON: JSON.stringify({ demo: true }),
                },
              },
            ],
          },
        ],
      },
    });

    await hooks.event({ event: { type: "global.disposed", properties: {} } });
  });

  it("sets permission.ask output to deny when Sigil denies", async () => {
    const hookServer = await startHookServer({
      action: "deny",
      reason: "blocked permission",
      evaluations: [],
    });
    servers.push(hookServer.server);

    const hooks = await createSigilHooks(config(hookServer.baseUrl), {
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
      type: "tool_call",
      toolCall: {
        id: "call-1",
        name: "bash",
        inputJSON: JSON.stringify({
          pattern: "rm *",
          title: "Run shell command",
          metadata: { command: "rm -rf /tmp/demo" },
        }),
      },
    });

    await hooks.event({ event: { type: "global.disposed", properties: {} } });
  });
});
