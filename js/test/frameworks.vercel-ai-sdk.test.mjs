import assert from 'node:assert/strict';
import test from 'node:test';
import { createSigilVercelAiSdk } from '../.test-dist/frameworks/vercel-ai-sdk/index.js';
import { defaultConfig, SigilClient } from '../.test-dist/index.js';

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

test('vercel ai sdk generateText hooks record single-step success', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client, {
      agentName: 'vercel-agent',
      agentVersion: '1.0.0',
    });

    const hooks = sigil.generateTextHooks({ conversationId: 'conv-1' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'world',
      reasoningText: 'reasoning detail',
      finishReason: 'stop',
      usage: {
        inputTokens: 10,
        outputTokens: 5,
        totalTokens: 15,
        inputTokenDetails: {
          cacheReadTokens: 2,
          cacheWriteTokens: 1,
        },
        outputTokenDetails: {
          reasoningTokens: 3,
        },
      },
      response: {
        id: 'resp-1',
        modelId: 'gpt-5',
      },
    });
  });

  assert.equal(generations.length, 1);
  const generation = generations[0];
  assert.equal(generation.operationName, 'generateText');
  assert.equal(generation.mode, 'SYNC');
  assert.equal(generation.conversationId, 'conv-1');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.model.name, 'gpt-5');
  assert.equal(generation.input[0].content, 'hello');
  assert.equal(generation.output[0].content, 'world');
  assert.equal(generation.usage.inputTokens, 10);
  assert.equal(generation.usage.outputTokens, 5);
  assert.equal(generation.usage.totalTokens, 15);
  assert.equal(generation.usage.cacheReadInputTokens, 2);
  assert.equal(generation.usage.cacheWriteInputTokens, 1);
  assert.equal(generation.usage.reasoningTokens, 3);
  assert.equal(generation.tags['sigil.framework.name'], 'vercel-ai-sdk');
  assert.equal(generation.metadata['sigil.framework.step_type'], 'initial');
  assert.equal(generation.metadata['sigil.framework.reasoning_text'], 'reasoning detail');
});

test('vercel ai sdk prepareStep records input messages for ai sdk v6', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-v6-prepare-step' });

    const prepareResult = hooks.prepareStep?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello from v6' }],
      steps: [],
      experimental_context: undefined,
    });
    assert.equal(prepareResult, undefined);

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'hello back',
      finishReason: 'stop',
      response: { id: 'resp-v6-prepare-step', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].conversationId, 'conv-v6-prepare-step');
  assert.equal(generations[0].input[0].content, 'hello from v6');
  assert.equal(generations[0].output[0].content, 'hello back');
});

test('vercel ai sdk streamText prepareStep records input messages for ai sdk v6', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-v6-stream-prepare-step' });

    const prepareResult = hooks.prepareStep?.({
      stepNumber: 0,
      model: { provider: 'anthropic', modelId: 'claude-sonnet-4-5' },
      messages: [{ role: 'user', content: 'stream from v6' }],
      steps: [],
      experimental_context: undefined,
    });
    assert.equal(prepareResult, undefined);

    hooks.onChunk?.({
      stepNumber: 0,
      chunk: {
        type: 'text-delta',
        text: 'hel',
      },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'hello stream',
      finishReason: 'stop',
      response: { id: 'resp-v6-stream-prepare-step', modelId: 'claude-sonnet-4-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].mode, 'STREAM');
  assert.equal(generations[0].input[0].content, 'stream from v6');
  assert.equal(generations[0].output[0].content, 'hello stream');
});

test('vercel ai sdk generateText hooks record single-step error', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-error' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'error',
      error: new Error('step failed'),
      response: { id: 'resp-error', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  const generation = generations[0];
  assert.match(generation.callError ?? '', /step failed/);
  assert.equal(generation.stopReason, 'error');
});

test('vercel ai sdk generateText hooks record step when step start callback is absent', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'fallback path',
      finishReason: 'stop',
      response: { id: 'resp-no-step-start', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  const generation = generations[0];
  assert.equal(generation.conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
  assert.equal(generation.model.provider, 'openai');
  assert.equal(generation.model.name, 'gpt-5');
  assert.equal(generation.output[0].content, 'fallback path');
});

test('vercel ai sdk generateText defers recorder creation until step finish', async () => {
  const { generations } = await captureSession(async (client) => {
    const originalStartGeneration = client.startGeneration.bind(client);
    let startGenerationCalls = 0;
    client.startGeneration = (...args) => {
      startGenerationCalls += 1;
      return originalStartGeneration(...args);
    };

    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-no-step-finish' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });

    assert.equal(startGenerationCalls, 0);
  });

  assert.equal(generations.length, 0);
});

test('vercel ai sdk generateText records tool execution from onStepFinish without experimental tool callbacks', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-v5-step-finish-tools' });

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'tool-result',
      text: 'it is 18c',
      finishReason: 'stop',
      toolCalls: [
        {
          toolCallId: 'tool-v5',
          toolName: 'weather',
          input: { city: 'Paris' },
        },
      ],
      toolResults: [
        {
          toolCallId: 'tool-v5',
          toolName: 'weather',
          output: { temp_c: 18 },
        },
      ],
      response: { id: 'resp-v5-step-finish-tools', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.equal(snapshot.toolExecutions[0].toolCallId, 'tool-v5');
  assert.deepEqual(snapshot.toolExecutions[0].arguments, { city: 'Paris' });
  assert.deepEqual(snapshot.toolExecutions[0].result, { temp_c: 18 });
});

test('vercel ai sdk fallback tool spans use deterministic seed-based conversation id', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'tool-result',
      text: 'it is 18c',
      finishReason: 'stop',
      toolCalls: [
        {
          toolCallId: 'tool-v5-fallback-conv',
          toolName: 'weather',
          input: { city: 'Paris' },
        },
      ],
      toolResults: [
        {
          toolCallId: 'tool-v5-fallback-conv',
          toolName: 'weather',
          output: { temp_c: 18 },
        },
      ],
      response: { id: 'resp-v5-fallback-conv', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.equal(generations[0].conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
  assert.equal(snapshot.toolExecutions[0].conversationId, generations[0].conversationId);
});

test('vercel ai sdk fallback conversation id is consistent across tool callback availability', async () => {
  const stepFinishOnly = await captureSingleGeneration(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'tool-result',
      text: 'it is 18c',
      finishReason: 'stop',
      toolCalls: [
        {
          toolCallId: 'tool-fallback-consistency-a',
          toolName: 'weather',
          input: { city: 'Paris' },
        },
      ],
      toolResults: [
        {
          toolCallId: 'tool-fallback-consistency-a',
          toolName: 'weather',
          output: { temp_c: 18 },
        },
      ],
      response: { id: 'resp-fallback-consistency-a', modelId: 'gpt-5' },
    });
  });

  const withToolCallbacks = await captureSingleGeneration(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'use a tool' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-fallback-consistency-b',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: { toolCallId: 'tool-fallback-consistency-b' },
      success: true,
      output: { temp_c: 18 },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'tool-result',
      text: 'it is 18c',
      finishReason: 'stop',
      response: { id: 'resp-fallback-consistency-b', modelId: 'gpt-5' },
    });
  });

  assert.equal(stepFinishOnly.conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
  assert.equal(withToolCallbacks.conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
  assert.equal(stepFinishOnly.conversationId, withToolCallbacks.conversationId);
});

test('vercel ai sdk onStepFinish fallback tool failures do not throw', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-v5-tool-failure' });

    assert.doesNotThrow(() => {
      hooks.onStepFinish?.({
        stepNumber: 0,
        stepType: 'tool-result',
        text: 'weather tool failed',
        finishReason: 'stop',
        toolCalls: [
          {
            toolCallId: 'tool-v5-failure',
            toolName: 'weather',
            input: { city: 'Paris' },
          },
        ],
        toolResults: [
          {
            toolCallId: 'tool-v5-failure',
            toolName: 'weather',
            isError: true,
            output: 'weather provider timeout',
          },
        ],
        response: { id: 'resp-v5-tool-failure', modelId: 'gpt-5' },
      });
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /weather provider timeout/);
});

test('vercel ai sdk generateText hooks support multi-step loop and tool lifecycle', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client, { captureInputs: true, captureOutputs: true });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-loop' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'weather in paris' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-call-1',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-call-1',
      },
      success: true,
      output: { temp_c: 18 },
      durationMs: 240,
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'calling weather',
      finishReason: 'tool-calls',
      toolCalls: [
        {
          toolCallId: 'tool-call-1',
          toolName: 'weather',
          input: { city: 'Paris' },
        },
      ],
      response: { id: 'resp-step-0', modelId: 'gpt-5' },
    });

    hooks.experimental_onStepStart?.({
      stepNumber: 1,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [
        { role: 'user', content: 'weather in paris' },
        {
          role: 'tool',
          content: [{ type: 'tool-result', toolCallId: 'tool-call-1', result: { temp_c: 18 } }],
        },
      ],
    });
    hooks.onStepFinish?.({
      stepNumber: 1,
      stepType: 'tool-result',
      text: 'it is 18c',
      finishReason: 'stop',
      response: { id: 'resp-step-1', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 2);
  assert.equal(generations[0].metadata['sigil.framework.step_type'], 'initial');
  assert.equal(generations[1].metadata['sigil.framework.step_type'], 'tool-result');
  assert.equal(generations[0].conversationId, 'conv-loop');
  assert.equal(generations[1].conversationId, 'conv-loop');
  assert.equal(generations[1].input[1].role, 'tool');

  assert.equal(snapshot.toolExecutions.length, 1);
  const toolExecution = snapshot.toolExecutions[0];
  assert.equal(toolExecution.toolCallId, 'tool-call-1');
  assert.deepEqual(toolExecution.arguments, { city: 'Paris' });
  assert.deepEqual(toolExecution.result, { temp_c: 18 });
  assert.equal(toolExecution.completedAt.getTime() - toolExecution.startedAt.getTime(), 240);
});

test('vercel ai sdk streamText hooks capture TTFT once and record streaming result', async () => {
  const { generations, firstTokenCalls } = await captureSession(
    async (client) => {
      const sigil = createSigilVercelAiSdk(client);
      const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream' });

      hooks.experimental_onStepStart?.({
        stepNumber: 0,
        model: { modelId: 'claude-sonnet-4-5' },
        messages: [{ role: 'user', content: 'stream hello' }],
      });
      hooks.onChunk?.({
        stepNumber: 0,
        chunk: {
          type: 'reasoning',
          text: 'thinking',
        },
      });
      hooks.onChunk?.({
        stepNumber: 0,
        chunk: {
          type: 'text-delta',
          text: 'hel',
        },
      });
      hooks.onChunk?.({
        stepNumber: 0,
        chunk: {
          type: 'text-delta',
          text: 'lo',
        },
      });
      hooks.onStepFinish?.({
        stepNumber: 0,
        stepType: 'initial',
        text: 'hello',
        finishReason: 'stop',
        response: {
          id: 'resp-stream',
          modelId: 'claude-sonnet-4-5',
        },
      });
    },
    { countFirstTokenCalls: true },
  );

  assert.equal(firstTokenCalls, 1);
  assert.equal(generations.length, 1);
  assert.equal(generations[0].mode, 'STREAM');
  assert.equal(generations[0].operationName, 'streamText');
  assert.equal(generations[0].output[0].content, 'hello');
});

test('vercel ai sdk streamText hooks close step and open tools on error', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-error' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-open',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.onError?.({ error: new Error('stream aborted') });
  });

  assert.equal(generations.length, 1);
  assert.match(generations[0].callError ?? '', /stream aborted/);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /stream aborted/);
});

test('vercel ai sdk streamText hooks export error when step start callback is unavailable', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-no-step-start-error' });
    hooks.onError?.({ error: new Error('stream aborted without step start') });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].mode, 'STREAM');
  assert.equal(generations[0].operationName, 'streamText');
  assert.match(generations[0].callError ?? '', /stream aborted without step start/);
});

test('vercel ai sdk streamText hooks close step and open tools on abort', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-abort' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-open-abort',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.onAbort?.();
  });

  assert.equal(generations.length, 1);
  assert.match(generations[0].callError ?? '', /stream aborted/);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /stream aborted/);
});

test('vercel ai sdk streamText hooks export abort when step start callback is unavailable', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-no-step-start-abort' });
    hooks.onAbort?.();
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].mode, 'STREAM');
  assert.equal(generations[0].operationName, 'streamText');
  assert.match(generations[0].callError ?? '', /stream aborted/);
});

test('vercel ai sdk streamText hooks normalize structured abort payloads to abort error', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-structured-abort' });
    hooks.onAbort?.({ steps: [{ stepNumber: 0 }] });
  });

  assert.equal(generations.length, 1);
  assert.match(generations[0].callError ?? '', /stream aborted/);
  assert.doesNotMatch(generations[0].callError ?? '', /\[object Object\]/);
});

test('vercel ai sdk streamText synthetic step uses call start when no step start callback or chunk is observed', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-finish-only-start' });

    await new Promise((resolve) => setTimeout(resolve, 20));

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'hello',
      finishReason: 'stop',
      response: { id: 'resp-stream-finish-only-start', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  const durationMs = generations[0].completedAt.getTime() - generations[0].startedAt.getTime();
  assert.ok(durationMs >= 10);
});

test('vercel ai sdk streamText synthetic step fallback does not reuse first-step start across later steps', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-finish-only-multi-step' });

    await new Promise((resolve) => setTimeout(resolve, 20));
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'first synthetic step',
      finishReason: 'stop',
      response: { id: 'resp-stream-finish-only-multi-step-0', modelId: 'gpt-5' },
    });

    await new Promise((resolve) => setTimeout(resolve, 20));
    hooks.onStepFinish?.({
      stepNumber: 1,
      stepType: 'continue',
      text: 'second synthetic step',
      finishReason: 'stop',
      response: { id: 'resp-stream-finish-only-multi-step-1', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 2);
  const first = generations.find((generation) => generation.output[0]?.content === 'first synthetic step');
  const second = generations.find((generation) => generation.output[0]?.content === 'second synthetic step');
  assert.ok(first);
  assert.ok(second);

  const firstDurationMs = first.completedAt.getTime() - first.startedAt.getTime();
  const secondDurationMs = second.completedAt.getTime() - second.startedAt.getTime();
  assert.ok(firstDurationMs >= 10);
  assert.ok(secondDurationMs < 15);
});

test('vercel ai sdk streamText synthetic step preserves pre-finish start timestamp', async () => {
  const { generations, firstTokenCalls } = await captureSession(
    async (client) => {
      const sigil = createSigilVercelAiSdk(client);
      const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-synthetic-start' });

      hooks.onChunk?.({
        stepNumber: 0,
        chunk: {
          type: 'text-delta',
          text: 'hel',
        },
      });

      await new Promise((resolve) => setTimeout(resolve, 20));

      hooks.onStepFinish?.({
        stepNumber: 0,
        stepType: 'initial',
        text: 'hello',
        finishReason: 'stop',
        response: { id: 'resp-stream-synthetic-start', modelId: 'gpt-5' },
      });
    },
    { countFirstTokenCalls: true },
  );

  assert.equal(generations.length, 1);
  assert.equal(firstTokenCalls, 1);
  const durationMs = generations[0].completedAt.getTime() - generations[0].startedAt.getTime();
  assert.ok(durationMs >= 10);
});

test('vercel ai sdk streamText synthetic step uses response model when step start callback is unavailable', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-model-fallback' });

    hooks.onChunk?.({
      stepNumber: 0,
      chunk: {
        type: 'text-delta',
        text: 'hel',
      },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'hello',
      finishReason: 'stop',
      response: { id: 'resp-stream-model-fallback', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].model.provider, 'openai');
  assert.equal(generations[0].model.name, 'gpt-5');
});

test('vercel ai sdk streamText records observed start from non-text chunks', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-reasoning-start' });

    hooks.onChunk?.({
      stepNumber: 0,
      chunk: {
        type: 'reasoning',
        text: 'thinking',
      },
    });

    await new Promise((resolve) => setTimeout(resolve, 20));

    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'done',
      finishReason: 'stop',
      response: { id: 'resp-stream-reasoning-start', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  const durationMs = generations[0].completedAt.getTime() - generations[0].startedAt.getTime();
  assert.ok(durationMs >= 10);
});

test('vercel ai sdk streamText fallback preserves TTFT when non-text chunks arrive before text', async () => {
  let recordedFirstTokenAt;
  const { generations } = await captureSession(async (client) => {
    const originalStartStreamingGeneration = client.startStreamingGeneration.bind(client);
    client.startStreamingGeneration = (...args) => {
      const recorder = originalStartStreamingGeneration(...args);
      const originalSetFirstTokenAt = recorder.setFirstTokenAt.bind(recorder);
      recorder.setFirstTokenAt = (timestamp) => {
        recordedFirstTokenAt = new Date(timestamp);
        originalSetFirstTokenAt(timestamp);
      };
      return recorder;
    };

    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-reasoning-ttft' });

    hooks.onChunk?.({
      stepNumber: 0,
      chunk: {
        type: 'reasoning',
        text: 'thinking',
      },
    });

    await new Promise((resolve) => setTimeout(resolve, 20));

    hooks.onChunk?.({
      stepNumber: 0,
      chunk: {
        type: 'text-delta',
        text: 'done',
      },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      stepType: 'initial',
      text: 'done',
      finishReason: 'stop',
      response: { id: 'resp-stream-reasoning-ttft', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.ok(recordedFirstTokenAt instanceof Date);
  const generation = generations[0];
  const durationMs = generation.completedAt.getTime() - generation.startedAt.getTime();
  const ttftMs = recordedFirstTokenAt.getTime() - generation.startedAt.getTime();
  assert.ok(durationMs >= 10);
  assert.ok(ttftMs >= 10);
});

test('vercel ai sdk streamText hooks preserve tool spans when step start callback is unavailable', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.streamTextHooks({ conversationId: 'conv-stream-tool-no-step-start' });

    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-no-step-start',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.onAbort?.();
  });

  assert.equal(generations.length, 1);
  assert.match(generations[0].callError ?? '', /stream aborted/);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /stream aborted/);
});

test('vercel ai sdk tool finish error path records call error and duration', async () => {
  const { snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-tool-error' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'run tool' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-error',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-error',
      },
      success: false,
      error: new Error('tool execution failed'),
      durationMs: 125,
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'error',
      error: new Error('step failed due to tool'),
      response: { id: 'resp-tool-error', modelId: 'gpt-5' },
    });
  });

  assert.equal(snapshot.toolExecutions.length, 1);
  const execution = snapshot.toolExecutions[0];
  assert.match(execution.callError ?? '', /tool execution failed/);
  assert.equal(execution.completedAt.getTime() - execution.startedAt.getTime(), 125);
});

test('vercel ai sdk capture toggles omit model and tool payload content', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client, {
      captureInputs: false,
      captureOutputs: false,
    });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-private' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'secret input' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-private',
        toolName: 'private_tool',
        input: { secret: true },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: { toolCallId: 'tool-private' },
      success: true,
      output: { value: 'secret result' },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'secret output',
      finishReason: 'stop',
      response: { id: 'resp-private', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].input, undefined);
  assert.equal(generations[0].output, undefined);

  assert.equal(snapshot.toolExecutions.length, 1);
  assert.equal(snapshot.toolExecutions[0].includeContent, false);
  assert.equal(snapshot.toolExecutions[0].arguments, undefined);
  assert.equal(snapshot.toolExecutions[0].result, undefined);
});

test('vercel ai sdk conversation id precedence explicit then resolver then fallback', async () => {
  const explicit = await captureSingleGeneration(async (client) => {
    const sigil = createSigilVercelAiSdk(client, {
      resolveConversationId: () => 'resolver-conv',
    });
    const hooks = sigil.generateTextHooks({ conversationId: 'explicit-conv' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'ok',
      finishReason: 'stop',
      response: { id: 'resp-explicit', modelId: 'gpt-5' },
    });
  });
  assert.equal(explicit.conversationId, 'explicit-conv');

  const resolver = await captureSingleGeneration(async (client) => {
    const sigil = createSigilVercelAiSdk(client, {
      resolveConversationId: () => 'resolver-conv',
    });
    const hooks = sigil.generateTextHooks();
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'ok',
      finishReason: 'stop',
      response: { id: 'resp-resolver', modelId: 'gpt-5' },
    });
  });
  assert.equal(resolver.conversationId, 'resolver-conv');

  const fallback = await captureSingleGeneration(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'ok',
      finishReason: 'stop',
      response: { id: 'resp-fallback-42', modelId: 'gpt-5' },
    });
  });
  assert.equal(fallback.conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
});

test('vercel ai sdk keeps fallback conversation id aligned between generation and tools', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks();

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'use a tool' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-fallback',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: { toolCallId: 'tool-fallback' },
      success: true,
      output: { temp_c: 18 },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'it is 18c',
      finishReason: 'stop',
      response: { id: 'resp-tool-fallback', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.equal(generations[0].conversationId, snapshot.toolExecutions[0].conversationId);
  assert.equal(generations[0].conversationId, 'sigil:framework:vercel-ai-sdk:call-1:step-0');
});

test('vercel ai sdk closes open tool recorders when parent step errors', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-parent-error' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-unfinished',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'error',
      error: new Error('step hard error'),
      response: { id: 'resp-parent-error', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.match(generations[0].callError ?? '', /step hard error/);
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /step hard error/);
});

test('vercel ai sdk cleans step and tool state before rethrowing generation recorder errors', async () => {
  const { generations, snapshot } = await captureSession(async (client) => {
    const originalStartGeneration = client.startGeneration.bind(client);
    let shouldInjectRecorderError = true;
    client.startGeneration = (...args) => {
      const recorder = originalStartGeneration(...args);
      if (!shouldInjectRecorderError) {
        return recorder;
      }
      shouldInjectRecorderError = false;
      const originalGetError = recorder.getError.bind(recorder);
      recorder.getError = () => originalGetError() ?? new Error('injected generation recorder error');
      return recorder;
    };

    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-recorder-error-step-cleanup' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'first pass' }],
    });
    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-unfinished-recorder-error',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    assert.throws(() => {
      hooks.onStepFinish?.({
        stepNumber: 0,
        text: 'first pass output',
        finishReason: 'stop',
        response: { id: 'resp-recorder-error-1', modelId: 'gpt-5' },
      });
    }, /injected generation recorder error/);

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'second pass' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'second pass output',
      finishReason: 'stop',
      response: { id: 'resp-recorder-error-2', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 2);
  assert.equal(generations[1].output[0].content, 'second pass output');
  assert.equal(snapshot.toolExecutions.length, 1);
  assert.match(snapshot.toolExecutions[0].callError ?? '', /tool call did not finish before step completion/);
});

test('vercel ai sdk removes tool state before rethrowing tool recorder errors', async () => {
  const { snapshot } = await captureSession(async (client) => {
    const originalStartToolExecution = client.startToolExecution.bind(client);
    let shouldInjectRecorderError = true;
    client.startToolExecution = (...args) => {
      const recorder = originalStartToolExecution(...args);
      if (!shouldInjectRecorderError) {
        return recorder;
      }
      shouldInjectRecorderError = false;
      const originalGetError = recorder.getError.bind(recorder);
      recorder.getError = () => originalGetError() ?? new Error('injected tool recorder error');
      return recorder;
    };

    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-recorder-error-tool-cleanup' });

    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'reuse tool id' }],
    });

    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-reused-id',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    assert.throws(() => {
      hooks.experimental_onToolCallFinish?.({
        stepNumber: 0,
        toolCall: { toolCallId: 'tool-reused-id' },
        success: true,
        output: { temp_c: 18 },
      });
    }, /injected tool recorder error/);

    hooks.experimental_onToolCallStart?.({
      stepNumber: 0,
      toolCall: {
        toolCallId: 'tool-reused-id',
        toolName: 'weather',
        input: { city: 'Paris' },
      },
    });
    hooks.experimental_onToolCallFinish?.({
      stepNumber: 0,
      toolCall: { toolCallId: 'tool-reused-id' },
      success: true,
      output: { temp_c: 19 },
    });

    hooks.onStepFinish?.({
      stepNumber: 0,
      text: 'done',
      finishReason: 'stop',
      response: { id: 'resp-recorder-error-tool', modelId: 'gpt-5' },
    });
  });

  assert.equal(snapshot.toolExecutions.length, 2);
  assert.equal(snapshot.toolExecutions[0].result.temp_c, 18);
  assert.equal(snapshot.toolExecutions[1].result.temp_c, 19);
});

test('vercel ai sdk isolates step state across concurrent calls', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooksA = sigil.generateTextHooks({ conversationId: 'conv-a' });
    const hooksB = sigil.generateTextHooks({ conversationId: 'conv-b' });

    hooksA.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'from-a' }],
    });
    hooksB.experimental_onStepStart?.({
      stepNumber: 0,
      model: { modelId: 'claude-sonnet-4-5' },
      messages: [{ role: 'user', content: 'from-b' }],
    });
    hooksB.onStepFinish?.({
      stepNumber: 0,
      text: 'out-b',
      finishReason: 'stop',
      response: { id: 'resp-b', modelId: 'claude-sonnet-4-5' },
    });
    hooksA.onStepFinish?.({
      stepNumber: 0,
      text: 'out-a',
      finishReason: 'stop',
      response: { id: 'resp-a', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 2);
  const generationA = generations.find((generation) => generation.conversationId === 'conv-a');
  const generationB = generations.find((generation) => generation.conversationId === 'conv-b');
  assert.ok(generationA);
  assert.ok(generationB);
  assert.equal(generationA.input[0].content, 'from-a');
  assert.equal(generationB.input[0].content, 'from-b');
  assert.equal(generationA.output[0].content, 'out-a');
  assert.equal(generationB.output[0].content, 'out-b');
});

test('vercel ai sdk captures Output.object schema into Generation.tools', async () => {
  const schema = {
    type: 'object',
    properties: {
      question: { type: 'string' },
      skills: { type: 'array', items: { type: 'string' } },
    },
    required: ['question', 'skills'],
  };
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client, { agentName: 'structured-agent' });
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-output-schema' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'generate a question' }],
      output: {
        name: 'object',
        responseFormat: Promise.resolve({
          type: 'json',
          schema,
          name: 'question_skills',
          description: 'Question with associated skills',
        }),
      },
    });
    await hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'stop',
      text: '{"question":"q?","skills":["s1"]}',
      response: { id: 'resp-schema', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  const [generation] = generations;
  assert.ok(generation.tools);
  assert.equal(generation.tools.length, 1);
  const [tool] = generation.tools;
  assert.equal(tool.name, 'question_skills');
  assert.equal(tool.type, 'output_schema');
  assert.equal(tool.description, 'Question with associated skills');
  assert.deepEqual(JSON.parse(tool.inputSchemaJSON), schema);
});

test('vercel ai sdk omits tools entry when no output schema is configured', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-no-schema' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
    });
    hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'hi',
      response: { id: 'resp-no-schema', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].tools, undefined);
});

test('vercel ai sdk recovers without tools when output schema extraction fails', async () => {
  const { generations } = await captureSession(async (client) => {
    const sigil = createSigilVercelAiSdk(client);
    const hooks = sigil.generateTextHooks({ conversationId: 'conv-schema-fail' });
    hooks.experimental_onStepStart?.({
      stepNumber: 0,
      model: { provider: 'openai', modelId: 'gpt-5' },
      messages: [{ role: 'user', content: 'hello' }],
      output: {
        name: 'object',
        responseFormat: Promise.reject(new Error('schema resolution failed')),
      },
    });
    await hooks.onStepFinish?.({
      stepNumber: 0,
      finishReason: 'stop',
      text: 'recovered',
      response: { id: 'resp-fail', modelId: 'gpt-5' },
    });
  });

  assert.equal(generations.length, 1);
  assert.equal(generations[0].tools, undefined);
  assert.equal(generations[0].output[0].content, 'recovered');
});

async function captureSingleGeneration(run) {
  const { generations } = await captureSession(run);
  assert.equal(generations.length, 1);
  return generations[0];
}

async function captureSession(run, options = {}) {
  const exporter = new CapturingExporter();
  const defaults = defaultConfig();
  const client = new SigilClient({
    generationExport: {
      ...defaults.generationExport,
      batchSize: 10,
      flushIntervalMs: 60_000,
    },
    generationExporter: exporter,
  });

  let firstTokenCalls = 0;
  if (options.countFirstTokenCalls === true) {
    const originalStartStreamingGeneration = client.startStreamingGeneration.bind(client);
    client.startStreamingGeneration = (...args) => {
      const recorder = originalStartStreamingGeneration(...args);
      if (typeof recorder?.setFirstTokenAt === 'function') {
        const originalSetFirstTokenAt = recorder.setFirstTokenAt.bind(recorder);
        recorder.setFirstTokenAt = (timestamp) => {
          firstTokenCalls += 1;
          originalSetFirstTokenAt(timestamp);
        };
      }
      return recorder;
    };
  }

  try {
    await run(client);
    await client.flush();
    const generations = [];
    for (const request of exporter.requests) {
      for (const generation of request.generations) {
        generations.push(generation);
      }
    }
    const snapshot = client.debugSnapshot();
    return {
      generations,
      snapshot,
      firstTokenCalls,
    };
  } finally {
    await client.shutdown();
  }
}
