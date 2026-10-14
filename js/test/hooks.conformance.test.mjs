// Hook wire conformance for the JS SDK.
//
// Checks the request the SDK serializes and the responses it parses against the
// shared fixtures in `conformance/hooks/`, which are the only contract for an
// endpoint with no generated stubs.

import assert from 'node:assert/strict';
import test from 'node:test';
import { HookTransformError } from '../.test-dist/core.js';
import { evaluateHook } from '../.test-dist/hooks.js';
import {
  bashToolSchema,
  diffJson,
  loadPostflightGuardRequest,
  loadPreflightRequest,
  loadResponses,
  postflightGuardRequest,
  preflightRequest,
} from './hooksFixtures.mjs';

const hooksConfig = { enabled: true, phases: ['preflight', 'postflight'], timeoutMs: 5_000, failOpen: false };

/** Runs evaluateHook against a stub fetch, returning the serialized body and the parsed response. */
async function runHook(request, responseBody, { status = 200, ...overrides } = {}) {
  let capturedBody;
  const fetchImpl = async (_url, init) => {
    capturedBody = JSON.parse(init.body);
    return {
      ok: status >= 200 && status < 300,
      status,
      text: async () => JSON.stringify(responseBody),
    };
  };
  const response = await evaluateHook({
    apiEndpoint: 'http://127.0.0.1:9/agent-observability',
    insecure: true,
    extraHeaders: undefined,
    hooks: { ...hooksConfig, ...overrides },
    request,
    fetchImpl,
  });
  return { body: capturedBody, response };
}

for (const [fixtureName, build, load] of [
  ['request-preflight.json', preflightRequest, loadPreflightRequest],
  ['request-postflight-guard.json', postflightGuardRequest, loadPostflightGuardRequest],
]) {
  test(`hook request matches ${fixtureName}`, async () => {
    const { body } = await runHook(build(), { action: 'allow', evaluations: [] });
    const diffs = diffJson(body, load());
    assert.deepEqual(diffs, [], `request does not match conformance/hooks/${fixtureName}:\n${diffs.join('\n')}`);
  });
}

for (const [name, payload] of Object.entries(loadResponses())) {
  test(`strict decoding accepts fixture ${name}`, async () => {
    const normal = await runHook(preflightRequest(), payload);
    const strict = await runHook(preflightRequest(), payload, { strictTransforms: true });
    assert.deepEqual(strict.response, normal.response);
  });
}

for (const transformedInput of [
  { messages: [{ role: 1, content: 'kept' }], systemPrompt: 'system', conversationPreview: 'preview' },
  { output: [{ role: 2, parts: [{ payload: { Text: 'output' } }] }] },
  { messages: [{ role: 3, parts: [{ tool_result: { content_json: { ok: true } } }] }] },
  { messages: [{ role: 'assistant', parts: [{ toolCall: { name: 'Bash', inputJson: ['ls'] } }] }] },
  { tools: [{ name: 'Bash', inputSchemaJson: '{"type":"object"}' }] },
]) {
  test(`strict decoding accepts alternate input ${JSON.stringify(transformedInput)}`, async () => {
    const payload = { action: 'allow', transformed_input: transformedInput };
    const normal = await runHook(preflightRequest(), payload);
    const strict = await runHook(preflightRequest(), payload, { strictTransforms: true });
    assert.deepEqual(strict.response, normal.response);
  });
}

const callOutput = (part) => ({ output: [{ role: 'assistant', parts: [part] }] });
const malformedJSON = '{"secret":"ORIGINAL_SECRET';
const malformedBase64 = Buffer.from(malformedJSON).toString('base64');
const originalCall = { id: 'call-1', name: 'Bash', input_json: { command: 'ORIGINAL_SECRET' } };
const redactedCall = { id: 'call-1', name: 'Bash', inputJSON: '{"command":"REDACTED"}' };
const conflictingTransforms = [
  ['tool-call aliases', callOutput({ tool_call: originalCall, toolCall: redactedCall })],
  ...['inputJson', 'inputJSON'].map((alias) => [
    `argument alias ${alias}`,
    callOutput({ tool_call: { ...originalCall, [alias]: redactedCall.inputJSON } }),
  ]),
  ['system aliases', { system_prompt: 'ORIGINAL_SECRET', systemPrompt: 'REDACTED' }],
  ['preview aliases', { conversation_preview: 'ORIGINAL_SECRET', conversationPreview: 'REDACTED' }],
  ['schema aliases', { tools: [{ name: 'Bash', input_schema_json: '{"a":1}', inputSchemaJSON: '{"a":2}' }] }],
  ['result aliases', callOutput({ tool_result: { content: 'original' }, toolResult: { content: 'redacted' } })],
  ['result content aliases', callOutput({ toolResult: { content_json: { a: 1 }, contentJSON: '{"a":2}' } })],
  ['result ID aliases', callOutput({ toolResult: { tool_call_id: 'one', toolCallId: 'two' } })],
  ['result error aliases', callOutput({ toolResult: { is_error: false, isError: true } })],
  [
    'malformed argument aliases',
    callOutput({ toolCall: { name: 'Bash', input_json: malformedJSON, inputJSON: '{}' } }),
  ],
  [
    'malformed tool-call aliases',
    callOutput({ tool_call: { name: 'Bash', input_json: malformedJSON }, toolCall: { name: 'Bash', inputJSON: '{}' } }),
  ],
  ['malformed result content aliases', callOutput({ toolResult: { content_json: malformedJSON, contentJSON: '{}' } })],
  [
    'malformed result aliases',
    callOutput({ tool_result: { content_json: malformedJSON }, toolResult: { contentJSON: '{}' } }),
  ],
  ['malformed schema aliases', { tools: [{ name: 'Bash', input_schema_json: malformedJSON, inputSchemaJSON: '{}' }] }],
  ['nested and top-level calls', callOutput({ Payload: { ToolCall: originalCall }, toolCall: redactedCall })],
  ['payload aliases', callOutput({ Payload: { ToolCall: originalCall }, payload: { ToolCall: redactedCall } })],
  ['nested and top-level text', callOutput({ Payload: { Text: 'original' }, text: 'redacted' })],
  ['nested and top-level thinking', callOutput({ Payload: { Thinking: 'original' }, thinking: 'redacted' })],
];

const equivalentTransforms = [
  ['system aliases', { system_prompt: 'redacted', systemPrompt: 'redacted' }],
  ['preview aliases', { conversation_preview: 'redacted', conversationPreview: 'redacted' }],
  [
    'argument aliases',
    callOutput({
      tool_call: {
        name: 'Bash',
        input_json: Buffer.from('{"a":1,"b":2}').toString('base64'),
        inputJson: '{ "b": 2, "a": 1 }',
        inputJSON: { a: 1, b: 2 },
      },
    }),
  ],
  [
    'tool-call aliases',
    callOutput({ tool_call: { name: 'Bash', input_json: { a: 1 } }, toolCall: { name: 'Bash', inputJSON: '{"a":1}' } }),
  ],
  [
    'Go and SDK call aliases',
    callOutput({
      Payload: { ToolCall: { name: 'Bash', input_json: { a: 1 } } },
      toolCall: { name: 'Bash', inputJSON: '{"a":1}' },
    }),
  ],
  ['payload aliases', callOutput({ Payload: { Text: 'kept' }, payload: { Text: 'kept' }, text: 'kept' })],
  [
    'result aliases',
    callOutput({
      tool_result: { tool_call_id: 'one', content_json: { a: 1 }, is_error: true },
      toolResult: { toolCallId: 'one', contentJSON: '{"a":1}', isError: true },
    }),
  ],
  [
    'result field aliases',
    callOutput({
      toolResult: {
        tool_call_id: 'one',
        toolCallId: 'one',
        content_json: { a: 1 },
        contentJson: '{"a":1}',
        contentJSON: Buffer.from('{"a":1}').toString('base64'),
        is_error: true,
        isError: true,
      },
    }),
  ],
  [
    'schema aliases',
    {
      tools: [
        {
          name: 'Bash',
          input_schema_json: '{"a":1,"b":2}',
          inputSchemaJson: '{ "b":2,"a":1 }',
          inputSchemaJSON: Buffer.from('{"a":1,"b":2}').toString('base64'),
        },
      ],
    },
  ],
  [
    'opaque arguments',
    callOutput({
      toolCall: {
        name: 'Bash',
        inputJSON: { unknown: { tool_call: 'argument data', systemPrompt: 'not a transform alias' } },
      },
    }),
  ],
  ['opaque result', callOutput({ toolResult: { contentJSON: { unknown: { a: 1 } } } })],
  [
    'malformed JSON call aliases',
    callOutput({
      tool_call: { name: 'Bash', input_json: malformedJSON, inputJson: malformedBase64 },
      toolCall: { name: 'Bash', inputJSON: malformedBase64 },
      Payload: { ToolCall: { name: 'Bash', input_json: malformedJSON } },
    }),
  ],
  [
    'malformed JSON result aliases',
    callOutput({
      tool_result: { content_json: malformedJSON, contentJson: malformedBase64 },
      toolResult: { contentJSON: malformedBase64 },
      payload: { ToolResult: { content_json: malformedJSON } },
    }),
  ],
  [
    'malformed JSON schema aliases',
    {
      tools: [
        {
          name: 'Bash',
          input_schema_json: malformedJSON,
          inputSchemaJson: malformedBase64,
          inputSchemaJSON: malformedJSON,
        },
      ],
    },
  ],
];

for (const [name, transformedInput] of equivalentTransforms) {
  for (const failOpen of [true, false]) {
    test(`strict decoding preserves unambiguous ${name}, failOpen=${failOpen}`, async () => {
      const payload = { action: 'allow', transformed_input: transformedInput };
      const normal = await runHook(preflightRequest(), payload, { failOpen });
      const strict = await runHook(preflightRequest(), payload, { strictTransforms: true, failOpen });
      assert.deepEqual(strict.response, normal.response);
      assert.ok(strict.response.transformedInput);
    });
  }
}

const unknownNestedTransforms = [
  ['message', { messages: [{ role: 'user', content: 'kept', unknown: 'rewrite' }] }],
  ['tool definition', { tools: [{ name: 'Bash', unknown: 'rewrite' }] }],
  ['part', callOutput({ text: 'kept', unknown: 'rewrite' })],
  ['Go payload', callOutput({ Payload: { Text: 'kept', Unknown: 'rewrite' } })],
  ['lowercase payload', callOutput({ payload: { Text: 'kept', Unknown: 'rewrite' } })],
  ['tool call', callOutput({ toolCall: { ...redactedCall, unknown: 'rewrite' } })],
  ['tool result', callOutput({ toolResult: { content: 'kept', unknown: 'rewrite' } })],
  ['nested call', callOutput({ Payload: { ToolCall: { ...originalCall, unknown: 'rewrite' } } })],
  ['nested result', callOutput({ Payload: { ToolResult: { content: 'kept', unknown: 'rewrite' } } })],
];

for (const [name, transformedInput] of [...conflictingTransforms, ...unknownNestedTransforms]) {
  for (const failOpen of [true, false]) {
    test(`strict decoding refuses lossy ${name}, failOpen=${failOpen}`, async () => {
      const payload = { action: 'allow', transformed_input: transformedInput };
      await assert.rejects(
        runHook(preflightRequest(), payload, { strictTransforms: true, failOpen }),
        HookTransformError,
      );
      const implicit = await runHook(preflightRequest(), payload, { failOpen });
      const explicit = await runHook(preflightRequest(), payload, { strictTransforms: false, failOpen });
      assert.deepEqual(implicit.response, explicit.response);
      assert.equal(implicit.response.action, 'allow');
    });
  }
}

const malformedTransforms = [
  null,
  false,
  'required rewrite',
  [],
  {},
  { unknown_field: 'required rewrite' },
  { system_prompt: '' },
  { systemPrompt: 1 },
  { conversation_preview: null },
  { conversationPreview: '' },
  { messages: {} },
  { messages: [] },
  { messages: [null] },
  { messages: [{ parts: [{ text: 'kept' }] }] },
  { messages: [{ role: 99, content: 'kept' }] },
  { messages: [{ role: '', content: 'kept' }] },
  { messages: [{ role: 'user' }] },
  { messages: [{ role: 'user', content: '' }] },
  { messages: [{ role: 'user', content: 4 }] },
  { messages: [{ role: 'user', parts: [] }] },
  { messages: [{ role: 'user', parts: 'required rewrite' }] },
  { messages: [{ role: 'user', parts: [null] }] },
  { messages: [{ role: 'user', parts: [{ text: 'kept' }, {}] }] },
  { messages: [{ role: 'user', parts: [{ kind: 4, text: 'kept' }] }] },
  { messages: [{ role: 'user', parts: [{ Payload: { Text: 4 } }] }] },
  { messages: [{ role: 'user', parts: [{ payload: [] }] }] },
  { messages: [{ role: 'assistant', parts: [{ toolCall: { name: 'Bash', inputJSON: '' } }] }] },
  { messages: [{ role: 'assistant', parts: [{ toolCall: { name: 'Bash', inputJSON: 4 } }] }] },
  callOutput({ toolCall: { name: 'Bash', input_json: false } }),
  callOutput({ toolCall: { name: 'Bash', inputJson: null } }),
  callOutput({ toolResult: { content_json: 4 } }),
  callOutput({ toolResult: { contentJson: '' } }),
  { messages: [{ role: 'tool', parts: [{ toolResult: {} }] }] },
  { messages: [{ role: 'tool', parts: [{ toolResult: { contentJSON: null } }] }] },
  { messages: [{ role: 'tool', parts: [{ toolResult: { content: 'ok', isError: 'true' } }] }] },
  { tools: [] },
  { tools: {} },
  { tools: [null] },
  { tools: [{}] },
  { tools: [{ name: '' }] },
  { tools: [{ name: 'Bash', description: 4 }] },
  { tools: [{ name: 'Bash', input_schema_json: {} }] },
  { tools: [{ name: 'Bash', inputSchemaJSON: '' }] },
  { tools: [{ name: 'Bash', input_schema_json: null }] },
  { tools: [{ name: 'Bash', inputSchemaJson: 4 }] },
  { tools: [{ name: 'Bash' }, false] },
  { output: [] },
  { output: [null] },
  { system_prompt: 'kept', messages: [] },
];

for (const [index, transformedInput] of malformedTransforms.entries()) {
  for (const failOpen of [true, false]) {
    test(`strict decoding refuses malformed transform ${index}, failOpen=${failOpen}`, async () => {
      const payload = { action: 'allow', transformed_input: transformedInput };
      await assert.rejects(
        runHook(preflightRequest(), payload, { strictTransforms: true, failOpen }),
        HookTransformError,
      );
      const { response } = await runHook(preflightRequest(), payload, { failOpen });
      assert.equal(response.action, 'allow');
    });
  }
}

for (const failOpen of [true, false]) {
  test(`evaluateHook preserves HookTransformError identity, failOpen=${failOpen}`, async () => {
    const error = new HookTransformError();
    await assert.rejects(
      evaluateHook({
        apiEndpoint: 'http://127.0.0.1:9',
        insecure: true,
        hooks: { ...hooksConfig, strictTransforms: true, failOpen },
        request: preflightRequest(),
        fetchImpl: async () => {
          throw error;
        },
      }),
      (caught) => caught === error,
    );
  });
}

test('the fixture comparison names divergent fields', () => {
  // Pins the comparator, not the serializer. The tests above are what check the
  // SDK's own output. Each case applies one divergence the server cannot read and
  // asserts the diff names the offending path. Without this test, a comparator
  // that accepted a renamed discriminator or a re-encoded payload would pass
  // every case above while checking nothing.
  const fixture = loadPreflightRequest();
  const cases = [
    {
      name: 'renamed discriminator',
      mutate: (body) => {
        const part = body.input.messages[0].parts[0];
        part.type = part.kind;
        delete part.kind;
      },
      wantPath: 'input.messages[0].parts[0].kind',
    },
    {
      name: 'camelCase tool call payload',
      mutate: (body) => {
        const part = body.input.messages[1].parts[1];
        part.toolCall = part.tool_call;
        delete part.tool_call;
      },
      wantPath: 'input.messages[1].parts[1].tool_call',
    },
    {
      name: 'base64 tool call input',
      mutate: (body) => {
        const call = body.input.messages[1].parts[1].tool_call;
        call.input_json = Buffer.from(JSON.stringify(call.input_json), 'utf8').toString('base64');
      },
      wantPath: 'input.messages[1].parts[1].tool_call.input_json',
    },
    {
      name: 'base64 tool result content',
      mutate: (body) => {
        const result = body.input.messages[2].parts[0].tool_result;
        result.content_json = Buffer.from(JSON.stringify(result.content_json), 'utf8').toString('base64');
      },
      wantPath: 'input.messages[2].parts[0].tool_result.content_json',
    },
    {
      name: 'raw JSON tool schema',
      mutate: (body) => {
        const tool = body.input.tools[0];
        tool.inputSchemaJSON = Buffer.from(tool.input_schema_json, 'base64').toString('utf8');
        delete tool.input_schema_json;
      },
      wantPath: 'input.tools[0].input_schema_json',
    },
  ];

  for (const testCase of cases) {
    const mutated = structuredClone(fixture);
    testCase.mutate(mutated);
    const diffs = diffJson(mutated, fixture);
    assert.ok(diffs.length > 0, `comparison accepted a divergent payload (${testCase.name})`);
    assert.ok(
      diffs.some((diff) => diff.startsWith(testCase.wantPath)),
      `diff did not name ${testCase.wantPath}: ${diffs.join(', ')}`,
    );
  }
});

test('the text shorthand reaches the server as a text part', async () => {
  const request = {
    phase: 'preflight',
    context: { model: { provider: 'openai', name: 'gpt-4o' } },
    input: { messages: [{ role: 'user', content: 'hello world' }] },
  };
  const { body } = await runHook(request, { action: 'allow', evaluations: [] });
  assert.deepEqual(body.input.messages, [{ role: 'user', parts: [{ kind: 'text', text: 'hello world' }] }]);
});

test('allow response parses into evaluations', async () => {
  const { response } = await runHook(preflightRequest(), loadResponses().allow);

  assert.equal(response.action, 'allow');
  assert.equal(response.transformedInput, undefined);
  assert.equal(response.evaluations.length, 1);
  assert.deepEqual(response.evaluations[0], {
    ruleId: 'pii-detect',
    evaluatorId: 'evaluator-pii',
    evaluatorKind: 'regex',
    passed: true,
    latencyMs: 12,
    explanation: 'no PII matches',
    reason: undefined,
  });
});

test('deny response preserves rule id and reason', async () => {
  const { response } = await runHook(preflightRequest(), loadResponses().deny);

  assert.equal(response.action, 'deny');
  assert.equal(response.ruleId, 'block-destructive-bash');
  assert.equal(response.reason, 'Bash(*rm*) is not allowed in this environment');
  assert.equal(response.evaluations.length, 1);
  assert.equal(response.evaluations[0].passed, false);
  assert.equal(response.evaluations[0].reason, 'blocked tool Bash');
});

test('transformed input keeps every part kind and decodes tool payloads', async () => {
  const { response } = await runHook(preflightRequest(), loadResponses().allow_with_transformed_input);

  assert.equal(response.action, 'allow');
  const input = response.transformedInput;
  assert.ok(input, 'transformed input was dropped');
  assert.equal(input.systemPrompt, 'You are a careful assistant.');
  assert.equal(input.conversationPreview, 'user: Delete the cache directory under [REDACTED].');
  assert.equal(input.messages.length, 3);

  const [user, assistant, tool] = input.messages;
  assert.equal(user.role, 'user');
  assert.deepEqual(user.parts, [{ type: 'text', text: 'Delete the cache directory under [REDACTED].' }]);

  assert.equal(assistant.role, 'assistant');
  assert.deepEqual(
    assistant.parts.map((part) => part.type),
    ['thinking', 'tool_call'],
  );
  assert.equal(assistant.parts[0].thinking, 'The request is destructive, so inspect the directory first.');
  assert.deepEqual(assistant.parts[1].toolCall, {
    id: 'call-bash',
    name: 'Bash',
    inputJSON: '{"command":"rm -rf /tmp/cache"}',
  });

  assert.equal(tool.role, 'tool');
  assert.equal(tool.name, 'Bash');
  assert.deepEqual(
    tool.parts.map((part) => part.type),
    ['tool_result'],
  );
  assert.deepEqual(tool.parts[0].toolResult, {
    toolCallId: 'call-bash',
    name: 'Bash',
    content: "rm: cannot remove '/tmp/cache': Permission denied",
    contentJSON: '{"exit_code":1}',
    isError: true,
  });

  assert.deepEqual(input.tools, [
    {
      name: 'Bash',
      description: 'Run a shell command.',
      type: 'function',
      inputSchemaJSON: bashToolSchema,
    },
  ]);
});

test('transformed input accepts proto-JSON integer roles', async () => {
  const { response } = await runHook(preflightRequest(), {
    action: 'allow',
    transformed_input: {
      messages: [
        { role: 2, parts: [{ text: 'hello' }] },
        { role: 3, parts: [{ kind: 'tool_result', tool_result: { tool_call_id: 'call-1', content: 'ok' } }] },
      ],
    },
    evaluations: [],
  });

  const input = response.transformedInput;
  assert.ok(input);
  assert.equal(input.messages[0].role, 'assistant');
  assert.deepEqual(input.messages[0].parts, [{ type: 'text', text: 'hello' }]);
  assert.equal(input.messages[1].role, 'tool');
  assert.deepEqual(input.messages[1].parts[0].toolResult, { toolCallId: 'call-1', content: 'ok' });
});

// Response payloads are base64 of whatever bytes the proto field held, and
// nothing guarantees those bytes are JSON. Whatever comes back has to stay a JSON
// document so a transform can be re-exported or re-sent. Go and Python resolve
// these four cases the same way; see `conformance/hooks/README.md`.
const payloadDecodingCases = [
  {
    name: 'base64 of JSON',
    value: Buffer.from('{"command":"ls /tmp"}', 'utf8').toString('base64'),
    want: '{"command":"ls /tmp"}',
  },
  {
    name: 'base64 of plain text',
    value: Buffer.from('plain text tool output', 'utf8').toString('base64'),
    want: '"plain text tool output"',
  },
  { name: 'embedded JSON text', value: '{"command":"ls /tmp"}', want: '{"command":"ls /tmp"}' },
  { name: 'neither base64 nor JSON', value: 'not base64 either', want: '"not base64 either"' },
  { name: 'malformed JSON text', value: malformedJSON, want: JSON.stringify(malformedJSON) },
  { name: 'base64 of malformed JSON', value: malformedBase64, want: JSON.stringify(malformedJSON) },
];

for (const testCase of payloadDecodingCases) {
  for (const strictTransforms of [false, true]) {
    for (const failOpen of [true, false]) {
      test(`payloads stay JSON: ${testCase.name}, strict=${strictTransforms}, failOpen=${failOpen}`, async () => {
        const { response } = await runHook(
          preflightRequest(),
          {
            action: 'allow',
            transformed_input: {
              messages: [
                {
                  role: 'assistant',
                  parts: [{ kind: 'tool_call', tool_call: { id: 'call-1', name: 'Bash', input_json: testCase.value } }],
                },
                { role: 'tool', parts: [{ tool_result: { content_json: testCase.value } }] },
              ],
              tools: [{ name: 'Bash', input_schema_json: testCase.value }],
            },
            evaluations: [],
          },
          { strictTransforms, failOpen },
        );

        const input = response.transformedInput;
        assert.ok(input, 'transformed input was dropped');
        for (const value of [
          input.messages[0].parts[0].toolCall.inputJSON,
          input.messages[1].parts[0].toolResult.contentJSON,
          input.tools[0].inputSchemaJSON,
        ]) {
          assert.equal(value, testCase.want);
          JSON.parse(value);
        }
      });
    }
  }
}

for (const schema of [{ type: 'object' }, 'eyJhIjoxfQ', 'not base64 either']) {
  test(`a malformed tool schema keeps the verdict: ${JSON.stringify(schema)}`, async () => {
    const { response } = await runHook(preflightRequest(), {
      action: 'deny',
      rule_id: 'block-destructive-bash',
      reason: 'denied',
      transformed_input: { tools: [{ name: 'Bash', input_schema_json: schema }] },
      evaluations: [],
    });

    assert.equal(response.action, 'deny');
    assert.equal(response.ruleId, 'block-destructive-bash');
  });
}

test('an unparsable tool payload reaches the server as text', async () => {
  // Streaming providers accumulate tool arguments without validating them, so a
  // truncated payload has to travel as text rather than break the request. Go and
  // Python send the same text.
  const request = {
    phase: 'preflight',
    context: { model: { provider: 'anthropic', name: 'claude-sonnet-4' } },
    input: {
      messages: [
        {
          role: 'assistant',
          parts: [
            { type: 'tool_call', toolCall: { id: 'call-1', name: 'Bash', inputJSON: '{"command":"truncat' } },
            { type: 'tool_result', toolResult: { toolCallId: 'call-1', contentJSON: '{"exit_code' } },
          ],
        },
      ],
    },
  };
  const { body } = await runHook(request, { action: 'allow', evaluations: [] });

  const [call, result] = body.input.messages[0].parts;
  assert.equal(call.tool_call.input_json, '{"command":"truncat');
  assert.equal(result.tool_result.content_json, '{"exit_code');
});

test('a part-less message serializes parts as an empty array', async () => {
  // All three SDKs send `"parts": []` rather than null.
  const request = {
    phase: 'preflight',
    context: { model: { provider: 'anthropic', name: 'claude-sonnet-4' } },
    input: { messages: [{ role: 'user', parts: [] }] },
  };
  const { body } = await runHook(request, { action: 'allow', evaluations: [] });

  assert.deepEqual(body.input.messages, [{ role: 'user', parts: [] }]);
});

test('an empty transformed_input is no transform', async () => {
  // The server emits `transformed_input:{}` for a rule that returns an empty input.
  // Reporting a transform for that body would let a caller replace the prompt
  // with nothing. Go and Python also report no transform.
  const { response } = await runHook(preflightRequest(), {
    action: 'allow',
    transformed_input: {},
    evaluations: [],
  });

  assert.equal(response.transformedInput, undefined);
});

// One response part, and the part the SDK must report for it.
//
// `kind` names the field the parser reads, and the parser commits to it: a part
// that lost that field is dropped rather than rebuilt from a leftover field, which
// would report a part the rule never wrote. An empty payload carries no content
// either, and keeping it would append an empty part that the request serializer
// drops on the way back out. A tool call without a name goes too, because the
// caller can neither route nor re-send it. A part with no `kind` at all is still
// read from whichever payload field is set, because the server always sets `kind`,
// so that shape only reaches the SDK from a hand-written or proto-JSON body.
//
// Go and Python resolve every case here the same way.
const responsePartCases = [
  ['text', { kind: 'text', text: 'kept' }, { type: 'text', text: 'kept' }],
  [
    'kind precedes type',
    { kind: 'text', type: 'thinking', text: 'kept', thinking: 'ignored' },
    { type: 'text', text: 'kept' },
  ],
  [
    'text precedes leftover thinking',
    { kind: 'text', text: 'kept', thinking: 'ignored' },
    { type: 'text', text: 'kept' },
  ],
  ['inferred thinking precedes text', { text: 'ignored', thinking: 'kept' }, { type: 'thinking', thinking: 'kept' }],
  ['text without text', { kind: 'text', text: '' }, undefined],
  // The shape the server emits for an empty text part: its encoder omits the field
  // rather than sending ''.
  ['text without a text field', { kind: 'text' }, undefined],
  ['thinking', { kind: 'thinking', thinking: 'planning' }, { type: 'thinking', thinking: 'planning' }],
  ['thinking without thinking', { kind: 'thinking', thinking: '' }, undefined],
  ['thinking without a thinking field', { kind: 'thinking' }, undefined],
  [
    'tool call',
    { kind: 'tool_call', tool_call: { id: 'call-1', name: 'Bash' } },
    { type: 'tool_call', toolCall: { name: 'Bash', id: 'call-1' } },
  ],
  ['tool call without payload', { kind: 'tool_call' }, undefined],
  ['tool call without name', { kind: 'tool_call', tool_call: { id: 'call-1' } }, undefined],
  [
    'tool result',
    { kind: 'tool_result', tool_result: { tool_call_id: 'call-1', content: 'ok' } },
    { type: 'tool_result', toolResult: { toolCallId: 'call-1', content: 'ok' } },
  ],
  ['tool result without payload', { kind: 'tool_result' }, undefined],
  ['unknown kind', { kind: 'image' }, undefined],
  [
    'unknown kind with text',
    { kind: 'image', text: 'described by the server as text' },
    { type: 'text', text: 'described by the server as text' },
  ],
  ['unknown kind with a tool call', { kind: 'image', tool_call: { name: 'Bash' } }, undefined],
  ['tool call kind with leftover text', { kind: 'tool_call', text: 'not a tool call' }, undefined],
  ['tool result kind with leftover text', { kind: 'tool_result', text: 'not a tool result' }, undefined],
  ['thinking kind with leftover text', { kind: 'thinking', thinking: '', text: 'not thinking' }, undefined],
  ['text kind with leftover thinking', { kind: 'text', text: '', thinking: 'not text' }, undefined],
  ['text kind with a leftover tool call', { kind: 'text', text: '', tool_call: { name: 'Bash' } }, undefined],
  ['no kind with text', { text: 'recovered text' }, { type: 'text', text: 'recovered text' }],
  ['no kind with thinking', { thinking: 'recovered thinking' }, { type: 'thinking', thinking: 'recovered thinking' }],
  [
    'no kind with a tool call',
    { tool_call: { id: 'call-1', name: 'Bash' } },
    { type: 'tool_call', toolCall: { name: 'Bash', id: 'call-1' } },
  ],
  [
    'no kind with a tool result',
    { tool_result: { tool_call_id: 'call-1', content: 'ok' } },
    { type: 'tool_result', toolResult: { toolCallId: 'call-1', content: 'ok' } },
  ],
  ['empty part', {}, undefined],
];

for (const [name, part, want] of responsePartCases) {
  for (const strictTransforms of [false, true]) {
    test(`a response part is parsed by its kind: ${name}, strict=${strictTransforms}`, async () => {
      const pending = runHook(
        preflightRequest(),
        {
          action: 'allow',
          transformed_input: { messages: [{ role: 'assistant', parts: [part] }] },
          evaluations: [],
        },
        { strictTransforms },
      );
      if (strictTransforms && want === undefined) {
        await assert.rejects(pending, HookTransformError);
        return;
      }
      const { response } = await pending;
      const parts = response.transformedInput?.messages?.[0]?.parts ?? [];
      assert.deepEqual(parts, want === undefined ? [] : [want]);
    });
  }
}

// The three shapes only the JS parser has to tolerate. The server sends a
// snake_case `kind`, but this SDK also parses an echo of its own camelCase part
// and a protobuf `Part` marshalled with Go's `encoding/json`, which nests the
// oneof under a capitalized `Payload`.
const responsePartEncodingCases = [
  ['sdk text', { type: 'text', text: 'echoed' }, { type: 'text', text: 'echoed' }],
  ['sdk thinking', { type: 'thinking', thinking: 'echoed' }, { type: 'thinking', thinking: 'echoed' }],
  [
    'sdk tool call',
    { type: 'tool_call', toolCall: { id: 'call-1', name: 'Bash', inputJSON: '{"command":"ls"}' } },
    { type: 'tool_call', toolCall: { name: 'Bash', id: 'call-1', inputJSON: '{"command":"ls"}' } },
  ],
  [
    'sdk tool result',
    { type: 'tool_result', toolResult: { toolCallId: 'call-1', content: 'ok' } },
    { type: 'tool_result', toolResult: { toolCallId: 'call-1', content: 'ok' } },
  ],
  ['sdk tool call without payload', { type: 'tool_call' }, undefined],
  ['go text', { Payload: { Text: 'marshalled' } }, { type: 'text', text: 'marshalled' }],
  ['go thinking', { Payload: { Thinking: 'marshalled' } }, { type: 'thinking', thinking: 'marshalled' }],
  ['go tool call', { Payload: { ToolCall: { name: 'Bash' } } }, { type: 'tool_call', toolCall: { name: 'Bash' } }],
  [
    'go tool result',
    { Payload: { ToolResult: { tool_call_id: 'call-1' } } },
    { type: 'tool_result', toolResult: { toolCallId: 'call-1' } },
  ],
  ['go nil oneof', { Payload: null, kind: 'text', text: 'kept' }, { type: 'text', text: 'kept' }],
];

for (const [name, part, want] of responsePartEncodingCases) {
  for (const strictTransforms of [false, true]) {
    test(`a response part in an alternate encoding is parsed: ${name}, strict=${strictTransforms}`, async () => {
      const pending = runHook(
        preflightRequest(),
        {
          action: 'allow',
          transformed_input: { messages: [{ role: 'assistant', parts: [part] }] },
          evaluations: [],
        },
        { strictTransforms },
      );
      if (strictTransforms && want === undefined) {
        await assert.rejects(pending, HookTransformError);
        return;
      }
      const { response } = await pending;
      const parts = response.transformedInput?.messages?.[0]?.parts ?? [];
      assert.deepEqual(parts, want === undefined ? [] : [want]);
    });
  }
}
