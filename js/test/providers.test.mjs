import assert from 'node:assert/strict';
import test from 'node:test';
import { SpanStatusCode, trace } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, anthropic, defaultConfig, gemini, openai } from '../.test-dist/index.js';

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

test('anthropic messages wrapper maps strict request/response and records SYNC mode', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await anthropic.messages.create(
      client,
      {
        model: 'claude-sonnet-4-5',
        max_tokens: 320,
        temperature: 0.2,
        top_p: 0.85,
        thinking: { type: 'adaptive', budget_tokens: 2048 },
        system: [{ type: 'text', text: 'anthropic-system' }],
        messages: [{ role: 'user', content: [{ type: 'text', text: 'hello-anthropic' }] }],
      },
      async () => ({
        id: 'resp-anthropic',
        model: 'claude-sonnet-4-5',
        role: 'assistant',
        content: [{ type: 'text', text: 'output-anthropic' }],
        usage: {
          input_tokens: 100,
          output_tokens: 20,
          total_tokens: 120,
          server_tool_use: {
            web_search_requests: 2,
          },
        },
      }),
    );
  });

  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.model.provider, 'anthropic');
  assert.equal(generation.model.name, 'claude-sonnet-4-5');
  assert.equal(generation.temperature, 0.2);
  assert.equal(generation.topP, 0.85);
  assert.equal(generation.maxTokens, 320);
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 2048);
  assert.equal(generation.metadata['agento11y.gen_ai.usage.server_tool_use.web_search_requests'], 2);
  assert.equal(generation.metadata['agento11y.gen_ai.usage.server_tool_use.total_requests'], 2);
  assert.equal(generation.artifacts, undefined);
});

test('gemini models wrapper maps strict request/response and records SYNC mode', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await gemini.models.generateContent(
      client,
      'gemini-2.5-pro',
      [{ role: 'user', parts: [{ text: 'hello-gemini' }] }],
      {
        maxOutputTokens: 320,
        temperature: 0.2,
        topP: 0.85,
        toolConfig: {
          functionCallingConfig: {
            mode: 'ANY',
          },
        },
        thinkingConfig: {
          includeThoughts: true,
          thinkingBudget: 1536,
          thinkingLevel: 'HIGH',
        },
        systemInstruction: {
          role: 'user',
          parts: [{ text: 'gemini-system' }],
        },
      },
      async () => ({
        responseId: 'resp-gemini',
        modelVersion: 'gemini-2.5-pro-001',
        candidates: [
          {
            finishReason: 'STOP',
            content: {
              role: 'model',
              parts: [{ text: 'output-gemini' }],
            },
          },
        ],
        usageMetadata: {
          promptTokenCount: 100,
          candidatesTokenCount: 20,
          totalTokenCount: 120,
          thoughtsTokenCount: 6,
          toolUsePromptTokenCount: 5,
        },
      }),
    );
  });

  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.model.provider, 'gemini');
  assert.equal(generation.model.name, 'gemini-2.5-pro');
  assert.equal(generation.temperature, 0.2);
  assert.equal(generation.topP, 0.85);
  assert.equal(generation.maxTokens, 320);
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 1536);
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.level'], 'high');
  assert.equal(generation.metadata['agento11y.gen_ai.usage.tool_use_prompt_tokens'], 5);
  assert.equal(generation.artifacts, undefined);
});

test('anthropic and gemini stream wrappers set STREAM mode and include artifacts only on opt-in', async () => {
  const anthropicGeneration = await captureSingleGeneration(async (client) => {
    await anthropic.messages.stream(
      client,
      {
        model: 'claude-sonnet-4-5',
        max_tokens: 400,
        temperature: 0.1,
        top_p: 0.9,
        thinking: { type: 'adaptive', budget_tokens: 2048 },
        messages: [{ role: 'user', content: [{ type: 'text', text: 'stream-anthropic' }] }],
      },
      async () => ({
        outputText: 'stream-output-anthropic',
        events: [
          { type: 'content_block_delta', delta: { type: 'text_delta', text: 'stream-output-anthropic' } },
          { type: 'message_delta', usage: { server_tool_use: { web_search_requests: 1 } } },
        ],
      }),
      { rawArtifacts: true },
    );
  });

  assert.equal(anthropicGeneration.mode, 'STREAM');
  assert.equal(anthropicGeneration.model.provider, 'anthropic');
  assert.equal(anthropicGeneration.maxTokens, 400);
  assert.equal(anthropicGeneration.temperature, 0.1);
  assert.equal(anthropicGeneration.topP, 0.9);
  assert.equal(anthropicGeneration.metadata['agento11y.gen_ai.usage.server_tool_use.web_search_requests'], 1);
  assert.equal(anthropicGeneration.metadata['agento11y.gen_ai.usage.server_tool_use.total_requests'], 1);
  assert.ok(Array.isArray(anthropicGeneration.artifacts));
  assert.deepEqual(
    anthropicGeneration.artifacts.map((artifact) => artifact.type),
    ['request', 'provider_event'],
  );

  const geminiGeneration = await captureSingleGeneration(async (client) => {
    await gemini.models.generateContentStream(
      client,
      'gemini-2.5-pro',
      [{ role: 'user', parts: [{ text: 'stream-gemini' }] }],
      {
        maxOutputTokens: 400,
        temperature: 0.1,
        topP: 0.9,
        toolConfig: {
          functionCallingConfig: {
            mode: 'ANY',
          },
        },
        thinkingConfig: {
          includeThoughts: true,
          thinkingBudget: 1536,
          thinkingLevel: 'LOW',
        },
      },
      async () => ({
        outputText: 'stream-output-gemini',
        responses: [
          {
            candidates: [
              {
                content: {
                  role: 'model',
                  parts: [{ text: 'stream-output-gemini' }],
                },
              },
            ],
            usageMetadata: {
              toolUsePromptTokenCount: 3,
            },
          },
        ],
      }),
      { rawArtifacts: true },
    );
  });

  assert.equal(geminiGeneration.mode, 'STREAM');
  assert.equal(geminiGeneration.model.provider, 'gemini');
  assert.equal(geminiGeneration.maxTokens, 400);
  assert.equal(geminiGeneration.temperature, 0.1);
  assert.equal(geminiGeneration.topP, 0.9);
  assert.equal(geminiGeneration.metadata['agento11y.gen_ai.request.thinking.level'], 'low');
  assert.equal(geminiGeneration.metadata['agento11y.gen_ai.usage.tool_use_prompt_tokens'], 3);
  assert.ok(Array.isArray(geminiGeneration.artifacts));
  assert.deepEqual(
    geminiGeneration.artifacts.map((artifact) => artifact.type),
    ['request', 'response', 'provider_event'],
  );
});

test('openai chat completions wrapper maps strict request/response and records SYNC mode', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await openai.chat.completions.create(
      client,
      {
        model: 'gpt-5',
        max_completion_tokens: 320,
        temperature: 0.2,
        top_p: 0.85,
        tool_choice: { type: 'function', function: { name: 'weather' } },
        reasoning: { effort: 'high', max_output_tokens: 1024 },
        messages: [
          { role: 'system', content: 'system-message' },
          { role: 'user', content: 'hello-openai' },
        ],
      },
      async () => ({
        id: 'resp-openai-chat',
        model: 'gpt-5',
        choices: [
          {
            index: 0,
            finish_reason: 'stop',
            message: {
              role: 'assistant',
              content: 'output-openai',
            },
          },
        ],
        created: 0,
        object: 'chat.completion',
        usage: {
          prompt_tokens: 100,
          completion_tokens: 20,
          total_tokens: 120,
          prompt_tokens_details: { cached_tokens: 3 },
          completion_tokens_details: { reasoning_tokens: 4 },
        },
      }),
    );
  });

  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.model.name, 'gpt-5');
  assert.equal(generation.maxTokens, 320);
  assert.equal(generation.temperature, 0.2);
  assert.equal(generation.topP, 0.85);
  assert.equal(generation.stopReason, 'stop');
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 1024);
  assert.equal(generation.artifacts, undefined);
});

test('openai chat completions stream wrapper records STREAM mode and stream events artifacts on opt-in', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await openai.chat.completions.stream(
      client,
      {
        model: 'gpt-5',
        stream: true,
        max_completion_tokens: 400,
        reasoning: { effort: 'medium', max_output_tokens: 768 },
        messages: [{ role: 'user', content: 'stream-openai' }],
      },
      async () => ({
        outputText: 'stream-openai-output',
        events: [
          {
            id: 'evt-1',
            model: 'gpt-5',
            created: 0,
            object: 'chat.completion.chunk',
            choices: [{ index: 0, delta: { content: 'stream-openai-output' } }],
          },
        ],
      }),
      { rawArtifacts: true },
    );
  });

  assert.equal(generation.mode, 'STREAM');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.maxTokens, 400);
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 768);
  assert.ok(Array.isArray(generation.artifacts));
  assert.deepEqual(
    generation.artifacts.map((artifact) => artifact.type),
    ['request', 'provider_event'],
  );
});

test('openai responses wrapper maps strict request/response and records SYNC mode', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await openai.responses.create(
      client,
      {
        model: 'gpt-5',
        instructions: 'be concise',
        input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'hello' }] }],
        max_output_tokens: 256,
        temperature: 0.25,
        top_p: 0.9,
        tool_choice: { type: 'function', name: 'weather' },
        reasoning: { effort: 'high', max_output_tokens: 512 },
      },
      async () => ({
        id: 'resp-openai-responses',
        object: 'response',
        model: 'gpt-5',
        output: [
          {
            id: 'msg-1',
            type: 'message',
            role: 'assistant',
            status: 'completed',
            content: [{ type: 'output_text', text: 'world', annotations: [] }],
          },
        ],
        status: 'completed',
        parallel_tool_calls: false,
        temperature: 0.25,
        top_p: 0.9,
        tools: [],
        created_at: 0,
        incomplete_details: null,
        metadata: {},
        error: null,
        usage: {
          input_tokens: 80,
          output_tokens: 20,
          total_tokens: 100,
          input_tokens_details: { cached_tokens: 2 },
          output_tokens_details: { reasoning_tokens: 3 },
        },
      }),
    );
  });

  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.model.name, 'gpt-5');
  assert.equal(generation.maxTokens, 256);
  assert.equal(generation.temperature, 0.25);
  assert.equal(generation.topP, 0.9);
  assert.equal(generation.stopReason, 'stop');
  assert.equal(generation.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 512);
});

test('openai responses stream wrapper records STREAM mode with stream event artifacts', async () => {
  const generation = await captureSingleGeneration(async (client) => {
    await openai.responses.stream(
      client,
      {
        model: 'gpt-5',
        stream: true,
        input: 'stream this',
        max_output_tokens: 128,
      },
      async () => ({
        events: [
          {
            type: 'response.output_text.delta',
            sequence_number: 1,
            output_index: 0,
            item_id: 'msg-1',
            content_index: 0,
            delta: 'hello',
          },
          {
            type: 'response.completed',
            sequence_number: 2,
            response: {
              id: 'resp-stream',
              object: 'response',
              model: 'gpt-5',
              output: [
                {
                  id: 'msg-1',
                  type: 'message',
                  role: 'assistant',
                  status: 'completed',
                  content: [{ type: 'output_text', text: 'hello', annotations: [] }],
                },
              ],
              status: 'completed',
              parallel_tool_calls: false,
              temperature: 1,
              top_p: 1,
              tools: [],
              created_at: 0,
              incomplete_details: null,
              metadata: {},
              error: null,
              usage: {
                input_tokens: 10,
                output_tokens: 5,
                total_tokens: 15,
                input_tokens_details: { cached_tokens: 0 },
                output_tokens_details: { reasoning_tokens: 0 },
              },
            },
          },
        ],
      }),
      { rawArtifacts: true },
    );
  });

  assert.equal(generation.mode, 'STREAM');
  assert.equal(generation.model.provider, 'openai');
  assert.ok(Array.isArray(generation.artifacts));
  assert.ok(generation.artifacts.some((artifact) => artifact.type === 'provider_event'));
});

test('openai embeddings wrapper records embedding span and does not enqueue generation', async () => {
  const harness = newEmbeddingHarness();

  try {
    await openai.embeddings.create(
      harness.client,
      {
        model: 'text-embedding-3-small',
        input: ['hello', 'world'],
        dimensions: 256,
        encoding_format: 'float',
      },
      async () => ({
        model: 'text-embedding-3-small',
        usage: { prompt_tokens: 22 },
        data: [{ embedding: [0.1, 0.2, 0.3] }],
      }),
    );

    await harness.client.flush();

    assert.equal(harness.generationExporter.requests.length, 0);
    assert.equal(harness.client.debugSnapshot().generations.length, 0);

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.attributes['gen_ai.provider.name'], 'openai');
    assert.equal(span.attributes['gen_ai.request.model'], 'text-embedding-3-small');
    assert.equal(span.attributes['gen_ai.embeddings.input_count'], 2);
    assert.equal(span.attributes['gen_ai.usage.input_tokens'], 22);
    assert.equal(span.attributes['gen_ai.embeddings.dimension.count'], 3);
    assert.deepEqual(span.attributes['gen_ai.request.encoding_formats'], ['float']);
  } finally {
    await shutdownEmbeddingHarness(harness);
  }
});

test('openai embeddings wrapper treats token-array input as a single item', async () => {
  const harness = newEmbeddingHarness();

  try {
    await openai.embeddings.create(
      harness.client,
      {
        model: 'text-embedding-3-small',
        input: [101, 102, 103, 104],
      },
      async () => ({
        model: 'text-embedding-3-small',
        usage: { prompt_tokens: 4 },
        data: [{ embedding: [0.1, 0.2] }],
      }),
    );

    await harness.client.flush();

    assert.equal(harness.generationExporter.requests.length, 0);
    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.attributes['gen_ai.embeddings.input_count'], 1);
    assert.equal(span.attributes['gen_ai.usage.input_tokens'], 4);
  } finally {
    await shutdownEmbeddingHarness(harness);
  }
});

test('gemini embeddings wrapper maps usage and dimensions to embedding span', async () => {
  const harness = newEmbeddingHarness();

  try {
    await gemini.models.embedContent(
      harness.client,
      'gemini-embedding-001',
      [{ role: 'user', parts: [{ text: 'hello-gemini' }] }],
      { outputDimensionality: 384 },
      async () => ({
        embeddings: [
          { values: [0.1, 0.2, 0.3, 0.4], statistics: { tokenCount: 9 } },
          { values: [0.5, 0.6, 0.7, 0.8], statistics: { tokenCount: 6 } },
        ],
      }),
    );

    await harness.client.flush();

    assert.equal(harness.generationExporter.requests.length, 0);
    assert.equal(harness.client.debugSnapshot().generations.length, 0);

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.attributes['gen_ai.provider.name'], 'gemini');
    assert.equal(span.attributes['gen_ai.request.model'], 'gemini-embedding-001');
    assert.equal(span.attributes['gen_ai.embeddings.input_count'], 1);
    assert.equal(span.attributes['gen_ai.usage.input_tokens'], 15);
    assert.equal(span.attributes['gen_ai.embeddings.dimension.count'], 4);
  } finally {
    await shutdownEmbeddingHarness(harness);
  }
});

test('embedding provider wrapper errors set provider_call_error span status', async () => {
  const harness = newEmbeddingHarness();

  try {
    await assert.rejects(
      openai.embeddings.create(
        harness.client,
        {
          model: 'text-embedding-3-small',
          input: 'hello',
        },
        async () => {
          throw new Error('provider failure openai embedding');
        },
      ),
      /provider failure openai embedding/,
    );

    const span = singleEmbeddingSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.attributes['error.type'], 'provider_call_error');
  } finally {
    await shutdownEmbeddingHarness(harness);
  }
});

test('provider mappers throw on missing provider responses and stream summaries', () => {
  assert.throws(
    () =>
      openai.chat.completions.fromRequestResponse(
        {
          model: 'gpt-5',
          messages: [{ role: 'user', content: 'hello' }],
        },
        undefined,
      ),
    /reading 'id'/,
  );
  assert.throws(
    () =>
      openai.responses.fromRequestResponse(
        {
          model: 'gpt-5',
          input: 'hello',
        },
        undefined,
      ),
    /reading 'id'/,
  );
  assert.throws(
    () =>
      openai.chat.completions.fromStream(
        {
          model: 'gpt-5',
          stream: true,
          messages: [{ role: 'user', content: 'hello' }],
        },
        undefined,
      ),
    /reading 'outputText'/,
  );
  assert.throws(
    () =>
      openai.responses.fromStream(
        {
          model: 'gpt-5',
          stream: true,
          input: 'hello',
        },
        undefined,
      ),
    /reading 'events'/,
  );

  assert.throws(
    () =>
      anthropic.messages.fromRequestResponse(
        {
          model: 'claude-sonnet-4-5',
          max_tokens: 128,
          messages: [{ role: 'user', content: [{ type: 'text', text: 'hello' }] }],
        },
        undefined,
      ),
    /reading 'content'/,
  );
  assert.throws(
    () =>
      anthropic.messages.fromStream(
        {
          model: 'claude-sonnet-4-5',
          max_tokens: 128,
          messages: [{ role: 'user', content: [{ type: 'text', text: 'hello' }] }],
        },
        undefined,
      ),
    /reading 'events'/,
  );

  assert.throws(
    () =>
      gemini.models.fromRequestResponse(
        'gemini-2.5-pro',
        [{ role: 'user', parts: [{ text: 'hello' }] }],
        undefined,
        undefined,
      ),
    /reading 'candidates'/,
  );
  assert.throws(
    () =>
      gemini.models.fromStream('gemini-2.5-pro', [{ role: 'user', parts: [{ text: 'hello' }] }], undefined, undefined),
    /reading 'responses'/,
  );
});

test('provider wrappers surface mapper failures when provider payloads are missing', async () => {
  for (const suite of [
    {
      provider: 'openai',
      error: /reading 'id'/,
      run: async (client) => {
        await openai.chat.completions.create(
          client,
          {
            model: 'gpt-5',
            messages: [{ role: 'user', content: 'hello' }],
          },
          async () => undefined,
        );
      },
    },
    {
      provider: 'openai',
      error: /reading 'id'/,
      run: async (client) => {
        await openai.responses.create(
          client,
          {
            model: 'gpt-5',
            input: 'hello',
          },
          async () => undefined,
        );
      },
    },
    {
      provider: 'anthropic',
      error: /reading 'content'/,
      run: async (client) => {
        await anthropic.messages.create(
          client,
          {
            model: 'claude-sonnet-4-5',
            max_tokens: 128,
            messages: [{ role: 'user', content: [{ type: 'text', text: 'hello' }] }],
          },
          async () => undefined,
        );
      },
    },
    {
      provider: 'gemini',
      error: /reading 'candidates'/,
      run: async (client) => {
        await gemini.models.generateContent(
          client,
          'gemini-2.5-pro',
          [{ role: 'user', parts: [{ text: 'hello' }] }],
          undefined,
          async () => undefined,
        );
      },
    },
  ]) {
    const exporter = new CapturingExporter();
    const client = newClient(exporter);
    try {
      await assert.rejects(suite.run(client), suite.error);
      await client.flush();
      const generation = firstGeneration(exporter);
      assert.equal(generation.model.provider, suite.provider);
      assert.match(generation.callError ?? '', suite.error);
      assert.equal(generation.output, undefined);
    } finally {
      await client.shutdown();
    }
  }
});

test('anthropic provider namespace explicitly has no embeddings surface', () => {
  assert.ok(anthropic.messages);
  assert.equal(anthropic.embeddings, undefined);
});

test('provider wrappers propagate provider errors and persist callError', async () => {
  for (const suite of [
    {
      name: 'anthropic',
      provider: 'anthropic',
      run: (client) =>
        anthropic.messages.create(
          client,
          {
            model: 'claude-sonnet-4-5',
            max_tokens: 128,
            messages: [{ role: 'user', content: [{ type: 'text', text: 'hello' }] }],
          },
          async () => {
            throw new Error('provider failure anthropic');
          },
        ),
    },
    {
      name: 'gemini',
      provider: 'gemini',
      run: (client) =>
        gemini.models.generateContent(
          client,
          'gemini-2.5-pro',
          [{ role: 'user', parts: [{ text: 'hello' }] }],
          undefined,
          async () => {
            throw new Error('provider failure gemini');
          },
        ),
    },
  ]) {
    const exporter = new CapturingExporter();
    const client = newClient(exporter);

    try {
      await assert.rejects(suite.run(client), new RegExp(`provider failure ${suite.name}`));

      await client.flush();
      const generation = firstGeneration(exporter);
      assert.equal(generation.model.provider, suite.provider);
      assert.equal(generation.callError, `provider failure ${suite.name}`);
      assert.equal(generation.output, undefined);
    } finally {
      await client.shutdown();
    }
  }

  for (const run of [
    async (client) => {
      await openai.chat.completions.create(
        client,
        {
          model: 'gpt-5',
          messages: [{ role: 'user', content: 'hello' }],
        },
        async () => {
          throw new Error('provider failure openai chat');
        },
      );
    },
    async (client) => {
      await openai.responses.create(
        client,
        {
          model: 'gpt-5',
          input: 'hello',
        },
        async () => {
          throw new Error('provider failure openai responses');
        },
      );
    },
  ]) {
    const exporter = new CapturingExporter();
    const client = newClient(exporter);
    try {
      await assert.rejects(run(client));
      await client.flush();
      const generation = firstGeneration(exporter);
      assert.equal(generation.model.provider, 'openai');
      assert.match(generation.callError, /provider failure openai/);
      assert.equal(generation.output, undefined);
    } finally {
      await client.shutdown();
    }
  }
});

test('openai chat mapper aggregates system/developer, preserves tool role, and applies raw artifact policy', () => {
  const request = {
    model: 'gpt-5',
    max_completion_tokens: 256,
    max_tokens: 999,
    temperature: 0.3,
    top_p: 0.8,
    tool_choice: { type: 'function', function: { name: 'weather' } },
    reasoning: { effort: 'high', max_output_tokens: 1024 },
    messages: [
      { role: 'system', content: 'system-message' },
      { role: 'developer', content: 'developer-message' },
      { role: 'user', content: 'hello' },
      { role: 'tool', tool_call_id: 'call_weather', content: '{"ok":true}', name: 'tool-weather' },
    ],
    tools: [
      {
        type: 'function',
        function: {
          name: 'weather',
          description: 'lookup weather',
          parameters: { type: 'object' },
        },
      },
    ],
  };

  const response = {
    id: 'resp-openai',
    model: 'gpt-5',
    choices: [
      {
        index: 0,
        finish_reason: 'tool_calls',
        message: {
          role: 'assistant',
          content: 'world',
          tool_calls: [
            {
              id: 'call_weather',
              type: 'function',
              function: {
                name: 'weather',
                arguments: '{"city":"Paris"}',
              },
            },
          ],
        },
      },
    ],
    created: 0,
    object: 'chat.completion',
    usage: {
      prompt_tokens: 10,
      completion_tokens: 5,
      total_tokens: 15,
    },
  };

  const mappedDefault = openai.chat.completions.fromRequestResponse(request, response);
  assert.equal(mappedDefault.responseModel, 'gpt-5');
  assert.equal(mappedDefault.input.length, 2);
  assert.equal(mappedDefault.input[0].role, 'user');
  assert.equal(mappedDefault.input[1].role, 'tool');
  assert.equal(mappedDefault.input[1].parts[0].type, 'tool_result');
  assert.equal(mappedDefault.input[1].parts[0].toolResult.toolCallId, 'call_weather');
  assert.equal(mappedDefault.input[1].parts[0].toolResult.name, 'tool-weather');
  assert.equal(mappedDefault.input[1].parts[0].toolResult.content, '{"ok":true}');
  assert.equal(mappedDefault.maxTokens, 256);
  assert.equal(mappedDefault.temperature, 0.3);
  assert.equal(mappedDefault.topP, 0.8);
  assert.equal(mappedDefault.thinkingEnabled, true);
  assert.equal(mappedDefault.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 1024);
  assert.equal(mappedDefault.artifacts, undefined);
  assert.equal(mappedDefault.output[0].role, 'assistant');

  const mappedWithArtifacts = openai.chat.completions.fromRequestResponse(request, response, {
    rawArtifacts: true,
  });
  assert.deepEqual(
    mappedWithArtifacts.artifacts.map((artifact) => artifact.type),
    ['request', 'response', 'tools'],
  );
});

test('openai responses mapper maps input/output/usage and stream fallback from events', () => {
  const request = {
    model: 'gpt-5',
    instructions: 'Be concise',
    input: [
      {
        type: 'message',
        role: 'user',
        content: [{ type: 'input_text', text: 'hello' }],
      },
      {
        type: 'function_call_output',
        call_id: 'call_weather',
        name: 'weather',
        output: { temp_c: 18 },
      },
    ],
    max_output_tokens: 300,
    tool_choice: { type: 'function', name: 'weather' },
    reasoning: { effort: 'medium', max_output_tokens: 640 },
  };

  const response = {
    id: 'resp-1',
    object: 'response',
    model: 'gpt-5',
    output: [
      {
        id: 'msg-1',
        type: 'message',
        role: 'assistant',
        status: 'completed',
        content: [{ type: 'output_text', text: 'world', annotations: [] }],
      },
      {
        id: 'call-1',
        type: 'function_call',
        call_id: 'call_weather',
        name: 'weather',
        arguments: '{"city":"Paris"}',
      },
      {
        id: 'result-1',
        type: 'function_call_output',
        call_id: 'call_weather',
        name: 'weather',
        output: { temp_c: 18 },
      },
    ],
    status: 'completed',
    parallel_tool_calls: false,
    temperature: 1,
    top_p: 1,
    tools: [],
    created_at: 0,
    incomplete_details: null,
    metadata: {},
    error: null,
    usage: {
      input_tokens: 80,
      output_tokens: 20,
      total_tokens: 100,
      input_tokens_details: { cached_tokens: 2 },
      output_tokens_details: { reasoning_tokens: 3 },
    },
  };

  const mapped = openai.responses.fromRequestResponse(request, response);
  assert.equal(mapped.responseModel, 'gpt-5');
  assert.equal(mapped.input.length, 2);
  assert.equal(mapped.input[0].role, 'user');
  assert.equal(mapped.input[0].content, 'hello');
  assert.equal(mapped.input[1].role, 'tool');
  assert.equal(mapped.input[1].parts[0].type, 'tool_result');
  assert.equal(mapped.input[1].parts[0].toolResult.toolCallId, 'call_weather');
  assert.equal(mapped.input[1].parts[0].toolResult.contentJSON, '{"temp_c":18}');
  assert.equal(mapped.maxTokens, 300);
  assert.equal(mapped.stopReason, 'stop');
  assert.equal(mapped.thinkingEnabled, true);
  assert.equal(mapped.metadata['agento11y.gen_ai.request.thinking.budget_tokens'], 640);
  assert.equal(mapped.usage.totalTokens, 100);
  assert.equal(mapped.output.length, 3);
  assert.equal(mapped.output[2].role, 'tool');
  assert.equal(mapped.output[2].parts[0].type, 'tool_result');
  assert.equal(mapped.output[2].parts[0].toolResult.toolCallId, 'call_weather');
  assert.equal(mapped.output[2].parts[0].toolResult.contentJSON, '{"temp_c":18}');

  const streamed = openai.responses.fromStream(
    { ...request, stream: true },
    {
      events: [
        {
          type: 'response.output_text.delta',
          sequence_number: 1,
          output_index: 0,
          item_id: 'msg-1',
          content_index: 0,
          delta: 'delta-one',
        },
        {
          type: 'response.output_text.delta',
          sequence_number: 2,
          output_index: 0,
          item_id: 'msg-1',
          content_index: 0,
          delta: ' delta-two',
        },
      ],
    },
    { rawArtifacts: true },
  );

  assert.equal(streamed.responseModel, 'gpt-5');
  assert.equal(streamed.input.length, 2);
  assert.equal(streamed.input[0].content, 'hello');
  assert.equal(streamed.output.length, 1);
  assert.equal(streamed.output[0].content, 'delta-one delta-two');
  assert.deepEqual(
    streamed.artifacts.map((artifact) => artifact.type),
    ['request', 'provider_event'],
  );
});

test('provider mappers expose thinking disabled when explicitly configured', () => {
  const anthropicMapped = anthropic.messages.fromRequestResponse(
    {
      model: 'claude-sonnet',
      thinking: 'disabled',
      max_tokens: 128,
      messages: [{ role: 'user', content: [{ type: 'text', text: 'hi' }] }],
    },
    {
      id: 'resp-anthropic',
      model: 'claude-sonnet',
      role: 'assistant',
      content: [{ type: 'text', text: 'ok' }],
    },
  );
  assert.equal(anthropicMapped.thinkingEnabled, false);

  const geminiMapped = gemini.models.fromRequestResponse(
    'gemini-pro',
    [{ role: 'user', parts: [{ text: 'hi' }] }],
    { thinkingConfig: { includeThoughts: false } },
    {
      responseId: 'resp-gemini',
      modelVersion: 'gemini-pro',
      candidates: [{ content: { role: 'model', parts: [{ text: 'ok' }] } }],
    },
  );
  assert.equal(geminiMapped.thinkingEnabled, false);
});

test('embedding mappers extract input counts, texts, usage, and dimensions', () => {
  const openAIMapped = openai.embeddings.fromRequestResponse(
    {
      model: 'text-embedding-3-small',
      input: ['hello', { text: 'world' }, [1, 2, 3]],
    },
    {
      model: 'text-embedding-3-small',
      usage: { prompt_tokens: 30 },
      data: [{ embedding: [0.1, 0.2, 0.3] }],
    },
  );
  assert.equal(openAIMapped.inputCount, 3);
  assert.deepEqual(openAIMapped.inputTexts, ['hello', 'world']);
  assert.equal(openAIMapped.inputTokens, 30);
  assert.equal(openAIMapped.responseModel, 'text-embedding-3-small');
  assert.equal(openAIMapped.dimensions, 3);

  const openAITokenizedSingle = openai.embeddings.fromRequestResponse(
    {
      model: 'text-embedding-3-small',
      input: [101, 102, 103],
    },
    {
      model: 'text-embedding-3-small',
      usage: { prompt_tokens: 3 },
      data: [{ embedding: [0.1, 0.2] }],
    },
  );
  assert.equal(openAITokenizedSingle.inputCount, 1);
  assert.equal(openAITokenizedSingle.inputTexts, undefined);
  assert.equal(openAITokenizedSingle.inputTokens, 3);

  const geminiMapped = gemini.models.embeddingFromResponse(
    'gemini-embedding-001',
    ['alpha', { role: 'user', parts: [{ text: 'beta' }] }],
    { outputDimensionality: 128 },
    {
      embeddings: [
        { values: [0.1, 0.2], statistics: { tokenCount: 4 } },
        { values: [0.3, 0.4], statistics: { tokenCount: 5 } },
      ],
    },
  );
  assert.equal(geminiMapped.inputCount, 2);
  assert.deepEqual(geminiMapped.inputTexts, ['alpha', 'beta']);
  assert.equal(geminiMapped.inputTokens, 9);
  assert.equal(geminiMapped.dimensions, 2);
});

async function captureSingleGeneration(run) {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    await run(client);
    await client.flush();
    return firstGeneration(exporter);
  } finally {
    await client.shutdown();
  }
}

function firstGeneration(exporter) {
  assert.equal(exporter.requests.length, 1);
  assert.equal(exporter.requests[0].generations.length, 1);
  return exporter.requests[0].generations[0];
}

function newClient(generationExporter) {
  const defaults = defaultConfig();
  return new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 100,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
    generationExporter,
  });
}

function newEmbeddingHarness() {
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
    },
    generationExporter,
  });

  return {
    client,
    spanExporter,
    traceProvider,
    generationExporter,
  };
}

async function shutdownEmbeddingHarness(harness) {
  await harness.client.shutdown();
  await harness.traceProvider.shutdown();
}

function singleEmbeddingSpan(spanExporter) {
  const spans = spanExporter
    .getFinishedSpans()
    .filter((span) => span.attributes['gen_ai.operation.name'] === 'embeddings');
  assert.equal(spans.length, 1);
  return spans[0];
}
