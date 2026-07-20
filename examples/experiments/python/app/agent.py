"""Tiny Anthropic-backed agent and grader used by the experiment example."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any

from anthropic import Anthropic
from agento11y.models import TokenUsage
from agento11y.usage import from_anthropic


@dataclass(frozen=True)
class ModelCall:
    text: str
    model: str
    response_id: str
    stop_reason: str
    usage: TokenUsage


@dataclass(frozen=True)
class Grade:
    score: float
    passed: bool
    explanation: str
    prompt: str
    call: ModelCall


def answer_question(question: str) -> ModelCall:
    """Runs the candidate agent."""

    model = os.environ.get("AGENT_MODEL", "claude-3-5-haiku-latest")
    response = Anthropic().messages.create(
        model=model,
        max_tokens=256,
        messages=[{"role": "user", "content": question}],
    )
    return ModelCall(
        text=_message_text(response),
        model=model,
        response_id=str(getattr(response, "id", "")),
        stop_reason=str(getattr(response, "stop_reason", "")),
        usage=from_anthropic(getattr(response, "usage", None)),
    )


def grade_answer(*, question: str, expected: str, actual: str) -> Grade:
    """Uses an LLM grader and returns a parsed final score."""

    model = os.environ.get("GRADER_MODEL", os.environ.get("AGENT_MODEL", "claude-3-5-haiku-latest"))
    prompt = f"""Grade the candidate answer against the expected answer.

Return only JSON with this shape:
{{"passed": true, "score": 1.0, "explanation": "short reason"}}

Question:
{question}

Expected answer:
{expected}

Candidate answer:
{actual}
"""
    response = Anthropic().messages.create(
        model=model,
        max_tokens=256,
        messages=[{"role": "user", "content": prompt}],
    )
    call = ModelCall(
        text=_message_text(response),
        model=model,
        response_id=str(getattr(response, "id", "")),
        stop_reason=str(getattr(response, "stop_reason", "")),
        usage=from_anthropic(getattr(response, "usage", None)),
    )
    parsed = _parse_grade(call.text)
    return Grade(
        score=parsed["score"],
        passed=parsed["passed"],
        explanation=parsed["explanation"],
        prompt=prompt,
        call=call,
    )


def _message_text(response: Any) -> str:
    parts: list[str] = []
    for block in getattr(response, "content", []) or []:
        if getattr(block, "type", "") == "text":
            parts.append(str(getattr(block, "text", "")))
    return "\n".join(part for part in parts if part).strip()


def _parse_grade(text: str) -> dict[str, Any]:
    try:
        payload = json.loads(text)
    except json.JSONDecodeError:
        start = text.find("{")
        end = text.rfind("}")
        payload = {}
        if start >= 0 and end > start:
            try:
                payload = json.loads(text[start : end + 1])
            except json.JSONDecodeError:
                payload = {}

    score = float(payload.get("score", 0.0))
    score = max(0.0, min(1.0, score))
    return {
        "score": score,
        "passed": bool(payload.get("passed", score >= 0.5)),
        "explanation": str(payload.get("explanation", "")).strip() or "graded by LLM judge",
    }
