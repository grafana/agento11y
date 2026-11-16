import assert from 'node:assert/strict';
import test from 'node:test';
import { awaitAllCallbacks } from '@langchain/core/callbacks/promises';
import { HumanMessage } from '@langchain/core/messages';
import { StringOutputParser } from '@langchain/core/output_parsers';
import { ChatPromptTemplate } from '@langchain/core/prompts';
import { FakeListChatModel } from '@langchain/core/utils/testing';
import { END, MessagesAnnotation, START, StateGraph } from '@langchain/langgraph';
import { Agento11yLangChainHandler } from '../.test-dist/frameworks/langchain/index.js';
import { Agento11yLangGraphHandler } from '../.test-dist/frameworks/langgraph/index.js';
import { Agento11yLlamaIndexHandler } from '../.test-dist/frameworks/llamaindex/index.js';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

class CapturingExporter {
  generations = [];
  workflowSteps = [];

  async exportGenerations(request) {
    this.generations.push(...structuredClone(request.generations));
    return { results: request.generations.map((generation) => ({ generationId: generation.id, accepted: true })) };
  }

  async exportWorkflowSteps(request) {
    this.workflowSteps.push(...structuredClone(request.workflowSteps));
    return { results: request.workflowSteps.map((step) => ({ stepId: step.id, accepted: true })) };
  }
}

test('captures each node of a real LangGraph graph as a workflow step', async () => {
  const { steps, generations } = await capture(async (client) => {
    const model = new FakeListChatModel({ responses: ['route: answer', 'Paris'] });
    const graph = new StateGraph(MessagesAnnotation)
      .addNode('route', async (state, config) => ({ messages: [await model.invoke(state.messages, config)] }))
      .addNode('answer', async (state, config) => ({ messages: [await model.invoke(state.messages, config)] }))
      .addEdge(START, 'route')
      .addEdge('route', 'answer')
      .addEdge('answer', END)
      .compile();
    const handler = new Agento11yLangGraphHandler(client, { agentName: 'geo-agent', captureWorkflowSteps: true });

    await graph.invoke(
      { messages: [new HumanMessage('Capital of France?')] },
      { callbacks: [handler], configurable: { thread_id: 'thread-1' } },
    );
  });

  // The hidden `__start__` run is not a step, matching the Python handler.
  assert.deepEqual(
    steps.map((step) => step.stepName),
    ['route', 'answer'],
  );
  const [route, answer] = steps;
  assert.deepEqual(route.parentStepIds ?? [], []);
  assert.deepEqual(answer.parentStepIds, [route.id]);

  assert.equal(generations.length, 2);
  assert.deepEqual(route.linkedGenerationIds, [generations[0].id]);
  assert.deepEqual(answer.linkedGenerationIds, [generations[1].id]);
  assert.deepEqual(generations[1].parentGenerationIds, [generations[0].id]);

  for (const item of [...steps, ...generations]) {
    assert.equal(item.conversationId, 'thread-1');
  }
  assert.equal(route.framework, 'langgraph');
  assert.equal(route.agentName, 'geo-agent');
  assert.equal(route.tags['agento11y.workflow_step.type'], 'chain');
  assert.match(JSON.stringify(route.inputState), /Capital of France\?/);
  assert.match(JSON.stringify(answer.outputState), /Paris/);
});

test('a metadata_only client exports the steps of a real graph without their state', async () => {
  const { steps } = await capture(
    async (client) => {
      const model = new FakeListChatModel({ responses: ['Paris'] });
      const graph = new StateGraph(MessagesAnnotation)
        .addNode('answer', async (state, config) => ({ messages: [await model.invoke(state.messages, config)] }))
        .addNode('fail', async () => {
          throw new Error('upstream returned 503 for Capital of France?');
        })
        .addEdge(START, 'answer')
        .addEdge('answer', 'fail')
        .addEdge('fail', END)
        .compile();
      const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

      await assert.rejects(
        graph.invoke({ messages: [new HumanMessage('Capital of France?')] }, { callbacks: [handler] }),
      );
    },
    {},
    { contentCapture: 'metadata_only' },
  );

  assert.deepEqual(
    steps.map((step) => step.stepName),
    ['answer', 'fail'],
  );
  for (const step of steps) {
    assert.deepEqual(step.inputState ?? {}, {});
    assert.deepEqual(step.outputState ?? {}, {});
  }
  assert.equal(steps[1].error, 'server_error');
  assert.equal(steps[0].linkedGenerationIds.length, 1);
});

test('workflow step uses the handler agent name, not the step name', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { agentName: 'my-pipeline', captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'router' }, { q: 'hi' }, 'step', 'root');
    await handler.handleChainEnd({ foo: 'bar' }, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 1);
  assert.equal(steps[0].agentName, 'my-pipeline');
  assert.equal(steps[0].framework, 'langgraph');
  assert.equal(steps[0].stepName, 'router');
  assert.deepEqual(steps[0].inputState, { q: 'hi' });
  assert.deepEqual(steps[0].outputState, { foo: 'bar' });
});

test('linkedGenerationIds walks the parent chain for nested LLM runs', async () => {
  const { steps, generations } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'agent' }, {}, 'step', 'root');
    // A sub-chain inside the workflow step, such as an LCEL composition.
    await handler.handleChainStart({ name: 'router' }, {}, 'nested', 'step');
    await handler.handleChatModelStart({ name: 'ChatOpenAI' }, [[]], 'llm', 'nested', {
      invocation_params: { model: 'gpt-5' },
    });
    await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, 'llm');
    await handler.handleChainEnd({}, 'nested');
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 1);
  assert.equal(generations.length, 1);
  assert.deepEqual(steps[0].linkedGenerationIds, [generations[0].id]);
});

test('concurrent graph roots stay isolated', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root-a', undefined);
    await handler.handleChainStart(null, {}, 'root-b', undefined);
    await handler.handleChainStart({ name: 'node_a' }, {}, 'step-a', 'root-a');
    await handler.handleChainStart({ name: 'node_b' }, {}, 'step-b', 'root-b');
    await handler.handleChainEnd({ r: 'a' }, 'step-a');
    await handler.handleChainEnd({ r: 'b' }, 'step-b');
    await handler.handleChainEnd({}, 'root-a');
    await handler.handleChainEnd({}, 'root-b');
  });

  // Each step is the first node of its own graph, so neither has a parent.
  assert.equal(steps.length, 2);
  for (const step of steps) {
    assert.deepEqual(step.parentStepIds ?? [], []);
  }
});

test('graph state is cleaned up after the root ends', async () => {
  let handler;
  await capture(async (client) => {
    handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'router' }, {}, 'step', 'root');
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(handler.graphRootRunKeys.size, 0);
  assert.equal(handler.graphRunConversationId.size, 0);
  assert.equal(handler.graphRunLastStepId.size, 0);
  assert.equal(handler.graphRunLastGenerationId.size, 0);
  assert.equal(handler.workflowStepRuns.size, 0);
  assert.equal(handler.runToGraphKey.size, 0);
});

test('sequential workflow steps get parentStepIds', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'first' }, {}, 'first', 'root');
    await handler.handleChainEnd({}, 'first');
    await handler.handleChainStart({ name: 'second' }, {}, 'second', 'root');
    await handler.handleChainEnd({}, 'second');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 2);
  const [first, second] = steps;
  assert.deepEqual(first.parentStepIds ?? [], []);
  assert.deepEqual(second.parentStepIds, [first.id]);
});

test('a failing node records the error on its workflow step', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'tool_node' }, {}, 'step', 'root');
    await handler.handleChainError(new Error('tool timed out'), 'step');
    await handler.handleChainError(new Error('tool timed out'), 'root');
  });

  assert.equal(steps.length, 1);
  assert.equal(steps[0].error, 'tool timed out');
  assert.deepEqual(steps[0].outputState ?? {}, {});
});

test('captureInputs and captureOutputs false leave the step state empty', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, {
      captureWorkflowSteps: true,
      captureInputs: false,
      captureOutputs: false,
    });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'agent' }, { q: 'hi' }, 'step', 'root');
    await handler.handleChainEnd({ a: 'hello' }, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 1);
  assert.deepEqual(steps[0].inputState ?? {}, {});
  assert.deepEqual(steps[0].outputState ?? {}, {});
});

test('hidden child runs of a graph root are not workflow steps', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: '__start__' }, {}, 'start', 'root', ['graph:step:0', 'langsmith:hidden']);
    await handler.handleChainEnd({}, 'start');
    await handler.handleChainStart({ name: 'agent' }, {}, 'step', 'root', ['graph:step:1']);
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.deepEqual(
    steps.map((step) => step.stepName),
    ['agent'],
  );
});

test('langchain handler names steps by LangGraph node', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangChainHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'RunnableSequence' }, {}, 'step', 'root', [], {
      langgraph_node: 'retrieve',
    });
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 1);
  assert.equal(steps[0].stepName, 'retrieve');
  assert.equal(steps[0].framework, 'langchain');
  assert.equal(steps[0].metadata['agento11y.framework.langgraph.node'], 'retrieve');
});

test('concurrent graph runs on one handler keep their own conversations', async () => {
  const { steps, generations } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });
    const graph = slowAgentGraph();

    const invoke = (threadId) =>
      graph.invoke(
        { messages: [new HumanMessage(threadId)] },
        { callbacks: [handler], configurable: { thread_id: threadId } },
      );
    // Start the second run while the first one is inside its node.
    const first = invoke('customer-a');
    await new Promise((resolve) => setTimeout(resolve, 5));
    await Promise.all([first, invoke('customer-b')]);
  });

  assert.equal(steps.length, 2);
  assert.equal(generations.length, 2);
  for (const step of steps) {
    assert.match(JSON.stringify(step.inputState), new RegExp(step.conversationId));
  }
  assert.deepEqual(
    new Set(generations.map((generation) => generation.conversationId)),
    new Set(['customer-a', 'customer-b']),
  );
});

test('a stream stopped early does not change the conversation of later runs', async () => {
  let handler;
  const { steps } = await capture(async (client) => {
    handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });
    const graph = new StateGraph(MessagesAnnotation)
      .addNode('n1', async () => ({}))
      .addNode('n2', async () => ({}))
      .addNode('n3', async () => ({}))
      .addEdge(START, 'n1')
      .addEdge('n1', 'n2')
      .addEdge('n2', 'n3')
      .addEdge('n3', END)
      .compile();

    // LangGraph JS sends no chain end or chain error for the root of a
    // stream the caller stops early.
    for await (const _update of await graph.stream(
      { messages: [new HumanMessage('a')] },
      { callbacks: [handler], configurable: { thread_id: 'customer-a' } },
    )) {
      break;
    }
    await graph.invoke(
      { messages: [new HumanMessage('b')] },
      { callbacks: [handler], configurable: { thread_id: 'customer-b' } },
    );
  });

  const secondRun = steps.filter((step) => JSON.stringify(step.inputState).includes('"b"'));
  assert.equal(secondRun.length, 3);
  for (const step of secondRun) {
    assert.equal(step.conversationId, 'customer-b');
  }
});

test('graph runs that never end are capped per handler', async () => {
  let handler;
  await capture(async (client) => {
    handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    for (let index = 0; index <= 1000; index++) {
      await handler.handleChainStart(null, {}, `root-${index}`, undefined);
      await handler.handleChainStart({ name: 'agent' }, { index }, `step-${index}`, `root-${index}`);
    }
  });

  assert.equal(handler.graphRootRunKeys.size, 1000);
  assert.equal(handler.graphRootRunKeys.has('root-0'), false);
  assert.equal(handler.workflowStepRuns.has('step-0'), false);
  assert.equal(handler.workflowStepRuns.size, 1000);
});

test('a step still running when its root ends is dropped and the state is cleared', async () => {
  let handler;
  const { steps } = await capture(async (client) => {
    handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'agent' }, {}, 'step', 'root');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 0);
  assert.equal(handler.workflowStepRuns.size, 0);
  assert.equal(handler.runToGraphKey.size, 0);
});

test('a step the client rejects is skipped without failing the callback', async () => {
  const { steps } = await capture(
    async (client) => {
      const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

      await handler.handleChainStart(null, {}, 'root', undefined);
      await handler.handleChainStart({ name: 'big' }, { blob: 'x'.repeat(4096) }, 'big', 'root');
      await handler.handleChainEnd({}, 'big');
      await handler.handleChainStart({ name: 'small' }, {}, 'small', 'root');
      await handler.handleChainEnd({}, 'small');
      await handler.handleChainEnd({}, 'root');
    },
    { payloadMaxBytes: 1024 },
  );

  // The rejected step is not the parent of the next one.
  assert.deepEqual(
    steps.map((step) => step.stepName),
    ['small'],
  );
  assert.deepEqual(steps[0].parentStepIds ?? [], []);
});

test('error text is kept for errors that are not Error instances', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'a' }, {}, 'a', 'root');
    await handler.handleChainError({ message: 'object error' }, 'a');
    await handler.handleChainStart({ name: 'b' }, {}, 'b', 'root');
    await handler.handleChainError('string error', 'b');
    await handler.handleChainEnd({}, 'root');
  });

  assert.deepEqual(
    steps.map((step) => step.error),
    ['object error', 'string error'],
  );
});

test('one value that JSON cannot encode does not hide the rest of the state', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client, { captureWorkflowSteps: true });
    const cyclic = { name: 'node' };
    cyclic.self = cyclic;

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'a' }, { count: 10n, ok: 'yes', tags: new Set(['x']) }, 'a', 'root');
    await handler.handleChainEnd({ cyclic, lookup: new Map([['k', 'v']]) }, 'a');
    await handler.handleChainEnd({}, 'root');
  });

  assert.deepEqual(steps[0].inputState, { count: '10', ok: 'yes', tags: ['x'] });
  assert.deepEqual(steps[0].outputState, { cyclic: { name: 'node', self: '[Circular]' }, lookup: { k: 'v' } });
});

test('langchain handler names the steps of a real LCEL chain by run name', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangChainHandler(client, { captureWorkflowSteps: true });
    const chain = ChatPromptTemplate.fromMessages([['human', '{q}']])
      .pipe(new FakeListChatModel({ responses: ['ok'] }))
      .pipe(new StringOutputParser());

    await chain.invoke({ q: 'hi' }, { callbacks: [handler] });
  });

  // Not the class path (langchain_core.prompts.chat.ChatPromptTemplate).
  assert.deepEqual(
    steps.map((step) => step.stepName),
    ['ChatPromptTemplate', 'StrOutputParser'],
  );
  assert.deepEqual(steps[1].parentStepIds, [steps[0].id]);
});

test('state that is not an object is kept under raw', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLangChainHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'format' }, 'plain text', 'step', 'root');
    await handler.handleChainEnd(['a', 'b'], 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.deepEqual(steps[0].inputState, { raw: 'plain text' });
  assert.deepEqual(steps[0].outputState, { raw: ['a', 'b'] });
});

test('captureWorkflowSteps is ignored by handlers outside LangChain and LangGraph', async () => {
  const { steps } = await capture(async (client) => {
    const handler = new Agento11yLlamaIndexHandler(client, { captureWorkflowSteps: true });

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'agent' }, {}, 'step', 'root');
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 0);
});

test('without captureWorkflowSteps no steps are recorded and generations are not linked', async () => {
  const { steps, generations } = await capture(async (client) => {
    const handler = new Agento11yLangGraphHandler(client);

    await handler.handleChainStart(null, {}, 'root', undefined);
    await handler.handleChainStart({ name: 'agent' }, {}, 'step', 'root');
    for (const runId of ['llm-1', 'llm-2']) {
      await handler.handleChatModelStart({ name: 'ChatOpenAI' }, [[]], runId, 'step', {
        invocation_params: { model: 'gpt-5' },
      });
      await handler.handleLLMEnd({ generations: [[{ text: 'ok' }]] }, runId);
    }
    await handler.handleChainEnd({}, 'step');
    await handler.handleChainEnd({}, 'root');
  });

  assert.equal(steps.length, 0);
  assert.equal(generations.length, 2);
  for (const generation of generations) {
    assert.deepEqual(generation.parentGenerationIds ?? [], []);
  }
});

// One node that waits before calling the model, so two invocations overlap.
function slowAgentGraph() {
  const model = new FakeListChatModel({ responses: ['ok', 'ok'] });
  return new StateGraph(MessagesAnnotation)
    .addNode('agent', async (state, config) => {
      await new Promise((resolve) => setTimeout(resolve, 20));
      return { messages: [await model.invoke(state.messages, config)] };
    })
    .addEdge(START, 'agent')
    .addEdge('agent', END)
    .compile();
}

async function capture(run, exportOverrides = {}, clientOverrides = {}) {
  const exporter = new CapturingExporter();
  const client = new Agento11yClient({
    ...clientOverrides,
    generationExport: {
      ...defaultConfig().generationExport,
      batchSize: 50,
      flushIntervalMs: 60_000,
      ...exportOverrides,
    },
    generationExporter: exporter,
  });

  try {
    await run(client);
    // LangChain runs callbacks in a background queue unless a handler asks to
    // be awaited, so wait for it before flushing.
    await awaitAllCallbacks();
    await client.flush();
  } finally {
    await client.shutdown();
  }
  return { steps: exporter.workflowSteps, generations: exporter.generations };
}
