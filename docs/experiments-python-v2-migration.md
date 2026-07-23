# Python Experiments v2 Migration

Experiments v2 standardizes the public surface on `agento11y` package and
environment names and makes
distributed publishing authoritative. It intentionally does not retain
`SIGIL_*` or `*_CONTROL_PLANE_*` compatibility aliases.

## Configuration

Rename every experiment environment variable before upgrading:

| v1 | v2 |
| --- | --- |
| `SIGIL_ENDPOINT` or `SIGIL_API_ENDPOINT` | `AGENTO11Y_ENDPOINT` |
| `SIGIL_AUTH_TOKEN` | `AGENTO11Y_AUTH_TOKEN` |
| `SIGIL_AUTH_TENANT_ID` or `SIGIL_TENANT_ID` | `AGENTO11Y_AUTH_TENANT_ID` |
| `SIGIL_GRAFANA_URL` | `AGENTO11Y_GRAFANA_URL` |
| `SIGIL_INGEST_ACTOR` | `AGENTO11Y_INGEST_ACTOR` |
| `SIGIL_EXPERIMENT_ID` or `SIGIL_RUN_ID` | `AGENTO11Y_EXPERIMENT_ID` |
| `SIGIL_TEST_CASE_ID` | `AGENTO11Y_TEST_CASE_ID` |
| `SIGIL_ATTEMPT` | `AGENTO11Y_ATTEMPT` |
| `SIGIL_SUITE_ID` | `AGENTO11Y_SUITE_ID` |
| `SIGIL_SUITE_VERSION` | `AGENTO11Y_SUITE_VERSION` |
| `SIGIL_TRAJECTORY_ID` | `AGENTO11Y_TRAJECTORY_ID` |

These known `SIGIL_*` variables are ignored and emit a one-time migration
warning. Stored suite control is new in v2: configure
`AGENTO11Y_CONTROL_ENDPOINT` and `AGENTO11Y_SERVICE_ACCOUNT_TOKEN` only when
push/pull is needed. Run, trial, generation, score, artifact, and finalize
writes use the ingestion credential.

The exported `ENV_RUN_ID` and six `ENV_*_PREFERRED` constants were removed.
`ENV_EXPERIMENT_ID`, `ENV_TEST_CASE_ID`, `ENV_ATTEMPT`, `ENV_SUITE_ID`,
`ENV_SUITE_VERSION`, and `ENV_TRAJECTORY_ID` remain, but now contain canonical
`AGENTO11Y_*` names. Prefer `TrialRef.to_env()` and `TrialRef.from_env()` over
importing environment-name constants.

## Runner changes

- Import the entry point as `agento11y.experiments.experiment`.
- Construct exported request/response dataclasses with keyword arguments.
- Use `TrialRef.to_env()` to hand `AGENTO11Y_*` identifiers to worker processes.
- A standalone worker must call `Trial.from_ref(...); score(...); close()`.
  `close()` creates the trial lazily, flushes its scores, and marks it terminal
  so run finalization is not blocked by a running trial.
- Normal `Experiment.finalize()` omits `score_count`. Pass it only when an exact
  server-side count assertion is required.
- Numeric final scores without `passed=` are neutral, not implicit failures.
- Report aggregates such as `pass_rate`, `final_score_avg`, `total_cost`, and
  `total_tokens` are `None` when the server has no data.
- Run links use `/a/grafana-sigil-app/experiments/runs/{experiment_id}`.
- The experiment client now sends the stable default actor
  `ingest:sdk/python` when no actor is configured.
- Scalar case fields and non-default weights use the documented
  `agento11y.sdk.portability` metadata envelope when synchronized to the
  object-only stored-suite API.

Test suites and platform evaluator definitions remain optional. Local
`LLMJudge`, `RegexJudge`, and framework-produced `EvaluationResult` values can
publish scores and grader provenance without creating an evaluator in Grafana.
