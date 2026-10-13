# Harbor experiments

This example publishes [Harbor](https://github.com/harbor-framework/harbor)
evaluations to Agent Observability in Grafana Cloud. It covers the live
`agento11y harbor` command and an import of a completed job.

## Before you begin

You need:

- Docker, for Harbor's task sandboxes.
- The `agento11y` CLI. Run `agento11y login`, or set the Grafana Cloud
  variables from the [Harbor integration guide](../../../python-frameworks/harbor/README.md#configure-grafana-cloud).
- Harbor with the `agento11y-harbor` package in the same environment:

  ```bash
  uv tool install harbor --with-executables-from agento11y-harbor
  ```

## Publish a job while it runs

Run two Terminal-Bench tasks with Harbor's `oracle` agent. The agent applies
each task's reference solution, so it needs no model credentials:

```bash
agento11y harbor -- --dataset terminal-bench@2.0 --agent oracle --n-tasks 2
```

The command prints the experiment URL when the first trial starts. Trials
appear as Harbor starts them, and rewards, duration, and artifacts appear as
each one ends. To evaluate a model, swap in your agent and model, for example
`--agent codex --model openai/gpt-5`, and set that agent's provider
credentials.

## Import a completed job

Publish a job that Harbor already finished:

```bash
harbor run --dataset terminal-bench@2.0 --agent oracle --n-tasks 2
agento11y harbor export jobs/<job-name>
```

Harbor writes the job to `jobs/` in the directory you ran it from.

[`main.py`](main.py) does the same through the Python API. Run it from this
directory:

```bash
uv run python main.py <path-to-job-directory>
```

Add `--publish-suite subset` to also store the job's tasks as a published
suite checkpoint. That needs `AGENTO11Y_GRAFANA_URL` and
`AGENTO11Y_SERVICE_ACCOUNT_TOKEN`.

An experiment that's already finalized can't take the same job again, so the
import skips it. Pass `--experiment-id <new-id>` to publish the job as a
separate experiment.

## Link your agent's conversation

A custom Harbor agent that's instrumented with an Agent Observability SDK can
link its own conversation to the trial. Set the conversation ID in the agent's
context metadata:

```python
context.metadata = {"agento11y.conversation_id": conversation_id}
```

Without it, the plugin imports the agent's ATIF trajectory as the trial's
conversation. The plugin can't pass environment variables to agents, so don't
rely on `TrialRef.from_env()` inside an agent that runs under
`agento11y harbor`.
