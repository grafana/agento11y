import assert from 'node:assert/strict';
import test from 'node:test';

import { DEFAULT_LLM_JUDGE_PROMPT, jsonObjects, LLMJudge, RegexJudge } from '../.test-dist/experiments/evaluators.js';
import { Experiment } from '../.test-dist/experiments/experiment.js';
import { stableId } from '../.test-dist/experiments/ids.js';
import { FakeExperimentsClient } from './experimentsFakeClient.mjs';

const suite = {
  suiteId: 'smoke',
  name: 'Smoke',
  version: '1.0.0',
  testCases: [{ testCaseId: 'add', input: '2+2', expected: '4' }],
};

function judge(response, options = {}) {
  return new LLMJudge({
    evaluatorId: 'judge.default',
    modelName: 'claude-test',
    modelProvider: 'anthropic',
    invoke: () => response,
    ...options,
  });
}

// Mirrors python/tests/test_experiments.py's judge parsing cases.
const parseCases = [
  {
    name: 'a plain scored object',
    response: '{"score": 0.9, "passed": true, "explanation": "correct"}',
    score: 0.9,
    passed: true,
    explanation: 'correct',
  },
  {
    name: 'unrelated JSON before the verdict',
    response:
      'I first considered {"candidate": "incomplete"}.\n{"score": 0.8, "passed": true, "explanation": "grounded"}',
    score: 0.8,
    passed: true,
    explanation: 'grounded',
  },
  {
    name: 'a top-level score beating a nested rubric score',
    response:
      '{"score": 0.9, "passed": true, "explanation": "overall", "rubric": {"score": 0.2, "passed": false, "explanation": "nested"}}',
    score: 0.9,
    passed: true,
    explanation: 'overall',
  },
  {
    name: 'a fenced code block',
    response: '```json\n{"score": 0.4, "explanation": "partial"}\n```',
    score: 0.4,
    passed: false,
    explanation: 'partial',
  },
  {
    name: 'a score outside the unit range, clamped',
    response: '{"score": 7, "explanation": "over"}',
    score: 1,
    passed: true,
    explanation: 'over',
  },
  {
    name: 'a negative score, clamped',
    response: '{"score": -2, "explanation": "under"}',
    score: 0,
    passed: false,
    explanation: 'under',
  },
  {
    name: 'a string verdict',
    response: '{"score": 0.2, "passed": "yes", "explanation": "generous"}',
    score: 0.2,
    passed: true,
    explanation: 'generous',
  },
  {
    name: 'the reason alias',
    response: '{"score": 0.5, "reason": "borderline"}',
    score: 0.5,
    passed: true,
    explanation: 'borderline',
  },
  {
    name: 'a numeric string score',
    response: '{"score": "0.6"}',
    score: 0.6,
    passed: true,
    explanation: '',
  },
  {
    name: 'braces inside a string value',
    response: '{"score": 0.3, "explanation": "saw {not json} in the output"}',
    score: 0.3,
    passed: false,
    explanation: 'saw {not json} in the output',
  },
];

test('LLMJudge parses the documented response shapes', async () => {
  for (const testCase of parseCases) {
    const result = await judge(testCase.response).evaluateOutput({ input: '2+2', output: '4', expected: '4' });
    assert.equal(result.value, testCase.score, testCase.name);
    assert.equal(result.passed, testCase.passed, testCase.name);
    assert.equal(result.explanation, testCase.explanation, testCase.name);
  }
});

test('LLMJudge rejects a response with no JSON object', async () => {
  await assert.rejects(
    judge('not structured').evaluateOutput({ input: '', output: 'answer' }),
    /LLM judge response did not contain a JSON object/,
  );
});

test('LLMJudge rejects a JSON object without a numeric score', async () => {
  await assert.rejects(
    judge('{"verdict": "good"}').evaluateOutput({ input: '', output: 'answer' }),
    /LLM judge response requires a numeric 'score'/,
  );
  await assert.rejects(
    judge('{"score": "not a number"}').evaluateOutput({ input: '', output: 'answer' }),
    /LLM judge response requires a numeric 'score'/,
  );
  await assert.rejects(
    judge('{"score": true}').evaluateOutput({ input: '', output: 'answer' }),
    /LLM judge response requires a numeric 'score'/,
  );
});

test('a malformed leading slice does not hide a later valid object', async () => {
  const result = await judge('{"score": ,}\n{"score": 0.7}').evaluateOutput({ input: '', output: 'answer' });
  assert.equal(result.value, 0.7);
});

test('the pass threshold decides when the judge omits a verdict', async () => {
  const strict = await judge('{"score": 0.6}', { passThreshold: 0.7 }).evaluateOutput({ input: '', output: 'a' });
  assert.equal(strict.passed, false);
  const lenient = await judge('{"score": 0.6}', { passThreshold: 0.5 }).evaluateOutput({ input: '', output: 'a' });
  assert.equal(lenient.passed, true);
});

test('placeholders render in one pass so a value cannot become a placeholder', async () => {
  const prompts = [];
  const rendering = judge('{"score": 1, "passed": true, "explanation": "ok"}', {
    promptTemplate: 'Input={input}; Output={output}; Expected={expected}',
    invoke: (prompt) => {
      prompts.push(prompt);
      return '{"score": 1, "passed": true, "explanation": "ok"}';
    },
  });
  await rendering.evaluateOutput({ input: '{output}', output: '{expected}', expected: 'secret-answer' });
  assert.deepEqual(prompts, ['Input={output}; Output={expected}; Expected=secret-answer']);
});

test('the default prompt keeps its JSON example intact after rendering', async () => {
  const prompts = [];
  const defaulted = new LLMJudge({
    evaluatorId: 'judge.default-prompt',
    modelName: 'test-model',
    invoke: (prompt) => {
      prompts.push(prompt);
      return '{"score": 1}';
    },
  });
  await defaulted.evaluateOutput({ input: '2+2', output: '4', expected: '4' });
  assert.ok(DEFAULT_LLM_JUDGE_PROMPT.includes('{"score": <number from 0 to 1>'));
  assert.ok(prompts[0].includes('{"score": <number from 0 to 1>'), 'the JSON example is not treated as a placeholder');
  assert.ok(prompts[0].includes('Candidate output:\n4'));
});

test('the grader generation carries the prompt, response, and usage', async () => {
  const withUsage = judge('{"score": 0.9, "passed": true}', {
    invoke: () => ({
      content: '{"score": 0.9, "passed": true}',
      usage: { input_tokens: 120, output_tokens: 18 },
    }),
  });
  const result = await withUsage.evaluateOutput({ input: '2+2', output: '4', expected: '4' });
  assert.equal(result.grader.modelName, 'claude-test');
  assert.equal(result.grader.modelProvider, 'anthropic');
  assert.equal(result.grader.agentName, 'agento11y-llm-judge');
  assert.equal(result.grader.operationName, 'llm-judge');
  assert.equal(result.grader.output, '{"score": 0.9, "passed": true}');
  assert.equal(result.grader.usage.inputTokens, 120);
  assert.equal(result.grader.usage.outputTokens, 18);
  assert.equal(result.grader.usage.totalTokens, 138);
  assert.deepEqual(result.metadata, { judge_model: 'claude-test', judge_provider: 'anthropic' });
  assert.equal(result.evaluator.kind, 'llm_judge');
});

test('usage is read from the common provider and framework shapes', async () => {
  const shapes = [
    { usage: { prompt_tokens: 5, completion_tokens: 7 } },
    { usage_metadata: { input_tokens: 5, output_tokens: 7 } },
    { response_metadata: { token_usage: { input_tokens: 5, output_tokens: 7 } } },
  ];
  for (const shape of shapes) {
    const result = await judge('', {
      invoke: () => ({ content: '{"score": 1}', ...shape }),
    }).evaluateOutput({ input: '', output: '' });
    assert.equal(result.grader.usage.inputTokens, 5, JSON.stringify(shape));
    assert.equal(result.grader.usage.outputTokens, 7, JSON.stringify(shape));
    assert.equal(result.grader.usage.totalTokens, 12, JSON.stringify(shape));
  }
});

test('a cache-write field is read from either provider spelling', async () => {
  const result = await judge('', {
    invoke: () => ({ content: '{"score": 1}', usage: { input_tokens: 1, cache_creation_input_tokens: 9 } }),
  }).evaluateOutput({ input: '', output: '' });
  assert.equal(result.grader.usage.cacheWriteInputTokens, 9);
});

test('a custom parser and usage extractor replace the defaults', async () => {
  const custom = judge('score=3', {
    parser: (raw) => ({ score: Number(raw.split('=')[1]) / 10, passed: true, explanation: 'custom' }),
    usageExtractor: () => ({ inputTokens: 1, outputTokens: 2, totalTokens: 3 }),
  });
  const result = await custom.evaluateOutput({ input: '', output: '' });
  assert.equal(result.value, 0.3);
  assert.equal(result.explanation, 'custom');
  assert.equal(result.grader.usage.totalTokens, 3);
});

test('a list response is joined into text', async () => {
  const result = await judge('', {
    invoke: () => ({ content: [{ text: '{"score": ' }, { text: '0.25}' }] }),
  }).evaluateOutput({ input: '', output: '' });
  assert.equal(result.value, 0.25);
});

test('LLMJudge validates its own configuration', () => {
  const base = { evaluatorId: 'judge', modelName: 'model', invoke: () => '' };
  assert.throws(() => new LLMJudge({ ...base, evaluatorId: '  ' }), /evaluatorId is required/);
  assert.throws(() => new LLMJudge({ ...base, invoke: undefined }), /invoke must be callable/);
  assert.throws(() => new LLMJudge({ ...base, modelName: '  ' }), /modelName is required/);
  assert.throws(() => new LLMJudge({ ...base, promptTemplate: '  ' }), /promptTemplate is required/);
  assert.throws(() => new LLMJudge({ ...base, passThreshold: 1.5 }), /passThreshold must be between 0 and 1/);
});

const regexCases = [
  { name: 'a search match', options: {}, pattern: '4', output: 'the answer is 4', passed: true },
  { name: 'a full match', options: { fullMatch: true }, pattern: '^4$', output: '4', passed: true },
  {
    name: 'a full match that only matches partially',
    options: { fullMatch: true },
    pattern: '^4$',
    output: 'x4',
    passed: false,
  },
  { name: 'a negated match', options: { negate: true }, pattern: '4', output: 'the answer is 4', passed: false },
  { name: 'a negated non-match', options: { negate: true }, pattern: '4', output: 'nothing here', passed: true },
  { name: 'a case-insensitive flag', options: { flags: 'i' }, pattern: 'ANSWER', output: 'answer', passed: true },
];

test('RegexJudge scores deterministically', () => {
  for (const testCase of regexCases) {
    const judgeUnderTest = new RegexJudge({
      evaluatorId: 'regex.answer',
      pattern: testCase.pattern,
      ...testCase.options,
    });
    const result = judgeUnderTest.evaluateOutput({ input: '2+2', output: testCase.output, expected: '4' });
    assert.equal(result.passed, testCase.passed, testCase.name);
    assert.equal(result.value, testCase.passed, testCase.name);
    assert.equal(result.evaluator.kind, 'deterministic');
    assert.equal(result.scoreKey, 'regex_match');
    assert.equal(result.metadata.pattern, testCase.pattern, testCase.name);
    assert.equal(result.grader, undefined, 'a deterministic judge publishes no grader transcript');
  }
});

test('RegexJudge explains its verdict and accepts an override', () => {
  const matched = new RegexJudge({ evaluatorId: 'regex.answer', pattern: '4' }).evaluateOutput({
    input: '',
    output: '4',
  });
  assert.equal(matched.explanation, 'output matched /4/');
  const missed = new RegexJudge({ evaluatorId: 'regex.answer', pattern: '4' }).evaluateOutput({
    input: '',
    output: 'five',
  });
  assert.equal(missed.explanation, 'output did not match /4/');
  const overridden = new RegexJudge({
    evaluatorId: 'regex.answer',
    pattern: '4',
    explanation: 'checked the arithmetic',
  }).evaluateOutput({ input: '', output: '4' });
  assert.equal(overridden.explanation, 'checked the arithmetic');
});

test('RegexJudge validates its own configuration', () => {
  assert.throws(() => new RegexJudge({ evaluatorId: '  ', pattern: '4' }), /evaluatorId is required/);
  assert.throws(() => new RegexJudge({ evaluatorId: 'r', pattern: '' }), /pattern is required/);
});

test('jsonObjects does not merge unrelated brace ranges', () => {
  assert.deepEqual(jsonObjects('{"a": 1} text {"b": 2}'), [{ a: 1 }, { b: 2 }]);
  assert.deepEqual(jsonObjects('no objects here'), []);
  assert.deepEqual(jsonObjects('{"a": {"b": 1}}'), [{ a: { b: 1 } }]);
});

// --- recordEvaluation ----------------------------------------------------- //

test('recordEvaluation publishes the grader and derives its ids from the score id', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'judged', suite });
  let recorded;
  await experiment.withTrial('add', async (trial) => {
    recorded = await trial.evaluateOutput(judge('{"score": 0.9, "passed": true, "explanation": "correct"}'), {
      input: '2+2',
      output: '4',
      expected: '4',
      scoreKey: 'final',
    });
  });

  assert.equal(recorded.scoreKey, 'final');
  assert.equal(recorded.value.number, 0.9);
  assert.equal(recorded.passed, true);
  assert.equal(recorded.graderGenerationId, stableId('gen', recorded.scoreId, 'grader'));
  assert.equal(recorded.graderConversationId, stableId('conv', recorded.scoreId, 'grader'));
  assert.equal(client.generations[0], recorded.graderGenerationId);
  assert.equal(client.generationCalls[0].conversationId, recorded.graderConversationId);
  assert.equal(client.generationCalls[0].operationName, 'llm-judge');
  assert.deepEqual(client.generationCalls[0].tags, {
    'experiment.run_id': 'run-1',
    'test.case.id': 'add',
    'test.case.attempt': '1',
    'evaluator.id': 'judge.default',
  });
  // The score reaches the export with its grader links attached.
  assert.equal(client.scores.length, 1);
  assert.equal(client.scores[0].graderGenerationId, recorded.graderGenerationId);
  assert.equal(client.scores[0].evaluatorKind, 'llm_judge');
});

test('a deterministic judge records no grader generation', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { experimentId: 'run-regex', name: 'regex', suite });
  let recorded;
  await experiment.withTrial('add', async (trial) => {
    recorded = await trial.evaluateOutput(
      new RegexJudge({ evaluatorId: 'regex.answer', pattern: '^4$', fullMatch: true }),
      {
        input: '2+2',
        output: '4',
        expected: '4',
        scoreKey: 'final',
      },
    );
  });
  assert.equal(recorded.passed, true);
  assert.equal(recorded.value.boolean, true);
  assert.equal(recorded.graderGenerationId, '');
  assert.deepEqual(client.generations, []);
});

test('publishGrader false keeps the grader transcript local', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { experimentId: 'run-1', name: 'judged', suite });
  let recorded;
  await experiment.withTrial('add', async (trial) => {
    recorded = await trial.evaluateOutput(judge('{"score": 0.9, "passed": true}'), {
      input: '2+2',
      output: '4',
      publishGrader: false,
    });
  });
  assert.equal(recorded.graderGenerationId, '');
  assert.deepEqual(client.generations, []);
});

test('recordEvaluation accepts a framework-produced result', async () => {
  const client = new FakeExperimentsClient();
  const experiment = await Experiment.start(client, { experimentId: 'run-framework', name: 'framework', suite });
  let recorded;
  await experiment.withTrial('add', async (trial) => {
    recorded = await trial.recordEvaluation({
      evaluator: { evaluatorId: 'harbor.rubric', version: '1', kind: 'llm_judge' },
      value: 0.8,
      passed: true,
      explanation: 'framework-owned trajectory passed',
      metadata: { harness: 'harbor' },
      grader: {
        input: 'harbor-rendered trajectory',
        output: '{"score": 0.8}',
        modelProvider: 'anthropic',
        modelName: 'claude-test',
        usage: { inputTokens: 30, outputTokens: 6, totalTokens: 36 },
      },
    });
  });
  assert.equal(recorded.evaluatorId, 'harbor.rubric');
  assert.notEqual(recorded.graderGenerationId, '');
  assert.equal(client.generationCalls[0].inputText, 'harbor-rendered trajectory');
  assert.equal(client.generationCalls[0].usage.totalTokens, 36);
  assert.equal(client.generationCalls[0].operationName, 'evaluate');
  assert.equal(client.scores[0].metadata.harness, 'harbor');
});
