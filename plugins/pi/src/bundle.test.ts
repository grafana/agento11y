import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

describe("bundle", () => {
  it("loads from an ESM-only context", () => {
    // Run the build script directly: `pnpm run` first checks the whole
    // workspace lockfile against its supply-chain policy, which can take
    // longer than the test timeout.
    const build = spawnSync(process.execPath, ["scripts/build.mjs"], {
      cwd: packageDir,
      encoding: "utf-8",
    });
    const buildOutput = build.error?.message || build.stderr || build.stdout;
    expect(build.status, buildOutput).toBe(0);

    const result = spawnSync(
      process.execPath,
      ["--input-type=module", "-e", "await import('./dist/index.js');"],
      {
        cwd: packageDir,
        encoding: "utf-8",
      },
    );

    const resultOutput =
      result.error?.message || result.stderr || result.stdout;
    expect(result.status, resultOutput).toBe(0);
  });
});
