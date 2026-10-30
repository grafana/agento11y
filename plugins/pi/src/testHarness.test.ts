import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  assertSdkBuilt,
  runTestProcess,
  testEnvironment,
} from "./testHarness.mjs";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const node = realpathSync(process.execPath);
const path = [dirname(node), "/usr/bin", "/bin"].join(delimiter);

function temporaryRoot() {
  const root = realpathSync(
    mkdtempSync(join(tmpdir(), "agento11y-pi-harness-")),
  );
  return { root, remove: () => rmSync(root, { recursive: true, force: true }) };
}

function descendantScript(ready: string, witness: string, wait: number) {
  const descendant = `
    const { writeFileSync } = require('node:fs');
    writeFileSync(${JSON.stringify(ready)}, 'ready');
    setTimeout(() => writeFileSync(${JSON.stringify(witness)}, 'late write'), ${wait});
  `;
  return `
    const { spawn } = require('node:child_process');
    const child = spawn(process.execPath, ['-e', ${JSON.stringify(descendant)}], { stdio: 'ignore' });
    child.once('exit', code => process.exit(code ?? 1));
  `;
}

describe("test process", () => {
  it("reports output, failed exits, and spawn errors", async () => {
    const { root, remove } = temporaryRoot();
    try {
      const options = {
        name: "fixture",
        command: node,
        cwd: root,
        env: testEnvironment(root, "process", path),
        timeout: 5_000,
      };
      await expect(
        runTestProcess({
          ...options,
          args: ["-e", "console.log('output'); console.error('diagnostic')"],
        }),
      ).resolves.toEqual({ stdout: "output\n", stderr: "diagnostic\n" });
      await expect(
        runTestProcess({
          ...options,
          args: ["-e", "console.error('build failed'); process.exit(7)"],
        }),
      ).rejects.toThrow(/fixture: exit 7.*\nbuild failed/s);
      await expect(
        runTestProcess({
          ...options,
          command: join(root, "missing-executable"),
          args: [],
        }),
      ).rejects.toThrow(/ENOENT/);
    } finally {
      remove();
    }
  });

  it("allows the descendant witness without a timeout", async () => {
    const { root, remove } = temporaryRoot();
    try {
      const ready = join(root, "ready");
      const witness = join(root, "witness");
      await runTestProcess({
        name: "descendant control",
        command: node,
        args: ["-e", descendantScript(ready, witness, 100)],
        cwd: root,
        env: testEnvironment(root, "control", path),
        timeout: 5_000,
      });
      expect(readFileSync(ready, "utf8")).toBe("ready");
      expect(readFileSync(witness, "utf8")).toBe("late write");
    } finally {
      remove();
    }
  });

  it("kills compiler descendants before they can write after timeout", {
    timeout: 10_000,
  }, async () => {
    const { root, remove } = temporaryRoot();
    try {
      const ready = join(root, "ready");
      const witness = join(root, "witness");
      await expect(
        runTestProcess({
          name: "timed build",
          command: node,
          args: ["-e", descendantScript(ready, witness, 2_500)],
          cwd: root,
          env: testEnvironment(root, "timeout", path),
          timeout: 2_000,
        }),
      ).rejects.toThrow("timed build: deadline exceeded");
      expect(readFileSync(ready, "utf8")).toBe("ready");
      await delay(3_000);
      expect(existsSync(witness)).toBe(false);
    } finally {
      remove();
    }
  });
});

describe("native build isolation", () => {
  it("requires SDK prebuild and only builds and runs the copied package", {
    timeout: 15_000,
  }, async () => {
    const { root, remove } = temporaryRoot();
    try {
      const source = join(root, "source");
      const scripts = join(source, "scripts");
      const native = join(source, "src/testdata/native");
      mkdirSync(scripts, { recursive: true });
      mkdirSync(native, { recursive: true });
      mkdirSync(join(source, "dist"));
      writeFileSync(
        join(source, "dist/index.js"),
        "shared bundle must not change",
      );
      writeFileSync(
        join(source, "package.json"),
        JSON.stringify({
          type: "module",
          pi: { extensions: ["./dist/index.js"] },
        }),
      );
      writeFileSync(join(source, "tsconfig.json"), "{}");
      cpSync(
        join(packageDir, "scripts/test-native.mjs"),
        join(scripts, "test-native.mjs"),
      );
      cpSync(
        join(packageDir, "src/testHarness.mjs"),
        join(source, "src/testHarness.mjs"),
      );
      const pnpm = join(root, "pnpm.cjs");
      writeFileSync(pnpm, "throw new Error('native must not build SDK')");
      const options = {
        name: "native fixture",
        command: node,
        args: [join(scripts, "test-native.mjs"), "file-loader"],
        cwd: source,
        env: {
          ...testEnvironment(root, "orchestrator", path),
          npm_execpath: pnpm,
          AGENTO11Y_AUTH_TOKEN: "must-not-reach-child",
          OTEL_EXPORTER_OTLP_HEADERS: "must-not-reach-child",
        },
        timeout: 10_000,
      };
      await expect(runTestProcess(options)).rejects.toThrow(
        "mise run build:ts:sdk-js",
      );
      const sdk = join(source, "node_modules/@grafana/agento11y/dist");
      mkdirSync(sdk, { recursive: true });
      writeFileSync(join(sdk, "index.js"), "");
      expect(() => assertSdkBuilt(source)).toThrow("mise run build:ts:sdk-js");
      writeFileSync(join(sdk, "index.d.ts"), "");
      expect(() => assertSdkBuilt(source)).not.toThrow();
      writeFileSync(
        join(scripts, "build.mjs"),
        `
        import assert from 'node:assert/strict';
        import { existsSync, mkdirSync, realpathSync, writeFileSync } from 'node:fs';
        assert.notEqual(process.cwd(), ${JSON.stringify(source)});
        assert.equal(existsSync('dist'), false);
        assert.equal(realpathSync('node_modules'), ${JSON.stringify(join(source, "node_modules"))});
        assert.equal(process.env.AGENTO11Y_AUTH_TOKEN, undefined);
        assert.equal(process.env.OTEL_EXPORTER_OTLP_HEADERS, undefined);
        mkdirSync('dist');
        writeFileSync('dist/index.js', 'export const built = true; export const buildHome = ' + JSON.stringify(process.env.HOME) + ';');
      `,
      );
      writeFileSync(
        join(native, "runner.mjs"),
        `
        import assert from 'node:assert/strict';
        import { readFileSync } from 'node:fs';
        const root = new URL('../../../', import.meta.url);
        assert.notEqual(root.pathname, ${JSON.stringify(`${source}/`)});
        const manifest = JSON.parse(readFileSync(new URL('package.json', root), 'utf8'));
        const entry = await import(new URL(manifest.pi.extensions[0], root));
        assert.equal(entry.built, true);
        assert.notEqual(entry.buildHome, process.env.HOME);
        assert.equal(process.env.AGENTO11Y_AUTH_TOKEN, undefined);
        assert.equal(process.env.OTEL_EXPORTER_OTLP_HEADERS, undefined);
        console.log('copied manifest loader ran');
      `,
      );
      const result = await runTestProcess(options);
      expect(result.stdout).toContain("copied manifest loader ran");
      expect(readFileSync(join(source, "dist/index.js"), "utf8")).toBe(
        "shared bundle must not change",
      );
    } finally {
      remove();
    }
  });
});
