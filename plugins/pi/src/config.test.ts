import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { clearAgento11yEnv as clearEnv } from "./testEnv.js";

const { loggerMock } = vi.hoisted(() => ({
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

import { loadConfig, resolveConfig } from "./config.js";

describe("resolveConfig", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

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

  it("strips a trailing slash", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080/";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("strips an accidentally-pasted export-path suffix", () => {
    process.env.SIGIL_ENDPOINT =
      "http://localhost:8080/api/v1/generations:export";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("preserves a prefix path", () => {
    process.env.SIGIL_ENDPOINT = "https://sigil.example.com/sigil";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("https://sigil.example.com/sigil");
  });

  it("strips the export-path suffix from a prefix-mounted URL", () => {
    process.env.SIGIL_ENDPOINT =
      "https://sigil.example.com/sigil/api/v1/generations:export";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("https://sigil.example.com/sigil");
  });

  it("does not falsely match a similar-looking suffix", () => {
    process.env.SIGIL_ENDPOINT =
      "http://localhost:8080/api/v1/generations:export-debug";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe(
      "http://localhost:8080/api/v1/generations:export-debug",
    );
  });

  it("defaults contentCapture to metadata_only", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
  });

  it("defaults input message redaction to on", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.redactInputMessages).toBe(true);
  });

  it("SIGIL_REDACT_INPUT_MESSAGES controls input message redaction", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_REDACT_INPUT_MESSAGES = "false";
    const cfg = resolveConfig();
    expect(cfg?.redactInputMessages).toBe(false);
  });

  it("accepts mode string no_tool_content", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "no_tool_content";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("no_tool_content");
  });

  it("accepts SIGIL_CONTENT_CAPTURE_MODE=full_with_metadata_spans", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full_with_metadata_spans";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("full_with_metadata_spans");
  });

  it("maps SIGIL_CONTENT_CAPTURE_MODE=default to metadata_only without warning", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "default";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("warns and falls back on unknown mode string", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "yolo";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("unsupported contentCapture"),
    );
    warn.mockRestore();
  });

  it("defaults agentName to 'pi'", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "   ";
    const cfg = resolveConfig();
    expect(cfg?.agentName).toBe("pi");
  });

  it("env bool parsing is case-insensitive", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "On";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("full");
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

  it("falls back to none when only the tenant id is set", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
  });

  it("falls back to none when only the auth token is set", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
  });

  it("defaults auth to none when no creds are set", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
  });
});

describe("resolveConfig AGENTO11Y_* aliases", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  function setBranded(prefix: string): void {
    process.env[`${prefix}ENDPOINT`] = "http://localhost:8080";
    process.env[`${prefix}AUTH_TENANT_ID`] = "tenant-1";
    process.env[`${prefix}AUTH_TOKEN`] = "glc_token";
    process.env[`${prefix}AGENT_NAME`] = "pi-alias";
    process.env[`${prefix}AGENT_VERSION`] = "1.2.3";
    process.env[`${prefix}CONTENT_CAPTURE_MODE`] = "full";
    process.env[`${prefix}REDACT_INPUT_MESSAGES`] = "false";
    process.env[`${prefix}GUARDS_ENABLED`] = "true";
    process.env[`${prefix}GUARDS_TIMEOUT_MS`] = "2500";
    process.env[`${prefix}GUARDS_FAIL_OPEN`] = "false";
  }

  it("preferred-only env produces the same config as legacy-only env", () => {
    setBranded("AGENTO11Y_");
    const preferred = resolveConfig();
    clearEnv();
    setBranded("SIGIL_");
    const legacy = resolveConfig();
    expect(preferred).not.toBeNull();
    expect(preferred).toEqual(legacy);
    expect(preferred?.guards).toEqual({
      enabled: true,
      timeoutMs: 2500,
      failOpen: false,
    });
  });

  it("AGENTO11Y_ENDPOINT beats SIGIL_ENDPOINT", () => {
    process.env.AGENTO11Y_ENDPOINT = "http://preferred:8080";
    process.env.SIGIL_ENDPOINT = "http://legacy:8080";
    expect(resolveConfig()?.endpoint).toBe("http://preferred:8080");
  });

  it("blank AGENTO11Y_ENDPOINT falls back to SIGIL_ENDPOINT", () => {
    process.env.AGENTO11Y_ENDPOINT = "   ";
    process.env.SIGIL_ENDPOINT = "http://legacy:8080";
    expect(resolveConfig()?.endpoint).toBe("http://legacy:8080");
  });

  it("invalid AGENTO11Y_GUARDS_ENABLED keeps the default over a valid SIGIL_GUARDS_ENABLED", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_GUARDS_ENABLED = "maybe";
    process.env.SIGIL_GUARDS_ENABLED = "true";
    const cfg = resolveConfig();
    expect(cfg?.guards.enabled).toBe(false);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "invalid boolean value for AGENTO11Y_GUARDS_ENABLED",
      ),
    );
    warn.mockRestore();
  });

  it("invalid AGENTO11Y_CONTENT_CAPTURE_MODE ignores a valid SIGIL_CONTENT_CAPTURE_MODE", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "yolo";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("AGENTO11Y_CONTENT_CAPTURE_MODE"),
    );
    warn.mockRestore();
  });

  it("clearAgento11yEnv strips ambient AGENTO11Y_* vars", () => {
    process.env.AGENTO11Y_ENDPOINT = "http://ambient:8080";
    clearEnv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBeUndefined();
    expect(resolveConfig()).toBeNull();
  });
});

describe("resolveConfig canonical SIGIL_* env vars", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("reads SIGIL_ENDPOINT as the endpoint", () => {
    process.env.SIGIL_ENDPOINT = "http://canonical:8080";
    const cfg = resolveConfig();
    expect(cfg?.endpoint).toBe("http://canonical:8080");
  });

  it("SIGIL_CONTENT_CAPTURE_MODE sets content capture", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("full");
  });

  it("SIGIL_AGENT_NAME and SIGIL_AGENT_VERSION are read from env", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "pi-canonical";
    process.env.SIGIL_AGENT_VERSION = "9.9.9";
    const cfg = resolveConfig();
    expect(cfg?.agentName).toBe("pi-canonical");
    expect(cfg?.agentVersion).toBe("9.9.9");
  });
});

describe("resolveConfig canonical OTLP env vars", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("reads SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("falls back to OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("reads AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("whitespace SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT falls through to OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "https://std.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://std.example.com/otlp");
  });

  it("whitespace AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT falls through to OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "https://std.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://std.example.com/otlp");
  });

  it("returns no otlp when not configured", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.otlp).toBeUndefined();
  });

  it("synthesises OTLP Basic auth from canonical SIGIL_* creds", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.headers.Authorization).toMatch(/^Basic /);
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("tenant-1:glc_token");
  });

  it("SIGIL_OTEL_AUTH_TOKEN overrides SIGIL_AUTH_TOKEN for OTel only", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "sigil-only-token";
    process.env.SIGIL_OTEL_AUTH_TOKEN = "otel-only-token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("tenant-1:otel-only-token");
    expect(cfg?.auth).toEqual({
      mode: "basic",
      basicUser: "tenant-1",
      basicPassword: "sigil-only-token",
      tenantId: "tenant-1",
    });
  });

  it("keeps explicit OTEL_EXPORTER_OTLP_HEADERS Authorization", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "sigil-token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    process.env.OTEL_EXPORTER_OTLP_HEADERS =
      "Authorization=Basic explicit-otlp,X-Test=ok";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.headers.Authorization).toBe("Basic explicit-otlp");
    expect(cfg?.otlp?.headers["X-Test"]).toBe("ok");
  });

  it("env var overrides OTLP endpoint", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://env-otlp.example.com";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://env-otlp.example.com");
  });
});

describe("resolveConfig guards", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("defaults to disabled with 1500ms timeout and fail-open=true", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.guards).toEqual({
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    });
  });

  it("env values populate the guards block", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_ENABLED = "true";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "2500";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "false";

    const cfg = resolveConfig();

    expect(cfg?.guards).toEqual({
      enabled: true,
      timeoutMs: 2500,
      failOpen: false,
    });
  });

  it("enables guards via canonical env var only", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_ENABLED = "true";
    const cfg = resolveConfig();
    expect(cfg?.guards.enabled).toBe(true);
  });

  it("warns and falls back to default when SIGIL_GUARDS_ENABLED is not a boolean", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_ENABLED = "maybe";
    const cfg = resolveConfig();
    expect(cfg).not.toBeNull();
    expect(cfg?.guards.enabled).toBe(false);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("invalid boolean value for SIGIL_GUARDS_ENABLED"),
    );
    warn.mockRestore();
  });

  it("warns and falls back to default when SIGIL_GUARDS_TIMEOUT_MS is not numeric", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "fast";
    const cfg = resolveConfig();
    expect(cfg).not.toBeNull();
    expect(cfg?.guards.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "invalid integer value for SIGIL_GUARDS_TIMEOUT_MS",
      ),
    );
    warn.mockRestore();
  });

  it("rejects SIGIL_GUARDS_TIMEOUT_MS=0 so the SDK does not fall back to its 15s default", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "0";
    const cfg = resolveConfig();
    expect(cfg?.guards.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "invalid integer value for SIGIL_GUARDS_TIMEOUT_MS",
      ),
    );
    warn.mockRestore();
  });

  it("warns and falls back to default when SIGIL_GUARDS_FAIL_OPEN is not a boolean", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "sometimes";
    const cfg = resolveConfig();
    expect(cfg).not.toBeNull();
    expect(cfg?.guards.failOpen).toBe(true);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "invalid boolean value for SIGIL_GUARDS_FAIL_OPEN",
      ),
    );
    warn.mockRestore();
  });

  it("supports truthy aliases (on, yes, 1) for env vars", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_ENABLED = "on";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "no";
    const cfg = resolveConfig();
    expect(cfg?.guards.enabled).toBe(true);
    expect(cfg?.guards.failOpen).toBe(false);
  });
});

describe("loadConfig reads ~/.config/agento11y/config.env", () => {
  let dir: string;
  let homeBackup: string | undefined;

  beforeEach(() => {
    clearEnv();
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-loadconfig-"));
    // Redirect both XDG_CONFIG_HOME and HOME at the tmpdir so the dotenv
    // loader's lazy homedir() lookup follows. The dotenv file becomes the
    // only source of credentials.
    process.env.XDG_CONFIG_HOME = dir;
    homeBackup = process.env.HOME;
    process.env.HOME = dir;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (homeBackup === undefined) delete process.env.HOME;
    else process.env.HOME = homeBackup;
    clearEnv();
  });

  it("picks up SIGIL_* credentials from config.env when no shell env is set", async () => {
    const cfgDir = join(dir, "agento11y");
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

  it("ignores a stray ~/.config/sigil-pi/config.json on disk", async () => {
    const legacyDir = join(dir, "sigil-pi");
    mkdirSync(legacyDir, { recursive: true });
    writeFileSync(
      join(legacyDir, "config.json"),
      JSON.stringify({ endpoint: "http://legacy:9090" }),
    );

    const cfg = await loadConfig();
    expect(cfg).toBeNull();
  });
});
