"""Run ten paid, real-agent cases. Results and raw provider usage stay on disk."""

import argparse
import asyncio
import json
import os
from pathlib import Path

from agent import CASES, MODELS, client, conversation_id, run_agent, setup
from agento11y.experiments import Candidate, TestCase, TestSuite, TestSuitesClient, experiment


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("framework", choices=["grafana", "deepeval", "harbor"])
    parser.add_argument("--model", choices=MODELS, default="sonnet")
    parser.add_argument("--batch", required=True)
    parser.add_argument("--results", type=Path, required=True)
    args = parser.parse_args()
    if not os.getenv("ANTHROPIC_API_KEY"):
        raise RuntimeError("Set ANTHROPIC_API_KEY")
    os.environ["REAL_AGENT_RESULTS"] = str(args.results.resolve())
    args.results.mkdir(parents=True, exist_ok=True)
    tp, mp = setup()
    sdk = client()
    model = MODELS[args.model]
    run_id = f"{args.batch}-{args.framework}-{args.model}"
    suite_id = f"{args.batch}-{args.framework}"
    candidate = Candidate(
        agent_name="order-ledger-agent", agent_version="1", model_provider="anthropic", model_name=model
    )

    def execute(question, case_id, trial):
        # Bind before running: a case that fails midway has still exported generations.
        trial.bind_conversation(conversation_id(run_id, case_id))

        def record_usage(result):
            trial.set_usage(
                input_tokens=result["input_tokens"], output_tokens=result["output_tokens"], cost=result["cost"]
            )

        return asyncio.run(run_agent(question, model, run_id, case_id, sdk, on_result=record_usage))["output"]

    try:
        if args.framework == "deepeval":
            from agento11y_deepeval import run_deepeval
            from deepeval.metrics import ExactMatchMetric
            from deepeval.test_case import LLMTestCase

            native = [
                LLMTestCase(
                    name=c["id"],
                    input=c["question"],
                    actual_output="",
                    expected_output=c["expected"],
                    metadata={"agento11y.test_case_id": c["id"]},
                )
                for c in CASES
            ]

            def target(case, trial):
                return execute(case.input, case.name, trial)

            run = run_deepeval(
                native,
                target,
                [ExactMatchMetric()],
                experiment_name=f"Real agent / DeepEval / {args.model}",
                experiment_id=run_id,
                suite_id=suite_id,
                suite_version="1",
                client=sdk,
                candidate=candidate,
            )
        else:
            suite = TestSuite(
                suite_id=suite_id,
                version="1",
                name="Order ledger: ten reviewed tasks",
                test_cases=[
                    TestCase(
                        test_case_id=c["id"],
                        name=c["id"],
                        input={"prompt": c["question"]},
                        expected={"assistant_response": c["expected"]},
                    )
                    for c in CASES
                ],
            )
            if args.framework == "grafana":
                suites = TestSuitesClient()
                # Only publish once: both candidates execute the exact same checkpoint.
                checkpoint = args.results / f"{suite_id}-checkpoint.json"
                if checkpoint.exists():
                    suite = TestSuite.from_dict(json.loads(checkpoint.read_text()))
                else:
                    suite = suites.push_suite(suite, publish=True).suite
                    checkpoint.write_text(json.dumps(suite.to_dict(), indent=2))

                # Run every case even if one fails: a failed case is recorded as
                # errored, and the other cases still produce comparable results.
                with experiment(
                    f"Real agent / Grafana / {args.model}",
                    experiment_id=run_id,
                    suite=suite,
                    client=sdk,
                    candidate=candidate,
                    planned_trial_count=len(suite.cases),
                    use_experimental_otel=True,
                    metadata={"framework": "grafana", "execution_kind": "agent_run"},
                ) as run:
                    for case in suite.cases:
                        with run.trial(case) as trial:
                            try:
                                answer = execute(case.input["prompt"], case.test_case_id, trial)
                            except Exception as error:
                                trial.mark_errored(f"{type(error).__name__}: {error}")
                                continue
                            passed = answer == case.expected["assistant_response"]
                            trial.score("exact_match", passed, passed=passed, report_role="primary_verdict")
            else:
                from agento11y_harbor import run_harbor
                from harbor.models.trial.config import AgentConfig, EnvironmentConfig, TaskConfig, TrialConfig
                from harbor_agent import prepare_tasks

                tasks = prepare_tasks(args.results / "tasks")
                configs = {
                    c["id"]: TrialConfig(
                        task=TaskConfig(path=tasks / c["id"]),
                        trials_dir=args.results / "harbor-native" / run_id,
                        agent=AgentConfig(import_path="harbor_agent:LedgerAgent", model_name=model),
                        environment=EnvironmentConfig(type="docker"),
                    )
                    for c in CASES
                }
                run = asyncio.run(
                    run_harbor(
                        suite,
                        configs,
                        name=f"Real agent / Harbor / {args.model}",
                        experiment_id=run_id,
                        client=sdk,
                        candidate=candidate,
                    )
                )
        (args.results / f"{run_id}-run.json").write_text(
            json.dumps({"experiment_id": run.experiment_id, "url": run.url})
        )
        print(run.url, flush=True)
    finally:
        sdk.shutdown()
        tp.shutdown()
        mp.shutdown()


if __name__ == "__main__":
    main()
