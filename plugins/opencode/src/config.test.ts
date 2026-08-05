import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  loadConfig,
  normalizeBaseEndpoint,
  resolveAutoTagValues,
  resolveConfig,
} from "./config.js";
import { clearAgento11yEnv } from "./testEnv.js";

describe("resolveConfig", () => {
  beforeEach(clearAgento11yEnv);
  afterEach(clearAgento11yEnv);

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

  it("accepts SIGIL_CONTENT_CAPTURE_MODE=full_with_metadata_spans", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full_with_metadata_spans";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("full_with_metadata_spans");
  });

  it("maps SIGIL_CONTENT_CAPTURE_MODE=default to metadata_only without warning", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "default";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("warns and falls back to metadata_only on unknown content-capture value", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "yolo";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        'unsupported contentCapture value "yolo" for SIGIL_CONTENT_CAPTURE_MODE',
      ),
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

  it("defaults guards off, fail-open, with 1500ms timeout", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.guards).toEqual({
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    });
  });

  it("reads guard settings from canonical env vars", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_ENABLED = "on";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "2400";
    process.env.SIGIL_GUARDS_FAIL_OPEN = "no";

    const cfg = resolveConfig();
    expect(cfg?.guards).toEqual({
      enabled: true,
      timeoutMs: 2400,
      failOpen: false,
    });
  });

  it("falls back to the default guard timeout for invalid values", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "nope";
    const cfg = resolveConfig();
    expect(cfg?.guards?.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("invalid integer value"),
    );
    warn.mockRestore();
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

  it("resolves the same config from AGENTO11Y_-only env as from SIGIL_-only env", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AGENT_NAME = "opencode-custom";
    process.env.SIGIL_AGENT_VERSION = "9.9.9";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    process.env.SIGIL_DEBUG = "true";
    process.env.SIGIL_GUARDS_ENABLED = "on";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "2400";
    const legacy = resolveConfig();

    clearAgento11yEnv();
    process.env.AGENTO11Y_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_AGENT_NAME = "opencode-custom";
    process.env.AGENTO11Y_AGENT_VERSION = "9.9.9";
    process.env.AGENTO11Y_AUTH_TENANT_ID = "tenant-1";
    process.env.AGENTO11Y_AUTH_TOKEN = "glc_token";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "full";
    process.env.AGENTO11Y_DEBUG = "true";
    process.env.AGENTO11Y_GUARDS_ENABLED = "on";
    process.env.AGENTO11Y_GUARDS_TIMEOUT_MS = "2400";
    const preferred = resolveConfig();

    expect(preferred).not.toBeNull();
    expect(preferred).toEqual(legacy);
  });

  it("prefers AGENTO11Y_ENDPOINT over SIGIL_ENDPOINT", () => {
    process.env.AGENTO11Y_ENDPOINT = "http://preferred:8080";
    process.env.SIGIL_ENDPOINT = "http://legacy:8080";
    expect(resolveConfig()?.endpoint).toBe("http://preferred:8080");
  });

  it("falls back to SIGIL_ENDPOINT when AGENTO11Y_ENDPOINT is whitespace", () => {
    process.env.AGENTO11Y_ENDPOINT = "   ";
    process.env.SIGIL_ENDPOINT = "http://legacy:8080";
    expect(resolveConfig()?.endpoint).toBe("http://legacy:8080");
  });

  it("keeps the default when the invalid preferred capture mode shadows a valid legacy one", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_CONTENT_CAPTURE_MODE = "yolo";
    process.env.SIGIL_CONTENT_CAPTURE_MODE = "full";
    const cfg = resolveConfig();
    expect(cfg?.contentCapture).toBe("metadata_only");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        'unsupported contentCapture value "yolo" for AGENTO11Y_CONTENT_CAPTURE_MODE',
      ),
    );
    warn.mockRestore();
  });

  it("names the selected key when the invalid preferred guard timeout shadows a valid legacy one", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_GUARDS_TIMEOUT_MS = "nope";
    process.env.SIGIL_GUARDS_TIMEOUT_MS = "2400";
    const cfg = resolveConfig();
    expect(cfg?.guards?.timeoutMs).toBe(1500);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("AGENTO11Y_GUARDS_TIMEOUT_MS"),
    );
    warn.mockRestore();
  });

  it("names the AGENTO11Y_ spelling in the auth warning when the preferred token is set", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_AUTH_TOKEN = "glc_token";
    const cfg = resolveConfig();
    expect(cfg?.auth).toEqual({ mode: "none" });
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "AGENTO11Y_AUTH_TOKEN is set but AGENTO11Y_AUTH_TENANT_ID is missing",
      ),
    );
    warn.mockRestore();
  });
});

describe("resolveConfig OTLP env vars", () => {
  beforeEach(clearAgento11yEnv);
  afterEach(clearAgento11yEnv);

  it("returns no otlp when not configured", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    const cfg = resolveConfig();
    expect(cfg?.otlp).toBeUndefined();
  });

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

  it("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT overrides OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://sigil-otlp.example.com";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://fallback-otlp.example.com";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://sigil-otlp.example.com");
  });

  it("treats whitespace endpoint as unset", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    const cfg = resolveConfig();
    expect(cfg?.otlp).toBeUndefined();
  });

  it("prefers AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT over the SIGIL_ spelling", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://preferred-otlp.example.com";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://legacy-otlp.example.com";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://preferred-otlp.example.com");
  });

  it("whitespace branded OTLP endpoints fall through to OTEL_EXPORTER_OTLP_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
  });

  it("synthesises OTLP Basic auth from tenant and token", () => {
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

  it("omits Authorization header when tenant is missing", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TOKEN = "glc_token";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
    expect(cfg?.otlp?.headers.Authorization).toBeUndefined();
  });

  it("omits Authorization header when token is missing", () => {
    process.env.SIGIL_ENDPOINT = "http://localhost:8080";
    process.env.SIGIL_AUTH_TENANT_ID = "tenant-1";
    process.env.SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT =
      "https://otlp.example.com/otlp";
    const cfg = resolveConfig();
    expect(cfg?.otlp?.endpoint).toBe("https://otlp.example.com/otlp");
    expect(cfg?.otlp?.headers.Authorization).toBeUndefined();
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

describe("resolveAutoTagValues", () => {
  beforeEach(clearAgento11yEnv);
  afterEach(clearAgento11yEnv);

  it("leaves auto-tags unset when the switch is off", () => {
    expect(resolveAutoTagValues(tmpdir())).toBeUndefined();
  });

  it("leaves auto-tags unset when the switch is blank", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "   ";
    expect(resolveAutoTagValues(tmpdir())).toBeUndefined();
  });

  it("leaves auto-tags unset when the switch is false", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "false";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    expect(resolveAutoTagValues(tmpdir())).toBeUndefined();
  });

  it("resolves every name when the switch alone is on", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    // A temp directory is outside any checkout, so the repository and branch
    // resolve to nothing and the switch produces the user value alone.
    expect(resolveAutoTagValues(tmpdir())).toEqual({
      user: "alice@example.com",
    });
  });

  it("narrows the switch to the names in the allowlist", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    expect(resolveAutoTagValues(tmpdir())).toEqual({
      user: "alice@example.com",
    });
  });

  it("reads both variables under the legacy spelling too", () => {
    process.env.SIGIL_AUTO_CODING_AGENT_TAGS = "true";
    process.env.SIGIL_AUTO_CODING_AGENT_TAGS_NAMES = "user";
    process.env.SIGIL_USER_ID = "alice@example.com";
    expect(resolveAutoTagValues(tmpdir())).toEqual({
      user: "alice@example.com",
    });
  });

  it("lets an explicit tag win over the resolved value", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    process.env.AGENTO11Y_TAGS = "user=team-account";
    // The SDK layers AGENTO11Y_TAGS under the client tags, so leaving the key
    // out here is what makes the explicit value win.
    expect(resolveAutoTagValues(tmpdir())).toBeUndefined();
  });

  it("resolves the repository from the directory it is given", () => {
    // The reason resolution lives here and not in resolveConfig: the opencode
    // server can run outside the project root, so the caller passes the project
    // directory rather than process.cwd().
    const project = mkdtempSync(join(tmpdir(), "agento11y-opencode-project-"));
    const git = (args: string[]) =>
      execFileSync("git", args, {
        cwd: project,
        stdio: ["ignore", "pipe", "pipe"],
        encoding: "utf-8",
        timeout: 5000,
      });
    git(["init", "-q", "-b", "main"]);
    git(["config", "user.email", "test@example.com"]);
    git(["config", "user.name", "test"]);
    git(["config", "commit.gpgsign", "false"]);
    // An unborn branch has no resolvable HEAD, so the fixture needs a commit.
    git(["commit", "-q", "--allow-empty", "-m", "init"]);
    git(["remote", "add", "origin", "git@github.com:grafana/other-repo.git"]);
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "repo,branch";

    try {
      expect(resolveAutoTagValues(project)).toEqual({
        repo: "grafana/other-repo",
        "git.branch": "main",
      });
    } finally {
      rmSync(project, { recursive: true, force: true });
    }
  });

  it("warns about an unsupported name and keeps the recognized ones", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS = "true";
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user,team";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    expect(resolveAutoTagValues(tmpdir())).toEqual({
      user: "alice@example.com",
    });
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES has unsupported names team",
      ),
    );
    warn.mockRestore();
  });

  it("warns when the allowlist is set but the switch is off", () => {
    process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES = "user";
    process.env.AGENTO11Y_USER_ID = "alice@example.com";
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    expect(resolveAutoTagValues(tmpdir())).toBeUndefined();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES is set but AGENTO11Y_AUTO_CODING_AGENT_TAGS is off",
      ),
    );
    warn.mockRestore();
  });
});

describe("loadConfig reads ~/.config/agento11y/config.env", () => {
  let dir: string;
  let homeBackup: string | undefined;

  beforeEach(() => {
    clearAgento11yEnv();
    dir = mkdtempSync(join(tmpdir(), "sigil-opencode-loadconfig-"));
    process.env.XDG_CONFIG_HOME = dir;
    homeBackup = process.env.HOME;
    process.env.HOME = dir;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (homeBackup === undefined) delete process.env.HOME;
    else process.env.HOME = homeBackup;
    clearAgento11yEnv();
  });

  it("returns null when config.env is missing and no shell env is set", async () => {
    const cfg = await loadConfig();
    expect(cfg).toBeNull();
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

  it("picks up AGENTO11Y_* credentials from config.env when no shell env is set", async () => {
    const cfgDir = join(dir, "sigil");
    mkdirSync(cfgDir, { recursive: true });
    writeFileSync(
      join(cfgDir, "config.env"),
      [
        "AGENTO11Y_ENDPOINT=https://sigil.example.com",
        "AGENTO11Y_AUTH_TENANT_ID=tenant-1",
        "AGENTO11Y_AUTH_TOKEN=glc_token",
        "",
      ].join("\n"),
    );

    const cfg = await loadConfig();
    expect(cfg?.endpoint).toBe("https://sigil.example.com");
    expect(cfg?.auth).toEqual({
      mode: "basic",
      basicUser: "tenant-1",
      basicPassword: "glc_token",
      tenantId: "tenant-1",
    });
  });

  it("shell SIGIL_ENDPOINT beats config.env AGENTO11Y_ENDPOINT", async () => {
    const cfgDir = join(dir, "sigil");
    mkdirSync(cfgDir, { recursive: true });
    writeFileSync(
      join(cfgDir, "config.env"),
      "AGENTO11Y_ENDPOINT=https://file-preferred.example\n",
    );

    process.env.SIGIL_ENDPOINT = "https://shell-legacy.example";

    const cfg = await loadConfig();
    expect(cfg?.endpoint).toBe("https://shell-legacy.example");
  });
});
