import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  copyFile,
  cp,
  mkdir,
  mkdtemp,
  readFile,
  realpath,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const node = await realpath(process.execPath);
const root = await mkdtemp(join(tmpdir(), "agento11y-native-controls-"));
const bundle = await readFile(join(packageDir, "dist/index.js"), "utf8");

function replaceOnce(source, before, after) {
  assert.equal(
    source.split(before).length,
    2,
    `Fault target changed: ${before}`,
  );
  return source.replace(before, after);
}

const controls = [
  {
    name: "named-udp",
    scenario: "file-loader",
    inject: (source) =>
      `import { createSocket as injectedCreateSocket } from "node:dgram";
try {
  const socket = injectedCreateSocket("udp4");
  socket.send("native negative control", 9, "127.0.0.1", () => socket.close());
} catch {}
${source}`,
    failure: /Extension errors or unexpected requests/,
  },
  {
    name: "caught-shutdown",
    scenario: "file-loader",
    inject: (source) =>
      replaceOnce(
        source,
        'await sigil.shutdown();\n      } catch (err) {\n        logger.error("session shutdown failed", err);',
        'await sigil.shutdown();\n        throw new Error("native injected shutdown failure");\n      } catch (err) {\n        logger.error("session shutdown failed", err);',
      ),
    failure: /Unexpected plugin diagnostics/,
    witness: /native injected shutdown failure/,
  },
  ...["file-loader", "reload"].flatMap((scenario) =>
    [
      {
        name: "unclosed-metric-reader",
        before: "[meterProvider.shutdown(), tracerProvider.shutdown()]",
        after: "[meterProvider.forceFlush(), tracerProvider.shutdown()]",
        witness: /repeat: 15000/,
      },
      {
        name: "unclosed-client-flush",
        before: "clearInterval(this.flushTimer);",
        after: "void this.flushTimer;",
        witness: /repeat: 1000/,
      },
    ].map(({ name, before, after, witness }) => ({
      name: `${name}-${scenario}`,
      scenario,
      inject: (source) => replaceOnce(source, before, after),
      failure: new RegExp(
        `Plugin timers remain after ${scenario === "reload" ? "runtime replacement" : "session shutdown"}`,
      ),
      witness,
    })),
  ),
  {
    name: "unexpected-init-diagnostic",
    scenario: "guard-startup-disabled",
    inject: (source) =>
      replaceOnce(
        source,
        'logger.error("local capture unavailable", err);',
        'logger.error("local capture unavailable", err);\n        logger.error("native injected initialization failure");',
      ),
    failure: /Unexpected plugin diagnostics/,
    witness: /native injected initialization failure/,
  },
];

async function run(name, scenario, source) {
  const dir = join(root, name);
  const snapshot = join(dir, "package");
  const runnerDir = join(snapshot, "src/testdata/native");
  await mkdir(runnerDir, { recursive: true });
  await mkdir(join(snapshot, "dist"));
  await copyFile(
    join(packageDir, "package.json"),
    join(snapshot, "package.json"),
  );
  await cp(join(packageDir, "src/testdata/native"), runnerDir, {
    recursive: true,
  });
  await symlink(
    join(packageDir, "node_modules"),
    join(snapshot, "node_modules"),
    "dir",
  );
  await writeFile(join(snapshot, "dist/index.js"), source);
  const env = {
    PATH: `${dirname(node)}:/usr/bin:/bin`,
    CI: "1",
    LANG: "C.UTF-8",
    TZ: "UTC",
    PI_OFFLINE: "1",
    PI_SKIP_VERSION_CHECK: "1",
    PI_TELEMETRY: "0",
    DO_NOT_TRACK: "1",
  };
  for (const [key, subdir] of Object.entries({
    HOME: "home",
    USERPROFILE: "home",
    XDG_CONFIG_HOME: "config",
    XDG_CACHE_HOME: "cache",
    XDG_DATA_HOME: "data",
    XDG_STATE_HOME: "state",
    PI_CODING_AGENT_DIR: "agent",
    PI_CODING_AGENT_SESSION_DIR: "sessions",
    TMPDIR: "tmp",
  })) {
    env[key] = join(dir, subdir);
    await mkdir(env[key], { recursive: true });
  }
  const cwd = join(dir, "workspace");
  await mkdir(cwd);
  return new Promise((resolveRun, reject) => {
    const child = spawn(node, [join(runnerDir, "runner.mjs"), scenario], {
      cwd,
      env,
      detached: true,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    child.stdout.on("data", (chunk) => {
      output += chunk;
    });
    child.stderr.on("data", (chunk) => {
      output += chunk;
    });
    const timer = setTimeout(() => {
      process.kill(-child.pid, "SIGKILL");
      reject(new Error(`${name}: deadline exceeded\n${output}`));
    }, 60_000);
    child.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolveRun({ code, signal, output });
    });
  });
}

const baselineOnly = process.argv[2] === "--baseline-only";
assert(
  baselineOnly || process.argv.length === 2,
  "Pass --baseline-only followed by native scenario names, or no arguments",
);

try {
  const baselines = baselineOnly
    ? process.argv.slice(3)
    : [
        ...new Set([
          ...controls.map(({ scenario }) => scenario),
          "guard-startup-open",
          "guard-startup-strict",
        ]),
      ];
  assert(baselines.length > 0, "Pass at least one native scenario");
  for (const scenario of baselines) {
    const result = await run(`baseline-${scenario}`, scenario, bundle);
    assert.equal(
      result.code,
      0,
      `Baseline ${scenario} failed:\n${result.output}`,
    );
    console.log(`PASS baseline ${scenario}`);
  }
  for (const control of baselineOnly ? [] : controls) {
    const result = await run(
      control.name,
      control.scenario,
      control.inject(bundle),
    );
    assert.equal(
      result.signal,
      null,
      `${control.name} was killed:\n${result.output}`,
    );
    assert.notEqual(
      result.code,
      0,
      `${control.name} unexpectedly passed:\n${result.output}`,
    );
    assert.match(
      result.output,
      control.failure,
      `${control.name}: wrong failure`,
    );
    if (control.witness) assert.match(result.output, control.witness);
    console.log(`PASS negative control ${control.name}`);
  }
} finally {
  await rm(root, { recursive: true, force: true });
}
