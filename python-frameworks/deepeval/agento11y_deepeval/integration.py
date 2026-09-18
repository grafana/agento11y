"""DeepEval-to-Agent-Observability experiment adapter."""

from __future__ import annotations

import json
import math
import re
import time
from collections.abc import Callable, Iterable, Mapping, Sequence
from copy import deepcopy
from dataclasses import dataclass, fields, is_dataclass
from enum import Enum
from typing import Any

from agento11y import Message, assistant_text_message, user_text_message
from agento11y.experiments import (
    Candidate,
    Client,
    Evaluator,
    ReportRole,
    TestCase,
    TestSuite,
    TestSuitesClient,
    experiment,
    stable_id,
)


@dataclass(frozen=True, slots=True)
class PublishedDeepEvalRun:
    """Identity and counts for one published experiment."""

    experiment_id: str
    url: str
    trial_count: int
    score_count: int


@dataclass(frozen=True, slots=True)
class DeepEvalRun:
    """DeepEval's result together with its Agent Observability run."""

    evaluation_result: Any
    published: PublishedDeepEvalRun


def run_deepeval(
    test_cases: Sequence[Any],
    target: Callable[[Any, Any], str | Sequence[Any]],
    metrics: Sequence[Any],
    *,
    experiment_name: str,
    primary_metric: str | None = None,
    experiment_id: str = "",
    suite_id: str = "deepeval",
    suite_version: str = "1",
    client: Client | None = None,
    candidate: Candidate | Mapping[str, Any] | None = None,
    record_io: bool = False,
) -> PublishedDeepEvalRun:
    """Invoke target(native_case, trial) and DeepEval metrics inside live trials.

    A single-turn target returns its output string. A conversational target returns
    the completed ordered sequence of public DeepEval ``Turn`` objects. The target
    owns its instrumentation and may bind a conversation on ``trial``. Cases and
    metrics are copied per attempt; caller-owned objects are not mutated.
    """
    from deepeval.metrics.utils import copy_metrics

    if not test_cases or not metrics:
        raise ValueError("test_cases and metrics must not be empty")
    names = [str(metric.__name__) for metric in metrics]
    selected = primary_metric or (names[0] if len(names) == 1 else None)
    if selected not in names or len(set(names)) != len(names):
        raise ValueError("select a primary_metric from unique metric names")
    cases = [_test_case(case, index, suite_id) for index, case in enumerate(test_cases)]
    _validate_case_definitions(cases)
    suite = TestSuite(suite_id=suite_id, version=suite_version, test_cases=cases, tags=["framework:deepeval"])
    attempts: dict[str, int] = {}
    score_count = 0
    with experiment(
        experiment_name,
        experiment_id=experiment_id,
        suite=suite,
        client=client,
        candidate=dict(candidate) if isinstance(candidate, Mapping) else candidate,
        planned_trial_count=len(cases),
        use_experimental_otel=True,
        metadata={"framework": "deepeval", "execution_mode": "live"},
    ) as exp:
        for native, case in zip(test_cases, cases, strict=True):
            attempts[case.test_case_id] = attempts.get(case.test_case_id, 0) + 1
            with exp.trial(case, attempt=attempts[case.test_case_id]) as trial:
                current = deepcopy(native)
                started = time.perf_counter()
                try:
                    target_result = target(current, trial)
                    if _is_conversational(current):
                        if isinstance(target_result, (str, bytes)) or not isinstance(target_result, Sequence):
                            raise TypeError("conversational target must return an ordered sequence of DeepEval turns")
                        current.turns = deepcopy(list(target_result))
                        turns = _conversation_turns(current, require_assistant=True)
                    else:
                        current.actual_output = target_result
                        if not isinstance(current.actual_output, str):
                            raise TypeError("single-turn target must return a string")
                except Exception as error:
                    trial.mark_errored(str(error) or type(error).__name__)
                    continue
                finally:
                    trial.set_duration(int((time.perf_counter() - started) * 1000))
                if record_io and _is_conversational(current):
                    _bind_or_publish_conversation(
                        exp.client,
                        trial,
                        turns,
                        conversation_id=trial.conversation_id,
                        candidate=candidate,
                        publish=not bool(trial.conversation_id),
                    )
                elif record_io:
                    trial.record_io(input=current.input, output=current.actual_output)
                for template, name in zip(metrics, names, strict=True):
                    try:
                        metric = copy_metrics([template])[0]
                        metric.measure(current)
                        if getattr(metric, "error", None):
                            raise RuntimeError(metric.error)
                        passed = metric.is_successful()
                    except Exception as error:
                        trial.mark_errored(f"{name}: {error}")
                        continue
                    trial.score(
                        _score_key(name),
                        _metric_value(metric),
                        passed=passed,
                        report_role=ReportRole.PRIMARY_VERDICT if name == selected else ReportRole.DIAGNOSTIC,
                        evaluator=Evaluator(
                            f"deepeval.{_score_key(name)}",
                            version=_metric_version(metric),
                            kind="llm_judge" if getattr(metric, "evaluation_model", None) else "custom",
                        ),
                        explanation=str(getattr(metric, "reason", "") or ""),
                        metadata=_metric_metadata(metric),
                    )
                    score_count += 1
    return PublishedDeepEvalRun(exp.experiment_id, exp.url, len(cases), score_count)


def evaluate_with_agento11y(
    test_cases: Sequence[Any],
    metrics: Sequence[Any],
    *,
    experiment_name: str,
    primary_metric: str | None = None,
    experiment_id: str = "",
    suite_id: str = "deepeval",
    suite_version: str = "1",
    client: Client | None = None,
    candidate: Candidate | Mapping[str, Any] | None = None,
    record_io: bool = True,
    deepeval_options: Mapping[str, Any] | None = None,
    test_suites_client: TestSuitesClient | None = None,
    suite_publication_policy: str | None = None,
) -> DeepEvalRun:
    """Runs ``deepeval.evaluate`` and publishes its returned public result."""

    from deepeval import evaluate as deepeval_evaluate

    result = deepeval_evaluate(test_cases=list(test_cases), metrics=list(metrics), **dict(deepeval_options or {}))
    published = publish_deepeval_results(
        result,
        experiment_name=experiment_name,
        primary_metric=primary_metric,
        experiment_id=experiment_id,
        suite_id=suite_id,
        suite_version=suite_version,
        client=client,
        candidate=candidate,
        record_io=record_io,
        test_suites_client=test_suites_client,
        suite_publication_policy=suite_publication_policy,
    )
    return DeepEvalRun(evaluation_result=result, published=published)


def publish_deepeval_results(
    evaluation_result: Any,
    *,
    experiment_name: str,
    primary_metric: str | None = None,
    experiment_id: str = "",
    suite_id: str = "deepeval",
    suite_version: str = "1",
    client: Client | None = None,
    candidate: Candidate | Mapping[str, Any] | None = None,
    record_io: bool = True,
    metadata: Mapping[str, Any] | None = None,
    resolve_conversation_id: Callable[[Any], str | None] | None = None,
    resolve_generation_id: Callable[[Any], str | None] | None = None,
    test_suites_client: TestSuitesClient | None = None,
    suite_publication_policy: str | None = None,
) -> PublishedDeepEvalRun:
    """Publishes DeepEval's public ``EvaluationResult``.

    One DeepEval test result becomes one trial. The selected metric is marked
    ``primary_verdict`` and every other metric is marked ``diagnostic``. If the
    result contains more than one metric name, ``primary_metric`` is required.
    """

    results = list(getattr(evaluation_result, "test_results", None) or [])
    if not results:
        raise ValueError("DeepEval produced no test results to publish")
    selected_metric = _select_primary_metric(results, primary_metric)
    _validate_primary_metric(results, selected_metric)
    bindings = [
        (
            _nonblank(resolve_conversation_id(result) if resolve_conversation_id is not None else None)
            or _metadata_string(_deepeval_metadata(result), "agento11y.conversation_id"),
            _nonblank(resolve_generation_id(result) if resolve_generation_id is not None else None)
            or _metadata_string(_deepeval_metadata(result), "agento11y.generation_id"),
        )
        for result in results
    ]
    for index, (result, (conversation_id, generation_id)) in enumerate(zip(results, bindings, strict=True)):
        if _is_conversational(result) and generation_id and not conversation_id:
            raise ValueError(
                f"DeepEval test result {index} supplies a generation ID without the conversation ID "
                "required for a conversational verdict"
            )
    cases = [_test_case(result, index, suite_id) for index, result in enumerate(results)]
    _validate_case_definitions(cases)
    executed_cases = deepcopy(cases)
    suite = TestSuite(
        suite_id=suite_id,
        name="DeepEval evaluation",
        version=suite_version,
        tags=["framework:deepeval"],
        test_cases=cases,
    )
    if test_suites_client is not None:
        if suite_publication_policy not in {"subset", "replace"}:
            raise ValueError("suite_publication_policy must be 'subset' or 'replace' when publishing a suite")
        pushed = test_suites_client.push_suite(
            suite,
            publish=True,
            prune=suite_publication_policy == "replace",
        )
        canonical = {case.test_case_id: case for case in pushed.suite.cases}
        cases = [_canonical_case(canonical.get(case.test_case_id), case) for case in executed_cases]
        suite = pushed.suite
    score_count = 0
    attempts: dict[str, int] = {}
    run_url = ""
    run_id = experiment_id or str(getattr(evaluation_result, "test_run_id", "") or "")
    run_metadata = {
        "framework": "deepeval",
        "deepeval_test_run_id": str(getattr(evaluation_result, "test_run_id", "") or ""),
        "suite_publication_policy": suite_publication_policy or "local",
        "executed_case_count": len({case.test_case_id for case in cases}),
        **dict(metadata or {}),
    }
    with experiment(
        experiment_name,
        experiment_id=run_id,
        suite=suite,
        client=client,
        candidate=dict(candidate) if isinstance(candidate, Mapping) else candidate,
        planned_trial_count=len(results),
        metadata=run_metadata,
    ) as exp:
        for result, case, executed_case, (conversation_id, generation_id) in zip(
            results, cases, executed_cases, bindings, strict=True
        ):
            attempts[case.test_case_id] = attempts.get(case.test_case_id, 0) + 1
            with exp.trial(
                case,
                attempt=attempts[case.test_case_id],
                metadata=_case_provenance(executed_case, case),
            ) as trial:
                result_metadata = _deepeval_metadata(result)
                if _is_conversational(result):
                    turns = _conversation_turns(result, require_assistant=not _has_deepeval_error(result))
                    if conversation_id or record_io:
                        _bind_or_publish_conversation(
                            exp.client,
                            trial,
                            turns,
                            conversation_id=conversation_id,
                            candidate=candidate,
                            publish=record_io and not bool(conversation_id),
                        )
                elif generation_id:
                    trial.bind_generation(generation_id, conversation_id=conversation_id or "")
                elif record_io:
                    if conversation_id:
                        trial.bind_conversation(conversation_id)
                    trial.record_io(
                        input=getattr(result, "input", None),
                        output=getattr(result, "actual_output", None),
                        agent_name="deepeval-target",
                    )
                elif conversation_id:
                    trial.bind_conversation(conversation_id)
                trace_id = _metadata_string(result_metadata, "agento11y.trace_id")
                if trace_id:
                    trial.bind_trace(trace_id)

                trial.set_duration(_duration_ms(result_metadata))
                metric_errors: list[str] = []
                result_error = _nonblank(str(getattr(result, "error", "") or ""))
                if result_error:
                    metric_errors.append(result_error)

                for metric in list(getattr(result, "metrics_data", None) or []):
                    name = str(getattr(metric, "name", "") or "").strip()
                    if not name:
                        continue
                    metric_error = _nonblank(str(getattr(metric, "error", "") or ""))
                    if metric_error:
                        metric_errors.append(f"{name}: {metric_error}")
                        continue
                    is_primary = name == selected_metric
                    passed = _metric_passed(metric)
                    value = _metric_value(metric)
                    metric_metadata = _metric_metadata(metric)
                    trial.score(
                        _score_key(name),
                        value,
                        evaluator=Evaluator(
                            evaluator_id=f"deepeval.{_score_key(name)}",
                            version=_metric_version(metric),
                            kind=("llm_judge" if getattr(metric, "evaluation_model", None) else "custom"),
                        ),
                        passed=passed,
                        report_role=(ReportRole.PRIMARY_VERDICT if is_primary else ReportRole.DIAGNOSTIC),
                        explanation=str(getattr(metric, "reason", None) or getattr(metric, "error", None) or ""),
                        metadata={"framework": "deepeval", "deepeval_metric": name, **metric_metadata},
                    )
                    score_count += 1
                if metric_errors:
                    trial.mark_errored("; ".join(metric_errors))
            run_url = exp.url
        run_id = exp.experiment_id
    return PublishedDeepEvalRun(
        experiment_id=run_id,
        url=run_url,
        trial_count=len(results),
        score_count=score_count,
    )


def _select_primary_metric(results: Iterable[Any], requested: str | None) -> str:
    requested = _nonblank(requested)
    names = {
        str(getattr(metric, "name", "") or "").strip()
        for result in results
        for metric in list(getattr(result, "metrics_data", None) or [])
        if str(getattr(metric, "name", "") or "").strip()
    }
    if requested:
        if requested not in names:
            raise ValueError(f"primary_metric {requested!r} was not present in the DeepEval results")
        return requested
    if len(names) == 1:
        return next(iter(names))
    if not names:
        raise ValueError("DeepEval results contain no metrics")
    raise ValueError("primary_metric is required when DeepEval returns multiple metrics: " + ", ".join(sorted(names)))


def _validate_primary_metric(results: Iterable[Any], primary_metric: str) -> None:
    for index, result in enumerate(results):
        has_error = _has_deepeval_error(result)
        if _is_conversational(result):
            _conversation_turns(result, index=index, require_assistant=not has_error)
        names = [
            str(getattr(metric, "name", "") or "").strip() for metric in getattr(result, "metrics_data", None) or []
        ]
        names = [name for name in names if name]
        if len(names) != len(set(names)):
            raise ValueError(f"DeepEval test result {index} contains duplicate metric names")
        if primary_metric not in names and not has_error:
            raise ValueError(f"DeepEval test result {index} does not contain primary metric {primary_metric!r}")


def _test_case(result: Any, index: int, suite_id: str) -> TestCase:
    result_metadata = _deepeval_metadata(result)
    explicit_id = _metadata_string(result_metadata, "agento11y.test_case_id")
    if _is_conversational(result):
        turns = _conversation_turns(result, index=index)
        case_input: dict[str, Any] = {"turns": turns}
        expected: dict[str, Any] = {}
        for field_name in ("scenario", "user_description"):
            value = getattr(result, field_name, None)
            if value is not None:
                case_input[field_name] = _json_value(value, f"DeepEval test result {index}.{field_name}")
        for field_name in ("expected_outcome", "context", "chatbot_role"):
            value = getattr(result, field_name, None)
            if value is not None:
                expected[field_name] = _json_value(value, f"DeepEval test result {index}.{field_name}")
        identity_input = {**case_input, "turns": [turn for turn in turns if turn["role"] == "user"]}
    else:
        case_input = {"input": getattr(result, "input", None)}
        expected = {
            "output": getattr(result, "expected_output", None),
            "context": getattr(result, "context", None),
            "retrieval_context": getattr(result, "retrieval_context", None),
        }
        expected = {key: value for key, value in expected.items() if value is not None}
        identity_input = case_input
    test_case_id = explicit_id or stable_id(
        "case",
        suite_id,
        getattr(result, "name", ""),
        identity_input,
        expected,
    )
    return TestCase(
        test_case_id=test_case_id,
        name=str(getattr(result, "name", "") or test_case_id),
        input=case_input,
        expected=expected,
        metadata={"framework": "deepeval", "deepeval_index": index, **result_metadata},
    )


_TURN_FIELDS = (
    "order",
    "user_id",
    "audio",
    "latency_ms",
    "interrupted",
    "retrieval_context",
    "tools_called",
    "mcp_tools_called",
    "mcp_resources_called",
    "mcp_prompts_called",
    "metadata",
    "comments",
)


def _is_conversational(value: Any) -> bool:
    return bool(getattr(value, "conversational", False)) or (
        hasattr(value, "turns") and not hasattr(value, "actual_output")
    )


def _conversation_turns(
    value: Any,
    *,
    index: int | None = None,
    require_assistant: bool = False,
) -> list[dict[str, Any]]:
    label = f"DeepEval test result {index}" if index is not None else "DeepEval conversational case"
    native_turns = getattr(value, "turns", None)
    if isinstance(native_turns, (str, bytes)) or not isinstance(native_turns, Sequence) or not native_turns:
        raise ValueError(f"{label} must contain an ordered, non-empty turns sequence")
    turns: list[dict[str, Any]] = []
    for turn_index, turn in enumerate(native_turns):
        role = getattr(turn, "role", None)
        content = getattr(turn, "content", None)
        if role not in {"user", "assistant"}:
            raise ValueError(f"{label} turn {turn_index} has unsupported role {role!r}")
        if not isinstance(content, str) or not content.strip():
            raise ValueError(f"{label} turn {turn_index} content must be a non-blank string")
        serialized: dict[str, Any] = {"role": role, "content": content}
        for field_name in _TURN_FIELDS:
            field_value = getattr(turn, field_name, None)
            if field_value is not None:
                if field_name == "user_id" and not isinstance(field_value, str):
                    raise ValueError(f"{label} turn {turn_index}.user_id must be a string")
                serialized[field_name] = _json_value(field_value, f"{label} turn {turn_index}.{field_name}")
        turns.append(serialized)
    if require_assistant and not any(turn["role"] == "assistant" for turn in turns):
        raise ValueError(f"{label} must contain at least one assistant turn")
    return turns


def _json_value(value: Any, path: str) -> Any:
    if value is None or isinstance(value, (str, bool, int)):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError(f"{path} must be JSON-serializable")
        return value
    if isinstance(value, Enum):
        return _json_value(value.value, path)
    if isinstance(value, Mapping):
        if not all(isinstance(key, str) for key in value):
            raise ValueError(f"{path} metadata keys must be strings")
        return {key: _json_value(item, f"{path}.{key}") for key, item in value.items()}
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
        return [_json_value(item, f"{path}[{item_index}]") for item_index, item in enumerate(value)]
    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        return _json_value(model_dump(mode="json"), path)
    if is_dataclass(value) and not isinstance(value, type):
        return {
            field.name: _json_value(getattr(value, field.name), f"{path}.{field.name}")
            for field in fields(value)
            if not field.name.startswith("_")
        }
    raise ValueError(f"{path} has unsupported public value type {type(value).__name__}")


def _bind_or_publish_conversation(
    client: Any,
    trial: Any,
    turns: Sequence[Mapping[str, Any]],
    *,
    conversation_id: str = "",
    candidate: Candidate | Mapping[str, Any] | None = None,
    publish: bool,
) -> None:
    conversation_id = conversation_id or stable_id(
        "conv",
        trial.ref.experiment_id,
        trial.ref.test_case_id,
        trial.ref.attempt,
        "deepeval",
    )
    trial.bind_conversation(conversation_id)
    if not publish:
        return

    history: list[Message] = []
    previous_generation_id = ""
    user_id = ""
    for turn_index, turn in enumerate(turns):
        content = turn["content"]
        if turn["role"] == "user":
            history.append(user_text_message(content))
            user_id = turn.get("user_id") or user_id
            continue
        generation_id = stable_id(
            "gen",
            trial.ref.experiment_id,
            trial.ref.test_case_id,
            trial.ref.attempt,
            "deepeval-turn",
            turn_index,
        )
        client.record_generation(
            generation_id,
            conversation_id=conversation_id,
            input_messages=list(history),
            output_messages=[assistant_text_message(content)],
            model_provider=_candidate_value(candidate, "model_provider") or "eval",
            model_name=_candidate_value(candidate, "model_name") or "deepeval-conversation",
            user_id=user_id,
            agent_name=_candidate_value(candidate, "agent_name") or "deepeval-target",
            agent_version=_candidate_value(candidate, "agent_version"),
            tags={
                "experiment_id": trial.ref.experiment_id,
                "test.case.id": trial.ref.test_case_id,
                "framework": "deepeval",
            },
            metadata={
                "experiment_id": trial.ref.experiment_id,
                "test_case_id": trial.ref.test_case_id,
                "trial_id": trial.trial_id,
                "attempt": trial.ref.attempt,
                "deepeval_turn_index": turn_index,
                "deepeval_turn": {key: value for key, value in turn.items() if key not in {"role", "content"}},
                "deepeval_context_turns": [
                    {
                        "turn_index": context_index,
                        **{key: value for key, value in context_turn.items() if key not in {"role", "content"}},
                    }
                    for context_index, context_turn in enumerate(turns[:turn_index])
                    if any(key not in {"role", "content"} for key in context_turn)
                ],
            },
            parent_generation_ids=[previous_generation_id] if previous_generation_id else [],
        )
        previous_generation_id = generation_id
        history.append(assistant_text_message(content))

    if turns[-1]["role"] == "user":
        turn_index = len(turns) - 1
        client.record_generation(
            stable_id(
                "gen",
                trial.ref.experiment_id,
                trial.ref.test_case_id,
                trial.ref.attempt,
                "deepeval-turn",
                turn_index,
            ),
            conversation_id=conversation_id,
            input_messages=history,
            model_provider=_candidate_value(candidate, "model_provider") or "eval",
            model_name=_candidate_value(candidate, "model_name") or "deepeval-conversation",
            user_id=user_id,
            agent_name=_candidate_value(candidate, "agent_name") or "deepeval-target",
            agent_version=_candidate_value(candidate, "agent_version"),
            tags={
                "experiment_id": trial.ref.experiment_id,
                "test.case.id": trial.ref.test_case_id,
                "framework": "deepeval",
            },
            metadata={
                "experiment_id": trial.ref.experiment_id,
                "test_case_id": trial.ref.test_case_id,
                "trial_id": trial.trial_id,
                "attempt": trial.ref.attempt,
                "deepeval_turn_index": turn_index,
                "deepeval_pending_response": True,
            },
            parent_generation_ids=[previous_generation_id] if previous_generation_id else [],
        )


def _candidate_value(candidate: Candidate | Mapping[str, Any] | None, key: str) -> str:
    if isinstance(candidate, Mapping):
        value = candidate.get(key)
    else:
        value = getattr(candidate, key, "")
    return str(value or "")


def _metric_value(metric: Any) -> float | bool:
    score = getattr(metric, "score", None)
    if isinstance(score, (int, float)) and not isinstance(score, bool) and math.isfinite(float(score)):
        return float(score)
    success = getattr(metric, "success", None)
    if isinstance(success, bool):
        return success
    return False


def _has_deepeval_error(result: Any) -> bool:
    if _nonblank(str(getattr(result, "error", "") or "")):
        return True
    return any(
        _nonblank(str(getattr(metric, "error", "") or "")) for metric in getattr(result, "metrics_data", None) or []
    )


def _metric_passed(metric: Any) -> bool | None:
    success = getattr(metric, "success", None)
    if isinstance(success, bool):
        return success
    return None


def _metric_metadata(metric: Any) -> dict[str, Any]:
    values = {
        "threshold": getattr(metric, "threshold", None),
        "strict_mode": getattr(metric, "strict_mode", None),
        "flaky": getattr(metric, "flaky", None),
        "evaluation_model": getattr(metric, "evaluation_model", None),
        "evaluation_cost": getattr(metric, "evaluation_cost", None),
        "input_tokens": getattr(metric, "input_tokens", None),
        "output_tokens": getattr(metric, "output_tokens", None),
        "error": getattr(metric, "error", None),
    }
    return {key: value for key, value in values.items() if value is not None}


def _metric_version(metric: Any) -> str:
    config = {
        key: value
        for key, value in _metric_metadata(metric).items()
        if key not in {"error", "evaluation_cost", "input_tokens", "output_tokens", "flaky"}
    }
    return stable_id("config", json.dumps(config, sort_keys=True, default=str, separators=(",", ":")))


def _duration_ms(metadata: Mapping[str, Any]) -> int | None:
    value = metadata.get("agento11y.duration_ms")
    if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0:
        return int(value)
    return None


def _validate_case_definitions(cases: Sequence[TestCase]) -> None:
    definitions: dict[str, str] = {}
    for case in cases:
        case_input = case.input
        if isinstance(case_input, Mapping) and isinstance(case_input.get("turns"), list):
            case_input = {
                **case_input,
                "turns": [turn for turn in case_input["turns"] if turn.get("role") == "user"],
            }
        digest = stable_id("case-content", case_input, case.expected)
        previous = definitions.setdefault(case.test_case_id, digest)
        if previous != digest:
            raise ValueError(f"Conflicting DeepEval definitions for case {case.test_case_id}")


def _canonical_case(canonical: TestCase | None, executed: TestCase) -> TestCase:
    return deepcopy(canonical) if canonical is not None else executed


def _case_provenance(executed: TestCase, stored: TestCase) -> dict[str, Any]:
    executed_digest = _case_digest(executed)
    stored_digest = _case_digest(stored)
    return {
        "agento11y_executed_case_digest": executed_digest,
        "agento11y_stored_case_digest": stored_digest,
        "agento11y_case_transformed": executed_digest != stored_digest,
    }


def _case_digest(case: TestCase) -> str:
    content = json.dumps(
        {"input": case.input, "expected": case.expected},
        sort_keys=True,
        default=str,
        separators=(",", ":"),
    )
    return stable_id("case-content", content)


def _score_key(name: str) -> str:
    normalized = re.sub(r"[^a-z0-9]+", "_", name.lower()).strip("_")
    return normalized or "deepeval"


def _mapping(value: Any) -> dict[str, Any]:
    return dict(value) if isinstance(value, Mapping) else {}


def _deepeval_metadata(value: Any) -> dict[str, Any]:
    if hasattr(value, "metadata"):
        return _mapping(value.metadata)
    return _mapping(getattr(value, "additional_metadata", None))


def _metadata_string(metadata: Mapping[str, Any], key: str) -> str:
    value = metadata.get(key)
    return _nonblank(value if isinstance(value, str) else None) or ""


def _nonblank(value: str | None) -> str | None:
    normalized = (value or "").strip()
    return normalized or None
