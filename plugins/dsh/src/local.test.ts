import { describe, expect, it, vi } from "vitest";

// Preserve the custom promisify hook used when local.ts imports execFile.
const { execFileCalls } = vi.hoisted(() => ({
  execFileCalls: [] as { file: string; args: string[] }[],
}));

vi.mock("node:child_process", async () => {
  const { promisify: p } = await import("node:util");
  const execFile = () => {
    throw new Error("callback form is not used");
  };
  Object.defineProperty(execFile, p.custom, {
    value: async (file: string, args: string[]) => {
      execFileCalls.push({ file, args });
      return { stdout: '{"running":false}\n', stderr: "" };
    },
  });
  return { execFile };
});

import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import {
  isLocalEndpoint,
  LocalReceiverError,
  resolveLocalReceiver,
} from "./local.js";

function statusJSON(endpoint: string): string {
  return `${JSON.stringify({
    running: true,
    pid: 4242,
    port: 8765,
    endpoint,
    started_at: "2024-05-05T10:00:00Z",
  })}\n`;
}

function enoent(bin: string): NodeJS.ErrnoException {
  const err = new Error(`spawn ${bin} ENOENT`) as NodeJS.ErrnoException;
  err.code = "ENOENT";
  return err;
}

describe("isLocalEndpoint", () => {
  it.each([
    ["http://127.0.0.1:8765", true],
    ["http://localhost:8765", true],
    ["http://[::1]:8765", true],
    ["http://127.0.0.1:8765/", true],
    ["https://127.0.0.1:8765", false],
    ["http://localhost.attacker.com/", false],
    ["http://10.0.0.1:8765", false],
    ["not a url", false],
    ["", false],
  ])("%s -> %s", (endpoint, want) => {
    expect(isLocalEndpoint(endpoint)).toBe(want);
  });
});

describe("resolveLocalReceiver", () => {
  it("reuses an injected loopback endpoint that answers, without looking up a binary", async () => {
    const runStatus = vi.fn();
    const probe = vi.fn(async () => true);
    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_ENDPOINT: "http://127.0.0.1:8768" },
      runStatus,
      probe,
      exists: () => true,
    });

    expect(receiver).toEqual({
      endpoint: "http://127.0.0.1:8768",
      otlpEndpoint: "http://127.0.0.1:8768/otlp",
    });
    expect(probe).toHaveBeenCalledWith("http://127.0.0.1:8768");
    expect(runStatus).not.toHaveBeenCalled();
  });

  it("normalizes an injected endpoint that carries the export path", async () => {
    const probe = vi.fn(async () => true);
    const receiver = await resolveLocalReceiver({
      env: {
        AGENTO11Y_ENDPOINT: "http://127.0.0.1:8765/api/v1/generations:export",
      },
      runStatus: vi.fn(),
      probe,
      exists: () => false,
    });

    expect(receiver).toEqual({
      endpoint: "http://127.0.0.1:8765",
      otlpEndpoint: "http://127.0.0.1:8765/otlp",
    });
    expect(probe).toHaveBeenCalledWith("http://127.0.0.1:8765");
  });

  it("asks the binary when nothing answers at the injected endpoint", async () => {
    const runStatus = vi.fn(async () => statusJSON("http://127.0.0.1:8790"));
    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_ENDPOINT: "http://127.0.0.1:8768" },
      runStatus,
      probe: async () => false,
      exists: () => false,
    });

    expect(receiver.endpoint).toBe("http://127.0.0.1:8790");
    expect(runStatus).toHaveBeenCalledWith("agento11y");
  });

  it("reports a stopped receiver when the injected endpoint is dead too", async () => {
    await expect(
      resolveLocalReceiver({
        env: { AGENTO11Y_ENDPOINT: "http://127.0.0.1:8768" },
        runStatus: async () => '{"running":false}\n',
        probe: async () => false,
        exists: () => false,
      }),
    ).rejects.toThrow(/no local receiver is running/);
  });

  it("ignores a configured Cloud endpoint and asks the binary", async () => {
    const runStatus = vi.fn(async () => statusJSON("http://127.0.0.1:8765"));
    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_ENDPOINT: "https://cloud.example.com" },
      runStatus,
      exists: () => false,
    });

    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
    expect(runStatus).toHaveBeenCalledWith("agento11y");
  });

  it("uses the dynamic port the receiver reports", async () => {
    const receiver = await resolveLocalReceiver({
      env: {},
      runStatus: async () => statusJSON("http://127.0.0.1:8797"),
      exists: () => false,
    });

    expect(receiver).toEqual({
      endpoint: "http://127.0.0.1:8797",
      otlpEndpoint: "http://127.0.0.1:8797/otlp",
    });
  });

  it("reads the endpoint from an older binary's prose output", async () => {
    const receiver = await resolveLocalReceiver({
      env: {},
      runStatus: async () =>
        "agento11y local receiver: running at http://127.0.0.1:8766 (pid 12, started 2024-05-05T10:00:00Z)\n",
      exists: () => false,
    });

    expect(receiver.endpoint).toBe("http://127.0.0.1:8766");
  });

  it.each([
    ["json", '{"running":false}\n'],
    ["prose", "agento11y local receiver: not running\n"],
  ])("reports a stopped receiver from %s output", async (_name, stdout) => {
    await expect(
      resolveLocalReceiver({
        env: {},
        runStatus: async () => stdout,
        exists: () => false,
      }),
    ).rejects.toThrow(/no local receiver is running/);
  });

  it("rejects a non-loopback endpoint", async () => {
    await expect(
      resolveLocalReceiver({
        env: {},
        runStatus: async () => statusJSON("http://sigil.example.com:8765"),
        exists: () => false,
      }),
    ).rejects.toThrow(/expected an HTTP URL with host/);
  });

  it("prefers AGENTO11Y_BIN over every other candidate", async () => {
    const tried: string[] = [];
    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_BIN: "/opt/checkout/agento11y", SIGIL_BIN: "/opt/old" },
      runStatus: async (bin) => {
        tried.push(bin);
        return statusJSON("http://127.0.0.1:8765");
      },
      exists: () => true,
    });

    expect(tried).toEqual(["/opt/checkout/agento11y"]);
    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
  });

  it("falls back to the well-known install paths before the legacy name", async () => {
    const tried: string[] = [];
    const receiver = await resolveLocalReceiver({
      env: { HOME: "/home/dev" },
      runStatus: async (bin) => {
        tried.push(bin);
        if (bin !== "/opt/homebrew/bin/agento11y") throw enoent(bin);
        return statusJSON("http://127.0.0.1:8765");
      },
      exists: (path) =>
        path === "/opt/homebrew/bin/agento11y" ||
        path === "/home/dev/go/bin/sigil",
    });

    expect(tried).toEqual(["agento11y", "/opt/homebrew/bin/agento11y"]);
    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
  });

  it("keeps looking after a candidate that runs and fails", async () => {
    const tried: string[] = [];
    const receiver = await resolveLocalReceiver({
      env: { HOME: "/home/dev" },
      runStatus: async (bin) => {
        tried.push(bin);
        if (bin !== "/home/dev/go/bin/agento11y") {
          throw new Error("Command failed: exit status 2");
        }
        return statusJSON("http://127.0.0.1:8765");
      },
      exists: (path) => path === "/home/dev/go/bin/agento11y",
    });

    expect(tried).toEqual(["agento11y", "/home/dev/go/bin/agento11y"]);
    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
  });

  it("reports every candidate it tried when none is installed", async () => {
    await expect(
      resolveLocalReceiver({
        env: { HOME: "/home/dev" },
        runStatus: async (bin) => {
          throw enoent(bin);
        },
        exists: () => false,
      }),
    ).rejects.toThrow(
      /no usable agento11y binary found \(tried agento11y, sigil\)/,
    );
  });

  it("names a candidate that ran and failed", async () => {
    await expect(
      resolveLocalReceiver({
        env: {},
        runStatus: async (bin) => {
          if (bin === "agento11y") throw new Error("Command failed: timeout");
          throw enoent(bin);
        },
        exists: () => false,
      }),
    ).rejects.toThrow(
      /no usable agento11y binary found \(tried agento11y \(Command failed: timeout\), sigil\)/,
    );
  });

  it("reports the binary AGENTO11Y_BIN names when it cannot be run", async () => {
    await expect(
      resolveLocalReceiver({
        env: { AGENTO11Y_BIN: "/opt/checkout/agento11y" },
        runStatus: async (bin) => {
          if (bin === "/opt/checkout/agento11y") {
            const err = new Error("spawn EACCES") as NodeJS.ErrnoException;
            err.code = "EACCES";
            throw err;
          }
          throw enoent(bin);
        },
        exists: () => false,
      }),
    ).rejects.toThrow(
      /cannot run the agento11y binary at \/opt\/checkout\/agento11y \(from AGENTO11Y_BIN\): spawn EACCES/,
    );
  });

  it("still resolves when AGENTO11Y_BIN is stale but a binary is on PATH", async () => {
    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_BIN: "/gone/agento11y" },
      runStatus: async (bin) => {
        if (bin === "/gone/agento11y") throw enoent(bin);
        return statusJSON("http://127.0.0.1:8765");
      },
      exists: () => false,
    });

    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
  });

  it.each([
    ["healthy", 200, true],
    ["unhealthy", 503, false],
  ])("probes /healthz on the injected endpoint and reuses a %s one", async (_name, status, reused) => {
    const requests: string[] = [];
    const server = createServer((req, res) => {
      requests.push(`${req.method} ${req.url}`);
      res.writeHead(status, { "content-type": "application/json" });
      res.end('{"status":"ok"}');
    });
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", () => resolve());
    });
    const { port } = server.address() as AddressInfo;
    try {
      const runStatus = vi.fn(async () => statusJSON("http://127.0.0.1:8765"));
      const receiver = await resolveLocalReceiver({
        env: { AGENTO11Y_ENDPOINT: `http://127.0.0.1:${port}` },
        runStatus,
        exists: () => false,
      });

      expect(requests).toEqual(["GET /healthz"]);
      expect(receiver.endpoint).toBe(
        reused ? `http://127.0.0.1:${port}` : "http://127.0.0.1:8765",
      );
      expect(runStatus.mock.calls.length).toBe(reused ? 0 : 1);
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });

  it("falls through to the binary when nothing listens on the injected endpoint", async () => {
    const server = createServer();
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", () => resolve());
    });
    const { port } = server.address() as AddressInfo;
    await new Promise((resolve) => server.close(resolve));

    const receiver = await resolveLocalReceiver({
      env: { AGENTO11Y_ENDPOINT: `http://127.0.0.1:${port}` },
      runStatus: async () => statusJSON("http://127.0.0.1:8765"),
      exists: () => false,
    });

    expect(receiver.endpoint).toBe("http://127.0.0.1:8765");
  });

  it("asks the binary for status and never for a start", async () => {
    // A plain dsh process must not start a daemon it cannot stop.
    execFileCalls.length = 0;
    await expect(
      resolveLocalReceiver({ env: {}, exists: () => false }),
    ).rejects.toThrow(/no local receiver is running/);

    expect(execFileCalls).toEqual([
      { file: "agento11y", args: ["local", "status", "--json"] },
    ]);
  });

  it.each([
    ["prints nothing", "   \n", /printed nothing/],
    ["prints unreadable json", "{oops\n", /cannot parse/],
    ["claims to run with no endpoint", '{"running":true}\n', /no endpoint/],
    [
      "prints prose with no url",
      "something unexpected\n",
      /cannot read a receiver endpoint/,
    ],
  ])("rejects a binary that %s", async (_name, stdout, want) => {
    const call = () =>
      resolveLocalReceiver({
        env: {},
        runStatus: async () => stdout,
        exists: () => false,
      });

    await expect(call()).rejects.toBeInstanceOf(LocalReceiverError);
    await expect(call()).rejects.toThrow(want);
  });
});
