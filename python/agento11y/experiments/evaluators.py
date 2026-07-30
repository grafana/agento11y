"""Locally configured evaluator helpers for experiment trials."""

from __future__ import annotations

import json
import re
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any, Protocol

from ..models import TokenUsage
from .types import Evaluator, EvaluatorKind

JudgeInvoke = Callable[[str], Any]
JudgeParser = Callable[[str], tuple[float, bool, str]]
JudgeUsageExtractor = Callable[[Any], TokenUsage | None]

DEFAULT_LLM_JUDGE_PROMPT = """Grade the candidate output against the input and expected result.

Input:
{input}

Expected:
{expected}

Candidate output:
{output}

Return only JSON with this shape:
{"score": <number from 0 to 1>, "passed": <boolean>, "explanation": "<brief reason>"}
"""


@dataclass(frozen=True, slots=True)
class GraderGeneration:
    """A grader request/response that can be published as a generation."""

    input: str
    output: str
    model_provider: str
    model_name: str
    agent_name: str = "agento11y-llm-judge"
    agent_version: str = ""
    operation_name: str = "evaluate"
    usage: TokenUsage | None = None


@dataclass(frozen=True, slots=True)
class EvaluationResult:
    """A local evaluator result ready to attach to a trial."""

    evaluator: Evaluator
    value: float | bool | str
    passed: bool
    explanation: str = ""
    score_key: str = "final"
    metadata: dict[str, Any] = field(default_factory=dict)
    grader: GraderGeneration | None = None


class OutputEvaluator(Protocol):
    """Evaluator that scores caller-supplied output without a platform definition."""

    evaluator: Evaluator

    def evaluate_output(self, *, input: Any, output: Any, expected: Any = None) -> EvaluationResult: ...


@dataclass(slots=True)
class LLMJudge:
    """Provider-neutral LLM judge with automatic structured-result parsing.

    ``invoke`` receives the rendered prompt and may return a string or an object
    with a ``content`` attribute, which covers common provider and framework
    clients. No platform evaluator or control-plane credential is required.
    """

    evaluator_id: str
    invoke: JudgeInvoke
    model_name: str
    prompt_template: str = DEFAULT_LLM_JUDGE_PROMPT
    model_provider: str = ""
    version: str = "1"
    score_key: str = "final"
    pass_threshold: float = 0.5
    parser: JudgeParser | None = None
    usage_extractor: JudgeUsageExtractor | None = None
    agent_name: str = "agento11y-llm-judge"
    agent_version: str = ""
    operation_name: str = "llm-judge"
    evaluator: Evaluator = field(init=False)

    def __post_init__(self) -> None:
        if not self.evaluator_id.strip():
            raise ValueError("evaluator_id is required")
        if not callable(self.invoke):
            raise ValueError("invoke must be callable")
        if not self.model_name.strip():
            raise ValueError("model_name is required")
        if not self.prompt_template.strip():
            raise ValueError("prompt_template is required")
        if not 0.0 <= self.pass_threshold <= 1.0:
            raise ValueError("pass_threshold must be between 0 and 1")
        if self.usage_extractor is not None and not callable(self.usage_extractor):
            raise ValueError("usage_extractor must be callable")
        self.evaluator = Evaluator(
            evaluator_id=self.evaluator_id,
            version=self.version,
            kind=EvaluatorKind.LLM_JUDGE.value,
        )

    def evaluate_output(self, *, input: Any, output: Any, expected: Any = None) -> EvaluationResult:
        """Grades explicit input/output values; it does not fetch a conversation."""

        prompt = _render_prompt(self.prompt_template, input=input, output=output, expected=expected)
        response = self.invoke(prompt)
        raw = _response_text(response)
        usage = self.usage_extractor(response) if self.usage_extractor is not None else _response_usage(response)
        score, passed, explanation = self.parser(raw) if self.parser is not None else self._parse_default(raw)
        metadata = {
            "judge_model": self.model_name,
            "judge_provider": self.model_provider,
        }
        return EvaluationResult(
            evaluator=self.evaluator,
            value=score,
            passed=passed,
            explanation=explanation,
            score_key=self.score_key,
            metadata={key: value for key, value in metadata.items() if value},
            grader=GraderGeneration(
                input=prompt,
                output=raw,
                model_provider=self.model_provider,
                model_name=self.model_name,
                agent_name=self.agent_name,
                agent_version=self.agent_version or self.version,
                operation_name=self.operation_name,
                usage=usage,
            ),
        )

    def _parse_default(self, raw: str) -> tuple[float, bool, str]:
        objects = _json_objects(raw)
        if not objects:
            raise ValueError("LLM judge response did not contain a JSON object")
        for payload in reversed(objects):
            try:
                score = max(0.0, min(1.0, float(payload["score"])))
            except (KeyError, TypeError, ValueError):
                continue
            passed = _parse_passed(payload.get("passed", payload.get("pass")), score, self.pass_threshold)
            explanation = str(payload.get("explanation", payload.get("reason", ""))).strip()
            return score, passed, explanation
        raise ValueError("LLM judge response requires a numeric 'score'")


@dataclass(slots=True)
class RegexJudge:
    """Deterministic regular-expression evaluator for candidate output."""

    evaluator_id: str
    pattern: str
    version: str = "1"
    score_key: str = "regex_match"
    flags: int = 0
    full_match: bool = False
    negate: bool = False
    explanation: str = ""
    evaluator: Evaluator = field(init=False)
    _compiled: re.Pattern[str] = field(init=False, repr=False)

    def __post_init__(self) -> None:
        if not self.evaluator_id.strip():
            raise ValueError("evaluator_id is required")
        if not self.pattern:
            raise ValueError("pattern is required")
        self._compiled = re.compile(self.pattern, self.flags)
        self.evaluator = Evaluator(
            evaluator_id=self.evaluator_id,
            version=self.version,
            kind=EvaluatorKind.DETERMINISTIC.value,
        )

    def evaluate_output(self, *, input: Any, output: Any, expected: Any = None) -> EvaluationResult:
        del input, expected
        text = str(output)
        matched = bool(self._compiled.fullmatch(text) if self.full_match else self._compiled.search(text))
        passed = not matched if self.negate else matched
        explanation = self.explanation or (
            f"output {'did not match' if self.negate else 'matched'} /{self.pattern}/"
            if passed
            else f"output {'matched excluded' if self.negate else 'did not match'} /{self.pattern}/"
        )
        return EvaluationResult(
            evaluator=self.evaluator,
            value=passed,
            passed=passed,
            explanation=explanation,
            score_key=self.score_key,
            metadata={"pattern": self.pattern},
        )


def _response_text(response: Any) -> str:
    content = getattr(response, "content", response)
    if isinstance(content, str):
        return content
    if isinstance(content, dict):
        return json.dumps(content)
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, str):
                parts.append(item)
            elif isinstance(item, dict) and isinstance(item.get("text"), str):
                parts.append(item["text"])
            elif isinstance(getattr(item, "text", None), str):
                parts.append(item.text)
        if parts:
            return "".join(parts)
    return str(content)


def _response_usage(response: Any) -> TokenUsage | None:
    """Extracts common SDK and framework token-usage shapes without owning them."""

    response_metadata = _field(response, "response_metadata")
    candidates = [
        _field(response, "usage_metadata"),
        _field(response, "usage"),
        _field(response_metadata, "usage"),
        _field(response_metadata, "token_usage"),
    ]
    for candidate in candidates:
        if candidate is None:
            continue
        input_details = _field(candidate, "input_token_details")
        output_details = _field(candidate, "output_token_details")
        values = {
            "input_tokens": _first_int(candidate, "input_tokens", "prompt_tokens"),
            "output_tokens": _first_int(candidate, "output_tokens", "completion_tokens"),
            "total_tokens": _first_int(candidate, "total_tokens"),
            "cache_read_input_tokens": _first_int(candidate, "cache_read_input_tokens", "cache_read_tokens")
            or _first_int(input_details, "cache_read", "cache_read_tokens"),
            "cache_write_input_tokens": _first_int(
                candidate,
                "cache_write_input_tokens",
                "cache_creation_input_tokens",
                "cache_creation_tokens",
            )
            or _first_int(input_details, "cache_write", "cache_creation", "cache_creation_tokens"),
            "reasoning_tokens": _first_int(candidate, "reasoning_tokens")
            or _first_int(output_details, "reasoning", "reasoning_tokens"),
        }
        if any(value is not None for value in values.values()):
            return TokenUsage(**{key: value or 0 for key, value in values.items()}).normalize()
    return None


def _json_objects(raw: str) -> list[dict[str, Any]]:
    """Extracts complete JSON objects without merging unrelated brace ranges."""

    decoder = json.JSONDecoder()
    objects: list[dict[str, Any]] = []
    cursor = 0
    while True:
        start = raw.find("{", cursor)
        if start < 0:
            break
        try:
            value, end = decoder.raw_decode(raw, start)
        except json.JSONDecodeError:
            cursor = start + 1
            continue
        if isinstance(value, dict):
            objects.append(value)
        cursor = max(end, start + 1)
    return objects


def _field(value: Any, name: str) -> Any:
    if isinstance(value, dict):
        return value.get(name)
    return getattr(value, name, None)


def _first_int(value: Any, *names: str) -> int | None:
    for name in names:
        raw = _field(value, name)
        if raw is None or isinstance(raw, bool):
            continue
        try:
            parsed = int(raw)
        except (TypeError, ValueError):
            continue
        if parsed >= 0:
            return parsed
    return None


def _render_prompt(template: str, *, input: Any, output: Any, expected: Any) -> str:
    """Replaces judge placeholders without treating JSON braces as formatting."""

    values = {"input": str(input), "output": str(output), "expected": str(expected)}
    return re.sub(r"\{(input|output|expected)\}", lambda match: values[match.group(1)], template)


def _parse_passed(value: Any, score: float, threshold: float) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        normalized = value.strip().lower()
        if normalized in {"true", "yes", "1", "pass", "passed"}:
            return True
        if normalized in {"false", "no", "0", "fail", "failed"}:
            return False
    return score >= threshold
