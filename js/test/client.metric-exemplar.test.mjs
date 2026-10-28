import assert from 'node:assert/strict';
import test from 'node:test';
import { context, ProxyTracerProvider, SpanStatusCode, TraceFlags, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import {
  AggregationTemporality,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import {
  AlwaysOffSampler,
  BasicTracerProvider,
  InMemorySpanExporter,
  SimpleSpanProcessor,
} from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

const contextManager = new AsyncLocalStorageContextManager();

test.before(() => {
  contextManager.enable();
  context.setGlobalContextManager(contextManager);
});

test.after(() => {
  context.disable();
  contextManager.disable();
});

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
  recordings = [];
  instruments = [];

  constructor(inner) {
    this._inner = inner;
    this._capturedTraceIds = this.capturedTraceIds;
  }

  createHistogram(name, options) {
    const inner = this._inner.createHistogram(name, options);
    const capturedTraceIds = this._capturedTraceIds;
    const recordings = this.recordings;
    this.instruments.push({ name, options });
    return {
      record(value, attributes) {
        const activeSpan = trace.getSpan(context.active());
        recordings.push({ name, value, attributes: { ...attributes }, spanContext: activeSpan?.spanContext() });
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
    const tool = harness.client.startToolExecution({
      toolName: 'host-tool',
      executionKind: 'load_skill',
      skillName: 'review',
    });
    tool.end();
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
    assert.deepEqual(harness.client.debugSnapshot().generations, []);
    const identity = {
      'gen_ai.operation.name': 'generateText',
      'gen_ai.provider.name': 'openai',
      'gen_ai.request.model': 'gpt-5',
      'gen_ai.agent.name': '',
    };
    assert.deepEqual(
      harness.capturingMeter.recordings
        .filter(({ attributes }) => attributes['gen_ai.operation.name'] !== 'execute_tool')
        .map(({ name, attributes }) => ({ name, attributes })),
      [
        { name: durationMetric, attributes: { ...identity, 'error.type': '', 'error.category': '' } },
        { name: 'gen_ai.client.token.usage', attributes: { ...identity, 'gen_ai.token.type': 'input' } },
        { name: 'gen_ai.client.token.usage', attributes: { ...identity, 'gen_ai.token.type': 'output' } },
        {
          name: 'gen_ai.client.tool_calls_per_operation',
          attributes: {
            'gen_ai.provider.name': 'openai',
            'gen_ai.request.model': 'gpt-5',
            'gen_ai.agent.name': '',
          },
        },
      ],
    );
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
    const tool = harness.client.startToolExecution({
      toolName: 'host-tool',
      executionKind: 'load_skill',
      skillName: 'review',
    });
    tool.end();
    const recorder = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    recorder.setResult({ inputTokens: 42 });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleEmbeddingSpan(harness.spanExporter);
    const identity = {
      'gen_ai.operation.name': 'embeddings',
      'gen_ai.provider.name': 'openai',
      'gen_ai.request.model': 'text-embedding-3-small',
      'gen_ai.agent.name': '',
    };
    assert.deepEqual(
      harness.capturingMeter.recordings
        .filter(({ attributes }) => attributes['gen_ai.operation.name'] !== 'execute_tool')
        .map(({ name, attributes }) => ({ name, attributes })),
      [
        { name: durationMetric, attributes: { ...identity, 'error.type': '', 'error.category': '' } },
        { name: 'gen_ai.client.token.usage', attributes: { ...identity, 'gen_ai.token.type': 'input' } },
      ],
    );
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
    assert.deepEqual(harness.client.debugSnapshot().toolExecutions, []);
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

const durationMetric = 'gen_ai.client.operation.duration';
const durationBuckets = [0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92];
const toolMetricBaseline = {
  'gen_ai.operation.name': 'execute_tool',
  'gen_ai.tool.name': 'host-tool',
  'gen_ai.provider.name': '',
  'gen_ai.request.model': '',
  'gen_ai.agent.name': '',
  'error.type': '',
  'error.category': '',
};
const startedAt = new Date('2026-01-01T00:00:00Z');
const completedAt = new Date('2026-01-01T00:00:00.250Z');

for (const contentCapture of [undefined, 'full', 'no_tool_content', 'metadata_only', 'full_with_metadata_spans']) {
  for (const tracing of ['sampled', 'unsampled', 'noop']) {
    for (const executionKind of ['load_skill', 'read_skill_resource', 'command']) {
      test(`tool metric identity: ${contentCapture ?? 'default'}/${tracing}/${executionKind}`, async () => {
        const harness = newHarness({ ...(contentCapture ? { contentCapture } : {}) }, tracing);
        try {
          const start = {
            toolName: ' host-tool ',
            executionKind,
            skillName: ' code-review ',
            skillDescription: 'private instructions',
            skillSourceURI: 'file:///private/skill?secret=value',
            skillResourceName: 'private/resource.md',
            processExecutableName: 'private-command',
            processExecutablePath: '/private/command',
            toolDescription: 'private tool',
            conversationId: 'private-conversation',
            toolCallId: 'private-call',
            includeContent: true,
            startedAt,
          };
          const recorder = harness.client.startToolExecution(start);
          start.skillName = 'mutated';
          start.executionKind = 'future';
          recorder.setResult({
            completedAt,
            arguments: { private: 'arguments' },
            result: 'private result',
            processExitCode: 7,
          });
          recorder.end();
          recorder.end();
          assert.equal(recorder.getError(), undefined);
          assert.deepEqual(harness.client.debugSnapshot().toolExecutions, []);
          assert.deepEqual(
            harness.capturingMeter.recordings.map(({ name, value, attributes }) => ({ name, value, attributes })),
            [
              {
                name: durationMetric,
                value: 0.25,
                attributes: {
                  ...toolMetricBaseline,
                  'gen_ai.skill.name': 'code-review',
                  'agento11y.tool.execution.kind': executionKind,
                },
              },
            ],
          );
          const metrics = await harness.metricData();
          assert.deepEqual(
            metrics.map((metric) => metric.descriptor.name),
            [durationMetric],
          );
          const metric = metrics[0];
          assert.equal(metric.descriptor.unit, 's');
          assert.equal(metric.dataPoints.length, 1);
          assert.deepEqual(metric.dataPoints[0].attributes, harness.capturingMeter.recordings[0].attributes);
          assert.equal(metric.dataPoints[0].value.count, 1);
          assert.equal(metric.dataPoints[0].value.sum, 0.25);
          assert.deepEqual(metric.dataPoints[0].value.buckets.boundaries, durationBuckets);
          assert.deepEqual(metric.dataPoints[0].value.buckets.counts, [0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0]);
          assert.deepEqual(
            harness.capturingMeter.instruments.map(({ name }) => name),
            [
              durationMetric,
              'gen_ai.client.token.usage',
              'gen_ai.client.time_to_first_token',
              'gen_ai.client.tool_calls_per_operation',
            ],
          );
          if (tracing === 'sampled') {
            const span = singleToolSpan(harness.spanExporter);
            assert.equal(span.status.code, SpanStatusCode.OK);
            assert.deepEqual(harness.capturingMeter.recordings[0].spanContext, span.spanContext());
          } else {
            assert.deepEqual(toolSpans(harness.spanExporter), []);
            assert.equal(harness.capturingMeter.recordings[0].spanContext.traceFlags, TraceFlags.NONE);
            assert.equal(harness.capturingMeter.capturedTraceIds.length, tracing === 'unsampled' ? 1 : 0);
          }
        } finally {
          await shutdownHarness(harness);
        }
      });
    }
  }
}

for (const outcome of [
  { name: 'success' },
  { name: 'explicit error', callError: new Error('skill not found'), error: 'skill not found' },
  { name: 'arguments serialization', result: { arguments: 1n }, error: /BigInt/ },
  { name: 'result serialization', result: { result: 1n }, error: /BigInt/ },
  {
    name: 'explicit error before serialization',
    callError: new Error('skill not found'),
    result: { result: 1n },
    error: 'skill not found',
  },
  { name: 'uncaptured serialization', contentCapture: 'no_tool_content', result: { result: 1n } },
  { name: 'nonzero command success', executionKind: 'command', result: { processExitCode: 9 } },
]) {
  for (const named of [false, true]) {
    test(`tool metric preserves ${outcome.name}: ${named ? 'named' : 'generic'}`, async () => {
      const harness = newHarness({ contentCapture: outcome.contentCapture ?? 'full' });
      try {
        const recorder = harness.client.startToolExecution({
          toolName: 'host-tool',
          ...(named ? { executionKind: outcome.executionKind ?? 'load_skill', skillName: ' review ' } : {}),
          startedAt,
        });
        assert.doesNotThrow(() => recorder.setResult({ completedAt, ...outcome.result }));
        if (outcome.callError) recorder.setCallError(outcome.callError);
        assert.doesNotThrow(() => recorder.end());
        recorder.end();
        if (outcome.error instanceof RegExp) assert.match(recorder.getError()?.message, outcome.error);
        else assert.equal(recorder.getError()?.message, outcome.error);
        if (outcome.callError) assert.equal(recorder.getError(), outcome.callError);
        const span = singleToolSpan(harness.spanExporter);
        assert.equal(span.status.code, outcome.error ? SpanStatusCode.ERROR : SpanStatusCode.OK);
        assert.equal(span.attributes['error.type'], outcome.error ? 'tool_execution_error' : undefined);
        assert.equal(span.attributes['error.category'], outcome.error ? 'sdk_error' : undefined);
        assert.equal(span.status.message, outcome.error ? recorder.getError().message : undefined);
        assert.equal(span.events.filter((event) => event.name === 'exception').length, outcome.error ? 1 : 0);
        assert.deepEqual(
          harness.capturingMeter.recordings.map(({ attributes }) => attributes),
          [
            {
              ...toolMetricBaseline,
              ...(named
                ? {
                    'gen_ai.skill.name': 'review',
                    'agento11y.tool.execution.kind': outcome.executionKind ?? 'load_skill',
                  }
                : {}),
              'error.type': outcome.error ? 'tool_execution_error' : '',
              'error.category': outcome.error ? 'sdk_error' : '',
            },
          ],
        );
        const metrics = await harness.metricData();
        assert.deepEqual(
          metrics.map((metric) => metric.descriptor.name),
          [durationMetric],
        );
        assert.equal(metrics[0].dataPoints[0].value.count, 1);
        assert.equal(metrics[0].dataPoints[0].value.sum, 0.25);
      } finally {
        await shutdownHarness(harness);
      }
    });
  }
}

test('distinct skill series preserve aggregate tool totals, identity, and tags', async () => {
  const harness = newHarness({ tags: { team: ' sdk ' } });
  try {
    for (const [skillName, executionKind] of [
      ['review', 'load_skill'],
      ['deploy', 'load_skill'],
      ['review', 'load_skill'],
      ['ignored', undefined],
    ]) {
      const recorder = harness.client.startToolExecution({
        toolName: 'host-tool',
        executionKind,
        skillName,
        startedAt,
        requestProvider: ' openai ',
        requestModel: ' gpt-5 ',
        agentName: ' agent ',
        agentVersion: ' v1 ',
      });
      recorder.setResult({ completedAt });
      recorder.end();
      recorder.end();
    }
    const metrics = await harness.metricData();
    assert.deepEqual(
      metrics.map((metric) => metric.descriptor.name),
      [durationMetric],
    );
    const points = metrics[0].dataPoints;
    assert.equal(points.length, 3);
    assert.deepEqual(
      Object.fromEntries(
        points.map((point) => [point.attributes['gen_ai.skill.name'] ?? 'generic', point.value.count]),
      ),
      { review: 2, deploy: 1, generic: 1 },
    );
    assert.equal(
      points.reduce((sum, point) => sum + point.value.count, 0),
      4,
    );
    assert.equal(
      points.reduce((sum, point) => sum + point.value.sum, 0),
      1,
    );
    assert.equal(
      points.reduce((sum, point) => sum + point.value.buckets.counts[5], 0),
      4,
    );
    for (const point of points) {
      const skillName = point.attributes['gen_ai.skill.name'];
      assert.deepEqual(point.attributes, {
        ...toolMetricBaseline,
        'gen_ai.provider.name': 'openai',
        'gen_ai.request.model': 'gpt-5',
        'gen_ai.agent.name': 'agent',
        'gen_ai.agent.version': 'v1',
        'agento11y.tag.team': 'sdk',
        ...(skillName ? { 'gen_ai.skill.name': skillName, 'agento11y.tool.execution.kind': 'load_skill' } : {}),
      });
    }
    assert.deepEqual(harness.client.debugSnapshot().toolExecutions, []);
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function newHarness(overrides = {}, tracing = 'sampled') {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    ...(tracing === 'unsampled' ? { sampler: new AlwaysOffSampler() } : {}),
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer =
    tracing === 'noop'
      ? new ProxyTracerProvider().getTracer('agento11y-sdk-js-test')
      : traceProvider.getTracer('agento11y-sdk-js-test');
  const generationExporter = new CapturingExporter();
  const defaults = defaultConfig();

  const metricReader = new PeriodicExportingMetricReader({
    exporter: new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE),
    exportIntervalMillis: 60_000,
  });
  const meterProvider = new MeterProvider({ readers: [metricReader] });
  const capturingMeter = new SpanCapturingMeter(meterProvider.getMeter('agento11y-test-exemplar'));

  const client = new Agento11yClient({
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
    meterProvider,
    async metricData() {
      const { resourceMetrics } = await metricReader.collect();
      return resourceMetrics.scopeMetrics.flatMap((scope) => scope.metrics);
    },
  };
}

async function shutdownHarness(harness) {
  await harness.client.shutdown();
  await harness.traceProvider.shutdown();
  await harness.meterProvider.shutdown();
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
