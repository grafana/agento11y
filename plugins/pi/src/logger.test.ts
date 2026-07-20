import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
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
    expect(stateRoot()).toBe("/var/state/agento11y");
    expect(logFilePath()).toBe("/var/state/agento11y/logs/agento11y.log");
  });

  it("ignores a relative XDG_STATE_HOME and falls back to HOME", () => {
    process.env.XDG_STATE_HOME = "relative/path";
    process.env.HOME = "/home/alex";
    expect(logFilePath()).toBe(
      "/home/alex/.local/state/agento11y/logs/agento11y.log",
    );
  });

  it("falls back to an existing legacy sigil state dir", () => {
    const dir = mkdtempSync(join(tmpdir(), "agento11y-pi-state-"));
    try {
      process.env.XDG_STATE_HOME = dir;
      mkdirSync(join(dir, "sigil"), { recursive: true });
      expect(stateRoot()).toBe(join(dir, "sigil"));
      expect(logFilePath()).toBe(join(dir, "sigil", "logs", "agento11y.log"));
      // The new dir wins once it exists.
      mkdirSync(join(dir, "agento11y"), { recursive: true });
      expect(stateRoot()).toBe(join(dir, "agento11y"));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("logger", () => {
  let dir: string;
  const saved = {
    sigil: process.env.SIGIL_DEBUG,
    agento11y: process.env.AGENTO11Y_DEBUG,
  };

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "agento11y-pi-log-"));
    process.env.XDG_STATE_HOME = dir;
    delete process.env.SIGIL_DEBUG;
    delete process.env.AGENTO11Y_DEBUG;
    resetLoggerForTests();
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (saved.sigil === undefined) delete process.env.SIGIL_DEBUG;
    else process.env.SIGIL_DEBUG = saved.sigil;
    if (saved.agento11y === undefined) delete process.env.AGENTO11Y_DEBUG;
    else process.env.AGENTO11Y_DEBUG = saved.agento11y;
    resetLoggerForTests();
  });

  function readLog(): string {
    return readFileSync(
      join(dir, "agento11y", "logs", "agento11y.log"),
      "utf-8",
    );
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
    expect(body).toContain("agento11y[pi]:");
    expect(body).toContain("debug queued model=claude");
    expect(body).toContain("warn heads up");
    expect(body).toContain("error boom");
    expect(body).toContain("nope");
    // One line per call plus the Error's multi-line stack trace.
    expect(body.split("agento11y[pi]:")).toHaveLength(4);
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

  it("honors AGENTO11Y_DEBUG", () => {
    process.env.AGENTO11Y_DEBUG = "1";
    logger.debug("preferred");
    expect(readLog()).toContain("preferred");
  });

  it("nonblank AGENTO11Y_DEBUG=false wins over SIGIL_DEBUG=true", () => {
    process.env.AGENTO11Y_DEBUG = "false";
    process.env.SIGIL_DEBUG = "true";
    logger.warn("hidden");
    expect(() => readLog()).toThrow();
  });

  it("blank AGENTO11Y_DEBUG falls back to SIGIL_DEBUG", () => {
    process.env.AGENTO11Y_DEBUG = "   ";
    process.env.SIGIL_DEBUG = "true";
    logger.warn("visible");
    expect(readLog()).toContain("visible");
  });
});
