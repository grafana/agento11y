import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { loadConfig, normalizeBaseEndpoint, resolveConfig } from "./config.js";
import { clearSigilEnv } from "./testEnv.js";

describe("resolveConfig", () => {
  beforeEach(clearSigilEnv);
  afterEach(clearSigilEnv);

  it("returns null when endpoint is missing", () => {
    expect(resolveConfig()).toBeNull();
  });

  it("returns null when endpoint is whitespace", () => {
    process.env.SIGIL_ENDPOINT = "   ";
    expect(resolveConfig()).toBeNull();
  });

  it("stores the bare base URL when given a clean endpoint", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("defaults agentName to 'opencode'", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.agentName).toBe("opencode");
  });

  it("falls back to 'opencode' when SIGIL_AGENT_NAME is whitespace", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "   ";
    const cfg = resolveConfig();
    expect(cfg?.agentName).toBe("opencode");
  });

  it("reads SIGIL_AGENT_NAME and SIGIL_AGENT_VERSION", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "opencode-custom";
    process.env.SIGIL_AGENT_VERSION = "9.9.9";
    const cfg = resolveConfig();
    expect(cfg?.agentName).toBe("opencode-custom");
    expect(cfg?.agentVersion).toBe("9.9.9");
  });

  it("defaults contentCapture to metadata_only", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
  });

  it("accepts SIGIL_CONTENT_CAPTURE_MODE=full", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("full");
  });

  it("accepts SIGIL_CONTENT_CAPTURE_MODE=no_tool_content", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "no_tool_content";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("no_tool_content");
  });

  it("accepts SIGIL_CONTENT_CAPTURE_MODE=metadata_only", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "metadata_only";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
  });

  it("warns and falls back to metadata_only on unknown content-capture value", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "yolo";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("unsupported contentCapture"),
    );
    warn.mockRestore();
  });

  it("SIGIL_DEBUG flips debug on", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_DEBUG = "true";
    const cfg = resolveConfig();
    expect(cfg?.debug).toBe(true);
  });

  it("debug defaults to false", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.debug).toBe(false);
  });

  it("derives basic auth from SIGIL_AUTH_TENANT_ID + SIGIL_AUTH_TOKEN", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({
      mode: "basic",
      basicUser: "tenant-1",
      basicPassword: "glc_token",
      tenantId: "tenant-1",
    });
  });

  it("falls back to tenant mode when only the tenant id is set", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "tenant", tenantId: "tenant-1" });
  });

  it("warns and falls back to none when only the auth token is set", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("SIGIL_AUTH_TENANT_ID is missing"),
    );
    warn.mockRestore();
  });

  it("defaults auth to none when no creds are set", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
  });
});

describe("normalizeBaseEndpoint", () => {
  it("returns empty string for empty input", () => {
    expect(normalizeBaseEndpoint("")).toBe("");
  });

  it("preserves a clean base URL", () => {
    expect(normalizeBaseEndpoint("http://localhost:8080")).toBe(
      "http://localhost:8080",
    );
  });

  it("strips a trailing slash", () => {
    expect(normalizeBaseEndpoint("http://localhost:8080/")).toBe(
      "http://localhost:8080",
    );
  });

  it("strips an accidentally-pasted export-path suffix", () => {
    expect(
      normalizeBaseEndpoint("http://localhost:8080/api/v1/generations:export"),
    ).toBe("http://localhost:8080");
  });

  it("preserves a prefix path", () => {
    expect(normalizeBaseEndpoint("https://sigil.example.com/sigil")).toBe(
      "https://sigil.example.com/sigil",
    );
  });

  it("strips the export-path suffix from a prefix-mounted URL", () => {
    expect(
      normalizeBaseEndpoint(
        "https://sigil.example.com/sigil/api/v1/generations:export",
      ),
    ).toBe("https://sigil.example.com/sigil");
  });

  it("does not falsely match a similar-looking suffix", () => {
    expect(
      normalizeBaseEndpoint(
        "http://localhost:8080/api/v1/generations:export-debug",
      ),
    ).toBe("http://localhost:8080/api/v1/generations:export-debug");
  });
});

describe("loadConfig reads ~/.config/sigil/config.env", () => {
  let dir: string;
  let homeBackup: string | undefined;

  beforeEach(() => {
    clearSigilEnv();
    dir = mkdtempSync(join(tmpdir(), "sigil-opencode-loadconfig-"));
    process.env.XDG_CONFIG_HOME = dir;
    homeBackup = process.env.HOME;
    process.env.HOME = dir;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (homeBackup === undefined) delete process.env.HOME;
    else process.env.HOME = homeBackup;
    clearSigilEnv();
  });

  it("returns null when config.env is missing and no shell env is set", async () => {
    const cfg = await loadConfig();
    expect(cfg).toBeNull();
  });

  it("picks up SIGIL_* credentials from config.env when no shell env is set", async () => {
    const cfgDir = join(dir, "sigil");
    mkdirSync(cfgDir, { recursive: true });
    writeFileSync(
      join(cfgDir, "config.env"),
      [
        "SIGIL_ENDPOINT=https://sigil.example.com",
        "SIGIL_AUTH_TENANT_ID=tenant-1",
        "SIGIL_AUTH_TOKEN=glc_token",
        "",
      ].join("\n"),
    );

    const cfg = await loadConfig();
    expect(cfg).not.toBeNull();
    expect(cfg?.endpoint).toBe("https://sigil.example.com");
    expect(cfg?.auth).toEqual({
      mode: "basic",
      basicUser: "tenant-1",
      basicPassword: "glc_token",
      tenantId: "tenant-1",
    });
  });

  it("ignores a stray ~/.config/opencode/opencode-sigil.json on disk", async () => {
    const legacyDir = join(dir, ".config", "opencode");
    mkdirSync(legacyDir, { recursive: true });
    writeFileSync(
      join(legacyDir, "opencode-sigil.json"),
      JSON.stringify({
        enabled: true,
        endpoint: "http://legacy:9090",
        auth: { mode: "none" },
      }),
    );

    const cfg = await loadConfig();
    expect(cfg).toBeNull();
  });

  it("shell env overrides config.env values per key", async () => {
    const cfgDir = join(dir, "sigil");
    mkdirSync(cfgDir, { recursive: true });
    writeFileSync(
      join(cfgDir, "config.env"),
      [
        "SIGIL_ENDPOINT=https://shared.example",
        "SIGIL_AGENT_NAME=opencode",
        "",
      ].join("\n"),
    );

    process.env.SIGIL_ENDPOINT = "https://shell.example";

    const cfg = await loadConfig();
    expect(cfg?.endpoint).toBe("https://shell.example");
    expect(cfg?.agentName).toBe("opencode");
  });
});
