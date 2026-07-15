import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { createSigilVercelAiSdk } from '../.test-dist/frameworks/vercel-ai-sdk/index.js';
import { defaultConfig, HookDeniedError, SigilClient } from '../.test-dist/index.js';

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
    const sigil = createSigilVercelAiSdk(client, {
      agentName: 'guarded-agent',
      agentVersion: '2.0.0',
    });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-allow' });

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

test('vercel ai sdk preflight retains prepareStep input when experimental step start omits messages', async () => {
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
    const sigil = createSigilVercelAiSdk(client, { agentName: 'guarded-agent' });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-prepare-step' });

    hooks.prepareStep({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
      messages: [{ role: 'user', content: 'keep this prompt' }],
    });
    await hooks.experimental_onStepStart({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-4o' },
    });

    assert.equal(receivedBody.input.messages[0].role, 'user');
    assert.equal(receivedBody.input.messages[0].content, 'keep this prompt');
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
    const sigil = createSigilVercelAiSdk(client, { agentName: 'guarded-agent' });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-deny' });

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
    const sigil = createSigilVercelAiSdk(client, {
      agentName: 'guarded-agent',
      enableHooks: false,
    });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-skip' });

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
    const sigil = createSigilVercelAiSdk(client, {
      agentName: 'guarded-agent',
      enableHooks: true,
    });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-override' });

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
  return new SigilClient({
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
