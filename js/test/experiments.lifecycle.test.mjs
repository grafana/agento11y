import assert from 'node:assert/strict';
import test from 'node:test';

import { Experiment, Trial, withExperiment } from '../.test-dist/experiments/experiment.js';
import { FakeExperimentsClient } from './experimentsFakeClient.mjs';

const suite = {
  suiteId: 'smoke',
  name: 'Smoke',
  version: '1.0.0',
  testCases: [
    { testCaseId: 'add', name: 'Addition', input: '2+2', expected: '4' },
    { testCaseId: 'sub', input: '5-3', expected: '2' },
  ],
};

const verifier = { evaluatorId: 'exact', version: '1', kind: 'deterministic' };

async function openExperiment(client, options = {}) {
  return Experiment.start(client, { experimentId: 'run-1', name: 'smoke run', suite, ...options });
}

test('a trial with a local final score completes in the documented call order', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  await experiment.withTrial(suite.testCases[0], (trial) => {
    trial.finalScore(true, { evaluator: verifier, explanation: 'matched' });
  });
  await experiment.finalize('completed');

  assert.deepEqual(client.calls, [
    'upsert_experiment',
    'upsert_trial',
    'flush_generations',
    'export_scores',
    'update_trial',
    'finalize',
  ]);
  assert.equal(client.upserts[0].runId, 'run-1');
  assert.equal(client.upserts[0].suiteId, 'smoke');
  assert.equal(client.trials[0].status, 'running');
  assert.equal(client.trials[0].testCase.test_case_id, 'add');
  assert.equal(client.trialUpdates[0].status, 'completed');
  assert.equal(client.trialUpdates[0].error, '');
  assert.equal(client.finalized[0].status, 'completed');
  assert.equal(client.scores.length, 1);
  assert.equal(client.scores[0].scoreKey, 'final');
  assert.equal(client.scores[0].passed, true);
  assert.equal(client.scores[0].trialId, client.trials[0].trialId);
  assert.equal(client.scores[0].experimentId, 'run-1');
  assert.equal(client.scores[0].generationId, '', 'no anchor generation without recordIO');
});

test('a boolean final score sets the trial verdict', async () => {
  const cases = [
    { value: true, status: 'passed' },
    { value: false, status: 'failed' },
  ];
  for (const { value, status } of cases) {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.finalScore(value, { evaluator: verifier });
    await trial.close();
    assert.equal(trial.status, status, `value ${value}`);
    // The backend trial status is the lifecycle, not the verdict.
    assert.equal(client.trialUpdates[0].status, 'completed');
    await experiment.finalize();
  }
});

test('a numeric final score without a verdict closes completed', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(0.82, { evaluator: verifier });
  await trial.close();
  assert.equal(trial.status, 'completed');
  assert.equal(client.scores[0].passed, undefined);
});

test('a numeric final score with an explicit verdict keeps it', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(0.82, { passed: true, evaluator: verifier });
  await trial.close();
  assert.equal(trial.status, 'passed');
  assert.equal(client.scores[0].passed, true);
});

test('a trial closed without a final score fails with the documented error', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  await trial.close();

  assert.equal(trial.status, 'failed');
  assert.equal(trial.error, 'trial closed without a final score');
  assert.equal(client.trialUpdates[0].status, 'completed');
  assert.equal(client.trialUpdates[0].error, 'trial closed without a final score');
  assert.ok(!client.calls.includes('export_scores'), 'an empty buffer sends no score request');
});

test('a callback error terminalizes the trial and the run', async () => {
  const client = new FakeExperimentsClient();
  const failure = new Error('candidate failed');
  await assert.rejects(
    withExperiment(client, { experimentId: 'run-1', name: 'smoke run', suite }, async (experiment) => {
      await experiment.withTrial('add', () => {
        throw failure;
      });
    }),
    (error) => {
      assert.equal(error, failure);
      return true;
    },
  );

  assert.equal(client.trialUpdates.length, 1);
  assert.equal(client.trialUpdates[0].status, 'failed', 'the backend maps errored onto failed');
  assert.equal(client.trialUpdates[0].error, 'candidate failed');
  assert.equal(client.finalized.length, 1, 'the run is still finalized');
  assert.equal(client.finalized[0].status, 'failed');
  assert.equal(client.finalized[0].error, 'candidate failed');
});

test('a close failure keeps both errors when the callback threw a non-Error', async () => {
  // The other callback-error tests all throw an Error, so the path that used to
  // drop the close error was never exercised: a thrown string has no cause slot.
  const exportFailure = new Error('agento11y score export rejected 1 score(s): score-1: bad');
  const client = new FakeExperimentsClient({ exportScoresError: exportFailure });
  await assert.rejects(
    withExperiment(client, { experimentId: 'run-1', name: 'smoke run', suite }, async (experiment) => {
      await experiment.withTrial('add', (trial) => {
        trial.finalScore(true, { evaluator: verifier });
        throw 'boom';
      });
    }),
    (error) => {
      assert.equal(error, 'boom', 'the callback value is rethrown unchanged');
      return true;
    },
  );
  assert.ok(
    client.warnings.some((message) => message.includes('score export rejected')),
    `the score export failure must be reported, got ${JSON.stringify(client.warnings)}`,
  );
});

test('a callback error that already carries a cause still reports the close failure', async () => {
  const exportFailure = new Error('agento11y score export rejected 1 score(s): score-1: bad');
  const client = new FakeExperimentsClient({ exportScoresError: exportFailure });
  const rootCause = new Error('model refused');
  const callbackError = new Error('candidate failed', { cause: rootCause });
  const experiment = await openExperiment(client);
  await assert.rejects(
    experiment.withTrial('add', (trial) => {
      trial.finalScore(true, { evaluator: verifier });
      throw callbackError;
    }),
    (error) => {
      assert.equal(error, callbackError);
      return true;
    },
  );
  assert.equal(callbackError.cause, rootCause, 'the caller cause is not overwritten');
  assert.equal(rootCause.cause, exportFailure, 'the close failure joins the end of the chain');
  assert.ok(client.warnings.some((message) => message.includes('score export rejected')));
});

test('close reports the terminal patch failure and keeps the score export failure', async () => {
  const exportFailure = new Error('agento11y score export rejected 1 score(s): score-1: bad');
  const patchFailure = new Error('agento11y experiment transport failed: test case trial update: status 500');
  const client = new FakeExperimentsClient({ exportScoresError: exportFailure, updateTrialError: patchFailure });
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(true, { evaluator: verifier });
  await assert.rejects(trial.close(), (error) => {
    assert.equal(error, patchFailure);
    return true;
  });
  assert.equal(patchFailure.cause, exportFailure, 'the flush failure travels with the patch failure');
  assert.ok(client.warnings.some((message) => message.includes('score export rejected')));
  assert.equal(trial.isClosed, false, 'the scores stay buffered so a retry can publish them');
  assert.equal(trial.pendingScores.length, 1);
});

test('withExperiment finalizes completed on the success path', async () => {
  const client = new FakeExperimentsClient();
  const returned = await withExperiment(client, { experimentId: 'run-1', name: 'run', suite }, async (experiment) => {
    await experiment.withTrial('add', (trial) => {
      trial.finalScore(true, { evaluator: verifier });
    });
    return 'done';
  });
  assert.equal(returned, 'done');
  assert.equal(client.finalized[0].status, 'completed');
});

test('finalize closes trials still open', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(true, { evaluator: verifier });
  await experiment.finalize('completed');

  assert.equal(trial.isClosed, true);
  assert.equal(client.trialUpdates.length, 1);
  assert.equal(client.finalized.length, 1);
});

test('finalize runs once', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  await experiment.finalize('completed');
  await experiment.finalize('failed');
  assert.equal(client.finalized.length, 1);
  assert.equal(client.finalized[0].status, 'completed');
});

test('reusing a case and attempt is rejected', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const first = experiment.trial('add');
  await first.start();
  assert.throws(
    () => experiment.trial('add'),
    /trial for test case "add" attempt 1 already exists; increment attempt for a retry/,
  );
  // A retry with a new attempt is a different durable trial.
  const retry = experiment.trial('add', { attempt: 2 });
  assert.notEqual(retry.trialId, first.trialId);
});

test('an unbound trial mints no conversation until recordIO', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  assert.equal(trial.conversationId, '');
  trial.recordIO({ input: '2+2', output: '4' });
  assert.match(trial.conversationId, /^conv-[0-9a-f]{16}$/);
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();

  assert.deepEqual(
    client.calls,
    ['upsert_experiment', 'upsert_trial', 'export_generation', 'flush_generations', 'export_scores', 'update_trial'],
    'the anchor generation is exported before its scores',
  );
  assert.equal(client.generationCalls[0].inputText, '2+2');
  assert.equal(client.generationCalls[0].outputText, '4');
  assert.equal(client.generationCalls[0].conversationId, trial.conversationId);
  assert.equal(client.scores[0].generationId, trial.generationId);
});

test('bindConversation keeps the caller conversation', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.bindConversation(' conv-real ');
  trial.recordIO({ output: '4' });
  assert.equal(trial.conversationId, 'conv-real');
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();
  assert.equal(client.generationCalls[0].conversationId, 'conv-real');
  assert.equal(client.trialUpdates[0].conversationId, 'conv-real');
});

test('bindGeneration suppresses the anchor generation', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.bindGeneration('gen-external', { conversationId: 'conv-external' });
  trial.recordIO({ input: '2+2', output: '4' });
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();

  assert.ok(!client.calls.includes('export_generation'));
  assert.equal(client.scores[0].generationId, 'gen-external');
  assert.equal(client.scores[0].conversationId, 'conv-external');
});

test('a rejected anchor generation surfaces instead of being swallowed', async () => {
  const rejection = new Error('agento11y experiment transport failed: generation export rejected: bad conversation');
  const client = new FakeExperimentsClient({ exportGenerationError: rejection });
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.recordIO({ input: '2+2', output: '4' });
  trial.finalScore(true, { evaluator: verifier });
  await assert.rejects(trial.close(), (error) => {
    assert.equal(error, rejection);
    return true;
  });
  // The trial is still terminalized so the run does not hang on an open trial.
  assert.equal(client.trialUpdates.length, 1);
});

test('scores buffer until flush and keep their buffered order', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.checkScore('json_valid', { passed: true });
  trial.rubricScore('helpfulness', 0.9, { passed: true });
  trial.finalScore(true, { evaluator: verifier });
  assert.equal(trial.pendingScores.length, 3);
  assert.equal(client.scores.length, 0);

  const accepted = await trial.flush();
  assert.equal(accepted, 3);
  assert.equal(trial.pendingScores.length, 0);
  assert.deepEqual(
    client.scores.map((score) => score.scoreKey),
    ['json_valid', 'helpfulness', 'final'],
  );
  assert.equal(client.scores[0].evaluatorId, 'sdk.json_valid');
  assert.equal(client.scores[0].evaluatorKind, 'deterministic');
  assert.equal(client.scores[1].evaluatorKind, 'llm_judge');
  assert.equal(trial.acceptedScores, 3);
  assert.equal(experiment.acceptedScores, 3);

  // A second flush with an empty buffer sends nothing.
  const before = client.calls.filter((call) => call === 'export_scores').length;
  assert.equal(await trial.flush(), 0);
  assert.equal(client.calls.filter((call) => call === 'export_scores').length, before);
});

test('repeating one score key and evaluator yields distinct ids', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  const first = trial.score('accuracy', 1, { evaluator: verifier });
  const second = trial.score('accuracy', 0, { evaluator: verifier });
  assert.notEqual(first.scoreId, second.scoreId);
  assert.match(first.scoreId, /^score-[0-9a-f]{16}$/);
});

test('score metadata carries the trial identity', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add', { attempt: 3, metadata: { shard: 'a' } });
  await trial.start();
  trial.finalScore(true, { evaluator: verifier, metadata: { extra: 1 } });
  await trial.flush();
  assert.deepEqual(client.scores[0].metadata, {
    task_id: 'add',
    trial_id: trial.trialId,
    attempt: 3,
    shard: 'a',
    extra: 1,
  });
  assert.deepEqual(client.scores[0].source, { kind: 'experiment', id: 'run-1' });
});

test('setUsage reaches the terminal trial patch', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.setUsage({ inputTokens: 12, outputTokens: 34, cost: 0.02 });
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();
  assert.equal(client.trialUpdates[0].inputTokens, 12);
  assert.equal(client.trialUpdates[0].outputTokens, 34);
  assert.equal(client.trialUpdates[0].cost, 0.02);
  assert.ok(client.trialUpdates[0].durationMs >= 0);
});

test('bindTrace patches an already created trial', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  await trial.bindTrace('0af7651916cd43dd8448eb211c80319c', 'b7ad6b7169203331');
  assert.equal(client.trialUpdates[0].traceId, '0af7651916cd43dd8448eb211c80319c');
  assert.equal(client.trialUpdates[0].spanId, 'b7ad6b7169203331');
});

test('closing twice patches the trial once', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(true, { evaluator: verifier });
  await trial.close();
  await trial.close();
  assert.equal(client.trialUpdates.length, 1);
});

test('a trial opened from a ref works without a parent experiment', async () => {
  const client = new FakeExperimentsClient();
  const trial = Trial.fromRef(client, { experimentId: 'run-1', testCaseId: 'add', attempt: 1 });
  await trial.start();
  trial.finalScore(0.5, { passed: true, evaluator: verifier });
  await trial.close();
  assert.deepEqual(client.calls, ['upsert_trial', 'flush_generations', 'export_scores', 'update_trial']);
  assert.equal(client.scores[0].trialId, trial.trialId);
});

test('Trial.fromRef rejects a missing ref', () => {
  const client = new FakeExperimentsClient();
  assert.throws(
    () => Trial.fromRef(client, undefined),
    /trial ref is required; set AGENTO11Y_EXPERIMENT_ID and AGENTO11Y_TEST_CASE_ID/,
  );
});

test('a failed trial close forces the run to failed and drops the score count', async () => {
  const exportFailure = new Error('agento11y score export rejected 1 score(s): score-1: bad');
  const client = new FakeExperimentsClient({ exportScoresError: exportFailure });
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.finalScore(true, { evaluator: verifier });
  await assert.rejects(experiment.finalize('completed', { scoreCount: 1 }), (error) => {
    assert.equal(error, exportFailure);
    return true;
  });
  assert.equal(client.finalized[0].status, 'failed');
  assert.equal(client.finalized[0].scoreCount, undefined);
  assert.match(client.finalized[0].error, /^trial close failed: agento11y score export rejected/);
});

test('a caller score count reaches finalize when nothing was cloud evaluated', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  await experiment.withTrial('add', (trial) => {
    trial.finalScore(true, { evaluator: verifier });
  });
  await experiment.finalize('completed', { scoreCount: 1 });
  assert.equal(client.finalized[0].scoreCount, 1);
});

test('a plannedTrialCount below zero is rejected', async () => {
  const client = new FakeExperimentsClient();
  await assert.rejects(openExperiment(client, { plannedTrialCount: -1 }), /plannedTrialCount must be non-negative/);
});

test('an experiment without an id derives a stable one', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { name: 'nightly' });
  assert.match(experiment.experimentId, /^exp-[0-9a-f]{16}$/);
  assert.equal(experiment.name, 'nightly');
  assert.equal(client.upserts[0].runId, experiment.experimentId);
});

test('the experiment url comes from the client', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  assert.equal(experiment.url, 'http://ui/run-1');
});

test('finalize passes its status and error to the client unchanged', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  // Status validation lives in the real client and is covered by the client suite.
  await experiment.finalize('failed', { error: 'aborted by operator' });
  assert.equal(client.finalized[0].status, 'failed');
  assert.equal(client.finalized[0].error, 'aborted by operator');
});
