import assert from 'node:assert/strict';
import test from 'node:test';
import { trace } from '@opentelemetry/api';
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

test('validation rejects tool_call part for user role and reports input path', async () => {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    const recorder = client.startGeneration({
      model: {
        provider: 'anthropic',
        name: 'claude-sonnet-4-5',
      },
    });
    recorder.setResult({
      input: [
        {
          role: 'user',
          parts: [
            {
              type: 'tool_call',
              toolCall: {
                name: 'weather',
              },
            },
          ],
        },
      ],
      output: [{ role: 'assistant', content: 'ok' }],
    });
    recorder.end();

    assert.match(
      recorder.getError()?.message ?? '',
      /generation\.input\[0\].parts\[0\].tool_call only allowed for assistant role/,
    );
    await client.flush();
    assert.equal(exporter.requests.length, 0);
  } finally {
    await client.shutdown();
  }
});

test('validation rejects tool_result part for assistant role', async () => {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    const recorder = client.startGeneration({
      model: {
        provider: 'anthropic',
        name: 'claude-sonnet-4-5',
      },
    });
    recorder.setResult({
      input: [
        {
          role: 'assistant',
          parts: [
            {
              type: 'tool_result',
              toolResult: {
                toolCallId: 'toolu_1',
                content: 'sunny',
              },
            },
          ],
        },
      ],
      output: [{ role: 'assistant', content: 'ok' }],
    });
    recorder.end();

    assert.match(
      recorder.getError()?.message ?? '',
      /generation\.input\[0\].parts\[0\].tool_result only allowed for tool role/,
    );
    await client.flush();
    assert.equal(exporter.requests.length, 0);
  } finally {
    await client.shutdown();
  }
});

test('validation rejects thinking part for non-assistant roles and reports output path', async () => {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    const recorder = client.startGeneration({
      model: {
        provider: 'anthropic',
        name: 'claude-sonnet-4-5',
      },
    });
    recorder.setResult({
      input: [{ role: 'user', content: 'hello' }],
      output: [
        {
          role: 'user',
          parts: [
            {
              type: 'thinking',
              thinking: 'private reasoning',
            },
          ],
        },
      ],
    });
    recorder.end();

    assert.match(
      recorder.getError()?.message ?? '',
      /generation\.output\[0\].parts\[0\].thinking only allowed for assistant role/,
    );
    await client.flush();
    assert.equal(exporter.requests.length, 0);
  } finally {
    await client.shutdown();
  }
});

test('validation accepts conversation and response fields on valid payloads', async () => {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    const recorder = client.startGeneration({
      conversationId: 'conv-1',
      model: {
        provider: 'anthropic',
        name: 'claude-sonnet-4-5',
      },
    });
    recorder.setResult({
      responseId: 'resp-1',
      responseModel: 'claude-sonnet-4-5-20260201',
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'hi' }] }],
    });
    recorder.end();

    assert.equal(recorder.getError(), undefined);
    await client.flush();
    assert.equal(exporter.requests.length, 1);
    assert.equal(exporter.requests[0].generations[0].conversationId, 'conv-1');
    assert.equal(exporter.requests[0].generations[0].responseId, 'resp-1');
    assert.equal(exporter.requests[0].generations[0].responseModel, 'claude-sonnet-4-5-20260201');
  } finally {
    await client.shutdown();
  }
});

test('validation accepts artifacts with recordId and no payload', async () => {
  const exporter = new CapturingExporter();
  const client = newClient(exporter);

  try {
    const recorder = client.startGeneration({
      model: {
        provider: 'openai',
        name: 'gpt-5',
      },
    });
    recorder.setResult({
      output: [{ role: 'assistant', content: 'ok' }],
      artifacts: [
        {
          type: 'request',
          recordId: 'record-1',
        },
      ],
    });
    recorder.end();

    assert.equal(recorder.getError(), undefined);
    await client.flush();
    assert.equal(exporter.requests.length, 1);
  } finally {
    await client.shutdown();
  }
});

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
