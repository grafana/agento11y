import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { createSecretRedactionSanitizer, redactSecretText, redactSecretTextLightweight } from '../.test-dist/index.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const fixturesDir = join(__dirname, '../../redaction/fixtures/');

/** Loads a shared fixture file. Every redaction engine reads the same path. */
export function loadFixture(name) {
  return JSON.parse(readFileSync(join(fixturesDir, name), 'utf8'));
}

const stringCases = loadFixture('strings.json').cases;

test('conformance: redaction strings', async (t) => {
  assert.ok(stringCases.length > 0, 'strings.json has no cases');

  for (const testCase of stringCases) {
    await t.test(testCase.id, () => {
      const options = { redactEmailAddresses: testCase.emails };
      let actual;
      if (testCase.mode === 'full') {
        actual = redactSecretText(testCase.input, options);
      } else if (testCase.mode === 'light') {
        actual = redactSecretTextLightweight(testCase.input, options);
      } else {
        throw new Error(`unknown mode ${testCase.mode}`);
      }
      assert.equal(actual, testCase.expected);

      // Redacting a JSON payload must not corrupt its structure. Tool call
      // arguments reach the sanitizer as raw JSON bytes, and a truncated
      // payload is unreadable downstream.
      if (parsesAsJson(testCase.input)) {
        assert.ok(parsesAsJson(actual), `redacting ${testCase.id} produced invalid JSON: ${actual}`);
      }
    });
  }
});

function parsesAsJson(value) {
  if (!value.startsWith('{') && !value.startsWith('[')) return false;
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}

const generationFixtures = loadFixture('generations.json');
const probe = generationFixtures.probe;

const assistantParts = (value, callId) => [
  { type: 'text', text: value },
  { type: 'thinking', thinking: value },
  { type: 'tool_call', toolCall: { id: callId, name: 'bash', inputJSON: value } },
];

const toolParts = (value, callId) => [
  { type: 'text', text: value },
  { type: 'tool_result', toolResult: { toolCallId: callId, name: 'bash', content: value, contentJSON: value } },
];

/** Fills every slot in the matrix with the same probe. */
function buildProbeGeneration(value) {
  return {
    id: 'gen-conformance',
    mode: 'SYNC',
    operationName: 'generateText',
    model: { provider: 'openai', name: 'gpt-5' },
    startedAt: new Date('2026-01-01T00:00:00Z'),
    completedAt: new Date('2026-01-01T00:00:01Z'),
    systemPrompt: value,
    conversationTitle: value,
    callError: value,
    input: [
      { role: 'user', parts: [{ type: 'text', text: value }] },
      { role: 'assistant', parts: assistantParts(value, 'call-1') },
      { role: 'tool', parts: toolParts(value, 'call-1') },
    ],
    output: [
      { role: 'assistant', parts: assistantParts(value, 'call-2') },
      { role: 'tool', parts: toolParts(value, 'call-2') },
    ],
  };
}

function slotValues(generation) {
  return {
    systemPrompt: generation.systemPrompt,
    conversationTitle: generation.conversationTitle,
    callError: generation.callError,
    'input.user.text': generation.input[0].parts[0].text,
    'input.assistant.text': generation.input[1].parts[0].text,
    'input.assistant.thinking': generation.input[1].parts[1].thinking,
    'input.assistant.toolCallInputJson': generation.input[1].parts[2].toolCall.inputJSON,
    'input.tool.text': generation.input[2].parts[0].text,
    'input.tool.toolResultContent': generation.input[2].parts[1].toolResult.content,
    'input.tool.toolResultContentJson': generation.input[2].parts[1].toolResult.contentJSON,
    'output.assistant.text': generation.output[0].parts[0].text,
    'output.assistant.thinking': generation.output[0].parts[1].thinking,
    'output.assistant.toolCallInputJson': generation.output[0].parts[2].toolCall.inputJSON,
    'output.tool.text': generation.output[1].parts[0].text,
    'output.tool.toolResultContent': generation.output[1].parts[1].toolResult.content,
    'output.tool.toolResultContentJson': generation.output[1].parts[1].toolResult.contentJSON,
  };
}

test('conformance: redaction generation slots', async (t) => {
  for (const testCase of generationFixtures.cases) {
    await t.test(testCase.id, () => {
      const sanitize = createSecretRedactionSanitizer(
        {
          redactInputMessages: testCase.redactInputMessages,
          redactEmailAddresses: testCase.redactEmailAddresses,
        },
        {},
      );
      const actual = slotValues(sanitize(buildProbeGeneration(probe.input)));

      const expected = {};
      for (const [slot, mode] of Object.entries(testCase.slots)) {
        expected[slot] = probe[mode];
      }

      assert.deepEqual(
        Object.keys(actual).sort(),
        Object.keys(expected).sort(),
        'JS harness slots and fixture slots disagree',
      );
      assert.deepEqual(actual, expected);
    });
  }
});
