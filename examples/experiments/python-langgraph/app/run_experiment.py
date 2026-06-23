"""Run a LangGraph agent over a dataset as a Sigil experiment.

This is the shape a CI job or local script would take:

  1. Build the agent under test.
  2. Build a Sigil client pointed at the target stack.
  3. Hand a dataset, a target (invoke the graph), and scorer(s) to the runner.
  4. The runner creates the experiment, runs+grades each item, exports scores
     attributed to the run, finalizes the run, and prints a link.

Config via env: SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID, SIGIL_AUTH_TOKEN, RUN_ID,
GIT_SHA. With no OPENAI_API_KEY the agent uses a deterministic fake model.
"""

from __future__ import annotations

import os

from dotenv import load_dotenv
from sigil_sdk import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    ScoreValue,
)
from sigil_sdk_langgraph import DatasetItem, ExperimentRun, ExperimentRunner, ScoreOutput, TargetResult

from app.agent import build_graph

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


def required_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if value == "":
        raise RuntimeError(f"{name} is required; copy it from Grafana Cloud AI Observability")
    return value


def build_client() -> Client:
    endpoint = required_env("SIGIL_ENDPOINT").rstrip("/")
    tenant_id = required_env("SIGIL_AUTH_TENANT_ID")
    auth_token = required_env("SIGIL_AUTH_TOKEN")
    return Client(
        ClientConfig(
            api=ApiConfig(endpoint=endpoint),
            generation_export=GenerationExportConfig(
                protocol="http",
                endpoint=f"{endpoint}/api/v1/generations:export",
                auth=AuthConfig(mode="basic", tenant_id=tenant_id, basic_password=auth_token),
            ),
        )
    )


def target(item: DatasetItem, run: ExperimentRun) -> TargetResult:
    """Invoke the graph with the run's callbacks so generations carry the run_id."""

    state = {"question": str(item.input), "answer": ""}
    result = GRAPH.invoke(state, config=run.langgraph_config())
    # generation_ids are captured by the run automatically; the runner fills them in.
    return TargetResult(output=result["answer"])


def exact_match_scorer(item: DatasetItem, result: TargetResult) -> list[ScoreOutput]:
    """A trivial substring grader. Swap in an LLM-as-judge scorer here if desired."""

    passed = str(item.expected).lower() in str(result.output).lower()
    return [
        ScoreOutput(
            evaluator_id="example.exact_match",
            evaluator_version="2026-05-28",
            score_key="exact_match",
            value=ScoreValue(number=1.0 if passed else 0.0),
            passed=passed,
            explanation=f"expected '{item.expected}', got '{result.output}'",
        )
    ]


# Build the graph with canned answers so the offline fake model "answers" each
# item; a real ChatOpenAI model ignores these.
GRAPH = build_graph([str(item.expected) for item in DATASET])


def main() -> None:
    load_dotenv()
    client = build_client()
    run_id = os.environ.get("RUN_ID", f"langgraph-example-{os.environ.get('GIT_SHA', 'local')}")
    runner = ExperimentRunner(
        client=client,
        run_id=run_id,
        name="LangGraph example experiment",
        dataset={"id": "langgraph-example", "version": "2026-05-28"},
        candidate={"git_sha": os.environ.get("GIT_SHA", "local")},
        tags=["example", "langgraph"],
        agent_name="langgraph-example-agent",
        provider_resolver="auto",
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
