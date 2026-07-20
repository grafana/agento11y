/**
 * Minimal AI Observability getting-started example — TypeScript + OpenAI.
 */

import "dotenv/config";
import OpenAI from "openai";
import { metrics } from "@opentelemetry/api";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import {
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-http";
import { Resource } from "@opentelemetry/resources";
import { createSigilClient } from "@grafana/agento11y";
import type { GenerationRecorder } from "@grafana/agento11y";

const resource = new Resource({ "service.name": "getting-started-typescript" });

const tp = new NodeTracerProvider({ resource });
tp.addSpanProcessor(new BatchSpanProcessor(new OTLPTraceExporter()));
tp.register();

const mp = new MeterProvider({
  resource,
  readers: [
    new PeriodicExportingMetricReader({ exporter: new OTLPMetricExporter() }),
  ],
});
metrics.setGlobalMeterProvider(mp);

const openai = new OpenAI();
const model = "gpt-4.1-mini";

const sigil = createSigilClient({
  generationExport: {
    protocol: "http",
    endpoint: process.env.AGENTO11Y_ENDPOINT!,
    auth: {
      mode: "basic",
      tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID!,
      basicPassword: process.env.AGENTO11Y_AUTH_TOKEN!,
    },
  },
  // Client tags attach to every generation and become sigil.tag.<key>
  // attributes on OTel spans and metrics, so keep them low-cardinality
  // (team, env). See docs/concepts/tags-and-metadata.md.
  tags: { team: "checkout", env: "dev" },
});

const prompt = "Explain what LLM observability is in two sentences.";

const completion = await openai.chat.completions.create({
  model,
  messages: [
    { role: "system", content: "You are a helpful assistant." },
    { role: "user", content: prompt },
  ],
});

const responseText = completion.choices[0].message.content ?? "";
const usage = completion.usage;
console.log(`Response: ${responseText}\n`);

await sigil.startGeneration(
  {
    conversationId: "getting-started-typescript",
    agentName: "getting-started",
    agentVersion: "1.0.0",
    model: { provider: "openai", name: model },
    // userId sets the user.id span attribute (all SDKs); use it for end-user
    // identity instead of a high-cardinality tag.
    userId: "demo-user",
    // Per-generation tags and metadata are export-only: searchable on the
    // generation in Sigil, never emitted on spans or metrics.
    tags: { feature: "summarize" },
    metadata: { promptVersion: "v2" },
  },
  (rec: GenerationRecorder) => {
    rec.setResult({
      input: [{ role: "user", content: prompt }],
      output: [{ role: "assistant", content: responseText }],
      responseId: completion.id,
      responseModel: completion.model,
      stopReason: completion.choices[0].finish_reason ?? "",
      usage: {
        inputTokens: usage?.prompt_tokens ?? 0,
        outputTokens: usage?.completion_tokens ?? 0,
      },
    });
  },
);

await sigil.shutdown();
await tp.shutdown();
await mp.shutdown();
console.log(
  "Done — check the AI Observability plugin in your Grafana Cloud stack.",
);
