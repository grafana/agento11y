"""Direct Anthropic call, instrumented with the raw Sigil SDK.

Counterpoint to `agent.py`. When you're not using a framework Sigil
has a callback for, the pattern is:

    with agento11y.start_generation(GenerationStart(...)) as rec:
        response = provider_call(...)
        rec.set_result(input=..., output=..., usage=...)

The classifier decides whether the message is a weather question so the
caller can skip the agent on OFF_TOPIC. Both calls share a
`conversation_id` so they group together in the Sigil UI.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Literal

from anthropic import Anthropic
from anthropic.types import TextBlock

from agento11y import (
    Client,
    GenerationStart,
    ModelRef,
    TokenUsage,
    assistant_text_message,
    user_text_message,
)


Label = Literal["ON_TOPIC", "OFF_TOPIC", "UNKNOWN"]


@dataclass(frozen=True)
class Classification:
    label: Label
    raw: str


_PROMPT_TEMPLATE = """\
You are a strict classifier. Decide whether the user's message is asking about WEATHER.
Output exactly one token: ON_TOPIC if it's a weather/forecast question, OFF_TOPIC otherwise.
Do not explain.

User message:
{user_message}

Label:"""


def _parse_label(raw: str) -> Label:
    head = raw.strip().upper()
    if head.startswith("ON_TOPIC"):
        return "ON_TOPIC"
    if head.startswith("OFF_TOPIC"):
        return "OFF_TOPIC"
    return "UNKNOWN"


def classify_message(
    *,
    agento11y: Client,
    conversation_id: str,
    user_message: str,
    model_name: str,
) -> Classification:
    anthropic = Anthropic(api_key=os.environ["ANTHROPIC_API_KEY"])
    prompt = _PROMPT_TEMPLATE.format(user_message=user_message)

    raw_text = ""
    with agento11y.start_generation(
        GenerationStart(
            conversation_id=conversation_id,
            agent_name="topic-classifier",
            agent_version="1.0.0",
            model=ModelRef(provider="anthropic", name=model_name),
            max_tokens=8,
            temperature=0,
        )
    ) as rec:
        try:
            response = anthropic.messages.create(
                model=model_name,
                max_tokens=8,
                temperature=0,
                messages=[{"role": "user", "content": prompt}],
            )
        except Exception as exc:
            rec.set_call_error(exc)
            raise

        raw_text = "".join(
            block.text for block in response.content if isinstance(block, TextBlock)
        ).strip()

        rec.set_result(
            input=[user_text_message(prompt)],
            output=[assistant_text_message(raw_text)],
            usage=TokenUsage(
                input_tokens=response.usage.input_tokens,
                output_tokens=response.usage.output_tokens,
            ),
            response_id=response.id,
            response_model=response.model,
        )

    return Classification(label=_parse_label(raw_text), raw=raw_text)
