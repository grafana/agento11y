"""Run a dashboard-generation experiment and upload rendered image artifacts."""

from __future__ import annotations

import json
import os
from tempfile import TemporaryDirectory

from dotenv import load_dotenv
from sigil_sdk import experiments as sigil

from app.dashboard_agent import (
    DashboardCase,
    build_dashboard_spec,
    grade_dashboard,
    parse_dashboard_spec,
)
from app.dashboard_render import render_dashboard

TRIALS_PER_CASE = 3


CASES: list[DashboardCase] = [
    DashboardCase(
        id="latency-slo-dashboard",
        prompt="Build an API latency dashboard. Show p50 and p95 latency by day and include the 250 ms SLO threshold.",
        data={
            "days": ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
            "p50_ms": [92, 88, 96, 104, 111, 101, 97],
            "p95_ms": [211, 230, 246, 271, 265, 238, 224],
            "slo_ms": 250,
        },
        required=["line chart", "p50 series", "p95 series", "250 ms threshold", "daily x-axis"],
    ),
    DashboardCase(
        id="incident-mix-dashboard",
        prompt=(
            "Build an incident dashboard. Compare incident counts by severity for checkout, search, "
            "and profile services."
        ),
        data={
            "services": ["checkout", "search", "profile"],
            "critical": [4, 2, 1],
            "warning": [9, 6, 3],
            "info": [12, 8, 5],
        },
        required=["bar chart", "service x-axis", "critical series", "warning series", "info series"],
    ),
    DashboardCase(
        id="error-budget-burn-dashboard",
        prompt=(
            "Build an error budget dashboard. Show daily budget burn percentage and highlight the "
            "100% exhaustion line."
        ),
        data={
            "days": ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
            "burn_percent": [18, 27, 42, 64, 91, 118, 132],
            "exhaustion_percent": 100,
        },
        required=["line chart", "burn percentage series", "100% threshold", "daily x-axis"],
    ),
    DashboardCase(
        id="deployment-health-dashboard",
        prompt=(
            "Build a deployment health dashboard. Compare successful, rolled back, and failed deploys "
            "by environment."
        ),
        data={
            "environments": ["dev", "staging", "prod"],
            "successful": [42, 31, 18],
            "rolled_back": [3, 4, 2],
            "failed": [5, 3, 1],
        },
        required=["bar chart", "environment x-axis", "successful series", "rolled back series", "failed series"],
    ),
    DashboardCase(
        id="queue-depth-dashboard",
        prompt=(
            "Build a queue depth dashboard. Show queue backlog for ingest, scoring, and export workers "
            "over six intervals."
        ),
        data={
            "intervals": ["00:00", "04:00", "08:00", "12:00", "16:00", "20:00"],
            "ingest": [120, 180, 260, 310, 240, 170],
            "scoring": [80, 140, 210, 260, 230, 190],
            "export": [40, 70, 90, 120, 100, 75],
        },
        required=["line chart", "ingest series", "scoring series", "export series", "time x-axis"],
    ),
]


def main() -> None:
    load_dotenv()
    experiment_id = os.environ.get(
        "SIGIL_EXPERIMENT_ID",
        f"dashboard-example-{os.environ.get('GIT_SHA', 'manual')}",
    )
    suite = sigil.TestSuite(
        suite_id="dashboard-example",
        name="Dashboard generation example",
        version="2026-06-29",
        test_cases=[
            sigil.TestCase(
                test_case_id=case.id,
                input={"prompt": case.prompt, "data": case.data},
                expected={"required": case.required},
                category="dashboard",
            )
            for case in CASES
        ],
    )
    verifier = sigil.Evaluator(evaluator_id="example.dashboard_judge", version="2026-06-29", kind="llm_judge")

    with TemporaryDirectory() as artifact_dir:
        with sigil.experiment(
            name="Dashboard generation example",
            experiment_id=experiment_id,
            suite=suite,
            candidate={"git_sha": os.environ.get("GIT_SHA", "manual"), "agent_name": "dashboard-agent"},
            tags=["example", "dashboard"],
        ) as exp:
            for case in CASES:
                for attempt in range(1, TRIALS_PER_CASE + 1):
                    with exp.trial(case.id, attempt=attempt) as trial:
                        spec_call = build_dashboard_spec(case)
                        spec = parse_dashboard_spec(spec_call.text)
                        image_path = os.path.join(artifact_dir, f"{case.id}-attempt-{attempt}.png")
                        render_dashboard(spec, image_path)

                        agent_conversation_id = f"{trial.trial_id}-dashboard-agent"
                        exp.client.record_generation(
                            trial.generation_id,
                            conversation_id=agent_conversation_id,
                            input_text=json.dumps({"prompt": case.prompt, "data": case.data}, indent=2),
                            output_text=spec_call.text,
                            model_provider="anthropic",
                            model_name=spec_call.model,
                            agent_name="dashboard-agent",
                            operation_name="build_dashboard_spec",
                            input_tokens=spec_call.usage.input_tokens,
                            output_tokens=spec_call.usage.output_tokens,
                            tags={"experiment.run_id": exp.experiment_id, "task_id": case.id, "role": "candidate"},
                            metadata={
                                "response_id": spec_call.response_id,
                                "stop_reason": spec_call.stop_reason,
                                "attempt": attempt,
                            },
                        )
                        trial.bind_generation(trial.generation_id, conversation_id=agent_conversation_id)

                        grade = grade_dashboard(case=case, spec_text=spec_call.text)
                        grader_generation_id = f"grader-{trial.trial_id}"
                        grader_conversation_id = f"{trial.trial_id}-dashboard-grader"
                        exp.client.record_generation(
                            grader_generation_id,
                            conversation_id=grader_conversation_id,
                            input_text=grade.prompt,
                            output_text=grade.call.text,
                            model_provider="anthropic",
                            model_name=grade.call.model,
                            agent_name="dashboard-grader",
                            operation_name="grade_dashboard",
                            input_tokens=grade.call.usage.input_tokens,
                            output_tokens=grade.call.usage.output_tokens,
                            tags={"experiment.run_id": exp.experiment_id, "task_id": case.id, "role": "grader"},
                            metadata={
                                "response_id": grade.call.response_id,
                                "stop_reason": grade.call.stop_reason,
                                "attempt": attempt,
                            },
                        )
                        trial.score(
                            "final",
                            grade.score,
                            passed=grade.passed,
                            explanation=grade.explanation,
                            evaluator=verifier,
                            generation_id=trial.generation_id,
                            grader_conversation_id=grader_conversation_id,
                            grader_generation_id=grader_generation_id,
                        )
                        trial.artifact("dashboard-image", path=image_path, kind="image", mime="image/png")
                        trial.artifact("dashboard-spec", data=spec, kind="json")
                        trial.artifact(
                            "grading-details",
                            data={
                                "test_case_id": case.id,
                                "attempt": attempt,
                                "passed": grade.passed,
                                "score": grade.score,
                                "explanation": grade.explanation,
                                "agent_generation_id": trial.generation_id,
                                "agent_conversation_id": agent_conversation_id,
                                "grader_generation_id": grader_generation_id,
                                "grader_conversation_id": grader_conversation_id,
                            },
                            kind="json",
                        )

        report = exp.report()
    print(f"\nDashboard experiment '{experiment_id}' finished: {exp.accepted_scores} score(s) accepted.")
    print(f"pass_rate={report.summary.pass_rate:.2f} mean_score={report.summary.final_score_avg:.2f}")
    print(f"View in Sigil: {exp.url}")


if __name__ == "__main__":
    main()
