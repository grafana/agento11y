import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { trace } from '@opentelemetry/api';
import { defaultConfig, SDK_VERSION, SigilClient, userAgent } from '../.test-dist/index.js';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const protoPath = join(__dirname, '../proto/sigil/v1/generation_ingest.proto');
const protoLoadOptions = {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: false,
  oneofs: true,
};

test('userAgent() returns the SDK product token', () => {
  assert.equal(userAgent(), `sigil-sdk-js/${SDK_VERSION}`);
});

const defaultUserAgent = `sigil-sdk-js/${SDK_VERSION}`;
const overrideUserAgent = `sigil-plugin-opencode/1.2.3 ${defaultUserAgent}`;

// A non-blank caller User-Agent wins; a blank or whitespace-only one must not
// blank out the default. undefined exercises the no-header path. HTTP and gRPC
// must resolve identically.
const userAgentCases = [
  { name: 'no header', headers: undefined, wantUserAgent: defaultUserAgent },
  { name: 'override', headers: { 'user-agent': overrideUserAgent }, wantUserAgent: overrideUserAgent },
  { name: 'empty', headers: { 'user-agent': '' }, wantUserAgent: defaultUserAgent },
  { name: 'whitespace', headers: { 'user-agent': '   ' }, wantUserAgent: defaultUserAgent },
];

for (const { name, headers, wantUserAgent } of userAgentCases) {
  test(`HTTP export resolves the User-Agent (${name})`, async () => {
    const received = await captureHTTPUserAgent(headers);
    assert.equal(received, wantUserAgent);
  });

  test(`gRPC export resolves the User-Agent first token (${name})`, async () => {
    // grpc-js appends its own token after ours, so compare the first token.
    const received = await captureGRPCUserAgent(headers);
    assert.equal(firstToken(received), firstToken(wantUserAgent));
  });
}

async function captureHTTPUserAgent(headers) {
  let captured;
  const server = createServer(async (request, response) => {
    captured = request.headers['user-agent'];
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    const payload = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    response.writeHead(202, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        results: (payload.generations ?? []).map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      }),
    );
  });

  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  try {
    const client = newClient({
      protocol: 'http',
      endpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
      headers,
    });
    await runOneExport(client);
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
  return captured;
}

async function captureGRPCUserAgent(headers) {
  let captured;
  const grpcServer = await startGRPCServer((_request, metadata) => {
    captured = metadata['user-agent'];
  });
  try {
    const client = newClient({
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      headers,
    });
    await runOneExport(client);
  } finally {
    await stopGRPCServer(grpcServer.server);
  }
  return captured;
}

function newClient(generationExportOverrides) {
  const defaults = defaultConfig();
  return new SigilClient({
    tracer: trace.getTracer('sigil-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
      ...generationExportOverrides,
    },
  });
}

async function runOneExport(client) {
  const recorder = client.startGeneration({
    id: 'gen-ua',
    model: { provider: 'openai', name: 'gpt-5' },
  });
  recorder.setResult({
    input: [{ role: 'user', parts: [{ type: 'text', text: 'hi' }] }],
    output: [{ role: 'assistant', parts: [{ type: 'text', text: 'ok' }] }],
  });
  recorder.end();
  assert.equal(recorder.getError(), undefined);
  await client.shutdown();
}

function firstToken(userAgentValue) {
  const space = userAgentValue.indexOf(' ');
  return space >= 0 ? userAgentValue.slice(0, space) : userAgentValue;
}

async function startGRPCServer(onRequest) {
  const packageDefinition = await protoLoader.load(protoPath, protoLoadOptions);
  const loaded = grpc.loadPackageDefinition(packageDefinition);
  const service = loaded.sigil.v1.GenerationIngestService;

  const server = new grpc.Server();
  server.addService(service.service, {
    ExportGenerations(call, callback) {
      onRequest(call.request, call.metadata.getMap());
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

  return { server, port };
}

function stopGRPCServer(server) {
  return new Promise((resolve) => {
    server.tryShutdown(() => {
      resolve();
    });
  });
}
