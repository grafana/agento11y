import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { inspect } from "node:util";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
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
    xdg: process.env.XDG_STATE_HOME,
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
    if (saved.xdg === undefined) delete process.env.XDG_STATE_HOME;
    else process.env.XDG_STATE_HOME = saved.xdg;
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
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

  describe.each([
    "metadata_only",
    "no_tool_content",
    "full",
    "full_with_metadata_spans",
  ])("debug sanitation with %s capture", (mode) => {
    it.each([
      "debug",
      "warn",
      "error",
    ] as const)("redacts formatted %s messages and arguments even when capture redaction is disabled", (level) => {
      vi.stubEnv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode);
      vi.stubEnv("SIGIL_CONTENT_CAPTURE_MODE", mode);
      vi.stubEnv("AGENTO11Y_REDACT_INPUT_MESSAGES", "false");
      vi.stubEnv("AGENTO11Y_REDACT_SECRETS", "false");
      process.env.AGENTO11Y_DEBUG = "true";
      const token = "glc_abcdefghijklmnopqrstuvwxyz1234";
      logger[level](`message ${token}`);
      logger[level]("token=%s", token);
      logger[level]("object=%j", { password: "json-secret-private" });
      logger[level](
        "failure",
        new Error("DATABASE_PASSWORD=error-secret-private"),
      );
      logger[level]("object", { token });

      const body = readLog();
      expect(body).toContain("[REDACTED:");
      expect(body).not.toContain(token);
      expect(body).not.toContain("json-secret-private");
      expect(body).not.toContain("error-secret-private");
    });
  });

  it("swallows formatting errors without logging their raw details", () => {
    process.env.AGENTO11Y_DEBUG = "true";
    const value = {
      [inspect.custom]() {
        throw new Error("private formatter error");
      },
    };
    expect(() => logger.warn("value %s", value)).not.toThrow();
    logger.debug("still running");
    const body = readLog();
    expect(body).toContain("still running");
    expect(body).not.toContain("private formatter error");
    expect(body).not.toContain("value");
  });

  it("does not format arguments while debug logging is disabled", () => {
    const formatValue = vi.fn(() => {
      throw new Error("private formatter error");
    });
    logger.error("%s", { [inspect.custom]: formatValue });
    expect(formatValue).not.toHaveBeenCalled();
    expect(() => readLog()).toThrow();
  });

  it.each([
    "mkdir",
    "append",
    "cached directory removed",
  ])("swallows filesystem failures during %s without writing to the terminal", (failure) => {
    process.env.AGENTO11Y_DEBUG = "true";
    const stdout = vi.spyOn(process.stdout, "write").mockReturnValue(true);
    const stderr = vi.spyOn(process.stderr, "write").mockReturnValue(true);
    if (failure === "mkdir") {
      writeFileSync(join(dir, "agento11y"), "not a directory");
    } else if (failure === "append") {
      mkdirSync(logFilePath(), { recursive: true });
    } else {
      logger.debug("initial write");
      rmSync(join(dir, "agento11y"), { recursive: true });
    }
    expect(() => logger.error("DATABASE_PASSWORD=private-error")).not.toThrow();
    expect(stdout).not.toHaveBeenCalled();
    expect(stderr).not.toHaveBeenCalled();
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
