"""Minimal AI Observability getting-started example — Python + OpenAI."""

import os

from dotenv import load_dotenv
from openai import OpenAI
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from sigil_sdk import (
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    GenerationStart,
    ModelRef,
    TokenUsage,
    assistant_text_message,
    user_text_message,
)

load_dotenv()

resource = Resource.create({"service.name": "getting-started-python"})

tp = TracerProvider(resource=resource)
tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(tp)

mp = MeterProvider(
    resource=resource,
    metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
)
metrics.set_meter_provider(mp)

openai_client = OpenAI()
model = "gpt-4.1-mini"

sigil = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=os.environ["SIGIL_ENDPOINT"],
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["SIGIL_AUTH_TENANT_ID"],
                basic_password=os.environ["SIGIL_AUTH_TOKEN"],
            ),
        ),
        # Client tags attach to every generation and become sigil.tag.<key>
        # attributes on OTel spans and metrics, so keep them low-cardinality
        # (team, env). See docs/concepts/tags-and-metadata.md.
        tags={"team": "checkout", "env": "dev"},
    )
)

prompt = "Explain what LLM observability is in two sentences."

completion = openai_client.chat.completions.create(
    model=model,
    messages=[
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": prompt},
    ],
)

response_text = completion.choices[0].message.content
usage = completion.usage
print(f"Response: {response_text}\n")

with sigil.start_generation(
    GenerationStart(
        conversation_id="getting-started-python",
        agent_name="getting-started",
        agent_version="1.0.0",
        model=ModelRef(provider="openai", name=model),
        # user_id sets the user.id span attribute; use it for end-user
        # identity instead of a high-cardinality tag.
        user_id="demo-user",
        # Per-generation tags and metadata are export-only: searchable on the
        # generation in Sigil, never emitted on spans or metrics.
        tags={"feature": "summarize"},
        metadata={"prompt_version": "v2"},
    )
) as rec:
    rec.set_result(
        input=[user_text_message(prompt)],
        output=[assistant_text_message(response_text or "")],
        response_id=completion.id,
        response_model=completion.model,
        stop_reason=completion.choices[0].finish_reason or "",
        usage=TokenUsage(
            input_tokens=usage.prompt_tokens if usage else 0,
            output_tokens=usage.completion_tokens if usage else 0,
        ),
    )
    if rec.err() is not None:
        print("SDK error:", rec.err())

sigil.shutdown()
tp.shutdown()
mp.shutdown()
print("Done — check the AI Observability plugin in your Grafana Cloud stack.")
