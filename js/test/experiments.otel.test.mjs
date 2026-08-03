import assert from 'node:assert/strict';
import test from 'node:test';
import { context, SpanStatusCode, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';

import { ExperimentsClient } from '../.test-dist/experiments/client.js';
import { Experiment } from '../.test-dist/experiments/experiment.js';
import {
  runStatusTelemetry,
  scoreEventAttributes,
  scoreLabel,
  trialIdentityAttributes,
  trialStatusTelemetry,
} from '../.test-dist/experiments/otel.js';
import { FakeExperimentsClient } from './experimentsFakeClient.mjs';

const suite = {
  suiteId: 'smoke',
  name: 'Smoke',
  version: '2.0.0',
  testCases: [{ testCaseId: 'add', name: 'Addition', input: '2+2', expected: '4' }],
};

const verifier = { evaluatorId: 'exact', version: '2', kind: 'deterministic' };

/** Installs an in-memory exporter and restores the previous global provider. */
function withRecorder() {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(exporter)] });
  trace.setGlobalTracerProvider(provider);
  return {
    exporter,
    async dispose() {
      await provider.shutdown();
      trace.disable();
    },
  };
}

test('experimental telemetry is off by default', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient();
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'quiet', suite });
    await experiment.withTrial('add', (trial) => {
      trial.finalScore(true, { evaluator: verifier });
    });
    await experiment.finalize('completed');
    assert.deepEqual(recorder.exporter.getFinishedSpans(), [], 'no experiments span without the opt-in');
  } finally {
    await recorder.dispose();
  }
});

test('the opt-in emits a trial span with the documented identity attributes', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    await experiment.withTrial('add', (trial) => {
      trial.setUsage({ inputTokens: 11, outputTokens: 3 });
      trial.finalScore(true, { evaluator: verifier, explanation: 'matched' });
    });

    const spans = recorder.exporter.getFinishedSpans();
    assert.equal(spans.length, 1);
    const span = spans[0];
    assert.equal(span.name, 'eval.trial add');
    assert.equal(span.instrumentationScope.name, 'sigil_sdk.experiments');
    assert.equal(span.attributes['agento11y.eval.schema.version'], 'experiments-otel-2026-06');
    assert.equal(span.attributes['test.suite.run.id'], 'run-1');
    assert.equal(span.attributes['test.suite.id'], 'smoke');
    assert.equal(span.attributes['test.suite.version'], '2.0.0');
    assert.equal(span.attributes['test.suite.name'], 'Smoke');
    assert.equal(span.attributes['test.suite.run.status'], 'in_progress');
    assert.equal(span.attributes['test.case.id'], 'add');
    assert.equal(span.attributes['test.case.name'], 'Addition');
    assert.equal(span.attributes['test.case.run.attempt'], 1);
    assert.equal(span.attributes['test.case.result.status'], 'pass');
    assert.equal(span.attributes['gen_ai.operation.name'], 'invoke_agent');
    assert.equal(span.attributes['gen_ai.usage.input_tokens'], 11);
    assert.equal(span.attributes['gen_ai.usage.output_tokens'], 3);
    assert.equal(span.status.code, SpanStatusCode.OK);
  } finally {
    await recorder.dispose();
  }
});

test('each score emits a gen_ai.evaluation.result event', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    await experiment.withTrial('add', (trial) => {
      trial.checkScore('json_valid', { passed: true });
      trial.finalScore(0.9, { passed: true, evaluator: verifier, explanation: 'matched' });
    });

    const [span] = recorder.exporter.getFinishedSpans();
    assert.equal(span.events.length, 2);
    assert.ok(span.events.every((event) => event.name === 'gen_ai.evaluation.result'));

    const final = span.events[1];
    assert.equal(final.attributes['gen_ai.evaluation.name'], 'final');
    assert.equal(final.attributes['gen_ai.evaluation.score.value'], 0.9);
    assert.equal(final.attributes['gen_ai.evaluation.score.label'], 'pass');
    assert.equal(final.attributes['gen_ai.evaluation.explanation'], 'matched');
    assert.equal(final.attributes['gen_ai.evaluation.evaluator.id'], 'exact');
    assert.equal(final.attributes['gen_ai.evaluation.evaluator.version'], '2');
    assert.equal(final.attributes['gen_ai.evaluation.evaluator.type'], 'deterministic');

    const check = span.events[0];
    assert.equal(check.attributes['gen_ai.evaluation.name'], 'json_valid');
    assert.equal(check.attributes['gen_ai.evaluation.score.value'], 1, 'a boolean value becomes 1 or 0');
  } finally {
    await recorder.dispose();
  }
});

test('the trial span carries candidate identity and the bound conversation', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, {
      experimentId: 'run-1',
      name: 'nightly',
      suite,
      candidate: { agentName: 'calc', agentVersion: '3', modelProvider: 'openai', modelName: 'gpt-5' },
    });
    await experiment.withTrial('add', (trial) => {
      trial.bindConversation('conv-real');
      trial.finalScore(true, { evaluator: verifier });
    });
    const [span] = recorder.exporter.getFinishedSpans();
    assert.equal(span.attributes['gen_ai.agent.name'], 'calc');
    assert.equal(span.attributes['gen_ai.agent.version'], '3');
    assert.equal(span.attributes['gen_ai.provider.name'], 'openai');
    assert.equal(span.attributes['gen_ai.request.model'], 'gpt-5');
    assert.equal(span.attributes['gen_ai.conversation.id'], 'conv-real');
  } finally {
    await recorder.dispose();
  }
});

test('a trial that errors ends its span with the error status', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    await assert.rejects(
      experiment.withTrial('add', () => {
        throw new Error('candidate failed');
      }),
      /candidate failed/,
    );
    const [span] = recorder.exporter.getFinishedSpans();
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.status.message, 'candidate failed');
    assert.equal(span.attributes['gen_ai.evaluation.explanation'], 'candidate failed');
    assert.equal(span.attributes['test.case.result.status'], undefined, 'errored is not a verdict');
  } finally {
    await recorder.dispose();
  }
});

test('the trial adopts its span trace and span ids', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    const trial = experiment.trial('add');
    await trial.start();
    assert.match(trial.traceId, /^[0-9a-f]{32}$/);
    assert.match(trial.spanId, /^[0-9a-f]{16}$/);
    trial.finalScore(true, { evaluator: verifier });
    await trial.close();
    assert.equal(client.scores[0].traceId, trial.traceId);
    assert.equal(client.trialUpdates[0].traceId, trial.traceId);
  } finally {
    await recorder.dispose();
  }
});

test('a trial without a registered provider stores no trace id', async () => {
  // Every other OTel test installs a provider, so this path was untested: the noop
  // tracer returns an all-zero trace id that is still 32 characters long, and
  // storing it would point the trial and its scores at a trace that never existed.
  trace.disable();
  const client = new FakeExperimentsClient({ useExperimentalOtel: true });
  const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
  const trial = experiment.trial('add');
  await trial.start();
  assert.equal(trial.traceId, '');
  assert.equal(trial.spanId, '');
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();
  assert.equal(client.scores[0].traceId, '');
  assert.equal(client.trialUpdates[0].traceId, '');
});

test('a span started inside the callback becomes a child of the trial span', async () => {
  // Context propagation needs a context manager, which the OTel SDK installs
  // through `provider.register()` in a real app. Mirrors client.metric-exemplar.
  const contextManager = new AsyncLocalStorageContextManager().enable();
  context.setGlobalContextManager(contextManager);
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    await experiment.withTrial('add', (trial) => {
      // Stands in for a generation an instrumented agent emits inside the trial.
      trace.getTracer('test').startSpan('agent call').end();
      trial.finalScore(true, { evaluator: verifier });
    });

    const spans = recorder.exporter.getFinishedSpans();
    const trialSpan = spans.find((span) => span.name === 'eval.trial add');
    const childSpan = spans.find((span) => span.name === 'agent call');
    const parentSpanId = childSpan.parentSpanContext?.spanId ?? childSpan.parentSpanId;
    assert.equal(parentSpanId, trialSpan.spanContext().spanId, 'the agent span hangs off the trial span');
    assert.equal(childSpan.spanContext().traceId, trialSpan.spanContext().traceId);
  } finally {
    await recorder.dispose();
    context.disable();
    contextManager.disable();
  }
});

test('an artifact adds an event to the trial span', async () => {
  const recorder = withRecorder();
  try {
    const client = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'nightly', suite });
    await experiment.withTrial('add', async (trial) => {
      await trial.artifact('notes.md', { text: '# notes', kind: 'markdown', mime: 'text/markdown' });
      trial.finalScore(true, { evaluator: verifier });
    });
    const [span] = recorder.exporter.getFinishedSpans();
    const artifactEvent = span.events.find((event) => event.name === 'agento11y.eval.artifact');
    assert.equal(artifactEvent.attributes['agento11y.eval.artifact.name'], 'notes.md');
    assert.equal(artifactEvent.attributes['agento11y.eval.artifact.kind'], 'markdown');
  } finally {
    await recorder.dispose();
  }
});

test('the client opt-in follows AGENTO11Y_USE_EXPERIMENTAL_OTEL and the experiment can override it', async () => {
  const recorder = withRecorder();
  try {
    const enabled = new FakeExperimentsClient({ useExperimentalOtel: true });
    const experiment = await Experiment.start(enabled, {
      experimentId: 'run-1',
      name: 'nightly',
      suite,
      useExperimentalOtel: false,
    });
    await experiment.withTrial('add', (trial) => {
      trial.finalScore(true, { evaluator: verifier });
    });
    assert.deepEqual(recorder.exporter.getFinishedSpans(), [], 'the experiment opt-out wins');

    // The real client reads the environment variable.
    const base = { endpoint: 'http://localhost:1', ingestToken: 'tok' };
    assert.equal(new ExperimentsClient({ ...base, env: {} }).useExperimentalOtel, false);
    assert.equal(
      new ExperimentsClient({ ...base, env: { AGENTO11Y_USE_EXPERIMENTAL_OTEL: 'on' } }).useExperimentalOtel,
      true,
    );
  } finally {
    await recorder.dispose();
  }
});

// --- attribute builders --------------------------------------------------- //

test('status maps follow the documented enums', () => {
  const runCases = [
    ['running', 'in_progress'],
    ['completed', 'success'],
    ['succeeded', 'success'],
    ['failed', 'failure'],
    ['canceled', 'aborted'],
    ['cancelled', 'aborted'],
    ['nonsense', 'in_progress'],
  ];
  for (const [status, want] of runCases) {
    assert.equal(runStatusTelemetry(status), want, status);
  }
  const trialCases = [
    ['passed', 'pass'],
    ['failed', 'fail'],
    ['running', ''],
    ['errored', ''],
    ['skipped', ''],
    ['completed', ''],
  ];
  for (const [status, want] of trialCases) {
    assert.equal(trialStatusTelemetry(status), want, status);
  }
  assert.equal(scoreLabel(true), 'pass');
  assert.equal(scoreLabel(false), 'fail');
  assert.equal(scoreLabel(undefined), '');
});

test('blank identity fields are omitted rather than sent empty', () => {
  const attrs = trialIdentityAttributes({ experimentId: 'run-1', attempt: 2 });
  assert.deepEqual(Object.keys(attrs).sort(), [
    'agento11y.eval.schema.version',
    'gen_ai.operation.name',
    'test.case.run.attempt',
    'test.suite.run.id',
    'test.suite.run.status',
  ]);
  assert.equal(attrs['test.case.run.attempt'], 2);
});

test('a categorical score value becomes the label', () => {
  const attrs = scoreEventAttributes({ name: 'category', value: 'refusal' });
  assert.equal(attrs['gen_ai.evaluation.score.label'], 'refusal');
  assert.equal(attrs['gen_ai.evaluation.score.value'], undefined, 'a string is not a numeric score');

  const withVerdict = scoreEventAttributes({ name: 'category', value: 'refusal', passed: false });
  assert.equal(withVerdict['gen_ai.evaluation.score.label'], 'fail', 'the verdict wins over the category');
});

test('reference set provenance is emitted when present', () => {
  const attrs = scoreEventAttributes({
    name: 'final',
    value: 1,
    referenceSetId: 'golden',
    referenceSetVersion: '4',
    responseId: 'gen-1',
  });
  assert.equal(attrs['gen_ai.evaluation.reference_set.id'], 'golden');
  assert.equal(attrs['gen_ai.evaluation.reference_set.version'], '4');
  assert.equal(attrs['gen_ai.response.id'], 'gen-1');
});
