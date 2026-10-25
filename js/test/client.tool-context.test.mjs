import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import test from 'node:test';
import { context, createContextKey, propagation, ROOT_CONTEXT, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import {
  BasicTracerProvider,
  InMemorySpanExporter,
  SamplingDecision,
  SimpleSpanProcessor,
} from '@opentelemetry/sdk-trace-base';
import { Agento11yClient } from '../.test-dist/index.js';

// Use the trace SDK's own suppression helpers without adding a direct dependency.
const require = createRequire(import.meta.url);
const { isTracingSuppressed, suppressTracing } = createRequire(require.resolve('@opentelemetry/sdk-trace-base'))(
  '@opentelemetry/core',
);
const contextManager = new AsyncLocalStorageContextManager();
const samplerInput = createContextKey('tool-sampler-input');
const staleInput = createContextKey('generation-only-input');
const generationStart = { model: { provider: 'openai', name: 'gpt-5' } };

// The application, not Agento11yClient, owns async context propagation.
test.before(() => {
  contextManager.enable();
  assert.equal(context.setGlobalContextManager(contextManager), true);
});
test.after(() => {
  context.disable();
  contextManager.disable();
});

for (const method of ['startGeneration', 'startStreamingGeneration']) {
  test(`${method}: bound tools honor call-time suppression across await`, async () => {
    const harness = newHarness();
    try {
      const generation = context.with(ROOT_CONTEXT, () => harness.client[method](generationStart));
      assert.equal(harness.starts[0].span.isRecording(), true);
      await context.with(suppressTracing(ROOT_CONTEXT), async () => {
        await Promise.resolve();
        assert.equal(isTracingSuppressed(context.active()), true);
        const tool = generation.startToolExecution({ toolName: 'suppressed' });
        tool.end();
        assert.equal(tool.getError(), undefined);
      });
      generation.end();

      const toolStart = harness.starts[1];
      assert.equal(isTracingSuppressed(toolStart.parentContext), true);
      assert.equal(toolStart.span.spanContext().traceId, '0'.repeat(32));
      assert.equal(toolSpans(harness).length, 0);
      assert.equal(harness.spanExporter.getFinishedSpans().length, 1);
      assert.equal(harness.client.debugSnapshot().toolExecutions.length, 1);
    } finally {
      await shutdownHarness(harness);
    }
  });

  test(`${method}: generation-time suppression does not suppress a later bound tool`, async () => {
    const harness = newHarness();
    try {
      const generation = await context.with(suppressTracing(ROOT_CONTEXT), async () => {
        await Promise.resolve();
        return harness.client[method](generationStart);
      });
      await context.with(ROOT_CONTEXT, async () => {
        await Promise.resolve();
        assert.equal(isTracingSuppressed(context.active()), false);
        const tool = generation.startToolExecution({ toolName: 'unsuppressed' });
        tool.end();
        assert.equal(tool.getError(), undefined);
      });
      generation.end();

      assert.equal(harness.starts[0].span.spanContext().traceId, '0'.repeat(32));
      assert.equal(isTracingSuppressed(harness.starts[1].parentContext), false);
      const [tool] = toolSpans(harness);
      assert.ok(tool);
      assert.equal(tool.parentSpanContext, undefined);
      assert.equal(harness.spanExporter.getFinishedSpans().length, 1);
    } finally {
      await shutdownHarness(harness);
    }
  });

  test(`${method}: binding replaces only the parent span, not call-time baggage or context values`, async () => {
    const harness = newHarness();
    try {
      const oldContext = propagation
        .setBaggage(ROOT_CONTEXT, propagation.createBaggage({ tenant: { value: 'old' }, stale: { value: 'old' } }))
        .setValue(samplerInput, 'old')
        .setValue(staleInput, 'old');
      const generation = await context.with(oldContext, async () => {
        await Promise.resolve();
        return harness.client[method](generationStart);
      });
      const unrelated = trace.wrapSpanContext({
        traceId: '1'.repeat(32),
        spanId: '2'.repeat(16),
        traceFlags: 1,
      });
      const baggage = propagation.createBaggage({ tenant: { value: 'new' }, added: { value: 'new' } });
      const callContext = trace
        .setSpan(propagation.setBaggage(ROOT_CONTEXT, baggage), unrelated)
        .setValue(samplerInput, 'new');
      await context.with(callContext, async () => {
        await Promise.resolve();
        const tool = generation.startToolExecution({ toolName: 'baggage' });
        const parentContext = harness.starts[1].parentContext;
        assert.equal(propagation.getBaggage(parentContext), baggage);
        assert.equal(parentContext.getValue(samplerInput), 'new');
        assert.equal(parentContext.getValue(staleInput), undefined);
        assert.equal(trace.getSpan(parentContext), harness.starts[0].span);
        assert.equal(context.active(), callContext);
        assert.equal(trace.getSpan(context.active()), unrelated);
        tool.end();
        assert.equal(tool.getError(), undefined);
        await Promise.resolve();
        assert.equal(context.active(), callContext);
      });
      generation.end();
      assertParentage(harness);
    } finally {
      await shutdownHarness(harness);
    }
  });

  for (const [before, atCall] of [
    ['record', 'drop'],
    ['drop', 'record'],
  ]) {
    test(`${method}: baggage-dependent sampler uses tool-time ${atCall}, not generation-time ${before}`, async () => {
      const sampled = [];
      const sampler = {
        shouldSample(parentContext, _traceId, name) {
          if (!name.startsWith('execute_tool')) {
            return { decision: SamplingDecision.RECORD_AND_SAMPLED };
          }
          const policy = propagation.getBaggage(parentContext)?.getEntry('tool-policy')?.value;
          sampled.push({ policy, input: parentContext.getValue(samplerInput) });
          return {
            decision: policy === 'record' ? SamplingDecision.RECORD_AND_SAMPLED : SamplingDecision.NOT_RECORD,
          };
        },
        toString: () => 'ToolBaggageSampler',
      };
      const harness = newHarness(sampler);
      const policyContext = (policy) =>
        propagation
          .setBaggage(ROOT_CONTEXT, propagation.createBaggage({ 'tool-policy': { value: policy } }))
          .setValue(samplerInput, policy);
      try {
        const generation = await context.with(policyContext(before), async () => {
          await Promise.resolve();
          return harness.client[method](generationStart);
        });
        await context.with(policyContext(atCall), async () => {
          await Promise.resolve();
          const tool = generation.startToolExecution({ toolName: 'sampled' });
          tool.end();
          assert.equal(tool.getError(), undefined);
        });
        generation.end();

        assert.deepEqual(sampled, [{ policy: atCall, input: atCall }]);
        assert.equal(toolSpans(harness).length, atCall === 'record' ? 1 : 0);
        const parent = harness.starts[0].span.spanContext();
        assert.equal(harness.starts[1].span.spanContext().traceId, parent.traceId);
        assert.equal(trace.getSpanContext(harness.starts[1].parentContext).spanId, parent.spanId);
        if (atCall === 'record') {
          assertParentage(harness);
        }
      } finally {
        await shutdownHarness(harness);
      }
    });
  }
}

function newHarness(sampler) {
  const spanExporter = new InMemorySpanExporter();
  const traceProvider = new BasicTracerProvider({
    ...(sampler ? { sampler } : {}),
    spanProcessors: [new SimpleSpanProcessor(spanExporter)],
  });
  const tracer = traceProvider.getTracer('agento11y-tool-context-test');
  const starts = [];
  const client = new Agento11yClient({
    tracer: {
      startSpan(name, options, parentContext) {
        const span = tracer.startSpan(name, options, parentContext);
        starts.push({ span, parentContext });
        return span;
      },
    },
    generationExport: { protocol: 'none', flushIntervalMs: 0 },
  });
  return { client, spanExporter, traceProvider, starts };
}

async function shutdownHarness(harness) {
  await harness.client.shutdown();
  await harness.traceProvider.shutdown();
}

function toolSpans(harness) {
  return harness.spanExporter
    .getFinishedSpans()
    .filter((span) => span.attributes['gen_ai.operation.name'] === 'execute_tool');
}

function assertParentage(harness) {
  const [tool] = toolSpans(harness);
  const generation = harness.starts[0].span.spanContext();
  assert.equal(tool.spanContext().traceId, generation.traceId);
  assert.equal(tool.parentSpanContext?.spanId, generation.spanId);
}
