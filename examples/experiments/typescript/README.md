# TypeScript experiments example: stored evaluator

This example grades each trial with an evaluator that already exists in your
tenant, instead of computing the score in the runner. `trial.evaluate(...)` waits
until the worker finishes, so the run takes as long as the evaluations do.

The canned agent makes no provider call. The example needs `AGENTO11Y_ENDPOINT`,
`AGENTO11Y_AUTH_TOKEN`, optional `AGENTO11Y_AUTH_TENANT_ID`, and an evaluator id.

## Setup

```bash
cd examples/experiments/typescript
cp .env.example .env
# Fill in the endpoint, tenant, token, and evaluator id.
npm install
```

The `@grafana/agento11y` package is installed from the local monorepo through a
`file:` reference. Outside the monorepo, replace that reference with the
published package.

## Run

Define the evaluator in Agent Observability first: the example does not create
one. Stored-evaluator grading also needs the experimental opt-in, which
`.env.example` already sets to `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`.

```bash
set -a && source .env && set +a
npx tsx main.ts
```

Each trial closes as `completed` with no local final score, and the backend
counts the stored evaluator's scores.

If the opt-in is missing, the example prints
`agento11y: cloud trial evaluation is experimental; …` and exits before sending
any request.

## What it shows

| Step | Call |
| --- | --- |
| Open the run | `withExperiment(client, {...}, fn)` |
| Publish the agent's turn | `client.exportGeneration({...})` |
| Point the evaluator at the conversation | `trial.bindConversation(conversationId)` |
| Queue the stored evaluator and wait | `trial.evaluate(evaluatorId, { evaluatorVersion })` |
| Read the results | `experiment.report()` |

A real instrumented agent already exports its generation, so you drop the
`client.exportGeneration` call and bind only the conversation id the agent
produced.

## Reading the report

`report.summary.passRate` stays unset for a cloud-evaluated run: only a score
stored under the `final` key feeds it, and a stored evaluator writes under its own
key. The example prints "not applicable for a stored evaluator" rather than `0`,
which would read as "everything failed". The per-row score keys below it show what
the backend actually attached.

For the same reason, leave `scoreCount` unset when finalizing such a run. The SDK
drops a caller-supplied count once any trial queued an evaluation, because the
server checks it against every stored score, including the evaluator's.

## Errors worth handling

| Error | Meaning |
| --- | --- |
| `ExperimentalFeatureDisabledError` | The opt-in is missing; nothing was sent |
| `TrialEvaluationFailedError` | The worker finished unsuccessfully; carries `evaluationId` and `detail` |
| `TrialEvaluationTimeoutError` | The deadline passed while the evaluation was still queued; it keeps running server-side |

For credentials, see the
[credentials section in the repo README](../../../README.md#grafana-cloud-credentials).
