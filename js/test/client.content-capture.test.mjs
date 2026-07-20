import assert from 'node:assert/strict';
import test from 'node:test';
import { SpanStatusCode } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';
import {
  assertSpanErrorRedacted,
  createContentCaptureEnv,
  LEAK_MARKER,
  MODE_MATRIX,
  STRIPPED_MODES,
} from './_content_capture_env.mjs';

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

// ---------------------------------------------------------------------------
// stripContent — generation-level content stripping
// ---------------------------------------------------------------------------

test('metadata_only strips sensitive content from generation', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
      systemPrompt: 'You are helpful.',
      conversationTitle: 'Weather chat with user',
    });
    recorder.setResult({
      input: [
        { role: 'user', parts: [{ type: 'text', text: 'What is the weather?' }] },
        {
          role: 'tool',
          parts: [
            {
              type: 'tool_result',
              toolResult: { toolCallId: 'call_1', name: 'weather', content: 'sunny 18C', contentJSON: '{"temp":18}' },
            },
          ],
        },
      ],
      output: [
        {
          role: 'assistant',
          parts: [
            { type: 'thinking', thinking: 'let me think about weather' },
            {
              type: 'tool_call',
              toolCall: { id: 'call_1', name: 'weather', inputJSON: '{"city":"Paris"}' },
            },
            { type: 'text', text: "It's 18C and sunny in Paris." },
          ],
        },
      ],
      tools: [
        { name: 'weather', description: 'Get weather info', type: 'function', inputSchemaJSON: '{"type":"object"}' },
      ],
      usage: { inputTokens: 120, outputTokens: 42 },
      stopReason: 'end_turn',
      artifacts: [{ type: 'request', payload: 'raw' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);

    // Sensitive content stripped
    assert.equal(gen.systemPrompt, '');
    assert.equal(gen.conversationTitle, '');
    assert.equal(gen.metadata['agento11y.conversation.title'], undefined);
    assert.equal(gen.artifacts, null);
    assert.equal(gen.input[0].parts[0].text, '');
    assert.equal(gen.output[0].parts[0].thinking, '');
    assert.equal(gen.output[0].parts[1].toolCall.inputJSON, '');
    assert.equal(gen.output[0].parts[2].text, '');
    assert.equal(gen.input[1].parts[0].toolResult.content, '');
    assert.equal(gen.input[1].parts[0].toolResult.contentJSON, '');
    assert.equal(gen.tools[0].description, '');
    assert.equal(gen.tools[0].inputSchemaJSON, '');

    // Structure preserved
    assert.equal(gen.input.length, 2);
    assert.equal(gen.output.length, 1);
    assert.equal(gen.output[0].parts.length, 3);
    assert.equal(gen.input[0].role, 'user');
    assert.equal(gen.output[0].parts[0].type, 'thinking');
    assert.equal(gen.output[0].parts[1].toolCall.name, 'weather');
    assert.equal(gen.output[0].parts[1].toolCall.id, 'call_1');
    assert.equal(gen.input[1].parts[0].toolResult.toolCallId, 'call_1');
    assert.equal(gen.input[1].parts[0].toolResult.name, 'weather');

    // Operational metadata preserved
    assert.equal(gen.tools[0].name, 'weather');
    assert.equal(gen.usage.inputTokens, 120);
    assert.equal(gen.usage.outputTokens, 42);
    assert.equal(gen.stopReason, 'end_turn');
    assert.equal(gen.model.name, 'claude-sonnet-4-5');
    assert.equal(gen.metadata['agento11y.sdk.name'], 'sdk-js');
  } finally {
    await shutdownHarness(harness);
  }
});

test('metadata_only replaces callError with category', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setCallError(new Error('rate limit exceeded: 429 Too Many Requests'));
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    const gen = singleGeneration(harness.client);
    assert.equal(gen.callError, 'rate_limit');
    assert.equal(gen.metadata.call_error, undefined);
  } finally {
    await shutdownHarness(harness);
  }
});

test('metadata_only falls back to sdk_error without category', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setCallError(new Error('something went wrong'));
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    const gen = singleGeneration(harness.client);
    assert.equal(gen.callError, 'sdk_error');
  } finally {
    await shutdownHarness(harness);
  }
});

test('metadata_only does not leak raw callError into OTel span', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const rawError = 'rate limit exceeded: 429 Too Many Requests';
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setCallError(new Error(rawError));
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.status.message, 'rate_limit');
    assert.notEqual(span.status.message, rawError);
    assert.equal(span.attributes['error.category'], 'rate_limit', 'error.category attribute must be rate_limit');

    for (const event of span.events) {
      if (event.name === 'exception') {
        const msg = event.attributes?.['exception.message'];
        assert.notEqual(msg, rawError, 'span exception event must not contain raw error');
        assert.equal(msg, 'rate_limit');
      }
    }
  } finally {
    await shutdownHarness(harness);
  }
});

test('metadata_only does not leak conversationTitle into OTel span', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const sensitiveTitle = 'Secret project discussion with John';
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: sensitiveTitle,
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    const gen = singleGeneration(harness.client);
    assert.equal(gen.conversationTitle, '', 'conversationTitle should be stripped from generation');
    assert.equal(gen.metadata['agento11y.conversation.title'], undefined, 'metadata key should be deleted');

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(
      'agento11y.conversation.title' in span.attributes,
      false,
      'span must not carry agento11y.conversation.title under metadata_only',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

test('metadata_only span uses sdk_error for uncategorized callError', async () => {
  const harness = newHarness({ contentCapture: 'metadata_only' });

  try {
    const rawError = 'something completely unexpected happened';
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setCallError(new Error(rawError));
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    const span = singleGenerationSpan(harness.spanExporter);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.equal(span.status.message, 'sdk_error');
    assert.notEqual(span.status.message, rawError);
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// Content capture mode stamping in generation metadata
// ---------------------------------------------------------------------------

test('content capture mode is stamped in generation metadata', async () => {
  const cases = [
    { clientMode: 'default', genMode: undefined, wantMarker: 'no_tool_content', wantStripped: false },
    { clientMode: 'metadata_only', genMode: undefined, wantMarker: 'metadata_only', wantStripped: true },
    { clientMode: 'full', genMode: 'metadata_only', wantMarker: 'metadata_only', wantStripped: true },
    { clientMode: 'metadata_only', genMode: 'full', wantMarker: 'full', wantStripped: false },
    { clientMode: 'full', genMode: undefined, wantMarker: 'full', wantStripped: false },
    {
      clientMode: 'full_with_metadata_spans',
      genMode: undefined,
      wantMarker: 'full_with_metadata_spans',
      // FULL_WITH_METADATA_SPANS keeps proto content full.
      wantStripped: false,
    },
  ];

  for (const tc of cases) {
    const harness = newHarness({ contentCapture: tc.clientMode });

    try {
      const recorder = harness.client.startGeneration({
        model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
        contentCapture: tc.genMode,
      });
      recorder.setResult({
        systemPrompt: 'You are helpful.',
        input: [{ role: 'user', parts: [{ type: 'text', text: 'Hello' }] }],
        output: [{ role: 'assistant', parts: [{ type: 'text', text: 'Hi there' }] }],
        usage: { inputTokens: 10, outputTokens: 5 },
      });
      recorder.end();
      assert.equal(recorder.getError(), undefined, `case ${tc.clientMode}/${tc.genMode}: unexpected error`);

      const gen = singleGeneration(harness.client);
      assert.equal(
        gen.metadata['agento11y.sdk.content_capture_mode'],
        tc.wantMarker,
        `case ${tc.clientMode}/${tc.genMode}: marker`,
      );

      const stripped = gen.input[0].parts[0].text === '';
      assert.equal(stripped, tc.wantStripped, `case ${tc.clientMode}/${tc.genMode}: stripped`);

      // Structure always preserved
      assert.equal(gen.input.length, 1);
      assert.equal(gen.input[0].role, 'user');
      assert.equal(gen.usage.inputTokens, 10);
    } finally {
      await shutdownHarness(harness);
    }
  }
});

// ---------------------------------------------------------------------------
// Content capture with resolver
// ---------------------------------------------------------------------------

test('contentCaptureResolver overrides client mode', async () => {
  const harness = newHarness({
    contentCapture: 'full',
    contentCaptureResolver: () => 'metadata_only',
  });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'test', name: 'test-model' },
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'metadata_only');
    assert.equal(gen.input[0].parts[0].text, '');
  } finally {
    await shutdownHarness(harness);
  }
});

test('per-generation contentCapture overrides resolver', async () => {
  const harness = newHarness({
    contentCapture: 'default',
    contentCaptureResolver: () => 'metadata_only',
  });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'test', name: 'test-model' },
      contentCapture: 'full',
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'full');
    assert.equal(gen.input[0].parts[0].text, 'hello');
  } finally {
    await shutdownHarness(harness);
  }
});

test('resolver returning default defers to client mode', async () => {
  const harness = newHarness({
    contentCapture: 'metadata_only',
    contentCaptureResolver: () => 'default',
  });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'test', name: 'test-model' },
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'metadata_only');
    assert.equal(gen.input[0].parts[0].text, '');
  } finally {
    await shutdownHarness(harness);
  }
});

test('throwing resolver fails closed to metadata_only', async () => {
  const harness = newHarness({
    contentCapture: 'full',
    contentCaptureResolver: () => {
      throw new Error('resolver bug');
    },
  });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'test', name: 'test-model' },
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'metadata_only');
    assert.equal(gen.input[0].parts[0].text, '');
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// Tool execution content capture
// ---------------------------------------------------------------------------

test('tool execution content capture modes', async () => {
  const cases = [
    // client Default (→ NoToolContent), legacy controls.
    {
      name: 'client default, legacy false — suppressed',
      clientMode: 'default',
      toolMode: undefined,
      legacy: false,
      wantContent: false,
    },
    {
      name: 'client default, legacy true — included',
      clientMode: 'default',
      toolMode: undefined,
      legacy: true,
      wantContent: true,
    },
    // Explicit Full client — legacy irrelevant.
    {
      name: 'client full, legacy false — included',
      clientMode: 'full',
      toolMode: undefined,
      legacy: false,
      wantContent: true,
    },
    {
      name: 'client full, legacy true — included',
      clientMode: 'full',
      toolMode: undefined,
      legacy: true,
      wantContent: true,
    },
    // Client MetadataOnly — always suppressed.
    {
      name: 'client metadata_only, legacy true — suppressed',
      clientMode: 'metadata_only',
      toolMode: undefined,
      legacy: true,
      wantContent: false,
    },
    // Per-tool overrides.
    {
      name: 'tool full overrides client metadata_only',
      clientMode: 'metadata_only',
      toolMode: 'full',
      legacy: false,
      wantContent: true,
    },
    {
      name: 'tool metadata_only overrides client full',
      clientMode: 'full',
      toolMode: 'metadata_only',
      legacy: true,
      wantContent: false,
    },
  ];

  for (const tc of cases) {
    const harness = newHarness({ contentCapture: tc.clientMode });

    try {
      const recorder = harness.client.startToolExecution({
        toolName: 'test_tool',
        contentCapture: tc.toolMode,
        includeContent: tc.legacy,
      });
      recorder.setResult({
        arguments: { city: 'Paris' },
        result: { temp: 18 },
      });
      recorder.end();
      assert.equal(recorder.getError(), undefined, `case "${tc.name}": unexpected error`);

      const span = singleToolSpan(harness.spanExporter);
      const hasArgs = 'gen_ai.tool.call.arguments' in span.attributes;
      assert.equal(
        hasArgs,
        tc.wantContent,
        `case "${tc.name}": tool arguments present=${hasArgs}, want=${tc.wantContent}`,
      );

      assert.ok('gen_ai.tool.name' in span.attributes, `case "${tc.name}": tool name always present`);
    } finally {
      await shutdownHarness(harness);
    }
  }
});

test('tool execution with resolver overriding client mode', async () => {
  const harness = newHarness({
    contentCapture: 'full',
    contentCaptureResolver: () => 'metadata_only',
  });

  try {
    const recorder = harness.client.startToolExecution({
      toolName: 'test_tool',
      includeContent: true,
    });
    recorder.setResult({
      arguments: 'args',
      result: 'result',
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleToolSpan(harness.spanExporter);
    assert.equal(
      'gen_ai.tool.call.arguments' in span.attributes,
      false,
      'resolver metadata_only should suppress tool content',
    );
  } finally {
    await shutdownHarness(harness);
  }
});

test('per-tool contentCapture full overrides resolver metadata_only', async () => {
  const harness = newHarness({
    contentCapture: 'default',
    contentCaptureResolver: () => 'metadata_only',
  });

  try {
    const recorder = harness.client.startToolExecution({
      toolName: 'test_tool',
      contentCapture: 'full',
      includeContent: false,
    });
    recorder.setResult({
      arguments: 'args',
      result: 'result',
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const span = singleToolSpan(harness.spanExporter);
    assert.equal('gen_ai.tool.call.arguments' in span.attributes, true, 'per-tool full should override resolver');
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// full mode preserves all content
// ---------------------------------------------------------------------------

test('full mode preserves all generation content', async () => {
  const harness = newHarness({ contentCapture: 'full' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
      systemPrompt: 'You are helpful.',
      conversationTitle: 'Math question',
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'What is 2+2?' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: '4' }] }],
      usage: { inputTokens: 10, outputTokens: 1 },
      artifacts: [{ type: 'request', payload: '{}' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'full');
    assert.equal(gen.systemPrompt, 'You are helpful.');
    assert.equal(gen.conversationTitle, 'Math question');
    assert.equal(gen.input[0].parts[0].text, 'What is 2+2?');
    assert.equal(gen.output[0].parts[0].text, '4');
    assert.equal(gen.artifacts.length, 1);
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// no_tool_content mode preserves generation content
// ---------------------------------------------------------------------------

test('no_tool_content preserves generation content', async () => {
  const harness = newHarness({ contentCapture: 'no_tool_content' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      systemPrompt: 'Be concise.',
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'Hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'Hi!' }] }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = singleGeneration(harness.client);
    assert.equal(gen.metadata['agento11y.sdk.content_capture_mode'], 'no_tool_content');
    assert.equal(gen.systemPrompt, 'Be concise.');
    assert.equal(gen.input[0].parts[0].text, 'Hello');
    assert.equal(gen.output[0].parts[0].text, 'Hi!');
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// exported generation payload has content capture mode stamped
// ---------------------------------------------------------------------------

test('exported generation includes content capture mode metadata', async () => {
  const harness = newHarness({ contentCapture: 'full' });

  try {
    const recorder = harness.client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      output: [{ role: 'assistant', content: 'ok' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    await harness.client.flush();
    assert.equal(harness.generationExporter.requests.length, 1);
    const exported = harness.generationExporter.requests[0].generations[0];
    assert.equal(exported.metadata['agento11y.sdk.content_capture_mode'], 'full');
  } finally {
    await shutdownHarness(harness);
  }
});

// ---------------------------------------------------------------------------
// Mode × surface coverage matrix
// ---------------------------------------------------------------------------
//
// One full-content fixture run through every mode, asserted via MODE_MATRIX.
// Catches gaps in any mode without writing four separate tests per surface.

function matrixFixtureResult() {
  // SDK API uses *JSON-suffix camelCase fields (inputJSON, contentJSON,
  // inputSchemaJSON) which map to bytes input_json / content_json /
  // input_schema_json on the proto. After gRPC roundtrip the receiving side
  // sees those as inputJson / contentJson / inputSchemaJson Buffer values.
  return {
    systemPrompt: 'You are helpful.',
    input: [
      { role: 'user', parts: [{ type: 'text', text: 'What is the weather?' }] },
      {
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            toolResult: {
              toolCallId: 'call_1',
              name: 'weather',
              content: 'sunny 18C',
              contentJSON: '{"temp":18}',
            },
          },
        ],
      },
    ],
    output: [
      {
        role: 'assistant',
        parts: [
          { type: 'thinking', thinking: 'let me think about weather' },
          {
            type: 'tool_call',
            toolCall: { id: 'call_1', name: 'weather', inputJSON: '{"city":"Paris"}' },
          },
          { type: 'text', text: "It's 18C and sunny in Paris." },
        ],
      },
    ],
    tools: [
      {
        name: 'weather',
        description: 'Get weather',
        type: 'function',
        inputSchemaJSON: '{"type":"object"}',
      },
    ],
    usage: { inputTokens: 120, outputTokens: 42 },
    stopReason: 'end_turn',
  };
}

function assertProtoContent(field, actual, expected, expectStripped) {
  // After gRPC roundtrip, bytes fields come back as Buffer; string fields
  // stay as strings. Either an empty string or zero-length Buffer counts as
  // stripped.
  if (expectStripped) {
    const isEmpty =
      actual === '' ||
      actual === undefined ||
      (Buffer.isBuffer(actual) && actual.length === 0) ||
      (actual instanceof Uint8Array && actual.length === 0);
    assert.ok(isEmpty, `${field} should be stripped, got ${JSON.stringify(actual)}`);
    return;
  }
  const actualStr =
    Buffer.isBuffer(actual) || actual instanceof Uint8Array ? Buffer.from(actual).toString('utf8') : actual;
  assert.equal(actualStr, expected, field);
}

for (const expect of MODE_MATRIX) {
  test(`mode matrix: ${expect.marker} — generation proto + span`, async () => {
    const env = await createContentCaptureEnv({ contentCapture: expect.mode });

    try {
      const title = 'Sensitive conversation';
      const recorder = env.client.startGeneration({
        model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
        conversationTitle: title,
        systemPrompt: 'You are helpful.',
      });
      recorder.setResult(matrixFixtureResult());
      recorder.end();
      assert.equal(recorder.getError(), undefined);

      const gen = await env.singleGeneration();
      assert.equal(gen.metadata?.fields?.['agento11y.sdk.content_capture_mode']?.stringValue, expect.marker);

      // Content fields: stripped only under METADATA_ONLY.
      assertProtoContent('system_prompt', gen.systemPrompt, 'You are helpful.', expect.protoContentStripped);
      assertProtoContent(
        'input[0].text',
        gen.input[0].parts[0].text,
        'What is the weather?',
        expect.protoContentStripped,
      );
      assertProtoContent(
        'output[0].thinking',
        gen.output[0].parts[0].thinking,
        'let me think about weather',
        expect.protoContentStripped,
      );
      assertProtoContent(
        'output[0].tool_call.input_json',
        gen.output[0].parts[1].toolCall.inputJson,
        '{"city":"Paris"}',
        expect.protoContentStripped,
      );
      assertProtoContent(
        'output[0].text',
        gen.output[0].parts[2].text,
        "It's 18C and sunny in Paris.",
        expect.protoContentStripped,
      );
      assertProtoContent(
        'input[1].tool_result.content',
        gen.input[1].parts[0].toolResult.content,
        'sunny 18C',
        expect.protoContentStripped,
      );
      assertProtoContent('tools[0].description', gen.tools[0].description, 'Get weather', expect.protoContentStripped);
      assertProtoContent(
        'tools[0].input_schema_json',
        gen.tools[0].inputSchemaJson,
        '{"type":"object"}',
        expect.protoContentStripped,
      );

      // Structural fields always preserved. inputTokens is serialized as a
      // string under protoLoader's longs: String option.
      assert.equal(gen.input.length, 2);
      assert.equal(gen.output[0].parts[1].toolCall.name, 'weather');
      assert.equal(Number(gen.usage.inputTokens), 120);

      // Conversation title metadata mirror: present iff the proto keeps it.
      const titleMirror = gen.metadata?.fields?.['agento11y.conversation.title']?.stringValue;
      if (expect.protoContentStripped) {
        assert.ok(!titleMirror, `expected title mirror to be absent, got ${titleMirror}`);
      } else {
        assert.equal(titleMirror, title);
      }

      // Span path: title presence is what the mode advertises.
      const span = env.generationSpan();
      if (expect.spanTitlePresent) {
        assert.equal(span.attributes['agento11y.conversation.title'], title);
      } else {
        assert.equal('agento11y.conversation.title' in span.attributes, false);
      }
    } finally {
      await env.close();
    }
  });

  test(`mode matrix: ${expect.marker} — generation call_error`, async () => {
    const env = await createContentCaptureEnv({ contentCapture: expect.mode });

    try {
      const rawError = `provider returned HTTP 400: blocked content '${LEAK_MARKER}'`;
      const recorder = env.client.startGeneration({
        model: { provider: 'openai', name: 'gpt-5' },
        agentName: 'agent-matrix-error',
      });
      recorder.setCallError(new Error(rawError));
      recorder.setResult({
        input: [{ role: 'user', parts: [{ type: 'text', text: 'x' }] }],
        output: [{ role: 'assistant', parts: [{ type: 'text', text: 'y' }] }],
        usage: { inputTokens: 1, outputTokens: 1 },
      });
      recorder.end();
      assert.equal(recorder.getError(), undefined);

      const gen = await env.singleGeneration();
      if (expect.protoCallErrorRaw) {
        assert.equal(gen.callError, rawError);
        assert.equal(gen.metadata?.fields?.call_error?.stringValue, rawError);
      } else {
        assert.notEqual(gen.callError, rawError);
        assert.ok(gen.callError, 'proto.callError should be non-empty error category');
        assert.equal(gen.metadata?.fields?.call_error, undefined);
      }

      const span = env.generationSpan();
      if (expect.spanRawError) {
        assert.ok((span.status.message ?? '').includes(LEAK_MARKER), 'span should echo raw error');
      } else {
        assertSpanErrorRedacted(span, 'provider_call_error');
      }
    } finally {
      await env.close();
    }
  });
}

test('streaming full_with_metadata_spans — proto full, span title absent', async () => {
  // Streaming changes the span operation name to streamText but the
  // redaction logic is shared with non-streaming. Catches regressions where
  // the two paths drift apart.
  const env = await createContentCaptureEnv({ contentCapture: 'full_with_metadata_spans' });

  try {
    const title = 'Sensitive streaming conversation';
    const recorder = env.client.startStreamingGeneration({
      model: { provider: 'anthropic', name: 'claude-sonnet-4-5' },
      conversationTitle: title,
      systemPrompt: 'Be helpful.',
    });
    recorder.setResult({
      systemPrompt: 'Be helpful.',
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'hi' }] }],
      usage: { inputTokens: 1, outputTokens: 1 },
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    const gen = await env.singleGeneration();
    assert.equal(gen.systemPrompt, 'Be helpful.');
    assert.equal(gen.input[0].parts[0].text, 'hello');
    assert.equal(gen.metadata?.fields?.['agento11y.conversation.title']?.stringValue, title);
    assert.equal(gen.metadata?.fields?.['agento11y.sdk.content_capture_mode']?.stringValue, 'full_with_metadata_spans');

    const streamSpan = env.streamingGenerationSpan();
    assert.equal('agento11y.conversation.title' in streamSpan.attributes, false);
  } finally {
    await env.close();
  }
});

// Tool span content omission and embedding span content omission both apply
// to metadata_only and full_with_metadata_spans. Embeddings have no proto
// export, and the tool path doesn't have one either, so both modes are
// equivalent on the span path.
for (const mode of STRIPPED_MODES) {
  test(`${mode} tool span omits content attributes`, async () => {
    // The full set of content-bearing attributes the tool span can carry.
    // Under either stripped mode none of them should appear.
    const env = await createContentCaptureEnv({ contentCapture: mode });

    try {
      const recorder = env.client.startToolExecution({
        toolName: 'weather',
        toolCallId: 'call_1',
        includeContent: true,
        conversationTitle: 'Sensitive tool title',
        toolDescription: 'Get weather: free-form provider-supplied text',
      });
      recorder.setResult({ arguments: { city: 'Paris' }, result: { temp_c: 18 } });
      recorder.end();
      assert.equal(recorder.getError(), undefined);

      const span = env.toolSpan();
      assert.equal('gen_ai.tool.call.arguments' in span.attributes, false, 'tool args must be absent');
      assert.equal('gen_ai.tool.call.result' in span.attributes, false, 'tool result must be absent');
      assert.equal('agento11y.conversation.title' in span.attributes, false, 'conversation title must be absent');
      assert.equal('gen_ai.tool.description' in span.attributes, false, 'tool description must be absent');
      // Identity attributes still emitted.
      assert.equal(span.attributes['gen_ai.tool.name'], 'weather');
    } finally {
      await env.close();
    }
  });

  test(`${mode} tool span redacts raw provider callError`, async () => {
    // Tools have no proto export — the raw provider error must not echo on
    // the span path under either stripped mode.
    const env = await createContentCaptureEnv({ contentCapture: mode });

    try {
      const rawError = `provider returned HTTP 400: blocked content '${LEAK_MARKER}'`;
      const recorder = env.client.startToolExecution({
        toolName: 'weather',
        toolCallId: 'call_1',
        includeContent: true,
      });
      recorder.setCallError(new Error(rawError));
      recorder.setResult({ arguments: { city: 'Paris' }, result: { temp_c: 18 } });
      recorder.end();

      assertSpanErrorRedacted(env.toolSpan(), 'tool_execution_error');
    } finally {
      await env.close();
    }
  });

  test(`${mode} embedding span omits input_texts even when captureInput=true`, async () => {
    const env = await createContentCaptureEnv({
      contentCapture: mode,
      embeddingCapture: { captureInput: true, maxInputItems: 5, maxTextLength: 100 },
    });

    try {
      const recorder = env.client.startEmbedding({
        model: { provider: 'openai', name: 'text-embedding-3-small' },
      });
      recorder.setResult({
        inputCount: 1,
        inputTokens: 10,
        inputTexts: ['sensitive input text'],
        responseModel: 'text-embedding-3-small',
      });
      recorder.end();
      assert.equal(recorder.getError(), undefined);

      const span = env.embeddingSpan();
      assert.equal('gen_ai.embeddings.input_texts' in span.attributes, false, 'input_texts must be absent');
      // Non-content embedding span fields remain.
      assert.equal(span.attributes['gen_ai.embeddings.input_count'], 1);
      assert.equal(span.attributes['gen_ai.usage.input_tokens'], 10);
      assert.equal(span.attributes['gen_ai.response.model'], 'text-embedding-3-small');
    } finally {
      await env.close();
    }
  });

  test(`${mode} embedding span redacts raw provider callError`, async () => {
    // Embeddings have no proto export, so the raw provider error must not
    // echo on the span path under either stripped mode.
    const env = await createContentCaptureEnv({
      contentCapture: mode,
      embeddingCapture: { captureInput: true, maxInputItems: 5, maxTextLength: 100 },
    });

    try {
      const rawError = `provider returned HTTP 400: blocked content '${LEAK_MARKER}'`;
      const recorder = env.client.startEmbedding({
        model: { provider: 'openai', name: 'text-embedding-3-small' },
      });
      recorder.setCallError(new Error(rawError));
      recorder.setResult({ inputCount: 1, inputTexts: ['sensitive input text'] });
      recorder.end();

      assertSpanErrorRedacted(env.embeddingSpan(), 'provider_call_error');
    } finally {
      await env.close();
    }
  });
}

test('resolver returning full_with_metadata_spans hides embedding input_texts (client default full)', async () => {
  const env = await createContentCaptureEnv({
    contentCapture: 'full',
    contentCaptureResolver: () => 'full_with_metadata_spans',
    embeddingCapture: { captureInput: true, maxInputItems: 5, maxTextLength: 100 },
  });

  try {
    const recorder = env.client.startEmbedding({
      model: { provider: 'openai', name: 'text-embedding-3-small' },
    });
    recorder.setResult({ inputCount: 1, inputTexts: ['resolver-gated sensitive text'] });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    assert.equal('gen_ai.embeddings.input_texts' in env.embeddingSpan().attributes, false);
  } finally {
    await env.close();
  }
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function newHarness(overrides = {}) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('agento11y-sdk-js-test');
  const generationExporter = new CapturingExporter();
  const defaults = defaultConfig();

  const { contentCapture, contentCaptureResolver, embeddingCapture, ...exportOverrides } = overrides;

  const client = new Agento11yClient({
    tracer,
    generationExport: {
      ...defaults.generationExport,
      batchSize: 100,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
      ...exportOverrides,
    },
    contentCapture,
    contentCaptureResolver,
    embeddingCapture,
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

function singleGenerationSpan(spanExporter) {
  const spans = generationSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}

function toolSpans(spanExporter) {
  return spanExporter.getFinishedSpans().filter((span) => span.attributes['gen_ai.operation.name'] === 'execute_tool');
}

function singleToolSpan(spanExporter) {
  const spans = toolSpans(spanExporter);
  assert.equal(spans.length, 1);
  return spans[0];
}
