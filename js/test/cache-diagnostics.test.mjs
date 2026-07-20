import assert from 'node:assert/strict';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
import {
  Agento11yClient,
  CACHE_DIAGNOSTICS_MISS_REASON_KEY,
  CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
  CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
  defaultConfig,
  setCacheDiagnostics,
} from '../.test-dist/index.js';

class MockGenerationExporter {
  requests = [];

  async exportGenerations(request) {
    this.requests.push(structuredClone(request));
    return {
      results: request.generations.map((generation) => ({
        generationId: generation.id,
        accepted: true,
      })),
    };
  }

  async shutdown() {}
}

test('setCacheDiagnostics stamps metadata on generation', async () => {
  const exporter = new MockGenerationExporter();
  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
  });
  try {
    const rec = client.startGeneration({
      conversationId: 'conv-cd',
      model: { provider: 'anthropic', name: 'claude-3-5-sonnet-latest' },
    });
    setCacheDiagnostics(rec, 'messages_changed', {
      missedInputTokens: 500,
      previousMessageId: 'msg_prev',
    });
    rec.setResult({ output: [{ role: 'assistant', content: 'ok' }] });
    rec.end();
    assert.equal(rec.getError(), undefined);
    await client.flush();
    const snap = client.debugSnapshot();
    assert.equal(snap.generations.length, 1);
    const md = snap.generations[0].metadata ?? {};
    assert.equal(md[CACHE_DIAGNOSTICS_MISS_REASON_KEY], 'messages_changed');
    assert.equal(md[CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY], '500');
    assert.equal(md[CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY], 'msg_prev');
  } finally {
    await client.shutdown();
  }
});

test('setCacheDiagnostics ignores empty reason', async () => {
  const exporter = new MockGenerationExporter();
  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
  });
  try {
    const rec = client.startGeneration({
      model: { provider: 'anthropic', name: 'claude-3-5-sonnet-latest' },
    });
    setCacheDiagnostics(rec, '   ');
    rec.setResult({ output: [{ role: 'assistant', content: 'ok' }] });
    rec.end();
    await client.flush();
    const md = client.debugSnapshot().generations[0].metadata ?? {};
    assert.equal(md[CACHE_DIAGNOSTICS_MISS_REASON_KEY], undefined);
  } finally {
    await client.shutdown();
  }
});
