# Experiments

Offline evals for the TypeScript and JavaScript SDK: run an agent over a dataset,
grade it, and publish runs, trials, and scores to Agent Observability.

The API ships as the `@grafana/agento11y/experiments` subpath, not as part of
`@grafana/agento11y-core`: core must keep loading on edge runtimes without
`process` or `Buffer`, and experiments needs crypto and environment access.
Experiments only runs as an offline batch job, so the split does not limit what you
can grade. The core client instruments an edge-deployed agent, and a separate
runner grades it later.

The surface matches the Python and Go experiments SDKs, in camelCase. Identities,
request bodies, and the cloud-evaluation call order are shared across all three,
so a run started by one SDK can be resumed by another. This guide calls the unit
of work a run; the API spells it `experimentId`, and the ingest route keys on
`experiment_id`.

## Connect

```ts
import { ExperimentsClient } from "@grafana/agento11y/experiments";

const client = new ExperimentsClient({
  endpoint: process.env.AGENTO11Y_ENDPOINT,
  tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
  ingestToken: process.env.AGENTO11Y_AUTH_TOKEN,
});
```

Every option falls back to an environment variable:

| Option | Environment variable | Purpose |
|--------|----------------------|---------|
| `endpoint` | `AGENTO11Y_ENDPOINT` | Agent Observability API URL |
| `ingestToken` | `AGENTO11Y_AUTH_TOKEN` | Cloud ingestion API key |
| `tenantId` | `AGENTO11Y_AUTH_TENANT_ID` | Stack id; when set, requests use basic auth |
| `actor` | `AGENTO11Y_INGEST_ACTOR` | Ingest actor, default `ingest:sdk/js` |
| `grafanaUrl` | `AGENTO11Y_GRAFANA_URL` | Base URL for `experiment.url` deep links |
| `useExperimentalOtel` | `AGENTO11Y_USE_EXPERIMENTAL_OTEL` | Emit trial spans and evaluation events |

Run, trial, generation, score, and artifact writes all use the ingest credential.
Stored test suites are the exception: they live behind the Grafana plugin control
plane and use `TestSuitesClient` with its own service-account token.

The SDK reads only the `AGENTO11Y_*` names. It warns once per process about a
removed `SIGIL_*` trial-handoff name, through the client logger or `console.warn`,
and never reads its value.

## Run an experiment

```ts
import { withExperiment } from "@grafana/agento11y/experiments";

const suite = {
  suiteId: "smoke",
  name: "Smoke",
  version: "1.0.0",
  testCases: [
    { testCaseId: "add", name: "Addition", input: "2+2", expected: "4" },
    { testCaseId: "sub", input: "5-3", expected: "2" },
  ],
};
const verifier = { evaluatorId: "exact", version: "1", kind: "deterministic" };

await withExperiment(client, { experimentId: "nightly-42", name: "nightly", suite }, async (experiment) => {
  for (const testCase of suite.testCases) {
    await experiment.withTrial(testCase, async (trial) => {
      const answer = await runAgent(testCase.input);
      trial.recordIO({ input: testCase.input, output: answer });
      trial.setUsage({ inputTokens: 120, outputTokens: 18 });
      trial.finalScore(answer === testCase.expected, { evaluator: verifier });
    });
  }
  console.log((await experiment.report()).summary);
});
```

`withExperiment` upserts the run, then finalizes it as `completed` or, when the
callback throws, as `failed` before rethrowing. `withTrial` starts the trial,
closes it, and terminalizes it as `errored` when the callback throws.

The explicit form suits a runner with its own control flow:

```ts
const experiment = await Experiment.start(client, { experimentId: "nightly-42", name: "nightly", suite });
const trial = experiment.trial("add");
await trial.start();
trial.finalScore(0.82, { passed: true, evaluator: verifier });
await trial.close();
await experiment.finalize("completed");
```

Reusing the same case and attempt in one experiment is rejected: the second trial
would derive the same trial and score identities as the first. Pass
`{ attempt: 2 }` for a retry.

## Trial close status

| Situation | Trial status | Sent to the backend |
|-----------|--------------|---------------------|
| Callback threw | `errored` | `failed` with the error text |
| No final score, no cloud evaluation | `failed` | `failed`, error `trial closed without a final score` |
| Cloud evaluation succeeded | `completed` | `completed` |
| Final score with a `passed` verdict | `passed` or `failed` | `completed` |
| Final score without a verdict | `completed` | `completed` |

The backend trial status reports the lifecycle, not the verdict. The pass/fail
verdict lives in the final score's `passed`, which is what the report's pass rate
counts.

## Scores

| Method | Use |
|--------|-----|
| `trial.finalScore(value, options)` | Headline score under the `final` key, plus the trial verdict |
| `trial.checkScore(name, { passed })` | Deterministic check, for example `json_valid` |
| `trial.rubricScore(name, value)` | One LLM-judge rubric criterion |
| `trial.score(key, value, options)` | The general primitive |
| `trial.recordEvaluation(result)` | A result produced by a framework or helper |
| `trial.evaluateOutput(judge, io)` | Grade caller-supplied output with a local judge |

Scores buffer locally and export on `trial.flush()` or `trial.close()`. An empty
buffer sends no request. `flush()` keeps the buffer on failure, so a later close
can publish it.

`finalScore(true)` and `finalScore(false)` set the verdict from the boolean. A
numeric score needs an explicit `passed` to become a verdict.

## Local judges

`LLMJudge` and `RegexJudge` need no evaluator stored in Grafana:

```ts
import { LLMJudge } from "@grafana/agento11y/experiments";

const judge = new LLMJudge({
  evaluatorId: "judge.helpfulness",
  modelProvider: "anthropic",
  modelName: "claude-sonnet-4",
  invoke: async (prompt) => callYourModel(prompt),
});

await experiment.withTrial(testCase, async (trial) => {
  const answer = await runAgent(testCase.input);
  await trial.evaluateOutput(judge, { input: testCase.input, output: answer, expected: testCase.expected });
});
```

The default parser reads the last JSON object in the response that carries a
numeric `score`, so the SDK still reads a judge that narrates before its verdict.
A top-level score always wins over a nested rubric score. Pass `parser` to take
over parsing, and `usageExtractor` to take over token accounting.

A judge result carries a grader transcript. `recordEvaluation` publishes it as a
generation whose ids derive from the score id, so the grading call is visible next
to the score. Pass `{ publishGrader: false }` to keep the transcript local.

## Cloud evaluation (experimental)

`trial.evaluate` grades the conversation Agent Observability already stored, using
an evaluator defined in your tenant:

```ts
await experiment.withTrial(testCase, async (trial) => {
  const { conversationId } = await runInstrumentedAgent(testCase.input);
  trial.bindConversation(conversationId);
  await trial.evaluate("helpfulness", { evaluatorVersion: "2", timeoutMs: 120_000 });
});
```

This is experimental. Set `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`, or the
call rejects with `ExperimentalFeatureDisabledError` before sending any request.
The API can change or be removed in any release.

The call persists the conversation binding, exports the anchor generation from
`recordIO`, queues the evaluator, and polls until the worker reaches a terminal
status. The poll interval starts at `pollIntervalMs` (default 500 ms) and doubles
to a 5000 ms ceiling, and `timeoutMs` (default 300000 ms) bounds the whole wait.
The SDK keeps a caller interval that is already above the ceiling. Pass `signal`
to cancel: polling stops and the signal's own abort reason is rethrown unchanged.

| Outcome | Rejection |
|---------|-----------|
| Worker reported `failed` | `TrialEvaluationFailedError` with `evaluationId` and `detail` |
| Deadline passed | `TrialEvaluationTimeoutError` with `evaluationId` |
| Gate closed | `ExperimentalFeatureDisabledError` |
| Unrecognized status | Transport error, rather than polling forever |

Three consequences for reading the results back:

- The evaluator grades the conversation, not one generation, so its score carries
  the trial's `conversationId` and `trialId` and no `generationId`. Read it from
  the run's scores or the trial's scores in the report; a per-generation lookup
  returns nothing.
- `report.summary.passRate` stays unset, because only a score under the `final`
  key feeds it while a stored evaluator writes under its own key. It is
  `undefined`, not `0`: do not print it as a zero pass rate.
- `experiment.finalize` drops a caller-supplied `scoreCount` once any trial queued
  an evaluation. The server checks the count against every stored score, including
  the evaluator's, which this process never sees.

To evaluate several trials at once, trigger and poll yourself with
`client.triggerTrialEvaluation(...)` and `client.getTrialEvaluation(...)`, keeping
both inside the experiment block. Finalizing while an evaluation is still queued
answers 409 with the `pending_evaluations` conflict kind.

## Binding an already-instrumented agent

When normal instrumentation already emitted the attempt, bind it instead of
recording I/O again:

| Method | Effect |
|--------|--------|
| `trial.bindConversation(id)` | Attaches the trial and its scores to a stored conversation |
| `trial.bindGeneration(id, { conversationId })` | Attaches scores to an existing generation; no anchor generation is exported |
| `await trial.bindTrace(traceId, spanId)` | Links the trial to a trace produced elsewhere |

`recordIO` is the other direction: the harness owns the candidate's input and
output, and the SDK exports one anchor generation so the attempt is visible.
A failed anchor-generation export rejects, unlike `Agento11yClient.flush()`, which
only warns. A trial that loses its conversation fails evaluation later with a
confusing server-side error.

## Artifacts

```ts
await trial.artifact("transcript.md", { text: renderTranscript(), kind: "markdown", mime: "text/markdown" });
await trial.artifact("trace.json", { data: { steps } });
await trial.artifact("screenshot.png", { bytes: png, mime: "image/png" });
```

Provide exactly one of `text`, `data`, or `bytes`. The artifact kind and MIME type
are inferred when omitted: `data` becomes `application/json`, `text` becomes
`text/plain`, and raw bytes become `application/octet-stream` unless a MIME type
maps onto `image`, `json`, `markdown`, `pdf`, `csv`, or `text`.

## Suites

Portable suites are plain YAML, readable by all three SDKs:

```ts
import { parseSuiteYAML, stringifySuiteYAML } from "@grafana/agento11y/experiments";

const suite = parseSuiteYAML(await readFile("suite.yaml", "utf8"));
await writeFile("suite.yaml", stringifySuiteYAML(suite));
```

Loading accepts the legacy aliases: a suite id under `suite_id` or `id`, cases
under `cases` or `test_cases`, and a case id under `id` or `test_case_id`. Saving
always writes the canonical spelling, so a load-save round trip normalizes an
older document.

Stored suites use the control plane:

```ts
import { TestSuitesClient } from "@grafana/agento11y/experiments";

const suites = new TestSuitesClient({
  grafanaUrl: process.env.AGENTO11Y_GRAFANA_URL,
  serviceAccountToken: process.env.AGENTO11Y_SERVICE_ACCOUNT_TOKEN,
});
const pulled = await suites.pullSuite("smoke", "latest_published");
const pushed = await suites.pushSuite(localSuite, { publish: true, changelog: "add refusal cases" });
```

`TestSuitesClient` also takes `controlEndpoint` (environment variable
`AGENTO11Y_CONTROL_ENDPOINT`), which falls back to `grafanaUrl`. It accepts a
Grafana base URL, a UI app URL, or the resources path itself; all three normalize
onto the plugin resources route. Versions resolve by exact name or through the
`latest_published`, `latest`, and `draft` aliases.

## Cross-process trials

A separate container opens one trial from environment variables the parent set:

```ts
import { Trial, trialRefFromEnv, trialRefToEnv } from "@grafana/agento11y/experiments";

// Parent
const env = trialRefToEnv({ experimentId: "nightly-42", testCaseId: "add", attempt: 1 });

// Child
const ref = trialRefFromEnv();
if (ref === undefined) {
  throw new Error("missing Agent Observability trial environment");
}
const trial = await Trial.fromRef(client, ref).start();
trial.finalScore(0.82, { passed: true });
await trial.close();
```

Trial, generation, conversation, and score ids are derived, not random:
`stableId` hashes the parts, so both processes compute the same identities and a
retry is idempotent.

## Telemetry

Experimental OpenTelemetry trial telemetry is off by default. Set
`AGENTO11Y_USE_EXPERIMENTAL_OTEL=true`, or pass `useExperimentalOtel: true`, to
emit one `eval.trial <case>` span per trial with `test.*` identity attributes and
one `gen_ai.evaluation.result` event per score. Some of those attribute names are
still moving through the OpenTelemetry GenAI SIG, so the SDK stamps
`agento11y.eval.schema.version` on the span. An upstream rename then shows up as a
version bump rather than silent drift.

`withTrial` runs its callback with the trial span active, so spans an instrumented
agent emits inside the callback become children of the trial span. If you drive
`start()` and `close()` by hand, wrap the agent call in
`trial.runInTrialContext(fn)` to get the same parenting.

Secret redaction is on by default and covers score explanations, score metadata,
generation payloads, and text artifacts. Pass `redactSecrets: false` to send
content unchanged.

## Errors

| Shape | Meaning |
|-------|---------|
| `agento11y experiment validation failed: …` | Rejected by the SDK, or HTTP 400/422 |
| `agento11y experiment not found: …` | HTTP 404 |
| `ExperimentConflictError` | HTTP 409, with a classified `kind` and `recoverable` |
| `agento11y experiment transport failed: …` | Transport failure, unusable response, or exhausted retries |
| `TrialEvaluationFailedError` | A stored evaluator ended the evaluation unsuccessfully |
| `TrialEvaluationTimeoutError` | The evaluation did not finish before the deadline |
| `ExperimentalFeatureDisabledError` | An experimental call ran without its opt-in |

Transport failures, HTTP 429, and 5xx get four total attempts with backoff
doubling from 100 ms to a 5000 ms ceiling. A 400, 404, 409, or 422 is a caller
error and is not retried. Control-plane requests get six total attempts.
