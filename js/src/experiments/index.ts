/**
 * Cloud experiments: run benchmarks and evals against Agent Observability with a
 * single ingest token.
 *
 * This is the high-level surface for evaluation harnesses. It writes over the v1
 * one-token ingest path (run upsert, trial upsert, generation export, score
 * export, finalize). Experimental OpenTelemetry eval telemetry is emitted only
 * when `useExperimentalOtel` or `AGENTO11Y_USE_EXPERIMENTAL_OTEL` is set.
 *
 * The module is published as `@grafana/agento11y/experiments`. It is deliberately
 * not part of `@grafana/agento11y-core`: experiments needs crypto and environment
 * access, while core must keep loading on edge runtimes.
 *
 * Quick start (`endpoint` is your Grafana Cloud Agent Observability URL,
 * `tenantId` your stack id, and `ingestToken` your Cloud ingestion API key):
 *
 * ```ts
 * import { ExperimentsClient, withExperiment } from '@grafana/agento11y/experiments';
 *
 * const client = new ExperimentsClient({
 *   endpoint: process.env.AGENTO11Y_ENDPOINT,
 *   tenantId: process.env.AGENTO11Y_AUTH_TENANT_ID,
 *   ingestToken: process.env.AGENTO11Y_AUTH_TOKEN,
 * });
 * const suite = {
 *   suiteId: 'smoke',
 *   name: 'Smoke',
 *   testCases: [{ testCaseId: 'add', input: '2+2', expected: '4' }],
 * };
 * const verifier = { evaluatorId: 'exact', version: '1', kind: 'deterministic' };
 *
 * await withExperiment(client, { experimentId: 'run-1', name: 'smoke run', suite }, async (experiment) => {
 *   for (const testCase of suite.testCases) {
 *     await experiment.withTrial(testCase, async (trial) => {
 *       const answer = await runAgent(testCase.input);
 *       trial.finalScore(answer === testCase.expected, { evaluator: verifier });
 *     });
 *   }
 * });
 * ```
 *
 * A stored Agent Observability evaluator can grade a conversation the agent's own
 * instrumentation already emitted. That path is experimental and requires
 * `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`:
 *
 * ```ts
 * await experiment.withTrial(testCase, async (trial) => {
 *   const { conversationId } = await runInstrumentedAgent(testCase.input);
 *   trial.bindConversation(conversationId);
 *   await trial.evaluate('helpfulness');
 * });
 * ```
 *
 * A separate process (for example a grading container) opens one trial from a
 * serialized ref:
 *
 * ```ts
 * import { Trial, trialRefFromEnv } from '@grafana/agento11y/experiments';
 *
 * const ref = trialRefFromEnv();
 * if (ref === undefined) {
 *   throw new Error('missing Agent Observability trial environment');
 * }
 * const trial = await Trial.fromRef(client, ref).start();
 * trial.finalScore(0.82, { passed: true });
 * await trial.close();
 * ```
 */

export type {
  ExperimentsClientOptions,
  ExportGenerationRequest,
  FinalizeExperimentOptions,
  FlushableGenerationClient,
  RequestOptions,
  UpdateTrialRequest,
  UploadArtifactRequest,
  UpsertTrialRequest,
} from './client.js';
export { DEFAULT_INGEST_ACTOR, ExperimentsClient, INGEST_ACTOR_HEADER } from './client.js';
export type { ConflictKind } from './errors.js';
export {
  CONFLICT_PREFIX,
  classifyConflict,
  ExperimentConflictError,
  NOT_FOUND_PREFIX,
  TRANSPORT_PREFIX,
  TrialEvaluationFailedError,
  TrialEvaluationTimeoutError,
  VALIDATION_PREFIX,
} from './errors.js';
export type {
  EvaluateOutputInput,
  EvaluationResult,
  GraderGeneration,
  LLMJudgeOptions,
  OutputEvaluator,
  ParsedJudgeResult,
  RegexJudgeOptions,
} from './evaluators.js';

export {
  DEFAULT_LLM_JUDGE_PROMPT,
  LLMJudge,
  RegexJudge,
} from './evaluators.js';
export type {
  ArtifactOptions,
  EvaluateOptions,
  ExperimentOptions,
  FinalizeOptions,
  RecordIOOptions,
  ScoreOptions,
  TrialOptions,
} from './experiment.js';

export { artifactKindFromMime, Experiment, Trial, withExperiment } from './experiment.js';
export {
  ENV_ENABLE_EXPERIMENTAL_FEATURES,
  ExperimentalFeatureDisabledError,
  experimentalFeaturesEnabled,
  FEATURE_CLOUD_TRIAL_EVALUATION,
  requireExperimental,
} from './experimental.js';

export { stableId } from './ids.js';
export type {
  Candidate,
  CreateExperimentRequest,
  Evaluator,
  EvaluatorKind,
  Experiment as ExperimentRun,
  ExperimentReport,
  ExperimentReportSummary,
  ExperimentStatus,
  ExportScoreResult,
  ExportScoresResponse,
  ScoreItem,
  ScoreSource,
  ScoreValue,
  TrialEvaluation,
  TrialEvaluationStatus,
  TrialStatus,
} from './models.js';
export {
  isTerminalEvaluationStatus,
  normalizeEvaluatorKind,
  parseExperiment,
  parseExperimentReport,
  parseExperimentRunResponse,
  parseTrialEvaluation,
} from './models.js';

export * as otel from './otel.js';
export * as routes from './routes.js';
export type { ListOptions, PushedSuite, PushSuiteOptions, TestSuitesClientOptions } from './suites.js';
export {
  localCaseToRemote,
  normalizeControlEndpoint,
  remoteCaseToLocal,
  TestSuitesClient,
} from './suites.js';
// The raw transport stays internal: it bypasses the validation, redaction, and
// experimental gate every ExperimentsClient method applies. Only the retry policy
// is public, because client options accept it.
export type { ExperimentsRetryPolicy } from './transport.js';
export type { TestCase, TestSuite, TrialRef } from './types.js';
export {
  candidateMetadata,
  ENV_ATTEMPT,
  ENV_EXPERIMENT_ID,
  ENV_SUITE_ID,
  ENV_SUITE_VERSION,
  ENV_TEST_CASE_ID,
  ENV_TRAJECTORY_ID,
  suiteCase,
  testCaseFromObject,
  testCaseToObject,
  testSuiteFromObject,
  testSuiteToObject,
  trialRefFromEnv,
  trialRefFromJSON,
  trialRefToEnv,
  trialRefToJSON,
} from './types.js';

export { parseSuiteYAML, stringifySuiteYAML, validateSuite } from './yaml.js';
