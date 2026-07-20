import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

test('submitConversationRating sends HTTP request and maps response', async () => {
  let receivedPath = '';
  let receivedHeaders = {};
  let receivedBody = {};

  const server = createServer(async (request, response) => {
    receivedPath = request.url ?? '';
    receivedHeaders = Object.fromEntries(Object.entries(request.headers));
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    receivedBody = JSON.parse(Buffer.concat(chunks).toString('utf8'));

    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        rating: {
          rating_id: 'rat-1',
          conversation_id: 'conv-1',
          rating: 'CONVERSATION_RATING_VALUE_BAD',
          created_at: '2026-02-13T12:00:00Z',
        },
        summary: {
          total_count: 1,
          good_count: 0,
          bad_count: 1,
          latest_rating: 'CONVERSATION_RATING_VALUE_BAD',
          latest_rated_at: '2026-02-13T12:00:00Z',
          has_bad_rating: true,
        },
      }),
    );
  });
  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve rating test server address');
  }

  const client = newClient({
    generationEndpoint: 'localhost:4317',
    generationProtocol: 'grpc',
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    auth: {
      mode: 'tenant',
      tenantId: 'tenant-a',
    },
  });

  try {
    const result = await client.submitConversationRating('conv-1', {
      ratingId: 'rat-1',
      rating: 'CONVERSATION_RATING_VALUE_BAD',
      comment: 'wrong answer',
      metadata: { channel: 'assistant' },
    });

    assert.equal(receivedPath, '/api/v1/conversations/conv-1/ratings');
    assert.equal(receivedHeaders['x-scope-orgid'], 'tenant-a');
    assert.deepEqual(receivedBody, {
      rating_id: 'rat-1',
      rating: 'CONVERSATION_RATING_VALUE_BAD',
      comment: 'wrong answer',
      metadata: { channel: 'assistant' },
    });

    assert.equal(result.rating.ratingId, 'rat-1');
    assert.equal(result.rating.conversationId, 'conv-1');
    assert.equal(result.summary.hasBadRating, true);
    assert.equal(result.summary.badCount, 1);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('submitConversationRating routes under a path prefix when api.endpoint has one', async () => {
  let receivedPath = '';
  const server = createServer(async (request, response) => {
    receivedPath = request.url ?? '';
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        rating: {
          rating_id: 'rat-1',
          conversation_id: 'conv-1',
          rating: 'CONVERSATION_RATING_VALUE_GOOD',
          created_at: '2026-02-13T12:00:00Z',
        },
        summary: {
          total_count: 1,
          good_count: 1,
          bad_count: 0,
          latest_rating: 'CONVERSATION_RATING_VALUE_GOOD',
          latest_rated_at: '2026-02-13T12:00:00Z',
          has_bad_rating: false,
        },
      }),
    );
  });
  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve rating prefix server address');
  }

  const client = newClient({
    generationEndpoint: 'localhost:4317',
    generationProtocol: 'grpc',
    apiEndpoint: `http://127.0.0.1:${address.port}/sigil`,
    auth: { mode: 'tenant', tenantId: 'tenant-a' },
  });

  try {
    await client.submitConversationRating('conv-1', {
      ratingId: 'rat-1',
      rating: 'CONVERSATION_RATING_VALUE_GOOD',
    });
    assert.equal(receivedPath, '/sigil/api/v1/conversations/conv-1/ratings');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('submitConversationRating maps 409 to conflict error', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(409, { 'content-type': 'text/plain' });
    response.end('idempotency conflict');
  });
  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve rating conflict server address');
  }

  const client = newClient({
    generationEndpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
    apiEndpoint: `http://127.0.0.1:${address.port}`,
  });

  try {
    await assert.rejects(
      () =>
        client.submitConversationRating('conv-1', {
          ratingId: 'rat-1',
          rating: 'CONVERSATION_RATING_VALUE_GOOD',
        }),
      /conversation rating conflict/,
    );
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('submitConversationRating validates required input', async () => {
  const client = newClient({
    generationEndpoint: 'http://localhost:8080/api/v1/generations:export',
    apiEndpoint: 'http://localhost:8080',
  });
  try {
    await assert.rejects(
      () =>
        client.submitConversationRating('conv-1', {
          ratingId: '',
          rating: 'CONVERSATION_RATING_VALUE_GOOD',
        }),
      /validation failed: ratingId is required/,
    );
  } finally {
    await client.shutdown();
  }
});

test('submitConversationRating applies bearer auth header from config', async () => {
  let authorizationHeader = '';
  const server = createServer(async (request, response) => {
    authorizationHeader = request.headers.authorization ?? '';
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        rating: {
          rating_id: 'rat-1',
          conversation_id: 'conv-1',
          rating: 'CONVERSATION_RATING_VALUE_GOOD',
          created_at: '2026-02-13T12:00:00Z',
        },
        summary: {
          total_count: 1,
          good_count: 1,
          bad_count: 0,
          latest_rating: 'CONVERSATION_RATING_VALUE_GOOD',
          latest_rated_at: '2026-02-13T12:00:00Z',
          has_bad_rating: false,
        },
      }),
    );
  });
  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve bearer auth rating server address');
  }

  const client = newClient({
    generationEndpoint: `127.0.0.1:${address.port}/api/v1/generations:export`,
    apiEndpoint: `127.0.0.1:${address.port}`,
    auth: {
      mode: 'bearer',
      bearerToken: 'token-a',
    },
    insecure: true,
  });

  try {
    await client.submitConversationRating('conv-1', {
      ratingId: 'rat-1',
      rating: 'CONVERSATION_RATING_VALUE_GOOD',
    });
    assert.equal(authorizationHeader, 'Bearer token-a');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

function newClient(options) {
  const defaults = defaultConfig();
  return new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-rating-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: options.generationProtocol ?? 'http',
      endpoint: options.generationEndpoint,
      auth: options.auth ?? defaults.generationExport.auth,
      insecure: options.insecure ?? true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
    api: {
      endpoint: options.apiEndpoint ?? defaults.api.endpoint,
    },
    generationExporter: {
      async exportGenerations() {
        return { results: [] };
      },
    },
  });
}

async function listen(server) {
  await new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', (error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(undefined);
    });
  });
}

async function close(server) {
  await new Promise((resolve, reject) => {
    server.close((error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(undefined);
    });
  });
}
