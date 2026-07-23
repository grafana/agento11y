# Go Experiment Example

Runs a tiny framework-free Go agent over a test suite as an Agent Observability experiment.

The shape mirrors the Python v1 experiment examples:

1. Build an agento11y client.
2. Define a `TestSuite` with `TestCase` entries.
3. Open an `ExperimentRun`, run one `WithTrial` callback per test case, record scores, finalize the run, and print a link.

Go does not have a LangGraph adapter in this repo. Existing Go agents should keep their normal `client.StartGeneration(ctx, ...)` instrumentation.

This example uses the production-style shape you would use for LLMSpec or A2A:

1. The experiment harness owns the run, typed trials, and scores.
2. The target sends only the `runID` across a simulated service boundary.
3. The receiving service restores it with `agento11y.WithExperimentRunID(ctx, runID)`.
4. Existing `client.StartGeneration(ctx, ...)` instrumentation automatically tags generations with `experiment.run_id` and `experiment_run_id`.
5. The service returns the generated `generationID` so scores can attach to the right generation.

When the runner and agent are in the same process, use the context passed to `WithTrial` or `run.Context(ctx)`; both tag existing generation instrumentation automatically. When the agent is behind HTTP, A2A, or a task queue, propagate the run ID explicitly and restore it with `WithExperimentRunID`.

This example is designed for Grafana Cloud Agent Observability.

## Run

```bash
cd examples/experiments/go
cp .env.example .env
# Fill in the Grafana Cloud Agent Observability values in .env.
set -a && source .env && set +a
GOWORK=off go run .
```

The canned sample does not call an LLM. Provider keys in `.env.example` are included because real experiment jobs often use them for the agent or grader. The required values for this sample are the Grafana Cloud ingest settings (`AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_MODE`, `AGENTO11Y_AUTH_TENANT_ID`, `AGENTO11Y_AUTH_TOKEN`).
