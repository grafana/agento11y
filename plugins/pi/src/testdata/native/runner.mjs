import assert from "node:assert/strict";
import dgram, { createSocket } from "node:dgram";
import {
  copyFile,
  mkdir,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:http";
import { syncBuiltinESMExports } from "node:module";
import net from "node:net";
import { dirname, join, resolve } from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";
import { assertPluginDiagnostics, observePluginTimers } from "./acceptance.mjs";

const scenario = process.argv[2];
const cwd = process.cwd();
const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const hostDir = resolve(
  dirname(fileURLToPath(import.meta.resolve("@mariozechner/pi-coding-agent"))),
  "..",
);
const manifest = JSON.parse(
  await readFile(join(packageDir, "package.json"), "utf8"),
);
const [entry] = manifest.pi.extensions;
assert.equal(manifest.pi.extensions.length, 1);
const extensionPath = resolve(packageDir, entry);
const failures = [];
const pluginTimers = observePluginTimers(extensionPath);
const pluginLog = join(
  process.env.XDG_STATE_HOME,
  "agento11y/logs/agento11y.log",
);
let allowedPort;

function rejectNetwork() {
  const error = new Error("Unexpected network request in native Pi fixture");
  failures.push(error);
  throw error;
}

// Check sockets as well as fetch: OpenTelemetry uses node:http, not fetch.
const connect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (...args) {
  const normalized = Array.isArray(args[0]) ? args[0] : args;
  const options = normalized[0];
  const host = typeof options === "object" ? options.host : normalized[1];
  const port = typeof options === "object" ? options.port : options;
  if (host !== "127.0.0.1" || Number(port) !== allowedPort) rejectNetwork();
  return connect.apply(this, args);
};
dgram.createSocket = rejectNetwork;
syncBuiltinESMExports();
const fetch = globalThis.fetch;
globalThis.fetch = (input, options) => {
  const url = new URL(
    typeof input === "string" || input instanceof URL ? input : input.url,
  );
  if (
    url.protocol !== "http:" ||
    url.hostname !== "127.0.0.1" ||
    Number(url.port) !== allowedPort
  )
    rejectNetwork();
  return fetch(input, options);
};

const generations = [];
const exportPaths = [];
const requests = [];
const hookRequests = [];
let hookReply = () => ({ action: "allow", evaluations: [] });
let hookStatus = 200;
let ordinaryAmbiguous = false;
const telemetry = { traces: [], metrics: [] };
let answer = () => "native answer";
const server = createServer(async (req, res) => {
  try {
    if (
      scenario.startsWith("guard-startup-") &&
      req.method === "GET" &&
      req.url === "/healthz"
    ) {
      res.writeHead(503);
      res.end();
      return;
    }
    assert.equal(req.method, "POST");
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    if (
      [
        "/api/v1/generations:export",
        "/first/api/v1/generations:export",
        "/second/api/v1/generations:export",
      ].includes(req.url)
    ) {
      exportPaths.push(req.url);
      assert(body.generations.length > 0);
      generations.push(...body.generations);
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          results: body.generations.map((generation) => ({
            generation_id: generation.id,
            accepted: true,
          })),
        }),
      );
    } else if (req.url === "/api/v1/hooks:evaluate") {
      hookRequests.push(body);
      res.statusCode = hookStatus;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(await hookReply(body)));
    } else if (req.url === "/v1/traces" || req.url === "/v1/metrics") {
      telemetry[req.url === "/v1/traces" ? "traces" : "metrics"].push(body);
      res.setHeader("Content-Type", "application/json");
      res.end("{}");
    } else if (req.url === "/v1/chat/completions") {
      requests.push(body);
      assert.equal(body.model, "native-model");
      assert.equal(body.stream, true);
      const text = answer(body, requests.length);
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      const chunk = (delta, finish_reason, usage) =>
        `data: ${JSON.stringify({
          id: `native-${requests.length}`,
          object: "chat.completion.chunk",
          created: 1,
          model: "native-model",
          choices: usage ? [] : [{ index: 0, delta, finish_reason }],
          ...(usage ? { usage } : {}),
        })}\n\n`;
      res.write(chunk({ role: "assistant", content: "" }, null));
      res.write(chunk({ content: text }, null));
      res.write(chunk({}, "stop"));
      res.write(
        chunk({}, null, {
          prompt_tokens: 10,
          completion_tokens: 2,
          total_tokens: 12,
        }),
      );
      res.end("data: [DONE]\n\n");
    } else {
      throw new Error(`Unexpected native fixture route: ${req.url}`);
    }
  } catch (error) {
    failures.push(error);
    res.writeHead(500, { "Content-Type": "application/json" });
    res.end("{}");
  }
});
await new Promise((resolveListen) =>
  server.listen(0, "127.0.0.1", resolveListen),
);
allowedPort = server.address().port;
const endpoint = `http://127.0.0.1:${allowedPort}`;
Object.assign(process.env, {
  AGENTO11Y_DEBUG: "true",
  AGENTO11Y_ENDPOINT: endpoint,
  AGENTO11Y_AUTH_TENANT_ID: "native-test",
  AGENTO11Y_AUTH_TOKEN: "synthetic-placeholder",
  AGENTO11Y_GUARDS_ENABLED: "false",
  AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT: endpoint,
  AGENTO11Y_AUTO_CODING_AGENT_TAGS: "false",
});
const host = await import("@mariozechner/pi-coding-agent");
const { AssistantMessageEventStream } = await import("@mariozechner/pi-ai");
const { AuthStorage } = await import(
  pathToFileURL(join(hostDir, "dist/core/auth-storage.js"))
);
const { emitSessionShutdownEvent } = await import(
  pathToFileURL(join(hostDir, "dist/core/extensions/runner.js"))
);
assert.equal(host.VERSION, "1.4.0");

const events = [];
let loader;
let loadedExtensionPath = extensionPath;
let replaceFiles = false;
let replacementSerial = 0;
let session;
let streamSimple;
let executedTools = 0;
let retry = false;
const summaryPreparations = [];
const summaryAfterGuard = [];
const sessionEvents = [];
const summaryCalls = [];
const ordinaryCalls = [];
const summaryOrder = [];
const summaryWitness = Symbol.for("agento11y.native.summary-witness");
const usage = {
  input: 10,
  output: 2,
  cacheRead: 0,
  cacheWrite: 0,
  totalTokens: 12,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

async function start(options = {}) {
  const settingsManager = host.SettingsManager.inMemory({
    defaultTools: [],
    packages: [],
    extensions: [],
    skills: [],
    prompts: [],
    themes: [],
    compaction: {
      enabled: false,
      keepRecentTokens: 0,
      reserveTokens: 100,
      ...options.compaction,
    },
    branchSummary: { reserveTokens: 100 },
    retry: {
      enabled: retry,
      maxRetries: 1,
      baseDelayMs: 1,
      maxAgentDelayMs: 2,
    },
    cacheWarming: "off",
    enableInstallTelemetry: false,
    enableAnalytics: false,
  });
  const modelRuntime = await host.ModelRuntime.create({
    credentials: AuthStorage.inMemory(),
    modelsPath: null,
    allowModelNetwork: false,
    refreshOnCreate: false,
  });
  const provider = {
    api: streamSimple ? "native-synthetic" : "openai-completions",
    baseUrl: `${endpoint}/v1`,
    apiKey: "synthetic-placeholder",
    models: [
      {
        id: "native-model",
        name: "Native synthetic model",
        reasoning: false,
        input: ["text"],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 32768,
        maxTokens: 1000,
      },
    ],
    ...(streamSimple ? { streamSimple } : {}),
  };
  modelRuntime.registerProvider("native-loopback", provider);
  if (options.virtual)
    modelRuntime.registerVirtualModel({
      provider: "native-loopback",
      id: "virtual-model",
      name: "Native virtual model",
      contextWindow: 32768,
      maxTokens: 1000,
      route: () => ({
        model: modelRuntime.getModel("native-loopback", "native-model"),
        thinkingLevel: "off",
      }),
    });
  const witness = join(cwd, "loader-witness.mjs");
  // A top-level counter distinguishes reimport from invoking a cached factory.
  await writeFile(
    witness,
    `const key = Symbol.for("agento11y.native.loader-witness");
const evaluation = globalThis[key] = (globalThis[key] ?? 0) + 1;
export default function (pi) {
  pi.on("session_start", () => pi.appendEntry("native-loader", { evaluation }));
  for (const type of ["session_before_compact", "session_before_tree"])
    pi.on(type, event => globalThis[Symbol.for("agento11y.native.summary-witness")]?.(event));
}
`,
  );
  const makeLoader = (paths) =>
    new host.DefaultResourceLoader({
      cwd,
      agentDir: process.env.PI_CODING_AGENT_DIR,
      settingsManager,
      noExtensions: true,
      noSkills: true,
      noPromptTemplates: true,
      noThemes: true,
      noContextFiles: true,
      additionalExtensionPaths: paths,
      systemPrompt: "Native fixture instructions",
      extensionFactories: [
        (pi) => {
          if (scenario.startsWith("summary-")) {
            for (const type of [
              "session_before_compact",
              "session_before_tree",
            ])
              pi.on(type, (event) => {
                summaryAfterGuard.push(event);
              });
          }
          if (scenario.startsWith("ordinary-")) {
            pi.on("message_end", ({ message }) =>
              message.role === "user"
                ? {
                    message: {
                      ...message,
                      content:
                        typeof message.content === "string"
                          ? message.content.replace("raw", "final")
                          : message.content.map((part) =>
                              part.type === "text"
                                ? {
                                    ...part,
                                    text: part.text.replace("raw", "final"),
                                  }
                                : part,
                            ),
                    },
                  }
                : undefined,
            );
            pi.on("context", ({ messages }) => {
              const output = messages.map((message) =>
                message.role === "user"
                  ? {
                      ...message,
                      content:
                        typeof message.content === "string"
                          ? `prefix ${message.content}`
                          : message.content.map((part) =>
                              part.type === "text"
                                ? { ...part, text: `prefix ${part.text}` }
                                : part,
                            ),
                    }
                  : message,
              );
              if (ordinaryAmbiguous)
                output.push({
                  ...output.findLast((message) => message.role === "user"),
                });
              return { messages: output };
            });
          }
          if (scenario.startsWith("guard-startup-")) {
            pi.registerTool({
              name: "native_action",
              label: "Native action",
              description: "Execute the native test action",
              parameters: { type: "object", properties: {} },
              async execute() {
                executedTools++;
                return { content: [{ type: "text", text: "executed" }] };
              },
            });
          }
          for (const type of [
            "session_start",
            "session_shutdown",
            "before_agent_start",
            "agent_start",
            "agent_end",
            "agent_settled",
            "session_before_compact",
          ])
            pi.on(type, (event) => {
              events.push(structuredClone(event));
              if (scenario === "lifecycle" && type === "session_before_compact")
                return { cancel: true };
            });
        },
      ],
    });
  loader = makeLoader(
    scenario.startsWith("summary-")
      ? [witness, packageDir]
      : [packageDir, witness],
  );
  await loader.reload();
  checkLoader();
  const resourceLoader = {
    getExtensions: () => loader.getExtensions(),
    getSkills: () => loader.getSkills(),
    getPrompts: () => loader.getPrompts(),
    getThemes: () => loader.getThemes(),
    getAgentsFiles: () => loader.getAgentsFiles(),
    getSystemPrompt: () => loader.getSystemPrompt(),
    getSystemPromptSource: () => loader.getSystemPromptSource(),
    getAppendSystemPrompt: () => loader.getAppendSystemPrompt(),
    getAppendSystemPromptSources: () => loader.getAppendSystemPromptSources(),
    extendResources: (...args) => loader.extendResources(...args),
    reload: async () => {
      await pluginTimers.assertReleased("runtime replacement");
      if (replaceFiles) {
        // Node caches native ESM imports by URL. A fresh on-disk package forces
        // module replacement without substituting a previously imported factory.
        const snapshot = join(cwd, `replacement-${++replacementSerial}`);
        await mkdir(join(snapshot, "dist"), { recursive: true });
        await writeFile(
          join(snapshot, "package.json"),
          JSON.stringify(manifest),
        );
        loadedExtensionPath = resolve(snapshot, entry);
        pluginTimers.watch(loadedExtensionPath);
        await copyFile(extensionPath, loadedExtensionPath);
        await symlink(
          join(packageDir, "node_modules"),
          join(snapshot, "node_modules"),
          "dir",
        );
        const replacementWitness = join(snapshot, "loader-witness.mjs");
        await copyFile(witness, replacementWitness);
        loader = makeLoader([snapshot, replacementWitness]);
      }
      await loader.reload();
    },
  };
  const sessionManager = options.seed
    ? host.SessionManager.create(cwd, process.env.PI_CODING_AGENT_SESSION_DIR)
    : host.SessionManager.inMemory(cwd);
  options.seed?.(sessionManager);
  ({ session } = await host.createAgentSession({
    cwd,
    agentDir: process.env.PI_CODING_AGENT_DIR,
    modelRuntime,
    model: modelRuntime.getModel(
      "native-loopback",
      options.virtual ? "virtual-model" : "native-model",
    ),
    thinkingLevel: "off",
    settingsManager,
    sessionManager,
    resourceLoader,
    noTools: true,
  }));
  if (scenario.startsWith("summary-"))
    session.subscribe((event) => sessionEvents.push(event));
  await session.bindExtensions({
    mode: "print",
    onError: (error) => failures.push(error),
  });
  if (!scenario.startsWith("guard-startup-")) pluginTimers.assertMetricReader();
}

function checkLoader() {
  const result = loader.getExtensions();
  assert.deepEqual(result.errors, []);
  assert(
    result.extensions.some(
      (extension) => extension.path === loadedExtensionPath,
    ),
    "Manifest extension was not loaded",
  );
  return result;
}

async function shutdown() {
  if (!session) return;
  await emitSessionShutdownEvent(session.extensionRunner, {
    type: "session_shutdown",
    reason: "exit",
  });
  session.dispose();
  session = undefined;
  await pluginTimers.assertReleased("session shutdown");
}

async function guardStartup(enabled, failOpen) {
  const binary = join(cwd, "missing-receiver");
  await writeFile(binary, "#!/bin/sh\nprintf '{\"running\":false}\\n'\n", {
    mode: 0o755,
  });
  Object.assign(process.env, {
    AGENTO11Y_BIN: binary,
    AGENTO11Y_LOCAL: "true",
    AGENTO11Y_GUARDS_ENABLED: String(enabled),
    AGENTO11Y_GUARDS_FAIL_OPEN: String(failOpen),
  });
  let calls = 0;
  let observedResult;
  streamSimple = (model, context) => {
    calls++;
    assert(calls <= 2, "Unexpected guard startup model request");
    observedResult = context.messages.find(
      (message) => message.role === "toolResult",
    );
    const message = {
      role: "assistant",
      api: model.api,
      provider: model.provider,
      model: model.id,
      timestamp: Date.now(),
      usage,
      content:
        calls === 1
          ? [
              {
                type: "toolCall",
                id: "native-guard",
                name: "native_action",
                arguments: {},
              },
            ]
          : [{ type: "text", text: "finished" }],
      stopReason: calls === 1 ? "toolUse" : "stop",
    };
    const stream = new AssistantMessageEventStream();
    queueMicrotask(() => {
      stream.push({
        type: "start",
        partial: { ...message, stopReason: "pending" },
      });
      stream.push({ type: "done", reason: message.stopReason, message });
      stream.end(message);
    });
    return stream;
  };
  await start();
  await session.prompt("guard initialization failure");
  assert.equal(calls, 2);
  assert.equal(executedTools, enabled && !failOpen ? 0 : 1);
  assert.equal(observedResult.isError, enabled && !failOpen);
  if (enabled && !failOpen) {
    assert.match(JSON.stringify(observedResult.content), /could not evaluate/);
    assert.doesNotMatch(
      JSON.stringify(observedResult.content),
      /missing-receiver|127\.0\.0\.1/,
    );
  }
  await shutdown();
  assert.equal(requests.length, 0);
  assert.equal(
    generations.length,
    0,
    "Local failure must never export to Cloud",
  );
  assert.equal(telemetry.traces.length, 0);
  assert.equal(telemetry.metrics.length, 0);
}

async function ordinary(mode) {
  Object.assign(process.env, {
    AGENTO11Y_CONTENT_CAPTURE_MODE: mode,
    AGENTO11Y_GUARDS_ENABLED: "true",
    AGENTO11Y_TAGS: "owner=native,cwd=configured",
  });
  hookReply = (body) => ({
    action: "allow",
    evaluations: [],
    transformed_input: {
      messages: JSON.parse(
        JSON.stringify(body.input.messages).replaceAll("Falcon", "[REDACTED]"),
      ),
    },
  });
  answer = (body) => {
    const text = JSON.stringify(body.messages);
    assert(text.includes("prefix final [REDACTED]"));
    assert(!text.includes("Falcon"));
    return "safe answer";
  };
  await start();
  await session.prompt("raw Falcon");
  assert(
    JSON.stringify(session.sessionManager.getEntries()).includes(
      "final Falcon",
    ),
  );
  ordinaryAmbiguous = true;
  await session.prompt("raw Falcon");
  await shutdown();
  assert.equal(hookRequests.length, 2);
  for (const request of hookRequests) {
    assert.equal(request.context.tags.owner, "native");
    assert.equal(request.context.tags.cwd, cwd);
    assert.deepEqual(request.context.model, {
      provider: "native-loopback",
      name: "native-model",
    });
    assert(request.input.messages.some((message) => message.role === "system"));
  }
  assert.equal(generations.length, 2);
  const input = JSON.stringify(generations[0].input);
  if (mode === "metadata_only") assert(!input.includes("prefix"));
  else assert(input.includes("prefix final [REDACTED]"));
  assert(!JSON.stringify(generations).includes("Falcon"));
  assert.equal(
    generations[1].input.length,
    0,
    "Duplicate occurrence correspondence must omit input",
  );
}

const marker = "Project Falcon";
const redact = (value) =>
  JSON.parse(JSON.stringify(value).replaceAll(marker, "[R]"));

function assistant(content, extra = {}) {
  return {
    role: "assistant",
    api: "native-synthetic",
    provider: "native-loopback",
    model: "native-model",
    timestamp: Date.now(),
    content:
      typeof content === "string" ? [{ type: "text", text: content }] : content,
    usage: structuredClone(usage),
    stopReason: "stop",
    ...extra,
  };
}

function seedFiles(manager) {
  manager.appendMessage({
    role: "user",
    content: [{ type: "text", text: `${marker} tool request` }],
    timestamp: Date.now(),
  });
  manager.appendMessage(
    assistant(
      [
        { type: "text", text: `${marker} assistant text` },
        ...["read", "write", "edit"].map((name) => ({
          type: "toolCall",
          id: `seed-${name}`,
          name,
          arguments: {
            path: `${marker}/${name}.txt`,
            content: `${marker} JSON string`,
            nested: { values: [`${marker} nested JSON`] },
          },
        })),
      ],
      { stopReason: "toolUse" },
    ),
  );
  for (const toolName of ["read", "write", "edit"])
    manager.appendMessage({
      role: "toolResult",
      toolCallId: `seed-${toolName}`,
      toolName,
      content: [{ type: "text", text: `${marker} ${toolName} result` }],
      isError: false,
      timestamp: Date.now(),
    });
}

function seedCompaction(manager, split = false) {
  const first = manager.appendMessage({
    role: "user",
    content: `${marker} retained history`,
    timestamp: Date.now(),
  });
  manager.appendMessage(assistant(`${marker} earlier answer`));
  manager.appendCompaction(`${marker} previous summary`, first, 100, {
    readFiles: [`${marker}/previous-read.txt`],
    modifiedFiles: [`${marker}/previous-modified.txt`],
  });
  seedFiles(manager);
  manager.appendMessage(assistant(`${marker} prefix answer`));
  if (!split)
    manager.appendMessage({
      role: "user",
      content: "Keep this recent user message",
      timestamp: Date.now(),
    });
}

let treeTarget;
function seedTree(manager) {
  manager.appendMessage({
    role: "user",
    content: "Root",
    timestamp: Date.now(),
  });
  treeTarget = manager.appendMessage(assistant("Root answer"));
  seedFiles(manager);
  manager.appendCustomMessageEntry(
    "summary-string",
    `${marker} custom string`,
    false,
  );
  manager.appendCustomMessageEntry(
    "summary-blocks",
    [{ type: "text", text: `${marker} custom block` }],
    false,
  );
  manager.branchWithSummary(
    manager.getLeafId(),
    `${marker} nested branch summary`,
    {
      readFiles: [`${marker}/branch-read.txt`],
      modifiedFiles: [`${marker}/branch-modified.txt`],
    },
  );
  manager.appendCompaction(
    `${marker} nested compaction summary`,
    treeTarget,
    100,
    {
      readFiles: [`${marker}/compaction-read.txt`],
      modifiedFiles: [`${marker}/compaction-modified.txt`],
    },
  );
  manager.appendMessage(assistant(`${marker} branch answer`));
}

function configureSummary({ failOpen = false, respond } = {}) {
  Object.assign(process.env, {
    AGENTO11Y_GUARDS_ENABLED: "true",
    AGENTO11Y_GUARDS_FAIL_OPEN: String(failOpen),
    AGENTO11Y_AUTO_CODING_AGENT_TAGS: "true",
    AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES: "user",
    AGENTO11Y_USER_ID: "native-summary-user",
    AGENTO11Y_TAGS: "owner=summary,cwd=configured,pi.call_kind=configured",
  });
  globalThis[summaryWitness] = (event) => {
    const preparation = event.preparation;
    summaryOrder.push("preparation");
    summaryPreparations.push({
      event,
      preparation,
      before: structuredClone(preparation),
      messages: preparation.messagesToSummarize,
      prefix: preparation.turnPrefixMessages,
      entries: preparation.entriesToSummarize,
      elements: preparation.entriesToSummarize?.slice(),
    });
  };
  hookReply = (body) => {
    summaryOrder.push("preflight");
    return {
      action: "allow",
      evaluations: [],
      transformed_input: { messages: redact(body.input.messages) },
    };
  };
  streamSimple = (model, context, options) => {
    const summary = options.cacheRetention === "none";
    (summary ? summaryCalls : ordinaryCalls).push(structuredClone(context));
    summaryOrder.push(summary ? "summary-provider" : "ordinary-provider");
    const message = assistant(summary ? "Safe summary" : "Ordinary answer", {
      api: model.api,
      provider: model.provider,
      model: model.id,
      ...respond?.({ summary, context, options }),
    });
    const stream = new AssistantMessageEventStream();
    const finish = () => {
      stream.push({
        type: "start",
        partial: { ...message, stopReason: "pending" },
      });
      stream.push(
        message.stopReason === "error" || message.stopReason === "aborted"
          ? { type: "error", reason: message.stopReason, error: message }
          : { type: "done", reason: message.stopReason, message },
      );
      stream.end(message);
    };
    queueMicrotask(finish);
    return stream;
  };
}

function summaryHooks(kind) {
  return hookRequests.filter(
    (request) => request.context.tags?.["pi.call_kind"] === kind,
  );
}

function assertSummaryHooks(kind, virtual = false) {
  const hooks = summaryHooks(kind);
  assert(hooks.length > 0, `Missing ${kind} preflight guard`);
  for (const request of hooks) {
    assert.equal(request.phase, "preflight");
    assert.equal(request.context.tags.owner, "summary");
    assert.equal(request.context.tags.user, "native-summary-user");
    assert.equal(request.context.tags.cwd, cwd);
    assert.deepEqual(
      request.context.model,
      virtual
        ? { provider: "unknown", name: "unknown" }
        : { provider: "native-loopback", name: "native-model" },
    );
    assert.equal(
      request.context.conversation_id,
      session.sessionManager.getSessionId(),
    );
    const names = request.input.messages.map((message) => message.name);
    assert.equal(new Set(names).size, names.length);
    for (const name of names) assert.match(name, /^pi:\d+$/);
    const ordinals = names.map((name) => Number(name.slice(3)));
    assert.deepEqual(
      ordinals,
      [...ordinals].sort((a, b) => a - b),
    );
  }
  return hooks;
}

async function historySnapshot() {
  return {
    entries: structuredClone(session.sessionManager.getEntries()),
    disk: await readFile(session.sessionFile, "utf8"),
  };
}

async function assertHistoryUnchanged(before) {
  const manager = session.sessionManager;
  for (const entry of before.entries)
    assert.deepEqual(
      manager.getEntry(entry.id),
      entry,
      `Mutated persisted entry ${entry.id}`,
    );
  const disk = await readFile(session.sessionFile, "utf8");
  assert(
    disk.startsWith(before.disk),
    "Summary rewrote existing JSONL history",
  );
  const persisted = disk
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  for (const entry of before.entries)
    assert.deepEqual(
      persisted.find((value) => value.id === entry.id),
      JSON.parse(JSON.stringify(entry)),
    );
}

function assertRequestLocalCompaction(split) {
  const captured = summaryPreparations.at(-1);
  const preparation = captured.preparation;
  assert.equal(
    summaryAfterGuard.at(-1).preparation,
    preparation,
    "Guard must modify the original preparation object",
  );
  assert.equal(preparation.isSplitTurn, split);
  assert(preparation.messagesToSummarize.length > 0);
  assert.notEqual(preparation.messagesToSummarize, captured.messages);
  assert.equal(preparation.previousSummary, "[R] previous summary");
  assert.equal(captured.before.previousSummary, `${marker} previous summary`);
  assert(JSON.stringify(captured.messages).includes(marker));
  for (const field of ["messagesToSummarize", "turnPrefixMessages"])
    assert(!JSON.stringify(preparation[field]).includes(marker));
  if (split) {
    assert(preparation.turnPrefixMessages.length > 0);
    assert.notEqual(preparation.turnPrefixMessages, captured.prefix);
    assert(JSON.stringify(captured.prefix).includes(marker));
  }
  for (const name of ["read", "written", "edited"]) {
    assert(preparation.fileOps[name].size > 0, `Missing fileOps.${name}`);
    assert(
      [...preparation.fileOps[name]].every((path) => path.includes("[R]")),
    );
    assert(
      [...captured.before.fileOps[name]].every((path) => path.includes(marker)),
    );
  }
  for (const context of summaryCalls) {
    assert(!JSON.stringify(context).includes(marker));
    assert(JSON.stringify(context).includes("[R]"));
  }
  assert(
    summaryOrder.indexOf("preflight") <
      summaryOrder.indexOf("summary-provider"),
  );
}

async function summaryCompaction(split = false, virtual = false) {
  configureSummary();
  await start({ seed: (manager) => seedCompaction(manager, split), virtual });
  const before = await historySnapshot();
  const result = await session.compact("Keep all file lists");
  const [hook] = assertSummaryHooks("compaction", virtual);
  const input = JSON.stringify(hook.input.messages);
  for (const text of [
    "previous summary",
    "previous-read.txt",
    "previous-modified.txt",
    "read.txt",
    "write.txt",
    "edit.txt",
    "Keep all file lists",
    "nested JSON",
  ])
    assert(input.includes(text), `Missing summary preflight surface: ${text}`);
  assert.equal(summaryCalls.length, split ? 2 : 1);
  assertRequestLocalCompaction(split);
  assert(!JSON.stringify(result).includes(marker));
  assert(result.details.readFiles.includes("[R]/read.txt"));
  assert(result.details.modifiedFiles.includes("[R]/write.txt"));
  assert(result.details.modifiedFiles.includes("[R]/edit.txt"));
  await assertHistoryUnchanged(before);
}

async function summaryTree() {
  configureSummary();
  await start({ seed: seedTree });
  const before = await historySnapshot();
  const result = await session.navigateTree(treeTarget, {
    summarize: true,
    customInstructions: `${marker} instruction override`,
    replaceInstructions: true,
  });
  assert.equal(result.cancelled, false);
  assert.equal(summaryCalls.length, 1);
  assert.equal(summaryHooks("branch_summary").length, 1);
  assertSummaryHooks("branch_summary");
  const captured = summaryPreparations[0];
  const preparation = captured.preparation;
  assert.equal(summaryAfterGuard[0].preparation, preparation);
  assert.equal(
    preparation.entriesToSummarize,
    captured.entries,
    "Host retains the entries array, so replace elements, not the array",
  );
  for (const type of [
    "message",
    "custom_message",
    "branch_summary",
    "compaction",
  ]) {
    const index = captured.elements.findIndex((entry) => entry.type === type);
    assert(index >= 0, `Missing tree entry type ${type}`);
    assert.notEqual(
      preparation.entriesToSummarize[index],
      captured.elements[index],
    );
  }
  for (let index = 0; index < captured.elements.length; index++) {
    const original = captured.elements[index];
    const transformed = preparation.entriesToSummarize[index];
    if (original.type === "message")
      assert.notEqual(transformed.message, original.message);
    if (original.type === "custom_message" && Array.isArray(original.content))
      assert.notEqual(transformed.content, original.content);
    if (original.type === "branch_summary") {
      assert.notEqual(transformed.details, original.details);
      for (const field of ["readFiles", "modifiedFiles"])
        assert.notEqual(transformed.details[field], original.details[field]);
    }
    if (original.type === "compaction")
      assert.equal(transformed.details, original.details);
  }
  const consumed = host.prepareBranchEntries(preparation.entriesToSummarize);
  assert(!JSON.stringify(consumed.messages).includes(marker));
  assert(
    !JSON.stringify([
      ...consumed.fileOps.read,
      ...consumed.fileOps.written,
      ...consumed.fileOps.edited,
    ]).includes(marker),
  );
  assert(JSON.stringify(captured.elements).includes(marker));
  const prompt = JSON.stringify(summaryCalls[0]);
  assert(!prompt.includes(marker));
  for (const text of [
    "[R] custom string",
    "[R] custom block",
    "[R] nested branch summary",
    "[R] nested compaction summary",
    "[R] instruction override",
    "[R] nested JSON",
  ])
    assert(prompt.includes(text), `Missing transformed tree text: ${text}`);
  assert(
    !prompt.includes("Use this EXACT format"),
    "Tree must honor replaceInstructions",
  );
  assert(result.summaryEntry.details.readFiles.includes("[R]/branch-read.txt"));
  assert(
    result.summaryEntry.details.modifiedFiles.includes(
      "[R]/branch-modified.txt",
    ),
  );
  assert(!JSON.stringify(result.summaryEntry).includes(marker));
  await assertHistoryUnchanged(before);
  const hooksBefore = hookRequests.length;
  const callsBefore = summaryCalls.length;
  // A deny reply proves that plain navigation does not consult the summary guard.
  hookReply = () => ({
    action: "deny",
    evaluations: [],
    reason: "No summaries",
  });
  const navigation = await session.navigateTree(before.entries.at(-1).id, {
    summarize: false,
  });
  assert.equal(navigation.cancelled, false);
  assert.equal(navigation.summaryEntry, undefined);
  assert.equal(hookRequests.length, hooksBefore);
  assert.equal(summaryCalls.length, callsBefore);
  await assertHistoryUnchanged(before);
}

async function summaryAbort(tree, failOpen) {
  configureSummary({ failOpen });
  process.env.AGENTO11Y_GUARDS_TIMEOUT_MS = "2147483647";
  await start({ seed: tree ? seedTree : seedCompaction });
  const before = await historySnapshot();
  const leaf = session.sessionManager.getLeafId();
  const evaluating = Promise.withResolvers();
  const response = Promise.withResolvers();
  hookReply = async () => {
    evaluating.resolve();
    await response.promise;
    return { action: "allow", evaluations: [] };
  };
  const running = tree
    ? session.navigateTree(treeTarget, { summarize: true }).then((result) => {
        assert.equal(result.cancelled, true);
      })
    : session.compact().then(
        () => assert.fail("Aborted compaction must not succeed"),
        (error) => assert.match(error.message, /Compaction cancelled/),
      );
  try {
    await evaluating.promise;
    const signal = summaryPreparations.at(-1).event.signal;
    assert.equal(signal.aborted, false);
    await Promise.all([running, session.abort()]);
    assert.equal(signal.aborted, true);
    assert.equal(session.isCompacting, false);
    assert.equal(session.isIdle, true);
    assert.equal(summaryCalls.length, 0);
    assert.equal(session.sessionManager.getLeafId(), leaf);
    assert.deepEqual(session.sessionManager.getEntries(), before.entries);
    await assertHistoryUnchanged(before);
  } finally {
    response.resolve();
  }
}

async function summaryPolicy(tree, verdict, failOpen) {
  configureSummary({ failOpen });
  await start({
    seed: (manager) => {
      manager.appendMessage({
        role: "user",
        content: "Root request",
        timestamp: Date.now(),
      });
      treeTarget = manager.appendMessage(assistant("Root answer"));
      manager.appendMessage({
        role: "user",
        content: `${marker} summary input`,
        timestamp: Date.now(),
      });
      manager.appendMessage(assistant(`${marker} summary answer`));
      if (!tree)
        manager.appendMessage({
          role: "user",
          content: "Keep recent input",
          timestamp: Date.now(),
        });
    },
  });
  const before = await historySnapshot();
  const leaf = session.sessionManager.getLeafId();
  if (verdict === "failure") hookStatus = 503;
  else if (verdict === "deny")
    hookReply = () => ({
      action: "deny",
      reason: "Summary denied",
      evaluations: [],
    });
  else if (verdict === "invalid")
    hookReply = (body) => ({
      action: "allow",
      evaluations: [],
      transformed_input: {
        messages: body.input.messages.map((message, index) =>
          index === 0 ? { ...message, name: "wrong-correspondence" } : message,
        ),
      },
    });
  const allowed = verdict === "failure" && failOpen;
  if (tree) {
    const result = await session.navigateTree(treeTarget, { summarize: true });
    assert.equal(result.cancelled, !allowed);
  } else if (allowed) await session.compact();
  else
    await assert.rejects(
      session.compact(
        verdict === "instructions" ? `${marker} focus` : undefined,
      ),
      /Compaction cancelled/,
    );
  assert.equal(summaryCalls.length, allowed ? 1 : 0);
  assertSummaryHooks(tree ? "branch_summary" : "compaction");
  if (!allowed) {
    assert.equal(session.sessionManager.getLeafId(), leaf);
    assert.deepEqual(session.sessionManager.getEntries(), before.entries);
    if (!tree) {
      const end = sessionEvents.findLast(
        (event) => event.type === "compaction_end",
      );
      assert.equal(end.aborted, true);
      assert.equal(end.result, undefined);
      assert.equal(end.errorMessage, undefined);
    }
  }
  await assertHistoryUnchanged(before);
}

async function summaryAutomatic(reason, cancel) {
  configureSummary({
    respond: ({ summary }) => {
      if (summary) return;
      if (ordinaryCalls.length === 1)
        return reason === "threshold"
          ? { usage: { ...usage, input: 32700, totalTokens: 32702 } }
          : { stopReason: "error", errorMessage: "context_length_exceeded" };
    },
  });
  if (cancel) {
    const transform = hookReply;
    hookReply = (body) =>
      body.context.tags?.["pi.call_kind"] === "compaction"
        ? { action: "deny", evaluations: [], reason: "No summary" }
        : transform(body);
  }
  await start({ seed: seedCompaction, compaction: { enabled: true } });
  const before = await historySnapshot();
  await session.prompt("Ordinary request after seeded history");
  const preparations = summaryPreparations.filter(
    ({ event }) => event.type === "session_before_compact",
  );
  assert.equal(preparations.length, 1);
  assert.equal(preparations[0].event.reason, reason);
  assert.equal(preparations[0].event.willRetry, reason === "overflow");
  assertSummaryHooks("compaction");
  assert.equal(summaryCalls.length > 0, !cancel);
  if (!cancel) {
    for (const context of summaryCalls)
      assert(!JSON.stringify(context).includes(marker));
    assert(
      summaryCalls.some((context) => JSON.stringify(context).includes("[R]")),
    );
  }
  assert.equal(ordinaryCalls.length, reason === "overflow" && !cancel ? 2 : 1);
  const end = sessionEvents.findLast(
    (event) => event.type === "compaction_end",
  );
  assert.equal(end.reason, reason);
  assert.equal(end.aborted, cancel);
  assert.equal(Boolean(end.result), !cancel);
  if (reason === "overflow") {
    const entries = session.sessionManager.getEntries();
    const failed = entries.find(
      (entry) =>
        entry.type === "message" &&
        entry.message.errorMessage === "context_length_exceeded",
    );
    assert(failed, "Raw failed response must remain in history");
    const edit = entries.find(
      (entry) => entry.type === "context_edit" && entry.targetId === failed.id,
    );
    assert(edit, "Recovery omission must persist even when the guard cancels");
    assert.equal(edit.replacement, null);
    assert(
      !session.sessionManager
        .buildSessionProjection()
        .messages.some(
          (message) => message.errorMessage === "context_length_exceeded",
        ),
    );
    const disk = await readFile(session.sessionFile, "utf8");
    assert(disk.includes(edit.id));
  }
  if (cancel) {
    const callsBefore = ordinaryCalls.length;
    await session.prompt("Following ordinary call must still run");
    assert.equal(ordinaryCalls.length, callsBefore + 1);
    assert.equal(session.getLastAssistantText(), "Ordinary answer");
    assert.equal(summaryCalls.length, 0);
    assert.equal(
      session.sessionManager
        .getEntries()
        .filter((entry) => entry.type === "compaction").length,
      before.entries.filter((entry) => entry.type === "compaction").length,
    );
  }
  await assertHistoryUnchanged(before);
}

async function summaryNavigation() {
  configureSummary();
  await start({ seed: seedTree });
  hookReply = () => ({
    action: "deny",
    evaluations: [],
    reason: "No summaries",
  });
  const before = await historySnapshot();
  const result = await session.navigateTree(treeTarget, { summarize: false });
  assert.equal(result.cancelled, false);
  assert.equal(result.summaryEntry, undefined);
  assert.equal(session.sessionManager.getLeafId(), treeTarget);
  assert.equal(
    summaryPreparations.length,
    1,
    "Host must emit the navigation hook",
  );
  assert.equal(summaryPreparations[0].before.userWantsSummary, false);
  assert.equal(
    hookRequests.length,
    0,
    "Plain navigation must not evaluate a summary guard",
  );
  assert.equal(summaryCalls.length, 0);
  assert.deepEqual(session.sessionManager.getEntries(), before.entries);
  await assertHistoryUnchanged(before);
}

async function summaryManualAbort() {
  let started;
  const active = new Promise((resolveStarted) => {
    started = resolveStarted;
  });
  let aborted = false;
  configureSummary();
  const respond = streamSimple;
  streamSimple = (model, context, options) => {
    if (options.cacheRetention === "none")
      return respond(model, context, options);
    ordinaryCalls.push(structuredClone(context));
    const stream = new AssistantMessageEventStream();
    options.signal.addEventListener(
      "abort",
      () => {
        aborted = true;
        summaryOrder.push("abort");
        const message = assistant("Interrupted", {
          api: model.api,
          stopReason: "aborted",
        });
        stream.push({ type: "error", reason: "aborted", error: message });
        stream.end(message);
      },
      { once: true },
    );
    started();
    return stream;
  };
  const transform = hookReply;
  hookReply = (body) => {
    if (body.context.tags?.["pi.call_kind"] !== "compaction")
      return transform(body);
    assert(
      aborted,
      "Manual compaction must abort the active call before preflight",
    );
    assert.equal(events.at(-1).type, "agent_settled");
    summaryOrder.push("summary-preflight");
    return { action: "deny", evaluations: [], reason: "No summary" };
  };
  await start({ seed: seedCompaction });
  const before = await historySnapshot();
  const running = session.prompt("Interrupted by manual summary");
  await active;
  await assert.rejects(session.compact(), /Compaction cancelled/);
  await running;
  assert(aborted);
  assert(
    summaryOrder.indexOf("abort") < summaryOrder.indexOf("summary-preflight"),
  );
  assert.equal(summaryCalls.length, 0);
  assert.equal(
    ordinaryCalls.length,
    1,
    "Manual cancellation must not retry the interrupted turn",
  );
  assertSummaryHooks("compaction");
  await assertHistoryUnchanged(before);
}

const scenarios = {
  ...Object.fromEntries(
    ["compact", "tree"].flatMap((kind) =>
      ["strict", "open"].map((policy) => [
        `summary-abort-${kind}-${policy}`,
        () => summaryAbort(kind === "tree", policy === "open"),
      ]),
    ),
  ),
  "summary-manual": () => summaryCompaction(),
  "summary-split": () => summaryCompaction(true),
  "summary-virtual": () => summaryCompaction(false, true),
  "summary-tree": summaryTree,
  "summary-navigation": summaryNavigation,
  "summary-manual-abort": summaryManualAbort,
  "summary-threshold": () => summaryAutomatic("threshold", false),
  "summary-threshold-cancel": () => summaryAutomatic("threshold", true),
  "summary-overflow": () => summaryAutomatic("overflow", false),
  "summary-overflow-cancel": () => summaryAutomatic("overflow", true),
  "ordinary-full": () => ordinary("full"),
  "ordinary-metadata": () => ordinary("metadata_only"),
  "ordinary-no-tools": () => ordinary("no_tool_content"),
  "ordinary-metadata-spans": () => ordinary("full_with_metadata_spans"),
  "guard-startup-disabled": () => guardStartup(false, false),
  "guard-startup-open": () => guardStartup(true, true),
  "guard-startup-strict": () => guardStartup(true, false),
  async "file-loader"() {
    const before = failures.length;
    for (const attempt of [
      () => net.connect({ host: "192.0.2.1", port: 443 }),
      () => net.connect({ host: "127.0.0.1", port: 0 }),
      () => net.connect({ path: join(cwd, "unexpected.sock") }),
      () => globalThis.fetch("https://example.invalid"),
      () => dgram.createSocket("udp4"),
      () => createSocket("udp4"),
    ]) {
      assert.throws(attempt, /Unexpected network request/);
      assert.equal(failures.length, before + 1);
      failures.pop();
    }
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    answer = (_body, n) => {
      assert(n <= 2, "Unexpected model request");
      return `native answer ${n}`;
    };
    await start();
    await session.prompt("native user prompt");
    assert.equal(session.getLastAssistantText(), "native answer 1");
    await session.sendCustomMessage(
      { customType: "native", content: "native custom prompt", display: false },
      { triggerTurn: true },
    );
    assert.equal(session.getLastAssistantText(), "native answer 2");
    assert.deepEqual(
      events
        .filter((event) => event.type === "before_agent_start")
        .map((event) => event.messageType),
      ["user", "custom"],
    );
    await shutdown();
    assert.equal(requests.length, 2);
    assert.equal(generations.length, 2);
    assert(JSON.stringify(generations[0].input).includes("native user prompt"));
    assert(telemetry.traces.length > 0);
    assert(telemetry.metrics.length > 0);
  },
  async reload(t) {
    const config = join(process.env.XDG_CONFIG_HOME, "agento11y/config.env");
    await mkdir(dirname(config), { recursive: true });
    delete process.env.AGENTO11Y_ENDPOINT;
    delete process.env.SIGIL_ENDPOINT;
    process.env.SIGIL_QUEUE_SIZE = "7";
    await writeFile(
      config,
      `AGENTO11Y_CONTENT_CAPTURE_MODE=full\nAGENTO11Y_ENDPOINT=${endpoint}/first\nAGENTO11Y_QUEUE_SIZE=999\n`,
    );
    answer = (_body, n) => {
      assert(n <= 2, "Unexpected model request");
      return "reload answer";
    };
    await start();
    assert.equal(process.env.AGENTO11Y_CONTENT_CAPTURE_MODE, "full");
    assert.equal(process.env.AGENTO11Y_QUEUE_SIZE, "7");
    await session.prompt("before reload");
    await session.reload();
    const evaluations = () =>
      session.sessionManager
        .getEntries()
        .filter((entry) => entry.customType === "native-loader")
        .map((entry) => entry.data.evaluation);
    await t.test(
      "same-path JavaScript reload reimports modules",
      { todo: "Pi 1.4.0 native ESM loader retains the module cache" },
      () => {
        assert.deepEqual(evaluations(), [1, 2]);
      },
    );
    const previous = checkLoader();
    const previousHandlers = previous.extensions.find(
      (extension) => extension.path === loadedExtensionPath,
    ).handlers;
    await writeFile(
      config,
      `AGENTO11Y_CONTENT_CAPTURE_MODE=metadata_only\nAGENTO11Y_ENDPOINT=${endpoint}/second\n`,
    );
    replaceFiles = true;
    await session.reload();
    const replacement = checkLoader();
    assert.notEqual(replacement.runtime, previous.runtime);
    assert.notEqual(
      replacement.extensions.find(
        (extension) => extension.path === loadedExtensionPath,
      ).handlers,
      previousHandlers,
    );
    assert.deepEqual(evaluations(), [1, 1, 2]);
    assert.equal(process.env.AGENTO11Y_CONTENT_CAPTURE_MODE, "metadata_only");
    assert.equal(process.env.AGENTO11Y_ENDPOINT, `${endpoint}/second`);
    await session.prompt("after reload");
    await rm(config);
    await session.reload();
    for (const prefix of ["AGENTO11Y", "SIGIL"]) {
      assert.equal(process.env[`${prefix}_CONTENT_CAPTURE_MODE`], undefined);
      assert.equal(process.env[`${prefix}_ENDPOINT`], undefined);
      assert.equal(process.env[`${prefix}_QUEUE_SIZE`], "7");
    }
    await writeFile(
      config,
      `AGENTO11Y_CONTENT_CAPTURE_MODE=full\nAGENTO11Y_ENDPOINT=${endpoint}/first\n`,
    );
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "no_tool_content";
    await session.reload();
    assert.equal(process.env.AGENTO11Y_CONTENT_CAPTURE_MODE, "no_tool_content");
    await shutdown();
    assert.equal(requests.length, 2);
    assert.equal(generations.length, 2);
    assert.deepEqual(exportPaths, [
      "/first/api/v1/generations:export",
      "/second/api/v1/generations:export",
    ]);
    assert(
      !JSON.stringify(generations[1].input ?? []).includes("after reload"),
    );
  },
  async lifecycle(t) {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    retry = true;
    let calls = 0;
    let signalAborted = false;
    let started;
    const active = new Promise((resolveStarted) => {
      started = resolveStarted;
    });
    streamSimple = (model, _context, options) => {
      calls++;
      assert(calls <= 3, "Unexpected synthetic model request");
      const stream = new AssistantMessageEventStream();
      const finish = (stopReason) => {
        const message = {
          role: "assistant",
          api: model.api,
          provider: model.provider,
          model: model.id,
          timestamp: Date.now(),
          content: [{ type: "text", text: "synthetic answer" }],
          usage,
          stopReason,
          ...(stopReason === "error" ? { errorMessage: "503 overloaded" } : {}),
        };
        stream.push({
          type: "start",
          partial: { ...message, stopReason: "pending" },
        });
        stream.push(
          stopReason === "error" || stopReason === "aborted"
            ? { type: "error", reason: stopReason, error: message }
            : { type: "done", reason: stopReason, message },
        );
        stream.end(message);
      };
      if (calls === 3) {
        options.signal.addEventListener(
          "abort",
          () => {
            signalAborted = true;
            queueMicrotask(() => finish("aborted"));
          },
          { once: true },
        );
        started();
      } else {
        queueMicrotask(() => finish(calls === 1 ? "error" : "stop"));
      }
      return stream;
    };
    await start();
    await session.prompt("retry prompt");
    assert.equal(calls, 2);
    assert.equal(
      events.filter((event) => event.type === "before_agent_start").length,
      1,
    );
    assert.equal(
      events.filter((event) => event.type === "agent_start").length,
      2,
    );
    assert.equal(events.at(-1).type, "agent_settled");
    const interrupted = session.prompt("interrupted by manual compaction");
    await active;
    await assert.rejects(session.compact(), /Compaction cancelled/);
    await interrupted;
    assert(signalAborted);
    const preparation = events.findIndex(
      (event) => event.type === "session_before_compact",
    );
    assert(
      preparation >
        events.findLastIndex((event) => event.type === "agent_settled"),
      "Manual compaction must await abort before its preparation hook",
    );
    assert.equal(calls, 3);
    await shutdown();
    assert.equal(requests.length, 0);
    assert.equal(generations.length, 3);
    await t.test(
      "retry omits unverified request prompts",
      { todo: "Request-field accuracy belongs to the dependent ticket" },
      () => {
        assert.equal(
          generations
            .slice(0, 2)
            .filter((generation) => generation.system_prompt).length,
          0,
        );
      },
    );
  },
};

for (const tree of [false, true])
  for (const verdict of [
    "deny",
    "invalid",
    "failure",
    ...(tree ? [] : ["instructions"]),
  ])
    for (const failOpen of [false, true])
      scenarios[
        `summary-policy-${tree ? "tree" : "compact"}-${verdict}-${failOpen ? "open" : "strict"}`
      ] = () => summaryPolicy(tree, verdict, failOpen);

function expectedDiagnostics() {
  if (scenario.startsWith("guard-startup-"))
    return [
      "error local capture unavailable LocalReceiverError: no local receiver is running; start one with `agento11y local start`",
      ...(scenario === "guard-startup-open"
        ? [
            "warn Agent Observability: guards are unavailable; tool calls are allowed by fail-open configuration.",
          ]
        : []),
    ];
  if (scenario.match(/^summary-policy-(compact|tree)-failure-open$/))
    return [
      "warn Agent Observability: summary guard evaluation failed; continuing with fail-open enabled.",
    ];
  if (
    scenario.match(
      /^summary-policy-(compact|tree)-(deny|invalid|failure|instructions)-(open|strict)$/,
    ) ||
    [
      "summary-manual-abort",
      "summary-threshold-cancel",
      "summary-overflow-cancel",
    ].includes(scenario)
  ) {
    // The continuation prompt repeats threshold compaction against the unchanged oversized history.
    return Array(scenario === "summary-threshold-cancel" ? 2 : 1).fill(
      "warn Agent Observability: summary cancelled because its guard evaluation or required transformation could not be applied.",
    );
  }
  return [];
}

await test(`native ${scenario}`, async (t) => {
  try {
    assert(scenarios[scenario], `Unknown native scenario: ${scenario}`);
    await scenarios[scenario](t);
  } finally {
    try {
      await shutdown();
    } catch (error) {
      failures.push(error);
    } finally {
      pluginTimers.close();
      server.closeAllConnections();
      await new Promise((resolveClose, reject) =>
        server.close((error) => (error ? reject(error) : resolveClose())),
      );
    }
  }
  await assertPluginDiagnostics(pluginLog, expectedDiagnostics());
  assert.deepEqual(failures, [], "Extension errors or unexpected requests");
});
