// Real-gRPC content-capture test env.
//
// Spins up an in-process gRPC ingest server that records exported Generation
// payloads as they actually leave the SDK, plus an InMemorySpanExporter for
// OTel span assertions. Use this when a test needs to assert on both the proto
// export and the span path (the proto/span split that
// `full_with_metadata_spans` introduces).

import assert from 'node:assert/strict';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { SpanStatusCode } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const protoPath = join(__dirname, '../proto/agento11y/v1/generation_ingest.proto');
const protoLoadOptions = {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: false,
  oneofs: true,
};

// Modes that strip content from spans. Generations carry a proto/span split;
// tools and embeddings have no separate proto export so both modes behave the
// same for them on the span path.
export const STRIPPED_MODES = ['metadata_only', 'full_with_metadata_spans'];

// Coverage matrix across every on-the-wire mode. DEFAULT is intentionally
// absent — it's the resolver fall-through. Each entry encodes the contract:
// what stays in the proto, what marker is stamped, and what the OTel span
// sees.
export const MODE_MATRIX = [
  {
    mode: 'full',
    marker: 'full',
    protoContentStripped: false,
    spanTitlePresent: true,
    protoCallErrorRaw: true,
    spanRawError: true,
  },
  {
    // NO_TOOL_CONTENT is generation-content-full; only tool spans gate
    // arguments/results via legacy includeContent.
    mode: 'no_tool_content',
    marker: 'no_tool_content',
    protoContentStripped: false,
    spanTitlePresent: true,
    protoCallErrorRaw: true,
    spanRawError: true,
  },
  {
    mode: 'metadata_only',
    marker: 'metadata_only',
    protoContentStripped: true,
    spanTitlePresent: false,
    protoCallErrorRaw: false, // replaced with error category
    spanRawError: false,
  },
  {
    mode: 'full_with_metadata_spans',
    marker: 'full_with_metadata_spans',
    protoContentStripped: false, // proto path keeps full content
    spanTitlePresent: false, // but the span drops the title
    protoCallErrorRaw: true,
    spanRawError: false,
  },
];

// Sentinel substring guaranteed not to appear in any error category classifier
// output. If it leaks onto a span, the redaction is broken.
export const LEAK_MARKER = 'ignore previous instructions';

export function assertSpanErrorRedacted(span, expectedErrorType) {
  assert.equal(span.status.code, SpanStatusCode.ERROR);
  assert.equal(
    (span.status.message ?? '').includes(LEAK_MARKER),
    false,
    `span status leaks raw error: ${span.status.message}`,
  );
  for (const event of span.events ?? []) {
    for (const value of Object.values(event.attributes ?? {})) {
      assert.equal(
        String(value).includes(LEAK_MARKER),
        false,
        `span event ${event.name} attr leaks raw error: ${value}`,
      );
    }
  }
  assert.equal(span.attributes['error.type'], expectedErrorType);
}

export async function createContentCaptureEnv(options = {}) {
  const { contentCapture, contentCaptureResolver, embeddingCapture } = options;

  const receivedRequests = [];
  const grpcServer = await startGRPCServer((request) => {
    receivedRequests.push(request);
  });

  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: traceProvider.getTracer('agento11y-content-capture-test'),
    contentCapture,
    contentCaptureResolver,
    embeddingCapture,
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      batchSize: 1,
      queueSize: 10,
      flushIntervalMs: 60 * 60 * 1_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 2,
    },
  });

  let clientShutdown = false;
  let closed = false;

  async function flushClient() {
    if (clientShutdown) return;
    clientShutdown = true;
    await client.shutdown();
  }

  return {
    client,
    spanExporter,
    // Flushes the client and returns the only proto Generation the gRPC
    // server received. Safe to call alongside close(); the tracer provider
    // stays alive so span assertions can run afterwards.
    async singleGeneration() {
      await flushClient();
      assert.equal(receivedRequests.length, 1);
      assert.equal(receivedRequests[0].generations?.length, 1);
      return receivedRequests[0].generations[0];
    },
    spanByOperation(operationName) {
      const spans = spanExporter
        .getFinishedSpans()
        .filter((span) => span.attributes['gen_ai.operation.name'] === operationName);
      assert.ok(spans.length > 0, `no span for operation ${operationName}`);
      return spans.at(-1);
    },
    generationSpan() {
      return this.spanByOperation('generateText');
    },
    streamingGenerationSpan() {
      return this.spanByOperation('streamText');
    },
    toolSpan() {
      return this.spanByOperation('execute_tool');
    },
    embeddingSpan() {
      return this.spanByOperation('embeddings');
    },
    async close() {
      if (closed) return;
      closed = true;
      await flushClient();
      await traceProvider.shutdown();
      await stopGRPCServer(grpcServer.server);
    },
  };
}

async function startGRPCServer(onRequest) {
  const packageDefinition = await protoLoader.load(protoPath, protoLoadOptions);
  const loaded = grpc.loadPackageDefinition(packageDefinition);
  const service = loaded.agento11y.v1.GenerationIngestService;

  const server = new grpc.Server();
  server.addService(service.service, {
    ExportGenerations(call, callback) {
      onRequest(call.request);
      callback(null, {
        results: (call.request.generations ?? []).map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      });
    },
  });

  const port = await new Promise((resolve, reject) => {
    server.bindAsync('127.0.0.1:0', grpc.ServerCredentials.createInsecure(), (error, boundPort) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(boundPort);
    });
  });

  server.start();
  return { server, port };
}

function stopGRPCServer(server) {
  return new Promise((resolve) => {
    server.tryShutdown(() => resolve());
  });
}
