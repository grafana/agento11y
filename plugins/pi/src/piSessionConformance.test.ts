// pi session conformance: the TypeScript half.
//
// The Go history importer (plugins/agento11y/internal/history/pi.go) hand-ports
// the export path of this plugin's mappers, because the importer is Go and live
// capture is TypeScript. conformance/pi-sessions/ is the only thing keeping the
// two in step: the same session inputs run through the importer in
// plugins/agento11y/internal/history/pi_conformance_test.go and through this
// plugin here, and both normalize to the shape in generations.json.
//
// conformance/pi-sessions/README.md documents the encodings, the ${PLACEHOLDER}
// rule, and every field that cannot agree, with the reason.

import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import registerExtension from "./index.js";
import { restoreEnv, snapshotAndClearTestEnv } from "./testEnv.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const FIXTURE_DIR = join(
  __dirname,
  "..",
  "..",
  "..",
  "conformance",
  "pi-sessions",
);

interface FixtureCase {
  id: string;
  description: string;
  session_file: string;
  files: Record<string, Record<string, unknown>[]>;
}

interface NormalizedPart {
  kind: string;
  text?: string;
  thinking?: string;
  tool_call?: { id: string; name: string; arguments: unknown };
  tool_result?: {
    tool_call_id: string;
    name: string;
    content: string;
    is_error: boolean;
  };
}

interface NormalizedMessage {
  role: string;
  parts: NormalizedPart[];
}

interface NormalizedGeneration {
  id: string;
  conversation_id: string;
  conversation_title: string;
  model: { provider: string; name: string };
  response_id: string;
  response_model: string;
  mode: string;
  operation_name: string;
  stop_reason: string;
  thinking_enabled: boolean;
  usage: Record<string, number>;
  started_at: string;
  completed_at: string;
  input: NormalizedMessage[];
  output: NormalizedMessage[];
  tools: { name: string }[];
  cost_usd: number | null;
  call_error: string;
  parent_generation_ids: string[];
  fork: { parent_session_id: string; parent_generation_id: string } | null;
}

function loadFixture<T>(name: string): T {
  return JSON.parse(readFileSync(join(FIXTURE_DIR, name), "utf-8")) as T;
}

function fixtureCases(): FixtureCase[] {
  const { cases } = loadFixture<{ cases: FixtureCase[] }>("sessions.json");
  expect(cases.length, "sessions.json holds no cases").toBeGreaterThan(0);
  return cases;
}

function expectedGenerations(): Record<string, unknown[]> {
  const { cases } = loadFixture<{ cases: Record<string, unknown[]> }>(
    "generations.json",
  );
  expect(
    Object.keys(cases).length,
    "generations.json holds no cases",
  ).toBeGreaterThan(0);
  return cases;
}

// materializeCase writes a case's session files into dir, one JSON entry per
// line, and returns the path of the file under test plus its parsed entries.
// ${DIR} is replaced with dir, which is how a fork's header names its trunk
// without the fixture carrying a machine-specific path.
function materializeCase(
  dir: string,
  fixture: FixtureCase,
): { sessionPath: string; entries: Record<string, any>[] } {
  let entries: Record<string, any>[] = [];
  for (const [name, lines] of Object.entries(fixture.files)) {
    const body = lines
      .map((entry) => JSON.stringify(entry).replaceAll("${DIR}", dir))
      .join("\n");
    const path = join(dir, name);
    writeFileSync(path, `${body}\n`);
    if (name === fixture.session_file) {
      entries = body.split("\n").map((line) => JSON.parse(line));
    }
  }
  expect(
    entries.length,
    `no entries for ${fixture.session_file}`,
  ).toBeGreaterThan(0);
  return { sessionPath: join(dir, fixture.session_file), entries };
}

// copiedEntryIds returns the entries a fork inherited from its trunk. Pi copies
// the trunk's entries with their own timestamps and stamps the fork's header at
// fork time, so an entry at or before the header instant came from the trunk.
// Live never fires turn_end for one, and the importer never exports one, so the
// replay must not send them either.
function copiedEntryIds(entries: Record<string, any>[]): Set<string> {
  const header = entries[0];
  const out = new Set<string>();
  if (!header?.parentSession) return out;
  const forkedAt = Date.parse(String(header.timestamp));
  if (Number.isNaN(forkedAt)) return out;
  for (const entry of entries.slice(1)) {
    const at = Date.parse(String(entry.timestamp));
    if (!Number.isNaN(at) && at <= forkedAt && typeof entry.id === "string") {
      out.add(entry.id);
    }
  }
  return out;
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

interface CapturedExport {
  generations: any[];
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
          errors.push(`invalid export JSON: ${String(err)}`);
          res.statusCode = 400;
          res.end(JSON.stringify({ error: "invalid export JSON" }));
          return;
        }
        captures.push({
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

const NUMERIC_USAGE_KEYS = [
  "input_tokens",
  "output_tokens",
  "total_tokens",
  "cache_read_input_tokens",
  "cache_write_input_tokens",
  "reasoning_tokens",
] as const;

const ROLES: Record<string, string> = {
  MESSAGE_ROLE_USER: "user",
  MESSAGE_ROLE_ASSISTANT: "assistant",
  MESSAGE_ROLE_TOOL: "tool",
  MESSAGE_ROLE_SYSTEM: "system",
};

// decodePayload reverses the export encoding of a tool payload: the JS SDK
// base64-encodes tool_call.input_json, so only the decoded value is comparable
// with the Go importer's embedded JSON.
function decodePayload(value: unknown): unknown {
  if (typeof value !== "string" || value.length === 0) return {};
  let text = value;
  try {
    const decoded = Buffer.from(value, "base64").toString("utf-8");
    if (Buffer.from(decoded, "utf-8").toString("base64") === value) {
      text = decoded;
    }
  } catch {
    // Not base64; fall through and parse the string itself.
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

function normalizePart(part: any): NormalizedPart {
  if (part?.tool_call) {
    return {
      kind: "tool_call",
      tool_call: {
        id: String(part.tool_call.id ?? ""),
        name: String(part.tool_call.name ?? ""),
        arguments: decodePayload(part.tool_call.input_json),
      },
    };
  }
  if (part?.tool_result) {
    return {
      kind: "tool_result",
      tool_result: {
        tool_call_id: String(part.tool_result.tool_call_id ?? ""),
        name: String(part.tool_result.name ?? ""),
        content: String(part.tool_result.content ?? ""),
        is_error: part.tool_result.is_error === true,
      },
    };
  }
  if (typeof part?.thinking === "string" && part.thinking.length > 0) {
    return { kind: "thinking", thinking: part.thinking };
  }
  return { kind: "text", text: String(part?.text ?? "") };
}

function normalizeMessages(messages: any): NormalizedMessage[] {
  if (!Array.isArray(messages)) return [];
  return messages.map((msg) => ({
    role: ROLES[String(msg?.role ?? "")] ?? String(msg?.role ?? ""),
    parts: Array.isArray(msg?.parts) ? msg.parts.map(normalizePart) : [],
  }));
}

// normalizeGeneration projects one exported wire generation into the shared
// shape. Import-only and live-only fields are dropped rather than compared;
// the fixture README lists them.
function normalizeGeneration(gen: any): NormalizedGeneration {
  const metadata = (gen.metadata ?? {}) as Record<string, unknown>;
  const usage: Record<string, number> = {};
  for (const key of NUMERIC_USAGE_KEYS) {
    usage[key] = Number(gen.usage?.[key] ?? 0);
  }
  const forkSession = metadata["pi.fork.parent_session_id"];
  return {
    id: String(gen.id ?? ""),
    conversation_id: String(gen.conversation_id ?? ""),
    conversation_title: String(
      gen.conversation_title ?? metadata["agento11y.conversation.title"] ?? "",
    ),
    model: {
      provider: String(gen.model?.provider ?? ""),
      name: String(gen.model?.name ?? ""),
    },
    response_id: String(gen.response_id ?? ""),
    response_model: String(gen.response_model ?? ""),
    mode: String(gen.mode ?? "").replace(/^GENERATION_MODE_/, ""),
    operation_name: String(gen.operation_name ?? ""),
    stop_reason: String(gen.stop_reason ?? ""),
    thinking_enabled: gen.thinking_enabled === true,
    usage,
    started_at: String(gen.started_at ?? ""),
    completed_at: String(gen.completed_at ?? ""),
    input: normalizeMessages(gen.input),
    output: normalizeMessages(gen.output),
    tools: Array.isArray(gen.tools)
      ? gen.tools.map((tool: any) => ({ name: String(tool?.name ?? "") }))
      : [],
    cost_usd: typeof metadata.cost_usd === "number" ? metadata.cost_usd : null,
    call_error: String(gen.call_error ?? ""),
    parent_generation_ids: Array.isArray(gen.parent_generation_ids)
      ? gen.parent_generation_ids.map(String)
      : [],
    fork:
      typeof forkSession === "string"
        ? {
            parent_session_id: forkSession,
            parent_generation_id: String(
              metadata["pi.fork.parent_generation_id"] ?? "",
            ),
          }
        : null,
  };
}

function isPlaceholder(want: unknown): boolean {
  return (
    typeof want === "string" && want.startsWith("${") && want.endsWith("}")
  );
}

// diffJson reports every structural difference between got and want as a dotted
// JSON path plus the two values, so a failure names the offending field instead
// of dumping two payloads. It is the TypeScript half of the comparator the Go
// suite ports; both must treat ${PLACEHOLDER} the same way.
function diffJson(path: string, got: unknown, want: unknown): string[] {
  const at = path === "" ? "<root>" : path;
  const show = (value: unknown) => JSON.stringify(value) ?? String(value);

  if (isPlaceholder(want)) {
    if (typeof got !== "string" || got.trim().length === 0) {
      return [
        `${at}: got ${show(got)}, want a non-empty value for placeholder ${show(want)}`,
      ];
    }
    return [];
  }

  if (Array.isArray(want)) {
    if (!Array.isArray(got)) {
      return [`${at}: got ${show(got)}, want an array`];
    }
    if (got.length !== want.length) {
      return [`${at}: got ${got.length} items, want ${want.length}`];
    }
    return want.flatMap((item, i) => diffJson(`${path}[${i}]`, got[i], item));
  }

  if (want !== null && typeof want === "object") {
    if (got === null || typeof got !== "object" || Array.isArray(got)) {
      return [`${at}: got ${show(got)}, want an object`];
    }
    const wantObj = want as Record<string, unknown>;
    const gotObj = got as Record<string, unknown>;
    const keys = [
      ...new Set([...Object.keys(wantObj), ...Object.keys(gotObj)]),
    ].sort();
    return keys.flatMap((key) => {
      const child = path === "" ? key : `${path}.${key}`;
      if (!(key in gotObj)) {
        return [`${child}: missing, want ${show(wantObj[key])}`];
      }
      if (!(key in wantObj)) {
        return [`${child}: unexpected ${show(gotObj[key])}`];
      }
      return diffJson(child, gotObj[key], wantObj[key]);
    });
  }

  if (got !== want) {
    return [`${at}: got ${show(got)}, want ${show(want)}`];
  }
  return [];
}

describe("pi session conformance", () => {
  let serverEnv: Awaited<ReturnType<typeof startExportCaptureServer>>;
  let savedEnv: Record<string, string | undefined> = {};

  beforeEach(async () => {
    serverEnv = await startExportCaptureServer();
    const homeDir = mkdtempSync(join(tmpdir(), "agento11y-pi-conformance-"));
    savedEnv = snapshotAndClearTestEnv();

    process.env.HOME = homeDir;
    process.env.USERPROFILE = homeDir;
    process.env.XDG_CONFIG_HOME = join(homeDir, ".config");
    process.env.AGENTO11Y_ENDPOINT = serverEnv.baseUrl;
    process.env.AGENTO11Y_AUTH_TENANT_ID = "tenant";
    process.env.AGENTO11Y_AUTH_TOKEN = "token";
    process.env.AGENTO11Y_AGENT_NAME = "pi";
    process.env.AGENTO11Y_AGENT_VERSION = "test-version";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "";
    process.env.AGENTO11Y_DEBUG = "";
    process.env.AGENTO11Y_TAGS = "";
    process.env.AGENTO11Y_PI_REDACTION_ENABLED = "false";
  });

  afterEach(async () => {
    await closeServer(serverEnv.server);
    restoreEnv(savedEnv);
    savedEnv = {};
  });

  // replayCase drives the plugin through the case's session log: one turn per
  // assistant entry the session itself produced, with the user messages that
  // preceded it, exactly what pi emits live.
  async function replayCase(
    fixture: FixtureCase,
  ): Promise<NormalizedGeneration[]> {
    const dir = mkdtempSync(join(tmpdir(), "agento11y-pi-session-"));
    const { sessionPath, entries } = materializeCase(dir, fixture);
    const header = entries[0] ?? {};
    const copied = copiedEntryIds(entries);
    const branch = entries.slice(1);
    const sessionName = branch
      .filter((entry) => entry.type === "session_info" && entry.name)
      .map((entry) => String(entry.name))
      .pop();

    const pi = new FakePi();
    registerExtension(pi as any);
    const ctx = {
      sessionManager: {
        getSessionFile: () => sessionPath,
        getSessionId: () => String(header.id ?? ""),
        getSessionName: () => sessionName,
        // The whole tree, copied entries included: lineage has to walk into
        // them to recognise a parent that belongs to the trunk.
        getBranch: () => branch,
      },
    };

    await pi.emit("session_start", {}, ctx);

    let pendingUsers: unknown[] = [];
    for (const entry of branch) {
      if (entry.type !== "message" || !entry.message) continue;
      if (copied.has(String(entry.id))) continue;
      if (entry.message.role === "user") {
        pendingUsers.push(entry.message);
        continue;
      }
      if (entry.message.role !== "assistant") continue;

      const toolResults = toolResultsFor(branch, entry) as any[];
      await pi.emit("turn_start", {}, ctx);
      for (const user of pendingUsers) {
        await pi.emit("message_end", { message: user }, ctx);
      }
      pendingUsers = [];
      await pi.emit("message_end", { message: { role: "assistant" } }, ctx);
      // Pi runs each tool between the assistant message and turn_end. The
      // plugin reads the tool names off these events when the host exposes no
      // tool registry, which is also all the session log can tell the importer.
      for (const result of toolResults) {
        await pi.emit(
          "tool_execution_start",
          { toolCallId: result.toolCallId, toolName: result.toolName },
          ctx,
        );
        await pi.emit(
          "tool_execution_end",
          { toolCallId: result.toolCallId, isError: result.isError === true },
          ctx,
        );
      }
      await pi.emit("turn_end", { message: entry.message, toolResults }, ctx);
    }
    await pi.emit("session_shutdown", {}, ctx);

    expect(serverEnv.errors).toEqual([]);
    const exported = serverEnv.captures
      .flatMap((capture) => capture.generations)
      .filter((gen) => gen?.agent_name === "pi");
    return exported.map(normalizeGeneration);
  }

  // toolResultsFor collects the tool results answering one assistant entry, in
  // the order the calls were made, which is the order pi hands them to
  // `turn_end`.
  function toolResultsFor(
    branch: Record<string, any>[],
    entry: Record<string, any>,
  ): unknown[] {
    const callIds = (entry.message.content ?? [])
      .filter((block: any) => block?.type === "toolCall")
      .map((block: any) => String(block.id));
    if (callIds.length === 0) return [];

    const byCallId = new Map<string, unknown>();
    let seen = false;
    for (const candidate of branch) {
      if (candidate === entry) {
        seen = true;
        continue;
      }
      if (!seen || candidate.type !== "message" || !candidate.message) continue;
      if (candidate.message.role === "assistant") break;
      if (candidate.message.role !== "toolResult") continue;
      const id = String(candidate.message.toolCallId);
      if (!byCallId.has(id)) byCallId.set(id, candidate.message);
    }
    return callIds
      .map((id: string) => byCallId.get(id))
      .filter((result: unknown) => result !== undefined);
  }

  for (const fixture of fixtureCases()) {
    it(`matches the fixture for ${fixture.id}`, async () => {
      const want = expectedGenerations()[fixture.id];
      if (!want) {
        throw new Error(`generations.json has no case ${fixture.id}`);
      }

      const got = await replayCase(fixture);
      expect(
        got.length,
        `exported ${got.length} generations, the fixture expects ${want.length}`,
      ).toBe(want.length);
      for (let i = 0; i < want.length; i++) {
        const diffs = diffJson("", JSON.parse(JSON.stringify(got[i])), want[i]);
        expect(
          diffs,
          `generation ${i} does not match conformance/pi-sessions/generations.json`,
        ).toEqual([]);
      }
    });
  }

  // The comparator itself, not the plugin: each case takes a real exported
  // generation, applies one divergence the fixture cannot accept, and asserts
  // the diff names the offending path. Without this a comparator that ignored a
  // field would keep every conformance case green while checking nothing.
  describe("the comparator names divergent fields", () => {
    const caseId = "tool-call-turn";

    it.each([
      {
        name: "total tokens recomputed the Go launcher way",
        mutate: (gen: NormalizedGeneration) => {
          gen.usage.total_tokens =
            (gen.usage.input_tokens ?? 0) + (gen.usage.output_tokens ?? 0);
        },
        wantPath: "usage.total_tokens",
      },
      {
        name: "stop reason passed through unmapped",
        mutate: (gen: NormalizedGeneration) => {
          gen.stop_reason = "toolUse";
        },
        wantPath: "stop_reason",
      },
      {
        name: "tool call arguments left base64",
        mutate: (gen: NormalizedGeneration) => {
          const part = gen.output[1]?.parts[0];
          if (!part?.tool_call) throw new Error("no tool call in output[1]");
          part.tool_call.arguments = Buffer.from(
            JSON.stringify(part.tool_call.arguments),
            "utf-8",
          ).toString("base64");
        },
        wantPath: "output[1].parts[0].tool_call.arguments",
      },
      {
        name: "a tool result dropped from the output",
        mutate: (gen: NormalizedGeneration) => {
          gen.output = gen.output.slice(0, -1);
        },
        wantPath: "output",
      },
      {
        // A placeholder says the value cannot agree, not that it may be
        // missing: an empty one still has to fail.
        name: "placeholder field left empty",
        mutate: (gen: NormalizedGeneration) => {
          gen.completed_at = "";
        },
        wantPath: "completed_at",
      },
    ])("$name", async ({ mutate, wantPath }) => {
      const fixture = fixtureCases().find((c) => c.id === caseId);
      if (!fixture) {
        throw new Error(
          `fixture case ${caseId} is gone; this test needs a turn with usage and tool traffic`,
        );
      }
      const got = await replayCase(fixture);
      const mutated = JSON.parse(
        JSON.stringify(got[0]),
      ) as NormalizedGeneration;
      mutate(mutated);

      const wantCase = expectedGenerations()[caseId];
      if (!wantCase?.[0]) {
        throw new Error(`generations.json has no case ${caseId}`);
      }
      const diffs = diffJson(
        "",
        JSON.parse(JSON.stringify(mutated)),
        wantCase[0],
      );
      expect(
        diffs.length,
        `the comparator accepted a ${wantPath} divergence`,
      ).toBeGreaterThan(0);
      expect(
        diffs.some((diff) => diff.startsWith(wantPath)),
        `diffs ${JSON.stringify(diffs)} name no path starting with ${wantPath}`,
      ).toBe(true);
    });
  });
});
