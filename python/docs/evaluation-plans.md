# Opinionated Grafana Evals (Python)

Use `run_evals` with an `EvaluationPlan` for reference-based correctness,
rubrics, deterministic checks and multiple judges. It uses the existing
experiment/trial APIs; there is no second evaluator engine or scheduler.

```python
from agento11y.experiments import (
    Check, EvaluationPlan, RegexJudge, StoredEvaluator,
    TestSuite, run_evals, setup_evals, text_case,
)

# Once per process, before constructing your instrumented agent/client.
# Reads OTEL_EXPORTER_OTLP_ENDPOINT and standard OTLP authentication variables.
telemetry = setup_evals()
suite = TestSuite("support", version="1", test_cases=[
    text_case("returns", "Can I return an opened item?",
              expected="Yes, within 30 days.",
              rubric="State the deadline without inventing exclusions."),
])
plan = EvaluationPlan([
    Check("correctness", StoredEvaluator.llm_judge(
        "support.correctness", provider="anthropic", model="claude-haiku-4-5-20251001")),
    Check("policy", StoredEvaluator.llm_judge(
        "support.policy", provider="anthropic", model="claude-haiku-4-5-20251001",
        mode="rubric")),
    Check("nonempty", RegexJudge("support.nonempty", r"\S")),
    Check("brief", RegexJudge("support.brief", r"^.{1,200}$"), required=False),
])
try:
    run = run_evals(suite, instrumented_agent, plan=plan)
    print(run.url)
finally:
    # Shut down your agent's client first, then providers owned by this helper.
    telemetry.shutdown()
```

`instrumented_agent` is your existing callable, not a new SDK-owned agent.
For a complete executable example with actual provider instrumentation and a
local + remote LLM judge, see
[`judged.py`](../../examples/experiments/grafana/judged.py).
That example makes paid calls; deterministic contract tests do not.

## Cases and ground truth

`text_case` produces this explicit `grafana.text.v1` convention:

```json
{
  "input": {"prompt": "Can I return an opened item?"},
  "expected": {
    "assistant_response": "Yes, within 30 days.",
    "rubric": "State the deadline without inventing exclusions."
  },
  "metadata": {"case_format": "grafana.text.v1"}
}
```

References are reviewed answers, never automatically copied observed outputs.
An empty reference string is valid; a missing or null reference is not equivalent.
Ground truth is supplied to judges, never automatically to the agent.

The default target receives `input.prompt`. If it needs structured context,
pass `input_selector="input"` and accept the whole input dictionary in your
target. The runner passes a copy so agent code cannot mutate case snapshots.
The selector cannot point at expected values or metadata.

Custom JSON remains available through `TestCase`. No automatic aliases or shape
conversions are performed. Select fields explicitly:

```python
judge = StoredEvaluator.llm_judge(
    "custom.correctness", provider="anthropic", model="claude-haiku-4-5-20251001",
    input_selector="input.question", expected_selector="expected.answer.text",
)
```

Modes are explicit: `reference` (default) requires the selected reference;
`rubric` requires the selected rubric; `reference_free` requires neither and
should have a task-appropriate `instruction`. Use separate checks when both
reference correctness and rubric compliance are required.

Local `LLMJudge.for_case(...)` accepts the same modes/selectors. Its `invoke`
callback receives the rendered prompt and returns a provider response; usage
and grader evidence are recorded separately from the agent conversation.
It does not also instrument an arbitrary provider callback. Avoid double
recording a judge by instrumenting that callback and publishing another copy
of its generation.

## Stored evaluators and versions

The remote prompt contains `{{test_case.input.prompt}}`,
`{{test_case.expected.assistant_response}}`, or `{{test_case.expected.rubric}}`.
These are evaluator configuration, not extra `trial.evaluate()` arguments.
Sigil resolves them from the trial snapshot. Local helpers use the same nested
case syntax and validate missing paths before invoking a judge. Whole objects
are rendered as JSON. Substitution is single-pass: data containing template
syntax cannot introduce another substitution.

The plan validates **all cases before any target call, run write or evaluator
provisioning**. Stored case placeholders belong in `user_prompt`. Existing
local `{input}`, `{expected}`, `{output}` templates continue to work.

`EvaluatorsClient.ensure(definition)` creates or verifies the exact definition.
`StoredEvaluator.llm_judge` derives a content-addressed ID/version from its
configuration. Identical definitions are reused; changed model, instructions
or selectors create a different identity. Plans pin those exact versions and
record each check's identity and required/diagnostic role in run metadata.

For manually versioned definitions, construct
`StoredEvaluator(id, version, config, output_keys, kind=...)`. Updating means
creating a new version, never overwriting one. The API currently reads the
latest version by ID: if a conflicting older version cannot be read back for
verification, provisioning fails closed. Content-addressed helpers avoid this
ambiguity. Provisioning is explicit, not an automatic online-evaluation rule.

Provisioning uses `AGENTO11Y_CONTROL_ENDPOINT` / `AGENTO11Y_GRAFANA_URL` and
`AGENTO11Y_SERVICE_ACCOUNT_TOKEN`, reusing the suite client's control transport.
Run publication uses the separate `AGENTO11Y_ENDPOINT` and
`AGENTO11Y_AUTH_TOKEN`. No credential is stored in evaluator metadata.
Pass `evaluators_client=EvaluatorsClient(control=your_suite_client)` to reuse
an existing control connection.

## Verdict and failure policy

All required checks must pass. Every individual result is diagnostic; the
runner writes exactly one `overall` primary verdict. Optional diagnostics
cannot improve or worsen the headline. Use `ExactMatch()` for strict equality
against the reference, not as a default measure of open-ended answer quality.

A required evaluator failure produces an explicit trial error and **no primary
rating**. A negative judgment is a completed, rated failure instead. Optional
judge failures leave error evidence but do not block required checks. Remaining
checks and cases execute after failures. No averaging, voting or inferred
weights are applied. Runs without a plan retain the existing callback/default
exact-match behavior.

Remote checks use `trial.evaluate` and verify the stored rated score—not just
successful work-item completion. Evaluation is sequential. Pending remote work
still prevents finalization; a timeout is not cancellation. Run status and
per-trial error/coverage counts must be considered together.

## Live telemetry and correlation

`setup_evals()` installs HTTP/protobuf trace and metric exporters only where
providers are absent. It reuses application-owned providers and never shuts
them down. Call it once per process, and shut it down at process exit. Missing
exporter endpoints fail explicitly rather than silently discarding telemetry.

`run_evals` registers the planned trial count, activates suite/trial spans and
sets the normal SDK conversation context during target execution. Provider
wrappers inherit it and publish real agent generations, tokens and spans.
`record_io` stays off by default; it creates anchors, not agent instrumentation.

When the agent owns another conversation ID or export client, return
`AgentOutput(text, conversation_id=..., flush=agent_client.flush)`.
The runner flushes that client and its own generation client before judging.
Actual provider usage is not fabricated; pricing, attribution and missing
measurements retain their normal backend semantics. Expected answers do not
become telemetry from the agent.

These convenience APIs currently belong to the Python default runner. The
stored JSON, evaluator definitions and existing trial APIs remain portable;
this does not claim convenience-API parity across languages or native runners.
