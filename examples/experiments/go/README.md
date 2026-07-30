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
