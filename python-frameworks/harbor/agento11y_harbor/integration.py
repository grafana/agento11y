"""Import completed Harbor jobs without depending on Harbor at runtime."""

from __future__ import annotations

import json
import logging
import math
from collections import defaultdict
from collections.abc import Mapping
from copy import deepcopy
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agento11y import TokenInputSemantics, TokenUsage
from agento11y.errors import ConflictError, ConflictKind
from agento11y.experiments import (
    Candidate,
    Client,
    Evaluator,
    ReportRole,
    TestCase,
    TestSuite,
    TestSuitesClient,
    TrialStatus,
    experiment,
    stable_id,
)

logger = logging.getLogger("agento11y_harbor")

ARTIFACT_MAX_BYTES = 10 * 1024 * 1024

# The SDK refuses a generation over 4 MiB, so one large tool result would fail
# the trial's whole trajectory import. Tool inputs share the agento11y plugin
# mappers' bound; each prompt and response keeps its first 1 MiB.
_TOOL_INPUT_MAX_BYTES = 4096
_TEXT_MAX_BYTES = 1 << 20

# Harbor still runs the verifier after these agent failures and counts its reward.
_GRADED_AGENT_ERRORS = frozenset({"AgentTimeoutError", "NonZeroAgentExitCodeError"})


@dataclass(frozen=True, slots=True)
class PublishedHarborRun:
    experiment_id: str
    url: str
    trial_count: int
    score_count: int
    candidate: str


@dataclass(frozen=True, slots=True)
class PublishedHarborJob:
    runs: list[PublishedHarborRun]
    trial_count: int
    score_count: int
    # Candidates whose experiment was already finalized, so they were left as they are.
    skipped: list[str] = field(default_factory=list)
    # Candidates that could not be published; the others still were.
    failed: list[str] = field(default_factory=list)


def publish_harbor_job(
    job_dir: str | Path,
    *,
    client: Client | None = None,
    test_suites_client: TestSuitesClient | None = None,
    suite_publication_policy: str | None = None,
    experiment_id: str = "",
    experiment_name: str = "",
    suite_id: str = "",
    main_reward: str = "reward",
    pass_threshold: float = 1.0,
    upload_artifacts: bool = True,
    artifact_max_bytes: int = ARTIFACT_MAX_BYTES,
) -> PublishedHarborJob:
    """Publishes every candidate in a completed Harbor job directory.

    Each agent/model/dataset candidate becomes one experiment whose ID is
    ``<experiment_id or Harbor job ID>-<candidate hash>``, the same ID the live
    plugin uses. A trial whose files can't be read is published as errored, and
    a trial whose ``result.json`` can't be read is skipped with a warning.

    Each candidate is published on its own: one that fails is listed in
    ``failed`` and the others are still published. A candidate whose
    experiment is already finalized is listed in ``skipped``, so rerunning an
    import that partly failed publishes only what's missing.
    """

    root = Path(job_dir).expanduser().resolve()
    job = _read_json(root / "result.json")
    trials = _trial_records(root)
    if not trials:
        raise ValueError(f"Harbor job contains no completed trial result.json files: {root}")
    main_reward = main_reward.strip()
    if not main_reward:
        raise ValueError("main_reward must be nonempty")
    if not math.isfinite(pass_threshold):
        raise ValueError("pass_threshold must be finite")
    if artifact_max_bytes < 0:
        raise ValueError("artifact_max_bytes must be non-negative")
    if test_suites_client is not None and suite_publication_policy not in {"subset", "replace"}:
        raise ValueError("suite_publication_policy must be 'subset' or 'replace' when publishing a suite")

    base_id = experiment_id.strip() or str(job.get("id") or root.name)
    base_name = experiment_name.strip() or f"Harbor {root.name}"
    sources = {str(result.get("source") or "adhoc") for _, result in trials}
    if suite_id.strip() and len(sources) > 1:
        raise ValueError("suite_id cannot combine multiple Harbor dataset sources")
    groups: dict[str, list[tuple[Path, dict[str, Any]]]] = defaultdict(list)
    for trial_dir, result in trials:
        groups[_group_identity(result)].append((trial_dir, result))

    prepared_suites: dict[str, tuple[TestSuite, dict[str, TestCase], dict[str, TestCase]]] = {}
    if test_suites_client is not None:
        for source in sources:
            source_trials = [
                (path, result) for path, result in trials if str(result.get("source") or "adhoc") == source
            ]
            prepared_suites[source] = _prepare_suite(
                source_trials,
                suite_id=suite_id,
                test_suites_client=test_suites_client,
                suite_publication_policy=suite_publication_policy,
            )

    runs: list[PublishedHarborRun] = []
    skipped: list[str] = []
    failed: list[str] = []
    for candidate_trials in groups.values():
        first = candidate_trials[0][1]
        run_id = _experiment_id(base_id, first)
        label = f"{_candidate_label(first)} ({run_id})"
        try:
            runs.append(
                _publish_candidate(
                    candidate_trials,
                    client=client,
                    suite_publication_policy=suite_publication_policy,
                    prepared_suite=prepared_suites.get(str(first.get("source") or "adhoc")),
                    experiment_id=run_id,
                    experiment_name=_experiment_name(base_name, first),
                    suite_id=suite_id,
                    main_reward=main_reward,
                    pass_threshold=pass_threshold,
                    upload_artifacts=upload_artifacts,
                    artifact_max_bytes=artifact_max_bytes,
                )
            )
        except _AlreadyFinalizedError as error:
            logger.warning("agento11y: %s", error)
            skipped.append(label)
        except Exception as error:
            logger.warning("agento11y: Harbor candidate %s could not be published: %s", label, error)
            failed.append(f"{label}: {type(error).__name__}: {error}")
    if not runs and skipped and not failed:
        raise ValueError(
            "Every experiment for this Harbor job is already finalized, so it cannot be imported again. "
            "Pass a new experiment_id (--experiment-id) to import it as a separate experiment."
        )
    return PublishedHarborJob(
        runs=runs,
        trial_count=sum(run.trial_count for run in runs),
        score_count=sum(run.score_count for run in runs),
        skipped=skipped,
        failed=failed,
    )


class _AlreadyFinalizedError(ValueError):
    """The candidate's experiment is finalized and can't accept this import."""


def _publish_candidate(
    trials: list[tuple[Path, dict[str, Any]]],
    *,
    client: Client | None,
    suite_publication_policy: str | None,
    prepared_suite: tuple[TestSuite, dict[str, TestCase], dict[str, TestCase]] | None,
    experiment_id: str,
    experiment_name: str,
    suite_id: str,
    main_reward: str,
    pass_threshold: float,
    upload_artifacts: bool,
    artifact_max_bytes: int,
) -> PublishedHarborRun:
    first = trials[0][1]
    if prepared_suite is None:
        suite, cases, executed_cases = _prepare_suite(trials, suite_id=suite_id)
    else:
        suite, all_cases, all_executed_cases = prepared_suite
        candidate_case_ids = {_case_id(result, suite.suite_id) for _, result in trials}
        cases = {case_id: all_cases[case_id] for case_id in candidate_case_ids}
        executed_cases = {case_id: all_executed_cases[case_id] for case_id in candidate_case_ids}
    attempts: dict[str, int] = defaultdict(int)
    score_count = 0
    errors: list[str] = []
    try:
        with experiment(
            experiment_name,
            experiment_id=experiment_id,
            suite=suite,
            client=client,
            candidate=_candidate(first),
            planned_trial_count=len(trials),
            metadata={
                "framework": "harbor",
                "harbor_candidate": _candidate_identity(first),
                "suite_publication_policy": suite_publication_policy or "local",
                "executed_case_count": len(cases),
            },
        ) as exp:
            for trial_dir, result in trials:
                case_id = _case_id(result, suite.suite_id)
                attempts[case_id] += 1
                trial = exp.trial(
                    cases[case_id],
                    attempt=attempts[case_id],
                    metadata=_case_provenance(executed_cases[case_id], cases[case_id]),
                )
                try:
                    with trial:
                        score_count += _record_guarded(
                            trial,
                            exp.client,
                            result,
                            trial_dir,
                            main_reward=main_reward,
                            pass_threshold=pass_threshold,
                            upload_artifacts=upload_artifacts,
                            artifact_max_bytes=artifact_max_bytes,
                        )
                except Exception as error:
                    if _is_terminal_conflict(error):
                        raise
                    _close_failed_trial(trial, error)
                if trial.status == TrialStatus.ERRORED.value:
                    errors.append(_trial_error(trial))
            if errors:
                exp.finalize("failed", error=_run_error(errors))
    except Exception as error:
        if _is_terminal_conflict(error):
            raise _AlreadyFinalizedError(
                f"Experiment {experiment_id!r} is already finalized, so this Harbor job cannot be imported "
                "into it again. Pass a new experiment_id (--experiment-id) to import it as a separate experiment."
            ) from error
        raise
    return PublishedHarborRun(experiment_id, exp.url, len(trials), score_count, _candidate_label(first))


def _record_guarded(trial: Any, client: Any, result: Mapping[str, Any], trial_dir: Path, **options: Any) -> int:
    """Records a result, turning an unexpected failure into an errored trial."""

    try:
        return _record_trial_result(trial, client, result, trial_dir, **options)
    except Exception as error:
        if _is_terminal_conflict(error):
            raise
        logger.warning("agento11y: Harbor trial %s could not be recorded: %s", trial_dir.name, error)
        trial.mark_errored(f"Harbor result export failed: {error}")
        return 0


def _close_failed_trial(trial: Any, error: BaseException) -> None:
    """Marks a trial errored after its start or close failed, and closes it once."""

    logger.warning("agento11y: Harbor trial %s failed to publish: %s", trial.ref.test_case_name, error)
    if trial.status != TrialStatus.ERRORED.value:
        trial.mark_errored(f"Agent Observability trial export failed: {error}")
    try:
        trial.close()
    except Exception as close_error:
        logger.warning("agento11y: Harbor trial %s could not be closed: %s", trial.ref.test_case_name, close_error)


def _record_trial_result(
    trial: Any,
    client: Any,
    result: Mapping[str, Any],
    trial_dir: Path,
    *,
    main_reward: str,
    pass_threshold: float,
    upload_artifacts: bool,
    artifact_max_bytes: int,
) -> int:
    """Records one Harbor result on an open trial and returns its score count.

    The plugin, the importer and ``run_harbor`` all use this function. A step
    that fails (a corrupt trajectory, an artifact upload) marks the trial
    errored, and the remaining steps still run.
    """

    problems: list[str] = []

    def guarded(label: str, step: Any, default: Any) -> Any:
        try:
            return step()
        except Exception as error:
            if _is_terminal_conflict(error):
                raise
            problems.append(f"{label}: {error}")
            return default

    try:
        conversation_id: str | None = _conversation_id(result)
    except ValueError as error:
        problems.append(str(error))
        conversation_id = None
    if conversation_id:
        # Instrumented agents publish their own conversation; link it instead of ATIF.
        trial.bind_conversation(conversation_id)
    elif conversation_id is not None:
        conversation_id, generation_id = guarded(
            "trajectory import",
            lambda: _publish_trajectory(
                client, trial_dir, result, trial.ref.experiment_id, trial.ref.test_case_id, trial.ref.attempt
            ),
            ("", ""),
        )
        if generation_id:
            trial.bind_generation(generation_id, conversation_id=conversation_id)
        elif conversation_id:
            trial.bind_conversation(conversation_id)
    usage = guarded("usage", lambda: _usage(result, trial_dir), {})
    trial.set_usage(
        input_tokens=usage.get("input_tokens"),
        output_tokens=usage.get("output_tokens"),
        cost=usage.get("cost"),
    )
    trial.set_duration(_duration_ms(result))
    if upload_artifacts:
        guarded("artifact upload", lambda: _upload_artifacts(trial, trial_dir, artifact_max_bytes), None)
    rewards = guarded("rewards", lambda: _rewards(result, trial_dir), {})
    infos = _exception_infos(result)
    result_error = _exception_text(infos)
    # Like Harbor's own metrics, a timed-out or failed agent run is still graded by its reward.
    graded_agent_error = (
        bool(infos)
        and main_reward in rewards
        and all(str(info.get("exception_type") or "") in _GRADED_AGENT_ERRORS for info in infos)
    )
    blocking_error = "" if graded_agent_error else result_error
    verdict_allowed = not blocking_error and not problems
    score_metadata = {"framework": "harbor"}
    if graded_agent_error:
        score_metadata["harbor_agent_error"] = result_error
    for name, value in rewards.items():
        is_primary = name == main_reward and verdict_allowed
        trial.score(
            name,
            value,
            evaluator=_reward_evaluator(name, result),
            passed=(float(value) >= pass_threshold) if is_primary else None,
            report_role=ReportRole.PRIMARY_VERDICT if is_primary else ReportRole.DIAGNOSTIC,
            metadata={**score_metadata, "harbor_reward": name},
        )
    errors = [blocking_error, *problems] if blocking_error else problems
    if not errors and main_reward not in rewards:
        errors = [f"Harbor result has no main reward {main_reward!r}"]
    if errors:
        trial.mark_errored("; ".join(errors))
    return len(rewards)


def _reward_evaluator(name: str, result: Mapping[str, Any]) -> Evaluator:
    # The task checksum covers the verifier tests, so it versions every reward.
    return Evaluator(
        evaluator_id=f"harbor.{name}",
        version=str(result.get("task_checksum") or "unknown"),
        kind="custom",
    )


def _experiment_id(base_id: str, result: Mapping[str, Any]) -> str:
    """Returns the per-candidate experiment ID shared by the plugin and the importer."""

    suffix = stable_id("candidate", _group_identity(result)).removeprefix("candidate-")
    return f"{base_id}-{suffix}"


def _experiment_name(base_name: str, result: Mapping[str, Any]) -> str:
    return f"{base_name} [{_candidate_label(result)}]"


def _trial_error(trial: Any) -> str:
    return f"{trial.ref.test_case_name or trial.ref.test_case_id} attempt {trial.ref.attempt}: {trial.error}"


def _run_error(errors: list[str], limit: int = 5) -> str:
    """Summarizes errored trials for the experiment error without unbounded growth."""

    summary = "; ".join(errors[:limit])
    if len(errors) > limit:
        summary += f"; and {len(errors) - limit} more errored trials"
    return summary


def _is_terminal_conflict(error: BaseException) -> bool:
    return isinstance(error, ConflictError) and error.kind == ConflictKind.TERMINAL


def _conversation_id(result: Mapping[str, Any]) -> str:
    """Returns the conversation ID an instrumented agent reported in Harbor metadata."""

    contexts = [
        result.get("agent_result"),
        *(step.get("agent_result") for step in result.get("step_results") or [] if isinstance(step, Mapping)),
    ]
    ids = [
        value.strip()
        for context in contexts
        if isinstance(context, Mapping)
        if isinstance(value := (context.get("metadata") or {}).get("agento11y.conversation_id"), str) and value.strip()
    ]
    if len(set(ids)) > 1:
        raise ValueError("Harbor step results contain inconsistent Agent Observability conversation IDs")
    return ids[-1] if ids else ""


def _prepare_suite(
    trials: list[tuple[Path, dict[str, Any]]],
    *,
    suite_id: str,
    test_suites_client: TestSuitesClient | None = None,
    suite_publication_policy: str | None = None,
) -> tuple[TestSuite, dict[str, TestCase], dict[str, TestCase]]:
    first = trials[0][1]
    resolved_suite_id = suite_id.strip() or stable_id("harbor-suite", str(first.get("source") or "adhoc"))
    cases = _cases(trials, resolved_suite_id)
    executed_cases = deepcopy(cases)
    suite = TestSuite(
        suite_id=resolved_suite_id,
        name=str(first.get("source") or "Harbor dataset"),
        version=stable_id(
            "harbor-checkpoint",
            *sorted(
                {f"{_case_id(result, resolved_suite_id)}:{result.get('task_checksum') or ''}" for _, result in trials}
            ),
        ),
        tags=["framework:harbor"],
        test_cases=list(cases.values()),
    )
    if test_suites_client is not None:
        pushed = test_suites_client.push_suite(
            suite,
            publish=True,
            prune=suite_publication_policy == "replace",
        )
        stored = {case.test_case_id: case for case in pushed.suite.cases}
        cases = {case_id: _canonical_case(stored.get(case_id), case) for case_id, case in executed_cases.items()}
        suite = pushed.suite
    return suite, cases, executed_cases


def _trial_records(root: Path) -> list[tuple[Path, dict[str, Any]]]:
    records: list[tuple[Path, dict[str, Any]]] = []
    for path in sorted(root.rglob("result.json")):
        if path == root / "result.json":
            continue
        try:
            value = _read_json(path)
        except ValueError as error:
            # Without a readable result there is no task or candidate to attach it to.
            logger.warning("agento11y: skipping unreadable Harbor trial result: %s", error)
            continue
        if value.get("task_name") and isinstance(value.get("agent_info"), Mapping):
            records.append((path.parent, value))
    return records


def _cases(trials: list[tuple[Path, dict[str, Any]]], suite_id: str) -> dict[str, TestCase]:
    cases: dict[str, TestCase] = {}
    checksums: dict[str, str] = {}
    for _, result in trials:
        case = _test_case(result, suite_id)
        checksum = str(result.get("task_checksum") or "")
        if case.test_case_id in checksums and checksums[case.test_case_id] != checksum:
            raise ValueError(f"Harbor task {result.get('task_name')!r} changed content within one job")
        checksums[case.test_case_id] = checksum
        cases.setdefault(case.test_case_id, case)
    return cases


def _test_case(result: Mapping[str, Any], suite_id: str) -> TestCase:
    case_id = _case_id(result, suite_id)
    return TestCase(
        test_case_id=case_id,
        name=str(result.get("task_name") or case_id),
        input={"task_name": result.get("task_name"), "task_id": result.get("task_id")},
        metadata={
            "framework": "harbor",
            "harbor_source": result.get("source") or "adhoc",
            "harbor_task_checksum": str(result.get("task_checksum") or ""),
        },
    )


def _case_id(result: Mapping[str, Any], suite_id: str) -> str:
    return stable_id("harbor-task", suite_id, result.get("source"), result.get("task_name"))


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
    return stable_id(
        "case-content",
        json.dumps(
            {"input": case.input, "expected": case.expected},
            sort_keys=True,
            default=str,
            separators=(",", ":"),
        ),
    )


def _candidate(result: Mapping[str, Any]) -> Candidate:
    agent = _mapping(result.get("agent_info"))
    model = _mapping(agent.get("model_info"))
    return Candidate(
        agent_name=str(agent.get("name") or ""),
        agent_version=str(agent.get("version") or ""),
        model_provider=str(model.get("provider") or ""),
        model_name=str(model.get("name") or ""),
    )


def _candidate_identity(result: Mapping[str, Any]) -> str:
    candidate = _candidate(result)
    return json.dumps(
        {
            "agent_name": candidate.agent_name,
            "agent_version": candidate.agent_version,
            "model_provider": candidate.model_provider,
            "model_name": candidate.model_name,
        },
        sort_keys=True,
        separators=(",", ":"),
    )


def _group_identity(result: Mapping[str, Any]) -> str:
    # Harbor fills in the agent version only after agent setup, so a trial
    # whose setup failed has none. Leave it out so every trial of a candidate,
    # live or imported, shares one experiment ID.
    candidate = _candidate(result)
    return json.dumps(
        {
            "candidate": {
                "agent_name": candidate.agent_name,
                "model_name": candidate.model_name,
                "model_provider": candidate.model_provider,
            },
            "source": result.get("source") or "adhoc",
        },
        sort_keys=True,
        separators=(",", ":"),
    )


def _candidate_label(result: Mapping[str, Any]) -> str:
    candidate = _candidate(result)
    return " / ".join(value for value in (candidate.agent_name, candidate.model_name) if value) or "unknown"


def _publish_trajectory(
    client: Any,
    trial_dir: Path,
    result: Mapping[str, Any],
    experiment_id: str,
    case_id: str,
    attempt: int,
) -> tuple[str, str]:
    paths = sorted(path for path in trial_dir.rglob("trajectory*.json") if path.is_file())
    if not paths:
        return "", ""
    root_path = next(
        (path for path in (trial_dir / "agent" / "trajectory.json", trial_dir / "trajectory.json") if path.is_file()),
        None,
    )
    documents = [
        (
            f"{path.relative_to(trial_dir)}:{label}",
            trajectory,
            path == root_path and index == 0,
        )
        for path in paths
        for index, (label, trajectory) in enumerate(_trajectories(_read_json(path)))
    ]
    harbor_session_id = str(
        next((trajectory.get("session_id") for _, trajectory, _ in documents if trajectory.get("session_id")), "")
    )
    conversation_id = stable_id("conv", experiment_id, case_id, attempt, harbor_session_id)
    root_generation = ""
    for document_id, trajectory, is_root in documents:
        agent = _mapping(trajectory.get("agent"))
        provider, model_name = _model_identity(str(agent.get("model_name") or ""), result)
        pending: list[str] = []
        for step in trajectory.get("steps") or []:
            if not isinstance(step, Mapping):
                continue
            source = str(step.get("source") or "")
            if source != "agent" or step.get("is_copied_context") is True or step.get("llm_call_count") == 0:
                pending.append(f"{source}: {_content_text(step.get('message'))}")
                continue
            generation_id = stable_id("gen", experiment_id, case_id, attempt, document_id, step.get("step_id"))
            output = _content_text(step.get("message"))
            if step.get("reasoning_content"):
                output += f"\n\nReasoning:\n{step['reasoning_content']}"
            if isinstance(step.get("tool_calls"), list):
                output += "\n\nTool calls:\n" + "\n".join(
                    _bound_text(json.dumps(call, sort_keys=True, default=str), _TOOL_INPUT_MAX_BYTES)
                    for call in step["tool_calls"]
                )
            if step.get("observation"):
                output += "\n\nObservation:\n" + json.dumps(step["observation"], sort_keys=True, default=str)
            metrics = _mapping(step.get("metrics"))
            prompt_tokens = _integer(metrics.get("prompt_tokens"))
            completion_tokens = _integer(metrics.get("completion_tokens"))
            cached_tokens = _integer(metrics.get("cached_tokens"))
            usage = TokenUsage(
                input_tokens=prompt_tokens or 0,
                output_tokens=completion_tokens or 0,
                cache_read_input_tokens=cached_tokens or 0,
                input_semantics=TokenInputSemantics.INCLUSIVE,
            )
            # ATIF timestamps are point events, not measured provider intervals.
            timestamp = _datetime(step.get("timestamp"))
            if timestamp is not None and timestamp.tzinfo is None:
                timestamp = None
            observed_at = timestamp or datetime.now(timezone.utc)
            client.record_generation(
                generation_id,
                conversation_id=conversation_id,
                input_text=_bound_text("\n\n".join(pending), _TEXT_MAX_BYTES),
                output_text=_bound_text(output, _TEXT_MAX_BYTES),
                model_provider=provider,
                model_name=str(step.get("model_name") or model_name),
                agent_name=str(agent.get("name") or _mapping(result.get("agent_info")).get("name") or "harbor"),
                agent_version=str(agent.get("version") or _mapping(result.get("agent_info")).get("version") or ""),
                usage=usage,
                started_at=observed_at,
                completed_at=observed_at,
                tags={"experiment_id": experiment_id, "test.case.id": case_id, "framework": "harbor"},
                metadata={
                    "atif_schema_version": trajectory.get("schema_version"),
                    "harbor_step_id": step.get("step_id"),
                    "harbor_session_id": harbor_session_id,
                    "harbor_trajectory": document_id,
                    "harbor_duration_known": False,
                    "harbor_timing_source": "atif_step_timestamp" if timestamp else "ingestion_time",
                    "harbor_step_timestamp": step.get("timestamp"),
                    "harbor_prompt_tokens": prompt_tokens,
                    "harbor_completion_tokens": completion_tokens,
                    "harbor_cached_tokens": cached_tokens,
                    "harbor_cost_usd": _number(metrics.get("cost_usd")),
                },
            )
            pending = []
            if is_root:
                root_generation = generation_id
    return conversation_id, root_generation


def _trajectories(root: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    out = [(str(root.get("trajectory_id") or "root"), root)]
    for index, nested in enumerate(root.get("subagent_trajectories") or []):
        if isinstance(nested, dict):
            for label, trajectory in _trajectories(nested):
                out.append((f"subagent-{index}/{label}", trajectory))
    return out


def _usage(result: Mapping[str, Any], trial_dir: Path) -> dict[str, int | float]:
    contexts = [_mapping(result.get("agent_result"))]
    if not any(contexts[0].values()):
        contexts = [
            _mapping(step.get("agent_result")) for step in result.get("step_results") or [] if isinstance(step, Mapping)
        ]
    usage: dict[str, int | float] = {}
    fields = {
        "input_tokens": ("n_input_tokens", "total_prompt_tokens"),
        "cache_read_input_tokens": ("n_cache_tokens", "total_cached_tokens"),
        "output_tokens": ("n_output_tokens", "total_completion_tokens"),
        "cost": ("cost_usd", "total_cost_usd"),
    }
    for target, (context_field, _) in fields.items():
        convert = _number if target == "cost" else _integer
        known = [value for context in contexts if (value := convert(context.get(context_field))) is not None]
        if known:
            usage[target] = sum(known)
            continue
        model_values = [
            value
            for context in contexts
            for model in _mapping(context.get("model_usage")).values()
            if isinstance(model, Mapping) and (value := convert(model.get(context_field))) is not None
        ]
        if model_values:
            usage[target] = sum(model_values)

    paths = [
        path for path in (trial_dir / "agent" / "trajectory.json", trial_dir / "trajectory.json") if path.is_file()
    ]
    for target, (_, trajectory_field) in fields.items():
        if target in usage:
            continue
        convert = _number if target == "cost" else _integer
        known = [
            value
            for path in paths
            if (value := convert(_mapping(_read_optional_json(path).get("final_metrics")).get(trajectory_field)))
            is not None
        ]
        if known:
            usage[target] = sum(known)
    return usage


def _duration_ms(result: Mapping[str, Any]) -> int | None:
    timings = [_mapping(result.get("agent_execution"))]
    if not any(timings[0].values()):
        timings = [
            _mapping(step.get("agent_execution"))
            for step in result.get("step_results") or []
            if isinstance(step, Mapping)
        ]
    durations = []
    for timing in timings:
        started = _datetime(timing.get("started_at"))
        finished = _datetime(timing.get("finished_at"))
        if started is not None and finished is not None:
            durations.append(max(0, int((finished - started).total_seconds() * 1000)))
    return sum(durations) if durations else None


def _rewards(result: Mapping[str, Any], trial_dir: Path) -> dict[str, float]:
    verifier = _mapping(result.get("verifier_result"))
    raw = verifier.get("rewards")
    if not isinstance(raw, Mapping):
        raw = _read_optional_json(trial_dir / "verifier" / "reward.json")
    rewards: dict[str, float] = {}
    for name, value in raw.items() if isinstance(raw, Mapping) else []:
        number = _number(value)
        if number is not None:
            rewards[str(name)] = number
    return rewards


def _exception_infos(result: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    infos = [_mapping(result.get("exception_info"))]
    infos.extend(
        _mapping(step.get("exception_info")) for step in result.get("step_results") or [] if isinstance(step, Mapping)
    )
    return [info for info in infos if info]


def _exception_text(infos: list[Mapping[str, Any]]) -> str:
    errors = []
    for info in infos:
        kind = str(info.get("exception_type") or "HarborError")
        message = str(info.get("exception_message") or "")
        errors.append(f"{kind}: {message}".rstrip(": "))
    return "; ".join(errors)


def _upload_artifacts(trial: Any, trial_dir: Path, max_bytes: int) -> None:
    paths = [trial_dir / "result.json", trial_dir / "trajectory.json"]
    for pattern in (
        "**/trajectory*.json",
        "**/verifier/reward.json",
        "**/verifier/reward-details.json",
        "**/verifier/reward.txt",
        "**/verifier/test-stdout.txt",
        "**/verifier/test-stderr.txt",
        "**/artifacts/**/*",
    ):
        paths.extend(path for path in trial_dir.glob(pattern) if path.is_file())
    seen: set[Path] = set()
    for path in paths:
        if path in seen or not path.is_file() or not 0 < path.stat().st_size <= max_bytes:
            continue
        seen.add(path)
        name = str(path.relative_to(trial_dir)).replace("/", "__")
        trial.artifact(name, path=str(path))


def _model_identity(model_name: str, result: Mapping[str, Any]) -> tuple[str, str]:
    if "/" in model_name:
        provider, name = model_name.split("/", 1)
        return provider, name
    model = _mapping(_mapping(result.get("agent_info")).get("model_info"))
    return str(model.get("provider") or ""), model_name or str(model.get("name") or "")


def _bound_text(text: str, limit: int) -> str:
    encoded = text.encode("utf-8")
    if len(encoded) <= limit:
        return text
    return encoded[:limit].decode("utf-8", errors="ignore") + " [truncated]"


def _content_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(
            str(part.get("text") or part.get("data") or "") if isinstance(part, Mapping) else str(part)
            for part in value
        )
    return "" if value is None else str(value)


def _read_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"Cannot read Harbor JSON {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise ValueError(f"Harbor JSON must contain an object: {path}")
    return value


def _read_optional_json(path: Path) -> dict[str, Any]:
    return _read_json(path) if path.is_file() else {}


def _mapping(value: Any) -> dict[str, Any]:
    return dict(value) if isinstance(value, Mapping) else {}


def _number(value: Any) -> float | None:
    if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value):
        return float(value)
    return None


def _integer(value: Any) -> int | None:
    number = _number(value)
    return int(number) if number is not None else None


def _datetime(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
