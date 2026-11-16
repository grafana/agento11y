"""LangChain agent instrumented via the agento11y callback handler.

The entire agento11y integration is one line: wrap the runnable config with
`with_agento11y_langchain_callbacks(...)`. LLM calls, tool invocations, and
chain spans are all recorded automatically — no manual `start_generation`
needed. Compare with `classifier.py` for the raw-SDK pattern.
"""

from __future__ import annotations

from datetime import date
from typing import Any

from agento11y import Client
from agento11y_langchain import with_agento11y_langchain_callbacks
from langchain.agents import create_agent
from langchain.chat_models import init_chat_model
from langchain_core.tools import tool

from .weather import known_cities, lookup_forecast

SYSTEM_PROMPT_TEMPLATE = (
    "You are a concise weather assistant. Only answer weather questions. "
    "When the user asks about weather, always call the `get_weather` tool. "
    "Today's date is {today}; resolve relative dates like 'today' or "
    "'tomorrow' against it."
)


@tool
def get_weather(city: str, date: str) -> str:
    """Get the weather forecast for a city on a specific date.

    Args:
        city: City name, e.g. "London" or "San Francisco".
        date: ISO date (YYYY-MM-DD). Only April 17-19 2026 are supported.
    """
    forecast = lookup_forecast(city, date)
    if forecast is None:
        return (
            f"No forecast available for {city!r} on {date!r}. "
            f"Known cities: {', '.join(known_cities())}. "
            f"Dates available: 2026-04-17, 2026-04-18, 2026-04-19."
        )
    return (
        f"{forecast.city} on {forecast.date}: {forecast.condition}. "
        f"High {forecast.high_c}°C / low {forecast.low_c}°C, "
        f"precipitation {forecast.precipitation_mm} mm."
    )


def build_agent(model_name: str) -> Any:
    llm = init_chat_model(model_name)
    return create_agent(
        model=llm,
        tools=[get_weather],
        system_prompt=SYSTEM_PROMPT_TEMPLATE.format(today=date.today().isoformat()),
    )


def run_agent(
    *,
    agent: Any,
    agento11y: Client,
    user_message: str,
    conversation_id: str,
) -> str:
    """Invoke the agent with agento11y callbacks attached, return the final reply."""
    config = with_agento11y_langchain_callbacks(
        {"metadata": {"conversation_id": conversation_id}},
        client=agento11y,
        provider_resolver="auto",
        agent_name="weather-agent",
        agent_version="1.0.0",
    )

    result = agent.invoke(
        {"messages": [{"role": "user", "content": user_message}]},
        config=config,
    )

    return _final_text(result)


def _final_text(result: Any) -> str:
    """Extract the assistant's final text reply from a LangChain agent result."""
    messages = result.get("messages", []) if isinstance(result, dict) else []
    for msg in reversed(messages):
        content = getattr(msg, "content", None)
        if isinstance(content, str) and content.strip():
            return content
        if isinstance(content, list):
            parts = [part.get("text", "") for part in content if isinstance(part, dict) and part.get("type") == "text"]
            joined = "".join(parts).strip()
            if joined:
                return joined
    return ""
