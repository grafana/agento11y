/**
 * Minimal Sigil Cloud example using Strands Agents TypeScript.
 */

import 'dotenv/config';
import { metrics } from '@opentelemetry/api';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { resourceFromAttributes } from '@opentelemetry/resources';
import {
  MeterProvider,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { Agent, tool } from '@strands-agents/sdk';
import { OpenAIModel } from '@strands-agents/sdk/models/openai';
import { createAgento11yClient } from '@grafana/agento11y';
import { withAgento11yStrandsHooks } from '@grafana/agento11y/strands';
import { z } from 'zod';

function env(name: string, fallback: string): string {
  const value = process.env[name]?.trim();
  return value && value.length > 0 ? value : fallback;
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set (see .env.example).`);
  }
  return value;
}

const resource = resourceFromAttributes({
  'service.name': env('OTEL_SERVICE_NAME', 'agento11y-strands-typescript-example'),
});

const tracerProvider = new NodeTracerProvider({
  resource,
  spanProcessors: [new BatchSpanProcessor(new OTLPTraceExporter())],
});
tracerProvider.register();

const metricReader = new PeriodicExportingMetricReader({
  exporter: new OTLPMetricExporter(),
  exportIntervalMillis: Number(env('OTEL_METRIC_EXPORT_INTERVAL_MILLIS', '1000')),
});
const meterProvider = new MeterProvider({
  resource,
  readers: [metricReader],
});
metrics.setGlobalMeterProvider(meterProvider);

const addNumbers = tool({
  name: 'add_numbers',
  description: 'Add two integers.',
  inputSchema: z.object({
    left: z.number().int().describe('The left integer.'),
    right: z.number().int().describe('The right integer.'),
  }),
  callback: ({ left, right }) => left + right,
});

const tenantId = requiredEnv('AGENTO11Y_AUTH_TENANT_ID');
const agento11y = createAgento11yClient({
  generationExport: {
    protocol: env('AGENTO11Y_PROTOCOL', 'http') as 'http' | 'grpc' | 'none',
    endpoint: requiredEnv('AGENTO11Y_ENDPOINT'),
    auth: {
      mode: 'basic',
      tenantId,
      basicUser: tenantId,
      basicPassword: requiredEnv('AGENTO11Y_AUTH_TOKEN'),
    },
  },
  meter: meterProvider.getMeter('agento11y-strands-typescript-example'),
});

const model = new OpenAIModel({
  api: 'chat',
  apiKey: requiredEnv('OPENAI_API_KEY'),
  modelId: env('OPENAI_MODEL', 'gpt-4o-mini'),
  temperature: 0.2,
});

try {
  const agent = new Agent(
    withAgento11yStrandsHooks(
      {
        name: 'strands-demo',
        model,
        tools: [addNumbers],
        systemPrompt: 'You are concise and show the final answer.',
        printer: false,
        appState: {
          conversation_id: env('AGENTO11Y_CONVERSATION_ID', 'agento11y-strands-demo'),
        },
      },
      agento11y,
      { providerResolver: 'auto' },
    ),
  );

  const result = await agent.invoke(
    'Use the add_numbers tool to add 19 and 23, then answer in one sentence.',
  );

  console.log(result.toString());
} finally {
  await agento11y.shutdown();
  await tracerProvider.shutdown();
  await meterProvider.shutdown();
}

console.log('Done');
