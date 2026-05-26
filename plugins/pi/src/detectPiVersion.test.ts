import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { detectPiVersion } from "./detectPiVersion.js";

const originalArgv = process.argv;

function stageHostPi(
  root: string,
  packageName: string,
  version: string,
): string {
  const pkgDir = join(root, "lib", "node_modules", ...packageName.split("/"));
  const distDir = join(pkgDir, "dist");
  mkdirSync(distDir, { recursive: true });
  writeFileSync(
    join(pkgDir, "package.json"),
    JSON.stringify({ name: packageName, version }),
  );
  const cli = join(distDir, "cli.js");
  writeFileSync(cli, "#!/usr/bin/env node\n");
  return cli;
}

function setArgv(entry: string): void {
  process.argv = [originalArgv[0] ?? "node", entry];
}

describe("detectPiVersion", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-detect-"));
  });

  afterEach(() => {
    process.argv = originalArgv;
    rmSync(dir, { recursive: true, force: true });
  });

  it("reads version from @earendil-works host package", () => {
    const cli = stageHostPi(dir, "@earendil-works/pi-coding-agent", "0.75.4");
    setArgv(cli);
    expect(detectPiVersion()).toBe("0.75.4");
  });

  it("reads version from @mariozechner host package", () => {
    const cli = stageHostPi(dir, "@mariozechner/pi-coding-agent", "0.73.1");
    setArgv(cli);
    expect(detectPiVersion()).toBe("0.73.1");
  });

  it("follows a symlinked entry script via realpath", () => {
    const cli = stageHostPi(dir, "@earendil-works/pi-coding-agent", "0.75.4");
    const binDir = join(dir, "bin");
    mkdirSync(binDir, { recursive: true });
    const symlink = join(binDir, "pi");
    symlinkSync(cli, symlink);
    setArgv(symlink);
    expect(detectPiVersion()).toBe("0.75.4");
  });

  it("ignores a stale peer copy bundled next to the extension", () => {
    const cli = stageHostPi(dir, "@earendil-works/pi-coding-agent", "0.75.4");
    const staleDir = join(
      dir,
      "lib",
      "node_modules",
      "@grafana",
      "sigil-pi",
      "node_modules",
      "@mariozechner",
      "pi-coding-agent",
    );
    mkdirSync(staleDir, { recursive: true });
    writeFileSync(
      join(staleDir, "package.json"),
      JSON.stringify({
        name: "@mariozechner/pi-coding-agent",
        version: "0.70.5",
      }),
    );
    setArgv(cli);
    const detected = detectPiVersion();
    expect(detected).toBe("0.75.4");
    expect(detected).not.toBe("0.70.5");
  });

  it("returns undefined when argv[1] is missing", () => {
    process.argv = [originalArgv[0] ?? "node"];
    expect(detectPiVersion()).toBeUndefined();
  });

  it("returns undefined when no matching package.json is found", () => {
    const scriptDir = join(dir, "some", "unrelated", "tree");
    mkdirSync(scriptDir, { recursive: true });
    const script = join(scriptDir, "script.js");
    writeFileSync(script, "");
    setArgv(script);
    expect(detectPiVersion()).toBeUndefined();
  });

  it("returns undefined and does not throw when realpath fails", () => {
    setArgv(join(dir, "does-not-exist", "cli.js"));
    expect(() => detectPiVersion()).not.toThrow();
    expect(detectPiVersion()).toBeUndefined();
  });

  it("walks up until it finds the host package", () => {
    const deepDir = join(
      dir,
      "lib",
      "node_modules",
      "@earendil-works",
      "pi-coding-agent",
    );
    mkdirSync(deepDir, { recursive: true });
    writeFileSync(
      join(deepDir, "package.json"),
      JSON.stringify({
        name: "@earendil-works/pi-coding-agent",
        version: "0.75.4",
      }),
    );
    const nestedDir = join(deepDir, "a", "b", "c", "d", "e", "f", "g");
    mkdirSync(nestedDir, { recursive: true });
    const cli = join(nestedDir, "cli.js");
    writeFileSync(cli, "");
    setArgv(cli);
    expect(detectPiVersion()).toBe("0.75.4");
  });
});
