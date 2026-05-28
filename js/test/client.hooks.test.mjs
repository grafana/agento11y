import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
import { defaultConfig, HookDeniedError, SigilClient } from '../.test-dist/index.js';

test('evaluateHook returns allow without contacting server when disabled', async () => {
  const client = newClient({
    apiEndpoint: 'http://127.0.0.1:1', // would refuse if it tried to connect
  });
  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.deepEqual(response.evaluations, []);
  } finally {
    await client.shutdown();
  }
});

test('evaluateHook posts JSON to /api/v1/hooks:evaluate and parses allow response', async () => {
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
        action: 'allow',
        evaluations: [
          {
            rule_id: 'pii-detect',
            evaluator_id: 'evaluator-pii',
            evaluator_kind: 'regex',
            passed: true,
            latency_ms: 12,
            explanation: 'no PII matches',
          },
        ],
      }),
    );
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: {
        agentName: 'agent-a',
        agentVersion: '1.0.0',
        model: { provider: 'openai', name: 'gpt-4o' },
        tags: { env: 'test' },
      },
      input: {
        systemPrompt: 'be helpful',
        messages: [
          {
            role: 'user',
            parts: [{ type: 'text', text: 'hello world' }],
          },
        ],
      },
    });

    assert.equal(receivedPath, '/api/v1/hooks:evaluate');
    assert.equal(receivedHeaders['content-type'], 'application/json');
    assert.equal(receivedHeaders['x-sigil-hook-timeout-ms'], '15000');
    assert.deepEqual(receivedBody, {
      phase: 'preflight',
      context: {
        agent_name: 'agent-a',
        agent_version: '1.0.0',
        model: { provider: 'openai', name: 'gpt-4o' },
        tags: { env: 'test' },
      },
      input: {
        system_prompt: 'be helpful',
        messages: [
          {
            role: 'user',
            parts: [{ kind: 'text', text: 'hello world' }],
          },
        ],
      },
    });

    assert.equal(response.action, 'allow');
    assert.equal(response.evaluations.length, 1);
    assert.equal(response.evaluations[0].ruleId, 'pii-detect');
    assert.equal(response.evaluations[0].evaluatorId, 'evaluator-pii');
    assert.equal(response.evaluations[0].evaluatorKind, 'regex');
    assert.equal(response.evaluations[0].passed, true);
    assert.equal(response.evaluations[0].latencyMs, 12);
    assert.equal(response.evaluations[0].explanation, 'no PII matches');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook routes hooks:evaluate under a path prefix when api.endpoint has one', async () => {
  let receivedPath = '';
  const server = createServer(async (request, response) => {
    receivedPath = request.url ?? '';
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'allow', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}/sigil`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(receivedPath, '/sigil/api/v1/hooks:evaluate');
    assert.equal(response.action, 'allow');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook handles api.endpoint with a trailing slash', async () => {
  let receivedPath = '';
  const server = createServer(async (request, response) => {
    receivedPath = request.url ?? '';
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'allow', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}/sigil/`,
    hooksEnabled: true,
  });

  try {
    await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(receivedPath, '/sigil/api/v1/hooks:evaluate');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook serializes tool calls with guard-compatible wire keys', async () => {
  let receivedBody = {};

  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    receivedBody = JSON.parse(Buffer.concat(chunks).toString('utf8'));

    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'deny', reason: 'blocked', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
    hooksPhases: ['postflight'],
  });

  try {
    const response = await client.evaluateHook({
      phase: 'postflight',
      context: {
        agentName: 'opencode',
        model: { provider: 'anthropic', name: 'claude' },
      },
      input: {
        output: [
          {
            role: 'assistant',
            parts: [
              {
                type: 'tool_call',
                toolCall: {
                  id: 'call-1',
                  name: 'third-party-test-mcp_third_party_test_mcp_leak_fake_credential',
                  inputJSON: JSON.stringify({ demo: true }),
                },
              },
            ],
          },
        ],
      },
    });

    assert.equal(response.action, 'deny');
    assert.deepEqual(receivedBody.input.output, [
      {
        role: 'assistant',
        parts: [
          {
            kind: 'tool_call',
            tool_call: {
              id: 'call-1',
              name: 'third-party-test-mcp_third_party_test_mcp_leak_fake_credential',
              input_json: { demo: true },
            },
          },
        ],
      },
    ]);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook parses transformed_input from allow response', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'allow',
        transformed_input: { conversation_preview: '[REDACTED]' },
        evaluations: [],
      }),
    );
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.equal(response.transformedInput?.conversationPreview, '[REDACTED]');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook normalizes Go/json-encoded proto transformed_input messages', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'allow',
        transformed_input: {
          messages: [
            {
              role: 1,
              parts: [{ Payload: { Text: 'hello' } }],
            },
          ],
        },
        evaluations: [],
      }),
    );
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    const msg = response.transformedInput?.messages?.[0];
    assert.equal(msg?.role, 'user');
    assert.equal(msg?.parts?.[0]?.type, 'text');
    assert.equal(msg?.parts?.[0]?.type === 'text' ? msg.parts[0].text : '', 'hello');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook returns deny payload as-is', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'deny',
        rule_id: 'rule-block-secrets',
        reason: 'matched secret pattern',
        evaluations: [
          {
            rule_id: 'rule-block-secrets',
            evaluator_id: 'evaluator-secret',
            evaluator_kind: 'regex',
            passed: false,
            latency_ms: 7,
            reason: 'secret detected',
          },
        ],
      }),
    );
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'deny');
    assert.equal(response.ruleId, 'rule-block-secrets');
    assert.equal(response.reason, 'matched secret pattern');
    assert.equal(response.evaluations.length, 1);
    assert.equal(response.evaluations[0].passed, false);
    assert.equal(response.evaluations[0].reason, 'secret detected');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook fails open by default on HTTP error', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(500, { 'content-type': 'text/plain' });
    response.end('boom');
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.deepEqual(response.evaluations, []);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook throws on HTTP error when failOpen is false', async () => {
  const server = createServer((_request, response) => {
    response.writeHead(500, { 'content-type': 'text/plain' });
    response.end('boom');
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
    hooksFailOpen: false,
  });

  try {
    await assert.rejects(
      () =>
        client.evaluateHook({
          phase: 'preflight',
          context: { model: { provider: 'openai', name: 'gpt-4o' } },
          input: {},
        }),
      /sigil hook evaluation failed/,
    );
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook returns allow without contacting server when phase not configured', async () => {
  let serverHit = false;
  const server = createServer((_request, response) => {
    serverHit = true;
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'deny', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
    hooksPhases: ['postflight'],
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.equal(serverHit, false);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('HookDeniedError exposes rule id and evaluations', () => {
  const error = new HookDeniedError('blocked by rule', 'rule-1', [
    {
      ruleId: 'rule-1',
      evaluatorId: 'eval-1',
      evaluatorKind: 'static',
      passed: false,
      latencyMs: 0,
    },
  ]);
  assert.equal(error.action, 'deny');
  assert.equal(error.ruleId, 'rule-1');
  assert.equal(error.evaluations.length, 1);
  assert.match(error.message, /rule-1/);
  assert.match(error.message, /blocked by rule/);
});

test('evaluateHook fails open on URL building error when failOpen is true', async () => {
  const client = newClient({
    apiEndpoint: '',
    hooksEnabled: true,
    hooksFailOpen: true,
  });
  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.deepEqual(response.evaluations, []);
  } finally {
    await client.shutdown();
  }
});

test('evaluateHook throws on URL building error when failOpen is false', async () => {
  const client = newClient({
    apiEndpoint: '',
    hooksEnabled: true,
    hooksFailOpen: false,
  });
  try {
    await assert.rejects(
      () =>
        client.evaluateHook({
          phase: 'preflight',
          context: { model: { provider: 'openai', name: 'gpt-4o' } },
          input: {},
        }),
      /api endpoint is required/,
    );
  } finally {
    await client.shutdown();
  }
});

test('evaluateHook with hooksConfigOverride enables hooks when client has them disabled', async () => {
  let serverHit = false;
  const server = createServer(async (request, response) => {
    serverHit = true;
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'allow', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: false,
  });

  try {
    const response = await client.evaluateHook(
      {
        phase: 'preflight',
        context: { model: { provider: 'openai', name: 'gpt-4o' } },
        input: {},
      },
      { enabled: true },
    );
    assert.equal(serverHit, true, 'hooksConfigOverride { enabled: true } should contact server');
    assert.equal(response.action, 'allow');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('empty phases array falls back to default preflight phase', async () => {
  let receivedBody;
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    receivedBody = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ action: 'allow', evaluations: [] }));
  });
  await listen(server);
  const address = server.address();

  const client = newClient({
    apiEndpoint: `http://127.0.0.1:${address.port}`,
    hooksEnabled: true,
    hooksPhases: [],
  });

  try {
    const response = await client.evaluateHook({
      phase: 'preflight',
      context: { model: { provider: 'openai', name: 'gpt-4o' } },
      input: {},
    });
    assert.equal(response.action, 'allow');
    assert.equal(receivedBody.phase, 'preflight', 'empty phases should fallback to default preflight');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

function newClient(options) {
  const defaults = defaultConfig();
  return new SigilClient({
    tracer: trace.getTracer('sigil-sdk-js-hooks-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: 'http://127.0.0.1:1/api/v1/generations:export',
      insecure: true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
    api: {
      endpoint: options.apiEndpoint ?? defaults.api.endpoint,
    },
    hooks: {
      enabled: options.hooksEnabled ?? false,
      phases: options.hooksPhases,
      failOpen: options.hooksFailOpen ?? true,
      timeoutMs: options.hooksTimeoutMs,
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
