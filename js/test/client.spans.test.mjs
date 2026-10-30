import assert from 'node:assert/strict';
import test from 'node:test';
import { context, SpanStatusCode, trace } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

class CapturingExporter {
  requests = [];

  async exportGenerations(request) {
    this.requests.push(structuredClone(request));
    return {
      results: request.generations.map((generation) => ({
        generationId: generation.id,
        accepted: true,
      })),
    };
  }
}

test('overlapping async generations parent bound tools without an ambient context manager', async () => {
  const harness = newHarness({ debugBufferSize: 0 });
  let release;
  const ready = new Promise((resolve) => {
    release = resolve;
  });
  const recorders = [];
  const activeContext = context.active();

  try {
    const pending = ['startGeneration', 'startStreamingGeneration'].map((method, index) =>
      harness.client[method](
        { id: `generation-${index}`, model: { provider: 'openai', name: 'gpt-5' } },
        async (generation) => {
          recorders.push(generation);
          await ready;
          const tool = generation.startToolExecution({ toolName: `tool-${index}` });
          await Promise.all(
            [0, 1].map(async (childIndex) => {
              await Promise.resolve();
              const child = tool.startToolExecution({ toolName: `child-${index}-${childIndex}` });
              await Promise.resolve();
              const leaf = child.startToolExecution({ toolName: `leaf-${index}-${childIndex}` });
              leaf.end();
              child.end();
              assert.equal(leaf.getError(), undefined);
              assert.equal(child.getError(), undefined);
              assert.equal(context.active(), activeContext);
            }),
          );
          tool.setResult({ result: index });
          tool.end();
          assert.equal(tool.getError(), undefined);
          generation.setResult({ output: [{ role: 'assistant', content: `result-${index}` }] });
        },
      ),
    );
    assert.equal(recorders.length, 2);
    assert.equal(context.active(), activeContext);
    release();
    await Promise.all(pending);
    await harness.client.flush();

    const generations = generationSpans(harness.spanExporter);
    const tools = toolSpans(harness.spanExporter);
    assert.equal(generations.length, 2);
    assert.equal(tools.length, 10);
    assert.notEqual(generations[0].spanContext().traceId, generations[1].spanContext().traceId);
    for (const index of [0, 1]) {
      const parent = generations.find((span) => span.attributes['agento11y.generation.id'] === `generation-${index}`);
      const tool = tools.find((span) => span.attributes['gen_ai.tool.name'] === `tool-${index}`);
      assert.equal(tool.spanContext().traceId, parent.spanContext().traceId);
      assert.equal(tool.parentSpanContext?.spanId, parent.spanContext().spanId);
      for (const childIndex of [0, 1]) {
        const child = tools.find((span) => span.attributes['gen_ai.tool.name'] === `child-${index}-${childIndex}`);
        const leaf = tools.find((span) => span.attributes['gen_ai.tool.name'] === `leaf-${index}-${childIndex}`);
        assert.equal(child.parentSpanContext?.spanId, tool.spanContext().spanId);
        assert.equal(leaf.parentSpanContext?.spanId, child.spanContext().spanId);
        assert.equal(child.spanContext().traceId, parent.spanContext().traceId);
        assert.equal(leaf.spanContext().traceId, parent.spanContext().traceId);
      }
      const exported = harness.generationExporter.requests.flatMap((request) => request.generations);
      const generation = exported.find((item) => item.id === `generation-${index}`);
      assert.equal(generation.traceId, parent.spanContext().traceId);
      assert.equal(generation.spanId, parent.spanContext().spanId);
    }
    assert.equal(context.active(), activeContext);
    assert.deepEqual(harness.client.debugSnapshot().generations, []);
    assert.deepEqual(harness.client.debugSnapshot().toolExecutions, []);
  } finally {
    release();
    await shutdownHarness(harness);
  }
});

test('bound tools reuse tool semantics without inheriting generation identity or capture mode', async () => {
  const harness = newHarness({ contentCapture: 'full', agentName: 'client-agent', agentVersion: 'client-version' });

  try {
    const generation = harness.client.startGeneration({
      conversationId: 'generation-conversation',
      conversationTitle: 'generation-title',
      agentName: 'generation-agent',
      agentVersion: 'generation-version',
      contentCapture: 'metadata_only',
      model: { provider: 'openai', name: 'gpt-5' },
    });
    const tool = generation.startToolExecution({ toolName: 'load', executionKind: 'load_skill', skillName: 'review' });
    tool.setResult({ arguments: { input: 'visible' }, result: 'visible' });
    tool.end();
    tool.end();
    generation.end();
    assert.equal(tool.getError(), undefined);

    const span = singleToolSpan(harness.spanExporter);
    assert.equal(span.name, 'execute_tool load review');
    assert.equal(span.attributes['gen_ai.skill.name'], 'review');
    assert.equal(span.attributes['gen_ai.agent.name'], 'client-agent');
    assert.equal(span.attributes['gen_ai.agent.version'], 'client-version');
    for (const key of [
      'gen_ai.conversation.id',
      'agento11y.conversation.title',
      'gen_ai.request.model',
      'gen_ai.provider.name',
    ]) {
      assert.equal(span.attributes[key], undefined, key);
    }
    assert.equal(span.attributes['gen_ai.tool.call.arguments'], '{"input":"visible"}');
    assert.equal(span.attributes['gen_ai.tool.call.result'], '"visible"');
    assert.equal(harness.client.debugSnapshot().toolExecutions.length, 1);
  } finally {
    await shutdownHarness(harness);
  }
});

test('valid nonrecording contexts are exported and explicitly passed to recursive tool spans', async () => {
  const spanContext = { traceId: '1234567890abcdef1234567890abcdef', spanId: '1234567890abcdef', traceFlags: 0 };
  const toolContext = { ...spanContext, spanId: 'fedcba0987654321' };
  const starts = [];
  const tracer = {
    startSpan(name, options, parentContext) {
      const span = trace.wrapSpanContext(starts.length === 0 ? spanContext : toolContext);
      starts.push({ name, options, parentContext, span });
      return span;
    },
  };
  const harness = newHarness({ tracer });

  try {
    const generation = harness.client.startGeneration({ model: { provider: 'openai', name: 'gpt-5' } });
    const tool = generation.startToolExecution({ toolName: 'weather' });
    const child = tool.startToolExecution({ toolName: 'nested-weather' });
    child.end();
    tool.end();
    generation.end();
    await harness.client.flush();

    assert.equal(starts.length, 3);
    assert.ok(starts[1].parentContext);
    assert.deepEqual(trace.getSpanContext(starts[1].parentContext), spanContext);
    assert.equal(starts[1].options.attributes['gen_ai.tool.name'], 'weather');
    assert.deepEqual(trace.getSpanContext(starts[2].parentContext), toolContext);
    assert.equal(trace.getSpan(starts[2].parentContext), starts[1].span);
    assert.equal(starts[2].options.attributes['gen_ai.tool.name'], 'nested-weather');
    const exported = harness.generationExporter.requests[0].generations[0];
    assert.equal(exported.traceId, spanContext.traceId);
    assert.equal(exported.spanId, spanContext.spanId);
    assert.equal(singleGeneration(harness.client).traceId, spanContext.traceId);
  } finally {
    await shutdownHarness(harness);
  }
});

for (const spanContext of [
  { traceId: '0'.repeat(32), spanId: '0'.repeat(16), traceFlags: 0 },
  { traceId: '1234567890abcdef1234567890abcdef', spanId: '0'.repeat(16), traceFlags: 0 },
  { traceId: '0'.repeat(32), spanId: '1234567890abcdef', traceFlags: 0 },
  { traceId: 'invalid', spanId: '1234567890abcdef', traceFlags: 0 },
]) {
  test(`invalid generation span context is not exported: ${spanContext.traceId}/${spanContext.spanId}`, async () => {
    const harness = newHarness({ tracer: { startSpan: () => trace.wrapSpanContext(spanContext) } });
    try {
      const generation = harness.client.startGeneration({ model: { provider: 'openai', name: 'gpt-5' } });
      generation.end();
      await harness.client.flush();
      for (const record of [singleGeneration(harness.client), harness.generationExporter.requests[0].generations[0]]) {
        assert.equal(record.traceId, undefined);
        assert.equal(record.spanId, undefined);
      }
    } finally {
      await shutdownHarness(harness);
    }
  });
}

test('generation callback rejection finalizes its bound tool and generation once', async () => {
  const harness = newHarness();
  const failure = new Error('bound tool failed');
  let generationRecorder;
  let toolRecorder;
  try {
    await assert.rejects(
      harness.client.startGeneration({ model: { provider: 'openai', name: 'gpt-5' } }, async (generation) => {
        generationRecorder = generation;
        const tool = generation.startToolExecution({ toolName: 'weather' });
        toolRecorder = tool;
        try {
          await Promise.resolve();
          throw failure;
        } catch (error) {
          tool.setCallError(error);
          throw error;
        } finally {
          tool.end();
        }
      }),
      (error) => error === failure,
    );
    generationRecorder.end();
    toolRecorder.end();
    assert.equal(generationRecorder.getError(), undefined);
    assert.equal(toolRecorder.getError(), failure);
    const generation = singleGenerationSpan(harness.spanExporter);
    const tool = singleToolSpan(harness.spanExporter);
    assert.equal(tool.parentSpanContext?.spanId, generation.spanContext().spanId);
    assert.equal(tool.status.code, SpanStatusCode.ERROR);
    assert.equal(tool.attributes['error.type'], 'tool_execution_error');
    assert.equal(generation.status.code, SpanStatusCode.ERROR);
    assert.equal(generation.attributes['error.type'], 'provider_call_error');
    assert.equal(singleGeneration(harness.client).callError, failure.message);
    assert.equal(harness.client.debugSnapshot().toolExecutions.length, 1);
    await harness.client.flush();
    assert.equal(harness.generationExporter.requests[0].generations.length, 1);
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation result fields override seed and update span operation name', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      conversationId: 'conv-seed',
      agentName: 'agent-seed',
      agentVersion: 'v-seed',
      maxTokens: 512,
      temperature: 0.7,
      topP: 0.9,
      toolChoice: 'auto',
      thinkingEnabled: true,
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      conversationId: 'conv-result',
      agentName: 'agent-result',
      agentVersion: 'v-result',
      operationName: 'text_completion',
      maxTokens: 256,
      temperature: 0.2,
      topP: 0.85,
      toolChoice: 'required',
      thinkingEnabled: false,
      metadata: {
        'agento11y.gen_ai.request.thinking.budget_tokens': 4096,
        'agento11y.framework.run_id': 'framework-run-1',
        'agento11y.framework.thread_id': 'framework-thread-1',
        'agento11y.framework.parent_run_id': 'framework-parent-1',
        'agento11y.framework.component_name': 'ChatOpenAI',
        'agento11y.framework.run_type': 'chat',
        'agento11y.framework.retry_attempt': 3,
        'agento11y.framework.langgraph.node': 'answer_node',
        'agento11y.sdk.name': 'user-value',
      },
      stopReason: 'end_turn',
      output: [{ role: 'assistant', content: 'ok' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const generation = singleGeneration(harness.client);
    assert.equal(generation.conversationId, 'conv-result');
    assert.equal(generation.agentName, 'agent-result');
    assert.equal(generation.agentVersion, 'v-result');
    assert.equal(generation.operationName, 'text_completion');
    assert.equal(generation.maxTokens, 256);
    assert.equal(generation.temperature, 0.2);
    assert.equal(generation.topP, 0.85);
    assert.equal(generation.toolChoice, 'required');
    assert.equal(generation.thinkingEnabled, false);
    assert.equal(generation.metadata?.['agento11y.framework.run_id'], 'framework-run-1');
    assert.equal(generation.metadata?.['agento11y.framework.thread_id'], 'framework-thread-1');
    assert.equal(generation.metadata?.['agento11y.framework.parent_run_id'], 'framework-parent-1');
    assert.equal(generation.metadata?.['agento11y.framework.component_name'], 'ChatOpenAI');
    assert.equal(generation.metadata?.['agento11y.framework.run_type'], 'chat');
    assert.equal(generation.metadata?.['agento11y.framework.retry_attempt'], 3);
    assert.equal(generation.metadata?.['agento11y.framework.langgraph.node'], 'answer_node');
    assert.equal(generation.metadata?.['agento11y.sdk.name'], 'sdk-js');

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.name, 'text_completion gpt-5');
    assert.equal(span.attributes['gen_ai.operation.name'], 'text_completion');
    assert.equal(span.attributes['gen_ai.conversation.id'], 'conv-result');
    assert.equal(span.attributes['gen_ai.agent.name'], 'agent-result');
    assert.equal(span.attributes['gen_ai.agent.version'], 'v-result');
    assert.equal(span.attributes['gen_ai.request.max_tokens'], 256);
    assert.equal(span.attributes['gen_ai.request.temperature'], 0.2);
    assert.equal(span.attributes['gen_ai.request.top_p'], 0.85);
    assert.equal(span.attributes['agento11y.gen_ai.request.tool_choice'], 'required');
    assert.equal(span.attributes['agento11y.gen_ai.request.thinking.enabled'], false);
    assert.equal(span.attributes['agento11y.gen_ai.request.thinking.budget_tokens'], 4096);
    assert.equal(span.attributes['agento11y.framework.run_id'], 'framework-run-1');
    assert.equal(span.attributes['agento11y.framework.thread_id'], 'framework-thread-1');
    assert.equal(span.attributes['agento11y.framework.parent_run_id'], 'framework-parent-1');
    assert.equal(span.attributes['agento11y.framework.component_name'], 'ChatOpenAI');
    assert.equal(span.attributes['agento11y.framework.run_type'], 'chat');
    assert.equal(span.attributes['agento11y.framework.retry_attempt'], 3);
    assert.equal(span.attributes['agento11y.framework.langgraph.node'], 'answer_node');
    assert.equal(span.attributes['agento11y.sdk.name'], 'sdk-js');
    assert.deepEqual(span.attributes['gen_ai.response.finish_reasons'], ['end_turn']);
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation normalization trims only title and user fields', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      conversationId: '  conv-seed  ',
      conversationTitle: '  title-seed  ',
      userId: '  user-seed  ',
      agentName: '  agent-seed  ',
      agentVersion: '  v-seed  ',
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      conversationId: '  conv-result  ',
      conversationTitle: '  title-result  ',
      userId: '  user-result  ',
      agentName: '  agent-result  ',
      agentVersion: '  v-result  ',
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const generation = singleGeneration(harness.client);
    assert.equal(generation.conversationId, '  conv-result  ');
    assert.equal(generation.conversationTitle, 'title-result');
    assert.equal(generation.userId, 'user-result');
    assert.equal(generation.agentName, '  agent-result  ');
    assert.equal(generation.agentVersion, '  v-result  ');
    assert.equal(generation.metadata?.['agento11y.conversation.title'], 'title-result');
    assert.equal(generation.metadata?.['agento11y.user.id'], 'user-result');

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.attributes['gen_ai.conversation.id'], '  conv-result  ');
    assert.equal(span.attributes['agento11y.conversation.title'], 'title-result');
    assert.equal(span.attributes['user.id'], 'user-result');
    assert.equal(span.attributes['gen_ai.agent.name'], '  agent-result  ');
    assert.equal(span.attributes['gen_ai.agent.version'], '  v-result  ');
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation span reflects metadata fallback title and user id after normalization', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      metadata: {
        'agento11y.conversation.title': '  Meta title  ',
        'user.id': '  legacy-user  ',
      },
    });
    recorder.setResult({});
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const generation = singleGeneration(harness.client);
    assert.equal(generation.conversationTitle, 'Meta title');
    assert.equal(generation.userId, 'legacy-user');
    assert.equal(generation.metadata?.['agento11y.conversation.title'], 'Meta title');
    assert.equal(generation.metadata?.['agento11y.user.id'], 'legacy-user');

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.attributes['agento11y.conversation.title'], 'Meta title');
    assert.equal(span.attributes['user.id'], 'legacy-user');
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation span marks inclusive token semantics only for identified usage', async () => {
  const harness = newHarness();

  try {
    const markedRecorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5.6-sol' },
    });
    markedRecorder.setResult({
      usage: { inputTokens: 10, outputTokens: 4, cacheReadInputTokens: 3, inputSemantics: 'inclusive' },
    });
    markedRecorder.end();
    assert.equal(markedRecorder.getError(), undefined);

    const markedSpan = singleGenerationSpan(harness.spanExporter);
    assert.equal(markedSpan.attributes['gen_ai.token.semantics'], 'inclusive');
    harness.spanExporter.reset();

    const manualRecorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5.6-sol' },
    });
    manualRecorder.setResult({
      usage: { inputTokens: 10, outputTokens: 4, cacheReadInputTokens: 3 },
    });
    manualRecorder.end();
    assert.equal(manualRecorder.getError(), undefined);

    const manualSpan = singleGenerationSpan(harness.spanExporter);
    assert.equal(manualSpan.attributes['gen_ai.token.semantics'], undefined);
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation callError sets metadata and provider_call_error span status', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
    });
    recorder.setCallError(new Error('provider unavailable'));
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const generation = singleGeneration(harness.client);
    assert.equal(generation.callError, 'provider unavailable');
    assert.equal(generation.metadata?.call_error, 'provider unavailable');
    assert.equal(generation.metadata?.['agento11y.sdk.name'], 'sdk-js');

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.attributes['error.type'], 'provider_call_error');
    assert.equal(span.attributes['agento11y.sdk.name'], 'sdk-js');
  } finally {
    await shutdownHarness(harness);
  }
});

test('embedding span sets standard attributes and does not enqueue generation export', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
      agentName: 'agent-embed',
      agentVersion: 'v-embed',
      dimensions: 256,
      encodingFormat: 'float',
    });
    recorder.setResult({
      inputCount: 2,
      inputTokens: 64,
      responseModel: 'text-embedding-3-small',
      dimensions: 512,
      inputTexts: ['first', 'second'],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const snapshot = harness.client.debugSnapshot();
    assert.equal(snapshot.generations.length, 0);
    assert.equal(harness.generationExporter.requests.length, 0);

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.name, 'embeddings text-embedding-3-small');
    assert.equal(span.attributes['gen_ai.operation.name'], 'embeddings');
    assert.equal(span.attributes['gen_ai.provider.name'], 'openai');
    assert.equal(span.attributes['gen_ai.request.model'], 'text-embedding-3-small');
    assert.equal(span.attributes['gen_ai.agent.name'], 'agent-embed');
    assert.equal(span.attributes['gen_ai.agent.version'], 'v-embed');
    assert.equal(span.attributes['gen_ai.embeddings.dimension.count'], 512);
    assert.deepEqual(span.attributes['gen_ai.request.encoding_formats'], ['float']);
    assert.equal(span.attributes['gen_ai.embeddings.input_count'], 2);
    assert.equal(span.attributes['gen_ai.usage.input_tokens'], 64);
    assert.equal(span.attributes['gen_ai.response.model'], 'text-embedding-3-small');
    assert.equal(span.attributes['gen_ai.embeddings.input_texts'], undefined);
    assert.equal(span.status.code, SpanStatusCode.OK);
  } finally {
    await shutdownHarness(harness);
  }
});

test('embedding input text capture is opt-in with truncation limits', async () => {
  const harness = newHarness({
    embeddingCapture: {
      captureInput: true,
      maxInputItems: 2,
      maxTextLength: 8,
    },
  });

  try {
    const recorder = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    recorder.setResult({
      inputCount: 3,
      inputTexts: ['12345678', '123456789', 'dropped'],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.deepEqual(span.attributes['gen_ai.embeddings.input_texts'], ['12345678', '12345...']);
  } finally {
    await shutdownHarness(harness);
  }
});

test('embedding callError marks provider_call_error span status', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startEmbedding({
      model: { provider: 'gemini', name: 'text-embedding-004' },
    });
    recorder.setCallError(new Error('provider unavailable'));
    recorder.end();

    assert.equal(recorder.getError(), undefined);
    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.attributes['error.type'], 'provider_call_error');
  } finally {
    await shutdownHarness(harness);
  }
});

test('embedding validation error is surfaced locally and marks span', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startEmbedding({
      model: { provider: '', name: 'text-embedding-3-small' },
    });
    recorder.end();

    assert.match(recorder.getError()?.message ?? '', /embedding\.model\.provider is required/);
    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.attributes['error.type'], 'validation_error');
    assert.equal(span.attributes['error.category'], 'sdk_error');
  } finally {
    await shutdownHarness(harness);
  }
});

test('tool execution includeContent controls argument/result attributes', async () => {
  const harness = newHarness();

  try {
    const withContent = harness.client.startToolExecution({
      toolName: 'weather',
      includeContent: true,
      conversationId: 'conv-tool',
      agentName: 'agent-tool',
      agentVersion: 'v-tool',
      requestProvider: 'openai',
      requestModel: 'gpt-5',
    });
    withContent.setResult({
      arguments: { city: 'Paris' },
      result: { temp_c: 18 },
    });
    withContent.end();
    assert.equal(withContent.getError(), undefined);

    const withoutContent = harness.client.startToolExecution({
      toolName: 'weather',
    });
    withoutContent.setResult({
      arguments: { city: 'Paris' },
      result: { temp_c: 18 },
    });
    withoutContent.end();
    assert.equal(withoutContent.getError(), undefined);

    const spans = toolSpans(harness.spanExporter);
    assert.equal(spans.length, 2);

    const contentSpan = spans.find((span) => span.attributes['gen_ai.tool.call.arguments'] !== undefined);
    const noContentSpan = spans.find((span) => span.attributes['gen_ai.tool.call.arguments'] === undefined);

    assert.ok(contentSpan);
    assert.ok(noContentSpan);
    assert.equal(contentSpan.attributes['gen_ai.operation.name'], 'execute_tool');
    assert.equal(contentSpan.attributes['gen_ai.tool.name'], 'weather');
    assert.equal(contentSpan.attributes['gen_ai.conversation.id'], 'conv-tool');
    assert.equal(contentSpan.attributes['gen_ai.agent.name'], 'agent-tool');
    assert.equal(contentSpan.attributes['gen_ai.agent.version'], 'v-tool');
    assert.equal(contentSpan.attributes['gen_ai.provider.name'], 'openai');
    assert.equal(contentSpan.attributes['gen_ai.request.model'], 'gpt-5');
    assert.equal(contentSpan.attributes['agento11y.sdk.name'], 'sdk-js');
    assert.equal(noContentSpan.attributes['gen_ai.tool.call.arguments'], undefined);
    assert.equal(noContentSpan.attributes['gen_ai.tool.call.result'], undefined);
    assert.equal(noContentSpan.attributes['agento11y.sdk.name'], 'sdk-js');
  } finally {
    await shutdownHarness(harness);
  }
});

for (const [executionKind, processExitCode] of [
  ['load_skill', undefined],
  ['read_skill_resource', undefined],
  ['command', undefined],
  ['command', 0],
  ['command', 7],
]) {
  test(`tool execution snapshots preserve ${executionKind} with exit code ${processExitCode} and isolate clones`, async () => {
    const harness = newHarness();
    const fields = {
      executionKind,
      skillName: 'review',
      skillDescription: 'Review code changes',
      skillSourceURI: 'file:///skills/review/SKILL.md',
      skillResourceName: 'references/checklist.md',
      processExecutableName: 'node',
      processExecutablePath: '/usr/local/bin/node',
    };
    const startedAt = new Date('2026-01-01T00:00:00Z');
    const completedAt = new Date('2026-01-01T00:00:01Z');

    try {
      const start = { toolName: 'run', ...fields, startedAt: new Date(startedAt) };
      const recorder = harness.client.startToolExecution(start);
      for (const key of Object.keys(fields)) {
        start[key] = 'changed';
      }
      start.startedAt.setTime(0);

      const result = { completedAt: new Date(completedAt) };
      if (processExitCode !== undefined) {
        result.processExitCode = processExitCode;
      }
      recorder.setResult(result);
      result.processExitCode = 99;
      result.completedAt.setTime(0);
      recorder.end();
      assert.equal(recorder.getError(), undefined);

      const snapshot = harness.client.debugSnapshot();
      assert.equal(snapshot.toolExecutions.length, 1);
      const execution = snapshot.toolExecutions[0];
      for (const [key, value] of Object.entries(fields)) {
        assert.equal(execution[key], value, key);
      }
      assert.equal(execution.processExitCode, processExitCode);
      assert.deepEqual(execution.startedAt, startedAt);
      assert.deepEqual(execution.completedAt, completedAt);

      const expected = structuredClone(execution);
      for (const key of Object.keys(fields)) {
        execution[key] = 'changed';
      }
      execution.processExitCode = 99;
      execution.startedAt.setTime(0);
      execution.completedAt.setTime(0);
      assert.deepEqual(harness.client.debugSnapshot().toolExecutions, [expected]);
    } finally {
      await shutdownHarness(harness);
    }
  });
}

test('tool execution snapshots leave optional execution-kind, skill, and process fields undefined', async () => {
  const harness = newHarness();

  try {
    for (const result of [undefined, {}]) {
      const recorder = harness.client.startToolExecution({ toolName: 'legacy' });
      if (result !== undefined) {
        recorder.setResult(result);
      }
      recorder.end();
      assert.equal(recorder.getError(), undefined);
    }

    const executions = harness.client.debugSnapshot().toolExecutions;
    assert.equal(executions.length, 2);
    for (const execution of executions) {
      for (const key of [
        'executionKind',
        'skillName',
        'skillDescription',
        'skillSourceURI',
        'skillResourceName',
        'processExecutableName',
        'processExecutablePath',
        'processExitCode',
      ]) {
        assert.equal(execution[key], undefined, key);
      }
    }
  } finally {
    await shutdownHarness(harness);
  }
});

test('skill source URI projection validates absolute locations without resolving sources', async (t) => {
  const harness = newHarness({ contentCapture: 'full' });
  try {
    const cases = [
      [' file:///private/skills/review?secret=1#private ', 'file:///private/skills/review'],
      ['https://user:secret@skills.example.com:443/a/../review?q=secret#private', 'https://skills.example.com/review'],
      ['s3://user:secret@skills/review?secret=1#private', 's3://skills/review'],
      ['file:/opt/skills/review/SKILL.md', 'file:///opt/skills/review/SKILL.md'],
      ['file:///opt/skills/review%20guide/SKILL.md', 'file:///opt/skills/review%20guide/SKILL.md'],
      ['file://skills.example.com/review%20guide', 'file://skills.example.com/review%20guide'],
      [
        'https://user:secret@skills.example.com:8443/review%20guide%2Fnotes?token=secret#private',
        'https://skills.example.com:8443/review%20guide%2Fnotes',
      ],
      ['https://[::1]:8443/review%20guide', 'https://[::1]:8443/review%20guide'],
      ['s3://user:secret@skills/review%20guide%2Fnotes?secret=1#private', 's3://skills/review%20guide%2Fnotes'],
      ['gs://skills/review%20guide?secret=1#private', 'gs://skills/review%20guide'],
      ['https://skills.example.com/review%', undefined],
      ['https://skills.example.com/review%2', undefined],
      ['https://skills.example.com/review%GG', undefined],
      ['s3://skills/review%GG', undefined],
      ['file:///opt/skills/review%GG', undefined],
      ['https://skills.example.com/review?secret=%GG', undefined],
      ['https://skills.example.com/review#%GG', undefined],
      ['https://skills.example.com/review guide', undefined],
      ['https://skills.example.com/re\tview', undefined],
      ['https://skills.example.com/re\nview', undefined],
      ['https://skills.example.com/re\rview', undefined],
      ['https://skills.example.com/re\u0000view', undefined],
      ['https://skills.example.com/re\u007fview', undefined],
      ['s3://skills/review guide', undefined],
      ['file:///opt/skills/review guide', undefined],
      ['file:', undefined],
      ['file:relative', undefined],
      ['file://', undefined],
      ['https:///path', undefined],
      ['https:/path', undefined],
      ['https://', undefined],
      ['https://?secret=1', undefined],
      ['https://:443/path', undefined],
      ['https://skills.example.com:invalid/review', undefined],
      ['https://skills.example.com:65536/review', undefined],
      ['s3://skills:invalid/review', undefined],
      ['s3://skills:65536/review', undefined],
      ['https://skills.example.com:/review', undefined],
      ['s3://skills:/review', undefined],
      ['s3://user@@skills/review', undefined],
      ['s3://skills%2Fother/review', undefined],
      ['file://skills.example.com', undefined],
      ['https://skills.example.com\\review', undefined],
      ['s3:///review', undefined],
      ['s3:/skills/review', undefined],
      ['../skills/review', undefined],
      ['/skills/review', undefined],
      ['//skills.example.com/review', undefined],
      ['https://[broken', undefined],
      [' \t ', undefined],
    ];
    for (const [skillSourceURI, expected] of cases) {
      await t.test(JSON.stringify(skillSourceURI), () => {
        const recorder = harness.client.startToolExecution({
          toolName: 'load',
          executionKind: 'load_skill',
          skillSourceURI,
        });
        recorder.end();
        assert.equal(recorder.getError(), undefined);
        assert.equal(toolSpans(harness.spanExporter).at(-1).attributes['gen_ai.skill.source.uri'], expected);
        assert.equal(harness.client.debugSnapshot().toolExecutions.at(-1).skillSourceURI, skillSourceURI);
      });
    }
  } finally {
    await shutdownHarness(harness);
  }
});

test('tool exit codes are completion-only safe integers on explicit command spans', async () => {
  const harness = newHarness();
  try {
    for (const executionKind of [undefined, 'future', 'load_skill', 'read_skill_resource', 'command']) {
      for (const processExitCode of [
        undefined,
        null,
        true,
        false,
        '0',
        1.5,
        NaN,
        Infinity,
        -Infinity,
        2 ** 53,
        0,
        -1,
        Number.MAX_SAFE_INTEGER,
        Number.MIN_SAFE_INTEGER,
      ]) {
        const recorder = harness.client.startToolExecution({ toolName: 'exec', executionKind });
        recorder.setResult({ processExitCode });
        recorder.end();
        assert.equal(recorder.getError(), undefined);
        const expected =
          executionKind === 'command' && Number.isSafeInteger(processExitCode) ? processExitCode : undefined;
        assert.equal(toolSpans(harness.spanExporter).at(-1).attributes['process.exit.code'], expected);
      }
    }
  } finally {
    await shutdownHarness(harness);
  }
});

test('tool execution callError is surfaced locally and marks error span', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startToolExecution({
      toolName: 'weather',
    });
    recorder.setCallError(new Error('tool failed'));
    recorder.end();

    assert.equal(recorder.getError()?.message, 'tool failed');
    const span = singleToolSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.attributes['error.type'], 'tool_execution_error');
  } finally {
    await shutdownHarness(harness);
  }
});

test('generation and tool recorders are idempotent on duplicate end()', async () => {
  const harness = newHarness();

  try {
    const generationRecorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    generationRecorder.end();
    generationRecorder.end();
    assert.equal(generationRecorder.getError(), undefined);

    const toolRecorder = harness.client.startToolExecution({
      toolName: 'weather',
    });
    toolRecorder.end();
    toolRecorder.end();
    assert.equal(toolRecorder.getError(), undefined);

    const embeddingRecorder = harness.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    embeddingRecorder.end();
    embeddingRecorder.end();
    assert.equal(embeddingRecorder.getError(), undefined);

    assert.equal(generationSpans(harness.spanExporter).length, 1);
    assert.equal(toolSpans(harness.spanExporter).length, 1);
    assert.equal(embeddingSpans(harness.spanExporter).length, 1);
  } finally {
    await shutdownHarness(harness);
  }
});

test('empty tool name returns no-op tool recorder', async () => {
  const harness = newHarness();

  try {
    const recorder = harness.client.startToolExecution({
      toolName: '',
    });
    recorder.setResult({
      arguments: { city: 'Paris' },
      result: { temp_c: 18 },
    });
    recorder.setCallError(new Error('ignored'));
    recorder.end();

    assert.equal(recorder.getError(), undefined);
    assert.equal(toolSpans(harness.spanExporter).length, 0);
  } finally {
    await shutdownHarness(harness);
  }
});

function newHarness(overrides = {}) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('agento11y-sdk-js-test');
  const generationExporter = new CapturingExporter();
  const defaults = defaultConfig();

  const client = new Agento11yClient({
    debugBufferSize: 64,
    ...overrides,
    tracer: overrides.tracer ?? tracer,
    generationExport: {
      ...defaults.generationExport,
      batchSize: 100,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
      ...(overrides.generationExport ?? {}),
    },
    embeddingCapture: overrides.embeddingCapture,
    contentCapture: overrides.contentCapture,
    generationExporter,
  });

  return {
    client,
    spanExporter,
    traceProvider,
    generationExporter,
  };
}

async function shutdownHarness(harness) {
  await harness.client.shutdown();
  await harness.traceProvider.shutdown();
}

function singleGeneration(client) {
  const snapshot = client.debugSnapshot();
  assert.equal(snapshot.generations.length, 1);
  return snapshot.generations[0];
}

function generationSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => {
    const operation = span.attributes['gen_ai.operation.name'];
    return operation !== 'execute_tool' && operation !== 'embeddings';
  });
}

function embeddingSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => span.attributes['gen_ai.operation.name'] === 'embeddings');
}

function toolSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => span.attributes['gen_ai.operation.name'] === 'execute_tool');
}

function singleGenerationSpan(spanExporter) {
  const spans = generationSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}

function singleEmbeddingSpan(spanExporter) {
  const spans = embeddingSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}

function singleToolSpan(spanExporter) {
  const spans = toolSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}
