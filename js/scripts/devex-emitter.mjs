import { setTimeout as sleep } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

const LANGUAGE = 'javascript';
const SOURCES = ['openai', 'anthropic', 'gemini', 'mistral'];
const PERSONAS = ['planner', 'retriever', 'executor'];
let sdkModulePromise;
let frameworkModulePromise;

async function loadSdkModule() {
  if (sdkModulePromise === undefined) {
    sdkModulePromise = import('../dist/index.js');
  }
  return sdkModulePromise;
}

async function loadFrameworkModules() {
  if (frameworkModulePromise === undefined) {
    frameworkModulePromise = Promise.all([
      import('../dist/frameworks/langchain/index.js'),
      import('../dist/frameworks/langgraph/index.js'),
      import('../dist/frameworks/openai-agents/index.js'),
      import('../dist/frameworks/llamaindex/index.js'),
      import('../dist/frameworks/google-adk/index.js'),
    ]).then(([langchain, langgraph, openaiAgents, llamaindex, googleAdk]) => ({
      langchain,
      langgraph,
      openaiAgents,
      llamaindex,
      googleAdk,
    }));
  }
  return frameworkModulePromise;
}

export function intFromEnv(key, defaultValue) {
  const raw = process.env[key];
  if (raw === undefined || raw.trim().length === 0) {
    return defaultValue;
  }
  const value = Number.parseInt(raw, 10);
  if (!Number.isFinite(value) || value <= 0) {
    return defaultValue;
  }
  return value;
}

export function stringFromEnv(key, defaultValue) {
  const raw = process.env[key];
  if (raw === undefined) {
    return defaultValue;
  }
  const value = raw.trim();
  return value.length === 0 ? defaultValue : value;
}

export function loadConfig() {
  return {
    intervalMs: intFromEnv('SIGIL_TRAFFIC_INTERVAL_MS', 2000),
    streamPercent: intFromEnv('SIGIL_TRAFFIC_STREAM_PERCENT', 30),
    conversations: intFromEnv('SIGIL_TRAFFIC_CONVERSATIONS', 3),
    rotateTurns: intFromEnv('SIGIL_TRAFFIC_ROTATE_TURNS', 24),
    customProvider: stringFromEnv('SIGIL_TRAFFIC_CUSTOM_PROVIDER', 'mistral'),
    genGrpcEndpoint: stringFromEnv('SIGIL_TRAFFIC_GEN_GRPC_ENDPOINT', 'sigil:4317'),
    maxCycles: intFromEnv('SIGIL_TRAFFIC_MAX_CYCLES', 0),
  };
}

export function sourceTagFor(source) {
  return source === 'mistral' ? 'core_custom' : 'provider_wrapper';
}

export function providerShapeFor(source, turn = 0) {
  switch (source) {
    case 'openai':
      return turn % 2 === 0 ? 'openai_chat_completions' : 'openai_responses';
    case 'anthropic':
      return 'messages';
    case 'gemini':
      return 'generate_content';
    default:
      return 'core_generation';
  }
}

export function scenarioFor(source, turn) {
  const even = turn % 2 === 0;
  if (source === 'openai') {
    return even ? 'openai_briefing' : 'openai_live_status';
  }
  if (source === 'anthropic') {
    return even ? 'anthropic_reasoning' : 'anthropic_delta';
  }
  if (source === 'gemini') {
    return even ? 'gemini_tool_shape' : 'gemini_stream_story';
  }
  return even ? 'custom_mistral_sync' : 'custom_mistral_stream';
}

export function personaForTurn(turn) {
  return PERSONAS[turn % PERSONAS.length];
}

export function newConversationId(source, slot) {
  return `devex-${LANGUAGE}-${source}-${slot}-${Date.now()}`;
}

export function chooseMode(randomValue, streamPercent) {
  return randomValue < streamPercent ? 'STREAM' : 'SYNC';
}

export function createSourceState(conversations) {
  return {
    cursor: 0,
    slots: Array.from({ length: conversations }, () => ({
      conversationId: '',
      turn: 0,
    })),
  };
}

export function resolveThread(state, rotateTurns, source, slot) {
  const thread = state.slots[slot];
  if (thread.conversationId.length === 0 || thread.turn >= rotateTurns) {
    thread.conversationId = newConversationId(source, slot);
    thread.turn = 0;
  }
  return thread;
}

export function buildTagsAndMetadata(source, mode, turn, slot) {
  const agentPersona = personaForTurn(turn);
  return {
    agentPersona,
    tags: {
      'agento11y.devex.language': LANGUAGE,
      'agento11y.devex.provider': source,
      'agento11y.devex.source': sourceTagFor(source),
      'agento11y.devex.scenario': scenarioFor(source, turn),
      'agento11y.devex.mode': mode,
    },
    metadata: {
      turn_index: turn,
      conversation_slot: slot,
      agent_persona: agentPersona,
      emitter: 'sdk-traffic',
      provider_shape: providerShapeFor(source, turn),
    },
  };
}

async function emitOpenAISync(sdk, client, context) {
  const request = {
    model: 'gpt-5',
    max_completion_tokens: 320,
    temperature: 0.2,
    top_p: 0.9,
    reasoning: { effort: 'medium', max_output_tokens: 768 },
    messages: [
      { role: 'system', content: 'Return compact project-planning bullets.' },
      { role: 'user', content: `Draft release checkpoint plan #${context.turn}.` },
    ],
  };

  await sdk.openai.chat.completions.create(
    client,
    request,
    async () => ({
      id: `js-openai-sync-${context.turn}`,
      model: 'gpt-5',
      object: 'chat.completion',
      created: 0,
      choices: [
        {
          index: 0,
          finish_reason: 'stop',
          message: {
            role: 'assistant',
            content: `Plan ${context.turn}: validate rollout, assign owner, publish timeline.`,
          },
        },
      ],
      usage: {
        prompt_tokens: 88 + (context.turn % 9),
        completion_tokens: 26 + (context.turn % 7),
        total_tokens: 114 + (context.turn % 13),
      },
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitOpenAIStream(sdk, client, context) {
  const request = {
    model: 'gpt-5',
    stream: true,
    max_completion_tokens: 220,
    reasoning: { effort: 'medium', max_output_tokens: 640 },
    messages: [
      { role: 'system', content: 'Stream incident status updates in short clauses.' },
      { role: 'user', content: `Stream checkpoint status for ticket ${context.turn}.` },
    ],
  };

  await sdk.openai.chat.completions.stream(
    client,
    request,
    async () => ({
      events: [
        {
          id: `js-openai-stream-${context.turn}`,
          model: 'gpt-5',
          created: 0,
          object: 'chat.completion.chunk',
          choices: [{ index: 0, delta: { content: 'Ticket update: canary healthy' } }],
        },
        {
          id: `js-openai-stream-${context.turn}`,
          model: 'gpt-5',
          created: 0,
          object: 'chat.completion.chunk',
          choices: [{ index: 0, delta: { content: '; promote gate passed.' }, finish_reason: 'stop' }],
        },
      ],
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitOpenAIResponsesSync(sdk, client, context) {
  const request = {
    model: 'gpt-5',
    instructions: 'Return concise plan bullets with one action per line.',
    input: [
      {
        type: 'message',
        role: 'user',
        content: [{ type: 'input_text', text: `Draft release checkpoint plan #${context.turn}.` }],
      },
    ],
    max_output_tokens: 320,
    temperature: 0.2,
    top_p: 0.9,
    reasoning: { effort: 'medium', max_output_tokens: 768 },
  };

  await sdk.openai.responses.create(
    client,
    request,
    async () => ({
      id: `js-openai-responses-sync-${context.turn}`,
      object: 'response',
      model: 'gpt-5',
      output: [
        {
          id: `js-openai-responses-sync-msg-${context.turn}`,
          type: 'message',
          role: 'assistant',
          status: 'completed',
          content: [
            {
              type: 'output_text',
              text: `Plan ${context.turn}: validate rollout, assign owner, publish timeline.`,
              annotations: [],
            },
          ],
        },
      ],
      status: 'completed',
      parallel_tool_calls: false,
      temperature: 0.2,
      top_p: 0.9,
      tools: [],
      created_at: 0,
      incomplete_details: null,
      metadata: {},
      error: null,
      usage: {
        input_tokens: 88 + (context.turn % 9),
        output_tokens: 26 + (context.turn % 7),
        total_tokens: 114 + (context.turn % 13),
        input_tokens_details: { cached_tokens: 3 },
        output_tokens_details: { reasoning_tokens: 4 },
      },
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitOpenAIResponsesStream(sdk, client, context) {
  const request = {
    model: 'gpt-5',
    stream: true,
    instructions: 'Stream concise incident status deltas.',
    input: `Stream checkpoint status for ticket ${context.turn}.`,
    max_output_tokens: 220,
  };

  await sdk.openai.responses.stream(
    client,
    request,
    async () => ({
      events: [
        {
          type: 'response.output_text.delta',
          sequence_number: 1,
          output_index: 0,
          item_id: `js-openai-responses-stream-msg-${context.turn}`,
          content_index: 0,
          delta: 'Ticket update: canary healthy',
        },
        {
          type: 'response.output_text.delta',
          sequence_number: 2,
          output_index: 0,
          item_id: `js-openai-responses-stream-msg-${context.turn}`,
          content_index: 0,
          delta: '; promote gate passed.',
        },
      ],
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitAnthropicSync(sdk, client, context) {
  const request = {
    model: 'claude-sonnet-4-5',
    max_tokens: 384,
    system: [{ type: 'text', text: 'Reason in two phases: diagnosis then recommendation.' }],
    messages: [
      { role: 'user', content: [{ type: 'text', text: `Summarize reliability drift set ${context.turn}.` }] },
    ],
  };

  await sdk.anthropic.messages.create(
    client,
    request,
    async () => ({
      id: `js-anthropic-sync-${context.turn}`,
      model: 'claude-sonnet-4-5',
      role: 'assistant',
      content: [{ type: 'text', text: `Diagnosis ${context.turn}: latency drift in eu-west. Recommendation: rebalance workers.` }],
      stop_reason: 'end_turn',
      usage: {
        input_tokens: 73 + (context.turn % 8),
        output_tokens: 31 + (context.turn % 5),
        total_tokens: 104 + (context.turn % 11),
        cache_read_input_tokens: 12,
      },
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitAnthropicStream(sdk, client, context) {
  const request = {
    model: 'claude-sonnet-4-5',
    max_tokens: 384,
    system: [{ type: 'text', text: 'Use concise streaming deltas for operational narration.' }],
    messages: [
      { role: 'user', content: [{ type: 'text', text: `Stream mitigation deltas for change ${context.turn}.` }] },
    ],
  };

  await sdk.anthropic.messages.stream(
    client,
    request,
    async () => ({
      outputText: `Change ${context.turn}: rollback guard armed; verification complete.`,
      finalResponse: {
        id: `js-anthropic-stream-${context.turn}`,
        model: 'claude-sonnet-4-5',
        role: 'assistant',
        content: [{ type: 'text', text: `Change ${context.turn}: rollback guard armed; verification complete.` }],
        stop_reason: 'end_turn',
        usage: {
          input_tokens: 46 + (context.turn % 6),
          output_tokens: 18 + (context.turn % 4),
          total_tokens: 64 + (context.turn % 8),
        },
      },
      events: [
        { type: 'content_block_delta', delta: { type: 'text_delta', text: 'rollback guard armed' } },
        { type: 'content_block_delta', delta: { type: 'text_delta', text: '; verification complete.' } },
      ],
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitGeminiSync(sdk, client, context) {
  const model = 'gemini-2.5-pro';
  const contents = [
    { role: 'user', parts: [{ text: `Generate launch note ${context.turn} using function-style tone.` }] },
    {
      role: 'user',
      parts: [
        {
          functionResponse: {
            id: 'release_metrics',
            name: 'release_metrics',
            response: { tool: 'release_metrics', status: 'green' },
          },
        },
      ],
    },
  ];
  const config = {
    systemInstruction: { role: 'user', parts: [{ text: 'Write release notes with explicit structured tool language.' }] },
    toolConfig: { functionCallingConfig: { mode: 'ANY' } },
    thinkingConfig: { includeThoughts: true, thinkingBudget: 1536 },
  };

  await sdk.gemini.models.generateContent(
    client,
    model,
    contents,
    config,
    async () => ({
      responseId: `js-gemini-sync-${context.turn}`,
      modelVersion: 'gemini-2.5-pro-001',
      candidates: [
        {
          finishReason: 'STOP',
          content: {
            role: 'model',
            parts: [{ text: `Launch ${context.turn}: all quality gates green; release metrics consistent.` }],
          },
        },
      ],
      usageMetadata: {
        promptTokenCount: 62 + (context.turn % 7),
        candidatesTokenCount: 20 + (context.turn % 5),
        totalTokenCount: 82 + (context.turn % 9),
        thoughtsTokenCount: 6,
      },
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitGeminiStream(sdk, client, context) {
  const model = 'gemini-2.5-pro';
  const contents = [
    { role: 'user', parts: [{ text: `Stream migration sequence ${context.turn} for canary rollout.` }] },
  ];
  const config = {
    systemInstruction: { role: 'user', parts: [{ text: 'Emit stream checkpoints as staged migration updates.' }] },
    thinkingConfig: { includeThoughts: true, thinkingBudget: 1536 },
  };

  await sdk.gemini.models.generateContentStream(
    client,
    model,
    contents,
    config,
    async () => ({
      outputText: `Wave ${context.turn}: shard sync complete; traffic shift finalized.`,
      finalResponse: {
        responseId: `js-gemini-stream-${context.turn}`,
        modelVersion: 'gemini-2.5-pro-001',
        candidates: [
          {
            finishReason: 'STOP',
            content: {
              role: 'model',
              parts: [{ text: `Wave ${context.turn}: shard sync complete; traffic shift finalized.` }],
            },
          },
        ],
        usageMetadata: {
          promptTokenCount: 47 + (context.turn % 5),
          candidatesTokenCount: 16 + (context.turn % 4),
          totalTokenCount: 63 + (context.turn % 7),
        },
      },
      responses: [
        {
          candidates: [
            {
              content: {
                role: 'model',
                parts: [{ text: `Wave ${context.turn}: shard sync complete; traffic shift finalized.` }],
              },
            },
          ],
        },
      ],
    }),
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      tags: context.tags,
      metadata: context.metadata,
    }
  );
}

async function emitCustomSync(client, cfg, context) {
  await client.startGeneration(
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      model: {
        provider: cfg.customProvider,
        name: 'mistral-large-devex',
      },
      tags: context.tags,
      metadata: context.metadata,
    },
    async (recorder) => {
      recorder.setResult({
        input: [{ role: 'user', content: `Draft custom provider checkpoint ${context.turn}.` }],
        output: [{ role: 'assistant', content: `Custom provider sync ${context.turn}: all guardrails satisfied.` }],
        usage: {
          inputTokens: 29 + (context.turn % 6),
          outputTokens: 15 + (context.turn % 5),
          totalTokens: 44 + (context.turn % 8),
        },
        stopReason: 'stop',
      });
    }
  );
}

async function emitCustomStream(client, cfg, context) {
  await client.startStreamingGeneration(
    {
      conversationId: context.conversationId,
      agentName: context.agentName,
      agentVersion: context.agentVersion,
      model: {
        provider: cfg.customProvider,
        name: 'mistral-large-devex',
      },
      tags: context.tags,
      metadata: context.metadata,
    },
    async (recorder) => {
      recorder.setResult({
        input: [{ role: 'user', content: `Stream custom remediation report ${context.turn}.` }],
        output: [
          {
            role: 'assistant',
            parts: [
              { type: 'thinking', thinking: 'assembling synthetic stream chunks' },
              { type: 'text', text: `Custom stream ${context.turn}: segment A complete; segment B complete.` },
            ],
          },
        ],
        usage: {
          inputTokens: 24 + (context.turn % 5),
          outputTokens: 17 + (context.turn % 4),
          totalTokens: 41 + (context.turn % 7),
        },
        stopReason: 'end_turn',
      });
    }
  );
}

export async function emitSource(sdk, client, cfg, source, mode, context) {
  if (source === 'openai') {
    const shape = providerShapeFor('openai', context.turn);
    const useResponses = shape === 'openai_responses';

    if (mode === 'STREAM') {
      if (useResponses) {
        await emitOpenAIResponsesStream(sdk, client, context);
        return;
      }
      await emitOpenAIStream(sdk, client, context);
      return;
    }
    if (useResponses) {
      await emitOpenAIResponsesSync(sdk, client, context);
      return;
    }
    await emitOpenAISync(sdk, client, context);
    return;
  }

  if (source === 'anthropic') {
    if (mode === 'STREAM') {
      await emitAnthropicStream(sdk, client, context);
      return;
    }
    await emitAnthropicSync(sdk, client, context);
    return;
  }

  if (source === 'gemini') {
    if (mode === 'STREAM') {
      await emitGeminiStream(sdk, client, context);
      return;
    }
    await emitGeminiSync(sdk, client, context);
    return;
  }

  if (mode === 'STREAM') {
    await emitCustomStream(client, cfg, context);
    return;
  }
  await emitCustomSync(client, cfg, context);
}

function frameworkModelForSource(source) {
  switch (source) {
    case 'openai':
      return 'gpt-5';
    case 'anthropic':
      return 'claude-sonnet-4-5';
    case 'gemini':
      return 'gemini-2.5-pro';
    default:
      return 'custom-framework-model';
  }
}

async function emitFrameworkHandler(HandlerClass, client, source, mode, context) {
  const model = frameworkModelForSource(source);
  const runID = `run-${source}-${context.turn}-${Math.trunc(Math.random() * 100_000)}`;
  const frameworkMetadata = {
    conversation_id: context.conversationId,
    thread_id: context.conversationId,
    event_id: `framework-event-${runID}`,
  };
  const handler = new HandlerClass(client, {
    providerResolver: 'auto',
    agentName: context.agentName,
    agentVersion: context.agentVersion,
    extraTags: context.tags,
    extraMetadata: context.metadata,
  });

  if (mode === 'STREAM') {
    await handler.handleLLMStart(
      { kwargs: { model } },
      [`stream framework status ${context.turn}`],
      runID,
      undefined,
      { invocation_params: { model, stream: true } },
      ['framework'],
      frameworkMetadata
    );
    await handler.handleLLMNewToken('framework ', undefined, runID);
    await handler.handleLLMNewToken(`${context.turn}`, undefined, runID);
    await handler.handleLLMEnd(
      {
        llm_output: {
          model_name: model,
          token_usage: {
            prompt_tokens: 12 + (context.turn % 4),
            completion_tokens: 6 + (context.turn % 3),
          },
        },
      },
      runID
    );
    return;
  }

  await handler.handleChatModelStart(
    { name: 'ChatModel' },
    [[{ type: 'human', content: `summarize framework path ${context.turn}` }]],
    runID,
    undefined,
    { invocation_params: { model, stream: false } },
    ['framework'],
    frameworkMetadata
  );
  await handler.handleLLMEnd(
    {
      generations: [[{ text: `framework output ${context.turn}` }]],
      llm_output: {
        model_name: model,
        finish_reason: 'stop',
        token_usage: {
          prompt_tokens: 12 + (context.turn % 4),
          completion_tokens: 6 + (context.turn % 3),
          total_tokens: 18 + (context.turn % 7),
        },
      },
    },
    runID
  );
}

export async function emitFrameworks(frameworks, client, source, mode, context) {
  if (source !== 'openai' && source !== 'anthropic' && source !== 'gemini') {
    return;
  }

  await emitFrameworkHandler(frameworks.langchain.Agento11yLangChainHandler, client, source, mode, context);
  await emitFrameworkHandler(frameworks.langgraph.Agento11yLangGraphHandler, client, source, mode, context);
  await emitFrameworkHandler(frameworks.openaiAgents.Agento11yOpenAIAgentsHandler, client, source, mode, context);
  await emitFrameworkHandler(frameworks.llamaindex.Agento11yLlamaIndexHandler, client, source, mode, context);
  await emitFrameworkHandler(frameworks.googleAdk.Agento11yGoogleAdkHandler, client, source, mode, context);
}

export async function runEmitter(config = loadConfig()) {
  const sdk = await loadSdkModule();
  const frameworks = await loadFrameworkModules();
  const client = new sdk.Agento11yClient({
    generationExport: {
      protocol: 'grpc',
      endpoint: config.genGrpcEndpoint,
      auth: { mode: 'none' },
      insecure: true,
    },
  });

  const sourceState = new Map(SOURCES.map((source) => [source, createSourceState(config.conversations)]));
  let cycles = 0;
  let stopping = false;

  const stop = () => {
    stopping = true;
  };

  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);

  console.log(
    `[js-emitter] started interval_ms=${config.intervalMs} stream_percent=${config.streamPercent} conversations=${config.conversations} rotate_turns=${config.rotateTurns} custom_provider=${config.customProvider}`
  );

  try {
    while (!stopping) {
      for (const source of SOURCES) {
        const state = sourceState.get(source);
        const slot = state.cursor % config.conversations;
        state.cursor += 1;

        const thread = resolveThread(state, config.rotateTurns, source, slot);
        const mode = chooseMode(Math.floor(Math.random() * 100), config.streamPercent);
        const context = buildTagsAndMetadata(source, mode, thread.turn, slot);

        const agentName = `devex-${LANGUAGE}-${source}-${context.agentPersona}`;
        const agentVersion = 'devex-1';

        await emitSource(sdk, client, config, source, mode, {
          ...context,
          conversationId: thread.conversationId,
          turn: thread.turn,
          slot,
          agentName,
          agentVersion,
        });
        await emitFrameworks(frameworks, client, source, mode, {
          ...context,
          conversationId: thread.conversationId,
          turn: thread.turn,
          slot,
          agentName,
          agentVersion,
        });

        thread.turn += 1;
      }

      cycles += 1;
      if (config.maxCycles > 0 && cycles >= config.maxCycles) {
        break;
      }

      const jitterMs = Math.floor(Math.random() * 401) - 200;
      const sleepMs = Math.max(200, config.intervalMs + jitterMs);
      await sleep(sleepMs);
    }
  } finally {
    await client.shutdown();
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  runEmitter().catch((error) => {
    console.error('[js-emitter] fatal error', error);
    process.exit(1);
  });
}
