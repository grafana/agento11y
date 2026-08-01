# Go experiments example

The root example is an o11y-bench-shaped streaming runner built on
`github.com/grafana/agento11y/go/agento11y/experiments`. It publishes each
scored attempt immediately, including candidate I/O, multiple verifier scores,
token usage, cost, and a file artifact.

`RUN_ID`, test-case ID, and the explicit attempt number produce stable run,
trial, generation, conversation, and occurrence-aware score identities.
Re-running a resumed job with the same identities is idempotent; increment the
attempt only for a genuinely new attempt.

```bash
cd examples/experiments/go
cp .env.example .env
set -a && source .env && set +a
GOWORK=off go run .
```

The canned agent makes no provider call. It needs only `AGENTO11Y_ENDPOINT`,
`AGENTO11Y_AUTH_TOKEN`, and optional `AGENTO11Y_AUTH_TENANT_ID`.

| Entry point | Command | What it demonstrates |
| --- | --- | --- |
| Streaming runner | `GOWORK=off go run .` | Candidate I/O, several verifier scores, usage, cost, artifact |
| Stored evaluator | `GOWORK=off go run ./cloud-evaluator` | Binds the agent's conversation and grades it with an evaluator stored in your tenant, with no local score |
| Prompt optimizer | `GOWORK=off go run ./prompt-optimization` | Real-model hill climbing on the finalized report score |

## Stored evaluator

[`cloud-evaluator/`](cloud-evaluator/) grades each trial with an evaluator that
already exists in your tenant instead of scoring locally. `trial.Evaluate(...)`
blocks until the worker finishes, so the run takes as long as the evaluations do:

Stored-evaluator grading is experimental and refuses to run without the opt-in:

```bash
export AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true
export AGENTO11Y_EXPERIMENT_ID=cloud-evaluator-${GIT_SHA:-manual}
export EVALUATOR_ID=<an-evaluator-id-in-your-tenant>
# Optional: pin a version instead of the latest active one.
export EVALUATOR_VERSION=<version>
GOWORK=off go run ./cloud-evaluator
```

The example does not create the evaluator; define it in Agent Observability
first. Trials close as `completed` with no local final score, and the backend
counts the stored evaluator's scores.

## Prompt optimization

[`prompt-optimization/`](prompt-optimization/) is a real-model example that
ports the Sigil prompt optimizer to Go. It asks a reasoning model for candidate
prompts, evaluates each candidate over 27 embedded unit-inference fixtures, and
uses the finalized Agent Observability report score as its hill-climbing
objective:

```bash
GOWORK=off go run ./prompt-optimization
```

See its README for model configuration, suite versioning, and the explicit
content-recording opt-in.

To synchronize the local suite before a run:

```go
suites, _ := experiments.NewTestSuitesClient(experiments.TestSuitesClientOptions{})
pushed, err := suites.PushSuite(ctx, suite, experiments.PushSuiteOptions{
	Prune: true, Publish: true, Changelog: "nightly dataset sync",
})
```

Stored suite access additionally needs `AGENTO11Y_CONTROL_ENDPOINT` (or
`AGENTO11Y_GRAFANA_URL`) and `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`. Use
`NewExperimentFromSuite`/`WithExperimentFromSuite` to resolve
`latest_published`, `latest`, `draft`, or an exact version and stamp that exact
suite identity onto the run and trials. A stored suite is optional: the example
publishes the in-memory suite directly.
