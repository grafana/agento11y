"""Real Anthropic tool agent. No canned responses, mocked usage, or LLM judges."""

import asyncio
import contextlib
import json
import os
import sys
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


def conversation_id(run_id, case_id):
    return stable_id("conv", run_id, case_id)


class AgentRunFailed(RuntimeError):
    """A run that did not finish. ``on_result`` has already received its completed turns."""


def write_result_file(run_id, case_id, result):
    """Keep a copy for inspection. Best effort: it never changes the run's outcome."""
    results_dir = os.environ.get("REAL_AGENT_RESULTS")
    if not results_dir:
        return
    try:
        folder = Path(results_dir) / run_id
        folder.mkdir(parents=True, exist_ok=True)
        (folder / f"{case_id}.json").write_text(json.dumps(result, indent=2))
    except OSError as exc:
        print(f"{run_id}/{case_id}: could not write the result file: {exc}", file=sys.stderr, flush=True)


async def run_agent(question, model, run_id, case_id, sdk, *, tool=None, on_result=None):
    """Tool callbacks read/write actual sandbox files for Harbor, local files otherwise.

    ``on_result`` receives the usage of the completed turns however the run ends:
    success, failure, a failed flush, or cancellation (Harbor's timeout).
    """
    conv = conversation_id(run_id, case_id)
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

    def finish(error):
        usage = [r["usage"] for r in responses]
        fresh = sum(u["input_tokens"] for u in usage)
        output = sum(u["output_tokens"] for u in usage)
        cached = sum(u.get("cache_read_input_tokens", 0) or 0 for u in usage)
        written = sum(u.get("cache_creation_input_tokens", 0) or 0 for u in usage)
        inp_price, out_price = PRICES[model]
        cost = (fresh * inp_price + output * out_price) / 1_000_000
        # No prompt caching configured; reject unexpected pricing semantics instead of guessing.
        if cached or written:
            cost = None
            error = error or "Unexpected cache usage: reconcile cache pricing before publication"
        if error is None and answer is None:
            error = "Agent did not submit an answer within five model turns"
        result = dict(
            output=answer,
            conversation_id=conv,
            input_tokens=fresh,
            output_tokens=output,
            cost=cost,
            duration_ms=int((time.perf_counter() - started) * 1000),
            model=model,
            tool_calls=tool_count,
            cost_basis="provider_usage_x_published_list_price",
            error=error,
            responses=responses,
        )
        # A failed export becomes the run's error instead of escaping, so the usage still reaches the caller.
        try:
            sdk.flush_generations()
        except Exception as exc:
            result["error"] = error or f"Generation export failed: {type(exc).__name__}: {exc}"
        if on_result:
            on_result(result)
        write_result_file(run_id, case_id, result)
        return result

    error = None
    try:
        async with AsyncAnthropic(max_retries=1, timeout=60) as api:
            with trace.get_tracer(__name__).start_as_current_span("order-ledger-agent"):
                for _ in range(5):
                    request = dict(
                        model=model,
                        max_tokens=512,
                        system=(
                            "Read the ledger using tools, solve the task, then submit_answer "
                            "with only the numeric answer. Do not guess data."
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
    except asyncio.CancelledError:
        # Harbor cancels the coroutine on timeout: keep the completed turns, then stay cancelled.
        with contextlib.suppress(Exception):
            finish("Cancelled before the agent finished")
        raise
    except Exception as exc:
        error = f"{type(exc).__name__}: {exc}"
    result = finish(error)
    if result["error"] is not None:
        raise AgentRunFailed(result["error"])
    print(
        f"{run_id}/{case_id}: {result['input_tokens']}+{result['output_tokens']} tokens, "
        f"${result['cost']:.6f}, {answer}",
        flush=True,
    )
    return result
