"""Deterministic Grafana Agent O11y Evals demo. Local Docker only, no model calls."""

from agento11y.experiments import Client, TestCase, TestSuite, TestSuitesClient, run_evals


def main() -> None:
    suites = TestSuitesClient(
        control_endpoint="http://localhost:8080/api/v1/eval",
        service_account_token="local-development",
    )
    published = suites.push_suite(
        TestSuite(
            suite_id="grafana-evals-demo",
            name="Grafana Agent O11y Evals demo",
            tags=["framework:grafana"],
            test_cases=[
                TestCase(
                    "capital-fr", input={"prompt": "Capital of France?"}, expected={"assistant_response": "Paris"}
                ),
                TestCase("addition", input={"prompt": "2 + 2?"}, expected={"assistant_response": "4"}),
            ],
        ),
        publish=True,
    )
    suite = suites.pull_suite(published.suite_id, published.suite_version)
    client = Client("http://localhost:8080", ingest_token="local-development", grafana_url="http://localhost:3000")
    for version, addition in [("v1", "5"), ("v2", "4")]:
        answers = {"Capital of France?": "Paris", "2 + 2?": addition}
        run = run_evals(
            suite,
            answers.__getitem__,
            name=f"Grafana Evals demo {version}",
            client=client,
            candidate={"agent_name": "grafana-evals-demo", "agent_version": version},
            record_io=True,
        )
        print(run.url)
    client.shutdown()


if __name__ == "__main__":
    main()
