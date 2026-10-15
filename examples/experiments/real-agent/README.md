# Compare an instrumented agent in Grafana Cloud

Run ten ledger questions through Grafana Evals, DeepEval, or Harbor and inspect
the resulting experiments in Grafana Cloud Agent Observability. The agent calls
`read_orders`, reasons over the ledger, and calls `submit_answer`. Each candidate
makes its own Anthropic requests; the example makes **paid model calls**.

Expected answers are test data used by deterministic graders. They are not
provided as the agent's responses. This small tool agent demonstrates
instrumentation and evaluation; the task set is not a model-quality benchmark.

## Prerequisites

- A Grafana Cloud stack with Agent Observability enabled.
- Agent Observability ingestion credentials and a Grafana service-account token
  with permission to create, read, and publish test suites.
- Cloud OTLP endpoint and authentication settings for traces and metrics.
- An `ANTHROPIC_API_KEY` with access to the model IDs in [agent.py](agent.py).
- Python 3.12 or later, `uv`, and this SDK checkout. The Harbor runner also
  needs Docker.

## Configure Cloud export

Find your endpoint, stack ID, and access-policy token in the
[Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/).
Set these variables in the runner environment or secret manager:

| Variable | Value |
| --- | --- |
| `AGENTO11Y_ENDPOINT` | Your Cloud Agent Observability ingestion URL. |
| `AGENTO11Y_AUTH_TENANT_ID` | Your ingestion instance or stack ID. |
| `AGENTO11Y_AUTH_TOKEN` | Your Cloud ingestion access-policy token. |
| `AGENTO11Y_GRAFANA_URL` | Your Grafana Cloud stack URL, such as `https://your-stack.grafana.net`. |
| `AGENTO11Y_SERVICE_ACCOUNT_TOKEN` | Your Grafana service-account token for stored suites. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Your Cloud OTLP HTTP endpoint. |
| `OTEL_EXPORTER_OTLP_HEADERS` | The authentication header configuration supplied by your Cloud OTLP setup. |
| `ANTHROPIC_API_KEY` | Your Anthropic API key. |

The experiment client and suite client use the Agent Observability and Grafana
credentials respectively. The OpenTelemetry exporters use the OTLP settings.
Keep all three configured for your intended Cloud stack. Model requests go to
Anthropic. Do not commit credentials or result files containing agent content.

## Run candidates

From this directory, run the Grafana runner for each candidate:

```sh
uv run python run.py grafana \
  --model haiku --batch ledger-comparison-1 --results results/ledger-comparison-1
```

Repeat with `--model sonnet` and the same `--batch` and `--results` values, so
both candidates run the same published suite version. Use a new batch name for
the next comparison, because a finalized run can't be overwritten. The model
labels resolve to the IDs in [agent.py](agent.py); check their availability and
the example's rate card before running. A case that fails is recorded as
errored and the remaining cases still run.

For the DeepEval or Harbor runner, add its extra and replace `grafana`:

```sh
uv run --extra deepeval python run.py deepeval \
  --model haiku --batch ledger-comparison-1 --results results/ledger-comparison-1
uv run --extra harbor python run.py harbor \
  --model haiku --batch ledger-comparison-1 --results results/ledger-comparison-1
```

Harbor creates Docker tasks and runs its native verifier. Its hosted custom
agent instruments the model calls and the tools that read and write sandbox
files. Instrumenting an installed coding agent requires that agent's own
integration.

For Promptfoo, use the separate [Promptfoo example](../promptfoo/README.md).

## Inspect results

Open the printed experiment URL in your Grafana Cloud stack. Check completed
versus planned trials, rated-case coverage, primary verdicts, and linked
conversations before comparing candidates. Grafana uses a published checkpoint;
the DeepEval and Harbor paths use versioned native case snapshots and their
native grading. Compare candidates within the same runner and case set.

The results directory contains per-case provider responses and native runner
artifacts for inspection. Token counts come from provider usage. Reported cost
uses the example's rate card and is an estimate, not an invoice; review the
rates when changing models. Harbor target duration excludes sandbox startup
and verification. Quality failures remain visible in the report.

These runners grade locally, so their runs can't pass the
[checkpoint gate](../ci/README.md#gate-a-published-suite-version), which
requires the evaluators pinned to a version in Grafana Cloud. To gate CI, run
your agent with
[`run_checkpoint`](../python/README.md#run-a-published-suite-version-with-your-agent).
