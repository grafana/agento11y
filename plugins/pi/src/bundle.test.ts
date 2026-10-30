import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  assertSdkBuilt,
  copyTestPackage,
  runTestProcess,
  testEnvironment,
} from "./testHarness.mjs";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const node = realpathSync(process.execPath);

describe("bundle", () => {
  it("loads from an ESM-only context", { timeout: 160_000 }, async () => {
    assertSdkBuilt(packageDir);
    const root = mkdtempSync(join(tmpdir(), "agento11y-pi-bundle-"));
    try {
      const isolatedPackage = copyTestPackage(packageDir, root);
      const path = [
        realpathSync(join(isolatedPackage, "node_modules/.bin")),
        dirname(node),
        "/usr/bin",
        "/bin",
      ].join(delimiter);
      expect(path.split(delimiter)[0]).toBe(
        realpathSync(join(packageDir, "node_modules/.bin")),
      );
      // Run the build script directly: `pnpm run` first checks the whole
      // workspace lockfile against its supply-chain policy, which can take
      // longer than the test timeout.
      await runTestProcess({
        name: "plugin build",
        command: node,
        args: ["scripts/build.mjs"],
        cwd: isolatedPackage,
        env: testEnvironment(root, "build", path),
        timeout: 120_000,
      });
      await runTestProcess({
        name: "ESM import",
        command: node,
        args: ["--input-type=module", "-e", "await import('./dist/index.js');"],
        cwd: isolatedPackage,
        env: testEnvironment(root, "import", path),
        timeout: 30_000,
      });
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
