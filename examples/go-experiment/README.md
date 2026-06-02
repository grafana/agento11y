# Go Experiment Example

Runs a tiny framework-free Go agent over a dataset as a Sigil experiment.

The shape mirrors the Python experiment examples:

1. Build a Sigil client.
2. Define a dataset, target, and scorer.
3. Run `ExperimentRunner`, which creates the experiment, passes an experiment-aware context into the target, exports scores, finalizes the run, and prints a link.

Go does not have a LangGraph adapter in this repo. Existing Go agents should keep their normal `client.StartGeneration(ctx, ...)` instrumentation; the runner passes a context that adds the experiment run ID and captures generation IDs automatically.

This example is designed for Grafana Cloud AI Observability. There is no supported local Grafana path for users; local/self-hosted Sigil is only a development override for SDK contributors.

## Run

```bash
cd examples/go-experiment
cp .env.example .env
# Fill in the Grafana Cloud Sigil values in .env.
set -a && source .env && set +a
GOWORK=off go run .
```

The canned sample does not call an LLM. Provider keys in `.env.example` are included because real experiment jobs often use them for the agent or grader. The required values for this sample are the Grafana Cloud ingest settings (`SIGIL_ENDPOINT`, `SIGIL_AUTH_MODE`, `SIGIL_AUTH_TENANT_ID`, `SIGIL_AUTH_TOKEN`) and eval settings (`SIGIL_EVAL_ENDPOINT`, `SIGIL_EVAL_PATH_PREFIX`, `SIGIL_EVAL_AUTH_TOKEN`).
