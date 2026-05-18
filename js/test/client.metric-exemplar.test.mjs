import assert from 'node:assert/strict';
import test from 'node:test';
import { context, metrics as metricsApi, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { defaultConfig, SigilClient } from '../.test-dist/index.js';

const contextManager = new AsyncLocalStorageContextManager();
context.setGlobalContextManager(contextManager);

class CapturingExporter {
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
}

class SpanCapturingMeter {
  capturedTraceIds = [];

  constructor(inner) {
    this._inner = inner;
    this._capturedTraceIds = this.capturedTraceIds;
  }

  createHistogram(name, options) {
    const inner = this._inner.createHistogram(name, options);
    const capturedTraceIds = this._capturedTraceIds;
    return {
      record(value, attributes) {
        const activeSpan = trace.getSpan(context.active());
        if (activeSpan?.spanContext) {
          const sc = activeSpan.spanContext();
          if (sc.traceId && sc.traceId !== '00000000000000000000000000000000') {
            capturedTraceIds.push(sc.traceId);
          }
        }
        return inner.record(value, attributes);
      },
    };
  }

  createCounter(name, options) {
    return this._inner.createCounter(name, options);
  }

  createUpDownCounter(name, options) {
    return this._inner.createUpDownCounter(name, options);
  }

  createObservableGauge(name, options) {
    return this._inner.createObservableGauge(name, options);
  }

  createObservableCounter(name, options) {
    return this._inner.createObservableCounter(name, options);
  }

  createObservableUpDownCounter(name, options) {
    return this._inner.createObservableUpDownCounter(name, options);
  }

  createGauge(name, options) {
    return this._inner.createGauge(name, options);
  }
}

test('generation metric recording has active span context', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      input: [{ role: 'user', content: 'hi' }],
      output: [{ role: 'assistant', content: 'hello' }],
      usage: { inputTokens: 10, outputTokens: 5 },
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleGenerationSpan(harness.spanExporter);
    assert.ok(
      harness.capturingMeter.capturedTraceIds.length > 0,
      'histogram.record should have been called with active span',
    );
    assert.ok(
      harness.capturingMeter.capturedTraceIds.includes(span.spanContext().traceId),
      'metric should carry generation span trace_id',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

test('embedding metric recording has active span context', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    recorder.setResult({ inputTokens: 42 });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.ok(
      harness.capturingMeter.capturedTraceIds.length > 0,
      'histogram.record should have been called with active span',
    );
    assert.ok(
      harness.capturingMeter.capturedTraceIds.includes(span.spanContext().traceId),
      'metric should carry embedding span trace_id',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

test('tool execution metric recording has active span context', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startToolExecution({
      toolName: 'weather',
    });
    recorder.setResult({ result: 'sunny' });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleToolSpan(harness.spanExporter);
    assert.ok(
      harness.capturingMeter.capturedTraceIds.length > 0,
      'histogram.record should have been called with active span',
    );
    assert.ok(
      harness.capturingMeter.capturedTraceIds.includes(span.spanContext().traceId),
      'metric should carry tool span trace_id',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function newHarness(overrides = {}) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('sigil-sdk-js-test');
  const generationExporter = new CapturingExporter();
  const defaults = defaultConfig();

  const capturingMeter = new SpanCapturingMeter(metricsApi.getMeter('sigil-test-exemplar'));

  const client = new SigilClient({
    tracer,
    meter: capturingMeter,
    generationExport: {
      ...defaults.generationExport,
      batchSize: 100,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
    generationExporter,
    ...overrides,
  });

  return {
    client,
    spanExporter,
    traceProvider,
    generationExporter,
    capturingMeter,
  };
}

async function shutdownHarness(harness) {
  await harness.client.shutdown();
  await harness.traceProvider.shutdown();
}

function generationSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => {
    const operation = span.attributes['gen_ai.operation.name'];
    return operation !== 'execute_tool' && operation !== 'embeddings';
  });
}

function embeddingSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => span.attributes['gen_ai.operation.name'] === 'embeddings');
}

function toolSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => span.attributes['gen_ai.operation.name'] === 'execute_tool');
}

function singleGenerationSpan(spanExporter) {
  const spans = generationSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}

function singleEmbeddingSpan(spanExporter) {
  const spans = embeddingSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}

function singleToolSpan(spanExporter) {
  const spans = toolSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}
