import assert from 'node:assert/strict';
import test from 'node:test';

import { publishPromptfooResults } from '../.test-dist/integrations/promptfoo/index.js';
import { FakeExperimentsClient } from './experimentsFakeClient.mjs';

test('publishes Promptfoo aggregate and named scores with report roles', async () => {
  const client = new FakeExperimentsClient();
  const published = await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'promptfoo-run-1',
      results: [
        {
          id: 'result-1',
          testIdx: 0,
          promptIdx: 0,
          success: true,
          score: 0.9,
          latencyMs: 12,
          vars: { question: 'What is 2+2?' },
          provider: { id: 'file://provider.mjs', label: 'deterministic-agent' },
          response: { output: '4' },
          gradingResult: { reason: 'All assertions passed' },
          namedScores: { exact_match: 1, concise: 0.8 },
          tokenUsage: { prompt: 4, completion: 1 },
        },
      ],
    },
    { client },
  );

  assert.equal(published.runs[0].experimentId, 'promptfoo-run-1');
  assert.equal(published.trialCount, 1);
  assert.equal(published.scoreCount, 3);
  assert.equal(client.upserts[0].metadata.framework, 'promptfoo');
  assert.equal(client.generations.length, 1);
  assert.equal(client.generationCalls[0].inputTokens, 4);
  assert.equal(client.trialUpdates[0].error, '');
  assert.equal(client.trialUpdates[0].durationMs, 12);
  assert.deepEqual(
    client.scores.map(({ scoreKey, reportRole, passed }) => ({ scoreKey, reportRole, passed })),
    [
      { scoreKey: 'promptfoo', reportRole: 'primary_verdict', passed: true },
      { scoreKey: 'exact_match', reportRole: 'diagnostic', passed: undefined },
      { scoreKey: 'concise', reportRole: 'diagnostic', passed: undefined },
    ],
  );
});

test('publishes each Promptfoo prompt/provider candidate as a separate comparable run', async () => {
  const client = new FakeExperimentsClient();
  const result = {
    testIdx: 0,
    promptIdx: 0,
    success: true,
    score: 1,
    provider: { id: 'openai:gpt-5', label: 'gpt-5' },
    prompt: { raw: '{{question}}', label: 'prompt-v1' },
  };
  const published = await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'comparison',
      results: [result, { ...result, promptIdx: 1, prompt: { raw: 'Answer: {{question}}', label: 'prompt-v2' } }],
    },
    { client, recordIO: false, candidate: { agentName: 'checkout-agent', agentVersion: 'v2' } },
  );

  assert.equal(published.runs.length, 2);
  assert.equal(client.upserts.length, 2);
  assert.deepEqual(
    client.upserts.map((run) => run.candidate.prompt_version),
    ['prompt-v1', 'prompt-v2'],
  );
  assert.ok(client.upserts.every((run) => run.candidate.agent_name === 'checkout-agent'));
  assert.deepEqual(
    client.trials.map((trial) => trial.testCaseId),
    [client.trials[0].testCaseId, client.trials[0].testCaseId],
    'the same test aligns across candidate runs',
  );
});

test('publishes native test definitions once and pins every candidate to the same suite version', async () => {
  const client = new FakeExperimentsClient();
  const suites = [];
  const result = {
    testIdx: 0,
    promptIdx: 0,
    success: true,
    score: 1,
    vars: { question: '2+2?', locale: 'en' },
    testCase: { assert: [{ type: 'equals', value: '4' }] },
  };
  await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'stored',
      results: [result, { ...result, promptIdx: 1, vars: { locale: 'en', question: '2+2?' } }],
    },
    {
      client,
      recordIO: false,
      testSuitesClient: {
        async pushSuite(suite, options) {
          suites.push(suite);
          assert.equal(options.publish, true);
          return { suiteVersion: 'v9', suite };
        },
      },
      suitePublicationPolicy: 'subset',
    },
  );
  assert.equal(suites.length, 1);
  assert.deepEqual(suites[0].testCases[0].input, { vars: { question: '2+2?', locale: 'en' } });
  assert.deepEqual(suites[0].tags, ['framework:promptfoo']);
  assert.deepEqual(
    client.upserts.map((run) => run.suiteVersion),
    ['v9', 'v9'],
  );
});

test('uses distinct run ids for labeled variants of the same provider', async () => {
  const client = new FakeExperimentsClient();
  const base = { testIdx: 0, promptIdx: 0, success: true, score: 1, provider: { id: 'openai:gpt-5' } };
  const published = await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'collision',
      results: [
        { ...base, provider: { ...base.provider, label: 'temperature-0' } },
        { ...base, provider: { ...base.provider, label: 'temperature-1' } },
      ],
    },
    { client, recordIO: false },
  );

  assert.equal(new Set(published.runs.map(({ experimentId }) => experimentId)).size, 2);
});

test('keeps provider errors unrated and preserves native latency', async () => {
  const client = new FakeExperimentsClient();
  await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'native-error',
      results: [
        {
          testIdx: 0,
          promptIdx: 0,
          success: false,
          score: 0,
          error: 'provider timed out',
          failureReason: 2,
          latencyMs: 12345,
        },
        { testIdx: 1, promptIdx: 0, success: false, score: 0, failureReason: 1 },
        { testIdx: 2, promptIdx: 0, success: false, score: 0, failureReason: 2 },
      ],
    },
    { client, recordIO: false },
  );

  assert.equal(client.scores.length, 1, 'the valid assertion failure remains rated');
  assert.equal(client.scores[0].passed, false);
  assert.equal(client.trialUpdates[0].status, 'failed');
  assert.equal(client.trialUpdates[0].error, 'provider timed out');
  assert.equal(client.trialUpdates[0].durationMs, 12345);
  assert.equal('durationMs' in client.trialUpdates[1], false, 'missing native latency stays unknown');
  assert.equal(client.trialUpdates[2].error, 'Promptfoo provider error');
});

test('uses the server-returned canonical case and records transformation provenance', async () => {
  const client = new FakeExperimentsClient();
  await publishPromptfooResults(
    {
      suite: {},
      prompts: [],
      config: {},
      evalId: 'canonical',
      results: [{ testIdx: 0, promptIdx: 0, success: true, score: 1, vars: { email: 'person@example.com' } }],
    },
    {
      client,
      recordIO: false,
      suitePublicationPolicy: 'subset',
      testSuitesClient: {
        async pushSuite(suite) {
          return {
            suiteVersion: 'v2',
            suite: { ...suite, testCases: suite.testCases.map((testCase) => ({ ...testCase, input: { vars: {} } })) },
          };
        },
      },
    },
  );

  assert.deepEqual(client.trials[0].testCase.input, { vars: {} });
  assert.equal(client.trials[0].metadata.agento11y_case_transformed, true);
  assert.equal(JSON.stringify(client.trials[0].testCase).includes('person@example.com'), false);
});

test('uses attempts for repeated Promptfoo rows with the same explicit case id', async () => {
  const client = new FakeExperimentsClient();
  const base = {
    testIdx: 0,
    promptIdx: 0,
    success: true,
    score: 1,
    metadata: { 'agento11y.test_case_id': 'capital-fr' },
  };
  await publishPromptfooResults(
    { suite: {}, prompts: [], config: {}, evalId: 'repeated', results: [base, { ...base, id: 'repeat-2' }] },
    { client, recordIO: false },
  );

  assert.deepEqual(
    client.trials.map(({ testCaseId, attempt }) => ({ testCaseId, attempt })),
    [
      { testCaseId: 'capital-fr', attempt: 1 },
      { testCaseId: 'capital-fr', attempt: 2 },
    ],
  );
});

test('rejects conflicting explicit case definitions before writing', async () => {
  const client = new FakeExperimentsClient();
  const base = {
    testIdx: 0,
    promptIdx: 0,
    success: true,
    score: 1,
    metadata: { 'agento11y.test_case_id': 'stable-case' },
  };

  await assert.rejects(
    publishPromptfooResults(
      {
        suite: {},
        prompts: [],
        config: {},
        evalId: 'conflicting-case',
        results: [
          { ...base, vars: { question: 'first' } },
          { ...base, vars: { question: 'changed' } },
        ],
      },
      { client, recordIO: false },
    ),
    /Conflicting Promptfoo definitions/,
  );
  assert.equal(client.upserts.length, 0);
});

test('uses explicit model identity instead of Promptfoo provider ids and display labels', async () => {
  const base = {
    testIdx: 0,
    promptIdx: 0,
    success: true,
    score: 1,
    provider: { id: 'openai:chat:gpt-4o-mini', label: 'baseline' },
    response: { output: 'ok' },
  };
  for (const { result, candidate, provider, name } of [
    { result: base, provider: '', name: '' },
    {
      result: base,
      candidate: { modelProvider: 'openai', modelName: 'gpt-4o-mini' },
      provider: 'openai',
      name: 'gpt-4o-mini',
    },
    {
      result: {
        ...base,
        response: {
          ...base.response,
          metadata: {
            'agento11y.model_provider': 'anthropic',
            'agento11y.model_name': 'claude-sonnet',
          },
        },
      },
      provider: 'anthropic',
      name: 'claude-sonnet',
    },
  ]) {
    const client = new FakeExperimentsClient();
    await publishPromptfooResults(
      { suite: {}, prompts: [], config: {}, evalId: 'model-identity', results: [result] },
      { client, candidate },
    );
    assert.equal(client.upserts[0].candidate.model_provider ?? '', provider);
    assert.equal(client.upserts[0].candidate.model_name ?? '', name);
    assert.equal(client.generationCalls[0].modelProvider, provider);
    assert.equal(client.generationCalls[0].modelName, name);
    assert.equal(client.scores[0].metadata.promptfoo_provider_label, 'baseline');
  }
});
