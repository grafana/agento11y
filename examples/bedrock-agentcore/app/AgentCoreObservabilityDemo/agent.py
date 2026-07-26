from __future__ import annotations

import json
import os
import time
from typing import Any
from uuid import uuid4

from agento11y import Client
from agento11y_langchain import with_agento11y_langchain_callbacks
from demo_data import get_order, get_support_cases, make_return, search_products
from langchain.agents import create_agent
from langchain_core.tools import tool
from opentelemetry import metrics
from telemetry import Telemetry

SYSTEM_PROMPT = """You are a demo service-operations assistant.

Use tools for inventory, order status, support cases, and return creation. Do
not invent stock counts, order status, prices, account IDs, case IDs, or return
IDs. If data is unavailable, say what is missing and suggest the next
operational step.

Keep replies concise and practical. Include SKU, warehouse, low-stock status,
account impact, and escalation recommendation when relevant.
"""

TOOLS: list[Any]
_TOOL_CALL_COUNTER: Any | None = None


def _json(data: Any) -> str:
    return json.dumps(data, indent=2, sort_keys=True)


def _record_tool_call(tool_name: str) -> None:
    global _TOOL_CALL_COUNTER
    if _TOOL_CALL_COUNTER is None:
        meter = metrics.get_meter(os.getenv("OTEL_SERVICE_NAME", "agento11y-bedrock-agentcore"))
        _TOOL_CALL_COUNTER = meter.create_counter(
            "agentcore_demo.tool.calls",
            description="LangChain tool invocations by tool name.",
        )
    _TOOL_CALL_COUNTER.add(1, {"tool.name": tool_name})


@tool
def lookup_inventory(query: str, region: str = "US") -> str:
    """Search demo inventory by SKU, product, category, or warehouse."""
    _record_tool_call("lookup_inventory")
    return _json({"matches": search_products(query=query, region=region)})


@tool
def check_order_status(order_id: str) -> str:
    """Look up a demo order by ID, such as DEMO-1007."""
    _record_tool_call("check_order_status")
    order = get_order(order_id)
    if order is None:
        return _json(
            {
                "found": False,
                "order_id": order_id,
                "message": "No order found.",
            }
        )
    return _json({"found": True, "order_id": order_id.strip().upper(), **order})


@tool
def list_support_cases(sku: str) -> str:
    """List recent demo support cases for a product SKU."""
    _record_tool_call("list_support_cases")
    return _json({"sku": sku.strip().upper(), "cases": get_support_cases(sku)})


@tool
def create_return_authorization(
    account_id: str,
    sku: str,
    quantity: int,
    reason: str,
) -> str:
    """Create a synthetic return authorization for an account."""
    _record_tool_call("create_return_authorization")
    return _json(
        make_return(
            account_id=account_id,
            sku=sku,
            quantity=quantity,
            reason=reason,
        )
    )


TOOLS = [
    lookup_inventory,
    check_order_status,
    list_support_cases,
    create_return_authorization,
]


def build_model() -> Any:
    provider = os.getenv("AGENT_MODEL_PROVIDER", "bedrock").strip().lower()
    temperature = float(os.getenv("AGENT_TEMPERATURE", "0.2"))
    max_tokens = int(os.getenv("AGENT_MAX_TOKENS", "1200"))

    if provider == "bedrock":
        from langchain_aws import ChatBedrockConverse

        return ChatBedrockConverse(
            model=os.getenv(
                "BEDROCK_MODEL_ID",
                "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
            ),
            region_name=(os.getenv("BEDROCK_REGION") or os.getenv("AWS_REGION") or "us-west-2"),
            temperature=temperature,
            max_tokens=max_tokens,
        )

    if provider != "anthropic":
        raise ValueError("AGENT_MODEL_PROVIDER must be 'bedrock' or 'anthropic'.")
    if not os.getenv("ANTHROPIC_API_KEY"):
        raise RuntimeError("ANTHROPIC_API_KEY is required when AGENT_MODEL_PROVIDER=anthropic.")

    from langchain_anthropic import ChatAnthropic

    return ChatAnthropic(
        model=os.getenv(
            "ANTHROPIC_MODEL",
            "claude-sonnet-4-5-20250929",
        ),
        temperature=temperature,
        max_tokens=max_tokens,
    )


def build_agent() -> Any:
    return create_agent(
        model=build_model(),
        tools=TOOLS,
        system_prompt=SYSTEM_PROMPT,
    )


def run_agent(
    *,
    agent: Any,
    agento11y: Client,
    telemetry: Telemetry,
    prompt: str,
    conversation_id: str | None = None,
    account_id: str = "acct-demo-1042",
) -> dict[str, Any]:
    conversation_id = (conversation_id or "").strip() or f"conv-{uuid4().hex[:12]}"
    provider = os.getenv("AGENT_MODEL_PROVIDER", "bedrock").strip().lower()
    metric_attributes = {
        "gen_ai.provider.name": provider,
    }

    started = time.perf_counter()
    with telemetry.tracer.start_as_current_span("agentcore_demo.agent.invoke") as span:
        span.set_attribute("gen_ai.conversation.id", conversation_id)
        span.set_attribute("account.id", account_id)
        try:
            config = with_agento11y_langchain_callbacks(
                {
                    "metadata": {
                        "conversation_id": conversation_id,
                    }
                },
                client=agento11y,
                provider_resolver="auto",
                agent_name=os.getenv(
                    "AGENTO11Y_AGENT_NAME",
                    "agentcore-operations-demo",
                ),
                agent_version=os.getenv(
                    "AGENTO11Y_AGENT_VERSION",
                    "0.1.0",
                ),
                extra_tags={
                    "runtime": "bedrock-agentcore",
                    "framework": "langchain",
                },
                extra_metadata={
                    "account_id": account_id,
                    "agentcore.session_id": conversation_id,
                },
            )
            result = agent.invoke(
                {"messages": [{"role": "user", "content": prompt}]},
                config=config,
            )
            telemetry.requests.add(
                1,
                {**metric_attributes, "status": "ok"},
            )
            return {
                "conversation_id": conversation_id,
                "reply": _final_text(result),
                "status": "ok",
            }
        except Exception as error:
            span.record_exception(error)
            telemetry.requests.add(
                1,
                {**metric_attributes, "status": "error"},
            )
            return {
                "conversation_id": conversation_id,
                "reply": "The agent hit an error while processing the request.",
                "status": "error",
            }
        finally:
            telemetry.duration.record(
                time.perf_counter() - started,
                metric_attributes,
            )


def _final_text(result: Any) -> str:
    messages = result.get("messages", []) if isinstance(result, dict) else []
    for message in reversed(messages):
        content = getattr(message, "content", None)
        if isinstance(content, str) and content.strip():
            return content
        if isinstance(content, list):
            parts = [
                str(part.get("text", "")) for part in content if isinstance(part, dict) and part.get("type") == "text"
            ]
            text = "".join(parts).strip()
            if text:
                return text
    return ""
