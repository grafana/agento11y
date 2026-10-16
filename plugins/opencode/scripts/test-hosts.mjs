#!/usr/bin/env node
/**
 * Test the packed, already-built plugin, not source wrappers or mocked hosts:
 *
 * Build first (pnpm --dir plugins/opencode build). --imports checks package
 * imports/declarations only, not runtime capture. Hosts require macOS
 * sandbox-exec; imports can use Node >=26 permissions. No unrestricted fallback.
 * Provisioning permits npm registry reads with lifecycle scripts disabled.
 * Probes write only to temp and use loopback only (Node permissions deny all
 * network). Credentials and personal config are not inherited.
 *
 * v1.2.16 needs a root-entry file URL; later v1 hosts use their package loader.
 * v2 uses a local tarball, /server detection and --standalone;
 * import success cannot establish runtime compatibility. Runtime failures stay
 * fatal. Guards cover allow only, not deny/transform/tool/permission scenarios.
 */
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  access,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  realpath,
  rm,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { gunzipSync } from "node:zlib";

const PLUGIN = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const VERSIONS = ["1.2.16", "1.3.13", "1.18.29", "2.0.22", "2.0.29"];
const REGISTRY = "https://registry.npmjs.org/";
const NAME = "@grafana/agento11y-opencode";
const MODEL = "packed-harness";
const PROMPT =
  "packed-package-compatibility-turn: reply with PACKED_HOST_OK, no tools.";
const REPLY = "PACKED_HOST_OK";
const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

function options(args) {
  const result = {
    imports: false,
    versions: [],
    keep: false,
    timeout: 120_000,
  };
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--imports") result.imports = true;
    else if (arg === "--keep-temp") result.keep = true;
    else if (arg === "--help" || arg === "-h") result.help = true;
    else if (arg === "--version" || arg === "--versions") {
      assert(
        args[i + 1] && !args[i + 1].startsWith("--"),
        `${arg} needs exact versions`,
      );
      result.versions.push(...args[++i].split(","));
    } else if (arg === "--timeout-ms") {
      result.timeout = Number(args[++i]);
      assert(
        Number.isInteger(result.timeout) && result.timeout >= 1000,
        "--timeout-ms must be an integer of at least 1000 milliseconds",
      );
    } else throw new Error(`Unknown option: ${arg}; use --help`);
  }
  for (const version of result.versions) {
    assert(
      VERSIONS.includes(version),
      `Unsupported version ${version}; choose ${VERSIONS.join(", ")}`,
    );
  }
  assert(
    !result.imports || !result.versions.length,
    "--imports does not run hosts. Omit --version and --versions.",
  );
  result.versions = [
    ...new Set(result.versions.length ? result.versions : VERSIONS),
  ];
  return result;
}

async function json(path) {
  return JSON.parse(await readFile(path, "utf8"));
}
async function put(path, value) {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(
    path,
    typeof value === "string" ? value : `${JSON.stringify(value, null, 2)}\n`,
  );
}

// Inherit only absolute PATH entries, not credentials or personal config.
async function environment(base) {
  const home = join(base, "home");
  const env = {
    PATH: (process.env.PATH ?? "/usr/bin:/bin")
      .split(delimiter)
      .filter((entry) => entry.startsWith("/"))
      .join(delimiter),
    HOME: home,
    USERPROFILE: home,
    USER: "packed-host-test",
    LOGNAME: "packed-host-test",
    XDG_CONFIG_HOME: join(base, "config"),
    XDG_CACHE_HOME: join(base, "cache"),
    XDG_DATA_HOME: join(base, "data"),
    XDG_STATE_HOME: join(base, "state"),
    TMPDIR: join(base, "tmp"),
    TMP: join(base, "tmp"),
    TEMP: join(base, "tmp"),
    OPENCODE_CONFIG_DIR: join(base, "opencode-config"),
    npm_config_cache: join(base, "npm-cache"),
    npm_config_userconfig: join(base, "npmrc"),
    npm_config_globalconfig: join(base, "global-npmrc"),
    npm_config_registry: REGISTRY,
    npm_config_ignore_scripts: "true",
    npm_config_legacy_peer_deps: "true",
    npm_config_audit: "false",
    npm_config_fund: "false",
    npm_config_update_notifier: "false",
    PNPM_HOME: join(base, "pnpm"),
    COREPACK_ENABLE_AUTO_PIN: "0",
    COREPACK_ENABLE_DOWNLOAD_PROMPT: "0",
    BUN_INSTALL_CACHE_DIR: join(base, "bun-cache"),
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_GLOBAL: join(base, "gitconfig"),
    CI: "1",
    NO_COLOR: "1",
    TERM: "dumb",
    LANG: "en_US.UTF-8",
    TZ: "UTC",
  };
  for (const key of [
    "HOME",
    "XDG_CONFIG_HOME",
    "XDG_CACHE_HOME",
    "XDG_DATA_HOME",
    "XDG_STATE_HOME",
    "TMPDIR",
    "OPENCODE_CONFIG_DIR",
    "PNPM_HOME",
  ]) {
    await mkdir(env[key], { recursive: true });
  }
  await put(
    env.npm_config_userconfig,
    "ignore-scripts=true\nlegacy-peer-deps=true\naudit=false\nfund=false\n",
  );
  await put(env.npm_config_globalconfig, "");
  await put(env.GIT_CONFIG_GLOBAL, "");
  return env;
}

// Kill the entire process group, including v2's private standalone server.
function killGroup(child, signal) {
  if (!child.pid) return;
  try {
    process.kill(-child.pid, signal);
  } catch (error) {
    if (error.code !== "ESRCH") throw error;
  }
}
async function command(
  executable,
  args,
  { cwd, env, timeout = 120_000, sandbox, label } = {},
) {
  const program = sandbox ? "/usr/bin/sandbox-exec" : executable;
  const argv = sandbox ? ["-f", sandbox, executable, ...args] : args;
  return new Promise((done, reject) => {
    let output = "";
    let timedOut = false;
    let interrupted;
    const child = spawn(program, argv, {
      cwd,
      env,
      detached: true,
      stdio: ["ignore", "pipe", "pipe"],
    });
    const collect = (chunk) => {
      output = (output + chunk.toString()).slice(-100_000);
    };
    child.stdout.on("data", collect);
    child.stderr.on("data", collect);
    let hardKill;
    const stop = (signal) => {
      killGroup(child, signal);
      hardKill ??= setTimeout(() => killGroup(child, "SIGKILL"), 2000);
    };
    const timer = setTimeout(() => {
      timedOut = true;
      stop("SIGTERM");
    }, timeout);
    const interrupt = (signal) => {
      interrupted ??= signal;
      clearTimeout(timer);
      stop(signal);
    };
    const onInterrupt = () => interrupt("SIGINT");
    const onTerminate = () => interrupt("SIGTERM");
    process.on("SIGINT", onInterrupt);
    process.on("SIGTERM", onTerminate);
    const cleanup = () => {
      clearTimeout(timer);
      clearTimeout(hardKill);
      process.off("SIGINT", onInterrupt);
      process.off("SIGTERM", onTerminate);
    };
    child.once("error", (error) => {
      cleanup();
      reject(new Error(`${label ?? executable} unavailable: ${error.message}`));
    });
    child.once("close", (code, signal) => {
      cleanup();
      killGroup(child, "SIGKILL");
      if (interrupted) {
        reject(
          Object.assign(
            new Error(`${label ?? executable} cancelled (${interrupted})`),
            {
              signal: interrupted,
            },
          ),
        );
      } else if (code !== 0 || timedOut) {
        reject(
          new Error(
            `${label ?? executable} ${timedOut ? `timed out after ${timeout}ms` : `failed (${code ?? signal})`}\n${output}`,
          ),
        );
      } else done(output);
    });
  });
}

// OpenCode discovers system config and per-user managed preferences outside HOME/XDG.
const MACHINE_CONFIG_ROOTS = [
  "/Library/Application Support/opencode",
  "/Library/Managed Preferences",
  "/System/Volumes/Data/Library/Application Support/opencode",
  "/System/Volumes/Data/Library/Managed Preferences",
];

function sandboxPolicy(root) {
  return `(version 1)
(allow default)
(deny network*)
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:*"))
(deny file-write*)
(allow file-write* (subpath ${JSON.stringify(root)}) (literal "/dev/null") (literal "/dev/tty"))
${MACHINE_CONFIG_ROOTS.map((path) => `(deny file-read* (subpath ${JSON.stringify(path)}))`).join("\n")}
`;
}

async function sandboxProfile(root) {
  assert.equal(
    process.platform,
    "darwin",
    "Requires macOS sandbox-exec for loopback-only hosts/imports; refusing an unsandboxed run",
  );
  await access("/usr/bin/sandbox-exec");
  const profile = join(root, "loopback.sb");
  await put(profile, sandboxPolicy(root));
  return profile;
}

const isHostPackage = (name) =>
  name === "opencode-ai" ||
  name.startsWith("opencode-") ||
  name.startsWith("@opencode-ai/") ||
  name.startsWith("@opencode/");

async function noHostDependencies(directory) {
  // Host packages are allowed only in the separate declaration consumers.
  async function walk(path) {
    for (const entry of await readdir(path, { withFileTypes: true })) {
      if (entry.isSymbolicLink())
        throw new Error(
          `Unexpected installed symlink: ${join(path, entry.name)}`,
        );
      if (!entry.isDirectory()) continue;
      const child = join(path, entry.name);
      if (
        entry.name !== "node_modules" &&
        !entry.name.startsWith("@") &&
        !entry.name.startsWith(".")
      ) {
        try {
          const pkg = await json(join(child, "package.json"));
          assert(
            !isHostPackage(pkg.name ?? ""),
            `Runtime consumer installed host dependency ${pkg.name} at ${child}`,
          );
        } catch (error) {
          if (error.code !== "ENOENT") throw error;
        }
      }
      await walk(child);
    }
  }
  await walk(join(directory, "node_modules"));
}

async function pack(root, env) {
  const pkg = await json(join(PLUGIN, "package.json"));
  assert.equal(pkg.name, NAME);
  try {
    await access(join(PLUGIN, "dist/index.js"));
  } catch {
    throw new Error(
      "Built artifact missing: plugins/opencode/dist/index.js. Run pnpm --dir plugins/opencode build first; this harness never builds or edits the checkout.",
    );
  }
  const destination = join(root, "packed");
  await mkdir(destination);
  await command("pnpm", ["pack", "--pack-destination", destination], {
    cwd: PLUGIN,
    env,
    label: "pnpm pack (built plugin)",
  });
  const files = (await readdir(destination)).filter((name) =>
    name.endsWith(".tgz"),
  );
  assert.equal(files.length, 1, "pnpm pack must produce exactly one tarball");
  const tarball = join(destination, files[0]);
  const listing = await command("tar", ["-tzf", tarball], { cwd: root, env });
  assert(
    listing
      .split("\n")
      .filter(Boolean)
      .every(
        (name) =>
          name.startsWith("package/") && !name.split("/").includes(".."),
      ),
    "Unsafe packed tar paths",
  );
  console.log(`PACK ${files[0]}`);
  return { tarball, pkg };
}

async function consumer(base, tarball, env) {
  await put(join(base, "package.json"), { private: true, type: "module" });
  await command(
    "npm",
    [
      "install",
      "--ignore-scripts",
      "--legacy-peer-deps=false",
      "--omit=dev",
      "--no-audit",
      "--no-fund",
      "--package-lock=false",
      tarball,
    ],
    {
      cwd: base,
      env,
      label: "Install packed runtime consumer",
    },
  );
  await noHostDependencies(base);
  return join(base, "node_modules", ...NAME.split("/"));
}

// Only Node >=26 gates networking; older releases need the OS sandbox.
const permissionArgs = (root, sandbox) =>
  sandbox
    ? []
    : ["--permission", `--allow-fs-read=${root}`, `--allow-fs-write=${root}`];

async function importJavascript(root, artifact, env, sandbox) {
  const nodePermissions = permissionArgs(root, sandbox);
  const base = join(root, "imports-runtime");
  await consumer(base, artifact.tarball, env);
  console.log(
    "PASS packed runtime install: no v1/v2 host dependencies (normal peer resolution)",
  );
  const probe = join(base, "imports.mjs");
  await put(
    probe,
    `import assert from 'node:assert/strict';
let failures = 0;
for (const specifier of ['${NAME}', '${NAME}/server']) {
  try {
    const module = await import(specifier);
    if (specifier.endsWith('/server')) {
      assert(module.default && typeof module.default.id === 'string', '/server must default-export a v2 plugin definition');
      assert(typeof module.default.setup === 'function' || typeof module.default.effect === 'function', '/server must have setup or effect');
      assert.equal(typeof module.default.server, 'function', '/server must retain the newer v1 loader adapter');
    } else assert.equal(typeof module.Agento11yPlugin, 'function', 'root must retain the v1 named plugin export');
    console.log('PASS actual import ' + specifier);
  } catch (error) { failures++; console.error('FAIL actual import ' + specifier + ': ' + error.stack); }
}
assert.equal(failures, 0, 'packed import failures');
`,
  );
  await command(process.execPath, [...nodePermissions, probe], {
    cwd: base,
    env,
    sandbox,
    label: "Packed JS imports (NOT host runtime)",
  });
  await noHostDependencies(base);
  console.log(
    "PASS packed JS imports, no installed v1/v2 host runtime dependencies",
  );
}

async function importDeclarations(root, artifact, env, sandbox) {
  const nodePermissions = permissionArgs(root, sandbox);
  const failures = [];
  for (const version of VERSIONS) {
    const native = version.startsWith("2.");
    const host = native ? "@opencode/plugin" : "@opencode-ai/plugin";
    const unusedHost = native ? "@opencode-ai/plugin" : "@opencode/plugin";
    const types = join(root, `imports-types-${version}`);
    try {
      await put(join(types, "package.json"), {
        private: true,
        type: "module",
        dependencies: { [NAME]: `file:${artifact.tarball}` },
        devDependencies: {
          typescript: artifact.pkg.devDependencies.typescript,
          "@types/node": artifact.pkg.devDependencies["@types/node"],
          "@types/json-schema": "7.0.15",
          [host]: version,
          ...(!native ? { "@opencode-ai/sdk": version } : {}),
        },
      });
      await command(
        "npm",
        [
          "install",
          "--ignore-scripts",
          "--legacy-peer-deps",
          "--no-audit",
          "--no-fund",
        ],
        {
          cwd: types,
          env,
          timeout: 240_000,
          label: `Provision isolated ${version} declaration consumer`,
        },
      );
      await assert.rejects(
        access(join(types, "node_modules", ...unusedHost.split("/"))),
        { code: "ENOENT" },
        `Unused host API ${unusedHost} must not be installed`,
      );
      await put(
        join(types, "probe.mts"),
        `import server from '${NAME}/server';
import type { Plugin } from '${host}';
${
  native
    ? "const plugin: Plugin.Plugin = server;"
    : `import { Agento11yPlugin } from '${NAME}';
const root: Plugin = Agento11yPlugin;
const plugin: Plugin = server.server;
void root;
// @ts-expect-error the root plugin is callable, not a number
const badRoot: number = Agento11yPlugin;`
}
const id: string = server.id;
void plugin; void id;
// @ts-expect-error the server definition is an object, not a number
const badServer: number = server;
`,
      );
      // Upstream 1.2.16 declarations omit extensions required by NodeNext.
      const bundler = version === "1.2.16";
      await put(join(types, "tsconfig.json"), {
        compilerOptions: {
          target: "ES2022",
          module: bundler ? "ESNext" : "NodeNext",
          moduleResolution: bundler ? "Bundler" : "NodeNext",
          strict: true,
          noEmit: true,
          skipLibCheck: false,
          types: ["node"],
        },
        files: ["probe.mts"],
      });
      await command(
        process.execPath,
        [
          ...nodePermissions,
          join(types, "node_modules/typescript/bin/tsc"),
          "-p",
          "tsconfig.json",
        ],
        {
          cwd: types,
          env,
          sandbox,
          label: `Packed ${version}-only declarations (tsc, skipLibCheck=false)`,
        },
      );
      console.log(
        `PASS packed ${version}-only declarations; unused host API absent; NOT a real-host runtime result`,
      );
    } catch (error) {
      if (error.signal) throw error;
      failures.push(version);
      console.error(
        `FAIL packed ${version}-only declarations: ${error.message}`,
      );
    }
  }
  assert.deepEqual(
    failures,
    [],
    "Isolated packed declaration consumers failed",
  );
}

async function officialBinary(version, root, env) {
  const base = join(root, `official-${version}`);
  const name = version.startsWith("2.") ? "@opencode/cli" : "opencode-ai";
  await put(join(base, "package.json"), { private: true });
  try {
    await command(
      "npm",
      [
        "install",
        "--ignore-scripts",
        "--legacy-peer-deps",
        "--no-audit",
        "--no-fund",
        "--include=optional",
        "--save-exact",
        `${name}@${version}`,
      ],
      {
        cwd: base,
        env,
        timeout: 240_000,
        label: `Fetch official ${name}@${version}`,
      },
    );
    const manifest = await json(
      join(base, "node_modules", ...name.split("/"), "package.json"),
    );
    assert.equal(manifest.name, name, "Official npm host name mismatch");
    assert.equal(
      manifest.version,
      version,
      "Official npm host version mismatch",
    );
    // Bypass disabled postinstall and wrapper caches; use the exact optional binary.
    const platform = `${version.startsWith("2.") ? "@opencode/cli" : "opencode"}-${process.platform}-${process.arch}`;
    assert.equal(
      manifest.optionalDependencies?.[platform],
      version,
      `No exact official binary dependency ${platform}@${version}`,
    );
    const binaryPackage = join(base, "node_modules", ...platform.split("/"));
    const binaryManifest = await json(join(binaryPackage, "package.json"));
    assert.equal(binaryManifest.name, platform);
    assert.equal(binaryManifest.version, version);
    const binary = join(binaryPackage, "bin/opencode");
    await access(binary);
    return binary;
  } catch (error) {
    if (error.signal) throw error;
    throw new Error(
      `BINARY UNAVAILABLE: official ${name}@${version} (${process.platform}/${process.arch}). No substitute host, skipped pass, or import-only fallback.\n${error.message}`,
    );
  }
}

async function loopback() {
  const state = {
    models: [],
    generations: [],
    guards: [],
    otlp: [],
    errors: [],
  };
  const server = createServer(async (req, res) => {
    try {
      const chunks = [];
      let size = 0;
      for await (const chunk of req) {
        size += chunk.length;
        assert(size <= 16 * 1024 * 1024, "Oversize loopback request");
        chunks.push(chunk);
      }
      const raw = Buffer.concat(chunks);
      const body =
        req.headers["content-encoding"] === "gzip" ? gunzipSync(raw) : raw;
      const path = new URL(req.url, "http://127.0.0.1").pathname;
      const reply = (value) => {
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(value));
      };
      if (req.method === "POST" && path === "/api/v1/generations:export") {
        const payload = JSON.parse(body.toString());
        assert(
          Array.isArray(payload.generations),
          "Export must contain generations array",
        );
        state.generations.push(...payload.generations);
        reply({
          results: payload.generations.map((g) => ({
            generation_id: g.id,
            accepted: true,
          })),
        });
      } else if (req.method === "POST" && path === "/api/v1/hooks:evaluate") {
        state.guards.push(JSON.parse(body.toString()));
        reply({ action: "allow", evaluations: [] });
      } else if (
        req.method === "POST" &&
        ["/v1/traces", "/v1/metrics", "/v1/logs"].includes(path)
      ) {
        state.otlp.push({ path, bytes: body.length });
        res.setHeader(
          "Content-Type",
          req.headers["content-type"] ?? "application/x-protobuf",
        );
        res.end(
          String(req.headers["content-type"]).includes("json")
            ? "{}"
            : Buffer.alloc(0),
        );
      } else if (
        req.method === "POST" &&
        path === "/model/v1/chat/completions"
      ) {
        const payload = JSON.parse(body.toString());
        assert.equal(
          payload.model,
          MODEL,
          "Unexpected loopback model selection",
        );
        assert(
          JSON.stringify(payload.messages).includes(
            "packed-package-compatibility-turn",
          ),
          "Unexpected auxiliary/title model call; this test requires a single user turn",
        );
        state.models.push(payload);
        const common = {
          id: "chatcmpl-packed",
          created: 1700000000,
          model: MODEL,
        };
        const usage = {
          prompt_tokens: 7,
          completion_tokens: 3,
          total_tokens: 10,
        };
        if (!payload.stream) {
          reply({
            ...common,
            object: "chat.completion",
            choices: [
              {
                index: 0,
                message: { role: "assistant", content: REPLY },
                finish_reason: "stop",
              },
            ],
            usage,
          });
        } else {
          res.setHeader("Content-Type", "text/event-stream");
          res.setHeader("Cache-Control", "no-cache");
          for (const item of [
            {
              choices: [
                {
                  index: 0,
                  delta: { role: "assistant", content: REPLY },
                  finish_reason: null,
                },
              ],
            },
            {
              choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
              usage,
            },
          ])
            res.write(
              `data: ${JSON.stringify({ ...common, object: "chat.completion.chunk", ...item })}\n\n`,
            );
          res.end("data: [DONE]\n\n");
        }
      } else {
        throw new Error(`Unexpected loopback request: ${req.method} ${path}`);
      }
    } catch (error) {
      state.errors.push(error.message);
      res.statusCode = 500;
      res.end(JSON.stringify({ error: error.message }));
    }
  });
  await new Promise((done, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", done);
  });
  return {
    state,
    url: `http://127.0.0.1:${server.address().port}`,
    close: () =>
      new Promise((done) => {
        server.close(done);
        server.closeAllConnections();
      }),
  };
}

function hostConfig(version, packageDir, tarball, url, rootEntry) {
  if (version.startsWith("2.")) {
    return {
      update: "disable",
      share: "disabled",
      snapshots: false,
      warming: false,
      model: { providerID: "packed", model: MODEL },
      // Exercise package /server resolution, not a direct-entry wrapper.
      plugins: [`file:${tarball}`],
      providers: {
        packed: {
          name: "Loopback only",
          package: "@opencode/ai/providers/openai-compatible",
          env: [],
          settings: {
            provider: "packed",
            baseURL: `${url}/model/v1`,
            apiKey: "loopback-not-a-secret",
          },
          models: {
            [MODEL]: { name: MODEL, limit: { context: 8192, output: 1024 } },
          },
        },
      },
    };
  }
  return {
    autoupdate: false,
    share: "disabled",
    snapshot: false,
    model: `packed/${MODEL}`,
    small_model: `packed/${MODEL}`,
    enabled_providers: ["packed"],
    plugin: [
      pathToFileURL(
        version === "1.2.16" ? join(packageDir, rootEntry) : packageDir,
      ).href,
    ],
    provider: {
      packed: {
        name: "Loopback only",
        npm: "@ai-sdk/openai-compatible",
        env: [],
        options: {
          baseURL: `${url}/model/v1`,
          apiKey: "loopback-not-a-secret",
        },
        models: {
          [MODEL]: {
            name: MODEL,
            tool_call: false,
            limit: { context: 8192, output: 1024 },
          },
        },
      },
    },
  };
}

async function realHost(version, binary, root, artifact, sandbox, timeout) {
  const base = join(root, `runtime-${version}`);
  const env = await environment(base);
  const work = join(base, "consumer");
  const packageDir = await consumer(work, artifact.tarball, env);
  const packedManifest = await json(join(packageDir, "package.json"));
  const entry = packedManifest.exports?.["."]?.import ?? packedManifest.main;
  assert.equal(
    typeof entry,
    "string",
    "Packed plugin needs a root import entry",
  );
  assert(!entry.split("/").includes(".."), "Root entry escapes package");
  const capture = await loopback();
  try {
    const config = join(env.OPENCODE_CONFIG_DIR, "opencode.json");
    await put(
      config,
      hostConfig(version, packageDir, artifact.tarball, capture.url, entry),
    );
    Object.assign(env, {
      OPENCODE_CONFIG: config,
      OPENCODE_DISABLE_AUTOUPDATE: "true",
      OPENCODE_DISABLE_DEFAULT_PLUGINS: "true",
      OPENCODE_DISABLE_MODELS_FETCH: "true",
      OPENCODE_MODELS_URL: capture.url,
      OPENCODE_DISABLE_PROJECT_CONFIG: "true",
      OPENCODE_CONFIG_PROJECT_DISABLE: "true",
      OPENCODE_DISABLE_FILEWATCHER: "true",
      OPENCODE_FILEWATCHER_DISABLE: "true",
      OPENCODE_DISABLE_FFF: "true",
      OPENCODE_DISABLE_LSP_DOWNLOAD: "true",
      OPENCODE_DISABLE_SHARE: "true",
      OPENCODE_EXPERIMENTAL_DISABLE_COPY_ON_SELECT: "true",
      AGENTO11Y_ENDPOINT: capture.url,
      AGENTO11Y_LOCAL: "false",
      AGENTO11Y_AUTH_TENANT_ID: "packed-test",
      AGENTO11Y_AUTH_TOKEN: "loopback-not-a-secret",
      AGENTO11Y_CONTENT_CAPTURE_MODE: "full",
      AGENTO11Y_DEBUG: "true",
      AGENTO11Y_GUARDS_ENABLED: "true",
      AGENTO11Y_GUARDS_FAIL_OPEN: "false",
      AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT: capture.url,
      OTEL_EXPORTER_OTLP_ENDPOINT: capture.url,
      OTEL_EXPORTER_OTLP_PROTOCOL: "http/protobuf",
      OTEL_METRIC_EXPORT_INTERVAL: "250",
      OTEL_BSP_SCHEDULE_DELAY: "250",
      // Fail SDK/default-plugin downloads locally after provisioning.
      npm_config_registry: capture.url,
      npm_config_offline: "true",
      BUN_CONFIG_NO_CLEAR_TERMINAL: "true",
    });
    await put(
      env.npm_config_userconfig,
      `ignore-scripts=true\nlegacy-peer-deps=true\noffline=true\naudit=false\nfund=false\nregistry=${capture.url}\n`,
    );
    const reported = await command(binary, ["--version"], {
      cwd: work,
      env,
      sandbox,
      label: `Official binary ${version} --version`,
    });
    assert(
      reported.trim().split(/\s+/).includes(version),
      `Binary reported ${reported.trim()}, expected ${version}`,
    );
    const args = [
      "run",
      ...(version.startsWith("2.") ? ["--standalone"] : []),
      "--model",
      `packed/${MODEL}`,
      "--title",
      "Packed compatibility",
      "--format",
      "json",
      PROMPT,
    ];
    let output;
    let failure;
    try {
      output = await command(binary, args, {
        cwd: work,
        env,
        sandbox,
        timeout,
        label: `REAL HOST ${version}`,
      });
    } catch (error) {
      failure = error;
    }
    // Include late exports; duplicate adapter exports must fail.
    await sleep(500);
    await put(join(base, "result.json"), {
      version,
      output,
      failure: failure?.message,
      ...capture.state,
    });
    if (failure) throw failure;
    assert.deepEqual(
      capture.state.errors,
      [],
      `Host ${version}: unexpected local traffic`,
    );
    assert.equal(
      capture.state.models.length,
      1,
      `Host ${version}: expected exactly one actual loopback model turn`,
    );
    assert(
      output.includes(REPLY),
      `Host ${version}: CLI did not complete the loopback assistant reply\n${output}`,
    );
    assert.equal(
      capture.state.generations.length,
      1,
      `Host ${version}: expected exactly ONE adapter to export ONE generation, got ${capture.state.generations.length}. Check loading, flushing, or duplicate adapters; imports alone do not prove runtime compatibility.`,
    );
    const [generation] = capture.state.generations;
    assert(
      generation.id && generation.conversation_id,
      "Generation identity/session missing",
    );
    assert.equal(generation.model?.name, MODEL);
    assert.equal(generation.model?.provider, "packed");
    assert(
      JSON.stringify(generation.input).includes(
        "packed-package-compatibility-turn",
      ),
      "Captured user input missing",
    );
    assert(
      JSON.stringify(generation.output).includes(REPLY),
      "Captured model output missing",
    );
    assert.equal(String(generation.usage?.input_tokens), "7");
    assert.equal(String(generation.usage?.output_tokens), "3");
    assert(
      capture.state.guards.length > 0,
      "Allow-only loopback guard was not consulted",
    );
    await noHostDependencies(work);
    console.log(
      `PASS REAL HOST ${version}: one model turn, one exported generation, one active adapter (${version === "1.2.16" ? "manual root config" : version.startsWith("2.") ? "--standalone, package /server loader" : "host package loader"}); OTLP requests=${capture.state.otlp.length}`,
    );
  } finally {
    await capture.close();
  }
}

async function main() {
  const opts = options(process.argv.slice(2));
  if (opts.help) {
    console.log(
      `Usage: node plugins/opencode/scripts/test-hosts.mjs [--imports | --versions ${VERSIONS.join(",")}] [--keep-temp] [--timeout-ms 120000]\nRequires a built dist and pnpm/npm/Node; hosts require macOS sandbox-exec, imports require Node >=26 or macOS sandbox-exec.\n--imports checks actual packed JS imports + tsc declarations only, NEVER runtime capture.\nDefault: test every listed official host. v2 always uses --standalone. All failures are fatal.\nGuards cover allow only, not deny/transform/tool/permission scenarios. The checkout and personal config are unchanged. Temporary configs are written under TEMP.`,
    );
    return;
  }
  const root = await realpath(
    await mkdtemp(join(tmpdir(), "agento11y-packed-hosts-")),
  );
  console.log(`TEMP ${root}`);
  try {
    const env = await environment(join(root, "provision"));
    const sandbox =
      opts.imports && Number(process.versions.node.split(".")[0]) >= 26
        ? undefined
        : await sandboxProfile(root);
    if (sandbox)
      await command("/usr/bin/true", [], {
        cwd: root,
        env,
        sandbox,
        label: "Loopback sandbox preflight",
      });
    const artifact = await pack(root, env);
    if (opts.imports) {
      let failures = 0;
      for (const [name, check] of [
        ["packed JavaScript imports", importJavascript],
        ["packed declarations", importDeclarations],
      ]) {
        try {
          await check(root, artifact, env, sandbox);
        } catch (error) {
          if (error.signal) throw error;
          failures++;
          console.error(`FAIL ${name}: ${error.message}`);
        }
      }
      assert.equal(
        failures,
        0,
        `${failures}/2 import/declaration checks failed; real-host runtime NOT TESTED`,
      );
      console.log("PASS IMPORTS ONLY; real-host runtime NOT TESTED");
    } else {
      let failures = 0;
      for (const version of opts.versions) {
        try {
          const binary = await officialBinary(version, root, env);
          await realHost(
            version,
            binary,
            root,
            artifact,
            sandbox,
            opts.timeout,
          );
        } catch (error) {
          if (error.signal) throw error;
          failures++;
          console.error(`FAIL ${version}: ${error.message}`);
        }
      }
      assert.equal(
        failures,
        0,
        `${failures}/${opts.versions.length} real-host checks failed (no skipped passes)`,
      );
    }
  } finally {
    if (opts.keep)
      console.log(
        `KEPT ${root} (contains only synthetic test data and public packages)`,
      );
    else await rm(root, { recursive: true, force: true });
  }
}

export { command, options, sandboxPolicy };

if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  main().catch((error) => {
    console.error(`FAIL: ${error.message}`);
    process.exitCode =
      error.signal === "SIGINT" ? 130 : error.signal === "SIGTERM" ? 143 : 1;
  });
}
