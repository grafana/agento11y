import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';

import { ExperimentsClient } from '../.test-dist/experiments/client.js';
import { TrialEvaluationFailedError, TrialEvaluationTimeoutError } from '../.test-dist/experiments/errors.js';
import { Experiment } from '../.test-dist/experiments/experiment.js';
import { ENV_ENABLE_EXPERIMENTAL_FEATURES } from '../.test-dist/experiments/experimental.js';
import { evaluation, FakeExperimentsClient } from './experimentsFakeClient.mjs';

const suite = {
  suiteId: 'smoke',
  name: 'Smoke',
  version: '1.0.0',
  testCases: [{ testCaseId: 'add', input: '2+2', expected: '4' }],
};

/** Runs `fn` with the experimental gate open, restoring the previous value. */
async function withGate(fn) {
  const previous = process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES];
  process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES] = 'true';
  try {
    return await fn();
  } finally {
    if (previous === undefined) {
      delete process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES];
    } else {
      process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES] = previous;
    }
  }
}

async function openExperiment(client) {
  return Experiment.start(client, { experimentId: 'run-1', name: 'cloud eval', suite });
}

test('a successful evaluation follows the pinned call order', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    client.evaluationStatuses = [evaluation('queued'), evaluation('queued'), evaluation('success')];
    client.evaluationResult = evaluation('queued');
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    const result = await trial.evaluate('helpfulness', { pollIntervalMs: 500 });
    assert.equal(result.status, 'success');
    assert.equal(result.evaluationId, 'eval-123');
    assert.deepEqual(client.evaluationOrder, ['update', 'flush', 'trigger', 'status', 'status', 'status']);
    assert.deepEqual(client.triggeredEvaluations, [
      { experimentId: 'run-1', trialId: trial.trialId, evaluatorId: 'helpfulness', evaluatorVersion: '' },
    ]);
    assert.equal(client.trialUpdates[0].conversationId, 'conv-real');
    assert.ok(!client.calls.includes('export_scores'), 'a cloud-evaluated trial writes no local score');

    await trial.close();
    assert.equal(trial.status, 'completed', 'no local final score is still a completed trial');
    assert.equal(trial.error, '');

    await experiment.finalize('completed', { scoreCount: 3 });
    assert.equal(client.finalized[0].scoreCount, undefined, 'a caller score count is dropped');
    assert.ok(!client.calls.includes('export_scores'));
  });
});

test('recordIO exports the anchor generation before the trigger', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.recordIO({ input: '2+2', output: '4' });

    await trial.evaluate('helpfulness');
    assert.deepEqual(client.evaluationOrder, ['update', 'generation', 'flush', 'trigger']);
    assert.equal(client.generationCalls[0].conversationId, trial.conversationId);
  });
});

test('a failed worker rejects with the evaluation id and detail', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    client.evaluationResult = evaluation('failed', { error: 'grader crashed' });
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    await assert.rejects(trial.evaluate('helpfulness'), (error) => {
      assert.ok(error instanceof TrialEvaluationFailedError);
      assert.equal(error.name, 'TrialEvaluationFailedError');
      assert.equal(error.evaluationId, 'eval-123');
      assert.equal(error.detail, 'grader crashed');
      assert.equal(error.message, 'agento11y trial evaluation eval-123 failed: grader crashed');
      return true;
    });
    assert.equal(trial.isCloudEvaluated, false);
  });
});

test('a blank detail and a blank id still produce a readable message', () => {
  assert.equal(new TrialEvaluationFailedError('eval-1', '   ').message, 'agento11y trial evaluation eval-1 failed');
  assert.equal(
    new TrialEvaluationTimeoutError('', 'waited 5ms').message,
    'agento11y trial evaluation unknown timed out: waited 5ms',
  );
});

test('the poll interval doubles to the ceiling and the deadline stops the wait', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    // Every read stays queued, so only the deadline can end the loop.
    client.evaluationResult = evaluation('queued');
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    await assert.rejects(trial.evaluate('helpfulness', { timeoutMs: 30_000, pollIntervalMs: 500 }), (error) => {
      assert.ok(error instanceof TrialEvaluationTimeoutError);
      assert.equal(error.evaluationId, 'eval-123');
      assert.equal(error.message, 'agento11y trial evaluation eval-123 timed out: waited 30000ms');
      return true;
    });
    assert.deepEqual(client.sleeps.slice(0, 5), [500, 1000, 2000, 4000, 5000]);
    assert.ok(
      client.sleeps.every((delay) => delay <= 5000),
      'the interval never passes the 5000 ms ceiling',
    );
    // The clamped last sleep exactly consumes the remaining budget.
    assert.equal(
      client.sleeps.reduce((total, delay) => total + delay, 0),
      30_000,
    );
  });
});

test('a caller interval above the ceiling is preserved', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    client.evaluationResult = evaluation('queued');
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    await assert.rejects(
      trial.evaluate('helpfulness', { timeoutMs: 60_000, pollIntervalMs: 20_000 }),
      TrialEvaluationTimeoutError,
    );
    assert.deepEqual(client.sleeps, [20_000, 20_000, 20_000]);
  });
});

test('a sleep clamped to the remaining budget is followed by one final status read', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    // Queued, queued, then success on the read after the clamped sleep.
    client.evaluationStatuses = [evaluation('queued'), evaluation('success')];
    client.evaluationResult = evaluation('queued');
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    const result = await trial.evaluate('helpfulness', { timeoutMs: 600, pollIntervalMs: 500 });
    assert.equal(result.status, 'success');
    // 500 then a 100 ms clamp; the read after the clamp is what saw success.
    assert.deepEqual(client.sleeps, [500, 100]);
    assert.deepEqual(client.evaluationOrder, ['update', 'flush', 'trigger', 'status', 'status']);
  });
});

test('an aborted signal stops polling and rethrows its own reason', async () => {
  await withGate(async () => {
    const stop = new Error('stop');
    const controller = new AbortController();
    const client = new FakeExperimentsClient({
      onSleep: () => {
        controller.abort(stop);
      },
    });
    client.evaluationResult = evaluation('queued');
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');

    await assert.rejects(trial.evaluate('helpfulness', { signal: controller.signal }), (error) => {
      assert.equal(error, stop, 'not wrapped as a timeout or transport error');
      return true;
    });
    assert.equal(client.sleeps.length, 1, 'no further delay is scheduled');
    assert.deepEqual(client.evaluationOrder, ['update', 'flush', 'trigger']);
  });
});

test('an already aborted signal stops before any request', async () => {
  await withGate(async () => {
    const stop = new Error('too late');
    const controller = new AbortController();
    controller.abort(stop);
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');
    const callsBefore = client.calls.length;

    await assert.rejects(trial.evaluate('helpfulness', { signal: controller.signal }), (error) => {
      assert.equal(error, stop);
      return true;
    });
    assert.equal(client.calls.length, callsBefore);
  });
});

test('evaluate validates its inputs before touching the network', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    const callsBefore = client.calls.length;

    await assert.rejects(
      trial.evaluate('helpfulness'),
      /agento11y trial evaluation validation failed: bind a conversation first/,
    );
    trial.bindConversation('conv-real');
    await assert.rejects(
      trial.evaluate('   '),
      /agento11y trial evaluation validation failed: evaluator_id is required/,
    );
    await assert.rejects(
      trial.evaluate('helpfulness', { timeoutMs: 0 }),
      /agento11y trial evaluation validation failed: timeoutMs must be greater than zero/,
    );
    await assert.rejects(
      trial.evaluate('helpfulness', { pollIntervalMs: -1 }),
      /agento11y trial evaluation validation failed: pollIntervalMs must be greater than zero/,
    );
    assert.equal(client.calls.length, callsBefore);
  });
});

test('an evaluation error still closes the trial as errored', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    client.evaluationResult = evaluation('failed', { error: 'grader crashed' });
    const experiment = await openExperiment(client);
    await assert.rejects(
      experiment.withTrial('add', async (trial) => {
        trial.bindConversation('conv-real');
        await trial.evaluate('helpfulness');
      }),
      TrialEvaluationFailedError,
    );
    assert.equal(client.trialUpdates.at(-1).status, 'failed');
    assert.match(client.trialUpdates.at(-1).error, /^agento11y trial evaluation eval-123 failed: grader crashed$/);
  });
});

test('a callback error after a successful evaluation closes the trial as errored', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const failure = new Error('assertion failed after grading');
    await assert.rejects(
      experiment.withTrial('add', async (trial) => {
        trial.bindConversation('conv-real');
        await trial.evaluate('helpfulness');
        throw failure;
      }),
      (error) => {
        assert.equal(error, failure);
        return true;
      },
    );
    // The cloud verdict does not swallow an error raised after it.
    assert.equal(client.trialUpdates.at(-1).status, 'failed');
    assert.equal(client.trialUpdates.at(-1).error, 'assertion failed after grading');
  });
});

test('an evaluated trial still accepts a local score, and finalize keeps dropping the count', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    await experiment.withTrial('add', async (trial) => {
      trial.bindConversation('conv-real');
      await trial.evaluate('helpfulness');
      trial.checkScore('json_valid', { passed: true });
    });
    await experiment.finalize('completed', { scoreCount: 1 });
    assert.equal(client.scores.length, 1);
    assert.equal(client.finalized[0].scoreCount, undefined);
    assert.equal(experiment.hasCloudEvaluatedTrial, true);
  });
});

test('an evaluator version reaches the trigger', async () => {
  await withGate(async () => {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');
    await trial.evaluate('helpfulness', { evaluatorVersion: '3' });
    assert.equal(client.triggeredEvaluations[0].evaluatorVersion, '3');
  });
});

// --- the gate ------------------------------------------------------------- //

test('a closed gate blocks trial.evaluate without sending a request', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await openExperiment(client);
  const trial = experiment.trial('add');
  await trial.start();
  trial.bindConversation('conv-real');
  const callsBefore = client.calls.length;

  // Unset, not falsy: the absent case is the one the gate has to cover.
  assert.equal(process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES], undefined);
  await assert.rejects(trial.evaluate('helpfulness'), (error) => {
    assert.equal(error.name, 'ExperimentalFeatureDisabledError');
    assert.equal(error.feature, 'cloud trial evaluation');
    assert.equal(error.envVar, 'AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES');
    return true;
  });
  assert.equal(client.calls.length, callsBefore);
});

test('a closed gate blocks both client entry points before any HTTP request', async () => {
  const seen = [];
  const server = http.createServer((request, response) => {
    seen.push(request.url);
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end('{}');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const endpoint = `http://127.0.0.1:${server.address().port}`;
  try {
    const client = new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} });
    assert.equal(process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES], undefined);
    for (const call of [
      () => client.triggerTrialEvaluation('run-1', 'trial-1', 'helpfulness'),
      () => client.getTrialEvaluation('run-1', 'trial-1', 'eval-1'),
    ]) {
      await assert.rejects(call(), (error) => {
        assert.equal(error.name, 'ExperimentalFeatureDisabledError');
        assert.match(
          error.message,
          /^agento11y: cloud trial evaluation is experimental; set AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true to use it$/,
        );
        return true;
      });
    }
    assert.deepEqual(seen, [], 'zero HTTP requests reach the server');
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
});

test('an empty gate value is as closed as an unset one', async () => {
  process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES] = '';
  try {
    const client = new FakeExperimentsClient();
    const experiment = await openExperiment(client);
    const trial = experiment.trial('add');
    await trial.start();
    trial.bindConversation('conv-real');
    await assert.rejects(trial.evaluate('helpfulness'), /is experimental/);
  } finally {
    delete process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES];
  }
});

// --- the real transport --------------------------------------------------- //

test('the trigger and status routes carry the wire body and encoded ids', async () => {
  await withGate(async () => {
    const seen = [];
    const server = http.createServer((request, response) => {
      const chunks = [];
      request.on('data', (chunk) => chunks.push(chunk));
      request.on('end', () => {
        seen.push({ method: request.method, url: request.url, body: Buffer.concat(chunks).toString('utf8') });
        response.writeHead(200, { 'content-type': 'application/json' });
        response.end(JSON.stringify({ evaluation_id: 'eval-1', status: 'queued', attempts: 1 }));
      });
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const endpoint = `http://127.0.0.1:${server.address().port}`;
    try {
      const client = new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} });
      const triggered = await client.triggerTrialEvaluation('run-1', 'trial:one', 'helpfulness', '2');
      assert.equal(triggered.status, 'queued');
      assert.equal(seen[0].method, 'POST');
      assert.equal(seen[0].url, '/api/v1/experiment-runs/run-1/trials/trial%3Aone:evaluate');
      assert.deepEqual(JSON.parse(seen[0].body), { evaluator_id: 'helpfulness', evaluator_version: '2' });

      await client.getTrialEvaluation('run-1', 'trial:one', 'eval-1');
      assert.equal(seen[1].method, 'GET');
      assert.equal(seen[1].url, '/api/v1/experiment-runs/run-1/trials/trial%3Aone/evaluations/eval-1');
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });
});

test('an unrecognized status is a transport error, not another poll', async () => {
  await withGate(async () => {
    const server = http.createServer((_request, response) => {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ evaluation_id: 'eval-1', status: 'paused' }));
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const endpoint = `http://127.0.0.1:${server.address().port}`;
    try {
      const client = new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} });
      await assert.rejects(
        client.triggerTrialEvaluation('run-1', 'trial-1', 'helpfulness'),
        /agento11y experiment transport failed: unsupported evaluation status "paused"/,
      );
      await assert.rejects(
        client.getTrialEvaluation('run-1', 'trial-1', 'eval-1'),
        /agento11y experiment transport failed: unsupported evaluation status "paused"/,
      );
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });
});

test('a response without an evaluation id is rejected', async () => {
  await withGate(async () => {
    const server = http.createServer((_request, response) => {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ status: 'queued' }));
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const endpoint = `http://127.0.0.1:${server.address().port}`;
    try {
      await assert.rejects(
        new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} }).triggerTrialEvaluation(
          'run-1',
          'trial-1',
          'helpfulness',
        ),
        /evaluation response carries no evaluation_id/,
      );
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  });
});

test('the client rejects blank evaluation identifiers before requesting', async () => {
  await withGate(async () => {
    const client = new ExperimentsClient({ endpoint: 'http://127.0.0.1:1', ingestToken: 'tok', env: {} });
    await assert.rejects(
      client.triggerTrialEvaluation('  ', 'trial-1', 'helpfulness'),
      /agento11y trial evaluation validation failed: experiment_id is required/,
    );
    await assert.rejects(
      client.triggerTrialEvaluation('run-1', '  ', 'helpfulness'),
      /agento11y trial evaluation validation failed: trial_id is required/,
    );
    await assert.rejects(
      client.triggerTrialEvaluation('run-1', 'trial-1', '  '),
      /agento11y trial evaluation validation failed: evaluator_id is required/,
    );
    await assert.rejects(
      client.getTrialEvaluation('run-1', 'trial-1', '  '),
      /agento11y trial evaluation validation failed: evaluation_id is required/,
    );
  });
});
