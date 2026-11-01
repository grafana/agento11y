import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
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

import { createAgento11yClient, reportExportFailure } from "./client.js";

const WARNING =
  "Agent Observability: some telemetry could not be exported and may be lost.";
const PRIVATE_DETAIL =
  "https://private.example/evaluator response-body-private DATABASE_PASSWORD=secret";
const FAILURE_CATEGORIES = [
  "agento11y generation enqueue failed",
  "agento11y generation validation failed",
  "agento11y generation rejected",
  "agento11y generation export failed",
  "agento11y generation export flush on shutdown failed",
  "agento11y generation exporter shutdown failed",
];

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

  afterEach(() => {
    vi.restoreAllMocks();
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

  it("passes resolved auto-tags as client tags", () => {
    createAgento11yClient(
      makeConfig({
        autoTags: { user: "alice@example.com", repo: "grafana/agento11y" },
      }),
    );
    expect(Agento11yClientMock).toHaveBeenCalledWith(
      expect.objectContaining({
        tags: { user: "alice@example.com", repo: "grafana/agento11y" },
      }),
    );
  });

  it("leaves tags unset when no auto-tag resolved", () => {
    createAgento11yClient(makeConfig());
    const [arg] = Agento11yClientMock.mock.calls[0]!;
    expect("tags" in arg).toBe(false);
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

  describe.each([
    "debug",
    "warn",
    "error",
  ] as const)("SDK %s bridge", (level) => {
    it.each(
      FAILURE_CATEGORIES,
    )("reports %s and drops appended error text and arguments", (category) => {
      const sink = vi.fn();
      const client = createAgento11yClient(makeConfig(), {
        onExportFailure: sink,
      })!;
      const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
      logger[level](
        `${category}${category.endsWith("rejected") ? " id=private-id" : ""}: ${PRIVATE_DETAIL}`,
        new Error(PRIVATE_DETAIL),
        { evaluatorBody: PRIVATE_DETAIL },
      );
      reportExportFailure(client);
      logger[level](category, PRIVATE_DETAIL);

      expect(sink.mock.calls).toEqual([[WARNING]]);
      expect(loggerMock[level].mock.calls).toEqual([[category], [category]]);
    });

    it("replaces unknown SDK diagnostics without forwarding arguments", () => {
      const sink = vi.fn();
      createAgento11yClient(makeConfig(), { onExportFailure: sink });
      const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
      logger[level](PRIVATE_DETAIL, new Error(PRIVATE_DETAIL));
      logger[level](`agento11y generation export failedness ${PRIVATE_DETAIL}`);
      expect(loggerMock[level].mock.calls).toEqual([
        ["agento11y SDK diagnostic"],
        ["agento11y SDK diagnostic"],
      ]);
      expect(sink).not.toHaveBeenCalled();
    });
  });

  it.each([
    "metadata_only",
    "no_tool_content",
    "full",
    "full_with_metadata_spans",
  ] as const)("reports failure independently of %s capture", (contentCapture) => {
    const sink = vi.fn();
    const client = createAgento11yClient(makeConfig({ contentCapture }), {
      onExportFailure: sink,
    })!;
    reportExportFailure(client);
    const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
    for (const category of FAILURE_CATEGORIES) {
      logger.warn(`${category}: ${PRIVATE_DETAIL}`);
    }
    expect(sink.mock.calls).toEqual([[WARNING]]);
  });

  it.each([
    { name: "configured sink", sink: "ok", stderrThrows: false },
    { name: "absent sink", sink: "absent", stderrThrows: false },
    { name: "throwing sink", sink: "throws", stderrThrows: false },
    { name: "throwing sink and stderr", sink: "throws", stderrThrows: true },
    {
      name: "absent sink and throwing stderr",
      sink: "absent",
      stderrThrows: true,
    },
  ])("safely reports once with $name", ({ sink: kind, stderrThrows }) => {
    const stdout = vi.spyOn(process.stdout, "write").mockReturnValue(true);
    const stderr = vi.spyOn(process.stderr, "write").mockImplementation(() => {
      if (stderrThrows) throw new Error(PRIVATE_DETAIL);
      return true;
    });
    const sink = vi.fn(() => {
      if (kind === "throws") throw new Error(PRIVATE_DETAIL);
    });
    const client = createAgento11yClient(makeConfig(), {
      ...(kind !== "absent" ? { onExportFailure: sink } : {}),
    })!;
    expect(() => {
      reportExportFailure(client);
      const [{ logger }] = Agento11yClientMock.mock.calls[0]!;
      logger.warn(`agento11y generation export failed: ${PRIVATE_DETAIL}`);
      reportExportFailure(client);
    }).not.toThrow();
    expect(sink.mock.calls).toEqual(kind === "absent" ? [] : [[WARNING]]);
    expect(stderr.mock.calls).toEqual(kind === "ok" ? [] : [[`${WARNING}\n`]]);
    expect(stdout).not.toHaveBeenCalled();
  });

  it("deduplicates separately for each client", () => {
    const sink = vi.fn();
    const first = createAgento11yClient(makeConfig(), {
      onExportFailure: sink,
    })!;
    const second = createAgento11yClient(makeConfig(), {
      onExportFailure: sink,
    })!;
    reportExportFailure(first);
    reportExportFailure(first);
    reportExportFailure(second);
    expect(sink.mock.calls).toEqual([[WARNING], [WARNING]]);
  });

  it("deduplicates before calling a reentrant notification sink", () => {
    const sink = vi.fn(() => reportExportFailure(client));
    const client = createAgento11yClient(makeConfig(), {
      onExportFailure: sink,
    })!;
    reportExportFailure(client);
    expect(sink.mock.calls).toEqual([[WARNING]]);
  });

  it("returns null when sdk constructor throws", () => {
    // biome-ignore lint/complexity/useArrowFunction: must be constructable
    Agento11yClientMock.mockImplementationOnce(function () {
      throw new Error(PRIVATE_DETAIL);
    });

    const client = createAgento11yClient(makeConfig());
    expect(client).toBeNull();
    expect(loggerMock.error.mock.calls).toEqual([
      ["failed to create Agento11yClient"],
    ]);
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
