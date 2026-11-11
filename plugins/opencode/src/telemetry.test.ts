import { createServer, type IncomingHttpHeaders } from "node:http";
import { describe, expect, it } from "vitest";
import { createTelemetryProviders } from "./telemetry.js";

const OTLP_AGGREGATION_TEMPORALITY_CUMULATIVE = 2;

interface CapturedRequest {
  url: string | undefined;
  headers: IncomingHttpHeaders;
  body: string;
}

async function startOtlpServer() {
  const requests: CapturedRequest[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      requests.push({
        url: req.url,
        headers: req.headers,
        body: Buffer.concat(chunks).toString("utf-8"),
      });
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    });
  });

  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", resolve);
  });

  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("server did not bind to a TCP port");
  }

  return {
    endpoint: `http://127.0.0.1:${address.port}/otlp`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}

async function startHttpProxy() {
  const requests: string[] = [];
  const server = createServer((req, res) => {
    requests.push(req.url ?? "");
    req.resume();
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });

  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", resolve);
  });

  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("proxy did not bind to a TCP port");
  }

  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}

const PROXY_ENV_KEYS = [
  "HTTP_PROXY",
  "HTTPS_PROXY",
  "NO_PROXY",
  "http_proxy",
  "https_proxy",
  "no_proxy",
  "NODE_USE_ENV_PROXY",
] as const;

function snapshotProxyEnv(): Record<string, string | undefined> {
  return Object.fromEntries(
    PROXY_ENV_KEYS.map((key) => [key, process.env[key]]),
  );
}

function restoreProxyEnv(snapshot: Record<string, string | undefined>): void {
  for (const key of PROXY_ENV_KEYS) {
    const value = snapshot[key];
    if (value === undefined) {
      delete process.env[key];
    } else {
      process.env[key] = value;
    }
  }
}

function parseMetricPayload(requests: CapturedRequest[], metricName: string) {
  for (const request of requests.filter((r) => r.url === "/otlp/v1/metrics")) {
    const payload = JSON.parse(request.body) as {
      resourceMetrics?: Array<{
        resource?: {
          attributes?: Array<{
            key: string;
            value: { stringValue?: string };
          }>;
        };
        scopeMetrics?: Array<{
          metrics?: Array<{
            name: string;
            histogram?: { aggregationTemporality?: number };
          }>;
        }>;
      }>;
    };

    for (const resourceMetric of payload.resourceMetrics ?? []) {
      for (const scopeMetric of resourceMetric.scopeMetrics ?? []) {
        const metric = scopeMetric.metrics?.find((m) => m.name === metricName);
        if (metric) return { request, resourceMetric, metric };
      }
    }
  }

  throw new Error(`metric ${metricName} was not exported`);
}

describe("createTelemetryProviders", () => {
  it("exports metrics with sigil-opencode resource and cumulative temporality", async () => {
    const server = await startOtlpServer();
    try {
      const providers = createTelemetryProviders(
        {
          endpoint: server.endpoint,
          headers: { "X-Test-Header": "present" },
        },
        "instance-abc",
      );

      providers.meter
        .createHistogram("sigil_opencode.test.duration", { unit: "s" })
        .record(0.5, { test: "true" });

      await providers.forceFlush();
      await providers.shutdown();

      const { request, resourceMetric, metric } = parseMetricPayload(
        server.requests,
        "sigil_opencode.test.duration",
      );

      expect(request.headers["x-test-header"]).toBe("present");

      const resourceAttributes = Object.fromEntries(
        (resourceMetric.resource?.attributes ?? []).map((attr) => [
          attr.key,
          attr.value.stringValue,
        ]),
      );
      expect(resourceAttributes["service.name"]).toBe("sigil-opencode");
      expect(resourceAttributes["service.instance.id"]).toBe("instance-abc");
      expect(resourceAttributes["telemetry.sdk.language"]).toBe("nodejs");

      expect(metric.histogram?.aggregationTemporality).toBe(
        OTLP_AGGREGATION_TEMPORALITY_CUMULATIVE,
      );
    } finally {
      await server.close();
    }
  });

  it("tags each invocation's metrics with a distinct service.instance.id", async () => {
    const server = await startOtlpServer();
    try {
      const a = createTelemetryProviders(
        { endpoint: server.endpoint, headers: {} },
        "instance-a",
      );
      const b = createTelemetryProviders(
        { endpoint: server.endpoint, headers: {} },
        "instance-b",
      );

      a.meter
        .createHistogram("sigil_opencode.test.duration", { unit: "s" })
        .record(0.5);
      b.meter
        .createHistogram("sigil_opencode.test.duration", { unit: "s" })
        .record(0.5);

      await a.forceFlush();
      await b.forceFlush();
      await a.shutdown();
      await b.shutdown();

      const instanceIds = new Set<string | undefined>();
      for (const request of server.requests.filter(
        (r) => r.url === "/otlp/v1/metrics",
      )) {
        const payload = JSON.parse(request.body) as {
          resourceMetrics?: Array<{
            resource?: {
              attributes?: Array<{
                key: string;
                value: { stringValue?: string };
              }>;
            };
          }>;
        };
        for (const rm of payload.resourceMetrics ?? []) {
          for (const attr of rm.resource?.attributes ?? []) {
            if (attr.key === "service.instance.id") {
              instanceIds.add(attr.value.stringValue);
            }
          }
        }
      }

      expect(instanceIds.has("instance-a")).toBe(true);
      expect(instanceIds.has("instance-b")).toBe(true);
    } finally {
      await server.close();
    }
  });

  it("sends metrics and traces through HTTP_PROXY", async () => {
    const proxy = await startHttpProxy();
    const saved = snapshotProxyEnv();
    process.env.HTTP_PROXY = proxy.url;
    process.env.http_proxy = proxy.url;
    delete process.env.HTTPS_PROXY;
    delete process.env.https_proxy;
    delete process.env.NO_PROXY;
    delete process.env.no_proxy;
    delete process.env.NODE_USE_ENV_PROXY;
    try {
      const providers = createTelemetryProviders(
        {
          endpoint: "http://otlp-proxy-test.invalid:4318/otlp",
          headers: {},
        },
        "instance-proxy",
      );

      providers.meter
        .createCounter("sigil_opencode.proxy.test")
        .add(1, { test: "true" });
      providers.tracer.startSpan("proxy test").end();

      await providers.forceFlush();
      await providers.shutdown();

      expect(proxy.requests.some((url) => url.includes("/v1/metrics"))).toBe(
        true,
      );
      expect(proxy.requests.some((url) => url.includes("/v1/traces"))).toBe(
        true,
      );
    } finally {
      restoreProxyEnv(saved);
      await proxy.close();
    }
  }, 20_000);

  it("bypasses the proxy for hosts matched by NO_PROXY", async () => {
    const server = await startOtlpServer();
    const proxy = await startHttpProxy();
    const saved = snapshotProxyEnv();
    process.env.HTTP_PROXY = proxy.url;
    process.env.http_proxy = proxy.url;
    delete process.env.HTTPS_PROXY;
    delete process.env.https_proxy;
    process.env.NO_PROXY = "127.0.0.1";
    process.env.no_proxy = "127.0.0.1";
    delete process.env.NODE_USE_ENV_PROXY;
    try {
      const providers = createTelemetryProviders(
        { endpoint: server.endpoint, headers: {} },
        "instance-no-proxy",
      );

      providers.meter
        .createCounter("sigil_opencode.proxy.test")
        .add(1, { test: "true" });
      providers.tracer.startSpan("proxy bypass test").end();

      await providers.forceFlush();
      await providers.shutdown();

      expect(proxy.requests).toEqual([]);
      expect(
        server.requests.some((request) => request.url === "/otlp/v1/metrics"),
      ).toBe(true);
      expect(
        server.requests.some((request) => request.url === "/otlp/v1/traces"),
      ).toBe(true);
    } finally {
      restoreProxyEnv(saved);
      await proxy.close();
      await server.close();
    }
  });
});
