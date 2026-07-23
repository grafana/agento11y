"""Multi-agent dependency graph example — Python + OpenAI.

Two agents (researcher + critic) run independently, then a synthesizer
combines their outputs. Uses parent_generation_ids to declare the dependency.

    researcher ──┐
                 ├──► synthesizer
    critic ──────┘
"""

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
from agento11y import (
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

resource = Resource.create({"service.name": "getting-started-multi-agent"})

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

agento11y = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=os.environ["AGENTO11Y_ENDPOINT"],
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["GRAFANA_INSTANCE_ID"],
                basic_password=os.environ["GRAFANA_CLOUD_TOKEN"],
            ),
        ),
    )
)

topic = "the impact of LLM observability on production AI reliability"


def call_openai(system: str, user: str) -> tuple:
    """Make an OpenAI call and return (completion, response_text, usage)."""
    completion = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "system", "content": system}, {"role": "user", "content": user}],
    )
    return completion, completion.choices[0].message.content or "", completion.usage


def record(rec, completion, user_prompt, response_text, usage):
    """Record result on a generation recorder."""
    rec.set_result(
        input=[user_text_message(user_prompt)],
        output=[assistant_text_message(response_text)],
        response_id=completion.id,
        response_model=completion.model,
        stop_reason=completion.choices[0].finish_reason or "",
        usage=TokenUsage(
            input_tokens=usage.prompt_tokens if usage else 0,
            output_tokens=usage.completion_tokens if usage else 0,
        ),
    )


# ── Step 1: fan-out — researcher and critic (no parents) ────────────────

comp, research, usage = call_openai(
    "You are a research analyst. Give a 2-sentence factual overview.", topic
)
print(f"Researcher: {research}\n")

with agento11y.start_generation(
    GenerationStart(
        conversation_id="getting-started-multi-agent",
        conversation_title="Multi-Agent Example",
        agent_name="researcher",
        model=ModelRef(provider="openai", name=model),
    )
) as researcher_rec:
    record(researcher_rec, comp, topic, research, usage)

if researcher_rec.err() is not None:
    print("SDK error:", researcher_rec.err())
researcher_id = researcher_rec.last_generation.id

comp, critique, usage = call_openai(
    "You are a critical reviewer. List 2 counterarguments in 2 sentences.", topic
)
print(f"Critic: {critique}\n")

with agento11y.start_generation(
    GenerationStart(
        conversation_id="getting-started-multi-agent",
        conversation_title="Multi-Agent Example",
        agent_name="critic",
        model=ModelRef(provider="openai", name=model),
    )
) as critic_rec:
    record(critic_rec, comp, topic, critique, usage)

if critic_rec.err() is not None:
    print("SDK error:", critic_rec.err())
critic_id = critic_rec.last_generation.id

# ── Step 2: fan-in — synthesizer depends on both ────────────────────────

synth_prompt = f"Research:\n{research}\n\nCritique:\n{critique}"
comp, synthesis, usage = call_openai(
    "Combine the research and critique into a balanced 2-sentence summary.", synth_prompt
)
print(f"Synthesizer: {synthesis}\n")

with agento11y.start_generation(
    GenerationStart(
        conversation_id="getting-started-multi-agent",
        conversation_title="Multi-Agent Example",
        agent_name="synthesizer",
        model=ModelRef(provider="openai", name=model),
        parent_generation_ids=[researcher_id, critic_id],
    )
) as synth_rec:
    record(synth_rec, comp, synth_prompt, synthesis, usage)

if synth_rec.err() is not None:
    print("SDK error:", synth_rec.err())

# ── Shutdown ─────────────────────────────────────────────────────────────

agento11y.shutdown()
tp.shutdown()
mp.shutdown()
print("Done — check the Graph tab in the conversation detail in Agent Observability.")
