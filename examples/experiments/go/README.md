# Go experiments example

This is an o11y-bench-shaped streaming runner built on
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
