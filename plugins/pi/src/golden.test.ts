// Pi high-level real-SDK golden test.
//
// Drives the @grafana/sigil-pi extension through a faked pi host (event
// emitter shaped like the upstream ExtensionAPI), pointing the real Sigil
// JS SDK at a local HTTP server that captures the export payload. The
// normalized capture is compared against
// src/testdata/golden/pi-full-turn.golden.json.
//
// Set UPDATE_GOLDENS=1 to regenerate the golden after a deliberate change.
//
// Why a fake host instead of the real pi runtime: the upstream package's
// ExtensionAPI is event-based, and the events the plugin subscribes to
// are stable enough that we can exercise the full export path without
// pulling in pi's session manager / provider stack. The plugin's only
// pi-side dependency is the `on(event, handler)` shape.

import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import registerExtension from "./index.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const GOLDEN_PATH = join(
  __dirname,
  "testdata",
  "golden",
  "pi-full-turn.golden.json",
);

interface CapturedExport {
  path: string;
  generations: unknown[];
}

class FakePi {
  handlers = new Map<string, (event: any, ctx: any) => Promise<void> | void>();

  on(
    event: string,
    handler: (event: any, ctx: any) => Promise<void> | void,
  ): void {
    this.handlers.set(event, handler);
  }

  async emit(event: string, payload: any, ctx: any): Promise<void> {
    const handler = this.handlers.get(event);
    if (!handler) return;
    await handler(payload, ctx);
  }
}

// startExportCaptureServer launches a local HTTP server that returns an
// `accepted` result for each generation in the body and records the raw
// request. It binds to 127.0.0.1 on an OS-assigned port.
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

// piTurnFixture returns the message payloads used by the golden test:
// a user message, an assistant response with one tool call, and its result.
function piTurnFixture() {
  const userMsg = {
    role: "user" as const,
    content: "summarize the go files",
    timestamp: 1_700_000_000_000,
  };
  const assistantMsg = {
    role: "assistant" as const,
    content: [
      { type: "thinking" as const, thinking: "I should list the files first." },
      {
        type: "toolCall" as const,
        id: "tc-pi-1",
        name: "Bash",
        arguments: { command: "ls *.go" },
      },
      {
        type: "text" as const,
        text: "There are two go files: main.go and util.go.",
      },
    ],
    provider: "anthropic",
    model: "claude-sonnet-4-pi",
    responseId: "resp-pi-1",
    usage: {
      input: 120,
      output: 30,
      cacheRead: 10,
      cacheWrite: 0,
      totalTokens: 150,
      cost: {
        input: 0.001,
        output: 0.002,
        cacheRead: 0,
        cacheWrite: 0,
        total: 0.003,
      },
    },
    stopReason: "stop",
    timestamp: 1_700_000_001_000,
  };
  const toolResult = {
    role: "toolResult" as const,
    toolCallId: "tc-pi-1",
    toolName: "Bash",
    content: [{ type: "text", text: "main.go\nutil.go" }],
    isError: false,
    timestamp: 1_700_000_002_000,
  };
  return { userMsg, assistantMsg, toolResult };
}

describe("pi plugin: real-SDK golden export", () => {
  let serverEnv: Awaited<ReturnType<typeof startExportCaptureServer>>;
  let savedEnv: Record<string, string | undefined> = {};

  beforeEach(async () => {
    serverEnv = await startExportCaptureServer();
    const homeDir = mkdtempSync(join(tmpdir(), "sigil-pi-golden-"));
    savedEnv = snapshotAndClearTestEnv();

    process.env.HOME = homeDir;
    process.env.USERPROFILE = homeDir;
    process.env.XDG_CONFIG_HOME = join(homeDir, ".config");
    process.env.SIGIL_ENDPOINT = serverEnv.baseUrl;
    process.env.SIGIL_AUTH_TENANT_ID = "tenant";
    process.env.SIGIL_AUTH_TOKEN = "token";
    process.env.SIGIL_AGENT_NAME = "pi";
    process.env.SIGIL_AGENT_VERSION = "test-version";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.SIGIL_DEBUG = "";
    process.env.SIGIL_TAGS = "";
    process.env.SIGIL_PI_REDACTION_ENABLED = "false";
  });

  afterEach(async () => {
    await closeServer(serverEnv.server);
    restoreEnv(savedEnv);
    savedEnv = {};
  });

  // runFullTurn drives the FakePi event stream through one assistant turn and
  // returns the normalized export plus the turn generation. Tests that want a
  // non-default content capture mode set
  // process.env.SIGIL_CONTENT_CAPTURE_MODE before calling this helper.
  async function runFullTurn(): Promise<{
    exports: { path: string; generations: unknown[] }[];
    turn: any;
  }> {
    const pi = new FakePi();
    registerExtension(pi as any);

    const { userMsg, assistantMsg, toolResult } = piTurnFixture();

    // Static session branch matching the fixture: user u1 -> assistant a1.
    // Lineage resolution hashes (conversationId, entry id) so the
    // generation id in the golden stays stable across runs.
    const ctx = {
      sessionManager: {
        getSessionFile: () => "pi-session.jsonl",
        getSessionId: () => "pi-conv-1",
        getBranch: () => [
          {
            type: "message",
            id: "u1",
            parentId: null,
            message: userMsg,
          },
          {
            type: "message",
            id: "a1",
            parentId: "u1",
            message: assistantMsg,
          },
        ],
      },
    };

    await pi.emit("session_start", {}, ctx);
    await pi.emit("turn_start", {}, ctx);
    await pi.emit("message_end", { message: userMsg }, ctx);
    await pi.emit(
      "message_update",
      {
        message: { role: "assistant" },
        delta: { type: "text_delta", text: "Two" },
      },
      ctx,
    );
    await pi.emit(
      "tool_execution_start",
      { toolCallId: "tc-pi-1", toolName: "Bash" },
      ctx,
    );
    await pi.emit(
      "tool_execution_end",
      { toolCallId: "tc-pi-1", isError: false },
      ctx,
    );
    await pi.emit("message_end", { message: { role: "assistant" } }, ctx);
    await pi.emit(
      "turn_end",
      { message: assistantMsg, toolResults: [toolResult] },
      ctx,
    );
    await pi.emit("session_shutdown", {}, ctx);

    expect(serverEnv.errors).toEqual([]);
    expect(serverEnv.captures.length).toBeGreaterThanOrEqual(1);

    const exports = serverEnv.captures.map((c) => ({
      path: c.path,
      generations: c.generations.map(normalizeAny),
    }));

    // Sort generations within each request by `id` so concurrent enqueues
    // from the SDK don't produce flaky orderings in the golden.
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
    const turn = allGen.find((g) => g.agent_name === "pi");
    expect(turn, "expected a generation with agent_name=pi").toBeDefined();

    return { exports, turn };
  }

  // The proto export uses a oneof for `parts`, so a part is identified by
  // which field is populated (`tool_call`, `tool_result`, `text`, ...), not
  // by a `type` discriminator. Matches the helper in opencode's golden test.
  function findOutputPart(turn: any, key: string): any {
    for (const msg of turn.output ?? []) {
      for (const part of msg.parts ?? []) {
        if (part[key] !== undefined) return part;
      }
    }
    throw new Error(`missing output part ${key}`);
  }

  it("matches the recorded golden export for a full assistant turn", async () => {
    const { exports, turn } = await runFullTurn();

    expect(turn.conversation_id).toBe("pi-conv-1");
    expect(turn.model.name).toBe("claude-sonnet-4-pi");
    expect(turn.model.provider).toBe("anthropic");
    expect(turn.mode).toBe("GENERATION_MODE_STREAM");
    expect(String(turn.usage.input_tokens)).toBe("120");
    expect(String(turn.usage.output_tokens)).toBe("30");

    assertGoldenJSON(GOLDEN_PATH, exports);
  });

  it("keeps user SIGIL_TAGS and lets built-in git.branch win collisions", async () => {
    // Drive the user-tag merge path: SIGIL_TAGS becomes a client-level
    // tag (js/src/config.ts) that the SDK merges under the per-generation
    // (seed) tags, so the built-in git.branch must win over the
    // user-supplied one. team=ai must survive because it does not collide.
    process.env.SIGIL_TAGS = "team=ai,git.branch=should-lose";

    await runFullTurn();

    // Inspect the raw export capture (pre-normalization) so we can assert
    // on the actual git.branch value. The normalizeFields rule rewrites
    // git.branch to "<NORMALIZED>" by key, which would hide a regression
    // where the user value won.
    const allRawGen = serverEnv.captures.flatMap((c) => c.generations);
    const rawTurn = allRawGen.find((g: any) => g?.agent_name === "pi") as any;
    expect(rawTurn).toBeDefined();
    expect(rawTurn.tags.team).toBe("ai");
    expect(rawTurn.tags["git.branch"]).toBeDefined();
    expect(rawTurn.tags["git.branch"]).not.toBe("should-lose");
  });

  it.each([
    "full",
    "no_tool_content",
    "metadata_only",
    "full_with_metadata_spans",
  ] as const)("propagates content capture mode %s to the SDK export", async (contentCapture) => {
    process.env.SIGIL_CONTENT_CAPTURE_MODE = contentCapture;

    const { turn } = await runFullTurn();

    expect(turn.metadata["sigil.sdk.content_capture_mode"]).toBe(
      contentCapture,
    );

    // full and full_with_metadata_spans must keep tool bodies in the proto
    // export (the SDK splits content only on the OTel span side); see
    // go/sigil/content_capture.go on ContentCaptureModeFullWithMetadataSpans.
    const expectsToolBodies =
      contentCapture === "full" ||
      contentCapture === "full_with_metadata_spans";

    const toolCall = findOutputPart(turn, "tool_call");
    const toolResult = findOutputPart(turn, "tool_result");
    if (expectsToolBodies) {
      expect(toolCall.tool_call.input_json).not.toBe("");
      expect(toolResult.tool_result.content).not.toBe("");
    } else {
      expect(toolCall.tool_call.input_json).toBe("");
      expect(toolResult.tool_result.content).toBe("");
    }
  });
});

// normalizeFields lists JSON keys whose values are dynamic at runtime and
// have no value in a golden diff. Kept in sync with the Go harness's
// normalizeFields list (cmd/sigil/golden_integration_test.go) so the two
// pipelines stay comparable.
const normalizeFields: Record<string, string> = {
  started_at: "<NORMALIZED>",
  completed_at: "<NORMALIZED>",
  timestamp: "<NORMALIZED>",
  trace_id: "<NORMALIZED>",
  span_id: "<NORMALIZED>",
  parent_span_id: "<NORMALIZED>",
  "sigil.sdk.version": "<NORMALIZED>",
  "sigil.sdk.commit": "<NORMALIZED>",
  // effective_version is a sha256 derived from agent_version. Normalize so
  // a future agent_version bump does not silently change the golden hash.
  effective_version: "<NORMALIZED>",
  // The plugin resolves git.branch and cwd from process.cwd(), which
  // varies per developer checkout. Normalize so the golden is stable in
  // CI. The Go harness does not need the same rule because Go fixtures
  // pass cwd / git.branch in via transcript or event data, not from
  // process.cwd().
  "git.branch": "<NORMALIZED>",
  cwd: "<NORMALIZED>",
};

const normalizeKeySuffixes = [".started_at", ".completed_at", ".timestamp"];

// Generated-ID prefixes whose values look like `<prefix>-<random>` and need
// scrubbing. The SDK assigns `gen-*` when a producer-supplied id is absent
// and `span-*`/`trace-*` from the OTel SDK. Pi's deterministic `pi-*` ids
// are intentionally NOT scrubbed: they are stable per conversationId +
// session entry id, so they should remain visible in the golden fixture.
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
      // The SDK assigns a generation `id` if the producer didn't set one.
      // Pi sets a deterministic `pi-*` id from the session branch when
      // available; only random-prefixed ids (gen-/span-/trace-) are
      // scrubbed so the pi-* id stays visible in the golden.
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
