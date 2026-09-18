# DeepEval experiment example

**Start here:** [Live getting-started example](GETTING_STARTED.md) — run an
instrumented agent, watch trial progress, and inspect real usage in Grafana.

This example evaluates two canned answers with DeepEval's deterministic exact
match metric and publishes them to Agent Observability. One answer is deliberately
wrong, so the expected pass rate is 50%. It needs no LLM API key.

## Local Docker run

Run `uv run python main.py --local` against Sigil on localhost:8080. This mode
uses explicit local clients, publishes browsable test cases and an immutable suite
version, and prints a localhost:3000 report link. It records input/output anchors,
not real LLM traces. Disable framework telemetry with `DEEPEVAL_TELEMETRY_OPT_OUT=YES`.

To publish stored cases from your own integration, pass
`test_suites_client=TestSuitesClient()` to `evaluate_with_agento11y` or
`publish_deepeval_results`, together with `suite_publication_policy="subset"`
to retain remote-only cases or `"replace"` to prune them. This is an explicit control-plane write requiring a
Grafana service account in Cloud. The adapter tags the suite `framework:deepeval`,
publishes a checkpoint, and pins the experiment to the returned version. Omit it
when you only need run snapshots/provenance, not a stored browsable suite.

## Cloud configuration

```bash
cd examples/experiments/deepeval
cp .env.example .env
# Fill in the Agent Observability values.
set -a && source .env && set +a
uv run python main.py
```

With one metric, the adapter selects it automatically as the
`primary_verdict`. When using multiple metrics, pass the exact DeepEval metric
name explicitly, for example `primary_metric="Answer Relevancy"`; all other
metrics are published as diagnostics.

By default the adapter records each DeepEval input and actual output as an
anchor generation. Pass `record_io=False` if the target is already instrumented,
and provide `agento11y.conversation_id` and `agento11y.generation_id` in each
test case's metadata to bind the existing telemetry.

## Cloud ground-truth smoke test

`cloud.py` runs a deterministic test agent inside SDK trials, measures DeepEval
Exact Match as a `diagnostic`, uploads a JSON artifact, and invokes a stored
cloud judge as `primary_verdict`. It pushes/publishes and pulls a test suite
from `cases.json`; the cloud prompt references `{{test_case.expected.answer.text}}`.
One answer is deliberately wrong, so a successful full run should report 50% passing.
This exercises DeepEval directly inside a trial, not the bulk result adapter above.

With ingest and control-plane credentials already exported in your shell:

```bash
gcx --context grafana-dev agento11y evaluators upsert -f cloud-evaluator.json
DEEPEVAL_TELEMETRY_OPT_OUT=YES uv run python cloud.py
# Substitute the printed experiment ID:
gcx --context grafana-dev agento11y experiments get <experiment-id> -o json
gcx --context grafana-dev agento11y experiments list-trials <experiment-id> -o json
gcx --context grafana-dev agento11y experiments get-report <experiment-id> -o json
```

The example uses the local editable SDK. It requires
`AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TOKEN`, `AGENTO11Y_AUTH_TENANT_ID`,
`AGENTO11Y_CONTROL_ENDPOINT`, and `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`.
The backend must have test-case variables enabled and the evaluator's provider
configured. Cloud grading incurs model usage. No credential file is loaded by
this example. Each invocation publishes a suite version and creates a new run.

### Dev verification: 2026-09-11

- Suite `deepeval-dev-ground-truth-smoke@v2` was published and pulled successfully.
- Run `deepeval-cloud-d4c3f5c49238` reached the cloud evaluation worker, but failed
  with `judge provider "anthropic" is not configured`. Live testing stopped there.
- `gcx` verified the failed run, nested expected-answer snapshot, conversation
  binding, DeepEval score of 1, and uploaded `test-result.json` artifact.
- Python was missing cloud-evaluation `report_role` plumbing; added it through
  `Trial.evaluate`, `Client`, transport, and the returned evaluation model, with
  a regression check (166 focused tests passed).
- Retried using available provider `anthropic-vertex`, model `claude-sonnet-4-6`,
  and a new immutable evaluator version `2026-09-11-vertex`.
  Run `deepeval-cloud-b9b8bbdc59fe` completed both cases against suite version `v3`:
  the cloud judge returned true for Paris and false for the deliberately wrong 5.
  Both DeepEval diagnostics and JSON artifacts are present.
- Raw `gcx api` verifies cloud scores carry `primary_verdict` and local scores
  carry `diagnostic`. The installed typed `gcx ... get-report` drops those fields
  and renders absent summary values as zeros. Verify new fields with:

  ```bash
  gcx --context grafana-dev api /api/plugins/grafana-agento11y-app/resources/eval/experiments/deepeval-cloud-b9b8bbdc59fe/report -o json
  ```

- Backend reporting remains incorrect: the raw report returns `pass_denominator: 0`
  and `final_score_count: 0` despite the two cloud primary verdicts, instead of
  the expected 50% pass rate. No legacy `final` score was emitted to mask this.
  This is observed behavior; the backend root cause has not been isolated.
