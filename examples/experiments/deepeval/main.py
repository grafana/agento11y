import argparse

from agento11y.experiments import Client, TestSuitesClient
from agento11y_deepeval import evaluate_with_agento11y
from deepeval.metrics import ExactMatchMetric
from deepeval.test_case import LLMTestCase


def answer(question: str) -> str:
    return {
        "What is the capital of France?": "Paris",
        "What is 2 + 2?": "5",  # Deliberate regression: expected answer remains 4.
    }.get(question, "unknown")


questions = [
    ("capital-fr", "What is the capital of France?", "Paris"),
    ("addition", "What is 2 + 2?", "4"),
]
test_cases = [
    LLMTestCase(
        name=test_case_id,
        input=question,
        actual_output=answer(question),
        expected_output=expected,
        metadata={"agento11y.test_case_id": test_case_id},
    )
    for test_case_id, question, expected in questions
]

parser = argparse.ArgumentParser()
parser.add_argument("--local", action="store_true", help="Publish the suite and run to auth-disabled Docker Sigil only")
args = parser.parse_args()
client = (
    Client("http://localhost:8080", ingest_token="local-development", grafana_url="http://localhost:3000")
    if args.local
    else None
)
suites = (
    TestSuitesClient(control_endpoint="http://localhost:8080/api/v1/eval", service_account_token="local-development")
    if args.local
    else None
)

run = evaluate_with_agento11y(
    test_cases,
    [ExactMatchMetric()],
    experiment_name="DeepEval deterministic example",
    suite_id="deepeval-example",
    client=client,
    test_suites_client=suites,
    suite_publication_policy="subset" if suites else None,
)
print(run.published.url or run.published.experiment_id)
if client:
    client.shutdown()
