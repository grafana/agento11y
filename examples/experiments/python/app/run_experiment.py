"""Run a framework-free agent over a dataset as a Sigil experiment.

This is the shape a CI job or benchmark harness takes when your agent is already
instrumented normally and you want to publish experiment results to Sigil:

  1. Open an experiment with the Grafana Cloud ingestion API key.
  2. For each case, open a trial and run the agent.
  3. Bind or record the real conversation/generation when available.
  4. Emit scores and artifacts, then let the context managers flush/finalize.

Config via env: AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TOKEN, optional AGENTO11Y_AUTH_TENANT_ID,
AGENTO11Y_EXPERIMENT_ID, ANTHROPIC_API_KEY, AGENT_MODEL, GRADER_MODEL, GIT_SHA.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

from dotenv import load_dotenv
from agento11y import experiments

from app.agent import answer_question, grade_answer


@dataclass(frozen=True)
class Case:
    id: str
    input: str
    expected: str
    category: str


CASES: list[Case] = [
    Case("capital-france", "What is the capital of France?", "Paris", "trivia"),
    Case("two-plus-two", "What is 2 + 2? Answer with just the number.", "4", "math"),
    Case("largest-planet", "What is the largest planet in our solar system?", "Jupiter", "trivia"),
]


def main() -> None:
    load_dotenv()
    experiment_id = os.environ.get("AGENTO11Y_EXPERIMENT_ID", f"experiment-example-{os.environ.get('GIT_SHA', 'manual')}")
    suite = experiments.TestSuite(
        suite_id="experiment-example",
        name="Framework-free example experiment",
        version="2026-05-30",
        test_cases=[
            experiments.TestCase(
                test_case_id=case.id,
                input=case.input,
                expected=case.expected,
                category=case.category,
            )
            for case in CASES
        ],
    )
    verifier = experiments.Evaluator(evaluator_id="example.llm_judge", version="2026-05-30", kind="llm_judge")

    with experiments.experiment(
        name="Framework-free example experiment",
        experiment_id=experiment_id,
        suite=suite,
        candidate={"git_sha": os.environ.get("GIT_SHA", "manual"), "agent_name": "example-agent"},
        tags=["example"],
    ) as exp:
        for case in CASES:
            with exp.trial(case.id) as trial:
                answer = answer_question(case.input)
                agent_conversation_id = f"{trial.trial_id}-agent"
                exp.client.record_generation(
                    trial.generation_id,
                    conversation_id=agent_conversation_id,
                    input_text=case.input,
                    output_text=answer.text,
                    model_provider="anthropic",
                    model_name=answer.model,
                    agent_name="example-agent",
                    operation_name="answer_question",
                    input_tokens=answer.usage.input_tokens,
                    output_tokens=answer.usage.output_tokens,
                    tags={"experiment.run_id": exp.experiment_id, "task_id": case.id, "role": "candidate"},
                    metadata={"response_id": answer.response_id, "stop_reason": answer.stop_reason},
                )
                trial.bind_generation(trial.generation_id, conversation_id=agent_conversation_id)

                grade = grade_answer(question=case.input, expected=case.expected, actual=answer.text)
                grader_generation_id = f"grader-{trial.trial_id}"
                grader_conversation_id = f"{trial.trial_id}-grader"
                exp.client.record_generation(
                    grader_generation_id,
                    conversation_id=grader_conversation_id,
                    input_text=grade.prompt,
                    output_text=grade.call.text,
                    model_provider="anthropic",
                    model_name=grade.call.model,
                    agent_name="example-grader",
                    operation_name="grade_answer",
                    input_tokens=grade.call.usage.input_tokens,
                    output_tokens=grade.call.usage.output_tokens,
                    tags={"experiment.run_id": exp.experiment_id, "task_id": case.id, "role": "grader"},
                    metadata={"response_id": grade.call.response_id, "stop_reason": grade.call.stop_reason},
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
                trial.artifact(
                    "grading-details",
                    data={
                        "test_case_id": case.id,
                        "expected": case.expected,
                        "actual": answer.text,
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
                trial.artifact("agent-output", text=answer.text or "(empty)")
                trial.artifact("grader-output", text=grade.call.text or "(empty)")

    report = exp.report()
    print(f"\nExperiment '{exp.experiment_id}' finished: {exp.accepted_scores} score(s) accepted.")
    print(f"pass_rate={report.summary.pass_rate:.2f} mean_score={report.summary.final_score_avg:.2f}")
    print(f"View in Sigil: {exp.url}")


if __name__ == "__main__":
    main()
