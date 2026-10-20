import assert from 'node:assert/strict';
import test from 'node:test';
import { SpanStatusCode, trace } from '@opentelemetry/api';
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { Agento11yClient, createSecretRedactionSanitizer, defaultConfig } from '../.test-dist/index.js';

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

test('secret redaction sanitizer redacts assistant and tool content but leaves user input by default', async () => {
  const exporter = new CapturingExporter();
  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
    generationSanitizer: createSecretRedactionSanitizer(),
  });

  try {
    const secretToken = 'glc_abcdefghijklmnopqrstuvwxyz1234';
    const envSecret = 'DATABASE_PASSWORD=hunter2secret123';

    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: `user pasted ${secretToken}` }] }],
      output: [
        {
          role: 'assistant',
          parts: [
            { type: 'text', text: `assistant saw ${secretToken}` },
            { type: 'thinking', thinking: `thinking about ${secretToken}` },
            {
              type: 'tool_call',
              toolCall: {
                id: 'call-1',
                name: 'bash',
                inputJSON: JSON.stringify({ header: `Bearer ${'a'.repeat(30)}`, env: envSecret }),
              },
            },
          ],
        },
        {
          role: 'tool',
          parts: [
            {
              type: 'tool_result',
              toolResult: {
                toolCallId: 'call-1',
                name: 'bash',
                content: `output ${envSecret}`,
              },
            },
          ],
        },
      ],
    });
    recorder.end();

    await client.flush();

    const generation = exporter.requests[0].generations[0];
    assert.match(generation.input[0].parts[0].text, /glc_/);
    assert.doesNotMatch(generation.output[0].parts[0].text, /glc_/);
    assert.match(generation.output[0].parts[0].text, /\[REDACTED:grafana-cloud-token\]/);
    assert.doesNotMatch(generation.output[0].parts[1].thinking, /glc_/);
    assert.doesNotMatch(generation.output[0].parts[2].toolCall.inputJSON, /hunter2secret123|Bearer /);
    assert.match(generation.output[0].parts[2].toolCall.inputJSON, /\[REDACTED:/);
    assert.doesNotMatch(generation.output[1].parts[0].toolResult.content, /hunter2secret123/);
    assert.match(generation.output[1].parts[0].toolResult.content, /\[REDACTED:env-secret-value\]/);
  } finally {
    await client.shutdown();
  }
});

test('secret redaction sanitizer can redact user input when enabled', async () => {
  const sanitizer = createSecretRedactionSanitizer({ redactInputMessages: true });
  const sanitized = sanitizer({
    id: 'gen-1',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    input: [
      {
        role: 'user',
        parts: [{ type: 'text', text: 'key sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' }],
      },
    ],
  });

  assert.doesNotMatch(sanitized.input[0].parts[0].text, /sk-proj-/);
  assert.match(sanitized.input[0].parts[0].text, /\[REDACTED:openai-project-key\]/);
});

test('secret redaction sanitizer can redact email addresses when enabled', async () => {
  const sanitizer = createSecretRedactionSanitizer({ redactEmailAddresses: true });
  const sanitized = sanitizer({
    id: 'gen-2',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    output: [
      {
        role: 'assistant',
        parts: [{ type: 'text', text: 'Send me an email at example@example.com' }],
      },
    ],
  });

  assert.doesNotMatch(sanitized.output[0].parts[0].text, /example@example\.com/);
  assert.match(sanitized.output[0].parts[0].text, /\[REDACTED:email\]/);
});

test('secret redaction sanitizer leaves email addresses alone when disabled', async () => {
  const sanitizer = createSecretRedactionSanitizer({ redactEmailAddresses: false });
  const sanitized = sanitizer({
    id: 'gen-3',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    output: [
      {
        role: 'assistant',
        parts: [{ type: 'text', text: 'Send me an email at example@example.com' }],
      },
    ],
  });

  assert.match(sanitized.output[0].parts[0].text, /example@example\.com/);
});

test('generation sanitizer failure falls back to metadata_only stripping', async () => {
  const exporter = new CapturingExporter();
  const defaults = defaultConfig();
  const warnings = [];
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
    generationSanitizer: () => {
      throw new Error('boom');
    },
    logger: {
      warn(message, ...args) {
        warnings.push([message, ...args]);
      },
    },
  });

  try {
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: 'Top secret title',
      systemPrompt: 'system secret',
    });
    recorder.setResult({
      input: [{ role: 'user', parts: [{ type: 'text', text: 'hello' }] }],
      output: [{ role: 'assistant', parts: [{ type: 'text', text: 'world' }] }],
    });
    recorder.end();

    await client.flush();

    const generation = exporter.requests[0].generations[0];
    assert.equal(generation.metadata['agento11y.sdk.content_capture_mode'], 'metadata_only');
    assert.equal(generation.conversationTitle, '');
    assert.equal(generation.systemPrompt, '');
    assert.equal(generation.input[0].parts[0].text, '');
    assert.equal(generation.output[0].parts[0].text, '');
    assert.equal(warnings.length, 1);
    assert.match(warnings[0][0], /generation sanitization failed/);
  } finally {
    await client.shutdown();
  }
});

test('secret redaction sanitizer redacts systemPrompt, conversationTitle, and callError', async () => {
  const sanitizer = createSecretRedactionSanitizer();
  const apiKey = `sk-proj-${'a'.repeat(48)}`;
  const sanitized = sanitizer({
    id: 'gen-4',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    systemPrompt: `Use API key ${apiKey} to call the service`,
    conversationTitle: `Discussion about ${apiKey}`,
    callError: `API error: invalid key ${apiKey}`,
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
  });

  assert.doesNotMatch(sanitized.systemPrompt, /sk-proj-/);
  assert.match(sanitized.systemPrompt, /\[REDACTED:openai-project-key\]/);
  assert.doesNotMatch(sanitized.conversationTitle, /sk-proj-/);
  assert.match(sanitized.conversationTitle, /\[REDACTED:openai-project-key\]/);
  assert.doesNotMatch(sanitized.callError, /sk-proj-/);
  assert.match(sanitized.callError, /\[REDACTED:openai-project-key\]/);
});

function newSanitizingClient(
  exporter,
  generationSanitizer,
  { tracer = trace.getTracer('agento11y-sdk-js-test'), contentCapture } = {},
) {
  const defaults = defaultConfig();
  return new Agento11yClient({
    tracer,
    contentCapture,
    generationExport: {
      ...defaults.generationExport,
      batchSize: 1,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
    generationSanitizer,
  });
}

test('secret redaction sanitizer also redacts the title and call error copies in metadata', async () => {
  const exporter = new CapturingExporter();
  const client = newSanitizingClient(exporter, createSecretRedactionSanitizer());

  try {
    const apiKey = `sk-proj-${'a'.repeat(48)}`;
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: `Discussion about ${apiKey}`,
    });
    recorder.setCallError(new Error(`API error: invalid key ${apiKey}`));
    recorder.end();

    await client.flush();

    const generation = exporter.requests[0].generations[0];
    assert.match(generation.conversationTitle, /\[REDACTED:openai-project-key\]/);
    assert.match(generation.callError, /\[REDACTED:openai-project-key\]/);
    assert.equal(generation.metadata['agento11y.conversation.title'], generation.conversationTitle);
    assert.equal(generation.metadata.call_error, generation.callError);
    assert.doesNotMatch(JSON.stringify(generation), /sk-proj-/);
  } finally {
    await client.shutdown();
  }
});

test('generation sanitizer that clears the title and call error also drops their metadata copies', async () => {
  const exporter = new CapturingExporter();
  const client = newSanitizingClient(exporter, (generation) => ({
    ...generation,
    conversationTitle: '',
    callError: undefined,
  }));

  try {
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: 'Private title',
    });
    recorder.setCallError(new Error('private error'));
    recorder.end();

    await client.flush();

    const generation = exporter.requests[0].generations[0];
    assert.equal(generation.conversationTitle, '');
    assert.equal(generation.callError, undefined);
    assert.equal('agento11y.conversation.title' in generation.metadata, false);
    assert.equal('call_error' in generation.metadata, false);
  } finally {
    await client.shutdown();
  }
});

function newSpanCapture() {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  return { spanExporter, traceProvider, tracer: traceProvider.getTracer('agento11y-sdk-js-test') };
}

function singleSpan(spanExporter) {
  const spans = spanExporter.getFinishedSpans();
  assert.equal(spans.length, 1);
  return spans[0];
}

test('secret redaction sanitizer also redacts the title and call error on the span', async () => {
  const { spanExporter, traceProvider, tracer } = newSpanCapture();
  const client = newSanitizingClient(new CapturingExporter(), createSecretRedactionSanitizer(), { tracer });

  try {
    const apiKey = `sk-proj-${'a'.repeat(48)}`;
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: `Discussion about ${apiKey}`,
    });
    recorder.setCallError(new Error(`API error: invalid key ${apiKey}`));
    recorder.end();

    const span = singleSpan(spanExporter);
    assert.match(span.attributes['agento11y.conversation.title'], /\[REDACTED:openai-project-key\]/);
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.match(span.status.message, /\[REDACTED:openai-project-key\]/);
    assert.equal(span.events.length, 1);
    assert.match(span.events[0].attributes['exception.message'], /\[REDACTED:openai-project-key\]/);
    assert.doesNotMatch(JSON.stringify([span.attributes, span.status, span.events]), /sk-proj-/);
  } finally {
    await client.shutdown();
    await traceProvider.shutdown();
  }
});

test('generation sanitizer that clears the title and call error leaves neither on the span', async () => {
  const { spanExporter, traceProvider, tracer } = newSpanCapture();
  const client = newSanitizingClient(
    new CapturingExporter(),
    (generation) => ({ ...generation, conversationTitle: '', callError: undefined }),
    { tracer },
  );

  try {
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: 'Private title',
    });
    recorder.setCallError(new Error('private error'));
    recorder.end();

    const span = singleSpan(spanExporter);
    assert.equal(span.attributes['agento11y.conversation.title'], '');
    assert.equal(span.status.code, SpanStatusCode.ERROR);
    assert.ok(span.status.message.length > 0);
    assert.doesNotMatch(JSON.stringify([span.attributes, span.status, span.events]), /Private title|private error/);
  } finally {
    await client.shutdown();
    await traceProvider.shutdown();
  }
});

test('generation sanitizer that clears the title adds no title to a full_with_metadata_spans span', async () => {
  const { spanExporter, traceProvider, tracer } = newSpanCapture();
  const client = newSanitizingClient(
    new CapturingExporter(),
    (generation) => ({ ...generation, conversationTitle: '' }),
    { tracer, contentCapture: 'full_with_metadata_spans' },
  );

  try {
    const recorder = client.startGeneration({
      model: { provider: 'openai', name: 'gpt-5' },
      conversationTitle: 'Private title',
    });
    recorder.end();

    const span = singleSpan(spanExporter);
    assert.equal('agento11y.conversation.title' in span.attributes, false);
  } finally {
    await client.shutdown();
    await traceProvider.shutdown();
  }
});

test('generation sanitizer leaves no title on the span when the generation had none', async () => {
  const { spanExporter, traceProvider, tracer } = newSpanCapture();
  const client = newSanitizingClient(new CapturingExporter(), createSecretRedactionSanitizer(), { tracer });

  try {
    const recorder = client.startGeneration({ model: { provider: 'openai', name: 'gpt-5' } });
    recorder.end();

    const span = singleSpan(spanExporter);
    assert.equal('agento11y.conversation.title' in span.attributes, false);
  } finally {
    await client.shutdown();
    await traceProvider.shutdown();
  }
});

test('secret redaction sanitizer redacts assistant and tool messages in input', async () => {
  const sanitizer = createSecretRedactionSanitizer({ redactInputMessages: false });
  const secretToken = 'glc_abcdefghijklmnopqrstuvwxyz1234';
  const envSecret = 'DATABASE_PASSWORD=hunter2secret123';
  const sanitized = sanitizer({
    id: 'gen-5',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    input: [
      {
        role: 'assistant',
        parts: [
          { type: 'thinking', thinking: `thinking about ${secretToken}` },
          {
            type: 'tool_call',
            toolCall: {
              id: 'call-1',
              name: 'bash',
              inputJSON: JSON.stringify({ token: secretToken }),
            },
          },
        ],
      },
      {
        role: 'tool',
        parts: [
          {
            type: 'tool_result',
            toolResult: {
              toolCallId: 'call-1',
              name: 'bash',
              content: `output ${secretToken} and ${envSecret}`,
            },
          },
        ],
      },
    ],
  });

  assert.doesNotMatch(sanitized.input[0].parts[0].thinking, /glc_/);
  assert.match(sanitized.input[0].parts[0].thinking, /\[REDACTED:grafana-cloud-token\]/);
  assert.doesNotMatch(sanitized.input[0].parts[1].toolCall.inputJSON, /glc_/);
  // `json-secret-field` replaces the whole quoted value of a secret-looking JSON
  // key, so its label wins over the tier 1 one that matched inside the value.
  assert.match(sanitized.input[0].parts[1].toolCall.inputJSON, /\[REDACTED:json-secret-field\]/);
  assert.doesNotMatch(sanitized.input[1].parts[0].toolResult.content, /glc_/);
  assert.doesNotMatch(sanitized.input[1].parts[0].toolResult.content, /hunter2secret123/);
  assert.match(sanitized.input[1].parts[0].toolResult.content, /\[REDACTED:grafana-cloud-token\]/);
  assert.match(sanitized.input[1].parts[0].toolResult.content, /\[REDACTED:env-secret-value\]/);
});

function buildUserSecretGeneration() {
  return {
    id: 'gen-env-1',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    input: [
      {
        role: 'user',
        parts: [{ type: 'text', text: 'key sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' }],
      },
    ],
  };
}

test('createSecretRedactionSanitizer: SIGIL_REDACT_INPUT_MESSAGES truthy aliases enable user input redaction', () => {
  for (const value of ['1', 'true', 'TRUE', 'yes', 'on']) {
    const sanitizer = createSecretRedactionSanitizer({}, { SIGIL_REDACT_INPUT_MESSAGES: value });
    const sanitized = sanitizer(buildUserSecretGeneration());
    assert.match(sanitized.input[0].parts[0].text, /\[REDACTED:openai-project-key\]/, `value=${value}`);
  }
});

test('createSecretRedactionSanitizer: SIGIL_REDACT_INPUT_MESSAGES falsy aliases leave user input untouched', () => {
  for (const value of ['0', 'false', 'FALSE', 'no', 'off']) {
    const sanitizer = createSecretRedactionSanitizer({}, { SIGIL_REDACT_INPUT_MESSAGES: value });
    const sanitized = sanitizer(buildUserSecretGeneration());
    assert.match(sanitized.input[0].parts[0].text, /sk-proj-/, `value=${value}`);
  }
});

test('createSecretRedactionSanitizer: explicit redactInputMessages wins over env', () => {
  const sanitizer = createSecretRedactionSanitizer(
    { redactInputMessages: false },
    { SIGIL_REDACT_INPUT_MESSAGES: 'true' },
  );
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
});

test('createSecretRedactionSanitizer: invalid env value warns and falls back to false', () => {
  const warnings = [];
  const logger = { warn: (message) => warnings.push(message) };
  const sanitizer = createSecretRedactionSanitizer({}, { SIGIL_REDACT_INPUT_MESSAGES: 'maybe' }, logger);
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /SIGIL_REDACT_INPUT_MESSAGES.*maybe/);
});

test('createSecretRedactionSanitizer: blank env value is treated as unset (no warning)', () => {
  const warnings = [];
  const logger = { warn: (message) => warnings.push(message) };
  const sanitizer = createSecretRedactionSanitizer({}, { SIGIL_REDACT_INPUT_MESSAGES: '   ' }, logger);
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
  assert.equal(warnings.length, 0);
});

test('createSecretRedactionSanitizer: AGENTO11Y_REDACT_INPUT_MESSAGES enables user input redaction', () => {
  const sanitizer = createSecretRedactionSanitizer({}, { AGENTO11Y_REDACT_INPUT_MESSAGES: 'true' });
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /\[REDACTED:openai-project-key\]/);
});

test('createSecretRedactionSanitizer: explicit redactInputMessages wins over preferred env', () => {
  const sanitizer = createSecretRedactionSanitizer(
    { redactInputMessages: false },
    { AGENTO11Y_REDACT_INPUT_MESSAGES: 'true' },
  );
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
});

test('createSecretRedactionSanitizer: preferred env wins over legacy', () => {
  const sanitizer = createSecretRedactionSanitizer(
    {},
    { AGENTO11Y_REDACT_INPUT_MESSAGES: 'false', SIGIL_REDACT_INPUT_MESSAGES: 'true' },
  );
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
});

test('createSecretRedactionSanitizer: blank preferred falls through to legacy', () => {
  const sanitizer = createSecretRedactionSanitizer(
    {},
    { AGENTO11Y_REDACT_INPUT_MESSAGES: '   ', SIGIL_REDACT_INPUT_MESSAGES: 'true' },
  );
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /\[REDACTED:openai-project-key\]/);
});

test('createSecretRedactionSanitizer: invalid preferred blocks valid legacy and warns with preferred key', () => {
  const warnings = [];
  const logger = { warn: (message) => warnings.push(message) };
  const sanitizer = createSecretRedactionSanitizer(
    {},
    { AGENTO11Y_REDACT_INPUT_MESSAGES: 'maybe', SIGIL_REDACT_INPUT_MESSAGES: 'true' },
    logger,
  );
  const sanitized = sanitizer(buildUserSecretGeneration());
  assert.match(sanitized.input[0].parts[0].text, /sk-proj-/);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /AGENTO11Y_REDACT_INPUT_MESSAGES.*maybe/);
});
