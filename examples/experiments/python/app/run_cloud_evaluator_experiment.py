"""Grade experiment trials with an evaluator stored in Agent Observability.

Use this shape when the grading prompt lives in your tenant instead of in the
runner. The runner only has to make the agent's conversation findable:

  1. Open an experiment with the Grafana Cloud ingestion API key.
  2. For each case, run the already-instrumented agent and bind its conversation.
  3. Call ``trial.evaluate(evaluator_id)`` and let the stored evaluator score it.
  4. Close the trial without a local ``final_score``; the backend owns the verdict.

The agent here publishes its own generation so the example is runnable on its
own. A real agent instrumented with the SDK or a provider wrapper already emits
that generation, and you only need its conversation id.

Config via env: AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TOKEN, optional
AGENTO11Y_AUTH_TENANT_ID, AGENTO11Y_EXPERIMENT_ID, AGENTO11Y_EVALUATOR_ID,
AGENTO11Y_EVALUATOR_VERSION, ANTHROPIC_API_KEY, AGENT_MODEL, GIT_SHA.
"""

from __future__ import annotations

import os

from agento11y import experiments
from agento11y.errors import EvaluationExecutionError, EvaluationTimeoutError
from dotenv import load_dotenv

from app.agent import answer_question

CASES: list[tuple[str, str]] = [
    ("capital-france", "What is the capital of France?"),
    ("two-plus-two", "What is 2 + 2? Answer with just the number."),
    ("largest-planet", "What is the largest planet in our solar system?"),
]


def main() -> None:
    load_dotenv()
    git_sha = os.environ.get("GIT_SHA", "manual")
    experiment_id = os.environ.get("AGENTO11Y_EXPERIMENT_ID", f"cloud-evaluator-{git_sha}")
    # The evaluator must already exist in your tenant; this example does not create one.
    evaluator_id = os.environ.get("AGENTO11Y_EVALUATOR_ID", "helpfulness")
    evaluator_version = os.environ.get("AGENTO11Y_EVALUATOR_VERSION", "")

    with experiments.experiment(
        name="Cloud evaluator example experiment",
        experiment_id=experiment_id,
        planned_trial_count=len(CASES),
        candidate={"git_sha": git_sha, "agent_name": "example-agent"},
        tags=["example", "cloud-evaluator"],
    ) as exp:
        for case_id, question in CASES:
            with exp.trial(case_id) as trial:
                answer = answer_question(question)
                # Stand-in for your instrumentation's conversation. With a real
                # instrumented agent, bind the id it already exported instead.
                conversation_id = f"{trial.trial_id}-agent"
                exp.client.record_generation(
                    trial.generation_id,
                    conversation_id=conversation_id,
                    input_text=question,
                    output_text=answer.text,
                    model_provider="anthropic",
                    model_name=answer.model,
                    agent_name="example-agent",
                    operation_name="answer_question",
                    input_tokens=answer.usage.input_tokens,
                    output_tokens=answer.usage.output_tokens,
                    tags={"experiment.run_id": exp.experiment_id, "task_id": case_id},
                )
                # The generation above is already exported, so the evaluator can
                # read the conversation this binding points at.
                trial.bind_conversation(conversation_id)

                # Blocks until the worker finishes. No local final_score follows:
                # the stored evaluator writes the score and the report counts it.
                try:
                    evaluation = trial.evaluate(evaluator_id, evaluator_version)
                except EvaluationExecutionError as exc:
                    print(f"{case_id}: evaluation {exc.evaluation_id} failed: {exc.detail}")
                    raise
                except EvaluationTimeoutError as exc:
                    print(f"{case_id}: evaluation {exc.evaluation_id} still pending: {exc.detail}")
                    raise
                print(
                    f"{case_id}: {evaluation.status.value} "
                    f"evaluator={evaluation.evaluator_id}@{evaluation.evaluator_version or 'latest'} "
                    f"attempts={evaluation.attempts}"
                )

    report = exp.report()
    print(f"\nExperiment '{exp.experiment_id}' finished with stored-evaluator scores.")
    print(f"trials={report.summary.trial_count} completed={report.summary.completed_count}")
    # A stored evaluator scores under its own key, so pass_rate stays empty unless
    # that key is `final`. The report rows show which scores the backend attached.
    for row in report.rows:
        for result in row.get("trials", []):
            keys = [str(score.get("score_key", "")) for score in result.get("scores", [])]
            print(f"  {row.get('test_case_id', '')}: scores={', '.join(keys) or 'none'}")
    print(f"View in Agent Observability: {exp.url}")


if __name__ == "__main__":
    main()
