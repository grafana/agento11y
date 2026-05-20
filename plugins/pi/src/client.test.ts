import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SigilPiConfig } from "./config.js";

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
}));

import { createSigilClient } from "./client.js";

function makeConfig(overrides?: Partial<SigilPiConfig>): SigilPiConfig {
  return {
    endpoint: "http://localhost:8080",
    auth: { mode: "none" },
    agentName: "pi",
    contentCapture: "metadata_only",
    debug: false,
    redaction: {
      enabled: true,
      redactInputMessages: true,
      redactEmailAddresses: true,
    },
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
    // biome-ignore lint/complexity/useArrowFunction: must be a regular function for `new` to work
    SigilClientMock.mockImplementation(function () {
      return {};
    });
  });

  it("creates sdk client with tenant auth", () => {
    const client = createSigilClient(
      makeConfig({ auth: { mode: "tenant", tenantId: "t-1" } }),
    );

    expect(client).toEqual({});
    expect(SigilClientMock).toHaveBeenCalledTimes(1);
    expect(SigilClientMock).toHaveBeenCalledWith({
      generationExport: {
        protocol: "http",
        endpoint: "http://localhost:8080/api/v1/generations:export",
        auth: { mode: "tenant", tenantId: "t-1" },
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

  it("maps basic auth to sdk format with tenantId", () => {
    createSigilClient(
      makeConfig({
        auth: {
          mode: "basic",
          user: "12345",
          password: "pass",
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

  it("uses warn as the default sdk log level", () => {
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      createSigilClient(makeConfig());
      const [{ logger }] = SigilClientMock.mock.calls[0]!;
      logger.debug("debug");
      logger.warn("warn");
      logger.error("error");

      expect(debug).not.toHaveBeenCalled();
      expect(warn).toHaveBeenCalledWith("[sigil-pi] warn");
      expect(error).toHaveBeenCalledWith("[sigil-pi] error");
    } finally {
      debug.mockRestore();
      warn.mockRestore();
      error.mockRestore();
    }
  });

  it("downgrades best-effort export sdk logs to debug", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      createSigilClient(makeConfig());
      const [{ logger: defaultLogger }] = SigilClientMock.mock.calls[0]!;
      defaultLogger.warn("sigil generation export failed: transport down");
      defaultLogger.warn("sigil generation rejected id=g-1: invalid");

      expect(warn).not.toHaveBeenCalled();
      expect(error).not.toHaveBeenCalled();

      createSigilClient(makeConfig({ debug: true }));
      const [{ logger: debugLogger }] = SigilClientMock.mock.calls[1]!;
      debugLogger.warn("sigil generation export failed: transport down");

      expect(error).toHaveBeenCalledWith(
        "[sigil-pi] sigil generation export failed: transport down",
      );
    } finally {
      warn.mockRestore();
      error.mockRestore();
    }
  });

  it("returns null when sdk constructor throws", () => {
    SigilClientMock.mockImplementationOnce(() => {
      throw new Error("boom");
    });

    const client = createSigilClient(makeConfig());
    expect(client).toBeNull();
  });

  it("wires generationSanitizer when redaction is enabled", () => {
    createSigilClient(makeConfig());

    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledTimes(1);
    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledWith({
      redactInputMessages: true,
      redactEmailAddresses: true,
    });
    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ generationSanitizer: SANITIZER }),
    );
  });

  it("forwards redaction sub-flags to the sanitizer factory", () => {
    createSigilClient(
      makeConfig({
        redaction: {
          enabled: true,
          redactInputMessages: false,
          redactEmailAddresses: false,
        },
      }),
    );

    expect(createSecretRedactionSanitizerMock).toHaveBeenCalledWith({
      redactInputMessages: false,
      redactEmailAddresses: false,
    });
  });

  it("omits generationSanitizer when redaction is disabled", () => {
    createSigilClient(
      makeConfig({
        redaction: {
          enabled: false,
          redactInputMessages: true,
          redactEmailAddresses: true,
        },
      }),
    );

    expect(createSecretRedactionSanitizerMock).not.toHaveBeenCalled();
    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ generationSanitizer: undefined }),
    );
  });
});
