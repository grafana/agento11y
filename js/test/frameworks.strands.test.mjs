import assert from 'node:assert/strict';
import test from 'node:test';
import {
  Agento11yStrandsHookProvider,
  createAgento11yStrandsPlugin,
  withAgento11yStrandsHooks,
} from '../.test-dist/frameworks/strands/index.js';
import { Agento11yClient } from '../.test-dist/index.js';

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

test('strands hook provider records model, tool, and conversation metadata', async () => {
  const exporter = new CapturingExporter();
  const client = new Agento11yClient({
    generationExporter: exporter,
    generationExport: { protocol: 'none' },
  });
  const provider = new Agento11yStrandsHookProvider(client, {
    conversationId: 'local-agento11y-strands-demo',
    agentName: 'local-strands-demo',
    providerResolver: 'auto',
  });
  const agent = fakeAgent();

  provider.beforeInvocation({ agent });
  provider.beforeModelCall({ agent, model: agent.model });
  provider.modelStreamUpdate({
    agent,
    event: {
      type: 'modelContentBlockDeltaEvent',
      delta: { type: 'textDelta', text: 'The sum ' },
    },
  });
  provider.beforeToolCall({
    agent,
    toolUse: { name: 'add_numbers', toolUseId: 'tool-1', input: { left: 19, right: 23 } },
    tool: agent.tools[0],
  });
  provider.afterToolCall({
    agent,
    toolUse: { name: 'add_numbers', toolUseId: 'tool-1', input: { left: 19, right: 23 } },
    tool: agent.tools[0],
    result: {
      toolUseId: 'tool-1',
      status: 'success',
      content: [{ text: '42' }],
    },
  });
  provider.afterModelCall({
    agent,
    model: agent.model,
    stopData: {
      stopReason: 'end_turn',
      message: {
        role: 'assistant',
        content: [{ text: 'The sum of 19 and 23 is 42.' }],
        metadata: {
          usage: {
            inputTokens: 98,
            outputTokens: 120,
            totalTokens: 218,
          },
        },
      },
    },
  });
  provider.afterInvocation({ agent });

  await client.shutdown();
  const snapshot = client.debugSnapshot();
  assert.equal(snapshot.generations.length, 1);
  assert.equal(snapshot.toolExecutions.length, 1);

  const generation = snapshot.generations[0];
  assert.equal(generation.conversationId, 'local-agento11y-strands-demo');
  assert.equal(generation.agentName, 'local-strands-demo');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.model.name, 'gpt-4o-mini');
  assert.equal(generation.systemPrompt, 'You are concise and show the final answer.');
  assert.equal(generation.tags['agento11y.framework.name'], 'strands');
  assert.equal(generation.tags['agento11y.framework.source'], 'hooks');
  assert.equal(generation.tags['agento11y.framework.language'], 'typescript');
  assert.equal(generation.metadata['agento11y.framework.run_type'], 'chat');
  assert.equal(generation.input[0].parts[0].text, 'Use the add_numbers tool to add 19 and 23.');
  assert.equal(generation.output[0].parts[0].text, 'The sum of 19 and 23 is 42.');
  assert.equal(generation.usage.inputTokens, 98);
  assert.equal(generation.usage.outputTokens, 120);
  assert.equal(generation.tools[0].name, 'add_numbers');

  const toolExecution = snapshot.toolExecutions[0];
  assert.equal(toolExecution.toolName, 'add_numbers');
  assert.equal(toolExecution.toolCallId, 'tool-1');
  assert.deepEqual(toolExecution.arguments, { left: 19, right: 23 });
  assert.deepEqual(toolExecution.result.content, [{ text: '42' }]);
});

test('withAgento11yStrandsHooks appends a plugin for configs and registers existing agents once', () => {
  const client = new Agento11yClient({ generationExport: { protocol: 'none' } });
  const config = withAgento11yStrandsHooks({ name: 'agent' }, client, { conversationId: 'conversation-1' });
  assert.equal(config.plugins.length, 1);
  const configAgain = withAgento11yStrandsHooks(config, client, { conversationId: 'conversation-1' });
  assert.equal(configAgain.plugins.length, 1);

  const agent = {
    addHookCalls: [],
    addHook(eventType, callback) {
      this.addHookCalls.push({ eventType, callback });
      return () => {};
    },
  };
  withAgento11yStrandsHooks(agent, client, { conversationId: 'conversation-1' });
  withAgento11yStrandsHooks(agent, client, { conversationId: 'conversation-1' });
  assert.equal(agent.addHookCalls.length, 7);
});

test('createAgento11yStrandsPlugin exposes a stable Strands plugin', () => {
  const client = new Agento11yClient({ generationExport: { protocol: 'none' } });
  const plugin = createAgento11yStrandsPlugin(client);
  assert.equal(plugin.name, 'agento11y-strands-plugin');
});

function fakeAgent() {
  return {
    name: 'strands-demo',
    id: 'strands-demo',
    messages: [
      {
        role: 'user',
        content: [{ text: 'Use the add_numbers tool to add 19 and 23.' }],
      },
    ],
    systemPrompt: 'You are concise and show the final answer.',
    model: {
      getConfig() {
        return {
          modelId: 'gpt-4o-mini',
          temperature: 0.2,
        };
      },
    },
    tools: [
      {
        name: 'add_numbers',
        description: 'Add two integers.',
        toolSpec: {
          name: 'add_numbers',
          description: 'Add two integers.',
          inputSchema: {
            type: 'object',
            properties: {
              left: { type: 'integer' },
              right: { type: 'integer' },
            },
            required: ['left', 'right'],
          },
        },
      },
    ],
  };
}
