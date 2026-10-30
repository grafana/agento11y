import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  accessSync,
  cpSync,
  mkdirSync,
  realpathSync,
  symlinkSync,
} from "node:fs";
import { join } from "node:path";

export function assertSdkBuilt(packageDir) {
  for (const entry of ["index.js", "index.d.ts"]) {
    const path = join(
      packageDir,
      "node_modules/@grafana/agento11y/dist",
      entry,
    );
    assert.doesNotThrow(
      () => accessSync(path),
      "Pi tests require a prebuilt SDK. Run `mise run build:ts:sdk-js` first, " +
        "or use `mise run test:ts:plugin-pi-native` to prepare it automatically.",
    );
  }
}

export function copyTestPackage(packageDir, root) {
  const copy = join(root, "package");
  mkdirSync(join(copy, "scripts"), { recursive: true });
  for (const entry of [
    "package.json",
    "tsconfig.json",
    "scripts/build.mjs",
    "src",
  ]) {
    cpSync(join(packageDir, entry), join(copy, entry), { recursive: true });
  }
  symlinkSync(
    realpathSync(join(packageDir, "node_modules")),
    join(copy, "node_modules"),
    "junction",
  );
  return copy;
}

export function testEnvironment(root, name, path) {
  const dir = join(root, name);
  const env = {
    PATH: path,
    CI: "1",
    LANG: "C.UTF-8",
    TZ: "UTC",
    pnpm_config_verify_deps_before_run: "false",
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
    mkdirSync(env[key], { recursive: true });
  }
  return env;
}

export function runTestProcess({
  name,
  command,
  args,
  cwd,
  env,
  timeout,
  stdio = "pipe",
}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd, env, stdio, detached: true });
    let stdout = "";
    let stderr = "";
    let expired = false;
    child.stdout?.setEncoding("utf8").on("data", (data) => {
      stdout += data;
    });
    child.stderr?.setEncoding("utf8").on("data", (data) => {
      stderr += data;
    });
    const timer = setTimeout(() => {
      expired = true;
      // Killing only the build script leaves its compiler writing after cleanup.
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch (error) {
        if (error.code !== "ESRCH") reject(error);
      }
    }, timeout);
    child.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      if (code === 0 && !expired) resolve({ stdout, stderr });
      else
        reject(
          new Error(
            `${name}: ${expired ? "deadline exceeded" : `exit ${code}, signal ${signal}`}\n${stderr || stdout}`,
          ),
        );
    });
  });
}
