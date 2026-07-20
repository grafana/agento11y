import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildTagsAndMetadata,
  chooseMode,
  createSourceState,
  emitFrameworks,
  resolveThread,
  sourceTagFor,
} from '../scripts/devex-emitter.mjs';

test('devex emitter tags include required provider and language fields', () => {
  const payload = buildTagsAndMetadata('openai', 'SYNC', 2, 1);
  assert.equal(payload.tags['agento11y.devex.language'], 'javascript');
  assert.equal(payload.tags['agento11y.devex.provider'], 'openai');
  assert.equal(payload.tags['agento11y.devex.source'], 'provider_wrapper');
  assert.equal(payload.tags['agento11y.devex.mode'], 'SYNC');
  assert.equal(payload.metadata.turn_index, 2);
  assert.equal(payload.metadata.conversation_slot, 1);
  assert.equal(typeof payload.metadata.agent_persona, 'string');
  assert.equal(payload.metadata.emitter, 'sdk-traffic');
  assert.equal(typeof payload.metadata.provider_shape, 'string');
});

test('custom provider source is marked as core_custom', () => {
  assert.equal(sourceTagFor('mistral'), 'core_custom');
  assert.equal(sourceTagFor('gemini'), 'provider_wrapper');
});

test('mode chooser returns both SYNC and STREAM based on threshold', () => {
  assert.equal(chooseMode(0, 30), 'STREAM');
  assert.equal(chooseMode(29, 30), 'STREAM');
  assert.equal(chooseMode(30, 30), 'SYNC');
  assert.equal(chooseMode(99, 30), 'SYNC');
});

test('thread rotation resets turn and assigns a new conversation id', async () => {
  const state = createSourceState(1);

  let thread = resolveThread(state, 3, 'openai', 0);
  assert.equal(thread.turn, 0);
  const firstConversationId = thread.conversationId;
  assert.ok(firstConversationId.length > 0);

  thread.turn = 3;
  await new Promise((resolve) => setTimeout(resolve, 2));
  thread = resolveThread(state, 3, 'openai', 0);
  assert.equal(thread.turn, 0);
  assert.notEqual(thread.conversationId, firstConversationId);
});

test('framework emit path invokes all framework handlers for provider sources', async () => {
  const calls = [];
  class FakeHandler {
    async handleChatModelStart(_serialized, _messages, runID) {
      calls.push(`start:${runID}`);
    }
    async handleLLMEnd(_output, runID) {
      calls.push(`end:${runID}`);
    }
  }

  await emitFrameworks(
    {
      langchain: { Agento11yLangChainHandler: FakeHandler },
      langgraph: { Agento11yLangGraphHandler: FakeHandler },
      openaiAgents: { Agento11yOpenAIAgentsHandler: FakeHandler },
      llamaindex: { Agento11yLlamaIndexHandler: FakeHandler },
      googleAdk: { Agento11yGoogleAdkHandler: FakeHandler },
    },
    {},
    'openai',
    'SYNC',
    {
      conversationId: 'conv-framework',
      turn: 3,
      agentName: 'agent',
      agentVersion: 'v1',
      tags: { 'agento11y.devex.provider': 'openai' },
      metadata: { provider_shape: 'framework' },
    },
  );

  assert.equal(calls.length, 10);
});

test('framework emit path skips non-provider custom source', async () => {
  let called = false;
  class FakeHandler {
    constructor(_client, _options) {
      called = true;
    }
  }

  await emitFrameworks(
    {
      langchain: { Agento11yLangChainHandler: FakeHandler },
      langgraph: { Agento11yLangGraphHandler: FakeHandler },
      openaiAgents: { Agento11yOpenAIAgentsHandler: FakeHandler },
      llamaindex: { Agento11yLlamaIndexHandler: FakeHandler },
      googleAdk: { Agento11yGoogleAdkHandler: FakeHandler },
    },
    {},
    'mistral',
    'SYNC',
    {
      conversationId: 'conv-framework',
      turn: 3,
      agentName: 'agent',
      agentVersion: 'v1',
      tags: { 'agento11y.devex.provider': 'mistral' },
      metadata: { provider_shape: 'framework' },
    },
  );

  assert.equal(called, false);
});
