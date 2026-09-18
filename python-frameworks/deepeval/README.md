# DeepEval integration

`agento11y-deepeval` supports live target execution with native DeepEval metrics,
and publication of completed native evaluation results. Both use the existing
Agent Observability experiment lifecycle.

## Install

```bash
pip install agento11y agento11y-deepeval
```

Set `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TOKEN`, and, when required by your
stack, `AGENTO11Y_AUTH_TENANT_ID`. Set `AGENTO11Y_GRAFANA_URL` for experiment
deep links.

## Live runner

`run_deepeval` registers the planned count before invoking the target and exports
each case as it completes. Configure your OTel TracerProvider/exporter once;
the runner activates suite and trial spans around your instrumented target.

```python
from agento11y_deepeval import run_deepeval
from deepeval.metrics import ExactMatchMetric
from deepeval.test_case import LLMTestCase

run = run_deepeval(
    [LLMTestCase(input="2+2?", actual_output="", expected_output="4")],
    lambda case, trial: instrumented_agent(case.input),
    [ExactMatchMetric()],
    experiment_name="Live regression",
)
print(run.url)
```

The target receives the trial for conversation binding when needed. Execution
is sequential, with a fresh copy of each metric per case. Target and metric
exceptions mark the trial errored and retain successful siblings. Target duration
excludes grading. `record_io=False` avoids extra anchor generations by default.
Native batch scheduling, caching, and pytest can continue using the
completed-result importers below.

For `ConversationalTestCase`, the same target receives a private copy of the
case and must return the completed ordered `Sequence[Turn]`. The single-turn
`target(case, trial) -> str` contract is unchanged. Returning turns instead of
mutating the case keeps caller-owned cases reusable:

```python
from deepeval.metrics import ConversationCompletenessMetric
from deepeval.test_case import ConversationalTestCase, Turn

case = ConversationalTestCase(turns=[Turn(role="user", content="Can I return this?")])

def chat(case, trial):
    return [*case.turns, Turn(role="assistant", content="Yes, within 30 days.")]

run_deepeval(
    [case],
    chat,
    [ConversationCompletenessMetric()],
    experiment_name="Live support conversations",
    record_io=True,
)
```

With `record_io=True`, each assistant turn is exported as one generation using
all preceding turns as structured input. Assistant generations share one stable
conversation id and form a parent chain. A final user turn is retained as a
chained input-only generation awaiting a response. If the target already emitted
its own telemetry, bind that conversation on `trial` and leave `record_io=False`.

## Evaluate existing outputs and publish afterward

`evaluate_with_agento11y` below waits for the whole native batch before creating
the Grafana experiment. Use `run_deepeval` for live target execution/progress.

```python
from deepeval.metrics import AnswerRelevancyMetric, FaithfulnessMetric
from deepeval.test_case import LLMTestCase
from agento11y_deepeval import evaluate_with_agento11y

test_cases = [
    LLMTestCase(
        name="refund-policy",
        input="Can I return an opened item?",
        actual_output=run_agent("Can I return an opened item?"),
        expected_output="Opened items can be returned within 30 days.",
        retrieval_context=["Opened products may be returned within 30 days."],
        metadata={"agento11y.test_case_id": "refund-policy"},
    )
]

run = evaluate_with_agento11y(
    test_cases,
    [AnswerRelevancyMetric(), FaithfulnessMetric()],
    experiment_name="Checkout agent regression",
    primary_metric="Answer Relevancy",
    suite_id="checkout-regression",
    suite_version="v1",
)
print(run.published.url)
```

Each DeepEval test result becomes one trial. The metric named by
`primary_metric` is the one `primary_verdict`; every other metric is a
`diagnostic`. If all results contain exactly one metric name, the adapter selects
it automatically. With multiple metrics, the explicit selection is required and
validated before the experiment is written.

Conversational results preserve their ordered public `Turn` fields in the test
case snapshot. With `record_io=True`, each assistant response becomes a separate
generation; preceding user and assistant turns remain structured messages, not a
flattened transcript. Whole-conversation metric scores attach to the typed trial
and conversation, not to the final assistant generation.

```python
from deepeval import evaluate
from deepeval.metrics import ConversationCompletenessMetric
from deepeval.test_case import ConversationalTestCase, Turn

conversation = ConversationalTestCase(
    turns=[
        Turn(role="user", content="Can I return an opened item?"),
        Turn(role="assistant", content="Yes, within 30 days."),
    ],
    expected_outcome="The return policy is explained.",
)
result = evaluate(test_cases=[conversation], metrics=[ConversationCompletenessMetric()])
publish_deepeval_results(result, experiment_name="Support conversations")
```

Only public DeepEval `user` and `assistant` turns with non-blank string content are
accepted. Malformed turns fail validation before the remote experiment is
created. Turn order, user id, retrieval context, tool/MCP call details, audio,
latency, interruption state, comments, and custom metadata are retained in the
case snapshot and generation metadata. Text roles/content and user id also map
to their native Agent Observability fields.

Metric errors make the trial operationally failed and remain unrated; valid
sibling results are still published. Native duration can be supplied as
`metadata={"agento11y.duration_ms": 12345}`. Missing duration remains unknown.

Metric scores, pass/fail values, reasons, thresholds, model identity, evaluator
cost, and evaluator token counts are preserved. Evaluator cost and tokens remain
score metadata rather than candidate trial usage, because they describe the
grader rather than the agent under test.

## Publish a result you already evaluated

```python
from deepeval import evaluate
from agento11y_deepeval import publish_deepeval_results

result = evaluate(test_cases=test_cases, metrics=metrics)
published = publish_deepeval_results(
    result,
    experiment_name="Existing DeepEval run",
    primary_metric="Answer Relevancy",
)
```

## Generation and conversation correlation

The adapter records each single-turn result as one anchor generation by default;
conversational results produce one generation per assistant turn. If the
evaluated target already emits Agent Observability telemetry, use that telemetry
instead:

```python
published = publish_deepeval_results(
    result,
    experiment_name="Instrumented target",
    primary_metric="Answer Relevancy",
    record_io=False,
    resolve_conversation_id=lambda item: item.metadata["conversation_id"],
    resolve_generation_id=lambda item: item.metadata["generation_id"],
)
```

Without resolver functions, metadata keys `agento11y.conversation_id`,
`agento11y.generation_id`, and `agento11y.trace_id` are recognized. A stable case
id can be supplied as `agento11y.test_case_id`. For conversational results, a
resolved conversation id suppresses generation export even when `record_io=True`;
the optional generation id is not attached to whole-conversation scores.

To publish browsable cases, pass `test_suites_client` and explicitly choose
`suite_publication_policy="subset"` to retain remote-only cases or `"replace"`
to prune them. Trial snapshots use the canonical server-returned cases and
record digests when sanitization changed the executed case.

This adapter does not create or invoke Agent Observability stored evaluators.
DeepEval runs metrics locally (including model-backed metrics) and the adapter
publishes their scores.

See the [runnable deterministic example](../../examples/experiments/deepeval/README.md).
