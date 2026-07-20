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

function newHarness() {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('agento11y-sdk-js-test');
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
    generationExporter: new CapturingExporter(),
  });
  return { client, spanExporter };
}

function toolSpanNames(spanExporter) {
  return spanExporter
    .getFinishedSpans()
    .map((s) => s.name)
    .filter((n) => n.startsWith('execute_tool '));
}

test('executeToolCalls happy path with two tools', async () => {
  const { client, spanExporter } = newHarness();
  try {
    const messages = [
      {
        role: 'assistant',
        parts: [
          {
            type: 'tool_call',
            toolCall: { id: 'c1', name: 'weather', inputJSON: JSON.stringify({ city: 'Paris' }) },
          },
          {
            type: 'tool_call',
            toolCall: { id: 'c2', name: 'math', inputJSON: JSON.stringify({ a: 1, b: 2 }) },
          },
        ],
      },
    ];
    const out = await client.executeToolCalls(
      messages,
      (name, args) => {
        if (name === 'weather') return { temp_c: 18 };
        return args;
      },
      {
        conversationId: 'conv-loop',
        agentName: 'agent-x',
        agentVersion: '1.0.0',
        requestModel: 'gpt-test',
        requestProvider: 'openai',
      },
    );
    assert.equal(out.length, 2);
    assert.equal(out[0].role, 'tool');
    assert.equal(out[0].name, 'weather');
    assert.equal(out[0].parts[0].toolResult.toolCallId, 'c1');
    assert.equal(out[0].parts[0].toolResult.contentJSON, JSON.stringify({ temp_c: 18 }));
    assert.equal(out[1].parts[0].toolResult.toolCallId, 'c2');

    const names = toolSpanNames(spanExporter);
    assert.equal(names.filter((n) => n === 'execute_tool weather').length, 1);
    assert.equal(names.filter((n) => n === 'execute_tool math').length, 1);
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls propagates executor errors', async () => {
  const { client, spanExporter } = newHarness();
  try {
    const messages = [
      {
        role: 'assistant',
        parts: [{ type: 'tool_call', toolCall: { id: 'c1', name: 'boom', inputJSON: '{}' } }],
      },
    ];
    const out = await client.executeToolCalls(messages, () => {
      throw new Error('tool failed');
    });
    assert.equal(out.length, 1);
    assert.equal(out[0].parts[0].toolResult.isError, true);
    assert.match(out[0].parts[0].toolResult.content, /tool failed/);
    const boom = spanExporter.getFinishedSpans().find((s) => s.name === 'execute_tool boom');
    assert.ok(boom);
    assert.equal(boom.status.code, SpanStatusCode.ERROR);
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls no tool parts', async () => {
  const { client, spanExporter } = newHarness();
  try {
    const out = await client.executeToolCalls(
      [{ role: 'assistant', parts: [{ type: 'text', text: 'hi' }] }],
      () => null,
    );
    assert.deepEqual(out, []);
    assert.deepEqual(toolSpanNames(spanExporter), []);
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls single tool', async () => {
  const { client, spanExporter } = newHarness();
  try {
    const out = await client.executeToolCalls(
      [
        {
          role: 'assistant',
          parts: [{ type: 'tool_call', toolCall: { id: 'id1', name: 'echo', inputJSON: '{"x":1}' } }],
        },
      ],
      (_n, a) => a,
    );
    assert.equal(out.length, 1);
    assert.equal(out[0].parts[0].toolResult.toolCallId, 'id1');
    assert.deepEqual(toolSpanNames(spanExporter), ['execute_tool echo']);
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls empty messages', async () => {
  const { client, spanExporter } = newHarness();
  try {
    assert.deepEqual(await client.executeToolCalls([], () => null), []);
    assert.deepEqual(await client.executeToolCalls(undefined, () => null), []);
    assert.deepEqual(toolSpanNames(spanExporter), []);
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls serializes number/boolean results as contentJSON', async () => {
  const { client } = newHarness();
  try {
    const out = await client.executeToolCalls(
      [
        {
          role: 'assistant',
          parts: [
            { type: 'tool_call', toolCall: { id: 'n1', name: 'num', inputJSON: '{}' } },
            { type: 'tool_call', toolCall: { id: 'b1', name: 'bool', inputJSON: '{}' } },
          ],
        },
      ],
      (name) => (name === 'num' ? 42 : true),
    );
    assert.equal(out.length, 2);
    assert.equal(out[0].parts[0].toolResult.contentJSON, '42');
    assert.equal(out[0].parts[0].toolResult.content, '');
    assert.equal(out[1].parts[0].toolResult.contentJSON, 'true');
    assert.equal(out[1].parts[0].toolResult.content, '');
  } finally {
    await client.shutdown();
  }
});

test('executeToolCalls skips blank tool name', async () => {
  const { client, spanExporter } = newHarness();
  try {
    const out = await client.executeToolCalls(
      [{ role: 'assistant', parts: [{ type: 'tool_call', toolCall: { id: 'x', name: '   ', inputJSON: '{}' } }] }],
      () => 1,
    );
    assert.deepEqual(out, []);
    assert.deepEqual(toolSpanNames(spanExporter), []);
  } finally {
    await client.shutdown();
  }
});
