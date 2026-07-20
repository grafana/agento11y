import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SigilOpencodeConfig } from "./config.js";

const { SigilClientMock } = vi.hoisted(() => ({
  SigilClientMock: vi.fn(),
}));

vi.mock("@grafana/agento11y", () => ({
  SigilClient: SigilClientMock,
  userAgent: () => "sigil-sdk-js/0.0.0-test",
}));

import { createSigilClient } from "./client.js";

function makeConfig(
  overrides?: Partial<SigilOpencodeConfig>,
): SigilOpencodeConfig {
  return {
    endpoint: "http://localhost:8080",
    auth: { mode: "none" },
    agentName: "opencode",
    contentCapture: "metadata_only",
    debug: false,
    ...overrides,
  };
}

describe("createSigilClient", () => {
  beforeEach(() => {
    SigilClientMock.mockReset();
    // biome-ignore lint/complexity/useArrowFunction: must be a regular function for `new` to work
    SigilClientMock.mockImplementation(function () {
      return {};
    });
  });

  it("passes contentCapture to the SDK client", () => {
    createSigilClient(makeConfig({ contentCapture: "full" }));

    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ contentCapture: "full" }),
    );
  });

  it("creates SDK client with export config", () => {
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
            /^sigil-plugin-opencode\/.+ sigil-sdk-js\/0\.0\.0-test$/,
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
    });
  });

  it("sets the plugin User-Agent on the generation export", () => {
    createSigilClient(makeConfig());

    const [arg] = SigilClientMock.mock.calls[0];
    const ua = arg.generationExport.headers["User-Agent"];
    expect(ua.startsWith("sigil-plugin-opencode/")).toBe(true);
    expect(ua.endsWith("sigil-sdk-js/0.0.0-test")).toBe(true);
  });

  it("passes guard config to the SDK client", () => {
    createSigilClient(
      makeConfig({
        guards: { enabled: true, timeoutMs: 2500, failOpen: false },
      }),
    );

    expect(SigilClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        hooks: {
          enabled: true,
          phases: ["postflight"],
          timeoutMs: 2500,
          failOpen: false,
        },
      }),
    );
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
      }),
    );
  });

  it("returns null when SDK constructor throws", () => {
    SigilClientMock.mockImplementationOnce(() => {
      throw new Error("boom");
    });

    const client = createSigilClient(makeConfig());

    expect(client).toBeNull();
  });
});
