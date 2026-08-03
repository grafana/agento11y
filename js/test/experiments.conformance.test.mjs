// Checks the JS experiments SDK against the shared wire fixtures in
// `conformance/experiments/`. Go and Python run the same fixtures; see
// `conformance/experiments/README.md`.

import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';

import { ExperimentsClient } from '../.test-dist/experiments/client.js';
import { Experiment } from '../.test-dist/experiments/experiment.js';
import { ENV_ENABLE_EXPERIMENTAL_FEATURES } from '../.test-dist/experiments/experimental.js';
import { stableId } from '../.test-dist/experiments/ids.js';
import { parseExperimentReport, parseTrialEvaluation } from '../.test-dist/experiments/models.js';
import { diffJson, loadIds, loadInputs, loadRequests, loadResponses } from './experimentsFixtures.mjs';

const inputs = loadInputs();
const requests = loadRequests();
const responses = loadResponses();

test('stable ids match the shared vectors', () => {
  for (const vector of loadIds().vectors) {
    assert.equal(
      stableId(vector.prefix, ...vector.parts),
      vector.id,
      `${vector.prefix} ${JSON.stringify(vector.parts)}`,
    );
  }
});

test('run upsert matches the fixture', async () => {
  const captured = await capture(responses.run_upsert_response, (client) =>
    client.upsertExperiment({
      runId: inputs.experiment_id,
      name: inputs.experiment_name,
      tags: inputs.tags,
      suiteId: inputs.suite_id,
      suiteVersion: inputs.suite_version,
      plannedTrialCount: inputs.planned_trial_count,
    }),
  );
  assertMatches(captured[0], requests.run_upsert);
});

test('trial create matches the fixture', async () => {
  const captured = await capture({ trial_id: trialId() }, (client) =>
    client.upsertTrial(inputs.experiment_id, {
      trialId: trialId(),
      testCaseId: inputs.test_case_id,
      attempt: inputs.attempt,
    }),
  );
  assertMatches(captured[0], requests.trial_create);
});

test('the conversation patch matches the fixture', async () => {
  const captured = await capture({ trial_id: trialId() }, (client) =>
    client.updateTrial(inputs.experiment_id, trialId(), { conversationId: inputs.conversation_id }),
  );
  assertMatches(captured[0], requests.trial_patch_conversation);
});

test('the terminal patch matches the fixture', async () => {
  const captured = await capture({ trial_id: trialId() }, (client) =>
    client.updateTrial(inputs.experiment_id, trialId(), {
      status: 'completed',
      conversationId: inputs.conversation_id,
    }),
  );
  assertMatches(captured[0], requests.trial_patch_terminal);
});

test('the evaluate trigger matches the fixture, with and without a version', async () => {
  await withGate(async () => {
    const pinned = await capture(responses.evaluation_queued, (client) =>
      client.triggerTrialEvaluation(inputs.experiment_id, trialId(), inputs.evaluator_id, inputs.evaluator_version),
    );
    assertMatches(pinned[0], requests.trial_evaluate);

    const latest = await capture(responses.evaluation_queued, (client) =>
      client.triggerTrialEvaluation(inputs.experiment_id, trialId(), inputs.evaluator_id),
    );
    assertMatches(latest[0], requests.trial_evaluate_latest_version);
  });
});

test('a trial id with reserved characters keeps the route verb', async () => {
  await withGate(async () => {
    const captured = await capture(responses.evaluation_queued, (client) =>
      client.triggerTrialEvaluation(inputs.experiment_id, 'trial:one/blue', inputs.evaluator_id),
    );
    assertMatches(captured[0], requests.trial_evaluate_reserved_trial_id);
  });
});

test('the evaluation status read matches the fixture', async () => {
  await withGate(async () => {
    const captured = await capture(responses.evaluation_success, (client) =>
      client.getTrialEvaluation(inputs.experiment_id, trialId(), inputs.evaluation_id),
    );
    assert.equal(captured[0].method, requests.trial_evaluation_status.method);
    assert.equal(captured[0].path, requests.trial_evaluation_status.path);
    assert.equal(captured[0].body, '', 'a status read sends no body');
  });
});

test('score export matches the fixture', async () => {
  const captured = await capture(responses.scores_export_response, (client) =>
    client.exportScores([
      {
        scoreId: stableId('score', inputs.experiment_id, trialId(), inputs.score_key, inputs.score_evaluator_id),
        evaluatorId: inputs.score_evaluator_id,
        evaluatorVersion: inputs.score_evaluator_version,
        // Local-only on purpose: no SDK puts the evaluator kind on the wire.
        evaluatorKind: 'deterministic',
        scoreKey: inputs.score_key,
        value: { boolean: true },
        conversationId: inputs.conversation_id,
        experimentId: inputs.experiment_id,
        trialId: trialId(),
        testCaseId: inputs.test_case_id,
        passed: true,
        explanation: 'matched the expected answer',
        createdAt: new Date(inputs.score_created_at),
        source: { kind: 'experiment', id: inputs.experiment_id },
      },
    ]),
  );
  assertMatches(captured[0], requests.scores_export);
});

test('run finalize matches the fixture, with and without a score count', async () => {
  const plain = await capture(responses.run_finalize_response, (client) =>
    client.finalize(inputs.experiment_id, 'completed'),
  );
  assertMatches(plain[0], requests.run_finalize);

  const counted = await capture(responses.run_finalize_response, (client) =>
    client.finalize(inputs.experiment_id, 'completed', { scoreCount: 1 }),
  );
  assertMatches(counted[0], requests.run_finalize_with_score_count);
});

test('the canned evaluation responses parse into the same logical result', () => {
  // Every suite checks this field list on these fixtures, so a misparse in one SDK
  // cannot pass while the others check a different subset.
  for (const [name, status] of [
    ['evaluation_queued', 'queued'],
    ['evaluation_claimed', 'claimed'],
    ['evaluation_success', 'success'],
    ['evaluation_failed', 'failed'],
  ]) {
    const parsed = parseTrialEvaluation(responses[name]);
    assert.equal(parsed.status, status, name);
    assert.equal(parsed.evaluationId, inputs.evaluation_id, name);
    assert.equal(parsed.experimentId, inputs.experiment_id, name);
    assert.equal(parsed.trialId, trialId(), name);
    assert.equal(parsed.conversationId, inputs.conversation_id, name);
    assert.equal(parsed.evaluatorId, inputs.evaluator_id, name);
    assert.equal(parsed.evaluatorVersion, inputs.evaluator_version, name);
  }

  const queued = parseTrialEvaluation(responses.evaluation_queued);
  assert.equal(queued.attempts, 0);
  assert.equal(queued.testCaseId, inputs.test_case_id);

  const success = parseTrialEvaluation(responses.evaluation_success);
  assert.equal(success.attempts, 1);
  assert.equal(success.testCaseId, inputs.test_case_id);
  assert.equal(success.createdAt.toISOString(), '2026-01-01T00:00:00.000Z');
  assert.equal(success.updatedAt.toISOString(), '2026-01-01T00:00:05.000Z');

  const failed = parseTrialEvaluation(responses.evaluation_failed);
  assert.equal(failed.error, 'grader crashed');
  assert.equal(failed.attempts, 3);
});

test('the two responses that must fail do fail', () => {
  assert.throws(
    () => parseTrialEvaluation(responses.evaluation_unsupported_status),
    /unsupported evaluation status "paused"/,
  );
  assert.throws(
    () => parseTrialEvaluation(responses.evaluation_missing_id),
    /evaluation response carries no evaluation_id/,
  );
});

test('both report envelopes parse alike', () => {
  const fromExperiment = parseExperimentReport(responses.report_experiment_envelope);
  const fromRun = parseExperimentReport(stripComment(responses.report_run_envelope));
  assert.deepEqual(fromExperiment, fromRun);
  assert.equal(fromExperiment.run.runId, inputs.experiment_id);
  assert.equal(fromExperiment.summary.trialCount, 1);
  assert.equal(fromExperiment.summary.passRate, undefined, 'a stored evaluator leaves the pass rate unset');
});

test('one cloud-evaluated trial produces the pinned request sequence', async () => {
  await withGate(async () => {
    const seen = [];
    const bodies = [
      responses.run_upsert_response,
      { trial_id: trialId() },
      { trial_id: trialId() },
      responses.evaluation_queued,
      responses.evaluation_success,
      { trial_id: trialId() },
      responses.run_finalize_response,
    ];
    const { endpoint, close } = await startServer(seen, () => bodies.shift() ?? {});
    try {
      const client = new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {}, sleep: async () => {} });
      const experiment = await Experiment.start(client, {
        experimentId: inputs.experiment_id,
        name: inputs.experiment_name,
        tags: inputs.tags,
        plannedTrialCount: inputs.planned_trial_count,
        suite: { suiteId: inputs.suite_id, version: inputs.suite_version, testCases: [] },
      });
      await experiment.withTrial(inputs.test_case_id, async (trial) => {
        trial.bindConversation(inputs.conversation_id);
        await trial.evaluate(inputs.evaluator_id, { evaluatorVersion: inputs.evaluator_version });
      });
      await experiment.finalize('completed', { scoreCount: 1 });

      assert.deepEqual(
        seen.map((entry) => `${entry.method} ${entry.path}`),
        [
          'POST /api/v1/experiment-runs:upsert',
          `POST /api/v1/experiment-runs/${inputs.experiment_id}/trials`,
          `PATCH /api/v1/experiment-runs/${inputs.experiment_id}/trials/${trialId()}`,
          `POST /api/v1/experiment-runs/${inputs.experiment_id}/trials/${trialId()}:evaluate`,
          `GET /api/v1/experiment-runs/${inputs.experiment_id}/trials/${trialId()}/evaluations/${inputs.evaluation_id}`,
          `PATCH /api/v1/experiment-runs/${inputs.experiment_id}/trials/${trialId()}`,
          `POST /api/v1/experiment-runs/${inputs.experiment_id}:finalize`,
        ],
        'the Go and Python order plus one status read, because this fake answers :evaluate with queued',
      );
      assert.ok(
        !seen.some((entry) => entry.path === '/api/v1/scores:export'),
        'a cloud-evaluated trial writes no local score',
      );
      // The high-level wrapper adds one field the low-level fixture does not: it
      // stamps the suite identity into the run's metadata for grouping.
      assertMatches(seen[0], {
        ...requests.run_upsert,
        body: {
          ...requests.run_upsert.body,
          metadata: { suite_id: inputs.suite_id, suite_version: inputs.suite_version },
        },
      });
      assertMatches(seen[2], requests.trial_patch_conversation);
      assertMatches(seen[3], requests.trial_evaluate);
      // The caller's score count is dropped once a trial queued an evaluation.
      assertMatches(seen[6], requests.run_finalize);
    } finally {
      await close();
    }
  });
});

function trialId() {
  return stableId('trial', inputs.experiment_id, inputs.test_case_id, inputs.attempt);
}

function assertMatches(captured, fixture) {
  assert.equal(captured.method, fixture.method, 'method');
  assert.equal(captured.path, fixture.path, 'encoded path');
  const differences = diffJson(JSON.parse(captured.body), fixture.body);
  assert.deepEqual(differences, [], `body differs from the fixture:\n${differences.join('\n')}`);
}

function stripComment(payload) {
  const { comment, ...rest } = payload;
  return rest;
}

async function capture(responseBody, run) {
  const seen = [];
  const { endpoint, close } = await startServer(seen, () => responseBody);
  try {
    await run(new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} }));
  } finally {
    await close();
  }
  return seen;
}

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

async function startServer(seen, respond) {
  const server = http.createServer((request, response) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      const [path, query] = request.url.split('?');
      seen.push({
        method: request.method,
        path,
        query: query ?? '',
        headers: request.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      });
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify(respond(request) ?? {}));
    });
  });
  await new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', (error) => (error ? reject(error) : resolve(undefined)));
  });
  const { port } = server.address();
  return {
    endpoint: `http://127.0.0.1:${port}`,
    close: () => new Promise((resolve) => server.close(() => resolve(undefined))),
  };
}
