# Agent Observability framework-free experiment example

A minimal, runnable set of Agent Observability **experiment** examples using the core
`agento11y` only — **no framework adapter**.

There are seven entry points:

| Example | Command | What it demonstrates |
| --- | --- | --- |
| Suite-free deterministic eval | `uv run python -m app.run_suite_free_experiment` | Tracking primitives with no stored/local suite and no model dependency |
| Easy transcript eval | `uv run python -m app.run_experiment` | Candidate transcript, grader transcript, score links, JSON/text artifacts |
| Dashboard image eval | `uv run python -m app.run_dashboard_experiment` | Candidate dashboard spec transcript, rendered pyplot PNG artifact, grader transcript |
| Notebook walkthrough | `experiment_walkthrough.ipynb` | Step-by-step cloud run, suite association, agent call, score export, finalize |
| Push stored suite | `uv run python -m app.push_suite` | Upload local YAML cases into Agent Observability test suites, optionally publish |
| Pull stored suite | `uv run python -m app.pull_suite` | Download an Agent Observability suite as portable YAML |
| Run pulled suite | `uv run python -m app.run_pulled_suite_experiment` | Pull suite cases before running and preserve stored-suite provenance |

Use this shape when your agent is *not* built on a supported framework
(LangGraph, LangChain, OpenAI Agents, ...). For those, prefer the matching
framework example/skill — it auto-captures generation ids from the framework
callback.

| Piece | What it shows | Where |
| --- | --- | --- |
| `experiments.experiment(...)` | Creates/finalizes the run with one ingestion API key | `app/run_experiment.py` |
| `exp.trial(...)` | Creates a typed trial per test case | `app/run_experiment.py` |
| `exp.client.record_generation(...)` | Publishes candidate and grader transcripts as agento11y generations | `app/run_experiment.py` |
| `trial.bind_generation(...)` | Links the candidate transcript to the typed trial | `app/run_experiment.py` |
| LLM judge | Publishes a grader transcript and emits the final score with grader IDs | `app/run_experiment.py` |
| Tiny agent | Plain Anthropic calls for the candidate and grader | `app/agent.py` |
| Pyplot artifact | Renders dashboard specs and uploads PNG artifacts | `app/run_dashboard_experiment.py` |

## How it works

1. `experiments.experiment(...)` calls `POST /api/v1/experiment-runs:upsert`.
2. For each dataset item, `exp.trial(...)` creates a typed trial.
3. The candidate agent runs through Anthropic and the example publishes that
   request/response transcript as an agento11y generation.
4. The grader runs through Anthropic and the example publishes the grader
   prompt/response transcript as a second agento11y generation.
5. The final score links to the candidate generation plus the grader generation,
   then uploads small JSON/text artifacts for inspection.
6. When the dataset is done the experiment finalizes (`completed`/`failed`) and
   prints a deep link.

The dashboard example follows the same flow, but the candidate output is a JSON
dashboard spec. The script renders that spec with `matplotlib.pyplot` and uploads
the PNG as a `dashboard-image` artifact on the trial.

A/B testing is just two runs with different `AGENTO11Y_EXPERIMENT_ID`/`tags` over the same items.

## Prerequisites

- Python 3.11+ and [uv](https://docs.astral.sh/uv/)
- Grafana Cloud Agent Observability endpoint, stack ID, and access policy token
- `ANTHROPIC_API_KEY` only for the model-backed examples

## Run it

```bash
uv sync

# Grafana Cloud Agent Observability ingest API URL.
export AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
export AGENTO11Y_PROTOCOL=http
export AGENTO11Y_AUTH_TENANT_ID=<your-stack-id>
export AGENTO11Y_AUTH_TOKEN=<your-grafana-cloud-access-policy-token>
export AGENTO11Y_GRAFANA_URL=https://<your-stack>.grafana.net

# Required only for stored-suite push/pull through the Grafana plugin proxy.
# AGENTO11Y_CONTROL_ENDPOINT accepts the stack URL or app URL.
export AGENTO11Y_CONTROL_ENDPOINT=https://<your-stack>.grafana.net/a/grafana-sigil-app
export AGENTO11Y_SERVICE_ACCOUNT_TOKEN=<your-grafana-service-account-token>

# Optional: stable experiment id for CI retries / a real model.
export AGENTO11Y_EXPERIMENT_ID=experiment-example-${GIT_SHA:-manual}
export ANTHROPIC_API_KEY=<your-anthropic-api-key>
export AGENT_MODEL=${AGENT_MODEL:-claude-3-5-haiku-latest}
export GRADER_MODEL=${GRADER_MODEL:-$AGENT_MODEL}

uv run python -m app.run_experiment
```

The suite-free example needs only the Agent Observability ingestion configuration above;
it has no Anthropic or control-plane dependency:

```bash
export AGENTO11Y_EXPERIMENT_ID=suite-free-${GIT_SHA:-manual}
uv run python -m app.run_suite_free_experiment
```

For the dashboard/image artifact example, use a different experiment id if you
already finalized the easy run:

```bash
export AGENTO11Y_EXPERIMENT_ID=dashboard-example-${GIT_SHA:-manual}
uv run python -m app.run_dashboard_experiment
```

To push the sample YAML suite into Agent Observability test suites and publish it:

```bash
export AGENTO11Y_PUBLISH_SUITE=true
# Optional: delete cases from the draft that are absent from the YAML file.
export AGENTO11Y_PRUNE_SUITE=true
uv run python -m app.push_suite
```

To pull a stored suite back to the portable dataset shape:

```bash
export AGENTO11Y_SUITE_ID=dashboard-regression
export AGENTO11Y_SUITE_VERSION=latest_published
uv run python -m app.pull_suite
```

To run an experiment from that stored suite:

```bash
export AGENTO11Y_EXPERIMENT_ID=pulled-suite-${GIT_SHA:-manual}
uv run python -m app.run_pulled_suite_experiment
```

You should see output like:

```
Experiment 'experiment-example-manual' finished: 3 score(s) accepted.
pass_rate=1.00 mean_score=1.00
View in Agent Observability: https://<your-stack>.grafana.net/a/grafana-sigil-app/experiments/runs/experiment-example-manual
```

> The deep link uses `AGENTO11Y_GRAFANA_URL`; keep it pointed at your Grafana stack
> UI host. This can differ from `AGENTO11Y_ENDPOINT` when API and UI hosts differ.

## Adapt it

- **Real agent:** replace `answer_question(...)` with your agent. If your normal
  instrumentation already publishes generations, bind its generation id with
  `trial.bind_generation(...)` instead of calling `record_generation(...)`.
- **Real grading:** replace `grade_answer(...)` with your evaluator. If it uses
  an LLM, publish that grader transcript and pass `grader_conversation_id` /
  `grader_generation_id` on the score.
- **Image artifacts:** use `trial.artifact("name", path="/tmp/file.png")` after
  rendering the file before upload. The dashboard example uses this for pyplot PNGs.
- **CI gate:** inspect `report.summary.pass_rate` and exit non-zero to
  fail a pull request.
