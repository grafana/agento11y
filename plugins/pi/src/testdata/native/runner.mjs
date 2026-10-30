import assert from "node:assert/strict";
import { createHash } from "node:crypto";
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
let responseModel = "native-model";
const exportRequests = [];
let exportReply = ({ body }) => ({
  status: 200,
  body: {
    results: body.generations.map((generation) => ({
      generation_id: generation.id,
      accepted: true,
    })),
  },
});
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
      exportRequests.push({ body, headers: req.headers });
      const reply = await exportReply({ body, req, res });
      // Held responses are released on socket close too, so aborted exports
      // leave neither fixture timers nor an unresolved server handler behind.
      if (!reply || res.destroyed) return;
      res.writeHead(reply.status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(reply.body));
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
          model: responseModel,
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
let sessionRuntime;
const lifecycleObservations = [];
let streamSimple;
let executedTools = 0;
let nativeTools;
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
        contextWindow: options.contextWindow ?? 32768,
        maxTokens: 1000,
      },
    ],
    ...(streamSimple ? { streamSimple } : {}),
  };
  if (options.historyModel)
    provider.models.push({
      ...provider.models[0],
      id: "history-model",
      contextWindow: 32768,
    });
  modelRuntime.registerProvider("native-loopback", provider);
  if (options.virtual)
    modelRuntime.registerVirtualModel({
      provider: "native-loopback",
      id: "virtual-model",
      name: "Native virtual model",
      contextWindow: 32768,
      maxTokens: 1000,
      route: async (request) => {
        await options.beforeRoute?.(request);
        return {
          model: modelRuntime.getModel("native-loopback", "native-model"),
          thinkingLevel: "off",
        };
      },
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
          nativeTools?.register(pi);
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
            "turn_start",
            "session_before_compact",
            "session_compact",
            "session_compact_failed",
            "session_before_fork",
            "session_tree",
          ])
            pi.on(type, (event, ctx) => {
              events.push(structuredClone(event));
              lifecycleObservations.push({
                type,
                at: Date.now(),
                ctx,
                manager: ctx.sessionManager,
                conversationId: ctx.sessionManager.getSessionId(),
              });
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
  const sessionOptions = {
    model: modelRuntime.getModel(
      "native-loopback",
      options.virtual ? "virtual-model" : "native-model",
    ),
    thinkingLevel: "off",
    noTools: true,
  };
  const bind = async (replacement) => {
    session = replacement;
    session.subscribe((event) => sessionEvents.push(event));
    await session.bindExtensions({
      mode: "print",
      onError: (error) => failures.push(error),
    });
  };
  if (options.runtime) {
    let initial = true;
    sessionRuntime = await host.createAgentSessionRuntime(
      async ({
        cwd: runtimeCwd,
        agentDir,
        sessionManager,
        sessionStartEvent,
      }) => {
        if (!initial) await resourceLoader.reload();
        initial = false;
        const services = {
          cwd: runtimeCwd,
          agentDir,
          modelRuntime,
          settingsManager,
          resourceLoader,
          diagnostics: [],
        };
        return {
          ...(await host.createAgentSessionFromServices({
            services,
            sessionManager,
            sessionStartEvent,
            ...sessionOptions,
          })),
          services,
          diagnostics: [],
        };
      },
      { cwd, agentDir: process.env.PI_CODING_AGENT_DIR, sessionManager },
    );
    sessionRuntime.setRebindSession(bind);
    await bind(sessionRuntime.session);
  } else {
    const result = await host.createAgentSession({
      cwd,
      agentDir: process.env.PI_CODING_AGENT_DIR,
      modelRuntime,
      settingsManager,
      sessionManager,
      resourceLoader,
      ...sessionOptions,
    });
    await bind(result.session);
  }
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
  if (sessionRuntime) {
    await sessionRuntime.dispose();
    sessionRuntime = undefined;
  } else {
    await emitSessionShutdownEvent(session.extensionRunner, {
      type: "session_shutdown",
      reason: "exit",
    });
    session.dispose();
  }
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
  configureSummary({
    respond: ({ summary }) =>
      summary
        ? {
            usage: {
              ...usage,
              cacheWrite: 4,
              cacheWrite1h: 2,
              totalTokens: 16,
            },
          }
        : undefined,
  });
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
  await shutdown();
  const summaries = generations.filter(isSummaryGeneration);
  assert.equal(summaries.length, 1);
  const calls = split ? 2 : 1;
  assert.equal(Number(summaries[0].usage.cache_write_input_tokens), 4 * calls);
  assert.equal(Number(summaries[0].usage.total_tokens), 16 * calls);
  assert.equal(
    summaries[0].metadata["pi.usage.cache_write_1h_input_tokens"],
    2 * calls,
  );
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

const toolModes = {
  full: "full",
  metadata: "metadata_only",
  "no-tools": "no_tool_content",
  "metadata-spans": "full_with_metadata_spans",
};
const toolCases = [
  "sequential",
  "parallel",
  "repeated-ids",
  "nested",
  "transformed",
  "later-changes",
  "finalized-result",
  "result-delay",
  "blocked-invalid",
  "mutated-input",
];

function otlpSpans() {
  return telemetry.traces.flatMap((body) =>
    body.resourceSpans.flatMap((resource) =>
      resource.scopeSpans.flatMap((scope) =>
        scope.spans.map((span) => ({
          ...span,
          attributes: Object.fromEntries(
            (span.attributes ?? []).map(({ key, value }) => [
              key,
              value.stringValue ?? value.boolValue ?? value.intValue,
            ]),
          ),
        })),
      ),
    ),
  );
}

async function tools(kind, mode) {
  process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = mode;
  const guarded = ["transformed", "repeated-ids", "blocked-invalid"].includes(
    kind,
  );
  process.env.AGENTO11Y_GUARDS_ENABLED = String(guarded);
  const nested = ["nested", "finalized-result", "result-delay"].includes(kind);
  const observations = [];
  const entries = [];
  const gate = Promise.withResolvers();
  let arrivals = 0;
  const rendezvous = async (count) => {
    if (++arrivals === count) gate.resolve();
    await gate.promise;
  };
  const call = (id, value, name = "native_action") => ({
    type: "toolCall",
    id,
    name,
    arguments: { value },
  });
  let batches = [[call("native-one", `${kind}-proposal-body`)]];
  if (kind === "sequential" || kind === "parallel")
    batches = [
      [call("native-one", "first-body"), call("native-two", "second-body")],
    ];
  if (kind === "repeated-ids")
    batches = [
      [call("reused-id", "repeat-proposal-one")],
      [call("reused-id", "repeat-proposal-two")],
    ];
  if (nested) batches = [[call("root", "root-body")]];
  if (kind === "transformed")
    batches = [
      [
        {
          type: "toolCall",
          id: "native-one",
          name: "native_action",
          arguments: {
            legacy: "unmatched-proposal-body",
            removed: "removed-proposal-body",
          },
        },
      ],
    ];
  if (kind === "blocked-invalid")
    batches = [
      [
        call("guard-blocked", "guard-blocked-body"),
        call("later-blocked", "later-blocked-body"),
        { ...call("invalid", "unused"), arguments: {} },
        call("unknown", "unknown-body", "native_missing"),
      ],
    ];

  hookReply = (body) => {
    if (body.phase !== "postflight")
      return { action: "allow", evaluations: [] };
    const proposed = body.input.output[0].parts[0].tool_call;
    if (proposed.id === "guard-blocked")
      return { action: "deny", evaluations: [], reason: "Native tool policy" };
    if (
      kind !== "transformed" &&
      !(
        kind === "repeated-ids" &&
        proposed.input_json.value === "repeat-proposal-one"
      )
    )
      return { action: "allow", evaluations: [] };
    return {
      action: "allow",
      evaluations: [],
      transformed_input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                tool_call: {
                  id: proposed.id,
                  name: proposed.name,
                  input_json: JSON.stringify(
                    kind === "transformed"
                      ? { value: "guard-body", added: "guard-only-body" }
                      : { value: "repeat-guard-one" },
                  ),
                },
              },
            ],
          },
        ],
      },
    };
  };
  const content = (text) => [{ type: "text", text }];
  const observe = (event) => observations.push(structuredClone(event));
  nativeTools = {
    register(pi) {
      for (const type of ["tool_execution_start", "tool_execution_end"])
        pi.on(type, observe);
      pi.on("tool_call", (event) => {
        observe(event);
        if (kind === "later-changes") event.input.value = "later-call-body";
        if (event.toolCallId === "later-blocked")
          return { block: true, reason: "Later native policy" };
      });
      pi.on("tool_result", async (event) => {
        observe(event);
        if (kind === "result-delay" && !event.parentToolCallId) {
          observations.push({
            type: "result-delay-start",
            toolCallId: event.toolCallId,
          });
          await new Promise((resolveDelay) => setTimeout(resolveDelay, 100));
          observations.push({
            type: "result-delay-end",
            toolCallId: event.toolCallId,
          });
        }
        if (kind === "later-changes")
          return { content: content("later-result-body"), isError: true };
        if (kind === "finalized-result")
          return {
            content: content(
              event.parentToolCallId
                ? `later-result:${event.toolCallId}`
                : "earlier-top-level-body",
            ),
            isError: !event.parentToolCallId,
          };
      });
      pi.on("message_end", ({ message }) => {
        if (kind !== "finalized-result" || message.role !== "toolResult")
          return;
        observe({ type: "finalized-result", message });
        return {
          message: {
            ...message,
            content: content("final-transcript-body"),
            isError: false,
          },
        };
      });
      for (const name of [
        "native_action",
        ...(nested ? ["native_branch", "native_leaf"] : []),
      ])
        pi.registerTool({
          name,
          label: name,
          description: "Native tool lifecycle fixture",
          parameters: {
            type: "object",
            properties: { value: { type: "string" } },
            required: ["value"],
          },
          executionMode: kind === "sequential" ? "sequential" : "parallel",
          ...(kind === "transformed"
            ? {
                prepareArguments: () => ({
                  value: "prepared-body",
                  extra: "prepared-only-body",
                }),
              }
            : {}),
          async execute(id, input, _signal, _update, ctx) {
            entries.push({ id, input: structuredClone(input) });
            observations.push({ type: "entry", toolCallId: id });
            if (kind === "parallel") await rendezvous(2);
            if (nested) {
              let outcomes;
              if (name === "native_action")
                outcomes = await Promise.all([
                  ctx.executeTool("native_branch", { value: "branch-body" }),
                  ctx.executeTool("native_leaf", { value: "sibling-body" }),
                ]);
              else if (name === "native_branch")
                outcomes = await Promise.all([
                  ctx.executeTool("native_leaf", {
                    value: "grandchild-a-body",
                  }),
                  ctx.executeTool("native_leaf", {
                    value: "grandchild-b-body",
                  }),
                ]);
              else await rendezvous(3);
              for (const outcome of outcomes ?? [])
                assert.equal(outcome.isError, false);
            }
            if (kind === "mutated-input") {
              input.value = "post-execution-body";
              input.added = "tool-added-body";
            }
            await Promise.resolve();
            observations.push({ type: "exit", toolCallId: id });
            const units =
              name === "native_leaf" ? 1 : name === "native_branch" ? 2 : 4;
            return {
              content: content(`result:${input.value}`),
              ...(nested
                ? {
                    usage: {
                      input: units,
                      output: units,
                      reasoning: units,
                      cacheRead: units,
                      cacheWrite: 2 * units,
                      cacheWrite1h: units,
                      totalTokens: 5 * units,
                      cost: {
                        input: 0,
                        output: 0,
                        cacheRead: 0,
                        cacheWrite: 0,
                        total: units,
                      },
                    },
                  }
                : {}),
            };
          },
        });
    },
  };
  let modelCalls = 0;
  streamSimple = (model) => {
    assert(
      modelCalls <= batches.length,
      "Unexpected native tool model request",
    );
    const batch = batches[modelCalls++];
    const message = assistant(batch ?? "Tool fixture finished", {
      api: model.api,
      stopReason: batch ? "toolUse" : "stop",
      ...(nested
        ? {
            usage: {
              ...usage,
              cacheWrite: 5,
              cacheWrite1h: 2,
              totalTokens: 17,
            },
          }
        : {}),
    });
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
  await session.prompt("Observe native tool execution");
  const transcript = structuredClone(session.sessionManager.getEntries());
  await shutdown();
  assert.equal(requests.length, 0);
  assert.equal(modelCalls, batches.length + 1);
  assert.equal(generations.length, batches.length + 1);
  const starts = observations.filter(
    (event) => event.type === "tool_execution_start",
  );
  const ends = observations.filter(
    (event) => event.type === "tool_execution_end",
  );
  const results = observations.filter((event) => event.type === "tool_result");
  assert.equal(starts.length, nested ? 5 : batches.flat().length);
  assert.equal(ends.length, starts.length);
  assert.equal(entries.length, kind === "blocked-invalid" ? 0 : starts.length);
  assert.equal(results.length, entries.length);
  const position = (type, id) =>
    observations.findIndex(
      (event) => event.type === type && event.toolCallId === id,
    );
  if (kind === "sequential")
    assert(
      position("tool_execution_end", "native-one") <
        position("entry", "native-two"),
    );
  if (kind === "parallel") {
    assert.equal(arrivals, 2);
    for (const id of ["native-one", "native-two"])
      assert(
        position("entry", id) <
          observations.findIndex((event) => event.type === "exit"),
      );
  }
  if (nested) {
    const top = transcript.filter(
      (entry) =>
        entry.type === "message" && entry.message.role === "toolResult",
    );
    assert.equal(top.length, 1);
    assert.equal(top[0].message.usage.totalTokens, 45);
    for (const field of [
      "input_tokens",
      "output_tokens",
      "reasoning_tokens",
      "cache_read_input_tokens",
      "cache_write_1h_input_tokens",
      "cost_usd",
    ])
      assert.equal(generations[0].metadata[`pi.tool_usage.${field}`], 9);
    assert.equal(
      generations[0].metadata["pi.tool_usage.cache_write_input_tokens"],
      18,
    );
    assert.equal(generations[0].metadata["pi.tool_usage.total_tokens"], 45);
    for (const generation of generations) {
      assert.equal(Number(generation.usage.input_tokens), 10);
      assert.equal(Number(generation.usage.output_tokens), 2);
      assert.equal(Number(generation.usage.cache_write_input_tokens), 5);
      assert.equal(Number(generation.usage.total_tokens), 17);
      assert.equal(generation.metadata.cost_usd, 0);
      assert.equal(
        generation.metadata["pi.usage.cache_write_1h_input_tokens"],
        2,
      );
    }
    assert(
      !Object.keys(generations[1].metadata).some((key) =>
        key.startsWith("pi.tool_usage."),
      ),
    );
    assert.equal(arrivals, 3);
    assert.equal(starts.filter((event) => event.parentToolCallId).length, 4);
    for (const event of starts.filter((event) => event.parentToolCallId)) {
      assert(
        position("tool_execution_end", event.toolCallId) <
          position("tool_execution_end", event.parentToolCallId),
      );
      assert(
        !transcript.some(
          (entry) =>
            entry.type === "message" &&
            entry.message.role === "toolResult" &&
            entry.message.toolCallId === event.toolCallId,
        ),
      );
    }
  }
  if (kind === "transformed") {
    const postflight = hookRequests.find((body) => body.phase === "postflight");
    assert.deepEqual(postflight.input.output[0].parts[0].tool_call.input_json, {
      value: "prepared-body",
      extra: "prepared-only-body",
    });
    assert.deepEqual(entries[0].input, {
      value: "guard-body",
      added: "guard-only-body",
    });
  }
  if (kind === "repeated-ids")
    assert.deepEqual(
      entries.map((entry) => entry.input.value),
      ["repeat-guard-one", "repeat-proposal-two"],
    );
  if (kind === "mutated-input") {
    assert.equal(entries[0].input.value, "mutated-input-proposal-body");
    assert.deepEqual(results[0].input, {
      value: "post-execution-body",
      added: "tool-added-body",
    });
  }
  if (kind === "later-changes") {
    assert.equal(entries[0].input.value, "later-call-body");
    assert.equal(results[0].content[0].text, "result:later-call-body");
    assert.equal(ends[0].result.content[0].text, "later-result-body");
    assert.equal(ends[0].isError, true);
  }
  if (kind === "blocked-invalid") {
    assert(
      ends.every((event) => event.isError && event.durationMs === undefined),
    );
    assert.deepEqual(
      observations
        .filter((event) => event.type === "tool_call")
        .map((event) => event.toolCallId)
        .sort(),
      ["later-blocked"],
    );
  }

  const spans = otlpSpans();
  const toolSpans = spans.filter(
    (span) => span.attributes["gen_ai.operation.name"] === "execute_tool",
  );
  assert.equal(toolSpans.length, starts.length);
  assert.equal(
    new Set(toolSpans.map((span) => span.spanId)).size,
    starts.length,
  );
  const privateValues = [
    ...batches.flat().map((proposed) => proposed.arguments),
    ...entries.map((entry) => entry.input),
    ...results.map((result) => result.input),
  ].flatMap((input) =>
    Object.values(input).filter((value) => typeof value === "string"),
  );
  for (const value of privateValues) {
    if (mode !== "full")
      assert(
        !JSON.stringify(spans).includes(value),
        `Tool body leaked into stripped spans: ${value}`,
      );
    if (mode === "metadata_only" || mode === "no_tool_content")
      assert(
        !JSON.stringify(generations).includes(value),
        `Tool body leaked into stripped generations: ${value}`,
      );
  }
  const generationBodies =
    mode === "full" || mode === "full_with_metadata_spans";
  for (const [batchIndex, batch] of batches.entries()) {
    const generation = generations[batchIndex];
    const parent = spans.find((span) => span.spanId === generation.span_id);
    assert(parent, "Generation must have a real exported OTLP span");
    const calls = generation.output
      .flatMap((message) => message.parts ?? [])
      .flatMap((part) => (part.tool_call ? [part.tool_call] : []));
    assert.deepEqual(
      calls.map(({ id, name }) => ({ id, name })),
      batch.map(({ id, name }) => ({ id, name })),
    );
    for (const [index, proposed] of batch.entries()) {
      const callBody = calls[index].input_json;
      const changed =
        kind === "transformed" ||
        kind === "later-changes" ||
        kind === "mutated-input" ||
        kind === "blocked-invalid" ||
        (kind === "repeated-ids" && batchIndex === 0);
      if (!generationBodies || changed)
        assert(
          !callBody,
          "Uncertain proposal arguments must be omitted, not substituted",
        );
      else {
        assert(callBody, "Unchanged proposal arguments must be present");
        assert.deepEqual(
          JSON.parse(Buffer.from(callBody, "base64").toString("utf8")),
          proposed.arguments,
          "Unchanged proposal arguments must match the assistant proposal",
        );
      }
    }
    const batchSpans = toolSpans.filter(
      (span) =>
        span.traceId === generation.trace_id &&
        (span.parentSpanId === generation.span_id || nested),
    );
    const batchStarts = nested
      ? starts
      : starts.slice(
          batchIndex === 0 ? 0 : batches[0].length,
          (batchIndex === 0 ? 0 : batches[0].length) + batch.length,
        );
    assert.equal(batchSpans.length, batchStarts.length);
    for (const start of batchStarts) {
      const span = batchSpans.find(
        (span) => span.attributes["gen_ai.tool.call.id"] === start.toolCallId,
      );
      assert(span, `Missing tool span ${start.toolCallId}`);
      assert.equal(span.attributes["gen_ai.tool.name"], start.toolName);
      const expectedParent = start.parentToolCallId
        ? batchSpans.find(
            (candidate) =>
              candidate.attributes["gen_ai.tool.call.id"] ===
              start.parentToolCallId,
          )
        : parent;
      assert(expectedParent);
      assert.equal(span.parentSpanId, expectedParent.spanId);
      const occurrence = starts.indexOf(start);
      const end =
        kind === "repeated-ids"
          ? ends[occurrence]
          : ends.find((event) => event.toolCallId === start.toolCallId);
      const result =
        kind === "repeated-ids"
          ? results[occurrence]
          : results.find((event) => event.toolCallId === start.toolCallId);
      const finalized = start.parentToolCallId
        ? undefined
        : transcript.filter(
            (entry) =>
              entry.type === "message" &&
              entry.message.role === "toolResult" &&
              entry.message.toolCallId === start.toolCallId,
          )[kind === "repeated-ids" ? batchIndex : 0]?.message;
      assert(
        start.parentToolCallId || finalized,
        "Missing finalized top-level result",
      );
      const expectedResult = finalized ?? end.result;
      assert.equal(span.status.code, end.isError ? 2 : 1);
      const args = span.attributes["gen_ai.tool.call.arguments"];
      const output = span.attributes["gen_ai.tool.call.result"];
      if (mode === "full") {
        if (result)
          assert.deepEqual(
            JSON.parse(args),
            {
              observation: "tool_result.input",
              input: result.input,
            },
            "Tool span arguments must label the post-execution observation",
          );
        else
          assert.equal(
            args,
            undefined,
            "Unexecuted calls have no execution input",
          );
        assert.equal(
          JSON.parse(output),
          expectedResult.content
            .filter((part) => part.type === "text")
            .map((part) => part.text)
            .join("\n"),
        );
      } else {
        assert.equal(args, undefined);
        assert.equal(output, undefined);
      }
      if (!start.parentToolCallId) {
        const exported = generation.output
          .flatMap((message) => message.parts ?? [])
          .find(
            (part) => part.tool_result?.tool_call_id === start.toolCallId,
          )?.tool_result;
        assert(
          exported,
          "Top-level tool result structure must survive every capture mode",
        );
        assert.equal(exported.is_error ?? false, finalized.isError);
        assert.equal(
          exported.content ?? "",
          generationBodies
            ? finalized.content
                .filter((part) => part.type === "text")
                .map((part) => part.text)
                .join("\n")
            : "",
        );
      }
    }
  }
  if (kind === "finalized-result") {
    const replacements = observations.filter(
      (event) => event.type === "finalized-result",
    );
    assert.equal(replacements.length, 1);
    assert.equal(
      replacements[0].message.content[0].text,
      "earlier-top-level-body",
    );
    assert.equal(replacements[0].message.isError, true);
    const final = transcript.find(
      (entry) =>
        entry.type === "message" && entry.message.role === "toolResult",
    ).message;
    assert.equal(final.content[0].text, "final-transcript-body");
    assert.equal(final.isError, false);
    for (const end of ends.filter((event) => event.parentToolCallId))
      assert.equal(
        end.result.content[0].text,
        `later-result:${end.toolCallId}`,
      );
    for (const exported of [JSON.stringify(generations), JSON.stringify(spans)])
      assert(
        !exported.includes("earlier-top-level-body"),
        "Earlier top-level result must not be restored",
      );
  }
  if (kind === "result-delay") {
    assert(position("exit", "root") < position("result-delay-start", "root"));
    assert(
      position("result-delay-start", "root") <
        position("result-delay-end", "root"),
    );
    assert(
      position("result-delay-end", "root") <
        position("tool_execution_end", "root"),
    );
    for (const start of starts.filter((event) => event.parentToolCallId)) {
      assert(
        position("tool_execution_end", start.toolCallId) <
          position("result-delay-start", "root"),
      );
      const child = toolSpans.find(
        (span) => span.attributes["gen_ai.tool.call.id"] === start.toolCallId,
      );
      const parent = toolSpans.find(
        (span) =>
          span.attributes["gen_ai.tool.call.id"] === start.parentToolCallId,
      );
      assert(
        BigInt(parent.startTimeUnixNano) <= BigInt(child.startTimeUnixNano),
        "Awaited child must not start before its parent",
      );
    }
  }
  if (kind === "transformed")
    for (const body of [
      JSON.stringify(generations),
      JSON.stringify(telemetry.traces),
    ])
      for (const forbidden of [
        "unmatched-proposal-body",
        "removed-proposal-body",
        "prepared-only-body",
      ])
        assert(
          !body.includes(forbidden),
          `Leaked transformed proposal: ${forbidden}`,
        );
}

const stableGenerationId = (conversationId, entryId) =>
  `pi-${createHash("sha256").update(`${conversationId}\0${entryId}`).digest("hex").slice(0, 24)}`;

const lastAssistantEntry = () =>
  session.sessionManager
    .getBranch()
    .findLast(
      (entry) => entry.type === "message" && entry.message.role === "assistant",
    );

async function nextClockTick(after) {
  // Separate timestamps expose timer resets without imposing a duration threshold.
  while (Date.now() <= after)
    await new Promise((resolveTick) => setTimeout(resolveTick, 1));
}

const isSummaryGeneration = (generation) =>
  ["compaction", "branch_summary"].includes(generation.tags["pi.call_kind"]);

function assertSummaryAttribution() {
  for (const generation of generations.filter(isSummaryGeneration)) {
    assert.deepEqual(generation.model, {
      provider: "unknown",
      name: "unknown",
    });
    // Proto JSON encodes an absent response identity as an empty string.
    assert.equal(generation.response_model, "");
    assert.equal(
      generation.metadata["pi.summary.model_attribution"],
      "unknown",
    );
    assert.equal(generation.metadata["pi.summary.operation_aggregate"], true);
    assert.equal(
      generation.metadata["pi.summary.selected_model"],
      ["summary-virtual", "lifecycle-request-compaction"].includes(scenario)
        ? "virtual-model"
        : "native-model",
    );
    assert.equal(
      generation.metadata["pi.summary.selected_provider"],
      "native-loopback",
    );
    assert(Number(generation.usage.total_tokens) > 0);
  }
}

async function requestTimeCompaction() {
  process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
  configureSummary();
  await start({
    virtual: true,
    contextWindow: 4096,
    historyModel: true,
    compaction: { enabled: true },
    seed: (manager) => {
      manager.appendMessage({
        role: "user",
        content: "Old history ".repeat(500),
        timestamp: Date.now(),
      });
      manager.appendMessage(assistant("Old answer"));
      manager.appendMessage({
        role: "user",
        content: "Recent history",
        timestamp: Date.now(),
      });
      manager.appendMessage(
        assistant("Recent answer", {
          // Only the route to the smaller model must trigger compaction.
          model: "history-model",
          usage: { ...usage, input: 6000, totalTokens: 6002 },
        }),
      );
    },
    beforeRoute: async (request) => {
      if (request.reason !== "direct") {
        const turn = lifecycleObservations.findLast(
          (value) => value.type === "turn_start",
        );
        assert(turn, "Virtual routing must occur after the active turn starts");
        await nextClockTick(turn.at);
      }
    },
  });
  await session.prompt(
    "Active turn input must survive request-time compaction",
  );
  const entry = lastAssistantEntry();
  const conversationId = session.sessionManager.getSessionId();
  const turn = lifecycleObservations.find(
    (value) => value.type === "turn_start",
  );
  const preparation = lifecycleObservations.find(
    (value) => value.type === "session_before_compact",
  );
  assert(preparation);
  assert(turn.at < preparation.at);
  assert(
    events.findIndex((value) => value.type === "turn_start") <
      events.findIndex((value) => value.type === "session_before_compact"),
  );
  assert.equal(summaryPreparations.length, 1);
  assert.equal(summaryPreparations[0].event.reason, "threshold");
  assert.equal(summaryPreparations[0].event.willRetry, false);
  assert.equal(ordinaryCalls.length, 1);
  assert.equal(summaryCalls.length, 1);
  assert(
    summaryOrder.indexOf("summary-provider") <
      summaryOrder.indexOf("ordinary-provider"),
  );
  assert(
    JSON.stringify(ordinaryCalls[0]).includes("Active turn input must survive"),
  );
  assertSummaryHooks("compaction", true);
  await shutdown();
  assert.equal(generations.length, 2);
  const generation = generations.find(
    (value) => value.id === stableGenerationId(conversationId, entry.id),
  );
  const summary = generations.find(
    (value) => value.tags["pi.call_kind"] === "compaction",
  );
  assert(generation);
  assert(summary);
  assert(
    JSON.stringify(generation.input).includes(
      "Active turn input must survive request-time compaction",
    ),
  );
  assert(!JSON.stringify(generation.input).includes("Old history"));
  assert(Number.isFinite(Date.parse(generation.started_at)));
  assert(Date.parse(generation.started_at) <= turn.at);
  assert(Date.parse(generation.started_at) < Date.parse(summary.started_at));
  assert(Date.parse(summary.started_at) <= preparation.at);
  assert(
    Date.parse(summary.completed_at) <= Date.parse(generation.completed_at),
  );
}

async function summaryCleanup(cancel) {
  process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
  configureSummary();
  const respond = streamSimple;
  let first = true;
  let started;
  const active = new Promise((resolveStarted) => {
    started = resolveStarted;
  });
  streamSimple = (model, context, options) => {
    if (options.cacheRetention !== "none" || !first)
      return respond(model, context, options);
    first = false;
    summaryCalls.push(structuredClone(context));
    const stream = new AssistantMessageEventStream();
    const finish = () => {
      const message = assistant("", {
        api: model.api,
        stopReason: cancel ? "aborted" : "error",
        errorMessage: cancel ? "Aborted" : "Synthetic summary failure",
        usage: { ...usage, input: 0, output: 0, totalTokens: 0 },
      });
      stream.push({
        type: "error",
        reason: message.stopReason,
        error: message,
      });
      stream.end(message);
    };
    if (cancel)
      options.signal.addEventListener("abort", finish, { once: true });
    else queueMicrotask(finish);
    started();
    return stream;
  };
  await start({ seed: seedCompaction });
  const entriesBefore = session.sessionManager
    .getEntries()
    .filter((entry) => entry.type === "compaction").length;
  const attempt = assert.rejects(
    session.compact(),
    cancel ? /abort|cancel/i : /Synthetic summary failure/,
  );
  await active;
  if (cancel) session.abortCompaction();
  await attempt;
  const failed = events.filter(
    (event) => event.type === "session_compact_failed",
  );
  assert.equal(failed.length, 1);
  assert.equal(failed[0].reason, "manual");
  assert.equal(failed[0].aborted, cancel);
  assert.equal(failed[0].willRetry, false);
  assert.equal(failed[0].fromExtension, false);
  assert.equal(
    session.sessionManager
      .getEntries()
      .filter((entry) => entry.type === "compaction").length,
    entriesBefore,
  );
  await session.prompt("Ordinary input after unsuccessful summary");
  const beforeSuccessfulSummary = Date.now();
  await nextClockTick(beforeSuccessfulSummary);
  await session.compact();
  await session.prompt("Ordinary input after successful summary");
  await shutdown();
  assert.equal(summaryPreparations.length, 2);
  assert.equal(summaryPreparations[1].preparation.isSplitTurn, true);
  assert.equal(summaryCalls.length, 3);
  assert.equal(ordinaryCalls.length, 2);
  const summaries = generations.filter(isSummaryGeneration);
  assert.equal(
    summaries.length,
    1,
    "An unsuccessful summary must not invent a generation",
  );
  assert.equal(Number(summaries[0].usage.total_tokens), 24);
  assert(
    Date.parse(summaries[0].started_at) > beforeSuccessfulSummary,
    "Successful compaction must not reuse the failed operation's timer",
  );
  const turns = generations.filter((value) => !isSummaryGeneration(value));
  assert.equal(turns.length, 2);
  for (const [index, text] of [
    "Ordinary input after unsuccessful summary",
    "Ordinary input after successful summary",
  ].entries()) {
    assert(JSON.stringify(turns[index].input).includes(text));
    assert(
      Date.parse(turns[index].started_at) <=
        Date.parse(turns[index].completed_at),
    );
    assert.equal(turns[index].call_error, "");
  }
}

async function forkReload() {
  process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
  await start({ runtime: true });
  const manager = session.sessionManager;
  assert.equal(manager.isPersisted(), false);
  const trunkId = manager.getSessionId();
  await session.prompt("Trunk first turn");
  const inherited = lastAssistantEntry();
  await session.prompt("Trunk later turn");
  const later = lastAssistantEntry();
  const oldSession = session;
  const oldLoader = checkLoader();
  const originalStart = lifecycleObservations.find(
    (value) => value.type === "session_start",
  );
  await nextClockTick(Date.parse(later.timestamp));
  replaceFiles = true;
  assert.equal(
    (await sessionRuntime.fork(inherited.id, { position: "at" })).cancelled,
    false,
  );
  assert.notEqual(session, oldSession);
  assert.equal(session.sessionManager, manager);
  assert.equal(manager.getHeader().parentSession, undefined);
  const forkId = manager.getSessionId();
  assert.notEqual(forkId, trunkId);
  const forkLoader = checkLoader();
  const forkExtensionPath = loadedExtensionPath;
  assert.notEqual(forkExtensionPath, extensionPath);
  assert.equal(
    manager
      .getEntries()
      .findLast((entry) => entry.customType === "native-loader").data
      .evaluation,
    2,
  );
  assert.notEqual(forkLoader.runtime, oldLoader.runtime);
  assert.notEqual(
    forkLoader.extensions.find(
      (extension) => extension.path === loadedExtensionPath,
    ).handlers,
    oldLoader.extensions.find((extension) => extension.path === extensionPath)
      .handlers,
  );
  const forkStart = lifecycleObservations.findLast(
    (value) => value.type === "session_start",
  );
  assert.notEqual(forkStart.ctx, originalStart.ctx);
  assert.equal(forkStart.manager, originalStart.manager);
  assert.equal(
    events.findLast((value) => value.type === "session_start").reason,
    "fork",
  );
  assert(!manager.getEntries().some((entry) => entry.id === later.id));
  await nextClockTick(Date.parse(manager.getHeader().timestamp));
  await session.prompt("Fork first local turn");
  const local = lastAssistantEntry();
  await session.reload();
  assert.equal(session.sessionManager, manager);
  assert.notEqual(
    lifecycleObservations.findLast((value) => value.type === "session_start")
      .ctx,
    forkStart.ctx,
  );
  assert.notEqual(checkLoader().runtime, forkLoader.runtime);
  assert.notEqual(loadedExtensionPath, forkExtensionPath);
  assert.equal(
    manager
      .getEntries()
      .findLast((entry) => entry.customType === "native-loader").data
      .evaluation,
    3,
  );
  await session.prompt("Fork second local turn");
  const secondLocal = lastAssistantEntry();
  assert.equal(
    (await session.navigateTree(inherited.id, { summarize: false })).cancelled,
    false,
  );
  await session.reload();
  assert.equal(manager.getSessionId(), forkId);
  await session.prompt("Fork turn after inherited-entry navigation and reload");
  const navigated = lastAssistantEntry();
  await shutdown();
  assert.equal(requests.length, 5);
  assert.equal(generations.length, 5);
  const byId = new Map(
    generations.map((generation) => [generation.id, generation]),
  );
  for (const entry of [inherited, later])
    assert.equal(
      byId.get(stableGenerationId(trunkId, entry.id)).conversation_id,
      trunkId,
    );
  for (const entry of [local, secondLocal, navigated])
    assert.equal(
      byId.get(stableGenerationId(forkId, entry.id)).conversation_id,
      forkId,
    );
  for (const entry of [local, navigated]) {
    const generation = byId.get(stableGenerationId(forkId, entry.id));
    assert.deepEqual(generation.parent_generation_ids ?? [], []);
    assert.equal(generation.metadata["pi.fork.parent_session_id"], trunkId);
    assert.equal(
      generation.metadata["pi.fork.parent_generation_id"],
      stableGenerationId(trunkId, inherited.id),
    );
  }
  assert.deepEqual(
    byId.get(stableGenerationId(forkId, secondLocal.id)).parent_generation_ids,
    [stableGenerationId(forkId, local.id)],
  );
  assert(!byId.has(stableGenerationId(forkId, inherited.id)));
  assert(
    !generations.some((generation) =>
      generation.parent_generation_ids?.includes(
        stableGenerationId(forkId, inherited.id),
      ),
    ),
  );
}

const exportWarning =
  "Agent Observability: some telemetry could not be exported and may be lost.";
const exportPrivateBody = "native-export-private-response-body";
const exportSecret = "sk-native-export-credential-marker";

async function exportBarrier(promise, label) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`Missing export barrier: ${label}`)),
          10_000,
        );
        timer.unref();
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

function holdExportResponse(res, release) {
  return new Promise((resolveReply) => {
    const closed = () => resolveReply(undefined);
    res.once("close", closed);
    release.then((reply) => {
      res.off("close", closed);
      resolveReply(reply);
    });
  });
}

async function exportFailure(kind) {
  Object.assign(process.env, {
    AGENTO11Y_DEBUG: "false",
    AGENTO11Y_AUTH_TOKEN: exportSecret,
    AGENTO11Y_MAX_RETRIES: kind === "shutdown-deadline" ? "10" : "1",
    AGENTO11Y_MAX_BACKOFF_MS: kind === "shutdown-deadline" ? "10000" : "1",
    AGENTO11Y_QUEUE_SIZE: "100",
    AGENTO11Y_EXPORT_TIMEOUT_MS: kind === "shutdown-deadline" ? "250" : "10000",
  });
  // Use public provider/session APIs to fill the SDK's default 100-record
  // batch; do not sleep for its 1000ms periodic flush or replace its client.
  streamSimple = (model) => {
    const message = assistant("Export evidence answer", { api: model.api });
    const stream = new AssistantMessageEventStream();
    queueMicrotask(() => {
      stream.push({
        type: "start",
        partial: { ...message, stopReason: "pending" },
      });
      stream.push({ type: "done", reason: "stop", message });
      stream.end(message);
    });
    return stream;
  };
  const arrived = Promise.withResolvers();
  const retryArrived = Promise.withResolvers();
  const released = Promise.withResolvers();
  const closed = Promise.withResolvers();
  const warned = Promise.withResolvers();
  const accepted = new Set();
  const rejected = new Set();
  let stderr = "";
  let stdout = "";
  const stderrWrite = process.stderr.write;
  const stdoutWrite = process.stdout.write;
  process.stderr.write = function (chunk, ...args) {
    stderr += typeof chunk === "string" ? chunk : chunk.toString();
    if (stderr.includes(exportWarning)) warned.resolve();
    return stderrWrite.call(this, chunk, ...args);
  };
  process.stdout.write = function (chunk, ...args) {
    stdout += typeof chunk === "string" ? chunk : chunk.toString();
    return stdoutWrite.call(this, chunk, ...args);
  };
  exportReply = async ({ body, res }) => {
    const attempt = exportRequests.length;
    arrived.resolve();
    if (attempt === 2) retryArrived.resolve();
    const success = () => ({
      status: 200,
      body: {
        results: body.generations.map(({ id }) => {
          accepted.add(id);
          return { generation_id: id, accepted: true };
        }),
      },
    });
    if (kind === "partial-rejection")
      return {
        status: 200,
        body: {
          results: body.generations.map(({ id }, index) => {
            const ok = index >= 2;
            (ok ? accepted : rejected).add(id);
            return {
              generation_id: id,
              accepted: ok,
              ...(ok
                ? {}
                : {
                    error: `${exportPrivateBody} ${endpoint} ${exportSecret}`,
                  }),
            };
          }),
        },
      };
    if (
      kind === "exhausted-retries" ||
      (kind === "shutdown-deadline" && attempt === 1)
    )
      return {
        status: 503,
        body: { error: `${exportPrivateBody} ${endpoint} ${exportSecret}` },
      };
    res.once("close", () => closed.resolve());
    const reply = await holdExportResponse(res, released.promise);
    return reply ? success() : undefined;
  };
  try {
    await start();
    checkLoader();
    assert.equal(
      lifecycleObservations.find((event) => event.type === "session_start").ctx
        .hasUI,
      false,
    );
    const prompt = async (index) => {
      await session.prompt(`Export evidence turn ${index}`);
      assert.equal(lastAssistantEntry().message.stopReason, "stop");
    };
    for (let index = 0; index < 100; index++) await prompt(index);
    await exportBarrier(arrived.promise, "live asynchronous request");
    assert(session, "The export must begin before session shutdown");
    if (kind === "shutdown-deadline") {
      await exportBarrier(
        retryArrived.promise,
        "live retry after HTTP rejection",
      );
      assert.equal(
        stderr,
        "",
        "A retryable failure alone is not observed loss",
      );
      await shutdown();
      await exportBarrier(warned.promise, "shutdown loss warning");
      await exportBarrier(closed.promise, "shutdown aborts held response");
      assert.equal(
        exportRequests.length,
        2,
        "The shutdown deadline, not 11-attempt exhaustion, must stop retries",
      );
      assert.equal(accepted.size, 0);
    } else {
      if (kind === "queue-overflow") {
        assert.equal(
          stderr,
          "",
          "A pending request alone is not observed loss",
        );
        // Keep the first batch in flight so periodic flushes cannot drain the queue.
        for (let index = 100; index < 201; index++) await prompt(index);
      }
      await exportBarrier(
        warned.promise,
        "failure warning while session is live",
      );
      assert(
        session,
        "Asynchronous failure warning must not depend on shutdown",
      );
      assert.equal(stderr, `${exportWarning}\n`);
      if (kind === "queue-overflow") released.resolve(true);
      await prompt(201);
      await shutdown();
      if (kind === "partial-rejection") {
        assert(rejected.size >= 2);
        assert(
          accepted.size > 0,
          "The response must be a partial, not total, rejection",
        );
        assert.equal(new Set(generations.map(({ id }) => id)).size, 101);
        assert.equal(
          generations.length,
          101,
          "Terminal partial rejections must not be retried",
        );
      } else if (kind === "exhausted-retries") {
        assert(
          exportRequests.length >= 4,
          "Live and shutdown batches must both retry",
        );
        assert.equal(exportRequests.length % 2, 0);
        for (let index = 0; index < exportRequests.length; index += 2)
          assert.deepEqual(
            exportRequests[index].body,
            exportRequests[index + 1].body,
          );
        assert.equal(accepted.size, 0);
      } else {
        assert(accepted.size >= 100);
        assert(
          accepted.size < 202,
          "Overflow must represent actual missing telemetry",
        );
      }
    }
    assert.equal(
      stderr,
      `${exportWarning}\n`,
      "Warnings must be fixed, debug-independent and deduplicated",
    );
    assert(
      !stdout.includes(exportWarning),
      "Headless warnings must never use stdout",
    );
    const authorization = `Basic ${Buffer.from(`native-test:${exportSecret}`).toString("base64")}`;
    for (const privateValue of [
      exportPrivateBody,
      endpoint,
      exportSecret,
      authorization,
    ]) {
      assert(!stderr.includes(privateValue));
      assert(!stdout.includes(privateValue));
    }
    assert.equal(process.env.AGENTO11Y_DEBUG, "false");
    assert.equal(requests.length, 0);
    assert(
      exportRequests.every(
        ({ headers }) => headers.authorization === authorization,
      ),
      "The synthetic credential must actually reach the exporter",
    );
  } finally {
    released.resolve(true);
    try {
      await shutdown();
    } finally {
      process.stderr.write = stderrWrite;
      process.stdout.write = stdoutWrite;
    }
  }
}

const scenarios = {
  ...Object.fromEntries(
    [
      "partial-rejection",
      "exhausted-retries",
      "queue-overflow",
      "shutdown-deadline",
    ].map((kind) => [`export-${kind}`, () => exportFailure(kind)]),
  ),
  "lifecycle-request-compaction": requestTimeCompaction,
  "lifecycle-summary-failed": () => summaryCleanup(false),
  "lifecycle-summary-cancelled": () => summaryCleanup(true),
  "lifecycle-fork-reload": forkReload,
  async "telemetry-response-model"() {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    responseModel = "native-concrete-response-model";
    await start();
    await session.prompt("Concrete response through the real SSE adapter");
    const concrete = lastAssistantEntry();
    assert.equal(concrete.message.model, "native-model");
    assert.equal(concrete.message.responseModel, responseModel);
    responseModel = "native-model";
    await session.prompt("Matching request and response identities");
    await shutdown();
    assert.equal(requests.length, 2);
    assert.equal(generations.length, 2);
    assert.deepEqual(
      generations.map((generation) => generation.model),
      Array(2).fill({ provider: "native-loopback", name: "native-model" }),
    );
    assert.deepEqual(
      generations.map((generation) => generation.response_model),
      ["native-concrete-response-model", "native-model"],
    );
  },
  ...Object.fromEntries(
    toolCases.flatMap((kind) =>
      Object.entries(toolModes).map(([suffix, mode]) => [
        `tools-${kind}-${suffix}`,
        () => tools(kind, mode),
      ]),
    ),
  ),
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
  async lifecycle() {
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    retry = true;
    let calls = 0;
    let signalAborted = false;
    let started;
    const active = new Promise((resolveStarted) => {
      started = resolveStarted;
    });
    streamSimple = (model, context, options) => {
      calls++;
      assert(calls <= 3, "Unexpected synthetic model request");
      if (calls === 2) {
        assert(JSON.stringify(context.messages).includes("retry prompt"));
        assert(
          !context.messages.some(
            (message) =>
              message.role === "assistant" && message.stopReason === "error",
          ),
        );
      }
      const stream = new AssistantMessageEventStream();
      const finish = (stopReason) => {
        const message = {
          role: "assistant",
          api: model.api,
          provider: model.provider,
          model: model.id,
          timestamp: Date.now(),
          content: [
            {
              type: "text",
              text:
                stopReason === "error"
                  ? "failed attempt"
                  : stopReason === "stop"
                    ? "retry success"
                    : "interrupted turn",
            },
          ],
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
    const attempts = session.sessionManager
      .getEntries()
      .filter(
        (entry) =>
          entry.type === "message" && entry.message.role === "assistant",
      );
    assert.equal(attempts.length, 2);
    assert.deepEqual(
      attempts.map((entry) => entry.message.stopReason),
      ["error", "stop"],
    );
    const conversationId = session.sessionManager.getSessionId();
    const retryEvents = events.map((event) => event.type);
    assert(
      retryEvents.indexOf("agent_end") < retryEvents.lastIndexOf("agent_start"),
    );
    assert(
      retryEvents.lastIndexOf("agent_start") <
        retryEvents.indexOf("agent_settled"),
    );
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
    assert.deepEqual(
      generations.slice(0, 2).map((generation) => generation.id),
      attempts.map((entry) => stableGenerationId(conversationId, entry.id)),
    );
    assert.deepEqual(
      generations.slice(0, 2).map((generation) => generation.stop_reason),
      ["error", "end_turn"],
    );
    assert(JSON.stringify(generations[0].input).includes("retry prompt"));
    assert(JSON.stringify(generations[0].output).includes("failed attempt"));
    assert(!JSON.stringify(generations[0].output).includes("retry success"));
    assert(JSON.stringify(generations[1].output).includes("retry success"));
    assert(!JSON.stringify(generations[1].output).includes("failed attempt"));
    assert.deepEqual(
      generations
        .slice(0, 2)
        .map((generation) => Number(generation.usage.total_tokens)),
      [12, 12],
    );
    for (const generation of generations) {
      assert(Number.isFinite(Date.parse(generation.started_at)));
      assert(
        Date.parse(generation.started_at) <=
          Date.parse(generation.completed_at),
      );
    }
    assert(
      Date.parse(generations[0].completed_at) <=
        Date.parse(generations[1].started_at),
    );
    assert.equal(
      generations.slice(0, 2).filter((generation) => generation.system_prompt)
        .length,
      0,
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
  if (scenario.startsWith("tools-repeated-ids-"))
    return ["warn tool-call transform for reused-id applied"];
  if (scenario.startsWith("tools-transformed-"))
    return ["warn tool-call transform for native-one applied"];
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
    for (const generation of generations) {
      assert.equal(generation.system_prompt, "");
      for (const field of ["max_tokens", "temperature", "top_p", "tool_choice"])
        assert(
          !Object.hasOwn(generation, field),
          `Unverified request field: ${field}`,
        );
      const called = new Set(
        generation.output.flatMap((message) =>
          message.parts.flatMap((part) =>
            part.tool_call ? [part.tool_call.name] : [],
          ),
        ),
      );
      assert.deepEqual(
        new Set(generation.tools.map((tool) => tool.name)),
        called,
      );
      for (const tool of generation.tools) {
        assert.equal(tool.description, "");
        assert.equal(tool.input_schema_json, "");
      }
    }
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
  assertSummaryAttribution();
  await assertPluginDiagnostics(pluginLog, expectedDiagnostics());
  assert.deepEqual(failures, [], "Extension errors or unexpected requests");
});
