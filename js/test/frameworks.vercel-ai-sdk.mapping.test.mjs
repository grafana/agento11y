import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildFrameworkMetadata,
  buildFrameworkTags,
  extractOutputSchemaTool,
  isTextChunk,
  mapInputMessages,
  mapModelFromStepStart,
  mapStepOutput,
  mapUsageFromStepFinish,
  normalizeMetadata,
  parseToolCallFinish,
  parseToolCallStart,
  resolveConversationId,
} from '../.test-dist/frameworks/vercel-ai-sdk/index.js';

test('vercel ai sdk mapping resolves provider from model and explicit provider', () => {
  const explicit = mapModelFromStepStart({
    model: {
      provider: 'anthropic',
      modelId: 'claude-sonnet-4-5',
    },
  });
  assert.equal(explicit.provider, 'anthropic');
  assert.equal(explicit.modelName, 'claude-sonnet-4-5');

  const inferred = mapModelFromStepStart({
    model: {
      modelId: 'gpt-5',
    },
  });
  assert.equal(inferred.provider, 'openai');
  assert.equal(inferred.modelName, 'gpt-5');

  const namespaced = mapModelFromStepStart({
    model: {
      provider: 'openai.responses',
      modelId: 'gpt-5',
    },
  });
  assert.equal(namespaced.provider, 'openai');

  const google = mapModelFromStepStart({
    model: {
      provider: 'google',
      modelId: 'gemini-2.5-pro',
    },
  });
  assert.equal(google.provider, 'gemini');
});

test('vercel ai sdk mapping extracts usage with cache and reasoning token details', () => {
  const usage = mapUsageFromStepFinish({
    usage: {
      inputTokens: 12,
      outputTokens: 6,
      totalTokens: 18,
      inputTokenDetails: {
        cacheReadTokens: 3,
        cacheWriteTokens: 2,
      },
      outputTokenDetails: {
        reasoningTokens: 4,
      },
    },
  });

  assert.equal(usage.inputTokens, 12);
  assert.equal(usage.outputTokens, 6);
  assert.equal(usage.totalTokens, 18);
  assert.equal(usage.cacheReadInputTokens, 3);
  assert.equal(usage.cacheWriteInputTokens, 2);
  assert.equal(usage.reasoningTokens, 4);
});

test('vercel ai sdk mapping remains zero-safe when usage detail fields are absent', () => {
  const usage = mapUsageFromStepFinish({
    usage: {
      inputTokenDetails: {},
      outputTokenDetails: {},
    },
  });

  assert.equal(usage.inputTokens, 0);
  assert.equal(usage.outputTokens, 0);
  assert.equal(usage.totalTokens, 0);
  assert.equal(usage.cacheReadInputTokens, 0);
  assert.equal(usage.cacheWriteInputTokens, 0);
  assert.equal(usage.reasoningTokens, 0);
});

test('vercel ai sdk mapping recognizes text chunks from onChunk event wrapper', () => {
  assert.equal(isTextChunk({ chunk: { type: 'text', text: 'hel' } }), true);
  assert.equal(isTextChunk({ chunk: { type: 'text-delta', text: 'hel' } }), true);
  assert.equal(isTextChunk({ type: 'text-delta' }), true);
  assert.equal(isTextChunk({ chunk: { type: 'reasoning', text: 'thinking' } }), false);
});

test('vercel ai sdk mapping applies conversation precedence explicit resolver fallback-seed', () => {
  const fromExplicit = resolveConversationId({
    explicitConversationId: 'explicit-conv',
    resolver: () => 'resolver-conv',
    stepStartEvent: {},
    fallbackSeed: 'seed-1',
  });
  assert.equal(fromExplicit.conversationId, 'explicit-conv');
  assert.equal(fromExplicit.source, 'explicit');

  const fromResolver = resolveConversationId({
    resolver: () => 'resolver-conv',
    stepStartEvent: {},
    fallbackSeed: 'seed-2',
  });
  assert.equal(fromResolver.conversationId, 'resolver-conv');
  assert.equal(fromResolver.source, 'resolver');

  const fromSeedFallback = resolveConversationId({
    stepStartEvent: {},
    fallbackSeed: 'seed-3',
  });
  assert.equal(fromSeedFallback.conversationId, 'agento11y:framework:vercel-ai-sdk:seed-3');
  assert.equal(fromSeedFallback.source, 'fallback');
});

test('vercel ai sdk mapping normalizes metadata and drops unsupported values', () => {
  const circular = {};
  circular.self = circular;
  const metadata = normalizeMetadata({
    ok: true,
    fn: () => 'skip',
    date: new Date('2026-02-23T00:00:00.000Z'),
    nested: {
      value: 1,
      circular,
    },
  });

  assert.equal(metadata.ok, true);
  assert.equal(metadata.fn, undefined);
  assert.equal(metadata.date, '2026-02-23T00:00:00.000Z');
  assert.equal(metadata.nested.value, 1);
  assert.equal(metadata.nested.circular.self, '[circular]');
});

test('vercel ai sdk mapping converts rich input/output payloads', () => {
  const input = mapInputMessages([
    {
      role: 'user',
      content: [
        { type: 'text', text: 'hello' },
        { type: 'tool-call', toolName: 'weather', toolCallId: 'call-1', input: { city: 'Paris' } },
      ],
    },
    {
      role: 'tool',
      name: 'weather',
      content: '18c',
    },
  ]);
  assert.equal(input.length, 2);
  assert.equal(input[0].parts.length, 2);
  assert.equal(input[0].parts[1].type, 'tool_call');
  assert.equal(input[1].name, 'weather');
  assert.equal(input[1].role, 'tool');
  assert.equal(input[1].content, '18c');

  const output = mapStepOutput({
    text: 'done',
    stepType: 'tool-result',
    reasoningText: 'reasoned',
    toolResults: [{ toolCallId: 'call-1', toolName: 'weather', output: { temp_c: 18 } }],
  });
  assert.equal(output.stepType, 'tool-result');
  assert.equal(output.reasoningText, 'reasoned');
  assert.equal(output.output.length, 2);
  assert.equal(output.output[1].parts[0].type, 'tool_result');
});

test('vercel ai sdk mapping infers step type when step finish payload omits it', () => {
  const initial = mapStepOutput({
    stepNumber: 0,
    text: 'first',
  });
  assert.equal(initial.stepType, 'initial');

  const continuation = mapStepOutput({
    stepNumber: 2,
    text: 'next',
  });
  assert.equal(continuation.stepType, 'continue');

  const toolResult = mapStepOutput({
    stepNumber: 1,
    toolResults: [{ toolCallId: 'call-1', toolName: 'weather', output: { temp_c: 18 } }],
  });
  assert.equal(toolResult.stepType, 'tool-result');
});

test('vercel ai sdk mapping parses tool lifecycle events', () => {
  const start = parseToolCallStart({
    toolCall: {
      toolCallId: 'call-1',
      toolName: 'weather',
      input: { city: 'Paris' },
      type: 'function',
      description: 'Weather lookup',
    },
  });
  assert.equal(start.toolCallId, 'call-1');
  assert.equal(start.toolName, 'weather');

  const finish = parseToolCallFinish({
    toolCall: {
      toolCallId: 'call-1',
    },
    success: true,
    output: { temp_c: 18 },
    durationMs: 120,
  });
  assert.equal(finish.toolCallId, 'call-1');
  assert.equal(finish.success, true);
  assert.equal(finish.durationMs, 120);
});

test('extractOutputSchemaTool returns ToolDefinition for Output.object-style responseFormat', async () => {
  const schema = {
    type: 'object',
    properties: { question: { type: 'string' }, skills: { type: 'array', items: { type: 'string' } } },
    required: ['question', 'skills'],
  };
  const output = {
    name: 'object',
    responseFormat: Promise.resolve({ type: 'json', schema, name: 'person', description: 'A person record' }),
  };

  const tool = await extractOutputSchemaTool(output);
  assert.ok(tool);
  assert.equal(tool.name, 'person');
  assert.equal(tool.type, 'output_schema');
  assert.equal(tool.description, 'A person record');
  assert.deepEqual(JSON.parse(tool.inputSchemaJSON), schema);
});

test('extractOutputSchemaTool falls back to Output.name when responseFormat omits name', async () => {
  const schema = { type: 'object', properties: { value: { type: 'number' } } };
  const tool = await extractOutputSchemaTool({
    name: 'object',
    responseFormat: { type: 'json', schema },
  });
  assert.ok(tool);
  assert.equal(tool.name, 'object');
  assert.equal(tool.type, 'output_schema');
  assert.equal(tool.description, undefined);
  assert.deepEqual(JSON.parse(tool.inputSchemaJSON), schema);
});

test('extractOutputSchemaTool returns undefined for missing or non-json output', async () => {
  assert.equal(await extractOutputSchemaTool(undefined), undefined);
  assert.equal(await extractOutputSchemaTool(null), undefined);
  assert.equal(await extractOutputSchemaTool({}), undefined);
  assert.equal(await extractOutputSchemaTool({ responseFormat: { type: 'text' } }), undefined);
  assert.equal(await extractOutputSchemaTool({ responseFormat: { type: 'json' } }), undefined);
});

test('extractOutputSchemaTool fails open when responseFormat promise rejects', async () => {
  const tool = await extractOutputSchemaTool({
    responseFormat: Promise.reject(new Error('schema unavailable')),
  });
  assert.equal(tool, undefined);
});

test('extractOutputSchemaTool fails open when schema cannot be serialized', async () => {
  const circular = { type: 'object' };
  circular.self = circular;
  const tool = await extractOutputSchemaTool({
    responseFormat: { type: 'json', schema: circular },
  });
  assert.equal(tool, undefined);
});

test('vercel ai sdk mapping includes canonical framework identity in tags and metadata', () => {
  const tags = buildFrameworkTags({ env: 'test' });
  const metadata = buildFrameworkMetadata({ trace: 'abc' }, 'initial', 'reasoned');

  assert.equal(tags.env, 'test');
  assert.equal(tags['agento11y.framework.name'], 'vercel-ai-sdk');
  assert.equal(tags['agento11y.framework.source'], 'framework');
  assert.equal(tags['agento11y.framework.language'], 'typescript');

  assert.equal(metadata.trace, 'abc');
  assert.equal(metadata['agento11y.framework.name'], 'vercel-ai-sdk');
  assert.equal(metadata['agento11y.framework.source'], 'framework');
  assert.equal(metadata['agento11y.framework.language'], 'typescript');
  assert.equal(metadata['agento11y.framework.step_type'], 'initial');
  assert.equal(metadata['agento11y.framework.reasoning_text'], 'reasoned');
});
