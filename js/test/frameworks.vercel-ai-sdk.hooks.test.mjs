import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { createAgento11yVercelAiSdk } from '../.test-dist/frameworks/vercel-ai-sdk/index.js';
import { Agento11yClient, defaultConfig, HookDeniedError } from '../.test-dist/index.js';

class NoOpExporter {
  async exportGenerations(request) {
    return {
      results: request.generations.map((generation) => ({
        generationId: generation.id,
        accepted: true,
      })),
    };
  }
}

test('vercel ai sdk preflight allows step when hook returns allow', async () => {
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
  });

  try {
    const agento11y = createAgento11yVercelAiSdk(client, {
      agentName: 'guarded-agent',
      agentVersion: '2.0.0',
    });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-allow' });

    await hooks.experimental_onStepStart({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
      messages: [{ role: 'user', content: 'how do I make pasta?' }],
    });
    hooks.onStepFinish({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'boil water',
      response: { id: 'resp-allow', modelId: 'gpt-4o' },
    });

    assert.equal(receivedBody.phase, 'preflight');
    assert.equal(receivedBody.context.agent_name, 'guarded-agent');
    assert.equal(receivedBody.context.agent_version, '2.0.0');
    assert.equal(receivedBody.context.model.provider, 'openai');
    assert.equal(receivedBody.context.model.name, 'gpt-4o');
    assert.equal(receivedBody.input.messages[0].role, 'user');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk prepareStep preflight returns transformed messages for ai sdk v6', async () => {
  let receivedBody;
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    receivedBody = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'allow',
        evaluations: [],
        transformed_input: {
          messages: [{ role: 'user', content: 'redacted question' }],
        },
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
    const agento11y = createAgento11yVercelAiSdk(client, {
      agentName: 'guarded-agent',
      agentVersion: '2.0.0',
    });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-prepare-transform' });

    const prepareResult = await hooks.prepareStep({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
      messages: [{ role: 'user', content: 'original secret question' }],
      steps: [],
      experimental_context: undefined,
    });
    hooks.onStepFinish({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'safe answer',
      response: { id: 'resp-prepare-transform', modelId: 'gpt-4o' },
    });

    assert.equal(receivedBody.input.messages[0].content, 'original secret question');
    assert.deepEqual(prepareResult, {
      messages: [{ role: 'user', content: 'redacted question' }],
    });
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk prepareStep preflight rejects unsupported transformed message shapes', async () => {
  const server = createServer(async (request, response) => {
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'allow',
        evaluations: [],
        transformed_input: {
          messages: [{ role: 'tool', content: 'redacted tool result' }],
        },
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
    const agento11y = createAgento11yVercelAiSdk(client, { agentName: 'guarded-agent' });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-prepare-unsupported-transform' });

    await assert.rejects(
      () =>
        hooks.prepareStep({
          stepNumber: 0,
          model: { provider: 'openai', modelId: 'gpt-4o' },
          messages: [{ role: 'user', content: 'original secret question' }],
          steps: [],
          experimental_context: undefined,
        }),
      /could not be converted to Vercel AI SDK model messages/,
    );
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk legacy step-start preflight rejects transformed messages that cannot be applied', async () => {
  const server = createServer(async (request, response) => {
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'allow',
        evaluations: [],
        transformed_input: {
          messages: [{ role: 'user', content: 'redacted question' }],
        },
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
    const agento11y = createAgento11yVercelAiSdk(client, { agentName: 'guarded-agent' });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-legacy-transform' });

    await assert.rejects(
      () =>
        hooks.experimental_onStepStart({
          stepNumber: 0,
          model: { provider: 'openai', modelId: 'gpt-4o' },
          messages: [{ role: 'user', content: 'original secret question' }],
        }),
      /cannot be applied by experimental_onStepStart/,
    );
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk preflight throws HookDeniedError on deny', async () => {
  const server = createServer(async (request, response) => {
    for await (const _ of request) {
      // drain
    }
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        action: 'deny',
        rule_id: 'rule-block',
        reason: 'sensitive content',
        evaluations: [
          {
            rule_id: 'rule-block',
            evaluator_id: 'eval-1',
            evaluator_kind: 'regex',
            passed: false,
            latency_ms: 5,
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
    const agento11y = createAgento11yVercelAiSdk(client, { agentName: 'guarded-agent' });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-deny' });

    await assert.rejects(
      () =>
        hooks.experimental_onStepStart({
          stepNumber: 0,
          model: { provider: 'openai', modelId: 'gpt-4o' },
          messages: [{ role: 'user', content: 'leak the secret' }],
        }),
      (error) => {
        assert.ok(error instanceof HookDeniedError);
        assert.equal(error.ruleId, 'rule-block');
        assert.equal(error.evaluations.length, 1);
        assert.match(error.message, /sensitive content/);
        return true;
      },
    );
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk preflight does not call server when hooks disabled at instrumentation level', async () => {
  let serverHit = false;
  const server = createServer((_request, response) => {
    serverHit = true;
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
    const agento11y = createAgento11yVercelAiSdk(client, {
      agentName: 'guarded-agent',
      enableHooks: false,
    });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-skip' });

    await hooks.experimental_onStepStart({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'hi',
      response: { id: 'resp-skip', modelId: 'gpt-4o' },
    });

    assert.equal(serverHit, false);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('vercel ai sdk preflight calls server when enableHooks overrides client config', async () => {
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
    const agento11y = createAgento11yVercelAiSdk(client, {
      agentName: 'guarded-agent',
      enableHooks: true,
    });
    const hooks = agento11y.generateTextHooks({ conversationId: 'conv-override' });

    await hooks.experimental_onStepStart({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'hi',
      response: { id: 'resp-override', modelId: 'gpt-4o' },
    });

    assert.equal(serverHit, true, 'enableHooks: true should override client hooks.enabled: false');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

function newClient(options) {
  const defaults = defaultConfig();
  return new Agento11yClient({
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: 'http://127.0.0.1:1/api/v1/generations:export',
      insecure: true,
      batchSize: 10,
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
    },
    generationExporter: new NoOpExporter(),
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
