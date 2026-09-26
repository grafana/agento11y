# AI Education Assistant Setup Guide

This guide starts the grading service, agent, background traffic, and a six-case
Agent Observability experiment. Run all commands from the
`ai-education-assistant` directory unless a command changes directories.

## 1. Prerequisites

- Python 3.11 or newer
- [`uv`](https://docs.astral.sh/uv/getting-started/installation/)
- An Anthropic API key
- A Grafana Cloud stack with Agent Observability enabled

## 2. Create the Grafana credentials

The demo uses two separate Agent Observability credentials:

1. **Ingestion access**: create a Grafana Cloud access policy token with the
   `sigil:write` scope. Record the token, Agent Observability ingestion endpoint,
   and stack ID. These become `AGENTO11Y_AUTH_TOKEN`, `AGENTO11Y_ENDPOINT`, and
   `AGENTO11Y_AUTH_TENANT_ID`.
2. **Control-plane access**: create a Grafana service account with the Agent
   Observability Admin role, then create a service-account token.
   This becomes `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`; the corresponding endpoint is
   `https://<stack>.grafana.net/a/grafana-agento11y-app`.

The control-plane token publishes and pulls test suites. The ingestion token
writes conversations, generations, experiment runs, trials, and scores. They are
not interchangeable.

For service telemetry, also create or reuse a Grafana Cloud access policy token
with `metrics:write`, `traces:write`, and `logs:write`. Grafana's OTLP setup page
provides the gateway URL and a ready-to-use `Authorization` header. The same
access policy token may cover Agent Observability ingestion and OTLP if it has all
required scopes, but the control-plane credential must still be a service-account
token.

See Grafana's [Agent Observability setup](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/grafana-cloud/)
and [experiment authentication](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/experiments/#authenticate-experiment-operations)
documentation for the current UI steps and permissions.

## 3. Configure local environment files

Environment files are ignored by Git. Copy the safe templates:

```bash
cp grading-service/.env.example grading-service/.env
cp education-agent/.env.example education-agent/.env
```

Edit `grading-service/.env` and replace the OTLP placeholders. Keep
`ASSIGNMENT_STORE_FAILURE=false` for the normal demo. The fixture API used by
the experiment runner is disabled by default; set `ENABLE_TEST_FIXTURES=true`
only while running the local experiment in step 7.

Edit `education-agent/.env` and replace these placeholders:

```dotenv
ANTHROPIC_API_KEY=<anthropic-api-key>
AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
AGENTO11Y_PROTOCOL=http
AGENTO11Y_AUTH_TOKEN=<grafana-cloud-access-policy-token>
AGENTO11Y_AUTH_TENANT_ID=<grafana-cloud-stack-id>
AGENTO11Y_GRAFANA_URL=https://<stack>.grafana.net
AGENTO11Y_CONTROL_ENDPOINT=https://<stack>.grafana.net/a/grafana-agento11y-app
AGENTO11Y_SERVICE_ACCOUNT_TOKEN=<grafana-service-account-token>
```

Also copy the completed `OTEL_EXPORTER_OTLP_*` values from
`grading-service/.env` into `education-agent/.env`. Using the same Grafana stack
keeps the agent and grading-service spans together in one distributed trace.

Never commit either `.env` file or paste tokens into source files.

## 4. Install and run the grading service

```bash
cd grading-service
uv sync
uv run uvicorn app.main:app --host 127.0.0.1 --port 18181
```

In another terminal, verify it:

```bash
curl --fail http://127.0.0.1:18181/health
```

The expected response is `{"status":"ok"}`.

## 5. Install and run the agent

```bash
cd education-agent
uv sync
uv run uvicorn app.main:app --host 127.0.0.1 --port 8083
```

Open <http://127.0.0.1:8083>. Select **Low Score Student** and ask about the
first quiz to produce the intentionally unsafe baseline conversation.

Every assistant reply shows **Good** and **Bad** controls. **Bad** reveals an
optional comment box; **Good** submits immediately. Either one calls
`POST /api/feedback`, which records a conversation rating through
`agento11y.Client.submit_conversation_rating`. Confirm it worked by checking the
conversation in Agent Observability for the rating and comment, or — if OTLP is
configured — by finding the `ai-education-assistant.feedback` span in the trace.

## 6. Generate background traffic

With the grading service still running:

```bash
cd grading-service
uv run python traffic.py
```

The generator sends read-only requests every two seconds. Stop it with `Ctrl-C`.

## 7. Publish the suite and run an experiment

Restart the grading service with `ENABLE_TEST_FIXTURES=true`. Bind it to
`127.0.0.1` as shown above; this mutation endpoint is intended only for the
local experiment runner. The experiment invokes the agent in-process, so the
separately running browser agent is optional for this step.

On the first run, publish the source-controlled suite and immediately execute it:

```bash
cd education-agent
uv run python evals/run_experiment.py --publish-suite
```

This command uses the control-plane token to publish
`evals/ai-teaching-assistant-suite.yaml`, then uses the ingestion token to create
the experiment, trials, generations, and scores. It prints the Agent
Observability experiment URL when complete.

For later runs, pull the latest published suite without creating another suite
version:

```bash
uv run python evals/run_experiment.py
```

Each case seeds its own grade data through the local test-fixture endpoint before
calling the real LangGraph agent. A nonzero exit status means at least one case
failed evaluation — the LLM judge for all six cases, plus a deterministic
`RegexJudge` check for the `unsafe-drop-class-low-score` safety case specifically;
the experiment is still recorded either way.

Set `ENABLE_TEST_FIXTURES=false` and restart the grading service when the
experiment is complete.

## 8. Optional outage trace

Restart the grading service with `ASSIGNMENT_STORE_FAILURE=true`, select
**Outage Student** in the UI, and ask for a grade summary. The request returns
a deliberate 503 and records nested `assignment_store` and
`assignment_record_lookup` error spans. Set the variable back to `false` and
restart afterward.

## 9. Pre-commit safety check

From the repository root:

```bash
git status --short
git check-ignore -v \
  examples/ai-education-assistant/grading-service/.env \
  examples/ai-education-assistant/education-agent/.env
git diff --check
```

Only `.env.example` templates should appear in a commit. The repository-level
`.gitignore` also excludes `.env`, virtual environments, bytecode, and cache
directories.
