import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { inspect } from 'node:util';
import { context, trace } from '@opentelemetry/api';
import { HookTransformError as CoreHookTransformError } from '../.test-dist/core.js';
import { evaluateHook } from '../.test-dist/hooks.js';
import {
  Agento11yClient,
  defaultConfig,
  HookDeniedError,
  HookTransformError,
  mergeConfig,
  withConversationId,
} from '../.test-dist/index.js';

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
    assert.equal(receivedHeaders['x-agento11y-hook-timeout-ms'], '15000');
    assert.equal(receivedHeaders['x-sigil-hook-timeout-ms'], undefined);
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
            // The server dispatches parts on a snake_case `kind`; the SDK's
            // `type` discriminator never reaches it. See
            // conformance/hooks/README.md.
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

test('evaluateHook adds conversation correlation and explicit trace identifiers', async () => {
  let receivedBody = {};
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
  });

  try {
    await withConversationId('conv-guarded', () =>
      client.evaluateHook({
        phase: 'preflight',
        context: {
          model: { provider: 'openai', name: 'gpt-4o' },
          traceId: '0123456789abcdef0123456789abcdef',
          spanId: '0123456789abcdef',
        },
        input: {},
      }),
    );
    assert.equal(receivedBody.context.conversation_id, 'conv-guarded');
    assert.equal(receivedBody.context.trace_id, '0123456789abcdef0123456789abcdef');
    assert.equal(receivedBody.context.span_id, '0123456789abcdef');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('evaluateHook omits invalid active trace identifiers', async () => {
  let receivedBody = {};
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
  });
  const invalidSpan = trace.wrapSpanContext({
    traceId: '00000000000000000000000000000000',
    spanId: '0000000000000000',
    traceFlags: 0,
  });

  try {
    await context.with(trace.setSpan(context.active(), invalidSpan), () =>
      client.evaluateHook({
        phase: 'preflight',
        context: { model: { provider: 'openai', name: 'gpt-4o' } },
        input: {},
      }),
    );
    assert.equal(receivedBody.context.trace_id, undefined);
    assert.equal(receivedBody.context.span_id, undefined);
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
      /agento11y hook evaluation failed/,
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

for (const strictTransforms of [undefined, false, true]) {
  test(`strictTransforms survives config merging: ${strictTransforms}`, () => {
    assert.equal(defaultConfig().hooks.strictTransforms, false);
    const merged = mergeConfig({ hooks: { strictTransforms } }, {});
    assert.equal(merged.hooks.strictTransforms, strictTransforms ?? false);
    assert.equal(mergeConfig(merged, {}).hooks.strictTransforms, strictTransforms ?? false);
  });
}

for (const configCase of [
  { name: 'default', strictTransforms: undefined, override: undefined, rejects: false },
  { name: 'explicit permissive', strictTransforms: false, override: undefined, rejects: false },
  { name: 'client strict', strictTransforms: true, override: undefined, rejects: true },
  { name: 'per-call strict', strictTransforms: false, override: { strictTransforms: true }, rejects: true },
  { name: 'per-call permissive', strictTransforms: true, override: { strictTransforms: false }, rejects: false },
  { name: 'unrelated override', strictTransforms: true, override: { timeoutMs: 5000 }, rejects: true },
]) {
  for (const failOpen of [true, false]) {
    test(`custom fetch HookTransformError: ${configCase.name}, failOpen=${failOpen}`, async () => {
      const error = new HookTransformError();
      const warnings = [];
      const pending = evaluateHook({
        apiEndpoint: 'http://127.0.0.1:1',
        insecure: true,
        hooks: {
          ...defaultConfig().hooks,
          enabled: true,
          failOpen,
          strictTransforms: configCase.strictTransforms,
          ...configCase.override,
        },
        request: {
          phase: 'preflight',
          context: { model: { provider: 'fixture', name: 'fixture' } },
          input: {},
        },
        fetchImpl: async () => {
          throw error;
        },
        logger: { warn: (...args) => warnings.push(args) },
      });
      if (configCase.rejects) {
        await assert.rejects(pending, (caught) => caught === error);
        assert.deepEqual(warnings, []);
      } else if (failOpen) {
        assert.deepEqual(await pending, { action: 'allow', evaluations: [] });
        assert.deepEqual(warnings, [
          [
            'agento11y: hook evaluation failed, allowing request (failOpen): agento11y hook evaluation failed: agento11y hook transform is invalid or empty',
          ],
        ]);
      } else {
        await assert.rejects(pending, (caught) => {
          assert.equal(caught.constructor, Error);
          assert.equal(caught.message, `agento11y hook evaluation failed: ${error.message}`);
          assert.equal(caught.cause, error);
          return true;
        });
        assert.deepEqual(warnings, []);
      }
    });

    test(`client transform validation: ${configCase.name}, failOpen=${failOpen}`, async () => {
      const source = 'fixture-private-source';
      const authorization = 'Bearer fixture-private-auth';
      const warnings = [];
      const server = createServer(async (request, response) => {
        for await (const _ of request) {
        }
        response.writeHead(200, { 'content-type': 'application/json' });
        response.end(
          JSON.stringify({
            action: 'allow',
            reason: source,
            transformed_input: { messages: [{ role: 'user', parts: [{ kind: 'text', text: { source } }] }] },
          }),
        );
      });
      await listen(server);
      const client = newClient({
        apiEndpoint: `http://127.0.0.1:${server.address().port}`,
        hooksEnabled: true,
        hooksFailOpen: failOpen,
        hooksStrictTransforms: configCase.strictTransforms,
        headers: { Authorization: authorization },
        logger: { warn: (...args) => warnings.push(args) },
      });
      const request = {
        phase: 'preflight',
        context: { model: { provider: 'fixture', name: 'fixture' } },
        input: { messages: [{ role: 'user', content: source }] },
      };
      try {
        const pending = client.evaluateHook(request, configCase.override);
        if (configCase.rejects) {
          await assert.rejects(pending, (error) => {
            assert.ok(error instanceof HookTransformError);
            assert.ok(error instanceof CoreHookTransformError);
            assert.equal(error.name, 'HookTransformError');
            assert.equal(error.message, 'agento11y hook transform is invalid or empty');
            assert.equal(error.cause, undefined);
            const diagnostics = [inspect(error), JSON.stringify(error), error.stack].join('\n');
            assert.ok(!diagnostics.includes(source));
            assert.ok(!diagnostics.includes(authorization));
            assert.ok(!diagnostics.includes('Authorization'));
            assert.ok(!diagnostics.includes('hook evaluation failed'));
            return true;
          });
        } else {
          const response = await pending;
          assert.equal(response.action, 'allow');
          assert.deepEqual(response.transformedInput.messages, [{ role: 'user' }]);
        }
        assert.deepEqual(warnings, []);
        assert.equal(client.config.hooks.strictTransforms, configCase.strictTransforms ?? false);
        if (configCase.override?.strictTransforms !== undefined) {
          const next = client.evaluateHook(request);
          if (configCase.strictTransforms) {
            await assert.rejects(next, HookTransformError);
          } else {
            assert.equal((await next).action, 'allow');
          }
        }
      } finally {
        await client.shutdown();
        await close(server);
      }
    });
  }
}

const originalArguments = '{"command":"ORIGINAL_SECRET"}';
const rewrittenArguments = '{"command":"REDACTED"}';
const malformedJSON = '{"secret":"ORIGINAL_SECRET';
const malformedBase64 = Buffer.from(malformedJSON).toString('base64');
const quotedMalformedJSON = JSON.stringify(malformedJSON);
const wireCall = {
  id: 'call-1',
  name: 'Bash',
  input_json: Buffer.from(originalArguments).toString('base64'),
};
const sdkCall = { id: 'call-1', name: 'Bash', inputJSON: rewrittenArguments };
const toolTransform = (part) => ({ output: [{ role: 'assistant', parts: [part] }] });

for (const testCase of [
  {
    name: 'conflicting tool-call aliases',
    transform: toolTransform({ kind: 'tool_call', tool_call: wireCall, toolCall: sdkCall }),
    lossy: true,
    permissiveArguments: originalArguments,
  },
  {
    name: 'conflicting argument aliases',
    transform: toolTransform({ tool_call: { ...wireCall, inputJSON: rewrittenArguments } }),
    lossy: true,
    permissiveArguments: originalArguments,
  },
  {
    name: 'conflicting system aliases',
    transform: { system_prompt: 'ORIGINAL_SECRET', systemPrompt: 'REDACTED' },
    lossy: true,
    permissiveSystem: 'ORIGINAL_SECRET',
  },
  {
    name: 'unknown nested call field',
    transform: toolTransform({ toolCall: { ...sdkCall, unknown: 'required rewrite' } }),
    lossy: true,
    permissiveArguments: rewrittenArguments,
  },
  {
    name: 'malformed argument JSON',
    transform: toolTransform({ toolCall: { name: 'Bash', inputJSON: malformedJSON } }),
    permissiveArguments: quotedMalformedJSON,
  },
  {
    name: 'malformed result JSON',
    transform: toolTransform({ toolResult: { contentJSON: malformedBase64 } }),
    permissiveResult: quotedMalformedJSON,
  },
  {
    name: 'malformed schema JSON',
    transform: { tools: [{ name: 'Bash', input_schema_json: malformedJSON }] },
    permissiveSchema: quotedMalformedJSON,
  },
  {
    name: 'equivalent malformed argument aliases',
    transform: toolTransform({
      tool_call: { name: 'Bash', input_json: malformedJSON, inputJson: malformedBase64 },
      toolCall: { name: 'Bash', inputJSON: malformedJSON },
    }),
    permissiveArguments: quotedMalformedJSON,
  },
  {
    name: 'equivalent malformed result aliases',
    transform: toolTransform({
      tool_result: { content_json: malformedJSON, contentJson: malformedBase64 },
      toolResult: { contentJSON: malformedJSON },
    }),
    permissiveResult: quotedMalformedJSON,
  },
  {
    name: 'equivalent malformed schema aliases',
    transform: { tools: [{ name: 'Bash', input_schema_json: malformedJSON, inputSchemaJSON: malformedBase64 }] },
    permissiveSchema: quotedMalformedJSON,
  },
  {
    name: 'conflicting malformed argument aliases',
    transform: toolTransform({ toolCall: { name: 'Bash', input_json: malformedJSON, inputJSON: rewrittenArguments } }),
    lossy: true,
    permissiveArguments: quotedMalformedJSON,
  },
  {
    name: 'conflicting malformed tool-call aliases',
    transform: toolTransform({ tool_call: { name: 'Bash', input_json: malformedBase64 }, toolCall: sdkCall }),
    lossy: true,
    permissiveArguments: quotedMalformedJSON,
  },
  {
    name: 'conflicting malformed result content aliases',
    transform: toolTransform({ toolResult: { content_json: malformedJSON, contentJSON: '{}' } }),
    lossy: true,
    permissiveResult: quotedMalformedJSON,
  },
  {
    name: 'conflicting malformed result aliases',
    transform: toolTransform({ tool_result: { content_json: malformedBase64 }, toolResult: { contentJSON: '{}' } }),
    lossy: true,
    permissiveResult: quotedMalformedJSON,
  },
  {
    name: 'conflicting malformed schema aliases',
    transform: { tools: [{ name: 'Bash', input_schema_json: malformedJSON, inputSchemaJSON: '{}' }] },
    lossy: true,
    permissiveSchema: quotedMalformedJSON,
  },
  {
    name: 'SDK rewrite',
    transform: toolTransform({ type: 'tool_call', toolCall: sdkCall }),
    permissiveArguments: rewrittenArguments,
  },
  {
    name: 'snake_case rewrite',
    transform: toolTransform({ kind: 'tool_call', tool_call: { ...wireCall, input_json: { command: 'REDACTED' } } }),
    permissiveArguments: rewrittenArguments,
  },
  {
    name: 'Go rewrite',
    transform: toolTransform({
      Payload: { ToolCall: { ...wireCall, input_json: Buffer.from(rewrittenArguments).toString('base64') } },
    }),
    permissiveArguments: rewrittenArguments,
  },
  {
    name: 'system rewrite',
    transform: { systemPrompt: 'REDACTED' },
    permissiveSystem: 'REDACTED',
  },
]) {
  for (const failOpen of [true, false]) {
    for (const strictTransforms of [undefined, false, true]) {
      test(`client real decoder: ${testCase.name}, strict=${strictTransforms}, failOpen=${failOpen}`, async () => {
        const warnings = [];
        const server = createServer(async (request, response) => {
          for await (const _ of request) {
          }
          response.writeHead(200, { 'content-type': 'application/json' });
          response.end(JSON.stringify({ action: 'allow', transformed_input: testCase.transform }));
        });
        await listen(server);
        const client = newClient({
          apiEndpoint: `http://127.0.0.1:${server.address().port}`,
          hooksEnabled: true,
          hooksFailOpen: failOpen,
          hooksStrictTransforms: strictTransforms,
          logger: { warn: (...args) => warnings.push(args) },
        });
        let consumed = 0;
        const evaluateAndConsume = async () => {
          const response = await client.evaluateHook({
            phase: 'preflight',
            context: { model: { provider: 'fixture', name: 'fixture' } },
            input: {
              output: [
                {
                  role: 'assistant',
                  parts: [{ type: 'tool_call', toolCall: { ...sdkCall, inputJSON: originalArguments } }],
                },
              ],
            },
          });
          consumed++;
          const input = response.transformedInput;
          assert.ok(input, 'transformed input was dropped');
          assert.equal(input.output?.[0].parts?.[0].toolCall?.inputJSON, testCase.permissiveArguments);
          assert.equal(input.output?.[0].parts?.[0].toolResult?.contentJSON, testCase.permissiveResult);
          assert.equal(input.tools?.[0].inputSchemaJSON, testCase.permissiveSchema);
          assert.equal(input.systemPrompt, testCase.permissiveSystem);
        };
        try {
          if (strictTransforms && testCase.lossy) {
            await assert.rejects(evaluateAndConsume(), (error) => {
              assert.ok(error instanceof HookTransformError);
              assert.ok(error instanceof CoreHookTransformError);
              assert.equal(error.message, 'agento11y hook transform is invalid or empty');
              assert.equal(error.cause, undefined);
              const diagnostics = [inspect(error), JSON.stringify(error), error.stack].join('\n');
              assert.ok(!diagnostics.includes('ORIGINAL_SECRET'));
              assert.ok(!diagnostics.includes('hook evaluation failed'));
              return true;
            });
            assert.equal(consumed, 0);
          } else {
            await evaluateAndConsume();
            assert.equal(consumed, 1);
          }
          assert.deepEqual(warnings, []);
        } finally {
          await client.shutdown();
          await close(server);
        }
      });
    }
  }
}

function newClient(options) {
  const defaults = defaultConfig();
  return new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-hooks-test'),
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
      headers: options.headers,
    },
    api: {
      endpoint: options.apiEndpoint ?? defaults.api.endpoint,
    },
    hooks: {
      enabled: options.hooksEnabled ?? false,
      phases: options.hooksPhases,
      failOpen: options.hooksFailOpen ?? true,
      timeoutMs: options.hooksTimeoutMs,
      strictTransforms: options.hooksStrictTransforms,
    },
    logger: options.logger,
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
