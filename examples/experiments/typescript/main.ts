/**
 * Grades experiment trials with an evaluator stored in Agent Observability
 * instead of a score computed in the runner.
 *
 * Use this shape when the grading prompt lives in your tenant. The runner only
 * has to make the agent's conversation findable:
 *
 *   1. Open an experiment with the ingest credential.
 *   2. For each case, run the already-instrumented agent and bind its conversation.
 *   3. Call `trial.evaluate(evaluatorId)` and let the stored evaluator score it.
 *   4. Close the trial without a local `finalScore`; the backend owns the verdict.
 *
 * The canned agent here publishes its own generation so the example runs without
 * a provider key. A real agent instrumented with the SDK or a provider wrapper
 * already emits that generation, and you only need its conversation id.
 *
 * SDK config via env: AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TOKEN, optional
 * AGENTO11Y_AUTH_TENANT_ID and AGENTO11Y_EXPERIMENT_ID. Stored-evaluator grading
 * is experimental, so AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true is required.
 * AGENTO11Y_EVALUATOR_ID, AGENTO11Y_EVALUATOR_VERSION, and GIT_SHA are this
 * example's own knobs.
 */

import {
  type Experiment,
  ExperimentalFeatureDisabledError,
  ExperimentsClient,
  FEATURE_CLOUD_TRIAL_EVALUATION,
  type Trial,
  TrialEvaluationFailedError,
  TrialEvaluationTimeoutError,
  requireExperimental,
  withExperiment,
} from '@grafana/agento11y/experiments';

interface EvalCase {
  id: string;
  question: string;
  answer: string;
}

const cases: EvalCase[] = [
  { id: 'capital-france', question: 'What is the capital of France?', answer: 'Paris' },
  { id: 'two-plus-two', question: 'What is 2 + 2? Answer with just the number.', answer: '4' },
  { id: 'largest-planet', question: 'What is the largest planet in our solar system?', answer: 'Jupiter' },
];

async function main(): Promise<void> {
  // `trial.evaluate` checks this itself, but by then the run and its first trial
  // already exist. Checking up front leaves nothing behind in the tenant.
  requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION);

  const gitSha = env('GIT_SHA', 'manual');
  const experimentId = env('AGENTO11Y_EXPERIMENT_ID', `cloud-evaluator-${gitSha}`);
  // The evaluator must already exist in your tenant; this example does not create one.
  const evaluatorId = env('AGENTO11Y_EVALUATOR_ID', 'helpfulness');
  const evaluatorVersion = env('AGENTO11Y_EVALUATOR_VERSION', '');

  const client = new ExperimentsClient({
    grafanaUrl: process.env.AGENTO11Y_GRAFANA_URL,
  });

  const experiment = await withExperiment(
    client,
    {
      experimentId,
      name: 'TypeScript cloud evaluator example',
      plannedTrialCount: cases.length,
      candidate: { agentName: 'example-agent', gitSha },
      tags: ['example', 'typescript', 'cloud-evaluator'],
    },
    async (run) => {
      for (const testCase of cases) {
        await gradeCase(client, run, testCase, evaluatorId, evaluatorVersion);
      }
      return run;
    },
  );

  const report = await experiment.report();
  console.log(`trials=${report.summary.trialCount} completed=${report.summary.completedCount}`);
  // Only a score under the "final" key feeds report.summary.passRate, and a stored
  // evaluator scores under its own key, so it stays unset. Printing it as 0 would
  // read as "everything failed".
  console.log(`pass rate: ${report.summary.passRate ?? 'not applicable for a stored evaluator'}`);
  for (const row of report.rows) {
    const testCaseId = String(row.test_case_id ?? 'unknown');
    for (const trialResult of asArray(row.trials)) {
      const keys = asArray(trialResult.scores)
        .map((score) => String(score.score_key ?? ''))
        .filter((key) => key.length > 0);
      console.log(`  ${testCaseId}: scores=${keys.length > 0 ? keys.join(', ') : 'none'}`);
    }
  }
  console.log(`View in Agent Observability: ${experiment.url}`);
}

async function gradeCase(
  client: ExperimentsClient,
  experiment: Experiment,
  testCase: EvalCase,
  evaluatorId: string,
  evaluatorVersion: string,
): Promise<void> {
  await experiment.withTrial(testCase.id, async (trial: Trial) => {
    // Stand-in for your instrumentation's conversation. With a real instrumented
    // agent, bind the id it already exported instead.
    const conversationId = `${trial.trialId}-agent`;
    await client.exportGeneration({
      generationId: trial.generationId,
      conversationId,
      inputText: testCase.question,
      outputText: testCase.answer,
      modelProvider: 'example',
      modelName: 'canned-answer',
      agentName: 'example-agent',
      operationName: 'answer_question',
      usage: { inputTokens: 12, outputTokens: 3 },
      tags: { 'experiment.run_id': experiment.experimentId, task_id: testCase.id },
    });
    // The evaluator grades the conversation this binding points at.
    trial.bindConversation(conversationId);

    try {
      // Waits until the worker finishes. No local finalScore follows: the stored
      // evaluator writes the score and the report counts it.
      const evaluation = await trial.evaluate(evaluatorId, { evaluatorVersion });
      const version = evaluation.evaluatorVersion.length > 0 ? evaluation.evaluatorVersion : 'latest';
      console.log(
        `${testCase.id}: ${evaluation.status} evaluator=${evaluation.evaluatorId}@${version} attempts=${evaluation.attempts}`,
      );
    } catch (error) {
      if (error instanceof TrialEvaluationFailedError) {
        console.error(`${testCase.id}: evaluation ${error.evaluationId} failed: ${error.detail}`);
      } else if (error instanceof TrialEvaluationTimeoutError) {
        console.error(`${testCase.id}: evaluation ${error.evaluationId} still pending: ${error.detail}`);
      }
      throw error;
    }
  });
}

function env(key: string, fallback: string): string {
  const value = (process.env[key] ?? '').trim();
  return value.length > 0 ? value : fallback;
}

function asArray(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value) ? value.filter((entry): entry is Record<string, unknown> => typeof entry === 'object' && entry !== null) : [];
}

main().catch((error: unknown) => {
  if (error instanceof ExperimentalFeatureDisabledError) {
    console.error(`${error.message}\nNo request was sent.`);
    process.exit(1);
  }
  console.error(error);
  process.exit(1);
});
