# CI recipe: publish an experiment and gate a change

Use this recipe to run an offline experiment for a pull request or release
candidate, publish the result to Grafana Agent Observability, and then make
your CI job pass or fail from an explicit quality policy.

This is a **CI integration pattern**, not built-in deployment enforcement.
Agent Observability stores and compares experiment results; your CI provider and
branch or deployment rules decide what a non-zero runner exit blocks.

The recipe is language-neutral. Python, Go, and TypeScript all expose the same
core operations: open an experiment, run and score trials, finalize the run,
read its report, and return a non-zero exit code when the policy fails.

## Before you start

- Keep test cases in source control and retain stable test-case IDs.
- Give every run a stable, unique ID, such as `ci-<git-sha>` or
  `pr-<number>-<git-sha>`. This makes a retry idempotent without merging two
  different candidates into one run.
- Set `candidate.git_sha`, a model or prompt version where applicable, and a
  `ci` tag. Those fields make the resulting run comparable and searchable.
- Use your Grafana Cloud Agent Observability ingestion credentials in CI secrets:
  `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TOKEN`, and, when required,
  `AGENTO11Y_AUTH_TENANT_ID`. A service-account token is needed only when the
  job reads, updates, or publishes a stored test suite.

Do not put tokens, prompts, or retained traces in the repository or unprotected
CI artifacts. Publish input/output content only when your data policy permits
it.

## The required ordering

1. Run every selected case and publish its scores and artifacts.
2. Finalize the experiment, including when the quality policy will fail.
3. Read the finalized report and print the experiment URL.
4. Evaluate a policy you own, such as minimum pass rate or a permitted delta
   against a baseline.
5. Exit non-zero only after the report has been published and linked.

Do not use `run-eval && upload-results`: a failing quality check then loses the
very experiment report needed to diagnose the regression. Stop immediately on
cancellation or an operational error before a usable result exists.

Experiment reports can briefly be pending after finalization. A production
runner should wait with a bounded timeout for the expected final scores before
evaluating a gate. Treat a missing primary verdict or an incomplete report as a
CI error, not as a passing quality result.

## Gate a published suite version

For a suite version published in Grafana Cloud with pinned evaluators, the
Python SDK (0.20.0 or later) runs and gates it end to end:

1. Run the baseline agent once, and the candidate agent for each change, with
   [`run_checkpoint`](../python/README.md#run-a-published-suite-version-with-your-agent).
   Give each run its own ID with `experiment_id`, such as `ci-known-good` and
   `ci-$GITHUB_SHA`.
2. Compare the two runs:

```sh
agento11y-checkpoint-gate \
  --suite-id support-regression --suite-version 3 \
  --baseline ci-known-good --candidate "ci-$GITHUB_SHA" \
  --min-pass-rate 0.95 --max-drop 0.02
```

The gate needs the ingestion credentials, your stack URL in
`AGENTO11Y_GRAFANA_URL`, and a Grafana service-account token in
`AGENTO11Y_SERVICE_ACCOUNT_TOKEN`. Thresholds are
fractions from 0 to 1, and equality passes. Here the candidate must pass at
least 95% of cases and lose at most two percentage points against the baseline.

| Exit | Meaning |
| --- | --- |
| `0` | The candidate meets both thresholds. |
| `1` | The candidate fails a threshold. |
| `2` | The runs can't be compared, or the gate couldn't read them. |

Stdout is a JSON object with the run IDs, pass rates, thresholds and reasons,
plus report links when `AGENTO11Y_GRAFANA_URL` is set. It never includes case
content or error details from the transport. Configure CI to fail on any
non-zero exit.

The gate exits `2` unless both runs:

- ran every case of the exact published version once, with no errored case;
- were declared as actual-agent runs, not simulations or imports;
- have case snapshots the server verified against the published version;
- have one score from every pinned evaluator, with the same evaluators in both.

The gate doesn't wait for grading. `run_checkpoint` returns after every case is
graded, so run the gate after it.

## Gate one primary verdict

The gate must be based on one explicit primary verdict or legacy `final` score
per trial. Diagnostic scores remain useful for investigation, but they should
not silently decide whether a change merges. Stored-evaluator examples may use
their own score key and leave aggregate `pass_rate` unset; either configure a
primary verdict or implement a deliberate policy over the evaluator's score
key.

The snippets below illustrate only a threshold expression, after your runner
has separately verified completed execution and full verdict coverage. They
are not complete CI gates: a partial report can have a high pass rate. Prefer
the checkpoint gate above for pinned cloud grading. Other runner policies must
reject incomplete and mismatched evidence before applying a threshold.

### Python

```python
report = exp.report()  # Call after the `with experiments.experiment(...)` block.
pass_rate = report.summary.pass_rate
print(f"Agent Observability: {exp.url}")
if pass_rate is None:
    raise RuntimeError("experiment has no primary verdict pass rate")
if pass_rate < float(os.environ.get("MIN_PASS_RATE", "0.90")):
    raise SystemExit(f"quality gate failed: pass_rate={pass_rate:.3f}")
```

### Go

```go
report, err := run.Report(ctx) // Call after WithExperiment returned successfully.
if err != nil {
    return err
}
fmt.Printf("Agent Observability: %s\n", run.URL())
if report.Summary.PassRate == nil {
    return errors.New("experiment has no primary verdict pass rate")
}
if *report.Summary.PassRate < minPassRate {
    return fmt.Errorf("quality gate failed: pass_rate=%.3f", *report.Summary.PassRate)
}
```

### TypeScript

```ts
const report = await experiment.report(); // Call after withExperiment resolves.
console.log(`Agent Observability: ${experiment.url}`);
if (report.summary.passRate === undefined) {
  throw new Error('experiment has no primary verdict pass rate');
}
if (report.summary.passRate < Number(process.env.MIN_PASS_RATE ?? '0.90')) {
  process.exitCode = 1;
}
```

## GitHub Actions template

Copy [`github-actions.yml`](github-actions.yml) into the consuming repository's
`.github/workflows/` directory. It deliberately calls a repository-owned
`./scripts/run-agent-experiment` command: that command is where the customer
selects Python, Go, or TypeScript and applies its reviewed quality policy.

The template creates a required CI check when protected by the repository's
branch rules. It does not publish PR comments, set deployment status, approve a
release, or bypass any customer review control.

## Existing language examples

- [Python framework-free runner](../python/README.md) demonstrates local
  scoring, report retrieval, and source-controlled suite files.
- [Go runner](../go/README.md) demonstrates stable run identities, local final
  scores, and `Experiment.Report`.
- [TypeScript runner](../typescript/README.md) demonstrates the experiment
  lifecycle and stored evaluator results. For a pass-rate gate, use a local
  `trial.finalScore(...)` or designate a primary verdict.
