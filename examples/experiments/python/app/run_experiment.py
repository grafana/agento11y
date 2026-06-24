"""Run a framework-free agent over a dataset as a Sigil experiment.

This is the shape a CI job or local script would take when you are NOT using a
supported framework adapter (LangGraph, LangChain, ...):

  1. Build a Sigil client pointed at the target stack.
  2. Hand a dataset, a target (run the agent, recording its generation via
     ``run.start_generation(...)``), and scorer(s) to the runner.
  3. The runner creates the experiment, runs+grades each item, exports scores
     attributed to the run, finalizes the run, and prints a link.

Config via env: SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID, SIGIL_AUTH_TOKEN, RUN_ID,
GIT_SHA. With no OPENAI_API_KEY the agent uses deterministic canned answers.
"""

from __future__ import annotations

import os

from dotenv import load_dotenv
from sigil_sdk import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    DatasetItem,
    ExperimentRun,
    ExperimentRunner,
    Generation,
    GenerationExportConfig,
    GenerationStart,
    ModelRef,
    ScoreOutput,
    ScoreValue,
    TargetResult,
    assistant_text_message,
    user_text_message,
)

from app.agent import answer_question

DATASET: list[DatasetItem] = [
    DatasetItem(
        id="capital-france",
        input="What is the capital of France?",
        expected="Paris",
        metadata={"task_id": "capital_lookup", "task_category": "trivia"},
    ),
    DatasetItem(
        id="two-plus-two",
        input="What is 2 + 2? Answer with just the number.",
        expected="4",
        metadata={"task_id": "arithmetic", "task_category": "math"},
    ),
    DatasetItem(
        id="largest-planet",
        input="What is the largest planet in our solar system?",
        expected="Jupiter",
        metadata={"task_id": "astronomy", "task_category": "trivia"},
    ),
]

# Offline canned answers, keyed by question, used when OPENAI_API_KEY is unset.
CANNED = {str(item.input): str(item.expected) for item in DATASET}


def required_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if value == "":
        raise RuntimeError(f"{name} is required; copy it from Grafana Cloud AI Observability")
    return value


def build_client() -> Client:
    endpoint = required_env("SIGIL_ENDPOINT").rstrip("/")
    tenant_id = required_env("SIGIL_AUTH_TENANT_ID")
    token = required_env("SIGIL_AUTH_TOKEN")
    return Client(
        ClientConfig(
            api=ApiConfig(endpoint=endpoint),
            generation_export=GenerationExportConfig(
                protocol=os.environ.get("SIGIL_PROTOCOL", "http"),
                endpoint=f"{endpoint}/api/v1/generations:export",
                auth=AuthConfig(
                    mode=os.environ.get("SIGIL_AUTH_MODE", "basic"),
                    tenant_id=tenant_id,
                    basic_user=tenant_id,
                    basic_password=token,
                    bearer_token=token,
                ),
            ),
        )
    )


def target(item: DatasetItem, run: ExperimentRun) -> TargetResult:
    """Run the agent, recording its call so the generation carries the run_id."""

    question = str(item.input)
    # Recording through run.start_generation(...) tags the generation with the
    # experiment run_id and captures its id so the score below attaches to it.
    with run.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-4o-mini"))) as rec:
        answer = answer_question(question, canned=CANNED)
        rec.set_result(
            Generation(
                model=ModelRef(provider="openai", name="gpt-4o-mini"),
                input=[user_text_message(question)],
                output=[assistant_text_message(answer)],
            )
        )
    # generation_ids are captured by the run automatically; the runner fills them in.
    return TargetResult(output=answer)


def exact_match_scorer(item: DatasetItem, result: TargetResult) -> list[ScoreOutput]:
    """A trivial substring grader. Swap in an LLM-as-judge scorer here if desired."""

    passed = str(item.expected).lower() in str(result.output).lower()
    return [
        ScoreOutput(
            evaluator_id="example.exact_match",
            evaluator_version="2026-05-30",
            score_key="exact_match",
            value=ScoreValue(number=1.0 if passed else 0.0),
            passed=passed,
            explanation=f"expected '{item.expected}', got '{result.output}'",
        )
    ]


def main() -> None:
    load_dotenv()
    client = build_client()
    run_id = os.environ.get("RUN_ID", f"experiment-example-{os.environ.get('GIT_SHA', 'manual')}")
    runner = ExperimentRunner(
        client=client,
        run_id=run_id,
        name="Framework-free example experiment",
        dataset={"id": "experiment-example", "version": "2026-05-30"},
        candidate={"git_sha": os.environ.get("GIT_SHA", "manual")},
        tags=["example"],
        agent_name="example-agent",
    )
    try:
        result = runner.run(DATASET, target, [exact_match_scorer])
        print(f"\nExperiment '{result.run_id}' finished: {result.accepted_scores} score(s) accepted.")
        if result.report is not None:
            print(f"pass_rate={result.report.summary.pass_rate:.2f} mean_score={result.report.summary.mean_score:.2f}")
        print(f"View in Sigil: {result.url}")
    finally:
        client.shutdown()


if __name__ == "__main__":
    main()
