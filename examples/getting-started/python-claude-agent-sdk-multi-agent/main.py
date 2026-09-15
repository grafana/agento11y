"""Multi-agent dependency graph example — Python + Claude Agent SDK.

Same shape as ../python-multi-agent, but each agent is a Claude Agent SDK
``query()`` run instead of a raw OpenAI call. The Claude Agent SDK has no
Sigil framework adapter, so this shows manual instrumentation with the core
SDK: collect each run's text + token usage from the message stream, then
record one generation per agent and link them with parent_generation_ids.

    researcher ──┐
                 ├──► synthesizer
    critic ──────┘
"""

import asyncio
import os
from dataclasses import dataclass, field

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    query,
)
from dotenv import load_dotenv
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

resource = Resource.create({"service.name": "getting-started-claude-agent-sdk"})

tp = TracerProvider(resource=resource)
tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(tp)

mp = MeterProvider(
    resource=resource,
    metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
)
metrics.set_meter_provider(mp)

model = "claude-sonnet-4-5"

sigil = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=os.environ["SIGIL_ENDPOINT"],
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["GRAFANA_INSTANCE_ID"],
                basic_password=os.environ["GRAFANA_CLOUD_TOKEN"],
            ),
        ),
    )
)

topic = "the impact of LLM observability on production AI reliability"

CONVERSATION_ID = "getting-started-claude-agent-sdk"
CONVERSATION_TITLE = "Multi-Agent Example (Claude Agent SDK)"


@dataclass(slots=True)
class AgentRun:
    """Everything collected from one Claude Agent SDK query()."""

    prompt: str
    text: str = ""
    response_model: str = model
    response_id: str = ""
    stop_reason: str = ""
    usage: dict = field(default_factory=dict)


async def run_agent(system: str, prompt: str) -> AgentRun:
    """Run a one-shot Claude Agent SDK query and collect text + usage."""
    run = AgentRun(prompt=prompt)
    parts: list[str] = []
    options = ClaudeAgentOptions(system_prompt=system, model=model, max_turns=1)
    async for message in query(prompt=prompt, options=options):
        if isinstance(message, AssistantMessage):
            run.response_model = message.model or run.response_model
            run.response_id = getattr(message, "message_id", "") or run.response_id
            parts += [b.text for b in message.content if isinstance(b, TextBlock)]
        elif isinstance(message, ResultMessage):
            run.usage = message.usage or {}
            run.response_id = run.response_id or (message.session_id or "")
            run.stop_reason = getattr(message, "stop_reason", "") or message.subtype or ""
            run.text = "".join(parts) or (message.result or "")
    return run


def record(rec, run: AgentRun) -> None:
    """Record one Claude Agent SDK run on a generation recorder."""
    usage = run.usage
    rec.set_result(
        input=[user_text_message(run.prompt)],
        output=[assistant_text_message(run.text)],
        response_id=run.response_id,
        response_model=run.response_model,
        stop_reason=run.stop_reason,
        usage=TokenUsage(
            input_tokens=usage.get("input_tokens", 0),
            output_tokens=usage.get("output_tokens", 0),
            cache_read_input_tokens=usage.get("cache_read_input_tokens", 0),
            # Anthropic reports cache writes as cache_creation_input_tokens;
            # Sigil's field is cache_write_input_tokens (see CLAUDE.md).
            cache_write_input_tokens=usage.get("cache_creation_input_tokens", 0),
        ),
    )


def generation(agent_name: str, parents: list[str] | None = None) -> GenerationStart:
    """Build a GenerationStart seed shared by every agent in this example."""
    return GenerationStart(
        conversation_id=CONVERSATION_ID,
        conversation_title=CONVERSATION_TITLE,
        agent_name=agent_name,
        model=ModelRef(provider="anthropic", name=model),
        parent_generation_ids=parents or [],
    )


async def main() -> None:
    # ── Step 1: fan-out — researcher and critic (no parents) ────────────────
    research = await run_agent(
        "You are a research analyst. Give a 2-sentence factual overview.", topic
    )
    print(f"Researcher: {research.text}\n")
    with sigil.start_generation(generation("researcher")) as researcher_rec:
        record(researcher_rec, research)
    if researcher_rec.err() is not None:
        print("SDK error:", researcher_rec.err())
    researcher_id = researcher_rec.last_generation.id

    critique = await run_agent(
        "You are a critical reviewer. List 2 counterarguments in 2 sentences.", topic
    )
    print(f"Critic: {critique.text}\n")
    with sigil.start_generation(generation("critic")) as critic_rec:
        record(critic_rec, critique)
    if critic_rec.err() is not None:
        print("SDK error:", critic_rec.err())
    critic_id = critic_rec.last_generation.id

    # ── Step 2: fan-in — synthesizer depends on both ────────────────────────
    synth_prompt = f"Research:\n{research.text}\n\nCritique:\n{critique.text}"
    synthesis = await run_agent(
        "Combine the research and critique into a balanced 2-sentence summary.", synth_prompt
    )
    print(f"Synthesizer: {synthesis.text}\n")
    with sigil.start_generation(
        generation("synthesizer", parents=[researcher_id, critic_id])
    ) as synth_rec:
        record(synth_rec, synthesis)
    if synth_rec.err() is not None:
        print("SDK error:", synth_rec.err())


asyncio.run(main())

# ── Shutdown ─────────────────────────────────────────────────────────────
sigil.shutdown()
tp.shutdown()
mp.shutdown()
print("Done — check the Graph tab in the conversation detail in AI Observability.")
