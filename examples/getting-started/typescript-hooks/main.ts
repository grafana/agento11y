/**
 * Guarded Agent Observability getting-started example — TypeScript + OpenAI.
 *
 * The SDK evaluates an agento11y preflight hook before the provider call. Guard rules
 * configured in Grafana Cloud can allow the call, deny it, or return transformed
 * input such as redacted messages.
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
import { createAgento11yClient } from "@grafana/agento11y";
import type {
  GenerationRecorder,
  HookEvaluateRequest,
  Message,
} from "@grafana/agento11y";

function agento11yApiEndpoint(): string {
  const url = new URL(process.env.AGENTO11Y_ENDPOINT!);
  return `${url.protocol}//${url.host}`;
}

function messageText(message: Message): string {
  if (message.content !== undefined) {
    return message.content;
  }
  return (message.parts ?? [])
    .map((part) => (part.type === "text" ? part.text : ""))
    .filter((text) => text.length > 0)
    .join("\n");
}

function openaiMessages(
  systemPrompt: string,
  messages: Message[],
): OpenAI.Chat.Completions.ChatCompletionMessageParam[] {
  const out: OpenAI.Chat.Completions.ChatCompletionMessageParam[] = [
    { role: "system", content: systemPrompt },
  ];
  for (const message of messages) {
    out.push(
      message.role === "assistant"
        ? { role: "assistant", content: messageText(message) }
        : { role: "user", content: messageText(message) },
    );
  }
  return out;
}

const resource = new Resource({
  "service.name": "getting-started-typescript-hooks",
});

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

const agento11y = createAgento11yClient({
  generationExport: {
    protocol: "http",
    endpoint: process.env.AGENTO11Y_ENDPOINT!,
    auth: {
      mode: "basic",
      tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID!,
      basicPassword: process.env.AGENTO11Y_AUTH_TOKEN!,
    },
  },
  api: { endpoint: agento11yApiEndpoint() },
  hooks: { enabled: true, phases: ["preflight"] },
});

let systemPrompt = "You are a helpful assistant. Keep answers concise.";
const prompt =
  "My name is Jane Doe and my email is jane@example.com. Explain LLM guardrails in one sentence.";
let inputMessages: Message[] = [{ role: "user", content: prompt }];

const hookRequest: HookEvaluateRequest = {
  phase: "preflight",
  context: {
    agentName: "getting-started-hooks",
    agentVersion: "1.0.0",
    model: { provider: "openai", name: model },
  },
  input: {
    messages: inputMessages,
    systemPrompt,
    conversationPreview: prompt,
  },
};

const hookResponse = await agento11y.evaluateHook(hookRequest);

if (hookResponse.action === "deny") {
  console.log(
    `Blocked by guard rule ${hookResponse.ruleId ?? "<unknown>"}: ${hookResponse.reason ?? ""}`,
  );
} else {
  const transformed = hookResponse.transformedInput;
  if (transformed !== undefined) {
    if (transformed.messages && transformed.messages.length > 0) {
      inputMessages = transformed.messages;
    }
    if (transformed.systemPrompt) {
      systemPrompt = transformed.systemPrompt;
    }
    console.log("agento11y hook allowed the call with transformed input.\n");
  } else {
    console.log("agento11y hook allowed the call.\n");
  }

  const completion = await openai.chat.completions.create({
    model,
    messages: openaiMessages(systemPrompt, inputMessages),
  });

  const responseText = completion.choices[0].message.content ?? "";
  const usage = completion.usage;
  console.log(`Response: ${responseText}\n`);

  await agento11y.startGeneration(
    {
      conversationId: "getting-started-typescript-hooks",
      agentName: "getting-started-hooks",
      agentVersion: "1.0.0",
      model: { provider: "openai", name: model },
      systemPrompt,
    },
    (rec: GenerationRecorder) => {
      rec.setResult({
        input: inputMessages,
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

  console.log(
    "Done — check the Agent Observability plugin in your Grafana Cloud stack.",
  );
}

await agento11y.shutdown();
await tp.shutdown();
await mp.shutdown();
