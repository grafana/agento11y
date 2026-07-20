"""Anthropic-backed dashboard spec generator and grader."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any

from anthropic import Anthropic
from agento11y.usage import from_anthropic

from app.agent import ModelCall


@dataclass(frozen=True)
class DashboardCase:
    id: str
    prompt: str
    data: dict[str, Any]
    required: list[str]


@dataclass(frozen=True)
class DashboardGrade:
    score: float
    passed: bool
    explanation: str
    prompt: str
    call: ModelCall


def build_dashboard_spec(case: DashboardCase) -> ModelCall:
    """Asks the candidate model for a compact JSON dashboard spec."""

    model = os.environ.get("AGENT_MODEL", "claude-3-5-haiku-latest")
    prompt = f"""Create a dashboard chart spec for this request.

Return only JSON with this shape:
{{
  "title": "short dashboard title",
  "subtitle": "short context",
  "chart_type": "line or bar",
  "x_label": "x axis label",
  "y_label": "y axis label",
  "series": [
    {{"name": "series name", "values": [1, 2, 3]}}
  ],
  "x_labels": ["label 1", "label 2"],
  "threshold": 250
}}

Request:
{case.prompt}

Required elements:
{json.dumps(case.required)}

Data:
{json.dumps(case.data)}
"""
    response = Anthropic().messages.create(
        model=model,
        max_tokens=700,
        messages=[{"role": "user", "content": prompt}],
    )
    return _model_call(response, model)


def grade_dashboard(*, case: DashboardCase, spec_text: str) -> DashboardGrade:
    """Grades the generated dashboard spec before rendering."""

    model = os.environ.get("GRADER_MODEL", os.environ.get("AGENT_MODEL", "claude-3-5-haiku-latest"))
    prompt = f"""Grade this dashboard spec against the request.

Return only JSON with this shape:
{{"passed": true, "score": 1.0, "explanation": "short reason"}}

Request:
{case.prompt}

Required elements:
{json.dumps(case.required)}

Source data:
{json.dumps(case.data)}

Dashboard spec:
{spec_text}
"""
    response = Anthropic().messages.create(
        model=model,
        max_tokens=300,
        messages=[{"role": "user", "content": prompt}],
    )
    call = _model_call(response, model)
    parsed = _parse_json(call.text)
    score = max(0.0, min(1.0, float(parsed.get("score", 0.0))))
    return DashboardGrade(
        score=score,
        passed=bool(parsed.get("passed", score >= 0.7)),
        explanation=str(parsed.get("explanation", "")).strip() or "graded by dashboard judge",
        prompt=prompt,
        call=call,
    )


def parse_dashboard_spec(text: str) -> dict[str, Any]:
    """Parses and lightly normalizes the model's JSON chart spec."""

    spec = _parse_json(text)
    series = spec.get("series") if isinstance(spec.get("series"), list) else []
    normalized_series: list[dict[str, Any]] = []
    for item in series:
        if not isinstance(item, dict):
            continue
        values = item.get("values") if isinstance(item.get("values"), list) else []
        normalized_series.append(
            {
                "name": str(item.get("name", "series")),
                "values": [float(value) for value in values if isinstance(value, (int, float))],
            }
        )
    return {
        "title": str(spec.get("title", "Generated dashboard")),
        "subtitle": str(spec.get("subtitle", "")),
        "chart_type": str(spec.get("chart_type", "line")).lower(),
        "x_label": str(spec.get("x_label", "")),
        "y_label": str(spec.get("y_label", "")),
        "x_labels": [str(label) for label in spec.get("x_labels", []) if label is not None],
        "series": normalized_series,
        "threshold": spec.get("threshold"),
    }


def _model_call(response: Any, model: str) -> ModelCall:
    return ModelCall(
        text=_message_text(response),
        model=model,
        response_id=str(getattr(response, "id", "")),
        stop_reason=str(getattr(response, "stop_reason", "")),
        usage=from_anthropic(getattr(response, "usage", None)),
    )


def _message_text(response: Any) -> str:
    parts: list[str] = []
    for block in getattr(response, "content", []) or []:
        if getattr(block, "type", "") == "text":
            parts.append(str(getattr(block, "text", "")))
    return "\n".join(part for part in parts if part).strip()


def _parse_json(text: str) -> dict[str, Any]:
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
    return payload if isinstance(payload, dict) else {}
