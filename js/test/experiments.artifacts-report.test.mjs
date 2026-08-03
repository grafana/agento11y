import assert from 'node:assert/strict';
import test from 'node:test';

import { artifactKindFromMime, Experiment } from '../.test-dist/experiments/experiment.js';
import { parseExperimentReport, parseReportSummary } from '../.test-dist/experiments/models.js';
import { FakeExperimentsClient } from './experimentsFakeClient.mjs';

const suite = {
  suiteId: 'smoke',
  name: 'Smoke',
  version: '1.0.0',
  testCases: [{ testCaseId: 'add', input: '2+2', expected: '4' }],
};

async function openTrial(client) {
  const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'artifacts', suite });
  const trial = experiment.trial('add');
  await trial.start();
  return { experiment, trial };
}

test('a text artifact infers its kind and mime', async () => {
  const client = new FakeExperimentsClient();
  const { trial } = await openTrial(client);
  const record = await trial.artifact('transcript.txt', { text: 'hello' });

  assert.equal(record.artifact_id, 'art_transcript.txt');
  assert.equal(client.artifacts[0].experimentId, 'run-1');
  assert.equal(client.artifacts[0].parentId, trial.trialId);
  assert.equal(client.artifacts[0].kind, 'text');
  assert.equal(client.artifacts[0].mime, 'text/plain');
  assert.equal(new TextDecoder().decode(client.artifacts[0].content), 'hello');
  assert.deepEqual(trial.artifacts, [{ name: 'transcript.txt', kind: 'text', artifactId: 'art_transcript.txt' }]);
});

test('a data artifact is serialized as JSON', async () => {
  const client = new FakeExperimentsClient();
  const { trial } = await openTrial(client);
  await trial.artifact('trace.json', { data: { steps: [1, 2] } });
  assert.equal(client.artifacts[0].kind, 'json');
  assert.equal(client.artifacts[0].mime, 'application/json');
  assert.deepEqual(JSON.parse(new TextDecoder().decode(client.artifacts[0].content)), { steps: [1, 2] });
});

test('raw bytes default to binary and honor explicit metadata', async () => {
  const client = new FakeExperimentsClient();
  const { trial } = await openTrial(client);
  await trial.artifact('screenshot', { bytes: new Uint8Array([1, 2, 3]) });
  assert.equal(client.artifacts[0].kind, 'binary');
  assert.equal(client.artifacts[0].mime, 'application/octet-stream');

  await trial.artifact('screenshot.png', { bytes: new Uint8Array([1]), mime: 'image/png' });
  assert.equal(client.artifacts[1].kind, 'image');

  await trial.artifact('notes', { text: 'x', kind: 'markdown', mime: 'text/markdown' });
  assert.equal(client.artifacts[2].kind, 'markdown');
  assert.equal(client.artifacts[2].mime, 'text/markdown');
});

test('an artifact with no content is rejected', async () => {
  const client = new FakeExperimentsClient();
  const { trial } = await openTrial(client);
  await assert.rejects(
    trial.artifact('empty', {}),
    /agento11y experiment validation failed: artifact requires one of data, text, or bytes/,
  );
  assert.equal(client.artifacts.length, 0);
});

const mimeCases = [
  { mime: 'image/png', kind: 'image' },
  { mime: 'application/json', kind: 'json' },
  { mime: 'text/markdown', kind: 'markdown' },
  { mime: 'text/x-markdown', kind: 'markdown' },
  { mime: 'application/pdf', kind: 'pdf' },
  { mime: 'text/csv', kind: 'csv' },
  { mime: 'text/plain', kind: 'text' },
  { mime: 'application/octet-stream', kind: 'binary' },
  { mime: '', kind: 'binary' },
];

test('artifact kinds follow the documented MIME inference', () => {
  for (const { mime, kind } of mimeCases) {
    assert.equal(artifactKindFromMime(mime), kind, mime);
  }
});

test('a report summary keeps omitted aggregates undefined', () => {
  const summary = parseReportSummary({ trial_count: 4, completed_count: 3, pass_count: 2, pass_denominator: 4 });
  assert.equal(summary.trialCount, 4);
  assert.equal(summary.passRate, undefined, 'an omitted pass rate is not zero');
  assert.equal(summary.finalScoreAvg, undefined);
  assert.equal(summary.totalCost, undefined);
  assert.equal(summary.totalTokens, undefined);
  assert.deepEqual(summary.passAtK, {});
});

test('a report summary reads present aggregates', () => {
  const summary = parseReportSummary({
    test_case_count: 2,
    trial_count: 4,
    completed_count: 4,
    failed_count: 0,
    canceled_count: 0,
    pass_rate: 0.75,
    pass_at_k: { 1: 0.5, 2: 1 },
    pass_power_k: { 2: 0.25 },
    final_score_avg: 0.8,
    total_cost: 0.02,
    total_tokens: 1234,
    pass_count: 3,
    pass_denominator: 4,
    final_score_sum: 3.2,
    final_score_count: 4,
    token_coverage: 'complete',
    cost_coverage: 'partial',
  });
  assert.equal(summary.passRate, 0.75);
  assert.deepEqual(summary.passAtK, { 1: 0.5, 2: 1 });
  assert.deepEqual(summary.passPowerK, { 2: 0.25 });
  assert.equal(summary.finalScoreAvg, 0.8);
  assert.equal(summary.totalTokens, 1234);
  assert.equal(summary.tokenCoverage, 'complete');
  assert.equal(summary.costCoverage, 'partial');
});

test('both report envelopes normalize to the same report', () => {
  const payload = {
    summary: { trial_count: 1, completed_count: 1 },
    rows: [{ test_case_id: 'add' }, 'not a row'],
  };
  const run = { experiment_id: 'run-1', name: 'nightly', status: 'completed', tags: ['nightly'] };
  const fromExperiment = parseExperimentReport({ ...payload, experiment: run });
  const fromRun = parseExperimentReport({ ...payload, run });
  assert.deepEqual(fromExperiment, fromRun);
  assert.equal(fromExperiment.run.runId, 'run-1');
  assert.deepEqual(fromExperiment.run.tags, ['nightly']);
  assert.equal(fromExperiment.rows.length, 1, 'a non-object row is dropped');
});

test('an experiment id under run_id is still read', () => {
  const report = parseExperimentReport({ run: { run_id: 'run-legacy' }, summary: {} });
  assert.equal(report.run.runId, 'run-legacy');
});

test('a non-object report payload is a transport error', () => {
  assert.throws(() => parseExperimentReport('nope'), /agento11y experiment transport failed: invalid response payload/);
});

test('experiment.report goes through the client', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'reported', suite });
  const report = await experiment.report();
  assert.equal(report.run.runId, 'run-1');
  assert.ok(client.calls.includes('get_report'));
});
