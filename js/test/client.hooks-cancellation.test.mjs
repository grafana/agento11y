import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { createServer } from 'node:http';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
import { evaluateHook } from '../.test-dist/hooks.js';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

const request = {
  phase: 'preflight',
  context: { model: { provider: 'test', name: 'test' } },
  input: { messages: [{ role: 'user', content: 'test' }] },
};

async function within(pending) {
  let timer;
  try {
    return await Promise.race([
      pending,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error('cancellation did not settle within 1s')), 1000);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

for (const failOpen of [true, false]) {
  for (const stage of ['stale', 'headers', 'body']) {
    test(`caller abort rejects and closes HTTP: ${stage}, failOpen=${failOpen}`, async () => {
      let hits = 0;
      let received;
      let disconnected;
      const arrival = new Promise((resolve) => {
        received = resolve;
      });
      const closed = new Promise((resolve) => {
        disconnected = resolve;
      });
      const server = createServer(async (req, res) => {
        hits++;
        for await (const _ of req) {
        }
        if (hits > 1 || stage === 'stale') {
          res.end(JSON.stringify({ action: 'allow' }));
          return;
        }
        res.on('close', disconnected);
        if (stage === 'body') {
          res.writeHead(200, { 'content-type': 'application/json' });
          res.write('{"action":');
        }
        received();
      });
      await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
      const warnings = [];
      const client = new Agento11yClient({
        tracer: trace.getTracer('hook-cancellation-test'),
        api: { endpoint: `http://127.0.0.1:${server.address().port}` },
        generationExport: {
          protocol: 'http',
          endpoint: 'http://127.0.0.1:1/api/v1/generations:export',
          headers: {},
          flushIntervalMs: 0,
        },
        generationExporter: {
          exportGenerations: async () => ({ results: [] }),
          exportWorkflowSteps: async () => ({ results: [] }),
        },
        hooks: { enabled: true, failOpen, timeoutMs: 15000 },
        logger: { warn: (...args) => warnings.push(args) },
      });
      const controller = new AbortController();
      const reason = new Error('caller cancelled');
      try {
        if (stage === 'stale') controller.abort(reason);
        const pending = client.evaluateHook(request, { signal: controller.signal });
        const rejected = assert.rejects(within(pending), (error) => error === reason);
        if (stage !== 'stale') {
          await within(arrival);
          controller.abort(reason);
        }
        await rejected;
        if (stage === 'stale') {
          assert.equal(hits, 0);
        } else {
          await within(closed);
          assert.equal(hits, 1);
        }
        assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
        assert.deepEqual(warnings, []);
        assert.equal((await client.evaluateHook(request)).action, 'allow');
        assert.equal(hits, stage === 'stale' ? 1 : 2);
        assert.equal(client.hooksConfig.signal, undefined);
      } finally {
        controller.abort();
        server.closeAllConnections();
        await new Promise((resolve) => server.close(resolve));
        await client.shutdown();
      }
    });
  }
}

for (const failOpen of [true, false]) {
  test(`timeout keeps failOpen behavior and removes caller listener: ${failOpen}`, async () => {
    const controller = new AbortController();
    let transportSignal;
    const pending = evaluateHook({
      apiEndpoint: 'http://127.0.0.1:1',
      insecure: true,
      hooks: { ...defaultConfig().hooks, enabled: true, failOpen, timeoutMs: 10 },
      request,
      signal: controller.signal,
      fetchImpl: async (_url, { signal }) => {
        transportSignal = signal;
        return new Promise((_, reject) => {
          signal.addEventListener('abort', () => reject(signal.reason), { once: true });
        });
      },
    });
    if (failOpen) {
      assert.equal((await within(pending)).action, 'allow');
    } else {
      await assert.rejects(within(pending), /agento11y hook evaluation failed/);
    }
    assert.equal(transportSignal.aborted, true);
    assert.equal(controller.signal.aborted, false);
    assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  });
}

test('successful evaluation removes the caller listener and timeout', async () => {
  const controller = new AbortController();
  let transportSignal;
  await evaluateHook({
    apiEndpoint: 'http://127.0.0.1:1',
    insecure: true,
    hooks: { ...defaultConfig().hooks, enabled: true, timeoutMs: 10 },
    request,
    signal: controller.signal,
    fetchImpl: async (_url, { signal }) => {
      transportSignal = signal;
      return new Response('{"action":"allow"}');
    },
  });
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  controller.abort();
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(transportSignal.aborted, false);
});
