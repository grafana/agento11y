"""Real Anthropic tool agent. No canned responses, mocked usage, or LLM judges."""

import json
import os
import time
from pathlib import Path

from agento11y import ToolExecutionStart
from agento11y.experiments import Client, stable_id
from agento11y_anthropic import AnthropicOptions, messages
from anthropic import AsyncAnthropic
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

HERE = Path(__file__).resolve().parent
CASES = json.loads((HERE / "cases.json").read_text())
MODELS = {"sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5-20251001"}
# Standard global API USD/MTok, verified 2026-09-17. No caching requested.
# https://platform.claude.com/docs/en/about-claude/pricing
PRICES = {MODELS["sonnet"]: (2, 10), MODELS["haiku"]: (1, 5)}
TOOLS = [
    {
        "name": "read_orders",
        "description": "Read the order ledger. Order total = quantity * unit_price_cents.",
        "input_schema": {"type": "object", "properties": {}, "additionalProperties": False},
    },
    {
        "name": "submit_answer",
        "description": "Save the final numeric answer, after reading the ledger.",
        "input_schema": {
            "type": "object",
            "properties": {"answer": {"type": "string"}},
            "required": ["answer"],
            "additionalProperties": False,
        },
    },
]


def setup():
    # Require an explicit destination; OTel otherwise defaults to loopback.
    if not os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"):
        raise RuntimeError("Set OTEL_EXPORTER_OTLP_ENDPOINT to your Grafana Cloud OTLP endpoint")
    resource = Resource.create({"service.name": "agento11y-real-agent-comparison"})
    tp = TracerProvider(resource=resource)
    tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tp)
    mp = MeterProvider(
        resource=resource,
        metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter(), export_interval_millis=1000)],
    )
    metrics.set_meter_provider(mp)
    return tp, mp


def client():
    return Client(os.environ["AGENTO11Y_ENDPOINT"], tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"])


async def run_agent(question, model, run_id, case_id, sdk, *, tool=None):
    """Tool callbacks read/write actual sandbox files for Harbor, local files otherwise."""
    conv = stable_id("conv", run_id, case_id)
    history = [{"role": "user", "content": question}]
    responses = []
    read = False
    answer = None
    started = time.perf_counter()
    tool_count = 0
    options = AnthropicOptions(
        conversation_id=conv,
        agent_name="order-ledger-agent",
        agent_version="1",
        tags={"experiment_id": run_id, "test.case.id": case_id},
        metadata={"cost_basis": "provider_usage_x_published_list_price", "example": "real-agent"},
    )
    async with AsyncAnthropic(max_retries=1, timeout=60) as api:
        with trace.get_tracer(__name__).start_as_current_span("order-ledger-agent"):
            for _ in range(5):
                request = dict(
                    model=model,
                    max_tokens=512,
                    system=(
                        "Read the ledger using tools, solve the task, then submit_answer with only the numeric answer. "
                        "Do not guess data."
                    ),
                    messages=history,
                    tools=TOOLS,
                    tool_choice={"type": "any"},
                )
                response = await messages.create_async(
                    sdk.core, request, lambda req: api.messages.create(**req), options
                )
                responses.append(response.model_dump(mode="json"))
                history.append(
                    {
                        "role": "assistant",
                        "content": [block.model_dump(exclude_none=True) for block in response.content],
                    }
                )
                results = []
                for block in response.content:
                    if block.type != "tool_use":
                        continue
                    tool_count += 1
                    with sdk.core.start_tool_execution(
                        ToolExecutionStart(
                            tool_name=block.name,
                            tool_call_id=block.id,
                            conversation_id=conv,
                            agent_name="order-ledger-agent",
                            request_model=model,
                            request_provider="anthropic",
                        )
                    ) as rec:
                        if block.name == "read_orders":
                            result = await tool("read_orders", {}) if tool else (HERE / "orders.json").read_text()
                            read = True
                        elif block.name == "submit_answer" and read:
                            answer = str(block.input["answer"]).strip()
                            result = await tool("submit_answer", {"answer": answer}) if tool else "Answer received"
                        else:
                            result = "Read the ledger before submitting an answer."
                        rec.set_result(arguments=block.input, result=result)
                    results.append({"type": "tool_result", "tool_use_id": block.id, "content": str(result)})
                if answer is not None:
                    break
                history.append({"role": "user", "content": results})
    usage = [r["usage"] for r in responses]
    fresh = sum(u["input_tokens"] for u in usage)
    output = sum(u["output_tokens"] for u in usage)
    cached = sum(u.get("cache_read_input_tokens", 0) or 0 for u in usage)
    written = sum(u.get("cache_creation_input_tokens", 0) or 0 for u in usage)
    # No prompt caching configured; reject unexpected pricing semantics instead of guessing.
    if cached or written:
        raise RuntimeError("Unexpected cache usage: reconcile cache pricing before publication")
    inp_price, out_price = PRICES[model]
    result = dict(
        output=answer,
        conversation_id=conv,
        input_tokens=fresh,
        output_tokens=output,
        cost=(fresh * inp_price + output * out_price) / 1_000_000,
        duration_ms=int((time.perf_counter() - started) * 1000),
        model=model,
        tool_calls=tool_count,
        cost_basis="provider_usage_x_published_list_price",
        responses=responses,
    )
    folder = Path(os.environ["REAL_AGENT_RESULTS"]) / run_id
    folder.mkdir(parents=True, exist_ok=True)
    (folder / f"{case_id}.json").write_text(json.dumps(result, indent=2))
    sdk.flush_generations()
    if answer is None:
        raise RuntimeError("Agent did not submit an answer within five model turns")
    print(f"{run_id}/{case_id}: {fresh}+{output} tokens, ${result['cost']:.6f}, {answer}", flush=True)
    return result
