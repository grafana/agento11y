# Harbor integration

`agento11y-harbor` publishes [Harbor](https://github.com/harbor-framework/harbor)
evaluations to Agent Observability in Grafana Cloud. You can publish a job
while it runs, import a job that already finished, or run explicit Harbor
trials from Python.

## Install

Install the package in the same Python environment as Harbor, so Harbor can
discover the `agento11y` job plugin. With a `uv` tool install:

```bash
uv tool install harbor --with-executables-from agento11y-harbor
```

Or with `pip`, in Harbor's environment:

```bash
pip install agento11y-harbor
```

The package needs `agento11y` 0.20.0 or later and a Harbor release that
supports job plugins (`harbor run --plugin`).

## Configure Grafana Cloud

Set these environment variables before you run or import a job:

- `AGENTO11Y_ENDPOINT`: your Grafana Cloud Agent Observability endpoint.
- `AGENTO11Y_AUTH_TOKEN` and, when your stack requires it,
  `AGENTO11Y_AUTH_TENANT_ID`: your Grafana Cloud ingestion credentials.
- `AGENTO11Y_GRAFANA_URL`: your Grafana Cloud stack URL, for experiment links.
- `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`: a Grafana service account token. You need
  it only to publish a suite checkpoint with `--publish-suite`.

Refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/)
for the credentials. `agento11y harbor` also loads the credentials that
`agento11y login` saved.

## Publish a job while it runs

Run Harbor through the `agento11y` CLI. Every argument after `--` goes to
`harbor run` unchanged:

```bash
agento11y harbor -- \
  --dataset terminal-bench@2.0 \
  --agent codex \
  --model openai/gpt-5
```

`agento11y harbor run -- ...` is the same command. The launcher adds
`--plugin agento11y`, so `harbor run --plugin agento11y ...` works too. Pass
plugin options with `--plugin-kwarg`, for example
`--plugin-kwarg pass_threshold=0.5`. The options are `name`, `experiment_id`,
`main_reward` (default `reward`), `pass_threshold` (default `1.0`), and
`upload_artifacts` (default `true`).

The plugin creates one experiment per agent, model, and dataset. Each trial
appears when Harbor starts its agent, after the environment and agent setup,
so the experiment records the agent version Harbor detects. A trial whose
setup fails appears when it ends. When the trial ends, the plugin attaches its
rewards, duration, tokens, cost, errors, and verifier artifacts. The main
reward is the pass/fail verdict, and other rewards are diagnostics.

If your agent publishes its own conversation, put its ID in
`AgentContext.metadata["agento11y.conversation_id"]`, and the trial links to
it. Otherwise, the plugin imports the trial's ATIF trajectory as a
conversation. ATIF import creates generations, not traces. Each tool call's
arguments keep their first 4 KiB, and each step's prompt and response keep
their first 1 MiB, so one large tool result can't fail the import.

## Import a completed job

Publish a Harbor job directory that already finished. Harbor writes jobs to
`jobs/` in the directory you ran it from, unless you pass `--jobs-dir`:

```bash
agento11y harbor export jobs/<job-name>
```

The importer reads Harbor's `result.json`, ATIF, reward, and verifier files.
It doesn't need Harbor installed. Run `agento11y harbor export --help` for the
flags:

- `--experiment-id`: the base experiment ID. The default is the Harbor job ID.
- `--suite-id`: the suite ID for the imported cases. The default is derived
  from the dataset. On its own, it doesn't publish anything.
- `--publish-suite subset|replace`: store the job's tasks as a published suite
  checkpoint before the import. `subset` adds and updates cases. `replace` also
  removes stored cases that aren't in the job.
- `--main-reward`, `--pass-threshold`, and `--no-artifacts`: the same options
  as the plugin.

From Python, call `publish_harbor_job`:

```python
from agento11y.experiments import TestSuitesClient
from agento11y_harbor import publish_harbor_job

published = publish_harbor_job(
    "jobs/my-job",
    test_suites_client=TestSuitesClient(),
    suite_publication_policy="subset",
)
for run in published.runs:
    print(run.url)
```

## Experiment IDs

The plugin and the importer use the same experiment ID:
`<experiment_id or Harbor job ID>-<candidate hash>`. The hash comes from the
agent name, model, and dataset, so it's the same for every run of the same
candidate.

A finalized experiment can't change. When you import a job, a candidate whose
experiment is already finalized, either live or by an earlier import, is
skipped with a warning, and the import fails if every candidate is. Pass a new
`--experiment-id` to publish the job as separate experiments. Resuming a
Harbor job also reuses the job ID, so pass a new `experiment_id` plugin option
when the job's experiments already finalized.

## Errors and retries

Publishing problems never stop the Harbor job. When a trial can't be
published, or one of its files can't be read, that trial is marked errored and
the rest of the job continues. The plugin logs a warning. The errored trial's
rewards stay diagnostic.

An agent timeout or non-zero exit doesn't error a trial when Harbor's verifier
still produced the main reward: like Harbor's own metrics, the reward decides
pass or fail, and the agent error is kept in the score metadata. Any other
Harbor exception, or a missing main reward, errors the trial.

Any errored trial finalizes its experiment as failed, and the experiment error
lists the errored trials. An importer skips a trial whose `result.json` it
can't read and logs a warning. Each candidate is imported on its own, so one
that fails to publish doesn't stop the others. The export command lists it and
exits with status 1, and rerunning the command publishes only what's missing.

If the plugin can't create an experiment, the candidate's next trial tries
again. Once it exists, the experiment is finalized as failed, because it's
missing the trials that started before then. If it never exists, the plugin logs
how many trials it left out when the job ends. Either way, export the job
directory to publish every trial, with a new `--experiment-id` if the
experiment exists.

If the Harbor process is interrupted, for example with Ctrl-C, the plugin marks
trials without a result errored and finalizes the experiments as failed. To
publish the finished trials, export the job directory with a new
`--experiment-id`.

Harbor retries a failed trial under the same trial name. The plugin keeps one
trial per name and publishes only the final attempt, so a failure that passes
on retry counts once, as a pass. The importer sees the same result, because
Harbor deletes the files of superseded attempts. Separate attempts from
`--n-attempts` are separate trials. Their attempt numbers follow the order
their agents start live and trial-name order on import, so compare them by experiment, not by
attempt number.

## Run trials from Python

`run_harbor` runs explicit Harbor `TrialConfig` objects for one agent and
model, one case at a time. It needs Harbor installed:

```python
from agento11y_harbor import run_harbor

run = await run_harbor(suite, configs, name="Harbor regression", agent_env=otel_env)
print(run.url)
```

`configs` maps each suite case ID to a `harbor.models.trial.config.TrialConfig`.
The runner adds `agent_env`, `TRACEPARENT`, `TRACESTATE`, and the
`AGENTO11Y_*` trial identity to each agent's environment. A Python agent can
continue the trace and read the trial with `TrialRef.from_env()`:

```python
import os
from opentelemetry import context, propagate

carrier = {key.lower(): os.environ[key] for key in ("TRACEPARENT", "TRACESTATE") if key in os.environ}
token = context.attach(propagate.extract(carrier))
try:
    run_instrumented_agent()
finally:
    context.detach(token)
```

The live plugin can't add environment variables to agents, because Harbor
creates each agent before the plugin's hooks run. With the plugin, link
conversations through `AgentContext.metadata` instead.

`run_harbor` doesn't retry trials or run them concurrently. Use
`agento11y harbor` for Harbor's scheduler, retries, and datasets.
