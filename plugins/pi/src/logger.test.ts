import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  logFilePath,
  logger,
  resetLoggerForTests,
  stateRoot,
} from "./logger.js";

describe("logFilePath", () => {
  const saved = {
    xdg: process.env.XDG_STATE_HOME,
    home: process.env.HOME,
  };

  afterEach(() => {
    process.env.XDG_STATE_HOME = saved.xdg;
    process.env.HOME = saved.home;
  });

  it("honors an absolute XDG_STATE_HOME", () => {
    process.env.XDG_STATE_HOME = "/var/state";
    expect(stateRoot()).toBe("/var/state/sigil");
    expect(logFilePath()).toBe("/var/state/sigil/logs/sigil.log");
  });

  it("ignores a relative XDG_STATE_HOME and falls back to HOME", () => {
    process.env.XDG_STATE_HOME = "relative/path";
    process.env.HOME = "/home/alex";
    expect(logFilePath()).toBe("/home/alex/.local/state/sigil/logs/sigil.log");
  });
});

describe("logger", () => {
  let dir: string;
  const saved = process.env.SIGIL_DEBUG;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-log-"));
    process.env.XDG_STATE_HOME = dir;
    resetLoggerForTests();
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (saved === undefined) delete process.env.SIGIL_DEBUG;
    else process.env.SIGIL_DEBUG = saved;
    resetLoggerForTests();
  });

  function readLog(): string {
    return readFileSync(join(dir, "sigil", "logs", "sigil.log"), "utf-8");
  }

  it("writes nothing when SIGIL_DEBUG is off", () => {
    delete process.env.SIGIL_DEBUG;
    logger.debug("hidden");
    logger.warn("hidden");
    logger.error("hidden");
    expect(() => readLog()).toThrow();
  });

  it("appends formatted lines to the debug log when SIGIL_DEBUG is on", () => {
    process.env.SIGIL_DEBUG = "true";
    logger.debug("queued model=%s", "claude");
    logger.warn("heads up");
    logger.error("boom", new Error("nope"));

    const body = readLog();
    expect(body).toContain("sigil[pi]:");
    expect(body).toContain("debug queued model=claude");
    expect(body).toContain("warn heads up");
    expect(body).toContain("error boom");
    expect(body).toContain("nope");
    // One line per call plus the Error's multi-line stack trace.
    expect(body.split("sigil[pi]:")).toHaveLength(4);
  });

  it("re-reads SIGIL_DEBUG per call so dotenv-applied values take effect", () => {
    delete process.env.SIGIL_DEBUG;
    logger.warn("before");
    process.env.SIGIL_DEBUG = "1";
    logger.warn("after");

    const body = readLog();
    expect(body).toContain("after");
    expect(body).not.toContain("before");
  });
});
