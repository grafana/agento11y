import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resolveConfig, resolveEnvVars } from "./config.js";

function clearEnv() {
  for (const key of Object.keys(process.env)) {
    if (
      key.startsWith("SIGIL_PI_") ||
      key.startsWith("SIGIL_") ||
      key.startsWith("OTEL_")
    ) {
      delete process.env[key];
    }
  }
}

describe("resolveConfig", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("returns null when endpoint is missing", () => {
    expect(resolveConfig({})).toBeNull();
  });

  it("returns null when endpoint is whitespace", () => {
    expect(resolveConfig({ endpoint: "   " })).toBeNull();
  });

  it("parses full config from file", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080",
      auth: { mode: "tenant", tenantId: "my-tenant" },
      agentName: "pi-custom",
      agentVersion: "1.0.0",
      contentCapture: true,
    });
    expect(cfg).not.toBeNull();
    expect(cfg?.endpoint).toBe("http://localhost:8080");
    expect(cfg?.auth).toEqual({ mode: "tenant", tenantId: "my-tenant" });
    expect(cfg?.agentName).toBe("pi-custom");
    expect(cfg?.agentVersion).toBe("1.0.0");
    expect(cfg?.contentCapture).toBe("full");
  });

  it("stores the bare base URL when given a clean endpoint", () => {
    const cfg = resolveConfig({ endpoint: "http://localhost:8080" });
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("strips a trailing slash", () => {
    const cfg = resolveConfig({ endpoint: "http://localhost:8080/" });
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("strips an accidentally-pasted export-path suffix", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.endpoint).toBe("http://localhost:8080");
  });

  it("preserves a prefix path", () => {
    const cfg = resolveConfig({
      endpoint: "https://sigil.example.com/sigil",
    });
    expect(cfg?.endpoint).toBe("https://sigil.example.com/sigil");
  });

  it("strips the export-path suffix from a prefix-mounted URL", () => {
    const cfg = resolveConfig({
      endpoint: "https://sigil.example.com/sigil/api/v1/generations:export",
    });
    expect(cfg?.endpoint).toBe("https://sigil.example.com/sigil");
  });

  it("does not falsely match a similar-looking suffix", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export-debug",
    });
    expect(cfg?.endpoint).toBe(
      "http://localhost:8080/api/v1/generations:export-debug",
    );
  });

  it("defaults contentCapture to metadata_only", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.contentCapture).toBe("metadata_only");
  });

  it("maps boolean true to full", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      contentCapture: true,
    });
    expect(cfg?.contentCapture).toBe("full");
  });

  it("maps boolean false to metadata_only", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      contentCapture: false,
    });
    expect(cfg?.contentCapture).toBe("metadata_only");
  });

  it("accepts mode string no_tool_content", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      contentCapture: "no_tool_content",
    });
    expect(cfg?.contentCapture).toBe("no_tool_content");
  });

  it("warns and falls back on unknown mode string", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      contentCapture: "yolo",
    });
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("unsupported contentCapture"),
    );
    warn.mockRestore();
  });

  it("defaults agentName to 'pi'", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      agentName: "   ",
    });
    expect(cfg?.agentName).toBe("pi");
  });

  it("env vars override file values", () => {
    process.env.SIGIL_PI_ENDPOINT = "http://env:9090/api/v1/generations:export";
    process.env.SIGIL_PI_AGENT_NAME = "pi-env";
    process.env.SIGIL_PI_CONTENT_CAPTURE = "true";
    process.env.SIGIL_PI_AUTH_MODE = "bearer";
    process.env.SIGIL_PI_BEARER_TOKEN = "tok-123";

    const cfg = resolveConfig({
      endpoint: "http://file:8080",
      agentName: "pi-file",
      contentCapture: false,
      auth: { mode: "none" },
    });

    expect(cfg?.endpoint).toBe("http://env:9090");
    expect(cfg?.agentName).toBe("pi-env");
    expect(cfg?.contentCapture).toBe("full");
    expect(cfg?.auth).toEqual({ mode: "bearer", bearerToken: "tok-123" });
  });

  it("env bool parsing is case-insensitive", () => {
    process.env.SIGIL_PI_CONTENT_CAPTURE = "On";
    process.env.SIGIL_PI_ENDPOINT =
      "http://localhost:8080/api/v1/generations:export";

    const cfg = resolveConfig({ auth: { mode: "none" } });

    expect(cfg?.contentCapture).toBe("full");
  });

  it("parses bearer auth from file", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "bearer", bearerToken: "my-token" },
    });
    expect(cfg?.auth).toEqual({ mode: "bearer", bearerToken: "my-token" });
  });

  it("returns null on bearer auth with missing token", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "bearer", bearerToken: "" },
    });
    expect(cfg).toBeNull();
  });

  it("parses basic auth with tenantId defaulting to user", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "basic", user: "12345", password: "pass" },
    });
    expect(cfg?.auth).toEqual({
      mode: "basic",
      user: "12345",
      password: "pass",
      tenantId: "12345",
    });
  });

  it("parses basic auth with explicit tenantId", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: {
        mode: "basic",
        user: "12345",
        password: "pass",
        tenantId: "my-org",
      },
    });
    expect(cfg?.auth).toEqual({
      mode: "basic",
      user: "12345",
      password: "pass",
      tenantId: "my-org",
    });
  });

  it("returns null on basic auth with missing fields", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "basic", user: "user", password: "" },
    });
    expect(cfg).toBeNull();
  });

  it("defaults auth to none", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.auth).toEqual({ mode: "none" });
  });

  it("returns null on unsupported auth mode", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      auth: { mode: "berer" },
    });
    expect(cfg).toBeNull();
  });
});

describe("resolveConfig canonical SIGIL_* env vars", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("reads SIGIL_ENDPOINT as the endpoint", () => {
    process.env.SIGIL_ENDPOINT = "http://canonical:8080";
    const cfg = resolveConfig({});
    expect(cfg?.endpoint).toBe("http://canonical:8080");
  });

  it("SIGIL_ENDPOINT wins over SIGIL_PI_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://canonical:8080";
    process.env.SIGIL_PI_ENDPOINT = "http://legacy:9090";
    const cfg = resolveConfig({});
    expect(cfg?.endpoint).toBe("http://canonical:8080");
  });

  it("derives basic auth from SIGIL_AUTH_TENANT_ID + SIGIL_AUTH_TOKEN", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig({});
    expect(cfg?.auth).toEqual({
      mode: "basic",
      user: "tenant-1",
      password: "glc_token",
      tenantId: "tenant-1",
    });
  });

  it("does not derive auth from a partial canonical pair", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    const cfg = resolveConfig({});
    expect(cfg?.auth).toEqual({ mode: "none" });
  });

  it("explicit SIGIL_PI_AUTH_MODE bypasses canonical derivation", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    process.env.SIGIL_PI_AUTH_MODE = "bearer";
    process.env.SIGIL_PI_BEARER_TOKEN = "bearer-tok";
    const cfg = resolveConfig({});
    expect(cfg?.auth).toEqual({ mode: "bearer", bearerToken: "bearer-tok" });
  });

  it("explicit file.auth.mode bypasses canonical derivation", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig({ auth: { mode: "none" } });
    expect(cfg?.auth).toEqual({ mode: "none" });
  });

  it("SIGIL_DEBUG flips debug on", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_DEBUG = "true";
    const cfg = resolveConfig({});
    expect(cfg?.debug).toBe(true);
  });

  it("SIGIL_PI_DEBUG still wins as legacy override when SIGIL_DEBUG is unset", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_PI_DEBUG = "true";
    const cfg = resolveConfig({ debug: false });
    expect(cfg?.debug).toBe(true);
  });

  it("SIGIL_CONTENT_CAPTURE_MODE sets content capture", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    const cfg = resolveConfig({});
    expect(cfg?.contentCapture).toBe("full");
  });

  it("SIGIL_AGENT_NAME and SIGIL_AGENT_VERSION override file values", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "pi-canonical";
    process.env.SIGIL_AGENT_VERSION = "9.9.9";
    const cfg = resolveConfig({ agentName: "file-name", agentVersion: "1.0" });
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
    const cfg = resolveConfig({});
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("falls back to OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "https://otlp.example.com/otlp";
    const cfg = resolveConfig({});
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("synthesises OTLP Basic auth from canonical SIGIL_* creds", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig({});
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
    const cfg = resolveConfig({});
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("tenant-1:otel-only-token");
    // Generation auth still uses the unscoped token.
    expect(cfg?.auth).toEqual({
      mode: "basic",
      user: "tenant-1",
      password: "sigil-only-token",
      tenantId: "tenant-1",
    });
  });

  it("explicit SIGIL_PI_OTLP_* creds win over canonical synthesis", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "sigil-token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    process.env.SIGIL_PI_OTLP_BASIC_USER = "otlp-user";
    process.env.SIGIL_PI_OTLP_BASIC_PASSWORD = "otlp-pass";
    const cfg = resolveConfig({});
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("otlp-user:otlp-pass");
  });
});

describe("resolveConfig otlp", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("returns no otlp when not configured", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.otlp).toBeUndefined();
  });

  it("parses otlp with basic auth", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://otlp-gw.grafana.net/otlp",
        basicUser: "123456",
        basicPassword: "glc_token",
      },
    });
    expect(cfg?.otlp).toBeDefined();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp-gw.grafana.net/otlp");
    expect(cfg?.otlp?.headers.Authorization).toMatch(/^Basic /);
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("123456:glc_token");
  });

  it("parses otlp with bearer token", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://otlp.example.com",
        bearerToken: "my-token",
      },
    });
    expect(cfg?.otlp?.headers.Authorization).toBe("Bearer my-token");
  });

  it("parses otlp with custom headers", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://otlp.example.com",
        headers: { "X-Custom": "value" },
      },
    });
    expect(cfg?.otlp?.headers["X-Custom"]).toBe("value");
  });

  it("env vars override otlp config", () => {
    process.env.SIGIL_PI_OTLP_ENDPOINT = "https://env-otlp.example.com";
    process.env.SIGIL_PI_OTLP_BASIC_USER = "env-user";
    process.env.SIGIL_PI_OTLP_BASIC_PASSWORD = "env-pass";

    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://file-otlp.example.com",
        basicUser: "file-user",
        basicPassword: "file-pass",
      },
    });
    expect(cfg?.otlp?.endpoint).toBe("https://env-otlp.example.com");
    const decoded = Buffer.from(
      cfg!.otlp!.headers.Authorization!.replace("Basic ", ""),
      "base64",
    ).toString();
    expect(decoded).toBe("env-user:env-pass");
  });

  it("basic auth takes precedence over bearer", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://otlp.example.com",
        basicUser: "user",
        basicPassword: "pass",
        bearerToken: "token",
      },
    });
    expect(cfg?.otlp?.headers.Authorization).toMatch(/^Basic /);
  });

  it("explicit headers.Authorization wins over basic and bearer shorthand", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      otlp: {
        endpoint: "https://otlp.example.com",
        headers: { Authorization: "Bearer custom-token" },
        basicUser: "user",
        basicPassword: "pass",
        bearerToken: "token",
      },
    });
    expect(cfg?.otlp?.headers.Authorization).toBe("Bearer custom-token");
  });
});

describe("resolveConfig redaction", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("defaults all redaction knobs to true when file omits the block", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.redaction).toEqual({
      enabled: true,
      redactInputMessages: true,
      redactEmailAddresses: true,
    });
  });

  it("file-level redaction.enabled = false disables it", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      redaction: { enabled: false },
    });
    expect(cfg?.redaction.enabled).toBe(false);
    // partial override preserves the rest as defaults
    expect(cfg?.redaction.redactInputMessages).toBe(true);
    expect(cfg?.redaction.redactEmailAddresses).toBe(true);
  });

  it("partial file override preserves other defaults", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      redaction: { redactEmailAddresses: false },
    });
    expect(cfg?.redaction).toEqual({
      enabled: true,
      redactInputMessages: true,
      redactEmailAddresses: false,
    });
  });

  it("env vars override file values (truthy aliases)", () => {
    process.env.SIGIL_PI_REDACTION_ENABLED = "1";
    process.env.SIGIL_PI_REDACT_INPUT_MESSAGES = "true";
    process.env.SIGIL_PI_REDACT_EMAIL_ADDRESSES = "yes";

    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      redaction: {
        enabled: false,
        redactInputMessages: false,
        redactEmailAddresses: false,
      },
    });

    expect(cfg?.redaction).toEqual({
      enabled: true,
      redactInputMessages: true,
      redactEmailAddresses: true,
    });
  });

  it("env vars override file values (falsy aliases)", () => {
    process.env.SIGIL_PI_REDACTION_ENABLED = "0";
    process.env.SIGIL_PI_REDACT_INPUT_MESSAGES = "false";
    process.env.SIGIL_PI_REDACT_EMAIL_ADDRESSES = "off";

    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      redaction: {
        enabled: true,
        redactInputMessages: true,
        redactEmailAddresses: true,
      },
    });

    expect(cfg?.redaction).toEqual({
      enabled: false,
      redactInputMessages: false,
      redactEmailAddresses: false,
    });
  });

  it("accepts 'on' as truthy alias", () => {
    process.env.SIGIL_PI_REDACTION_ENABLED = "on";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      redaction: { enabled: false },
    });
    expect(cfg?.redaction.enabled).toBe(true);
  });

  it("accepts 'no' as falsy alias", () => {
    process.env.SIGIL_PI_REDACT_INPUT_MESSAGES = "no";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.redaction.redactInputMessages).toBe(false);
  });
});

describe("resolveConfig guards", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("defaults to disabled with 1500ms timeout and fail-open=true", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.guards).toEqual({
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    });
  });

  it("reads file-level guards block", () => {
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      guards: { enabled: true, timeoutMs: 2500, failOpen: false },
    });
    expect(cfg?.guards).toEqual({
      enabled: true,
      timeoutMs: 2500,
      failOpen: false,
    });
  });

  it("env overrides file values", () => {
    process.env.SIGIL_GUARDS_ENABLED = "true";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "1500";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "true";

    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      guards: { enabled: false, timeoutMs: 2500, failOpen: false },
    });

    expect(cfg?.guards).toEqual({
      enabled: true,
      timeoutMs: 1500,
      failOpen: true,
    });
  });

  it("enables guards via canonical env var only", () => {
    process.env.SIGIL_GUARDS_ENABLED = "true";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.guards.enabled).toBe(true);
  });

  it("does not recognise the pi-prefixed alias", () => {
    process.env.SIGIL_PI_GUARDS_ENABLED = "true";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.guards.enabled).toBe(false);
  });

  it("warns and falls back to default when SIGIL_GUARDS_ENABLED is not a boolean", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_GUARDS_ENABLED = "maybe";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg).not.toBeNull();
    expect(cfg?.guards.enabled).toBe(false);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("invalid boolean value for SIGIL_GUARDS_ENABLED"),
    );
    warn.mockRestore();
  });

  it("warns and falls back to default when SIGIL_GUARDS_TIMEOUT_MS is not numeric", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "fast";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
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
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "0";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.guards.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "invalid integer value for SIGIL_GUARDS_TIMEOUT_MS",
      ),
    );
    warn.mockRestore();
  });

  it("rejects guards.timeoutMs=0 from the file config", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
      guards: { timeoutMs: 0 },
    });
    expect(cfg?.guards.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("invalid integer value for guards.* file entry"),
    );
    warn.mockRestore();
  });

  it("warns and falls back to default when SIGIL_GUARDS_FAIL_OPEN is not a boolean", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_GUARDS_FAIL_OPEN = "sometimes";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
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
    process.env.SIGIL_GUARDS_ENABLED = "on";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "no";
    const cfg = resolveConfig({
      endpoint: "http://localhost:8080/api/v1/generations:export",
    });
    expect(cfg?.guards.enabled).toBe(true);
    expect(cfg?.guards.failOpen).toBe(false);
  });
});

describe("resolveEnvVars", () => {
  beforeEach(clearEnv);
  afterEach(clearEnv);

  it("replaces ${VAR} with env value", () => {
    process.env.MY_TOKEN = "secret-123";
    expect(resolveEnvVars("Bearer ${MY_TOKEN}")).toBe("Bearer secret-123");
  });

  it("replaces missing vars with empty string", () => {
    expect(resolveEnvVars("${NONEXISTENT}")).toBe("");
  });

  it("leaves strings without vars unchanged", () => {
    expect(resolveEnvVars("plain-value")).toBe("plain-value");
  });
});
