import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
import { defaultConfig, SigilClient } from '../.test-dist/index.js';

class MockGenerationExporter {
  requests = [];
  workflowStepRequests = [];
  attempts = 0;
  workflowStepAttempts = 0;
  shutdownCalls = 0;

  constructor(failuresBeforeSuccess = 0) {
    this.failuresBeforeSuccess = failuresBeforeSuccess;
  }

  async exportGenerations(request) {
    this.attempts++;
    this.requests.push(structuredClone(request));

    if (this.failuresBeforeSuccess > 0) {
      this.failuresBeforeSuccess--;
      throw new Error('forced export failure');
    }

    return {
      results: request.generations.map((generation) => ({
        generationId: generation.id,
        accepted: true,
      })),
    };
  }

  async exportWorkflowSteps(request) {
    this.workflowStepAttempts++;
    this.workflowStepRequests.push(structuredClone(request));

    if (this.workflowStepFailuresBeforeSuccess > 0) {
      this.workflowStepFailuresBeforeSuccess--;
      throw new Error('forced workflow-step export failure');
    }

    return {
      results: request.workflowSteps.map((step) => ({
        stepId: step.id,
        accepted: true,
      })),
    };
  }

  async shutdown() {
    this.shutdownCalls++;
  }
}

test('flushes generation exports by batch size', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 2,
    flushIntervalMs: 60_000,
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(1)), 1);
    endWithSuccess(client.startGeneration(seedGeneration(2)), 2);

    await waitFor(() => exporter.requests.length === 1);
    assert.equal(exporter.requests[0].generations.length, 2);
  } finally {
    await client.shutdown();
  }
});

test('flushes generation exports by interval', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 20,
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(3)), 3);

    await waitFor(() => exporter.requests.length === 1);
    assert.equal(exporter.requests[0].generations.length, 1);
  } finally {
    await client.shutdown();
  }
});

test('flush retries failed exports with backoff and succeeds', async () => {
  const exporter = new MockGenerationExporter(2);
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
    maxRetries: 2,
    initialBackoffMs: 1,
    maxBackoffMs: 1,
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(4)), 4);

    await client.flush();
    assert.equal(exporter.attempts, 3);
    assert.equal(exporter.requests.length, 3);
  } finally {
    await client.shutdown();
  }
});

test('flushes workflow step exports and records debug snapshots', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
  });

  try {
    client.enqueueWorkflowStep({
      id: 'wfs-route',
      conversationId: 'conv-workflow',
      stepName: 'route',
      framework: 'custom',
      startedAt: new Date(Date.UTC(2026, 1, 12, 10, 0, 0)),
      completedAt: new Date(Date.UTC(2026, 1, 12, 10, 0, 1)),
      linkedGenerationIds: ['gen-route'],
    });

    assert.equal(client.debugSnapshot().workflowSteps.length, 1);
    assert.equal(client.debugSnapshot().workflowStepQueueSize, 1);

    await client.flush();

    assert.equal(exporter.workflowStepRequests.length, 1);
    assert.equal(exporter.workflowStepRequests[0].workflowSteps[0].id, 'wfs-route');
    assert.equal(client.debugSnapshot().workflowStepQueueSize, 0);
  } finally {
    await client.shutdown();
  }
});

test('workflow step enqueue defaults timestamps with client clock and merges tags', async () => {
  const exporter = new MockGenerationExporter();
  const now = new Date(Date.UTC(2026, 1, 12, 10, 0, 0));
  const client = newClient(
    exporter,
    {
      batchSize: 10,
      flushIntervalMs: 60_000,
    },
    {
      now: () => new Date(now),
      tags: {
        client: 'tag',
        shared: 'client',
      },
    },
  );

  try {
    client.enqueueWorkflowStep({
      id: 'wfs-defaults',
      conversationId: 'conv-workflow',
      stepName: 'route',
      tags: {
        shared: 'step',
      },
    });

    const snapshotStep = client.debugSnapshot().workflowSteps[0];
    assert.equal(snapshotStep.startedAt.toISOString(), now.toISOString());
    assert.equal(snapshotStep.completedAt.toISOString(), now.toISOString());
    assert.deepEqual(snapshotStep.tags, {
      client: 'tag',
      shared: 'step',
    });

    await client.flush();

    const exportedStep = exporter.workflowStepRequests[0].workflowSteps[0];
    assert.equal(exportedStep.startedAt.toISOString(), now.toISOString());
    assert.equal(exportedStep.completedAt.toISOString(), now.toISOString());
    assert.deepEqual(exportedStep.tags, {
      client: 'tag',
      shared: 'step',
    });
  } finally {
    await client.shutdown();
  }
});

test('workflow step enqueue validates raw input before defaulting timestamps', async () => {
  const exporter = new MockGenerationExporter();
  const now = new Date(Date.UTC(2026, 1, 12, 10, 0, 0));
  const completedAt = new Date(Date.UTC(2020, 0, 1, 0, 0, 0));
  const client = newClient(exporter, { batchSize: 10, flushIntervalMs: 60_000 }, { now: () => new Date(now) });

  try {
    // Only completedAt is supplied. The completed < started rule must not fire
    // (started is unset), and defaulting started to now() must not retroactively
    // reject it. Matches Go and Python.
    client.enqueueWorkflowStep({
      id: 'wfs-completed-only',
      conversationId: 'conv-workflow',
      stepName: 'route',
      completedAt,
    });

    await client.flush();

    const exportedStep = exporter.workflowStepRequests[0].workflowSteps[0];
    assert.equal(exportedStep.startedAt.toISOString(), now.toISOString());
    assert.equal(exportedStep.completedAt.toISOString(), completedAt.toISOString());
  } finally {
    await client.shutdown();
  }
});

test('workflow step enqueue rejection does not record the step in the debug snapshot', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, { batchSize: 10, flushIntervalMs: 60_000, queueSize: 1 });

  try {
    client.enqueueWorkflowStep({ id: 'wfs-1', conversationId: 'c1', stepName: 'route' });
    // Queue size is 1 and the worker has not flushed, so the second enqueue
    // is rejected. The rejected step must not appear in the debug snapshot.
    assert.throws(
      () => client.enqueueWorkflowStep({ id: 'wfs-2', conversationId: 'c1', stepName: 'answer' }),
      /workflow step queue is full/,
    );
    assert.equal(client.debugSnapshot().workflowSteps.length, 1);
    assert.equal(client.debugSnapshot().workflowSteps[0].id, 'wfs-1');
  } finally {
    await client.shutdown();
  }
});

test('flush retries failed workflow step exports with backoff and succeeds', async () => {
  const exporter = new MockGenerationExporter();
  exporter.workflowStepFailuresBeforeSuccess = 2;
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
    maxRetries: 2,
    initialBackoffMs: 1,
    maxBackoffMs: 1,
  });

  try {
    client.enqueueWorkflowStep({
      id: 'wfs-retry',
      conversationId: 'conv-workflow',
      stepName: 'answer',
    });

    await client.flush();
    assert.equal(exporter.workflowStepAttempts, 3);
    assert.equal(exporter.workflowStepRequests.length, 3);
  } finally {
    await client.shutdown();
  }
});

test('flush attempts workflow steps even when generation export fails', async () => {
  const exporter = new MockGenerationExporter(1);
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
    maxRetries: 0,
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(11)), 11);
    client.enqueueWorkflowStep({
      id: 'wfs-isolated',
      conversationId: 'conv-workflow',
      stepName: 'route',
    });

    await assert.rejects(client.flush(), /forced export failure/);
    assert.equal(exporter.requests.length, 1);
    assert.equal(exporter.workflowStepRequests.length, 1);
    assert.equal(exporter.workflowStepRequests[0].workflowSteps[0].id, 'wfs-isolated');
  } finally {
    await client.shutdown();
  }
});

test('shutdown flushes pending generation batch', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
  });

  endWithSuccess(client.startGeneration(seedGeneration(5)), 5);

  await client.shutdown();

  assert.equal(exporter.requests.length, 1);
  assert.equal(exporter.requests[0].generations.length, 1);
  assert.equal(exporter.shutdownCalls, 1);
});

test('shutdown flushes pending workflow step batch', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 10,
    flushIntervalMs: 60_000,
  });

  client.enqueueWorkflowStep({
    id: 'wfs-shutdown',
    conversationId: 'conv-workflow',
    stepName: 'finalize',
  });

  await client.shutdown();

  assert.equal(exporter.workflowStepRequests.length, 1);
  assert.equal(exporter.workflowStepRequests[0].workflowSteps.length, 1);
  assert.equal(exporter.shutdownCalls, 1);
});

test('queue-full recorder local error is exposed and callback style throws', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 10,
    queueSize: 1,
    flushIntervalMs: 60_000,
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(6)), 6);

    const recorder = client.startGeneration(seedGeneration(7));
    recorder.setResult({ output: [{ role: 'assistant', content: 'full' }] });
    recorder.end();

    assert.match(recorder.getError()?.message ?? '', /queue is full/);

    await assert.rejects(
      client.startGeneration(seedGeneration(8), async (callbackRecorder) => {
        callbackRecorder.setResult({ output: [{ role: 'assistant', content: 'callback' }] });
      }),
      /queue is full/,
    );
  } finally {
    await client.shutdown();
  }
});

test('workflow step queue full and payload size errors are surfaced locally', async () => {
  const exporter = new MockGenerationExporter();
  const queueClient = newClient(exporter, {
    batchSize: 10,
    queueSize: 1,
    flushIntervalMs: 60_000,
  });

  try {
    queueClient.enqueueWorkflowStep({
      id: 'wfs-queued',
      conversationId: 'conv-workflow',
      stepName: 'route',
    });
    assert.throws(
      () =>
        queueClient.enqueueWorkflowStep({
          id: 'wfs-overflow',
          conversationId: 'conv-workflow',
          stepName: 'answer',
        }),
      /workflow step queue is full/,
    );
  } finally {
    await queueClient.shutdown();
  }

  const payloadClient = newClient(new MockGenerationExporter(), {
    batchSize: 10,
    payloadMaxBytes: 10,
    flushIntervalMs: 60_000,
  });
  try {
    assert.throws(
      () =>
        payloadClient.enqueueWorkflowStep({
          id: 'wfs-large',
          conversationId: 'conv-workflow',
          stepName: 'route',
          inputState: {
            prompt: 'this payload is intentionally too large',
          },
        }),
      /workflow step payload exceeds max bytes/,
    );
  } finally {
    await payloadClient.shutdown();
  }
});

test('built-in HTTP exporter posts generation batches to configured endpoint', async () => {
  const receivedRequests = [];
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }

    const payload = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    receivedRequests.push(payload);

    response.writeHead(202, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        results: payload.generations.map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      }),
    );
  });

  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve test server address');
  }

  const defaults = defaultConfig();
  const client = new SigilClient({
    tracer: trace.getTracer('sigil-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    endWithSuccess(client.startGeneration(seedGeneration(9)), 9);

    await waitFor(() => receivedRequests.length === 1);
    assert.equal(receivedRequests[0].generations.length, 1);
    assert.equal(receivedRequests[0].generations[0].mode, 'GENERATION_MODE_SYNC');
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('built-in none exporter records generations without sending', async () => {
  const defaults = defaultConfig();
  const client = new SigilClient({
    tracer: trace.getTracer('sigil-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'none',
      endpoint: 'http://127.0.0.1:1',
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
  });

  try {
    const recorder = client.startGeneration(seedGeneration(10));
    recorder.setResult({
      output: [{ role: 'assistant', content: 'ok-10' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    await client.flush();

    const snapshot = client.debugSnapshot();
    assert.equal(snapshot.generations.length, 1);
  } finally {
    await client.shutdown();
  }
});

test('embedding recorder does not enqueue generation exports', async () => {
  const exporter = new MockGenerationExporter();
  const client = newClient(exporter, {
    batchSize: 1,
    flushIntervalMs: 60_000,
  });

  try {
    const recorder = client.startEmbedding({
      model: {
        provider: 'openai',
        name: 'text-embedding-3-small',
      },
    });
    recorder.setResult({
      inputCount: 1,
      inputTokens: 12,
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    await client.flush();

    assert.equal(exporter.requests.length, 0);
    const snapshot = client.debugSnapshot();
    assert.equal(snapshot.generations.length, 0);
  } finally {
    await client.shutdown();
  }
});

function newClient(generationExporter, overrides, clientOverrides = {}) {
  const defaults = defaultConfig();
  return new SigilClient({
    tracer: trace.getTracer('sigil-sdk-js-test'),
    ...clientOverrides,
    generationExport: {
      ...defaults.generationExport,
      ...overrides,
    },
    generationExporter,
  });
}

function seedGeneration(seed) {
  return {
    conversationId: `conv-${seed}`,
    model: {
      provider: 'openai',
      name: 'gpt-5',
    },
  };
}

function endWithSuccess(recorder, seed) {
  recorder.setResult({
    output: [{ role: 'assistant', content: `ok-${seed}` }],
  });
  recorder.end();
  assert.equal(recorder.getError(), undefined);
}

async function waitFor(condition, timeoutMs = 750) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (condition()) {
      return;
    }
    await sleep(5);
  }
  throw new Error('timed out waiting for condition');
}

function sleep(durationMs) {
  return new Promise((resolve) => {
    setTimeout(resolve, durationMs);
  });
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject);
      resolve();
    });
  });
}

function close(server) {
  return new Promise((resolve, reject) => {
    server.close((error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve();
    });
  });
}
