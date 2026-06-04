import assert from 'node:assert/strict';
import test from 'node:test';
import {
  AggregationTemporality,
  DataPointType,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { defaultConfig, SigilClient } from '../.test-dist/index.js';

const clientTagProjectKey = 'sigil.tag.project';

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

test('client SIGIL_TAGS appear on generation span and metrics', async () => {
  const harness = newHarness({ tags: { project: 'checkout-svc' } });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      usage: { inputTokens: 10, outputTokens: 5 },
    });
    recorder.end();

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.attributes[clientTagProjectKey], 'checkout-svc');

    const metricAttrs = await harness.metricDataPointAttributes('gen_ai.client.operation.duration');
    assert.ok(
      metricAttrs.some((attrs) => attrs[clientTagProjectKey] === 'checkout-svc'),
      'expected sigil.tag.project on operation.duration metric',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

test('client tags on embedding and tool spans', async () => {
  const harness = newHarness({ tags: { project: 'embed-tools' } });

  try {
    const embed = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    embed.setResult({ inputTokens: 1 });
    embed.end();

    const embedSpan = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(embedSpan.attributes[clientTagProjectKey], 'embed-tools');

    const tool = harness.client.startToolExecution({ toolName: 'weather' });
    tool.setResult({ result: 'sunny' });
    tool.end();

    const toolSpan = singleToolSpan(harness.spanExporter);
    assert.equal(toolSpan.attributes[clientTagProjectKey], 'embed-tools');
  } finally {
    await shutdownHarness(harness);
  }
});

test('empty client tags are omitted from spans', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.end();

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.attributes[clientTagProjectKey], undefined);
  } finally {
    await shutdownHarness(harness);
  }
});

test('per-call generation tags stay export-only', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      tags: { call_only: 'yes' },
    });
    recorder.end();

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.attributes['sigil.tag.call_only'], undefined);

    await harness.client.shutdown();
    const generation = harness.generationExporter.requests[0]?.generations?.[0];
    assert.equal(generation?.tags?.call_only, 'yes');
  } finally {
    await harness.traceProvider.shutdown();
  }
});

function newHarness(overrides = {}) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('sigil-sdk-js-test');
  const generationExporter = new CapturingExporter();
  const defaults = defaultConfig();

  const metricExporter = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
  const metricReader = new PeriodicExportingMetricReader({
    exporter: metricExporter,
    exportIntervalMillis: 60_000,
  });
  const meterProvider = new MeterProvider({ readers: [metricReader] });

  const client = new SigilClient({
    tracer,
    meter: meterProvider.getMeter('sigil-test'),
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
    metricExporter,
    meterProvider,
    metricReader,
    async metricDataPointAttributes(metricName) {
      await metricReader.forceFlush();
      const metric = metricExporter
        .getMetrics()
        .flatMap((resourceMetrics) => resourceMetrics.scopeMetrics)
        .flatMap((scopeMetrics) => scopeMetrics.metrics)
        .find((m) => m.descriptor.name === metricName && m.dataPointType === DataPointType.HISTOGRAM);
      assert.ok(metric, `expected histogram metric ${metricName}`);
      return metric.dataPoints.map((point) => point.attributes);
    },
  };
}

async function shutdownHarness(harness) {
  await harness.client.shutdown();
  await harness.meterProvider.shutdown();
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
