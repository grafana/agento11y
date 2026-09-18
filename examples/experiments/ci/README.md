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
- Use an Agent Observability ingestion credential in your CI secret store:
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

## Gate one primary verdict

The gate must be based on one explicit primary verdict or legacy `final` score
per trial. Diagnostic scores remain useful for investigation, but they should
not silently decide whether a change merges. Stored-evaluator examples may use
their own score key and leave aggregate `pass_rate` unset; either configure a
primary verdict or implement a deliberate policy over the evaluator's score
key.

The snippets below show the final step after an experiment has completed. They
intentionally use a minimum pass rate, the smallest policy that is both
explainable and safe to copy. A baseline comparison should reject mismatched
suite versions and missing values before calculating a delta.

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
