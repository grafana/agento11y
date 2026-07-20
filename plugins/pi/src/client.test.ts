import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Agento11yPiConfig } from "./config.js";

const { loggerMock } = vi.hoisted(() => ({
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

const { Agento11yClientMock, createSecretRedactionSanitizerMock, SANITIZER } =
  vi.hoisted(() => {
    const sanitizer = Object.assign(() => ({}) as never, {
      __sentinel: "sanitizer",
    });
    return {
      Agento11yClientMock: vi.fn(),
      createSecretRedactionSanitizerMock: vi.fn(() => sanitizer),
      SANITIZER: sanitizer,
    };
  });

vi.mock("@grafana/agento11y", () => ({
  Agento11yClient: Agento11yClientMock,
  createSecretRedactionSanitizer: createSecretRedactionSanitizerMock,
  userAgent: () => "agento11y-sdk-js/0.0.0-test",
}));

import { createAgento11yClient } from "./client.js";

function makeConfig(overrides?: Partial<Agento11yPiConfig>): Agento11yPiConfig {
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

describe("createAgento11yClient", () => {
  beforeEach(() => {
    Agento11yClientMock.mockReset();
    createSecretRedactionSanitizerMock.mockClear();
    loggerMock.debug.mockReset();
    loggerMock.warn.mockReset();
    loggerMock.error.mockReset();
    // biome-ignore lint/complexity/useArrowFunction: must be a regular function for `new` to work
    Agento11yClientMock.mockImplementation(function () {
      return {};
    });
  });

  it("creates sdk client with no auth", () => {
    const client = createAgento11yClient(makeConfig());

    expect(client).toEqual({});
    expect(Agento11yClientMock).toHaveBeenCalledTimes(1);
    expect(Agento11yClientMock).toHaveBeenCalledWith({
      generationExport: {
        protocol: "http",
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "none" },
        headers: {
          "User-Agent": expect.stringMatching(
            /^agento11y-plugin-pi\/.+ agento11y-sdk-js\/0\.0\.0-test$/,
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
    createAgento11yClient(makeConfig());

    const [arg] = Agento11yClientMock.mock.calls[0]!;
    const ua = arg.generationExport.headers["User-Agent"];
    expect(ua.startsWith("agento11y-plugin-pi/")).toBe(true);
    expect(ua.endsWith("agento11y-sdk-js/0.0.0-test")).toBe(true);
  });

  it("appends the export path for a prefix-mounted endpoint", () => {
    createAgento11yClient(
      makeConfig({ endpoint: "https://sigil.example.com/sigil" }),
    );
    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        generationExport: expect.objectContaining({
          endpoint: "https://sigil.example.com/sigil/api/v1/generations:export",
        }),
        api: { endpoint: "https://sigil.example.com/sigil" },
      }),
    );
  });

  it("passes basic auth through with tenantId", () => {
    createAgento11yClient(
      makeConfig({
        auth: {
          mode: "basic",
          basicUser: "12345",
          basicPassword: "pass",
          tenantId: "12345",
        },
      }),
    );

    expect(Agento11yClientMock).toHaveBeenCalledWith({
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
            /^agento11y-plugin-pi\/.+ agento11y-sdk-js\/0\.0\.0-test$/,
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
    createAgento11yClient(
      makeConfig({
        endpoint: "https://sigil.example.com",
        guards: { enabled: true, timeoutMs: 2500, failOpen: false },
      }),
    );

    expect(Agento11yClientMock).toHaveBeenCalledWith(
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

  it("passes contentCapture to Agento11yClient", () => {
    createAgento11yClient(makeConfig({ contentCapture: "full" }));
    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ contentCapture: "full" }),
    );
  });

  it("routes sdk logs through the file logger by level", () => {
    createAgento11yClient(makeConfig());
    const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
    logger.debug("debug");
    logger.warn("warn");
    logger.error("error");

    expect(loggerMock.debug).toHaveBeenCalledWith("debug");
    expect(loggerMock.warn).toHaveBeenCalledWith("warn");
    expect(loggerMock.error).toHaveBeenCalledWith("error");
  });

  it("downgrades best-effort export sdk logs to debug", () => {
    createAgento11yClient(makeConfig());
    const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
    logger.warn("agento11y generation export failed: transport down");
    logger.warn("agento11y generation rejected id=g-1: invalid");

    // Best-effort export failures are demoted to debug, never warn.
    expect(loggerMock.warn).not.toHaveBeenCalled();
    expect(loggerMock.debug).toHaveBeenCalledWith(
      "agento11y generation export failed: transport down",
    );
    expect(loggerMock.debug).toHaveBeenCalledWith(
      "agento11y generation rejected id=g-1: invalid",
    );
  });

  it("returns null when sdk constructor throws", () => {
    Agento11yClientMock.mockImplementationOnce(() => {
      throw new Error("boom");
    });

    const client = createAgento11yClient(makeConfig());
    expect(client).toBeNull();
  });

  it("wires input redaction into the sanitizer", () => {
    createAgento11yClient(makeConfig({ redactInputMessages: false }));

    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledTimes(1);
    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledWith({
      redactInputMessages: false,
    });
    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ generationSanitizer: SANITIZER }),
    );
  });
});
