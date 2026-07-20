import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Agento11yOpencodeConfig } from "./config.js";

const { Agento11yClientMock } = vi.hoisted(() => ({
  Agento11yClientMock: vi.fn(),
}));

vi.mock("@grafana/agento11y", () => ({
  Agento11yClient: Agento11yClientMock,
  userAgent: () => "agento11y-sdk-js/0.0.0-test",
}));

import { createAgento11yClient } from "./client.js";

function makeConfig(
  overrides?: Partial<Agento11yOpencodeConfig>,
): Agento11yOpencodeConfig {
  return {
    endpoint: "http://localhost:8080",
    auth: { mode: "none" },
    agentName: "opencode",
    contentCapture: "metadata_only",
    debug: false,
    ...overrides,
  };
}

describe("createAgento11yClient", () => {
  beforeEach(() => {
    Agento11yClientMock.mockReset();
    // biome-ignore lint/complexity/useArrowFunction: must be a regular function for `new` to work
    Agento11yClientMock.mockImplementation(function () {
      return {};
    });
  });

  it("passes contentCapture to the SDK client", () => {
    createAgento11yClient(makeConfig({ contentCapture: "full" }));

    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({ contentCapture: "full" }),
    );
  });

  it("creates SDK client with export config", () => {
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
            /^agento11y-plugin-opencode\/.+ agento11y-sdk-js\/0\.0\.0-test$/,
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
    createAgento11yClient(makeConfig());

    const [arg] = Agento11yClientMock.mock.calls[0];
    const ua = arg.generationExport.headers["User-Agent"];
    expect(ua.startsWith("agento11y-plugin-opencode/")).toBe(true);
    expect(ua.endsWith("agento11y-sdk-js/0.0.0-test")).toBe(true);
  });

  it("passes guard config to the SDK client", () => {
    createAgento11yClient(
      makeConfig({
        guards: { enabled: true, timeoutMs: 2500, failOpen: false },
      }),
    );

    expect(Agento11yClientMock).toHaveBeenCalledWith(
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
    createAgento11yClient(
      makeConfig({ endpoint: "https://sigil.example.com/sigil" }),
    );

    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        generationExport: expect.objectContaining({
          endpoint: "https://sigil.example.com/sigil/api/v1/generations:export",
        }),
      }),
    );
  });

  it("returns null when SDK constructor throws", () => {
    Agento11yClientMock.mockImplementationOnce(() => {
      throw new Error("boom");
    });

    const client = createAgento11yClient(makeConfig());

    expect(client).toBeNull();
  });
});
