"""Live execution through Harbor's native Trial API (Harbor installed separately)."""

from __future__ import annotations

import math
from collections.abc import Mapping
from copy import deepcopy
from pathlib import Path
from typing import Any

from agento11y.experiments import Candidate, Client, TestSuite, TrialStatus, experiment
from opentelemetry.propagate import inject

from .integration import (
    ARTIFACT_MAX_BYTES,
    PublishedHarborRun,
    _close_failed_trial,
    _is_terminal_conflict,
    _record_guarded,
    _run_error,
    _trial_error,
)


async def run_harbor(
    suite: TestSuite,
    configs: Mapping[str, Any],
    *,
    name: str = "Harbor live evaluation",
    experiment_id: str = "",
    client: Client | None = None,
    candidate: Candidate | None = None,
    agent_env: Mapping[str, str] | None = None,
    main_reward: str = "reward",
    pass_threshold: float = 1.0,
) -> PublishedHarborRun:
    """Run explicit case-id -> Harbor TrialConfig entries, sequentially.

    Configure the sandbox agent's OTel exporter with ``agent_env``. Its process
    must extract TRACEPARENT/TRACESTATE and TrialRef environment on startup.
    Harbor owns sandbox setup, agent execution, verification and cleanup.
    """
    from harbor.trial.trial import Trial as HarborTrial

    main_reward = main_reward.strip()
    if not main_reward or not math.isfinite(pass_threshold):
        raise ValueError("main_reward must be nonempty and pass_threshold finite")
    if not suite.cases or set(configs) != {case.test_case_id for case in suite.cases}:
        raise ValueError("configs must contain exactly the suite's case IDs")
    if len(configs) != len(suite.cases):
        raise ValueError("suite contains duplicate case IDs")
    if len({(config.agent.name, config.agent.model_name) for config in configs.values()}) != 1:
        raise ValueError("run_harbor accepts one agent/model candidate per experiment")
    first_agent = next(iter(configs.values())).agent
    if candidate is None:
        candidate = Candidate(agent_name=first_agent.name or "harbor", model_name=first_agent.model_name or "")
    scores = 0
    errors: list[str] = []
    with experiment(
        name,
        experiment_id=experiment_id,
        suite=suite,
        client=client,
        candidate=candidate,
        planned_trial_count=len(configs),
        use_experimental_otel=True,
        metadata={"framework": "harbor", "execution_mode": "live", "execution_kind": "agent_run"},
    ) as exp:
        for case in suite.cases:
            trial = exp.trial(case)
            try:
                with trial:
                    trial.set_duration(None)
                    config = deepcopy(configs[case.test_case_id])
                    carrier: dict[str, str] = {}
                    inject(carrier)
                    config.agent.env = {
                        **config.agent.env,
                        **dict(agent_env or {}),
                        **trial.ref.to_env(),
                        **{key.upper(): value for key, value in carrier.items()},
                    }
                    try:
                        native = await HarborTrial.create(config)
                        result = (await native.run()).model_dump(mode="json")
                    except Exception as native_error:
                        trial.mark_errored(str(native_error) or type(native_error).__name__)
                    else:
                        scores += _record_guarded(
                            trial,
                            exp.client,
                            result,
                            Path(native.paths.trial_dir),
                            main_reward=main_reward,
                            pass_threshold=pass_threshold,
                            upload_artifacts=True,
                            artifact_max_bytes=ARTIFACT_MAX_BYTES,
                        )
            except Exception as error:
                if _is_terminal_conflict(error):
                    raise
                _close_failed_trial(trial, error)
            if trial.status == TrialStatus.ERRORED.value:
                errors.append(_trial_error(trial))
        if errors:
            exp.finalize("failed", error=_run_error(errors))
    candidate_label = " / ".join(value for value in (candidate.agent_name, candidate.model_name) if value) or "unknown"
    return PublishedHarborRun(exp.experiment_id, exp.url, len(configs), scores, candidate_label)
