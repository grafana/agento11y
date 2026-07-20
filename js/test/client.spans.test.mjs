import assert from 'node:assert/strict';
import test from 'node:test';
import { SpanStatusCode } from '@opentelemetry/api';
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
    tracer,
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
