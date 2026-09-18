from agento11y.experiments import TestSuitesClient
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

suites = TestSuitesClient()

run = evaluate_with_agento11y(
    test_cases,
    [ExactMatchMetric()],
    experiment_name="DeepEval deterministic example",
    suite_id="deepeval-example",
    test_suites_client=suites,
    suite_publication_policy="subset",
)
print(run.published.url or run.published.experiment_id)
