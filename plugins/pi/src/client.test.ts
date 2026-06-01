import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SigilPiConfig } from "./config.js";

const { loggerMock } = vi.hoisted(() => ({
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

const { SigilClientMock, createSecretRedactionSanitizerMock, SANITIZER } =
  vi.hoisted(() => {
    const sanitizer = Object.assign(() => ({}) as never, {
      __sentinel: "sanitizer",
    });
    return {
      SigilClientMock: vi.fn(),
      createSecretRedactionSanitizerMock: vi.fn(() => sanitizer),
      SANITIZER: sanitizer,
    };
  });

vi.mock("@grafana/sigil-sdk-js", () => ({
  SigilClient: SigilClientMock,
  createSecretRedactionSanitizer: createSecretRedactionSanitizerMock,
  userAgent: () => "sigil-sdk-js/0.0.0-test",
}));

import { createSigilClient } from "./client.js";

function makeConfig(overrides?: Partial<SigilPiConfig>): SigilPiConfig {
  return {
    endpoint: "http://localhost:8080",
    auth: { mode: "none" },
    agentName: "pi",
    contentCapture: "metadata_only",
    redactInputMessages: true,
    guards: {
      enabled: false,
      timeoutMs: 1500,
      failOpen: true,
    },
    ...overrides,
  };
}

describe("createSigilClient", () => {
  beforeEach(() => {
    SigilClientMock.mockReset();
    createSecretRedactionSanitizerMock.mockClear();
    loggerMock.debug.mockReset();
    loggerMock.warn.mockReset();
    loggerMock.error.mockReset();
    // biome-ignore lint/complexity/useArrowFunction: must be a regular function for `new` to work
    SigilClientMock.mockImplementation(function () {
      return {};
    });
  });

  it("creates sdk client with no auth", () => {
    const client = createSigilClient(makeConfig());

    expect(client).toEqual({});
    expect(SigilClientMock).toHaveBeenCalledTimes(1);
    expect(SigilClientMock).toHaveBeenCalledWith({
      generationExport: {
        protocol: "http",
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        headers: {
          "User-Agent": expect.stringMatching(
            /^sigil-plugin-pi\/.+ sigil-sdk-js\/0\.0\.0-test$/,
          ),
        },
      },
      api: { endpoint: "http://localhost:8080" },
      hooks: {
        enabled: false,
        phases: ["postflight"],
        timeoutMs: 1500,
        failOpen: true,
      },
      contentCapture: "metadata_only",
      logger: expect.any(Object),
      generationSanitizer: SANITIZER,
    });
  });

  it("sets the plugin User-Agent on the generation export", () => {
    createSigilClient(makeConfig());

    const [arg] = SigilClientMock.mock.calls[0]!;
    const ua = arg.generationExport.headers["User-Agent"];
    expect(ua.startsWith("sigil-plugin-pi/")).toBe(true);
    expect(ua.endsWith("sigil-sdk-js/0.0.0-test")).toBe(true);
  });

  it("appends the export path for a prefix-mounted endpoint", () => {
    createSigilClient(
      makeConfig({ endpoint: "https://sigil.example.com/sigil" }),
    );
    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        generationExport: expect.objectContaining({
          endpoint: "https://sigil.example.com/sigil/api/v1/generations:export",
        }),
        api: { endpoint: "https://sigil.example.com/sigil" },
      }),
    );
  });

  it("passes basic auth through with tenantId", () => {
    createSigilClient(
      makeConfig({
        auth: {
          mode: "basic",
          basicUser: "12345",
          basicPassword: "pass",
          tenantId: "12345",
        },
      }),
    );

    expect(SigilClientMock).toHaveBeenCalledWith({
      generationExport: {
        protocol: "http",
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: {
          mode: "basic",
          basicUser: "12345",
          basicPassword: "pass",
          tenantId: "12345",
        },
        headers: {
          "User-Agent": expect.stringMatching(
            /^sigil-plugin-pi\/.+ sigil-sdk-js\/0\.0\.0-test$/,
          ),
        },
      },
      api: { endpoint: "http://localhost:8080" },
      hooks: {
        enabled: false,
        phases: ["postflight"],
        timeoutMs: 1500,
        failOpen: true,
      },
      contentCapture: "metadata_only",
      logger: expect.any(Object),
      generationSanitizer: SANITIZER,
    });
  });

  it("forwards guards config to the SDK hooks block", () => {
    createSigilClient(
      makeConfig({
        endpoint: "https://sigil.example.com",
        guards: { enabled: true, timeoutMs: 2500, failOpen: false },
      }),
    );

    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        api: { endpoint: "https://sigil.example.com" },
        generationExport: expect.objectContaining({
          endpoint: "https://sigil.example.com/api/v1/generations:export",
        }),
        hooks: {
          enabled: true,
          phases: ["postflight"],
          timeoutMs: 2500,
          failOpen: false,
        },
      }),
    );
  });

  it("passes contentCapture to SigilClient", () => {
    createSigilClient(makeConfig({ contentCapture: "full" }));
    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ contentCapture: "full" }),
    );
  });

  it("routes sdk logs through the file logger by level", () => {
    createSigilClient(makeConfig());
    const [{ logger }] = SigilClientMock.mock.calls[0]!;
    logger.debug("debug");
    logger.warn("warn");
    logger.error("error");

    expect(loggerMock.debug).toHaveBeenCalledWith("debug");
    expect(loggerMock.warn).toHaveBeenCalledWith("warn");
    expect(loggerMock.error).toHaveBeenCalledWith("error");
  });

  it("downgrades best-effort export sdk logs to debug", () => {
    createSigilClient(makeConfig());
    const [{ logger }] = SigilClientMock.mock.calls[0]!;
    logger.warn("sigil generation export failed: transport down");
    logger.warn("sigil generation rejected id=g-1: invalid");

    // Best-effort export failures are demoted to debug, never warn.
    expect(loggerMock.warn).not.toHaveBeenCalled();
    expect(loggerMock.debug).toHaveBeenCalledWith(
      "sigil generation export failed: transport down",
    );
    expect(loggerMock.debug).toHaveBeenCalledWith(
      "sigil generation rejected id=g-1: invalid",
    );
  });

  it("returns null when sdk constructor throws", () => {
    SigilClientMock.mockImplementationOnce(() => {
      throw new Error("boom");
    });

    const client = createSigilClient(makeConfig());
    expect(client).toBeNull();
  });

  it("wires input redaction into the sanitizer", () => {
    createSigilClient(makeConfig({ redactInputMessages: false }));

    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledTimes(1);
    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledWith({
      redactInputMessages: false,
    });
    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ generationSanitizer: SANITIZER }),
    );
  });
});
