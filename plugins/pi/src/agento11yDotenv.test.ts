import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { loggerMock, homedirOverride } = vi.hoisted(() => ({
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
  homedirOverride: { value: undefined as string | undefined },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

// agento11yConfigEnvPath stats files under the resolved config root, so tests
// exercising the $HOME/.config fallback must not consult the developer's
// real home (which may contain a live config.env). Overridable homedir,
// pass-through otherwise.
vi.mock("node:os", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:os")>();
  return {
    ...actual,
    homedir: () => homedirOverride.value ?? actual.homedir(),
  };
});

import {
  agento11yConfigEnvPath,
  applyAgento11yDotenv,
  loadAgento11yDotenv,
  parseAgento11yDotenv,
} from "./agento11yDotenv.js";
import { clearAgento11yEnv } from "./testEnv.js";

describe("parseAgento11yDotenv", () => {
  it("parses the full sample from the Go reference test", () => {
    // Mirrors plugins/agento11y/internal/dotenv/dotenv_test.go::TestLoadDotenv
    const body = `# leading comment
SIGIL_ENDPOINT=https://sigil.example.com
export SIGIL_AUTH_TENANT_ID=alice
SIGIL_AUTH_TOKEN="secret with spaces"
SIGIL_CONTENT_CAPTURE_MODE='full'
SIGIL_TAGS=a=1,b=2  # inline comment
SIGIL_DEBUG=true
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com/otlp
PATH=/tmp/not-loaded
no_equals_line
=missingkey
EMPTY=
`;
    const got = parseAgento11yDotenv(body);
    expect(got).toEqual({
      SIGIL_ENDPOINT: "https://sigil.example.com",
      SIGIL_AUTH_TENANT_ID: "alice",
      SIGIL_AUTH_TOKEN: "secret with spaces",
      SIGIL_CONTENT_CAPTURE_MODE: "full",
      SIGIL_TAGS: "a=1,b=2",
      SIGIL_DEBUG: "true",
      OTEL_EXPORTER_OTLP_ENDPOINT: "https://otlp.example.com/otlp",
    });
  });

  it("handles quoted-value edge cases the same way as the Go parser", () => {
    // Mirrors TestLoadDotenvQuotedValueEdgeCases
    const body = `SIGIL_DOUBLE="my secret" # comment
SIGIL_SINGLE='other secret' # comment
SIGIL_HASH_INSIDE="value # not a comment"
SIGIL_PLAIN_COMMENT=plain # trailing
SIGIL_SPACES_INSIDE="  has spaces  "
SIGIL_UNTERMINATED="oops
`;
    const got = parseAgento11yDotenv(body);
    expect(got.SIGIL_DOUBLE).toBe("my secret");
    expect(got.SIGIL_SINGLE).toBe("other secret");
    expect(got.SIGIL_HASH_INSIDE).toBe("value # not a comment");
    expect(got.SIGIL_PLAIN_COMMENT).toBe("plain");
    expect(got.SIGIL_SPACES_INSIDE).toBe("  has spaces  ");
    // Unterminated quote falls through to the literal value, including
    // the leading quote. Matches Go parseDotenvValue.
    expect(got.SIGIL_UNTERMINATED).toBe('"oops');
  });

  it("skips comments, blank lines, and lines without an equals sign", () => {
    const body = `
# top-level comment
   # indented comment

SIGIL_ENDPOINT=https://ok

no_equals_line
=missingkey
EMPTY=
`;
    const got = parseAgento11yDotenv(body);
    expect(got).toEqual({ SIGIL_ENDPOINT: "https://ok" });
  });

  it("honors the optional 'export ' prefix", () => {
    const got = parseAgento11yDotenv(
      `export SIGIL_ENDPOINT=https://exported\n`,
    );
    expect(got).toEqual({ SIGIL_ENDPOINT: "https://exported" });
  });

  it("allows AGENTO11Y_-prefixed keys", () => {
    const got = parseAgento11yDotenv("AGENTO11Y_ENDPOINT=https://ok\n");
    expect(got).toEqual({ AGENTO11Y_ENDPOINT: "https://ok" });
  });

  it("ignores keys outside the allow-list", () => {
    const body = `PATH=/tmp/not-loaded
HOME=/tmp/not-loaded
SIGIL_ENDPOINT=https://ok
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic xyz
OTEL_EXPORTER_OTLP_INSECURE=true
OTEL_SERVICE_NAME=my-service
OTEL_RESOURCE_ATTRIBUTES=service.name=other
`;
    const got = parseAgento11yDotenv(body);
    expect(got).toEqual({
      SIGIL_ENDPOINT: "https://ok",
      OTEL_EXPORTER_OTLP_HEADERS: "Authorization=Basic xyz",
      OTEL_EXPORTER_OTLP_INSECURE: "true",
      OTEL_SERVICE_NAME: "my-service",
    });
    expect(got).not.toHaveProperty("PATH");
    expect(got).not.toHaveProperty("HOME");
    expect(got).not.toHaveProperty("OTEL_RESOURCE_ATTRIBUTES");
  });

  it("accepts both CRLF and LF line endings", () => {
    const body = "SIGIL_ENDPOINT=https://crlf\r\nSIGIL_DEBUG=true\r\n";
    expect(parseAgento11yDotenv(body)).toEqual({
      SIGIL_ENDPOINT: "https://crlf",
      SIGIL_DEBUG: "true",
    });
  });
});

describe("agento11yConfigEnvPath", () => {
  let fakeHome: string;

  beforeEach(() => {
    clearAgento11yEnv();
    fakeHome = mkdtempSync(join(tmpdir(), "sigil-pi-home-"));
    homedirOverride.value = fakeHome;
  });

  afterEach(() => {
    homedirOverride.value = undefined;
    rmSync(fakeHome, { recursive: true, force: true });
    clearAgento11yEnv();
  });

  it("honors an absolute XDG_CONFIG_HOME", () => {
    // Fresh dir, not a fixed /tmp path: resolution is stat-based, so a
    // leftover sigil/config.env at a shared path would flip the result.
    const xdg = join(fakeHome, "custom-config");
    process.env.XDG_CONFIG_HOME = xdg;
    expect(agento11yConfigEnvPath()).toBe(join(xdg, "agento11y", "config.env"));
  });

  it("ignores a relative XDG_CONFIG_HOME and falls back to $HOME/.config", () => {
    process.env.XDG_CONFIG_HOME = "relative-path";
    expect(agento11yConfigEnvPath()).toBe(
      join(fakeHome, ".config", "agento11y", "config.env"),
    );
  });

  it("falls back to $HOME/.config/agento11y/config.env when XDG_CONFIG_HOME is unset", () => {
    expect(agento11yConfigEnvPath()).toBe(
      join(fakeHome, ".config", "agento11y", "config.env"),
    );
  });

  it("ignores an XDG_CONFIG_HOME that is whitespace only", () => {
    process.env.XDG_CONFIG_HOME = "   ";
    expect(agento11yConfigEnvPath()).toBe(
      join(fakeHome, ".config", "agento11y", "config.env"),
    );
  });

  // Mirrors plugins/agento11y/internal/dotenv/dotenv_test.go::TestFilePathResolution.
  const resolutionCases: {
    name: string;
    apps: string[];
    wantApp: string;
  }[] = [
    { name: "neither exists defaults to new", apps: [], wantApp: "agento11y" },
    { name: "only new exists", apps: ["agento11y"], wantApp: "agento11y" },
    { name: "only legacy exists", apps: ["sigil"], wantApp: "sigil" },
    {
      name: "both exist prefers new",
      apps: ["agento11y", "sigil"],
      wantApp: "agento11y",
    },
  ];
  for (const tc of resolutionCases) {
    it(tc.name, () => {
      process.env.XDG_CONFIG_HOME = fakeHome;
      for (const app of tc.apps) {
        mkdirSync(join(fakeHome, app), { recursive: true });
        writeFileSync(join(fakeHome, app, "config.env"), "SIGIL_ENDPOINT=x\n");
      }
      expect(agento11yConfigEnvPath()).toBe(
        join(fakeHome, tc.wantApp, "config.env"),
      );
    });
  }
});

describe("loadAgento11yDotenv", () => {
  let dir: string;

  beforeEach(() => {
    clearAgento11yEnv();
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-dotenv-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    clearAgento11yEnv();
  });

  it("returns an empty map silently when the file is missing", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    const got = loadAgento11yDotenv(join(dir, "does-not-exist.env"));
    expect(got).toEqual({});
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("parses an on-disk file", () => {
    const path = join(dir, "config.env");
    writeFileSync(
      path,
      "SIGIL_ENDPOINT=https://from-file\nSIGIL_AUTH_TOKEN=tok\n",
    );
    expect(loadAgento11yDotenv(path)).toEqual({
      SIGIL_ENDPOINT: "https://from-file",
      SIGIL_AUTH_TOKEN: "tok",
    });
  });

  it("warns and returns an empty map on non-ENOENT read failures", () => {
    // A path that points to a directory rather than a file triggers an
    // EISDIR (or similar) read error, not ENOENT.
    const warn = loggerMock.warn;
    warn.mockClear();
    const got = loadAgento11yDotenv(dir);
    expect(got).toEqual({});
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn.mock.calls[0]?.[0]).toMatch(/^failed to read/);
    warn.mockRestore();
  });
});

describe("applyAgento11yDotenv", () => {
  let dir: string;

  beforeEach(() => {
    clearAgento11yEnv();
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-dotenv-apply-"));
    process.env.XDG_CONFIG_HOME = dir;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    clearAgento11yEnv();
  });

  function configPath(): string {
    return join(dir, "sigil", "config.env");
  }

  function writeConfig(body: string): void {
    const path = configPath();
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, body);
  }

  it("fills empty OS env values from config.env", () => {
    writeConfig(
      "SIGIL_ENDPOINT=https://from-file\nSIGIL_AUTH_TENANT_ID=tenant-1\nSIGIL_AUTH_TOKEN=tok\n",
    );
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.SIGIL_AUTH_TENANT_ID).toBe("tenant-1");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");
  });

  it("keeps non-empty OS env values intact", () => {
    process.env.SIGIL_ENDPOINT = "https://from-shell";
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-shell");
  });

  it("treats an empty OS env value as unset", () => {
    process.env.SIGIL_ENDPOINT = "";
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
  });

  it("treats a whitespace-only OS env value as unset", () => {
    process.env.SIGIL_ENDPOINT = "   ";
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
  });

  it("does nothing silently when config.env is missing", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBeUndefined();
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("does not touch keys outside the allow-list", () => {
    const before = process.env.PATH;
    writeConfig("PATH=/tmp/not-loaded\nSIGIL_ENDPOINT=https://ok\n");
    applyAgento11yDotenv();
    expect(process.env.PATH).toBe(before);
    expect(process.env.SIGIL_ENDPOINT).toBe("https://ok");
  });

  it("picks up edits to config.env on a second call", () => {
    // Owned keys must be refreshed on each call so edits to config.env
    // propagate across repeated session_start events.
    writeConfig("SIGIL_ENDPOINT=https://first\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://first");

    writeConfig("SIGIL_ENDPOINT=https://second\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://second");
  });

  it("clears keys that were removed from config.env on a second call", () => {
    writeConfig("SIGIL_ENDPOINT=https://from-file\nSIGIL_AUTH_TOKEN=tok\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");

    // Drop SIGIL_AUTH_TOKEN. After re-applying, the stale value must be
    // removed from process.env or downstream auth resolution would silently
    // keep using credentials the user thought they had deleted.
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.SIGIL_AUTH_TOKEN).toBeUndefined();
  });

  it("keeps shell-supplied OS env values intact across multiple calls", () => {
    process.env.SIGIL_ENDPOINT = "https://from-shell";
    writeConfig("SIGIL_ENDPOINT=https://first\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-shell");

    // A later config.env edit must still not clobber the shell export —
    // "OS env wins per key" is the contract loadConfig advertises.
    writeConfig("SIGIL_ENDPOINT=https://second\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-shell");
  });

  it("yields to a non-empty OS env value set after a key was copied from config.env", () => {
    // README/loadConfig promise: "OS env always wins per key". A non-empty
    // value present in process.env at the time of a call must win, even if
    // an earlier call had already copied that key from config.env. Without
    // this, a later session_start in the same Pi process would clobber an
    // override that another writer (extension, runtime assignment) put in
    // place between calls.
    writeConfig("SIGIL_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-file");

    process.env.SIGIL_AUTH_TOKEN = "from-shell";
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-shell");

    // A later edit to config.env must still not clobber the override.
    writeConfig("SIGIL_AUTH_TOKEN=from-file-2\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-shell");
  });

  it("does not delete a user-supplied value when the key is removed from config.env", () => {
    // After ownership has been released by a runtime override, removing
    // the key from config.env must leave the override in place —
    // otherwise the dotenv loader would silently delete data it no longer
    // owns.
    writeConfig("SIGIL_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();

    process.env.SIGIL_AUTH_TOKEN = "from-shell";
    writeConfig("");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-shell");
  });

  it("re-fills a key from config.env after the runtime override is cleared", () => {
    // If another writer first overrides our value and then clears it, the
    // key is no longer owned and the OS-env-wins check sees an empty
    // value, so the file value is allowed to take effect again on the
    // next call.
    writeConfig("SIGIL_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();

    process.env.SIGIL_AUTH_TOKEN = "from-shell";
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-shell");

    delete process.env.SIGIL_AUTH_TOKEN;
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-file");
  });

  it("keeps owned keys when config.env cannot be read", () => {
    const warn = loggerMock.warn;
    warn.mockClear();
    writeConfig("SIGIL_ENDPOINT=https://from-file\nSIGIL_AUTH_TOKEN=tok\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");

    rmSync(configPath());
    mkdirSync(configPath());
    applyAgento11yDotenv();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");
    warn.mockRestore();
  });

  it("clears a key when config.env disappears entirely", () => {
    writeConfig("SIGIL_AUTH_TOKEN=tok\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");

    rmSync(join(dir, "sigil"), { recursive: true, force: true });
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBeUndefined();
  });

  it("materializes a file value under both spellings", () => {
    writeConfig("SIGIL_AUTH_TOKEN=tok\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("tok");
  });

  it("file AGENTO11Y_ENDPOINT beats file SIGIL_ENDPOINT", () => {
    writeConfig(
      "AGENTO11Y_ENDPOINT=https://preferred\nSIGIL_ENDPOINT=https://legacy\n",
    );
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://preferred");
    expect(process.env.SIGIL_ENDPOINT).toBe("https://preferred");
  });

  it("shell SIGIL_ENDPOINT beats file AGENTO11Y_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "https://from-shell";
    writeConfig("AGENTO11Y_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-shell");
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://from-shell");
  });

  it("shell AGENTO11Y_ENDPOINT beats shell SIGIL_ENDPOINT", () => {
    process.env.AGENTO11Y_ENDPOINT = "https://preferred-shell";
    process.env.SIGIL_ENDPOINT = "https://legacy-shell";
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://preferred-shell");
    expect(process.env.SIGIL_ENDPOINT).toBe("https://preferred-shell");
  });

  it("clears both spellings when the file value is removed", () => {
    writeConfig("AGENTO11Y_AUTH_TOKEN=tok\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("tok");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");

    writeConfig("");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBeUndefined();
    expect(process.env.SIGIL_AUTH_TOKEN).toBeUndefined();
  });

  it("releases the whole family when a runtime writer replaces one spelling", () => {
    writeConfig("SIGIL_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("from-file");

    process.env.AGENTO11Y_AUTH_TOKEN = "from-writer";
    writeConfig("SIGIL_AUTH_TOKEN=from-file-2\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("from-writer");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-writer");
  });

  it("a runtime write to the legacy spelling wins over the file's preferred value", () => {
    writeConfig("AGENTO11Y_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-file");

    process.env.SIGIL_AUTH_TOKEN = "from-writer";
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-writer");
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("from-writer");
  });

  it("re-fills both spellings from the file after the runtime override is cleared", () => {
    writeConfig("SIGIL_AUTH_TOKEN=from-file\n");
    applyAgento11yDotenv();

    process.env.SIGIL_AUTH_TOKEN = "from-writer";
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-writer");

    delete process.env.SIGIL_AUTH_TOKEN;
    delete process.env.AGENTO11Y_AUTH_TOKEN;
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("from-file");
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("from-file");
  });
});
