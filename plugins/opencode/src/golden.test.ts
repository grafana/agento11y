// OpenCode high-level real-SDK golden test.
//
// Drives the OpenCode plugin through `createSigilHooks(config, client)`
// with a fake OpencodeClient. The Sigil JS SDK exporter is pointed at a
// local HTTP capture server, and the normalized export body is compared
// against src/testdata/golden/opencode-full-message.golden.json.
//
// We bypass `SigilPlugin`/`config.ts` because the config loader resolves a
// path at import time from `homedir()`. `createSigilHooks` is the cleaner
// seam — it accepts a config object directly and is the function
// `SigilPlugin` ultimately delegates to.
//
// Set UPDATE_GOLDENS=1 to regenerate the golden after a deliberate change.

import { readFileSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { SigilOpencodeConfig } from "./config.js";
import { createSigilHooks } from "./hooks.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const GOLDEN_PATH = join(
  __dirname,
  "testdata",
  "golden",
  "opencode-full-message.golden.json",
);

interface CapturedExport {
  path: string;
  generations: unknown[];
}

function startExportCaptureServer(): Promise<{
  server: Server;
  baseUrl: string;
  captures: CapturedExport[];
  errors: string[];
}> {
  const captures: CapturedExport[] = [];
  const errors: string[] = [];
  return new Promise((resolve) => {
    const server = createServer((req, res) => {
      let body = "";
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        let parsed: any;
        try {
          parsed = JSON.parse(body);
        } catch (err) {
          errors.push(`invalid export JSON: ${String(err)}; body=${body}`);
          res.statusCode = 400;
          res.end(JSON.stringify({ error: "invalid export JSON" }));
          return;
        }
        captures.push({
          path: req.url ?? "",
          generations: Array.isArray(parsed.generations)
            ? parsed.generations
            : [],
        });
        const results = (parsed.generations ?? []).map((g: any) => ({
          generation_id: g?.id ?? "",
          accepted: true,
        }));
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ results }));
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
        errors,
      });
    });
  });
}

function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}

// opencodeMessageFixture builds an OpenCode AssistantMessage + Part list
// representing a single turn that includes both an assistant text reply
// and a completed tool call. New tool scenarios are a small extension of
// this fixture: add another ToolPart to assistantParts with the desired
// state.
function opencodeMessageFixture() {
  const sessionID = "opencode-sess-1";
  const messageID = "msg-1";
  const userMessage = {
    id: "user-1",
    sessionID,
    role: "user",
    time: { created: 1_700_000_000_000 },
    agent: "build",
    model: { providerID: "anthropic", modelID: "claude-sonnet-4-opencode" },
    system: "you are a helpful assistant",
    tools: { Bash: true, Read: true },
  } as const;
  const userParts = [
    {
      id: "user-text-1",
      sessionID,
      messageID: "user-1",
      type: "text",
      text: "list go files in this repo",
    },
  ];
  const assistantMessage = {
    id: messageID,
    sessionID,
    role: "assistant",
    time: { created: 1_700_000_001_000, completed: 1_700_000_002_500 },
    parentID: "user-1",
    modelID: "claude-sonnet-4-opencode",
    providerID: "anthropic",
    mode: "build",
    path: { cwd: "/repo/oc-app", root: "/repo/oc-app" },
    cost: 0.005,
    tokens: {
      input: 200,
      output: 50,
      reasoning: 0,
      cache: { read: 30, write: 0 },
    },
    finish: "end_turn",
  } as const;
  const assistantParts = [
    {
      id: "assist-text-1",
      sessionID,
      messageID,
      type: "text",
      text: "I will list the go files for you.",
    },
    {
      id: "assist-tool-1",
      sessionID,
      messageID,
      type: "tool",
      callID: "tc-oc-1",
      tool: "Bash",
      state: {
        status: "completed",
        input: { command: "ls *.go" },
        output: "main.go\nutil.go",
        title: "ls *.go",
        metadata: {},
        time: { start: 1_700_000_001_500, end: 1_700_000_002_000 },
      },
    },
  ];
  return {
    sessionID,
    userMessage,
    userParts,
    assistantMessage,
    assistantParts,
  };
}

const SDK_ENV_KEYS = [
  "SIGIL_ENDPOINT",
  "SIGIL_PROTOCOL",
  "SIGIL_AUTH_MODE",
  "SIGIL_AUTH_TENANT_ID",
  "SIGIL_AUTH_TOKEN",
  "SIGIL_HEADERS",
  "SIGIL_AGENT_NAME",
  "SIGIL_AGENT_VERSION",
  "SIGIL_USER_ID",
  "SIGIL_TAGS",
  "SIGIL_CONTENT_CAPTURE_MODE",
  "SIGIL_DEBUG",
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
] as const;

describe("opencode plugin: real-SDK golden export", () => {
  let serverEnv: Awaited<ReturnType<typeof startExportCaptureServer>>;
  let savedEnv: Record<string, string | undefined> = {};

  beforeEach(async () => {
    for (const k of SDK_ENV_KEYS) {
      savedEnv[k] = process.env[k];
      delete process.env[k];
    }

    serverEnv = await startExportCaptureServer();
  });

  afterEach(async () => {
    await closeServer(serverEnv.server);
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) {
        delete process.env[k];
      } else {
        process.env[k] = v;
      }
    }
    savedEnv = {};
  });

  async function runCompleteAssistantTurn(
    configOverrides: Partial<SigilOpencodeConfig> = {},
  ) {
    const {
      sessionID,
      userMessage,
      userParts,
      assistantMessage,
      assistantParts,
    } = opencodeMessageFixture();

    const config: SigilOpencodeConfig = {
      endpoint: serverEnv.baseUrl,
      auth: { mode: "none" },
      agentName: "opencode",
      agentVersion: "test-version",
      contentCapture: "full",
      debug: false,
      ...configOverrides,
    };

    // Fake OpencodeClient — only client.session.message is consumed by the
    // plugin, and only to fetch assistant parts after the terminal
    // message.updated event. Returning the prepared parts plays the
    // happy-path branch in handleEvent.
    let messageFetches = 0;
    const fakeClient = {
      session: {
        message: async () => {
          messageFetches += 1;
          return { data: { parts: assistantParts } };
        },
      },
    } as any;

    const hooks = await createSigilHooks(config, fakeClient);
    if (!hooks) throw new Error("expected createSigilHooks to return hooks");

    // chat.message stores the user-side context. message.updated fires
    // once the assistant turn is terminal; the plugin then issues a
    // session.message REST call to fetch assistant parts and exports the
    // generation through the SDK.
    hooks.chatMessage(
      { sessionID },
      { message: userMessage as any, parts: userParts as any },
    );
    await hooks.event({
      event: {
        type: "message.updated",
        properties: { info: assistantMessage as any },
      },
    });

    // Lifecycle: session.idle triggers a flush. session.deleted clears
    // in-memory dedup state between table cases. global.disposed shuts down
    // the SDK so the HTTP exporter drains its outbox.
    await hooks.event({
      event: {
        type: "session.idle",
        properties: { info: { id: sessionID } },
      },
    });
    await hooks.event({
      event: {
        type: "session.deleted",
        properties: { info: { id: sessionID } },
      },
    });
    await hooks.event({
      event: {
        type: "global.disposed",
        properties: {},
      },
    });

    expect(serverEnv.errors).toEqual([]);
    expect(serverEnv.captures.length).toBeGreaterThanOrEqual(1);

    const exports = serverEnv.captures.map((c) => ({
      path: c.path,
      generations: c.generations.map(normalizeAny),
    }));
    for (const exp of exports) {
      exp.generations.sort((a, b) =>
        String((a as any).id ?? "").localeCompare(String((b as any).id ?? "")),
      );
    }

    for (const exp of exports) {
      expect(exp.path).toBe("/api/v1/generations:export");
    }
    const allGen = exports.flatMap((e) => e.generations) as any[];
    expect(allGen.length).toBeGreaterThan(0);
    const turn = allGen.find((g) => g.conversation_id === sessionID);
    expect(turn, "expected a generation for the session").toBeDefined();

    return { exports, sessionID, turn, messageFetches };
  }

  function expectCommonTurnFields(turn: any): void {
    expect(turn.agent_name).toBe("opencode:build");
    expect(turn.model.name).toBe("claude-sonnet-4-opencode");
    expect(turn.model.provider).toBe("anthropic");
    expect(String(turn.usage.input_tokens)).toBe("200");
    expect(String(turn.usage.output_tokens)).toBe("50");
  }

  it("matches the recorded golden for a complete assistant turn", async () => {
    const { exports, turn, messageFetches } = await runCompleteAssistantTurn();

    // Invariant assertions on top of the golden diff.
    expectCommonTurnFields(turn);
    expect(messageFetches).toBe(1);

    assertGoldenJSON(GOLDEN_PATH, exports);
  });

  it.each([
    "full",
    "no_tool_content",
    "metadata_only",
    "full_with_metadata_spans",
  ] as const)("propagates content capture mode %s to the SDK export", async (contentCapture) => {
    const { turn, messageFetches } = await runCompleteAssistantTurn({
      contentCapture,
    });

    expectCommonTurnFields(turn);
    expect(turn.metadata["sigil.sdk.content_capture_mode"]).toBe(
      contentCapture,
    );
    expect(messageFetches).toBe(contentCapture === "metadata_only" ? 0 : 1);
    if (contentCapture === "metadata_only") {
      expect(turn.tools.map((tool: any) => tool.name)).toEqual([
        "Bash",
        "Read",
      ]);
    }
    // full_with_metadata_spans must keep tool bodies in the proto export
    // (the SDK splits content only on the OTel span side); see
    // go/sigil/content_capture.go on ContentCaptureModeFullWithMetadataSpans.
    if (contentCapture === "full_with_metadata_spans") {
      expect(findOutputPart(turn, "tool_call").tool_call.input_json).not.toBe(
        "",
      );
      expect(findOutputPart(turn, "tool_result").tool_result.content).not.toBe(
        "",
      );
    }
  });

  it("keeps text but omits tool bodies in no_tool_content exports", async () => {
    const { turn } = await runCompleteAssistantTurn({
      contentCapture: "no_tool_content",
    });

    expect(turn.input[0].parts[0].text).toBe("list go files in this repo");
    expect(findOutputPart(turn, "text").text).toBe(
      "I will list the go files for you.",
    );
    expect(findOutputPart(turn, "tool_call").tool_call.input_json).toBe("");
    expect(findOutputPart(turn, "tool_result").tool_result.content).toBe("");
  });
});

function findOutputPart(turn: any, key: string): any {
  for (const message of turn.output ?? []) {
    for (const part of message.parts ?? []) {
      if (part[key] !== undefined) return part;
    }
  }
  throw new Error(`missing output part ${key}`);
}

const normalizeFields: Record<string, string> = {
  started_at: "<NORMALIZED>",
  completed_at: "<NORMALIZED>",
  timestamp: "<NORMALIZED>",
  trace_id: "<NORMALIZED>",
  span_id: "<NORMALIZED>",
  parent_span_id: "<NORMALIZED>",
  "sigil.sdk.version": "<NORMALIZED>",
  "sigil.sdk.commit": "<NORMALIZED>",
  // sha256 derived from agent_version; see Pi golden test for the rationale.
  effective_version: "<NORMALIZED>",
  // The plugin resolves git.branch and cwd from the opencode plugin
  // directory (PluginInput.directory, falling back to process.cwd() in
  // tests), which varies per developer checkout. Normalize so the golden
  // is stable in CI.
  "git.branch": "<NORMALIZED>",
  cwd: "<NORMALIZED>",
};

const normalizeKeySuffixes = [".started_at", ".completed_at", ".timestamp"];
const idPrefixes = ["gen-", "span-", "trace-"];

function normalizeAny(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(normalizeAny);
  }
  if (value !== null && typeof value === "object") {
    const obj: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      if (k in normalizeFields) {
        obj[k] = normalizeFields[k];
        continue;
      }
      let matched = false;
      for (const suffix of normalizeKeySuffixes) {
        if (k.endsWith(suffix)) {
          obj[k] = "<NORMALIZED>";
          matched = true;
          break;
        }
      }
      if (matched) continue;
      if (
        k === "id" &&
        typeof v === "string" &&
        idPrefixes.some((p) => v.startsWith(p))
      ) {
        obj[k] = "<NORMALIZED-ID>";
        continue;
      }
      obj[k] = normalizeAny(v);
    }
    return obj;
  }
  return value;
}

function assertGoldenJSON(path: string, got: unknown): void {
  const formatted = `${JSON.stringify(got, null, 2)}\n`;
  if (process.env.UPDATE_GOLDENS === "1") {
    writeFileSync(path, formatted);
    return;
  }
  let existing: string;
  try {
    existing = readFileSync(path, "utf-8");
  } catch (err) {
    throw new Error(
      `golden file missing: ${path} (run with UPDATE_GOLDENS=1 to seed): ${String(err)}`,
    );
  }
  expect(formatted.trim()).toBe(existing.trim());
}
