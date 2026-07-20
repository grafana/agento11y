import assert from 'node:assert/strict';
import test from 'node:test';
import { context, SpanStatusCode, trace } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yLangGraphHandler } from '../.test-dist/frameworks/langgraph/index.js';
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

test('langgraph handler records sync lifecycle with framework tags', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, {
      agentName: 'agent-langgraph',
      agentVersion: 'v1',
      extraTags: { env: 'test', 'agento11y.framework.name': 'override' },
      extraMetadata: {
        seed: 9,
        'agento11y.framework.run_id': 'override-run',
        'agento11y.framework.thread_id': 'override-thread',
      },
    });

    await handler.handleChatModelStart(
      { name: 'ChatOpenAI' },
      [[{ type: 'human', content: 'hello' }]],
      'run-sync',
      'parent-run-sync',
      { invocation_params: { model: 'gpt-5', retry_attempt: 2 } },
      ['prod', 'blue'],
      { thread_id: 'graph-thread-42', langgraph_node: 'answer_node' },
    );
    await handler.handleLLMEnd(
      {
        generations: [[{ text: 'world' }]],
        llm_output: {
          model_name: 'gpt-5',
          finish_reason: 'stop',
          token_usage: {
            prompt_tokens: 11,
            completion_tokens: 6,
            total_tokens: 17,
          },
        },
      },
      'run-sync',
    );
  });

  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.tags['agento11y.framework.name'], 'langgraph');
  assert.equal(generation.tags['agento11y.framework.source'], 'handler');
  assert.equal(generation.tags['agento11y.framework.language'], 'javascript');
  assert.equal(generation.tags.env, 'test');
  assert.equal(generation.conversationId, 'graph-thread-42');
  assert.equal(generation.metadata['agento11y.framework.run_id'], 'run-sync');
  assert.equal(generation.metadata['agento11y.framework.thread_id'], 'graph-thread-42');
  assert.equal(generation.metadata['agento11y.framework.parent_run_id'], 'parent-run-sync');
  assert.equal(generation.metadata['agento11y.framework.component_name'], 'ChatOpenAI');
  assert.equal(generation.metadata['agento11y.framework.run_type'], 'chat');
  assert.equal(generation.metadata['agento11y.framework.retry_attempt'], 2);
  assert.deepEqual(generation.metadata['agento11y.framework.tags'], ['prod', 'blue']);
  assert.equal(generation.metadata['agento11y.framework.langgraph.node'], 'answer_node');
  assert.equal(generation.metadata.seed, 9);
});

test('langgraph handler records stream mode and token fallback output', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    const handler = new Agento11yLangGraphHandler(client);

    await handler.handleLLMStart({ kwargs: { model: 'gemini-2.5-pro' } }, ['stream this'], 'run-stream', undefined, {
      invocation_params: { model: 'gemini-2.5-pro', streaming: true },
    });
    await handler.handleLLMNewToken('hello', undefined, 'run-stream');
    await handler.handleLLMNewToken(' world', undefined, 'run-stream');
    await handler.handleLLMEnd({ llm_output: { model_name: 'gemini-2.5-pro' } }, 'run-stream');
  });

  assert.equal(generation.mode, 'STREAM');
  assert.equal(generation.model.provider, 'gemini');
  assert.equal(generation.output[0].content, 'hello world');
});

test('langgraph generation span tracks active parent span and preserves export lineage', async () => {
  const spanExporter = new InMemorySpanExporter();
  const tracerProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const baseTracer = tracerProvider.getTracer('agento11y-framework-test');
  let parentContext;
  const tracer = {
    startSpan(name, options, contextArg) {
      return baseTracer.startSpan(name, options, contextArg ?? parentContext);
    },
    startActiveSpan(...args) {
      return baseTracer.startActiveSpan(...args);
    },
  };
  const defaults = defaultConfig();
  const exporter = new CapturingExporter();
  const client = new Agento11yClient({
    generationExport: {
      ...defaults.generationExport,
      batchSize: 10,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
    tracer,
  });

  try {
    const handler = new Agento11yLangGraphHandler(client);
    const parentSpan = baseTracer.startSpan('framework.request');
    parentContext = trace.setSpan(context.active(), parentSpan);
    await handler.handleChatModelStart(
      { name: 'ChatOpenAI' },
      [[{ type: 'human', content: 'hello' }]],
      'run-lineage',
      'parent-run-lineage',
      { invocation_params: { model: 'gpt-5' } },
      ['prod'],
      { thread_id: 'graph-thread-lineage-42', langgraph_node: 'answer_node' },
    );
    await handler.handleLLMEnd(
      {
        generations: [[{ text: 'world' }]],
        llm_output: { model_name: 'gpt-5', finish_reason: 'stop' },
      },
      'run-lineage',
    );
    parentSpan.end();

    await client.flush();
    const generation = exporter.requests[0].generations[0];
    const generationSpan = spanExporter
      .getFinishedSpans()
      .find((span) => span.attributes['gen_ai.operation.name'] === 'generateText');

    assert.ok(generationSpan);
    assert.equal(generationSpan.parentSpanContext?.spanId, parentSpan.spanContext().spanId);
    assert.equal(generationSpan.spanContext().traceId, parentSpan.spanContext().traceId);
    assert.equal(generation.traceId, generationSpan.spanContext().traceId);
    assert.equal(generation.spanId, generationSpan.spanContext().spanId);
  } finally {
    await client.shutdown();
    await tracerProvider.shutdown();
  }
});

test('langgraph provider mapping covers openai anthopic gemini and fallback', async () => {
  const providers = [];

  await captureGenerations(
    async (client) => {
      const handler = new Agento11yLangGraphHandler(client);

      await handler.handleLLMStart({}, ['x'], 'run-openai', undefined, { invocation_params: { model: 'o3-mini' } });
      await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, 'run-openai');

      await handler.handleLLMStart({}, ['x'], 'run-anthropic', undefined, {
        invocation_params: { model: 'claude-sonnet-4-5' },
      });
      await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, 'run-anthropic');

      await handler.handleLLMStart({}, ['x'], 'run-gemini', undefined, {
        invocation_params: { model: 'gemini-2.5-pro' },
      });
      await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, 'run-gemini');

      await handler.handleLLMStart({}, ['x'], 'run-custom', undefined, {
        invocation_params: { model: 'mistral-large' },
      });
      await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, 'run-custom');
    },
    (generation) => providers.push(generation.model.provider),
  );

  assert.deepEqual(providers, ['openai', 'anthropic', 'gemini', 'custom']);
});

test('langgraph backfills Bedrock inference-profile model from the response', async () => {
  // langgraph delegates to the shared handler, so the Bedrock backfill applies here too.
  const arnModel = 'arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6-v1:0';
  const generation = await captureSingleGeneration(async (client) => {
    const handler = new Agento11yLangGraphHandler(client);

    await handler.handleChatModelStart({ name: 'ChatBedrock' }, [[{ type: 'human', content: 'hi' }]], 'run-bedrock');
    await handler.handleLLMEnd(
      {
        generations: [[{ text: 'ok' }]],
        llm_output: { model_name: arnModel, token_usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } },
      },
      'run-bedrock',
    );
  });

  assert.equal(generation.model.name, arnModel);
  assert.equal(generation.model.provider, 'anthropic');
});

test('langgraph handler sets call_error on llm error', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    const handler = new Agento11yLangGraphHandler(client);

    await handler.handleLLMStart({}, ['x'], 'run-error', undefined, { invocation_params: { model: 'gpt-5' } });
    await handler.handleLLMError(new Error('provider unavailable'), 'run-error');
  });

  assert.match(generation.callError ?? '', /provider unavailable/);
  assert.equal(generation.tags['agento11y.framework.name'], 'langgraph');
});

test('langgraph handler explicitly has no embedding lifecycle', async () => {
  const client = new Agento11yClient(defaultConfig());
  try {
    const handler = new Agento11yLangGraphHandler(client);
    assert.equal(typeof handler.handleEmbeddingStart, 'undefined');
    assert.equal(typeof handler.handleEmbeddingEnd, 'undefined');
    assert.equal(typeof handler.handleEmbeddingError, 'undefined');
  } finally {
    await client.shutdown();
  }
});

test('langgraph handler maps tool callbacks and emits chain/retriever spans', async () => {
  const spanExporter = new InMemorySpanExporter();
  const tracerProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = tracerProvider.getTracer('agento11y-framework-test');

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    generationExport: {
      ...defaults.generationExport,
      batchSize: 10,
      flushIntervalMs: 60_000,
    },
    generationExporter: new CapturingExporter(),
    tracer,
  });

  try {
    const handler = new Agento11yLangGraphHandler(client);
    await handler.handleToolStart(
      { name: 'weather', description: 'Get weather' },
      '{"city":"Paris"}',
      'tool-run',
      'parent-run',
      ['tools'],
      { thread_id: 'graph-thread-42', langgraph_node: 'tool_node' },
    );
    await handler.handleToolEnd({ temp_c: 18 }, 'tool-run');

    await handler.handleChainStart(
      { name: 'PlanChain' },
      {},
      'chain-run',
      'parent-run',
      ['workflow'],
      { thread_id: 'graph-thread-42', langgraph_node: 'chain_node' },
      'chain',
    );
    await handler.handleChainEnd({}, 'chain-run');

    await handler.handleRetrieverStart(
      { name: 'VectorRetriever' },
      'where is my data',
      'retriever-run',
      'parent-run',
      ['retriever'],
      { thread_id: 'graph-thread-42', langgraph_node: 'retriever_node' },
    );
    await handler.handleRetrieverError(new Error('retriever failed'), 'retriever-run');

    const spans = spanExporter.getFinishedSpans();
    const toolSpan = spans.find((span) => span.attributes['gen_ai.operation.name'] === 'execute_tool');
    const chainSpan = spans.find((span) => span.attributes['gen_ai.operation.name'] === 'framework_chain');
    const retrieverSpan = spans.find((span) => span.attributes['gen_ai.operation.name'] === 'framework_retriever');

    assert.ok(toolSpan);
    assert.equal(toolSpan.attributes['gen_ai.tool.name'], 'weather');
    assert.equal(toolSpan.attributes['gen_ai.conversation.id'], 'graph-thread-42');

    assert.ok(chainSpan);
    assert.equal(chainSpan.attributes['agento11y.framework.run_type'], 'chain');
    assert.equal(chainSpan.attributes['agento11y.framework.component_name'], 'PlanChain');
    assert.equal(chainSpan.attributes['agento11y.framework.langgraph.node'], 'chain_node');
    assert.equal(chainSpan.status.code, SpanStatusCode.OK);

    assert.ok(retrieverSpan);
    assert.equal(retrieverSpan.attributes['agento11y.framework.run_type'], 'retriever');
    assert.equal(retrieverSpan.attributes['agento11y.framework.component_name'], 'VectorRetriever');
    assert.equal(retrieverSpan.attributes['agento11y.framework.langgraph.node'], 'retriever_node');
    assert.equal(retrieverSpan.attributes['error.type'], 'framework_error');
    assert.equal(retrieverSpan.status.code, SpanStatusCode.ERROR);
  } finally {
    await client.shutdown();
    await tracerProvider.shutdown();
  }
});

async function captureSingleGeneration(run) {
  const generations = [];
  await captureGenerations(run, (generation) => generations.push(generation));
  assert.equal(generations.length, 1);
  return generations[0];
}

async function captureGenerations(run, onGeneration) {
  const exporter = new CapturingExporter();
  const defaults = defaultConfig();
  const client = new Agento11yClient({
    generationExport: {
      ...defaults.generationExport,
      batchSize: 10,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
  });

  try {
    await run(client);
    await client.flush();
    for (const request of exporter.requests) {
      for (const generation of request.generations) {
        onGeneration(generation);
      }
    }
  } finally {
    await client.shutdown();
  }
}
