import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';

import { DEFAULT_INGEST_ACTOR, ExperimentsClient, INGEST_ACTOR_HEADER } from '../.test-dist/experiments/client.js';

test('a bearer-auth client sends the canonical actor header and sdk source', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { run: { experiment_id: 'run-1' } },
  }));
  try {
    const client = new ExperimentsClient({ endpoint, ingestToken: 'tok-1', env: {} });
    const experiment = await client.upsertExperiment({ runId: 'run-1', name: 'nightly', tags: ['a'] });
    assert.equal(experiment.runId, 'run-1');
    assert.equal(seen[0].headers.authorization, 'Bearer tok-1');
    assert.equal(seen[0].headers[INGEST_ACTOR_HEADER.toLowerCase()], DEFAULT_INGEST_ACTOR);
    assert.equal(seen[0].headers['x-sigil-ingest-actor'], undefined, 'the legacy header name is gone');
    assert.equal(seen[0].headers['x-scope-orgid'], undefined);
    assert.deepEqual(JSON.parse(seen[0].body), {
      name: 'nightly',
      source: { kind: 'sdk', id: 'js' },
      experiment_id: 'run-1',
      tags: ['a'],
    });
  } finally {
    await close();
  }
});

test('a tenant client sends basic auth and the tenant header', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    const client = new ExperimentsClient({ endpoint, tenantId: '12345', ingestToken: 'tok-2', env: {} });
    await client.upsertExperiment({ name: 'nightly' });
    assert.equal(seen[0].headers['x-scope-orgid'], '12345');
    assert.equal(seen[0].headers.authorization, `Basic ${Buffer.from('12345:tok-2').toString('base64')}`);
  } finally {
    await close();
  }
});

test('the client reads endpoint, token, tenant, and actor from the environment', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    const client = new ExperimentsClient({
      env: {
        AGENTO11Y_ENDPOINT: endpoint,
        AGENTO11Y_AUTH_TOKEN: 'tok-env',
        AGENTO11Y_INGEST_ACTOR: 'ingest:runner/nightly',
        AGENTO11Y_GRAFANA_URL: 'https://stack.grafana.net/',
      },
    });
    assert.equal(client.actor, 'ingest:runner/nightly');
    assert.equal(client.grafanaUrl, 'https://stack.grafana.net');
    await client.upsertExperiment({ name: 'nightly' });
    assert.equal(seen[0].headers[INGEST_ACTOR_HEADER.toLowerCase()], 'ingest:runner/nightly');
  } finally {
    await close();
  }
});

test('a missing endpoint or token fails at construction', () => {
  assert.throws(() => new ExperimentsClient({ env: {} }), /endpoint is required/);
  assert.throws(() => new ExperimentsClient({ endpoint: 'http://localhost:1', env: {} }), /ingestToken is required/);
});

test('finalize maps succeeded onto completed and rejects other statuses', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    const client = newClient(endpoint);
    await client.finalize('run-1', 'succeeded', { scoreCount: 4, error: '' });
    assert.equal(seen[0].url, '/api/v1/experiment-runs/run-1:finalize');
    assert.deepEqual(JSON.parse(seen[0].body), {
      status: 'completed',
      source: { kind: 'sdk', id: 'js' },
      score_count: 4,
    });
    await assert.rejects(
      client.finalize('run-1', 'canceled'),
      /agento11y experiment validation failed: status must be completed or failed/,
    );
    await assert.rejects(client.finalize('  ', 'completed'), /run_id is required/);
  } finally {
    await close();
  }
});

test('trial create and update send only the fields the caller set', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: { trial_id: 'trial-1' } }));
  try {
    const client = newClient(endpoint);
    await client.upsertTrial('run-1', { trialId: 'trial-1', testCaseId: 'add', attempt: 2 });
    assert.equal(seen[0].method, 'POST');
    assert.equal(seen[0].url, '/api/v1/experiment-runs/run-1/trials');
    assert.deepEqual(JSON.parse(seen[0].body), {
      trial_id: 'trial-1',
      test_case_id: 'add',
      attempt: 2,
      status: 'running',
    });

    await client.updateTrial('run-1', 'trial-1', { conversationId: 'conv-1' });
    assert.equal(seen[1].method, 'PATCH');
    assert.equal(seen[1].url, '/api/v1/experiment-runs/run-1/trials/trial-1');
    assert.deepEqual(JSON.parse(seen[1].body), { conversation_id: 'conv-1' });
  } finally {
    await close();
  }
});

test('score export sends the wire body and counts duplicates as recorded', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: {
      results: [
        { score_id: 'score-1', accepted: true },
        { score_id: 'score-2', accepted: false, status: 'duplicate' },
      ],
    },
  }));
  try {
    const client = newClient(endpoint);
    const recorded = await client.exportScores([
      {
        scoreId: 'score-1',
        evaluatorId: 'exact',
        evaluatorVersion: '1',
        evaluatorKind: 'check',
        scoreKey: 'final',
        value: { number: 0.5 },
        trialId: 'trial-1',
        experimentId: 'run-1',
        passed: true,
        explanation: 'ok',
        metadata: { attempt: 1 },
        source: { kind: 'experiment', id: 'run-1' },
      },
      {
        scoreId: 'score-2',
        evaluatorId: 'exact',
        evaluatorVersion: '1',
        scoreKey: 'final',
        value: { boolean: true },
        trialId: 'trial-1',
      },
    ]);
    assert.equal(recorded, 2, 'one fresh plus one idempotent duplicate');
    assert.equal(seen[0].url, '/api/v1/scores:export');
    const body = JSON.parse(seen[0].body);
    // No `evaluator_kind`: it stays local, matching Go's and Python's score body.
    assert.deepEqual(body.scores[0], {
      score_id: 'score-1',
      evaluator_id: 'exact',
      evaluator_version: '1',
      score_key: 'final',
      value: { number: 0.5 },
      trial_id: 'trial-1',
      experiment_id: 'run-1',
      passed: true,
      explanation: 'ok',
      metadata: { attempt: 1 },
      source: { kind: 'experiment', id: 'run-1' },
    });
    assert.deepEqual(body.scores[1].value, { bool: true });
  } finally {
    await close();
  }
});

test('an empty score list sends no request', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    assert.equal(await newClient(endpoint).exportScores([]), 0);
    assert.equal(seen.length, 0);
  } finally {
    await close();
  }
});

test('a rejected score raises with the backend detail', async () => {
  const { endpoint, close } = await startServer(() => ({
    status: 200,
    body: { results: [{ score_id: 'score-1', accepted: false, error: 'unknown trial' }] },
  }));
  try {
    await assert.rejects(
      newClient(endpoint).exportScores([
        {
          scoreId: 'score-1',
          evaluatorId: 'exact',
          evaluatorVersion: '1',
          scoreKey: 'final',
          value: { boolean: true },
          trialId: 'trial-1',
        },
      ]),
      /agento11y score export rejected 1 score\(s\): score-1: unknown trial/,
    );
  } finally {
    await close();
  }
});

test('a score missing an anchor is rejected before the request', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    await assert.rejects(
      newClient(endpoint).exportScores([
        { scoreId: 's', evaluatorId: 'e', evaluatorVersion: '1', scoreKey: 'final', value: { boolean: true } },
      ]),
      /agento11y score validation failed: generation_id or trial_id is required/,
    );
    assert.equal(seen.length, 0);
  } finally {
    await close();
  }
});

test('generation export checks acceptance and tolerates a duplicate', async () => {
  const responses = [
    { results: [{ generation_id: 'gen-1', accepted: true }] },
    { results: [{ generation_id: 'gen-1', accepted: false, error: 'generation already exists' }] },
    { results: [{ generation_id: 'gen-1', accepted: false, error: 'conversation not found' }] },
    { results: [] },
  ];
  let index = 0;
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: responses[index++] }));
  try {
    const client = newClient(endpoint);
    assert.equal(await client.exportGeneration({ generationId: 'gen-1', conversationId: 'conv-1' }), 'gen-1');
    assert.equal(seen[0].url, '/api/v1/generations:export');
    const generation = JSON.parse(seen[0].body).generations[0];
    assert.equal(generation.id, 'gen-1');
    assert.equal(generation.conversation_id, 'conv-1');
    assert.equal(generation.mode, 'GENERATION_MODE_SYNC');
    assert.deepEqual(generation.model, { provider: 'eval', name: 'experiment' });

    // A duplicate is the idempotent retry of the same anchor generation.
    assert.equal(await client.exportGeneration({ generationId: 'gen-1' }), 'gen-1');
    await assert.rejects(
      client.exportGeneration({ generationId: 'gen-1' }),
      /transport failed: generation export rejected: conversation not found/,
    );
    await assert.rejects(
      client.exportGeneration({ generationId: 'gen-1' }),
      /transport failed: generation export: response did not include a result/,
    );
  } finally {
    await close();
  }
});

test('generation export shapes input, output, and usage', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { results: [{ generation_id: 'gen-1', accepted: true }] },
  }));
  try {
    await newClient(endpoint).exportGeneration({
      generationId: 'gen-1',
      conversationId: 'conv-1',
      inputText: 'what is 2+2?',
      outputText: '4',
      modelProvider: 'openai',
      modelName: 'gpt-5',
      agentName: 'calc',
      inputTokens: 10,
      outputTokens: 2,
      tags: { 'experiment.run_id': 'run-1' },
      metadata: { attempt: 1 },
    });
    const generation = JSON.parse(seen[0].body).generations[0];
    assert.deepEqual(generation.input, [{ role: 'MESSAGE_ROLE_USER', parts: [{ text: 'what is 2+2?' }] }]);
    assert.deepEqual(generation.output, [{ role: 'MESSAGE_ROLE_ASSISTANT', parts: [{ text: '4' }] }]);
    assert.deepEqual(generation.usage, {
      input_tokens: 10,
      output_tokens: 2,
      total_tokens: 12,
      cache_read_input_tokens: 0,
      cache_write_input_tokens: 0,
      reasoning_tokens: 0,
    });
    assert.equal(generation.agent_name, 'calc');
  } finally {
    await close();
  }
});

test('a zero-token generation omits usage', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { results: [{ generation_id: 'gen-1', accepted: true }] },
  }));
  try {
    await newClient(endpoint).exportGeneration({ generationId: 'gen-1' });
    assert.equal(JSON.parse(seen[0].body).generations[0].usage, undefined);
  } finally {
    await close();
  }
});

test('score explanations are redacted by default and left alone when disabled', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { results: [{ score_id: 'score-1', accepted: true }] },
  }));
  const secret = 'token is glc_abcdefghijklmnopqrstuvwxyz012345';
  const score = {
    scoreId: 'score-1',
    evaluatorId: 'exact',
    evaluatorVersion: '1',
    scoreKey: 'final',
    value: { boolean: true },
    trialId: 'trial-1',
    explanation: secret,
  };
  try {
    await newClient(endpoint).exportScores([score]);
    assert.match(JSON.parse(seen[0].body).scores[0].explanation, /\[REDACTED:grafana-cloud-token\]/);

    await new ExperimentsClient({ endpoint, ingestToken: 'tok', redactSecrets: false, env: {} }).exportScores([score]);
    assert.equal(JSON.parse(seen[1].body).scores[0].explanation, secret);
  } finally {
    await close();
  }
});

test('score metadata is redacted through a null-prototype object and a class instance', async () => {
  // The recursion used to stop at anything whose prototype was not exactly
  // Object.prototype, so these two shapes carried their secrets through.
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { results: [{ score_id: 'score-1', accepted: true }] },
  }));
  const secret = 'glc_abcdefghijklmnopqrstuvwxyz012345';
  class Detail {
    constructor(note) {
      this.note = note;
    }
  }
  const bare = Object.create(null);
  bare.note = `bare ${secret}`;
  const score = {
    scoreId: 'score-1',
    evaluatorId: 'exact',
    evaluatorVersion: '1',
    scoreKey: 'final',
    value: { boolean: true },
    trialId: 'trial-1',
    metadata: { bare, instance: new Detail(`instance ${secret}`) },
  };
  try {
    await newClient(endpoint).exportScores([score]);
    const sent = JSON.parse(seen[0].body).scores[0].metadata;
    assert.match(sent.bare.note, /\[REDACTED:grafana-cloud-token\]/);
    assert.match(sent.instance.note, /\[REDACTED:grafana-cloud-token\]/);
  } finally {
    await close();
  }
});

test('artifact upload sends the metadata query and the raw bytes', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: { artifact_id: 'art-1' } }));
  try {
    const record = await newClient(endpoint).uploadArtifact({
      experimentId: 'run-1',
      parentId: 'trial-1',
      name: 'transcript.md',
      kind: 'markdown',
      mime: 'text/markdown',
      content: new TextEncoder().encode('# notes'),
    });
    assert.deepEqual(record, { artifact_id: 'art-1' });
    assert.equal(
      seen[0].url,
      '/api/v1/experiment-runs/run-1/trials/trial-1/artifacts:upload?name=transcript.md&kind=markdown&mime=text%2Fmarkdown',
    );
    assert.equal(seen[0].headers['content-type'], 'text/markdown');
    assert.equal(seen[0].body, '# notes');
  } finally {
    await close();
  }
});

test('artifact upload rejects an unsupported parent and empty content', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    const client = newClient(endpoint);
    await assert.rejects(
      client.uploadArtifact({
        experimentId: 'run-1',
        parentId: 'trial-1',
        name: 'a',
        kind: 'text',
        content: new Uint8Array(),
        parentKind: 'experiment',
      }),
      /only test_case_trial artifacts are supported/,
    );
    await assert.rejects(
      client.uploadArtifact({
        experimentId: 'run-1',
        parentId: 'trial-1',
        name: 'a',
        kind: 'text',
        content: new Uint8Array(),
      }),
      /content is required/,
    );
    assert.equal(seen.length, 0);
  } finally {
    await close();
  }
});

test('the report endpoint accepts either envelope', async () => {
  const summary = { trial_count: 2, completed_count: 2, pass_count: 1, pass_denominator: 2, pass_rate: 0.5 };
  const bodies = [
    { experiment: { experiment_id: 'run-1', name: 'nightly' }, summary, rows: [{ test_case_id: 'add' }] },
    { run: { experiment_id: 'run-1', name: 'nightly' }, summary, rows: [{ test_case_id: 'add' }] },
  ];
  let index = 0;
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: bodies[index++] }));
  try {
    const client = newClient(endpoint);
    const fromExperiment = await client.getReport('run-1');
    const fromRun = await client.getReport('run-1');
    assert.equal(seen[0].url, '/api/v1/eval/experiments/run-1/report');
    assert.deepEqual(fromExperiment, fromRun);
    assert.equal(fromExperiment.run.runId, 'run-1');
    assert.equal(fromExperiment.summary.passRate, 0.5);
    assert.equal(fromExperiment.rows.length, 1);
  } finally {
    await close();
  }
});

test('an omitted pass rate stays undefined instead of becoming zero', async () => {
  const { endpoint, close } = await startServer(() => ({
    status: 200,
    body: { experiment: { experiment_id: 'run-1' }, summary: { trial_count: 1, completed_count: 1 } },
  }));
  try {
    const report = await newClient(endpoint).getReport('run-1');
    assert.equal(report.summary.passRate, undefined);
    assert.equal(report.summary.finalScoreAvg, undefined);
    assert.equal(report.summary.trialCount, 1);
  } finally {
    await close();
  }
});

test('score listing defaults the page size and normalizes the cursor', async () => {
  const bodies = [
    { items: [{ score_id: 'score-1' }], next_cursor: 'abc' },
    { items: [], next_cursor: '0' },
  ];
  let index = 0;
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: bodies[index++] }));
  try {
    const client = newClient(endpoint);
    const first = await client.listScores('run-1');
    assert.equal(seen[0].url, '/api/v1/eval/experiments/run-1/scores?limit=50');
    assert.equal(first.nextCursor, 'abc');
    const second = await client.listScores('run-1', { limit: 10, cursor: 'abc' });
    assert.equal(seen[1].url, '/api/v1/eval/experiments/run-1/scores?limit=10&cursor=abc');
    assert.equal(second.nextCursor, undefined, 'a zero cursor means no more pages');
  } finally {
    await close();
  }
});

test('the experiment url prefers the grafana url', () => {
  const withGrafana = new ExperimentsClient({
    endpoint: 'https://ao.example',
    ingestToken: 'tok',
    grafanaUrl: 'https://stack.grafana.net',
    env: {},
  });
  assert.equal(
    withGrafana.experimentUrl('run 1'),
    'https://stack.grafana.net/a/grafana-agento11y-app/experiments/runs/run%201',
  );
  const withoutGrafana = new ExperimentsClient({ endpoint: 'https://ao.example', ingestToken: 'tok', env: {} });
  assert.equal(
    withoutGrafana.experimentUrl('run-1'),
    'https://ao.example/a/grafana-agento11y-app/experiments/runs/run-1',
  );
});

test('an attached core client is flushed and shut down', async () => {
  let flushes = 0;
  const client = new ExperimentsClient({
    endpoint: 'http://localhost:1',
    ingestToken: 'tok',
    env: {},
    coreClient: {
      async flush() {
        flushes++;
      },
    },
  });
  await client.flushGenerations();
  await client.shutdown();
  assert.equal(flushes, 2);
});

test('useExperimentalOtel follows its environment variable', () => {
  const base = { endpoint: 'http://localhost:1', ingestToken: 'tok' };
  assert.equal(new ExperimentsClient({ ...base, env: {} }).useExperimentalOtel, false);
  assert.equal(
    new ExperimentsClient({ ...base, env: { AGENTO11Y_USE_EXPERIMENTAL_OTEL: 'true' } }).useExperimentalOtel,
    true,
  );
  assert.equal(
    new ExperimentsClient({ ...base, env: { AGENTO11Y_USE_EXPERIMENTAL_OTEL: 'true' }, useExperimentalOtel: false })
      .useExperimentalOtel,
    false,
  );
});

function newClient(endpoint) {
  return new ExperimentsClient({ endpoint, ingestToken: 'tok', env: {} });
}

async function startServer(respond) {
  const seen = [];
  const server = http.createServer((request, response) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      seen.push({
        method: request.method,
        url: request.url,
        headers: request.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      });
      const { status, body } = respond(request);
      response.writeHead(status, { 'content-type': 'application/json' });
      response.end(JSON.stringify(body ?? {}));
    });
  });
  await new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', (error) => (error ? reject(error) : resolve(undefined)));
  });
  const { port } = server.address();
  return {
    endpoint: `http://127.0.0.1:${port}`,
    seen,
    close: () => new Promise((resolve) => server.close(() => resolve(undefined))),
  };
}
