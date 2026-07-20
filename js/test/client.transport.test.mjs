import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { trace } from '@opentelemetry/api';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const protoPath = join(__dirname, '../proto/agento11y/v1/generation_ingest.proto');
const protoLoadOptions = {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: false,
  oneofs: true,
};

test('HTTP transport roundtrip preserves full generation payload shape', async () => {
  const receivedGenerations = [];

  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }

    const payload = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    for (const generation of payload.generations ?? []) {
      receivedGenerations.push(canonicalizeProtoJSONGeneration(generation));
    }

    response.writeHead(202, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        results: (payload.generations ?? []).map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      }),
    );
  });

  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve transport test server address');
  }

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  const totalSeeds = 20;
  try {
    for (let seed = 1; seed <= totalSeeds; seed++) {
      const { start, result } = payloadFromSeed(seed);
      const recorder = start.mode === 'STREAM' ? client.startStreamingGeneration(start) : client.startGeneration(start);
      recorder.setResult(result);
      if (seed % 3 === 0) {
        recorder.setCallError(new Error(`provider_error_${seed}`));
      }
      recorder.end();

      assert.equal(recorder.getError(), undefined);
    }

    await waitFor(() => receivedGenerations.length === totalSeeds, 2_000);

    const expectedGenerations = client.debugSnapshot().generations.map(canonicalizeSDKGeneration);
    assert.deepEqual(receivedGenerations, expectedGenerations);
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('gRPC transport roundtrip preserves full generation payload shape', async () => {
  const receivedGenerations = [];
  const grpcServer = await startGRPCServer((request) => {
    for (const generation of request.generations ?? []) {
      receivedGenerations.push(generation);
    }
  });

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const seedStart = 11;
    const seedEnd = 30;
    for (let seed = seedStart; seed <= seedEnd; seed++) {
      const { start, result } = payloadFromSeed(seed);
      const recorder = start.mode === 'STREAM' ? client.startStreamingGeneration(start) : client.startGeneration(start);
      recorder.setResult(result);
      if (seed % 3 === 0) {
        recorder.setCallError(new Error(`provider_error_${seed}`));
      }
      recorder.end();
      assert.equal(recorder.getError(), undefined);
    }

    const totalSeeds = seedEnd - seedStart + 1;
    await waitFor(() => receivedGenerations.length === totalSeeds, 2_000);

    const expectedGenerations = client.debugSnapshot().generations.map(canonicalizeSDKGeneration);
    const actualGenerations = receivedGenerations.map(canonicalizeProtoGeneration);
    assert.deepEqual(actualGenerations, expectedGenerations);
  } finally {
    await client.shutdown();
    await stopGRPCServer(grpcServer.server);
  }
});

test('HTTP transport roundtrip exports workflow steps to sibling endpoint', async () => {
  const receivedWorkflowSteps = [];
  const receivedPaths = [];

  const server = createServer(async (request, response) => {
    receivedPaths.push(request.url);
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }

    const payload = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    for (const step of payload.workflow_steps ?? []) {
      receivedWorkflowSteps.push(step);
    }

    response.writeHead(202, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        results: (payload.workflow_steps ?? []).map((step) => ({
          step_id: step.id,
          accepted: true,
        })),
      }),
    );
  });

  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve transport test server address');
  }

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const step = workflowStepFromSeed(1);
    client.enqueueWorkflowStep(step);
    await waitFor(() => receivedWorkflowSteps.length === 1, 2_000);

    assert.equal(receivedPaths[0], '/api/v1/workflow-steps:export');
    assert.deepEqual(canonicalizeProtoJSONWorkflowStep(receivedWorkflowSteps[0]), canonicalizeSDKWorkflowStep(step));
  } finally {
    await client.shutdown();
    await close(server);
  }
});

test('gRPC transport roundtrip exports workflow steps through workflow service', async () => {
  const receivedWorkflowSteps = [];
  const grpcServer = await startGRPCServer(
    () => {},
    (request) => {
      for (const step of request.workflowSteps ?? []) {
        receivedWorkflowSteps.push(step);
      }
    },
  );

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const step = workflowStepFromSeed(2);
    client.enqueueWorkflowStep(step);
    await waitFor(() => receivedWorkflowSteps.length === 1, 2_000);

    assert.deepEqual(canonicalizeProtoWorkflowStep(receivedWorkflowSteps[0]), canonicalizeSDKWorkflowStep(step));
  } finally {
    await client.shutdown();
    await stopGRPCServer(grpcServer.server);
  }
});

test('gRPC transport maps typed message parts to proto payloads', async () => {
  const receivedGenerations = [];
  const grpcServer = await startGRPCServer((request) => {
    for (const generation of request.generations ?? []) {
      receivedGenerations.push(generation);
    }
  });

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const recorder = client.startGeneration({
      id: 'gen-parts',
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      input: [
        {
          role: 'assistant',
          parts: [
            {
              type: 'thinking',
              thinking: 'deliberation',
              metadata: { providerType: 'reasoning' },
            },
            {
              type: 'tool_call',
              toolCall: {
                id: 'tool-call-1',
                name: 'weather',
                inputJSON: '{"city":"paris"}',
              },
              metadata: { providerType: 'tool_call' },
            },
          ],
        },
      ],
      output: [
        {
          role: 'tool',
          parts: [
            {
              type: 'tool_result',
              toolResult: {
                toolCallId: 'tool-call-1',
                name: 'weather',
                content: 'sunny',
                contentJSON: '{"temp_c":22}',
                isError: false,
              },
              metadata: { providerType: 'tool_result' },
            },
          ],
        },
      ],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    await waitFor(() => receivedGenerations.length === 1, 2_000);
    const generation = receivedGenerations[0];
    const inputParts = generation.input[0].parts;
    const outputParts = generation.output[0].parts;

    assert.equal(inputParts[0].thinking, 'deliberation');
    assert.equal(inputParts[0].metadata.providerType, 'reasoning');

    assert.equal(inputParts[1].toolCall.id, 'tool-call-1');
    assert.equal(inputParts[1].toolCall.name, 'weather');
    assert.equal(asUTF8String(inputParts[1].toolCall.inputJson), '{"city":"paris"}');
    assert.equal(inputParts[1].metadata.providerType, 'tool_call');

    assert.equal(outputParts[0].toolResult.toolCallId, 'tool-call-1');
    assert.equal(outputParts[0].toolResult.name, 'weather');
    assert.equal(outputParts[0].toolResult.content, 'sunny');
    assert.equal(asUTF8String(outputParts[0].toolResult.contentJson), '{"temp_c":22}');
    assert.equal(outputParts[0].toolResult.isError, false);
    assert.equal(outputParts[0].metadata.providerType, 'tool_result');
  } finally {
    await client.shutdown();
    await stopGRPCServer(grpcServer.server);
  }
});

test('gRPC transport omits maxTokens when it is unset', async () => {
  const receivedGenerations = [];
  const grpcServer = await startGRPCServer((request) => {
    for (const generation of request.generations ?? []) {
      receivedGenerations.push(generation);
    }
  });

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const recorder = client.startGeneration({
      id: 'gen-no-max-tokens',
      model: { provider: 'openai', name: 'gpt-5' },
    });
    recorder.setResult({
      output: [{ role: 'assistant', content: 'ok' }],
    });
    recorder.end();
    assert.equal(recorder.getError(), undefined);

    await waitFor(() => receivedGenerations.length === 1, 2_000);
    assert.equal(receivedGenerations[0].maxTokens, undefined);
  } finally {
    await client.shutdown();
    await stopGRPCServer(grpcServer.server);
  }
});

test('HTTP transport applies generation tenant auth header', async () => {
  const receivedHeaders = [];
  const server = createServer(async (request, response) => {
    receivedHeaders.push(Object.fromEntries(Object.entries(request.headers)));
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    const payload = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    response.writeHead(202, { 'content-type': 'application/json' });
    response.end(
      JSON.stringify({
        results: (payload.generations ?? []).map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      }),
    );
  });

  await listen(server);
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('failed to resolve transport test server address');
  }

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'http',
      endpoint: `http://127.0.0.1:${address.port}/api/v1/generations:export`,
      auth: {
        mode: 'tenant',
        tenantId: 'tenant-a',
      },
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const { start, result } = payloadFromSeed(99);
    const recorder = client.startGeneration(start);
    recorder.setResult(result);
    recorder.end();
    assert.equal(recorder.getError(), undefined);
    await client.shutdown();

    assert.equal(receivedHeaders.length, 1);
    assert.equal(receivedHeaders[0]['x-scope-orgid'], 'tenant-a');
  } finally {
    await close(server);
  }
});

test('gRPC transport applies generation bearer auth metadata with explicit header override', async () => {
  const receivedMetadata = [];
  const grpcServer = await startGRPCServer((_request, metadata) => {
    receivedMetadata.push(metadata);
  });

  const defaults = defaultConfig();
  const client = new Agento11yClient({
    tracer: trace.getTracer('agento11y-sdk-js-test'),
    generationExport: {
      ...defaults.generationExport,
      protocol: 'grpc',
      endpoint: `127.0.0.1:${grpcServer.port}`,
      insecure: true,
      headers: {
        authorization: 'Bearer override-token',
      },
      auth: {
        mode: 'bearer',
        bearerToken: 'sdk-token',
      },
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxRetries: 1,
      initialBackoffMs: 1,
      maxBackoffMs: 1,
    },
  });

  try {
    const { start, result } = payloadFromSeed(101);
    const recorder = client.startGeneration(start);
    recorder.setResult(result);
    recorder.end();
    assert.equal(recorder.getError(), undefined);
    await client.shutdown();

    assert.equal(receivedMetadata.length, 1);
    assert.equal(receivedMetadata[0].authorization, 'Bearer override-token');
  } finally {
    await stopGRPCServer(grpcServer.server);
  }
});

function payloadFromSeed(seed) {
  const startedAt = new Date(Date.UTC(2026, 1, 12, 10, seed, 0));
  const completedAt = new Date(startedAt.getTime() + 250);
  const mode = seed % 2 === 0 ? 'STREAM' : 'SYNC';

  return {
    start: {
      id: `gen-${seed}`,
      conversationId: `conv-${seed}`,
      agentName: `agent-${seed}`,
      agentVersion: `v-${seed}`,
      mode,
      operationName: mode === 'STREAM' ? 'streamText' : 'generateText',
      model: {
        provider: 'openai',
        name: `gpt-5-${seed}`,
      },
      systemPrompt: `system-${seed}`,
      maxTokens: 1000 + seed,
      temperature: 0.7,
      topP: 0.9,
      toolChoice: 'auto',
      thinkingEnabled: true,
      tools: [
        {
          name: `tool-${seed}`,
          description: `description-${seed}`,
          type: 'function',
          inputSchemaJSON: JSON.stringify({
            type: 'object',
            properties: {
              seed: { type: 'number' },
            },
          }),
        },
      ],
      tags: {
        env: 'test',
        seed: String(seed),
      },
      metadata: {
        seed,
        nested: {
          seedSquared: seed * seed,
        },
      },
      startedAt,
    },
    result: {
      responseId: `resp-${seed}`,
      responseModel: `gpt-5-${seed}`,
      maxTokens: 200 + seed,
      temperature: 0.2,
      topP: 0.85,
      toolChoice: 'required',
      thinkingEnabled: seed % 2 === 0,
      input: [
        {
          role: 'user',
          content: `hello-${seed}`,
          name: 'user',
        },
      ],
      output: [
        {
          role: 'assistant',
          content: `world-${seed}`,
          name: 'assistant',
        },
      ],
      tools: [
        {
          name: `tool-${seed}`,
          description: `description-${seed}`,
          type: 'function',
          inputSchemaJSON: JSON.stringify({
            type: 'object',
            properties: {
              seed: { type: 'number' },
            },
          }),
        },
      ],
      usage: {
        inputTokens: seed * 10,
        outputTokens: seed * 20,
        totalTokens: seed * 30,
        cacheReadInputTokens: seed,
        cacheWriteInputTokens: seed + 1,
        reasoningTokens: seed + 2,
      },
      stopReason: 'stop',
      completedAt,
      tags: {
        stage: 'transport',
        seed: String(seed),
      },
      metadata: {
        source: 'transport-test',
        seed,
        nested: {
          seedPlusOne: seed + 1,
        },
      },
      artifacts: [
        {
          type: 'request',
          name: 'provider.request',
          payload: `payload-${seed}`,
          mimeType: 'application/json',
          recordId: `record-${seed}`,
          uri: `sigil://artifact/${seed}`,
        },
      ],
    },
  };
}

function workflowStepFromSeed(seed) {
  return {
    id: `wfs-${seed}`,
    conversationId: `conv-${seed}`,
    stepName: `step-${seed}`,
    framework: 'custom',
    startedAt: new Date(Date.UTC(2026, 1, 12, 11, seed, 0)),
    completedAt: new Date(Date.UTC(2026, 1, 12, 11, seed, 1)),
    inputState: {
      input: `input-${seed}`,
      nested: { seed },
    },
    outputState: {
      output: `output-${seed}`,
    },
    error: seed % 2 === 0 ? `error-${seed}` : undefined,
    tags: {
      env: 'test',
      seed: String(seed),
    },
    linkedGenerationIds: [`gen-${seed}`],
    parentStepIds: seed > 1 ? [`wfs-${seed - 1}`] : [],
    agentName: `agent-${seed}`,
    agentVersion: `v-${seed}`,
    traceId: `trace-${seed}`,
    spanId: `span-${seed}`,
    metadata: {
      source: 'transport-test',
      seed,
    },
  };
}

function canonicalizeSDKGeneration(generation) {
  const usage = generation.usage ?? {};
  const inputTokens = asNumber(usage.inputTokens);
  const outputTokens = asNumber(usage.outputTokens);
  return {
    id: generation.id,
    conversationId: generation.conversationId ?? '',
    agentName: generation.agentName ?? '',
    agentVersion: generation.agentVersion ?? '',
    mode: generation.mode,
    operationName: generation.operationName,
    traceId: generation.traceId ?? '',
    spanId: generation.spanId ?? '',
    model: {
      provider: generation.model.provider,
      name: generation.model.name,
    },
    responseId: generation.responseId ?? '',
    responseModel: generation.responseModel ?? '',
    systemPrompt: generation.systemPrompt ?? '',
    maxTokens: asNumber(generation.maxTokens),
    temperature: asNumber(generation.temperature),
    topP: asNumber(generation.topP),
    toolChoice: generation.toolChoice ?? '',
    thinkingEnabled: generation.thinkingEnabled ?? false,
    input: (generation.input ?? []).map(canonicalizeSDKMessage),
    output: (generation.output ?? []).map(canonicalizeSDKMessage),
    tools: (generation.tools ?? []).map((tool) => ({
      name: tool.name,
      description: tool.description ?? '',
      type: tool.type ?? '',
      inputSchemaJSON: tool.inputSchemaJSON ?? '',
    })),
    usage: {
      inputTokens,
      outputTokens,
      totalTokens: usage.totalTokens !== undefined ? asNumber(usage.totalTokens) : inputTokens + outputTokens,
      cacheReadInputTokens: asNumber(usage.cacheReadInputTokens),
      cacheWriteInputTokens: asNumber(usage.cacheWriteInputTokens),
      reasoningTokens: asNumber(usage.reasoningTokens),
    },
    stopReason: generation.stopReason ?? '',
    startedAt: new Date(generation.startedAt).toISOString(),
    completedAt: new Date(generation.completedAt).toISOString(),
    tags: generation.tags ?? {},
    metadata: generation.metadata ?? {},
    artifacts: (generation.artifacts ?? []).map((artifact) => ({
      type: artifact.type,
      name: artifact.name ?? artifact.type,
      payload: artifact.payload ?? '',
      mimeType: artifact.mimeType ?? 'application/json',
      recordId: artifact.recordId ?? '',
      uri: artifact.uri ?? '',
    })),
    callError: generation.callError ?? '',
  };
}

function canonicalizeProtoJSONGeneration(generation) {
  if (!isRecord(generation)) {
    throw new Error('invalid proto-json generation payload');
  }

  const usage = isRecord(generation.usage) ? generation.usage : {};
  return {
    id: asString(generation.id),
    conversationId: asString(generation.conversation_id),
    agentName: asString(generation.agent_name),
    agentVersion: asString(generation.agent_version),
    mode: fromProtoJSONGenerationMode(generation.mode),
    operationName: asString(generation.operation_name),
    traceId: asString(generation.trace_id),
    spanId: asString(generation.span_id),
    model: {
      provider: isRecord(generation.model) ? asString(generation.model.provider) : '',
      name: isRecord(generation.model) ? asString(generation.model.name) : '',
    },
    responseId: asString(generation.response_id),
    responseModel: asString(generation.response_model),
    systemPrompt: asString(generation.system_prompt),
    maxTokens: asNumber(generation.max_tokens),
    temperature: asNumber(generation.temperature),
    topP: asNumber(generation.top_p),
    toolChoice: asString(generation.tool_choice),
    thinkingEnabled: Boolean(generation.thinking_enabled),
    input: (Array.isArray(generation.input) ? generation.input : []).map(canonicalizeProtoJSONMessage),
    output: (Array.isArray(generation.output) ? generation.output : []).map(canonicalizeProtoJSONMessage),
    tools: (Array.isArray(generation.tools) ? generation.tools : []).map((tool) => ({
      name: isRecord(tool) ? asString(tool.name) : '',
      description: isRecord(tool) ? asString(tool.description) : '',
      type: isRecord(tool) ? asString(tool.type) : '',
      inputSchemaJSON: isRecord(tool) ? decodeBase64(asString(tool.input_schema_json)) : '',
    })),
    usage: {
      inputTokens: asNumber(usage.input_tokens),
      outputTokens: asNumber(usage.output_tokens),
      totalTokens: asNumber(usage.total_tokens),
      cacheReadInputTokens: asNumber(usage.cache_read_input_tokens),
      cacheWriteInputTokens: asNumber(usage.cache_write_input_tokens),
      reasoningTokens: asNumber(usage.reasoning_tokens),
    },
    stopReason: asString(generation.stop_reason),
    startedAt: timestampStringToISO(generation.started_at),
    completedAt: timestampStringToISO(generation.completed_at),
    tags: normalizeProtoJSONStringMap(generation.tags),
    metadata: normalizeProtoJSONMetadata(generation.metadata),
    artifacts: (Array.isArray(generation.raw_artifacts) ? generation.raw_artifacts : []).map((artifact) => ({
      type: isRecord(artifact) ? fromProtoArtifactKind(artifact.kind) : 'unknown',
      name: isRecord(artifact) ? asString(artifact.name) : '',
      payload: isRecord(artifact) ? decodeBase64(asString(artifact.payload)) : '',
      mimeType: isRecord(artifact) ? asString(artifact.content_type) : 'application/json',
      recordId: isRecord(artifact) ? asString(artifact.record_id) : '',
      uri: isRecord(artifact) ? asString(artifact.uri) : '',
    })),
    callError: asString(generation.call_error),
  };
}

function canonicalizeProtoJSONMessage(message) {
  if (!isRecord(message)) {
    return {
      role: 'user',
      name: '',
      content: '',
    };
  }

  return {
    role: fromProtoJSONMessageRole(message.role),
    name: asString(message.name),
    content: (Array.isArray(message.parts) ? message.parts : [])
      .map((part) => (isRecord(part) && typeof part.text === 'string' ? part.text : ''))
      .join(''),
  };
}

function canonicalizeSDKMessage(message) {
  return {
    role: normalizeSDKRole(message.role),
    name: message.name ?? '',
    content: message.content,
  };
}

function canonicalizeProtoGeneration(generation) {
  const usage = generation.usage ?? {};
  return {
    id: generation.id ?? '',
    conversationId: generation.conversationId ?? '',
    agentName: generation.agentName ?? '',
    agentVersion: generation.agentVersion ?? '',
    mode: fromProtoGenerationMode(generation.mode),
    operationName: generation.operationName ?? '',
    traceId: generation.traceId ?? '',
    spanId: generation.spanId ?? '',
    model: {
      provider: generation.model?.provider ?? '',
      name: generation.model?.name ?? '',
    },
    responseId: generation.responseId ?? '',
    responseModel: generation.responseModel ?? '',
    systemPrompt: generation.systemPrompt ?? '',
    maxTokens: asNumber(generation.maxTokens),
    temperature: asNumber(generation.temperature),
    topP: asNumber(generation.topP),
    toolChoice: generation.toolChoice ?? '',
    thinkingEnabled: Boolean(generation.thinkingEnabled),
    input: (generation.input ?? []).map(canonicalizeProtoMessage),
    output: (generation.output ?? []).map(canonicalizeProtoMessage),
    tools: (generation.tools ?? []).map((tool) => ({
      name: tool.name ?? '',
      description: tool.description ?? '',
      type: tool.type ?? '',
      inputSchemaJSON: asUTF8String(tool.inputSchemaJson),
    })),
    usage: {
      inputTokens: asNumber(usage.inputTokens),
      outputTokens: asNumber(usage.outputTokens),
      totalTokens: asNumber(usage.totalTokens),
      cacheReadInputTokens: asNumber(usage.cacheReadInputTokens),
      cacheWriteInputTokens: asNumber(usage.cacheWriteInputTokens),
      reasoningTokens: asNumber(usage.reasoningTokens),
    },
    stopReason: generation.stopReason ?? '',
    startedAt: timestampToISO(generation.startedAt),
    completedAt: timestampToISO(generation.completedAt),
    tags: generation.tags ?? {},
    metadata: normalizeProtoMetadata(generation.metadata),
    artifacts: (generation.rawArtifacts ?? []).map((artifact) => ({
      type: fromProtoArtifactKind(artifact.kind),
      name: artifact.name ?? '',
      payload: asUTF8String(artifact.payload),
      mimeType: artifact.contentType ?? 'application/json',
      recordId: artifact.recordId ?? '',
      uri: artifact.uri ?? '',
    })),
    callError: generation.callError ?? '',
  };
}

function canonicalizeSDKWorkflowStep(step) {
  return {
    id: step.id,
    conversationId: step.conversationId,
    stepName: step.stepName,
    framework: step.framework ?? '',
    startedAt: new Date(step.startedAt).toISOString(),
    completedAt: new Date(step.completedAt).toISOString(),
    inputState: step.inputState ?? {},
    outputState: step.outputState ?? {},
    error: step.error ?? '',
    tags: step.tags ?? {},
    linkedGenerationIds: step.linkedGenerationIds ?? [],
    parentStepIds: step.parentStepIds ?? [],
    agentName: step.agentName ?? '',
    agentVersion: step.agentVersion ?? '',
    traceId: step.traceId ?? '',
    spanId: step.spanId ?? '',
    metadata: step.metadata ?? {},
  };
}

function canonicalizeProtoJSONWorkflowStep(step) {
  if (!isRecord(step)) {
    throw new Error('invalid proto-json workflow step payload');
  }
  return {
    id: asString(step.id),
    conversationId: asString(step.conversation_id),
    stepName: asString(step.step_name),
    framework: asString(step.framework),
    startedAt: timestampStringToISO(step.started_at),
    completedAt: timestampStringToISO(step.completed_at),
    inputState: normalizeProtoJSONMetadata(step.input_state),
    outputState: normalizeProtoJSONMetadata(step.output_state),
    error: asString(step.error),
    tags: normalizeProtoJSONStringMap(step.tags),
    linkedGenerationIds: Array.isArray(step.linked_generation_ids) ? step.linked_generation_ids.map(asString) : [],
    parentStepIds: Array.isArray(step.parent_step_ids) ? step.parent_step_ids.map(asString) : [],
    agentName: asString(step.agent_name),
    agentVersion: asString(step.agent_version),
    traceId: asString(step.trace_id),
    spanId: asString(step.span_id),
    metadata: normalizeProtoJSONMetadata(step.metadata),
  };
}

function canonicalizeProtoWorkflowStep(step) {
  return {
    id: step.id ?? '',
    conversationId: step.conversationId ?? '',
    stepName: step.stepName ?? '',
    framework: step.framework ?? '',
    startedAt: timestampToISO(step.startedAt),
    completedAt: timestampToISO(step.completedAt),
    inputState: normalizeProtoMetadata(step.inputState),
    outputState: normalizeProtoMetadata(step.outputState),
    error: step.error ?? '',
    tags: step.tags ?? {},
    linkedGenerationIds: step.linkedGenerationIds ?? [],
    parentStepIds: step.parentStepIds ?? [],
    agentName: step.agentName ?? '',
    agentVersion: step.agentVersion ?? '',
    traceId: step.traceId ?? '',
    spanId: step.spanId ?? '',
    metadata: normalizeProtoMetadata(step.metadata),
  };
}

function canonicalizeProtoMessage(message) {
  return {
    role: fromProtoMessageRole(message.role),
    name: message.name ?? '',
    content: (message.parts ?? []).map((part) => (typeof part.text === 'string' ? part.text : '')).join(''),
  };
}

function normalizeSDKRole(role) {
  const normalized = String(role ?? '')
    .trim()
    .toLowerCase();
  if (normalized === 'assistant' || normalized === 'tool') {
    return normalized;
  }
  return 'user';
}

function fromProtoGenerationMode(mode) {
  if (mode === 'GENERATION_MODE_STREAM') {
    return 'STREAM';
  }
  return 'SYNC';
}

function fromProtoJSONGenerationMode(mode) {
  if (mode === 'GENERATION_MODE_STREAM') {
    return 'STREAM';
  }
  if (mode === 'GENERATION_MODE_SYNC' || mode === undefined || mode === null || mode === '') {
    return 'SYNC';
  }
  throw new Error(`unexpected proto-json generation mode: ${String(mode)}`);
}

function fromProtoJSONMessageRole(role) {
  switch (role) {
    case 'MESSAGE_ROLE_ASSISTANT':
      return 'assistant';
    case 'MESSAGE_ROLE_TOOL':
      return 'tool';
    case 'MESSAGE_ROLE_USER':
    case undefined:
    case null:
    case '':
      return 'user';
    default:
      throw new Error(`unexpected proto-json message role: ${String(role)}`);
  }
}

function fromProtoMessageRole(role) {
  switch (role) {
    case 'MESSAGE_ROLE_ASSISTANT':
      return 'assistant';
    case 'MESSAGE_ROLE_TOOL':
      return 'tool';
    case 'MESSAGE_ROLE_USER':
    default:
      return 'user';
  }
}

function fromProtoArtifactKind(kind) {
  switch (kind) {
    case 'ARTIFACT_KIND_REQUEST':
      return 'request';
    case 'ARTIFACT_KIND_RESPONSE':
      return 'response';
    case 'ARTIFACT_KIND_TOOLS':
      return 'tools';
    case 'ARTIFACT_KIND_PROVIDER_EVENT':
      return 'provider_event';
    default:
      return 'unknown';
  }
}

function normalizeProtoMetadata(metadata) {
  if (metadata === undefined || metadata === null) {
    return {};
  }

  if (isRecord(metadata) && isRecord(metadata.fields)) {
    return decodeStructFields(metadata.fields);
  }
  if (isRecord(metadata)) {
    return metadata;
  }
  return {};
}

function normalizeProtoJSONMetadata(metadata) {
  if (!isRecord(metadata)) {
    return {};
  }
  return metadata;
}

function normalizeProtoJSONStringMap(value) {
  if (!isRecord(value)) {
    return {};
  }
  const out = {};
  for (const [key, entry] of Object.entries(value)) {
    out[key] = asString(entry);
  }
  return out;
}

function decodeStructFields(fields) {
  const decoded = {};
  for (const [key, value] of Object.entries(fields)) {
    decoded[key] = decodeStructValue(value);
  }
  return decoded;
}

function decodeStructValue(value) {
  if (!isRecord(value)) {
    return null;
  }
  if ('stringValue' in value) {
    return value.stringValue;
  }
  if ('numberValue' in value) {
    return asNumber(value.numberValue);
  }
  if ('boolValue' in value) {
    return Boolean(value.boolValue);
  }
  if ('nullValue' in value) {
    return null;
  }
  if ('structValue' in value && isRecord(value.structValue) && isRecord(value.structValue.fields)) {
    return decodeStructFields(value.structValue.fields);
  }
  if ('listValue' in value && isRecord(value.listValue) && Array.isArray(value.listValue.values)) {
    return value.listValue.values.map((entry) => decodeStructValue(entry));
  }
  return null;
}

function timestampToISO(value) {
  if (!isRecord(value)) {
    return new Date(0).toISOString();
  }
  const seconds = asNumber(value.seconds);
  const nanos = asNumber(value.nanos);
  const milliseconds = seconds * 1_000 + Math.floor(nanos / 1_000_000);
  return new Date(milliseconds).toISOString();
}

function asUTF8String(value) {
  if (Buffer.isBuffer(value)) {
    return value.toString('utf8');
  }
  if (isRecord(value) && value.type === 'Buffer' && Array.isArray(value.data)) {
    return Buffer.from(value.data).toString('utf8');
  }
  if (typeof value === 'string') {
    return value;
  }
  return '';
}

function decodeBase64(value) {
  if (typeof value !== 'string' || value.length === 0) {
    return '';
  }
  return Buffer.from(value, 'base64').toString('utf8');
}

function timestampStringToISO(value) {
  if (typeof value !== 'string') {
    return new Date(0).toISOString();
  }
  const timestamp = new Date(value);
  if (Number.isNaN(timestamp.getTime())) {
    return new Date(0).toISOString();
  }
  return timestamp.toISOString();
}

function asString(value) {
  return typeof value === 'string' ? value : '';
}

function asNumber(value) {
  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : 0;
  }
  if (typeof value === 'string') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

function isRecord(value) {
  return typeof value === 'object' && value !== null;
}

function waitFor(condition, timeoutMs = 1_000) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (condition()) {
        resolve();
        return;
      }
      if (Date.now() >= deadline) {
        reject(new Error('timed out waiting for condition'));
        return;
      }
      setTimeout(tick, 5);
    };
    tick();
  });
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject);
      resolve();
    });
  });
}

function close(server) {
  return new Promise((resolve, reject) => {
    server.close((error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve();
    });
  });
}

async function startGRPCServer(onRequest, onWorkflowStepRequest = () => {}) {
  const packageDefinition = await protoLoader.load(protoPath, protoLoadOptions);
  const loaded = grpc.loadPackageDefinition(packageDefinition);
  const generationService = loaded.agento11y.v1.GenerationIngestService;
  const workflowStepService = loaded.agento11y.v1.WorkflowStepIngestService;

  const server = new grpc.Server();
  server.addService(generationService.service, {
    ExportGenerations(call, callback) {
      onRequest(call.request, call.metadata.getMap());
      callback(null, {
        results: (call.request.generations ?? []).map((generation) => ({
          generationId: generation.id,
          accepted: true,
        })),
      });
    },
  });
  server.addService(workflowStepService.service, {
    ExportWorkflowSteps(call, callback) {
      onWorkflowStepRequest(call.request, call.metadata.getMap());
      callback(null, {
        results: (call.request.workflowSteps ?? []).map((step) => ({
          stepId: step.id,
          accepted: true,
        })),
      });
    },
  });

  const port = await new Promise((resolve, reject) => {
    server.bindAsync('127.0.0.1:0', grpc.ServerCredentials.createInsecure(), (error, boundPort) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(boundPort);
    });
  });

  return { server, port };
}

function stopGRPCServer(server) {
  return new Promise((resolve) => {
    server.tryShutdown(() => {
      resolve();
    });
  });
}
