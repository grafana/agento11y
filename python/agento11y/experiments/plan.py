"""Sequential mixed judging on the existing trial lifecycle, with one verdict."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass, field
from typing import Any

from ..models import ReportRole
from .cases import case_paths, case_value, render_case_prompt
from .control import EvaluatorsClient, StoredEvaluator
from .evaluators import EvaluationResult, LLMJudge, OutputEvaluator
from .types import Evaluator, TestCase


@dataclass(frozen=True, slots=True)
class ExactMatch:
    expected_selector: str = "expected.assistant_response"
    evaluator: Evaluator = field(
        default_factory=lambda: Evaluator("grafana.exact_match", version="1", kind="deterministic")
    )

    def evaluate_output(self, *, input: Any, output: Any, expected: Any = None) -> EvaluationResult:
        reference = case_value(TestCase("exact_match", input=input, expected=expected), self.expected_selector)
        passed = output == reference
        return EvaluationResult(self.evaluator, passed, passed, score_key="exact_match")


@dataclass(frozen=True, slots=True)
class Check:
    name: str
    judge: OutputEvaluator | StoredEvaluator
    required: bool = True


class EvaluationPlan:
    """All required checks must pass. Diagnostics never affect the denominator.

    Every check is retained as a diagnostic score. A required evaluator error
    makes the trial errored/unrated and suppresses the aggregate primary verdict.
    Remaining judges still run; their valid evidence is retained.
    """

    def __init__(self, checks: Sequence[Check]) -> None:
        self.checks = tuple(checks)
        names = [c.name for c in self.checks]
        if not names or any(not n.strip() or n == "overall" for n in names) or len(set(names)) != len(names):
            raise ValueError("checks need unique nonempty names; 'overall' is reserved")
        if not any(c.required for c in self.checks):
            raise ValueError("at least one required check is needed for a primary verdict")
        identities = [
            (c.judge.evaluator_id, c.judge.version) for c in self.checks if isinstance(c.judge, StoredEvaluator)
        ]
        if len(set(identities)) != len(identities):
            raise ValueError("the same stored evaluator cannot run twice in one plan")

    def validate(self, cases: Sequence[TestCase]) -> None:
        """Validate every case before agent calls, run writes or judge provisioning."""
        for check in self.checks:
            judge = check.judge
            paths: tuple[str, ...] = ()
            if isinstance(judge, StoredEvaluator):
                paths = case_paths(str(judge.config.get("user_prompt", "")))
                if case_paths(str(judge.config.get("system_prompt", ""))):
                    raise ValueError("Put case placeholders in the stored evaluator's user_prompt")
            elif isinstance(judge, LLMJudge):
                paths = case_paths(judge.prompt_template)
            elif isinstance(judge, ExactMatch):
                paths = (judge.expected_selector,)
            for case in cases:
                for path in paths:
                    case_value(case, path)
                if isinstance(judge, LLMJudge):
                    render_case_prompt(judge.prompt_template, case, "")

    def provision(self, control: EvaluatorsClient) -> None:
        for check in self.checks:
            if isinstance(check.judge, StoredEvaluator):
                control.ensure(check.judge)

    def provenance(self) -> list[dict[str, Any]]:
        return [
            dict(
                name=c.name,
                required=c.required,
                evaluator_id=c.judge.evaluator_id
                if isinstance(c.judge, StoredEvaluator)
                else c.judge.evaluator.evaluator_id,
                version=c.judge.version if isinstance(c.judge, StoredEvaluator) else c.judge.evaluator.version,
            )
            for c in self.checks
        ]

    def evaluate(self, trial: Any, case: TestCase, output: Any, client: Any) -> None:
        required_results = []
        errors = []
        for check in self.checks:
            try:
                if isinstance(check.judge, StoredEvaluator):
                    judge = check.judge
                    trial.evaluate(judge.evaluator_id, judge.version, report_role=ReportRole.DIAGNOSTIC)
                    passed = self._remote_passed(client, trial, judge)
                else:
                    kwargs = {"case_metadata": case.metadata} if isinstance(check.judge, LLMJudge) else {}
                    result = check.judge.evaluate_output(
                        input=case.input, output=output, expected=case.expected, **kwargs
                    )
                    if not isinstance(result.passed, bool):
                        raise ValueError("judge returned no valid boolean verdict")
                    trial.record_evaluation(
                        result,
                        score_key=check.name,
                        report_role=ReportRole.DIAGNOSTIC,
                        metadata={"required": check.required},
                    )
                    passed = result.passed
                if check.required:
                    required_results.append(passed)
            except Exception as error:
                message = f"{check.name}: {type(error).__name__}: {error}"
                trial.score(
                    check.name + ".error",
                    str(error),
                    report_role=ReportRole.DIAGNOSTIC,
                    metadata={"evaluation_error": True, "required": check.required},
                )
                if check.required:
                    errors.append(message)
        if errors:
            trial.mark_errored("; ".join(errors))
        else:
            passed = all(required_results)
            trial.score(
                "overall",
                passed,
                passed=passed,
                report_role=ReportRole.PRIMARY_VERDICT,
                evaluator=Evaluator("grafana.all_required", version="1", kind="deterministic"),
                metadata={"checks": self.provenance()},
            )

    @staticmethod
    def _remote_passed(client: Any, trial: Any, judge: StoredEvaluator) -> bool:
        cursor = None
        found = []
        # ponytail: paginate run scores; use trial-filtered reads when the API exposes them.
        for _ in range(100):
            scores, cursor = client.list_scores(trial.ref.experiment_id, limit=200, cursor=cursor)
            found.extend(
                s
                for s in scores
                if s.get("trial_id") == trial.trial_id
                and s.get("evaluator_id") == judge.evaluator_id
                and s.get("evaluator_version") == judge.version
                and s.get("score_key") == judge.output_keys[0]["key"]
            )
            if cursor is None:
                break
        else:
            raise RuntimeError("score pagination exceeded 100 pages")
        if len(found) != 1 or not isinstance(found[0].get("passed"), bool):
            raise ValueError("stored judge did not produce exactly one rated score")
        return found[0]["passed"]
