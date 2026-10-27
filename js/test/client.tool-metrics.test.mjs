import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { context, SpanStatusCode, trace, ValueType } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import {
  AggregationTemporality,
  DataPointType,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  BasicTracerProvider,
  InMemorySpanExporter,
  SimpleSpanProcessor,
} from '@opentelemetry/sdk-trace-base';
import { Agento11yClient } from '../.test-dist/index.js';

const fixture = JSON.parse(readFileSync(new URL('../../conformance/tool-metrics/cases.json', import.meta.url)));
const legacyName = 'gen_ai.client.operation.duration';
const startedAt = new Date('2026-01-01T00:00:00Z');
const completedAt = new Date(startedAt.getTime() + 125);
const boundaries = [0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92];
const contextManager = new AsyncLocalStorageContextManager();

test.before(() => context.setGlobalContextManager(contextManager.enable()));
test.after(() => {
  context.disable();
  contextManager.disable();
});

function camelFields(fields = {}) {
  return Object.fromEntries(
    Object.entries(fields).map(([key, value]) => [key.replace(/_([a-z])/g, (_, c) => c.toUpperCase()), value]),
  );
}

function harness(overrides = {}, { sampling = 'on', failInstrument = false } = {}) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    sampler: sampling === 'off' ? new AlwaysOffSampler() : new AlwaysOnSampler(),
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const metricExporter = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
  const reader = new PeriodicExportingMetricReader({ exporter: metricExporter, exportIntervalMillis: 60_000 });
  const meterProvider = new MeterProvider({ readers: [reader] });
  const innerMeter = meterProvider.getMeter('tool-metrics-test');
  const records = [];
  const instruments = [];
  const warnings = [];
  const meter = {
    createHistogram(name, options) {
      instruments.push({ name, options });
      if (failInstrument && name === fixture.instrument) throw new Error('optional instrument unavailable');
      const inner = innerMeter.createHistogram(name, options);
      return {
        record(value, attributes) {
          records.push({
            name,
            value,
            attributes: { ...attributes },
            spanContext: trace.getSpan(context.active())?.spanContext(),
          });
          inner.record(value, attributes);
        },
      };
    },
  };
  const client = new Agento11yClient({
    tracer: sampling === 'none' ? trace.getTracer('tool-metrics-noop') : traceProvider.getTracer('tool-metrics-test'),
    meter,
    generationExport: { flushIntervalMs: 0 },
    generationExporter: {
      async exportGenerations() {
        return { results: [] };
      },
      async exportWorkflowSteps() {
        return { results: [] };
      },
    },
    logger: { warn: (...args) => warnings.push(args) },
    ...overrides,
  });
  return {
    client,
    records,
    instruments,
    warnings,
    spanExporter,
    tool: () => records.filter((r) => r.name === fixture.instrument),
    legacy: () => records.filter((r) => r.name === legacyName),
    async metric(name) {
      await reader.forceFlush();
      return metricExporter
        .getMetrics()
        .flatMap((r) => r.scopeMetrics)
        .flatMap((s) => s.metrics)
        .find((m) => m.descriptor.name === name);
    },
    async close() {
      await client.shutdown();
      await meterProvider.shutdown();
      await traceProvider.shutdown();
    },
  };
}

function execute(h, start = {}, result = {}, error) {
  const recorder = h.client.startToolExecution({ toolName: 'read', startedAt, ...start });
  recorder.setResult({ completedAt, ...result });
  if (error !== undefined) recorder.setCallError(error);
  recorder.end();
  return recorder;
}

for (const entry of fixture.cases) {
  test(`shared tool metric matrix: ${entry.id}`, async () => {
    const h = harness();
    try {
      const executions = entry.executions ?? 1;
      for (let i = 0; i < executions; i++) {
        const recorder = execute(h, camelFields(entry.start), camelFields(entry.end), entry.error);
        for (let j = 1; j < (entry.end_calls ?? 1); j++) recorder.end();
      }
      assert.equal(h.tool().length, entry.count);
      assert.equal(h.legacy().length, executions);
      assert.equal(h.client.debugSnapshot().toolExecutions.length, executions);
      for (const record of h.tool()) {
        assert.equal(record.value, 0.125);
        assert.deepEqual(record.attributes, entry.attributes);
      }
      for (const record of h.legacy()) {
        assert.equal(record.value, 0.125);
        assert.equal(record.attributes['error.type'], entry.error ? 'tool_execution_error' : '');
        for (const key of fixture.reserved_attributes) {
          if (key !== 'error.type') assert.equal(record.attributes[key], undefined, key);
        }
      }
      const metric = await h.metric(fixture.instrument);
      assert.equal(metric.dataPointType, DataPointType.HISTOGRAM);
      assert.equal(metric.dataPoints.length, 1);
      assert.equal(metric.dataPoints[0].value.count, entry.count);
      assert.equal(metric.dataPoints[0].value.sum, entry.count * 0.125);
    } finally {
      await h.close();
    }
  });
}

test('tool duration and legacy metrics are emitted by default', async () => {
  const h = harness();
  try {
    execute(h);
    assert.equal(h.tool().length, 1);
    assert.equal(h.legacy().length, 1);
    assert.equal(h.tool()[0].value, 0.125);
    assert.deepEqual(h.tool()[0].attributes, {
      'gen_ai.tool.name': 'read',
      'agento11y.tool.execution.kind': 'generic',
    });
    assert.equal(h.legacy()[0].attributes['gen_ai.operation.name'], 'execute_tool');
  } finally {
    await h.close();
  }
});

for (const sampling of ['on', 'off', 'none']) {
  for (const mode of ['default', 'full', 'no_tool_content', 'metadata_only', 'full_with_metadata_spans']) {
    for (const field of ['arguments', 'result']) {
      for (const includeContent of [undefined, false, true]) {
        test(`serialization-only failure: ${sampling}, ${mode}, ${field}, includeContent=${includeContent}`, async () => {
          const h = harness({ contentCapture: mode }, { sampling });
          try {
            const circular = {};
            circular.self = circular;
            const recorder = execute(
              h,
              { executionKind: 'load_skill', skillName: 'attempted', includeContent },
              { [field]: circular, verifiedSkillName: 'verified' },
            );
            const capturesContent =
              mode === 'full' || ((mode === 'default' || mode === 'no_tool_content') && includeContent === true);
            assert.equal(recorder.getError() !== undefined, capturesContent);
            assert.equal(h.tool()[0].attributes['error.type'], undefined);
            assert.equal(h.tool()[0].attributes['gen_ai.skill.name'], 'verified');
            assert.equal(h.legacy()[0].attributes['error.type'], capturesContent ? 'tool_execution_error' : '');
            const spans = h.spanExporter.getFinishedSpans();
            assert.equal(spans.length, sampling === 'on' ? 1 : 0);
            if (sampling === 'on') {
              assert.equal(spans[0].status.code, capturesContent ? SpanStatusCode.ERROR : SpanStatusCode.OK);
              assert.equal(spans[0].attributes['gen_ai.skill.name'], 'attempted');
              assert.equal(spans[0].name, 'execute_tool read attempted');
            }
          } finally {
            await h.close();
          }
        });
      }
    }
  }
}

test('execution errors beat serialization errors and benign command overrides', async () => {
  const h = harness({ contentCapture: 'full' });
  try {
    execute(
      h,
      { executionKind: 'command' },
      { result: 1n, processExitCode: 2, processExitCodeIsError: false },
      new Error('deadline exceeded'),
    );
    assert.equal(h.tool()[0].attributes['error.type'], 'tool_execution_error');
    assert.equal(h.legacy()[0].attributes['error.type'], 'tool_execution_error');
    assert.equal(h.spanExporter.getFinishedSpans()[0].attributes['error.category'], 'timeout');
  } finally {
    await h.close();
  }
});

for (const code of [
  2.5,
  NaN,
  Infinity,
  -Infinity,
  Number.MAX_SAFE_INTEGER + 1,
  '2',
  true,
  null,
  undefined,
  0,
  -2,
  Number.MAX_SAFE_INTEGER,
]) {
  test(`command exit validation: ${String(code)}`, async () => {
    const h = harness();
    try {
      execute(h, { executionKind: 'command' }, { processExitCode: code, processExitCodeIsError: true });
      const valid = typeof code === 'number' && Number.isSafeInteger(code);
      const attrs = h.tool()[0].attributes;
      assert.equal(attrs['process.exit.code'], valid ? code : undefined);
      assert.equal(attrs['error.type'], valid && code !== 0 ? String(code) : undefined);
      assert.equal(h.legacy()[0].attributes['error.type'], '');
      assert.equal(h.spanExporter.getFinishedSpans()[0].status.code, SpanStatusCode.OK);
    } finally {
      await h.close();
    }
  });
}

test('result replacement, mutation and debug snapshot copies retain only final completion identity', async () => {
  const h = harness();
  try {
    const seed = { toolName: 'exec', executionKind: 'command', skillName: 'attempted', startedAt: new Date(startedAt) };
    const recorder = h.client.startToolExecution(seed);
    seed.executionKind = 'load_skill';
    seed.startedAt.setFullYear(2000);
    recorder.setResult({
      verifiedSkillName: 'stale',
      verifiedSkillResourceName: 'stale.py',
      processExitCode: 2,
      processExitCodeIsError: false,
    });
    recorder.setResult({ completedAt });
    recorder.end();
    assert.equal(h.tool()[0].attributes['gen_ai.skill.name'], undefined);
    assert.equal(h.tool()[0].attributes['gen_ai.skill.resource.name'], undefined);
    assert.equal(h.tool()[0].attributes['process.exit.code'], undefined);
    const result = {
      completedAt: new Date(completedAt),
      verifiedSkillName: 'review',
      verifiedSkillResourceName: 'check.py',
      processExitCode: 2,
      processExitCodeIsError: false,
    };
    const next = h.client.startToolExecution({ toolName: 'exec', executionKind: 'command', startedAt });
    next.setResult(result);
    result.verifiedSkillName = 'mutated';
    result.verifiedSkillResourceName = 'mutated.py';
    result.processExitCodeIsError = true;
    result.processExitCode = 3;
    result.completedAt.setFullYear(2000);
    next.end();
    next.setResult({ verifiedSkillName: 'too late' });
    next.setCallError(new Error('too late'));
    next.end();
    const snapshot = h.client.debugSnapshot().toolExecutions[1];
    assert.equal(snapshot.verifiedSkillName, 'review');
    assert.equal(snapshot.verifiedSkillResourceName, 'check.py');
    assert.equal(snapshot.processExitCodeIsError, false);
    assert.equal(snapshot.processExitCode, 2);
    assert.equal(snapshot.completedAt.getTime(), completedAt.getTime());
    snapshot.verifiedSkillName = 'mutated snapshot';
    snapshot.verifiedSkillResourceName = 'mutated snapshot';
    snapshot.processExitCodeIsError = true;
    snapshot.completedAt.setFullYear(2000);
    const fresh = h.client.debugSnapshot().toolExecutions[1];
    assert.equal(fresh.verifiedSkillName, 'review');
    assert.equal(fresh.verifiedSkillResourceName, 'check.py');
    assert.equal(fresh.processExitCodeIsError, false);
    assert.equal(fresh.completedAt.getTime(), completedAt.getTime());
    assert.equal(h.tool().length, 2);
    assert.equal(h.tool()[1].attributes['gen_ai.skill.name'], 'review');
    assert.equal(h.tool()[1].attributes['error.type'], undefined);
  } finally {
    await h.close();
  }
});

test('allowlisted metric attributes exclude raw overrides and private or high-cardinality data', async () => {
  const raw = Object.fromEntries(fixture.reserved_attributes.map((key) => [key, 'injected']));
  const h = harness({
    tags: { team: 'tools', 'gen_ai.skill.name': 'tag-not-identity' },
  });
  try {
    execute(
      h,
      {
        toolName: ' exec ',
        toolType: ' function ',
        executionKind: 'command',
        skillName: 'attempted',
        skillDescription: 'private',
        skillSourceURI: 'file:///private',
        processExecutableName: 'python',
        processExecutablePath: '/private',
        conversationId: 'secret',
        toolCallId: 'secret',
        toolDescription: 'private',
        requestProvider: ' provider ',
        requestModel: ' model ',
        agentName: ' agent ',
        agentVersion: ' v1 ',
        attributes: raw,
        metricAttributes: raw,
      },
      { verifiedSkillResourceName: 'check.py', processExitCode: 0, attributes: raw, metricAttributes: raw },
    );
    assert.deepEqual(h.tool()[0].attributes, {
      'gen_ai.tool.name': 'exec',
      'gen_ai.tool.type': 'function',
      'agento11y.tool.execution.kind': 'command',
      'gen_ai.provider.name': 'provider',
      'gen_ai.request.model': 'model',
      'gen_ai.agent.name': 'agent',
      'gen_ai.agent.version': 'v1',
      'agento11y.tag.team': 'tools',
      'agento11y.tag.gen_ai.skill.name': 'tag-not-identity',
      'gen_ai.skill.resource.name': 'check.py',
      'process.exit.code': 0,
    });
  } finally {
    await h.close();
  }
});

test('optional instrument creation failure leaves all legacy instruments working', async () => {
  const h = harness({}, { failInstrument: true });
  try {
    execute(h).end();
    const generation = h.client.startGeneration({ model: { provider: 'test', name: 'test' } });
    generation.setResult({ usage: { inputTokens: 3 } });
    generation.end();
    assert.equal(h.tool().length, 0);
    assert.equal(h.legacy().length, 2);
    assert.ok(h.records.some((r) => r.name === 'gen_ai.client.token.usage'));
    assert.ok(h.records.some((r) => r.name === 'gen_ai.client.tool_calls_per_operation'));
    assert.equal(h.warnings.length, 1);
    assert.equal(h.warnings[0][0], 'agento11y: could not create tool duration histogram');
  } finally {
    await h.close();
  }
});

test('double histogram uses existing seconds buckets; dual emission shares exemplar context', async () => {
  const h = harness();
  try {
    execute(h);
    const instrument = h.instruments.find((i) => i.name === fixture.instrument);
    assert.equal(instrument.options.unit, fixture.unit);
    assert.equal(instrument.options.valueType, ValueType.DOUBLE);
    assert.deepEqual(instrument.options.advice.explicitBucketBoundaries, boundaries);
    const metric = await h.metric(fixture.instrument);
    const legacy = await h.metric(legacyName);
    assert.equal(metric.descriptor.unit, 's');
    assert.deepEqual(metric.dataPoints[0].value, legacy.dataPoints[0].value);
    assert.deepEqual(metric.dataPoints[0].value.buckets.boundaries, boundaries);
    assert.equal(metric.dataPoints[0].value.buckets.counts[4], 1);
    assert.equal(metric.dataPoints[0].value.count, 1);
    assert.equal(metric.dataPoints[0].value.sum, 0.125);
    const spanContext = h.spanExporter.getFinishedSpans()[0].spanContext();
    assert.deepEqual(h.tool()[0].spanContext, spanContext);
    assert.deepEqual(h.legacy()[0].spanContext, spanContext);
  } finally {
    await h.close();
  }
});

test('bucket boundary and overflow measurements remain additive and fractional', async () => {
  const h = harness();
  try {
    const durations = [0, ...boundaries, boundaries.at(-1) + 1];
    for (const seconds of durations) {
      execute(h, {}, { completedAt: new Date(startedAt.getTime() + seconds * 1_000) });
    }
    const metric = await h.metric(fixture.instrument);
    const legacy = await h.metric(legacyName);
    const value = metric.dataPoints[0].value;
    assert.deepEqual(value, legacy.dataPoints[0].value);
    assert.deepEqual(value.buckets.counts, [2, ...boundaries.slice(1).map(() => 1), 1]);
    assert.equal(value.count, durations.length);
    assert.ok(Math.abs(value.sum - durations.reduce((sum, v) => sum + v, 0)) < 1e-9);
  } finally {
    await h.close();
  }
});

test('verified completion identity never changes attempted sampler inputs or span identity', async () => {
  const starts = [];
  const spanContext = { traceId: '1234567890abcdef1234567890abcdef', spanId: '1234567890abcdef', traceFlags: 0 };
  const tracer = {
    startSpan(name, options) {
      starts.push({ name, attributes: { ...options.attributes } });
      return trace.wrapSpanContext(spanContext);
    },
  };
  const h = harness({ tracer });
  try {
    execute(h, { executionKind: 'load_skill', skillName: 'attempted' }, { verifiedSkillName: 'verified' });
    assert.equal(starts[0].name, 'execute_tool read attempted');
    assert.equal(starts[0].attributes['gen_ai.skill.name'], 'attempted');
    assert.equal(starts[0].attributes['agento11y.tool.execution.kind'], undefined);
    assert.equal(h.tool()[0].attributes['gen_ai.skill.name'], 'verified');
    assert.deepEqual(h.tool()[0].spanContext, spanContext);
    assert.equal(h.legacy().length, 1);
  } finally {
    await h.close();
  }
});

test('negative duration is clamped on both instruments, without turning validation into execution failure', async () => {
  const h = harness();
  try {
    const recorder = execute(h, {}, { completedAt: new Date(startedAt.getTime() - 100) });
    assert.ok(recorder.getError());
    assert.equal(h.tool()[0].value, 0);
    assert.equal(h.legacy()[0].value, 0);
    assert.equal(h.tool()[0].attributes['error.type'], undefined);
    assert.equal(h.legacy()[0].attributes['error.type'], 'tool_execution_error');
  } finally {
    await h.close();
  }
});
