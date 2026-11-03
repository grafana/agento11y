import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { homedirOverride } = vi.hoisted(() => ({
  homedirOverride: { value: undefined as string | undefined },
}));

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

  it("accepts AGENTO11Y_-prefixed keys", () => {
    const body = `AGENTO11Y_ENDPOINT=https://preferred
AGENTO11Y_AUTH_TOKEN=tok
`;
    expect(parseAgento11yDotenv(body)).toEqual({
      AGENTO11Y_ENDPOINT: "https://preferred",
      AGENTO11Y_AUTH_TOKEN: "tok",
    });
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
    fakeHome = mkdtempSync(join(tmpdir(), "sigil-opencode-home-"));
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

  it("falls back to $HOME/.config/agento11y/config.env when XDG_CONFIG_HOME is unset", () => {
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
    dir = mkdtempSync(join(tmpdir(), "sigil-opencode-dotenv-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    clearAgento11yEnv();
  });

  it("returns an empty map silently when the file is missing", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
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

  it("warns with the opencode prefix on non-ENOENT read failures", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const got = loadAgento11yDotenv(dir);
    expect(got).toEqual({});
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn.mock.calls[0]?.[0]).toMatch(/^\[sigil-opencode\]/);
    warn.mockRestore();
  });
});

describe("applyAgento11yDotenv", () => {
  let dir: string;

  beforeEach(() => {
    clearAgento11yEnv();
    vi.stubEnv("DO_NOT_TRACK", "");
    dir = mkdtempSync(join(tmpdir(), "sigil-opencode-dotenv-apply-"));
    process.env.XDG_CONFIG_HOME = dir;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    clearAgento11yEnv();
    vi.unstubAllEnvs();
  });

  function configPath(): string {
    return join(dir, "sigil", "config.env");
  }

  function writeConfig(body: string): void {
    const path = configPath();
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, body);
  }

  describe("ANONYMOUS_USAGE_STATS aliases", () => {
    const preferred = "AGENTO11Y_ANONYMOUS_USAGE_STATS";
    const legacy = "SIGIL_ANONYMOUS_USAGE_STATS";

    function expectBoth(value: string | undefined): void {
      expect(process.env[preferred]).toBe(value);
      expect(process.env[legacy]).toBe(value);
    }

    it.each([
      ["legacy file", undefined, undefined, undefined, "log", "log"],
      [
        "preferred file",
        undefined,
        undefined,
        "enabled",
        "disabled",
        "enabled",
      ],
      [
        "legacy shell over preferred file",
        undefined,
        "log",
        "disabled",
        "enabled",
        "log",
      ],
      [
        "preferred shell over legacy shell",
        "enabled",
        "disabled",
        "log",
        "disabled",
        "enabled",
      ],
      [
        "empty preferred shell",
        "",
        "disabled",
        "enabled",
        undefined,
        "disabled",
      ],
      [
        "blank preferred shell",
        "  ",
        "disabled",
        "enabled",
        undefined,
        "disabled",
      ],
      [
        "blank preferred file",
        undefined,
        undefined,
        "  ",
        "disabled",
        "disabled",
      ],
    ])("%s", (_name, shellPreferred, shellLegacy, filePreferred, fileLegacy, winner) => {
      if (shellPreferred !== undefined) process.env[preferred] = shellPreferred;
      if (shellLegacy !== undefined) process.env[legacy] = shellLegacy;
      writeConfig(
        `${preferred}="${filePreferred ?? ""}"\n${legacy}="${fileLegacy ?? ""}"\n`,
      );
      for (let call = 0; call < 2; call++) {
        applyAgento11yDotenv();
        expectBoth(winner);
      }
    });

    describe.each([preferred, legacy])("%s", (key) => {
      it.each([
        "1",
        " TrUe ",
      ])("shell enabled outranks DO_NOT_TRACK=%s and file disabled", (dnt) => {
        process.env[key] = "enabled";
        vi.stubEnv("DO_NOT_TRACK", dnt);
        writeConfig(`${preferred}=disabled\n${legacy}=disabled\n`);
        for (let call = 0; call < 2; call++) {
          applyAgento11yDotenv();
          expectBoth("enabled");
        }
      });

      it.each([
        " LoG ",
        " DiSaBlEd ",
        " invalid-mode ",
      ])("trims shell value %j without parsing its mode", (value) => {
        process.env[key] = value;
        vi.stubEnv("DO_NOT_TRACK", "1");
        writeConfig(`${preferred}=enabled\n${legacy}=enabled\n`);
        applyAgento11yDotenv();
        expectBoth(value.trim());
      });

      it.each([
        " DiSaBlEd ",
        " LoG ",
        " invalid-mode ",
      ])("trims file value %j without parsing its mode", (value) => {
        writeConfig(`${key}="${value}"\n`);
        for (let call = 0; call < 2; call++) {
          applyAgento11yDotenv();
          expectBoth(value.trim());
        }
      });

      it.each([
        ["1", "disabled"],
        [" TrUe ", "disabled"],
        ["0", "enabled"],
        ["false", "enabled"],
        ["yes", "enabled"],
        ["on", "enabled"],
        ["", "enabled"],
      ])("DO_NOT_TRACK=%j resolves file enabled to %s", (dnt, winner) => {
        vi.stubEnv("DO_NOT_TRACK", dnt);
        writeConfig(`${key}=enabled\n`);
        for (let call = 0; call < 2; call++) {
          applyAgento11yDotenv();
          expectBoth(winner);
        }
      });

      it("refreshes file edits across aliases and removes owned values", () => {
        writeConfig(`${key}=enabled\n`);
        applyAgento11yDotenv();
        expectBoth("enabled");

        writeConfig(`${key === preferred ? legacy : preferred}=disabled\n`);
        applyAgento11yDotenv();
        expectBoth("disabled");

        writeConfig("");
        applyAgento11yDotenv();
        expectBoth(undefined);
      });

      it("removes owned values when the file disappears", () => {
        writeConfig(`${key}=enabled\n`);
        applyAgento11yDotenv();
        expectBoth("enabled");

        rmSync(configPath());
        applyAgento11yDotenv();
        expectBoth(undefined);
      });

      it("re-resolves DO_NOT_TRACK activation and removal", () => {
        writeConfig(`${key}=enabled\n`);
        applyAgento11yDotenv();
        expectBoth("enabled");

        vi.stubEnv("DO_NOT_TRACK", "1");
        for (let call = 0; call < 2; call++) {
          applyAgento11yDotenv();
          expectBoth("disabled");
        }

        delete process.env.DO_NOT_TRACK;
        applyAgento11yDotenv();
        expectBoth("enabled");

        writeConfig("");
        applyAgento11yDotenv();
        expectBoth(undefined);
      });
    });

    it("synthesizes nothing without a preference or config file", () => {
      for (let call = 0; call < 2; call++) {
        applyAgento11yDotenv();
        expectBoth(undefined);
      }
    });

    it.each([
      ["changed-value runtime legacy override", undefined, "disabled"],
      ["original shell provenance", "enabled", "enabled"],
    ])("preserves %s after module replacement", async (_name, shell, winner) => {
      if (shell !== undefined) process.env[legacy] = shell;
      writeConfig(`${preferred}=enabled\n`);
      applyAgento11yDotenv();
      if (shell === undefined) process.env[legacy] = winner;

      vi.resetModules();
      const replacement = await import("./agento11yDotenv.js");
      expect(replacement.applyAgento11yDotenv).not.toBe(applyAgento11yDotenv);
      vi.stubEnv("DO_NOT_TRACK", "1");
      writeConfig(`${preferred}=disabled\n`);
      replacement.applyAgento11yDotenv();
      expectBoth(winner);

      vi.resetModules();
      const nextReplacement = await import("./agento11yDotenv.js");
      writeConfig("");
      nextReplacement.applyAgento11yDotenv();
      expectBoth(winner);
    });

    it.each([
      [undefined, undefined, "", "disabled"],
      [undefined, undefined, "1", "disabled"],
      [preferred, "enabled", "1", "enabled"],
      [legacy, "enabled", "1", "enabled"],
      [preferred, "log", "", "log"],
      [legacy, "invalid-mode", "", "invalid-mode"],
    ])("resolves failed reads with shell %s=%s and DNT=%j", (key, shell, dnt, winner) => {
      if (key !== undefined) process.env[key] = shell;
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
      writeConfig(
        `${preferred}=enabled\nSIGIL_AUTH_TOKEN=file-token\nOTEL_SERVICE_NAME=file-service\n`,
      );
      applyAgento11yDotenv();
      vi.stubEnv("DO_NOT_TRACK", dnt);
      process.env.SIGIL_ENDPOINT = "runtime-endpoint";

      rmSync(configPath());
      mkdirSync(configPath());
      vi.resetModules();
      return import("./agento11yDotenv.js").then((replacement) => {
        for (let call = 0; call < 2; call++) {
          replacement.applyAgento11yDotenv();
          expectBoth(winner);
          expect(process.env.SIGIL_AUTH_TOKEN).toBe("file-token");
          expect(process.env.OTEL_SERVICE_NAME).toBe("file-service");
          expect(process.env.SIGIL_ENDPOINT).toBe("runtime-endpoint");
        }

        rmSync(configPath(), { recursive: true });
        writeConfig(`${preferred}=enabled\n`);
        replacement.applyAgento11yDotenv();
        expectBoth(shell ?? (dnt === "1" ? "disabled" : "enabled"));
        expect(warn).toHaveBeenCalledTimes(2);
        warn.mockRestore();
      });
    });

    it.each([
      [undefined, "disabled"],
      [preferred, "enabled"],
      [legacy, "enabled"],
    ])("resolves an initially unreadable file with shell %s", (key, winner) => {
      if (key !== undefined) process.env[key] = "enabled";
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
      mkdirSync(configPath(), { recursive: true });
      applyAgento11yDotenv();
      expectBoth(winner);
      expect(warn).toHaveBeenCalledTimes(1);
      warn.mockRestore();
    });

    it("clears usage ownership through clearAgento11yEnv", () => {
      writeConfig(`${preferred}=enabled\n`);
      applyAgento11yDotenv();

      clearAgento11yEnv();
      process.env.XDG_CONFIG_HOME = dir;
      process.env[legacy] = "enabled";
      writeConfig(`${preferred}=disabled\n`);
      applyAgento11yDotenv();
      expectBoth("enabled");
    });
  });

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

  it("does nothing silently when config.env is missing", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
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

  it("materializes a file value under both spellings", () => {
    writeConfig("SIGIL_ENDPOINT=https://from-file\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_ENDPOINT).toBe("https://from-file");
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://from-file");
  });

  it("materializes export timeout under both spellings", () => {
    writeConfig("SIGIL_EXPORT_TIMEOUT_MS=2500\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_EXPORT_TIMEOUT_MS).toBe("2500");
    expect(process.env.AGENTO11Y_EXPORT_TIMEOUT_MS).toBe("2500");
  });

  it("shell SIGIL_ENDPOINT beats file AGENTO11Y_ENDPOINT", () => {
    process.env.SIGIL_ENDPOINT = "https://shell-legacy";
    writeConfig("AGENTO11Y_ENDPOINT=https://file-preferred\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://shell-legacy");
    expect(process.env.SIGIL_ENDPOINT).toBe("https://shell-legacy");
  });

  it("materializes both automatic-tag variables under both spellings", () => {
    // config.ts reads them as alias families, so both suffixes have to be in
    // ALIAS_SUFFIXES: without them a config.env `SIGIL_AUTO_CODING_AGENT_TAGS`
    // would never reach the preferred spelling.
    writeConfig(
      "SIGIL_AUTO_CODING_AGENT_TAGS=true\nSIGIL_AUTO_CODING_AGENT_TAGS_NAMES=user,repo\n",
    );
    applyAgento11yDotenv();
    expect(process.env.SIGIL_AUTO_CODING_AGENT_TAGS).toBe("true");
    expect(process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS).toBe("true");
    expect(process.env.SIGIL_AUTO_CODING_AGENT_TAGS_NAMES).toBe("user,repo");
    expect(process.env.AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES).toBe(
      "user,repo",
    );
  });

  it("materializes both local-mode variables under both spellings", () => {
    // config.ts reads LOCAL and local.ts reads BIN; neither reaches the SDK,
    // so nothing else in the suite would notice them missing from
    // ALIAS_SUFFIXES. Outside it they keep exact-key semantics, and a file
    // AGENTO11Y_LOCAL=true would beat a shell SIGIL_LOCAL=false instead of
    // losing to it.
    writeConfig("SIGIL_LOCAL=true\nSIGIL_BIN=/from/file/agento11y\n");
    applyAgento11yDotenv();
    expect(process.env.SIGIL_LOCAL).toBe("true");
    expect(process.env.AGENTO11Y_LOCAL).toBe("true");
    expect(process.env.SIGIL_BIN).toBe("/from/file/agento11y");
    expect(process.env.AGENTO11Y_BIN).toBe("/from/file/agento11y");
  });

  it("file AGENTO11Y_ENDPOINT beats file SIGIL_ENDPOINT", () => {
    writeConfig(
      "SIGIL_ENDPOINT=https://file-legacy\nAGENTO11Y_ENDPOINT=https://file-preferred\n",
    );
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://file-preferred");
    expect(process.env.SIGIL_ENDPOINT).toBe("https://file-preferred");
  });

  it("mirrors a shell-only legacy value to the preferred name", () => {
    process.env.SIGIL_AUTH_TOKEN = "tok";
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_AUTH_TOKEN).toBe("tok");
    expect(process.env.SIGIL_AUTH_TOKEN).toBe("tok");
  });

  it("treats whitespace shell values as unset during family resolution", () => {
    process.env.AGENTO11Y_ENDPOINT = "   ";
    writeConfig("SIGIL_ENDPOINT=https://file-legacy\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_ENDPOINT).toBe("https://file-legacy");
    expect(process.env.SIGIL_ENDPOINT).toBe("https://file-legacy");
  });

  it("selects one whole TAGS value instead of merging spellings", () => {
    writeConfig("AGENTO11Y_TAGS=a=1\nSIGIL_TAGS=b=2\n");
    applyAgento11yDotenv();
    expect(process.env.AGENTO11Y_TAGS).toBe("a=1");
    expect(process.env.SIGIL_TAGS).toBe("a=1");
  });
});
