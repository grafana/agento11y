/**
 * OpenTelemetry GenAI evaluation telemetry for Agent Observability experiments.
 *
 * The experiments surface writes scores over the v1 ingest path. When explicitly
 * enabled, it also emits OpenTelemetry so already-instrumented agents line up
 * with Agent Observability's eval model.
 *
 * OpenTelemetry names are emitted directly; there is no parallel `agento11y.*`
 * mirror. Merged names go out as-is (the `gen_ai.evaluation.result` event and its
 * attributes, `gen_ai.request.model` / `gen_ai.agent.*` on the candidate, and the
 * `test.*` suite and case identity). Names Grafana is pushing through the OTel
 * GenAI SIG are a best-effort prediction and can change until upstream adopts them;
 * `agento11y.eval.schema.version` stamps which prediction generation a consumer
 * is reading.
 *
 * Attribute names are kept identical to `python/agento11y/experiments/otel.py`.
 */

import type { AttributeValue } from '@opentelemetry/api';

/**
 * The OTel instrumentation scope name is a telemetry-visible data contract
 * consumed outside this repository, so it intentionally keeps the pre-rename
 * module name. Do not update it for the sigil-sdk -> agento11y rename without
 * server-side dual-read support.
 */
export const INSTRUMENTATION_NAME = 'sigil_sdk.experiments';
export const SCHEMA_VERSION = 'experiments-otel-2026-06';
export const ATTR_SCHEMA_VERSION = 'agento11y.eval.schema.version';

/** Trial span name prefix; the test case id is appended. */
export const TRIAL_SPAN_NAME = 'eval.trial';

export const EVENT_EVAL_RESULT = 'gen_ai.evaluation.result';
export const EVAL_NAME = 'gen_ai.evaluation.name';
export const EVAL_SCORE_VALUE = 'gen_ai.evaluation.score.value';
export const EVAL_SCORE_LABEL = 'gen_ai.evaluation.score.label';
export const EVAL_EXPLANATION = 'gen_ai.evaluation.explanation';
export const RESPONSE_ID = 'gen_ai.response.id';

export const EVAL_EVALUATOR_ID = 'gen_ai.evaluation.evaluator.id';
export const EVAL_EVALUATOR_VERSION = 'gen_ai.evaluation.evaluator.version';
export const EVAL_EVALUATOR_TYPE = 'gen_ai.evaluation.evaluator.type';
export const EVAL_REFERENCE_SET_ID = 'gen_ai.evaluation.reference_set.id';
export const EVAL_REFERENCE_SET_VERSION = 'gen_ai.evaluation.reference_set.version';

export const TEST_SUITE_RUN_ID = 'test.suite.run.id';
export const TEST_SUITE_NAME = 'test.suite.name';
export const TEST_SUITE_RUN_STATUS = 'test.suite.run.status';
export const TEST_SUITE_ID = 'test.suite.id';
export const TEST_SUITE_VERSION = 'test.suite.version';
export const TEST_CASE_ID = 'test.case.id';
export const TEST_CASE_NAME = 'test.case.name';
export const TEST_CASE_RESULT_STATUS = 'test.case.result.status';
export const TEST_CASE_RUN_ID = 'test.case.run.id';
export const TEST_CASE_RUN_ATTEMPT = 'test.case.run.attempt';

export const OPERATION_NAME = 'gen_ai.operation.name';
export const CONVERSATION_ID = 'gen_ai.conversation.id';

export const ARTIFACT_EVENT = 'agento11y.eval.artifact';
export const ARTIFACT_NAME = 'agento11y.eval.artifact.name';
export const ARTIFACT_KIND = 'agento11y.eval.artifact.kind';

/** Experiment/run status (API) mapped onto the general OTel `test.suite.run.status` enum. */
const runStatusMap: Record<string, string> = {
  running: 'in_progress',
  succeeded: 'success',
  completed: 'success',
  failed: 'failure',
  canceled: 'aborted',
  cancelled: 'aborted',
};

/**
 * Trial status (API) mapped onto `test.case.result.status`. The merged registry
 * enum is `pass|fail` only, so non-verdict states emit nothing: the terminal state
 * lives on the REST trial and the span status.
 */
const trialStatusMap: Record<string, string> = {
  passed: 'pass',
  failed: 'fail',
};

export function runStatusTelemetry(status: string): string {
  return runStatusMap[status.trim().toLowerCase()] ?? 'in_progress';
}

export function trialStatusTelemetry(status: string): string {
  return trialStatusMap[status.trim().toLowerCase()] ?? '';
}

/** The OTel `gen_ai.evaluation.score.label` for a pass/fail verdict. */
export function scoreLabel(passed: boolean | undefined): string {
  if (passed === undefined) {
    return '';
  }
  return passed ? 'pass' : 'fail';
}

export interface TrialIdentityInput {
  experimentId: string;
  experimentName?: string;
  suiteId?: string;
  suiteVersion?: string;
  suiteName?: string;
  testCaseId?: string;
  testCaseName?: string;
  attempt?: number;
  trialId?: string;
  runStatus?: string;
  trialStatus?: string;
  conversationId?: string;
  operationName?: string;
}

/** Builds the `test.*` identity attribute set for a trial span. */
export function trialIdentityAttributes(input: TrialIdentityInput): Record<string, AttributeValue> {
  const attrs: Record<string, AttributeValue> = { [ATTR_SCHEMA_VERSION]: SCHEMA_VERSION };
  put(attrs, OPERATION_NAME, input.operationName ?? 'invoke_agent');
  put(attrs, TEST_SUITE_RUN_ID, input.experimentId);
  put(attrs, TEST_SUITE_NAME, firstNonBlank(input.suiteName, input.experimentName));
  put(attrs, TEST_SUITE_RUN_STATUS, runStatusTelemetry(input.runStatus ?? 'running'));
  put(attrs, TEST_SUITE_ID, input.suiteId);
  put(attrs, TEST_SUITE_VERSION, input.suiteVersion);
  put(attrs, TEST_CASE_ID, input.testCaseId);
  put(attrs, TEST_CASE_NAME, firstNonBlank(input.testCaseName, input.testCaseId));
  put(attrs, TEST_CASE_RESULT_STATUS, trialStatusTelemetry(input.trialStatus ?? 'running'));
  put(attrs, TEST_CASE_RUN_ID, input.trialId);
  attrs[TEST_CASE_RUN_ATTEMPT] = Math.trunc(input.attempt ?? 1);
  put(attrs, CONVERSATION_ID, input.conversationId);
  return attrs;
}

export interface ScoreEventInput {
  name: string;
  value: number | boolean | string | undefined;
  passed?: boolean;
  explanation?: string;
  evaluatorId?: string;
  evaluatorVersion?: string;
  evaluatorKind?: string;
  referenceSetId?: string;
  referenceSetVersion?: string;
  responseId?: string;
}

/** Builds the attribute set for one `gen_ai.evaluation.result` event. */
export function scoreEventAttributes(input: ScoreEventInput): Record<string, AttributeValue> {
  const attrs: Record<string, AttributeValue> = {};
  put(attrs, EVAL_NAME, input.name);
  // OTel score.value is a double, so only numeric and boolean values set it.
  if (typeof input.value === 'boolean') {
    attrs[EVAL_SCORE_VALUE] = input.value ? 1 : 0;
  } else if (typeof input.value === 'number') {
    attrs[EVAL_SCORE_VALUE] = input.value;
  }
  // The verdict lives in the label: pass/fail when known, else a categorical value.
  let label = scoreLabel(input.passed);
  if (label.length === 0 && typeof input.value === 'string') {
    label = input.value;
  }
  put(attrs, EVAL_SCORE_LABEL, label);
  put(attrs, EVAL_EXPLANATION, input.explanation);
  put(attrs, RESPONSE_ID, input.responseId);
  put(attrs, EVAL_EVALUATOR_ID, input.evaluatorId);
  put(attrs, EVAL_EVALUATOR_VERSION, input.evaluatorVersion);
  put(attrs, EVAL_EVALUATOR_TYPE, input.evaluatorKind);
  put(attrs, EVAL_REFERENCE_SET_ID, input.referenceSetId);
  put(attrs, EVAL_REFERENCE_SET_VERSION, input.referenceSetVersion);
  return attrs;
}

function put(attrs: Record<string, AttributeValue>, key: string, value: string | undefined): void {
  if (value !== undefined && value.length > 0) {
    attrs[key] = value;
  }
}

function firstNonBlank(...values: (string | undefined)[]): string | undefined {
  for (const value of values) {
    if (value !== undefined && value.length > 0) {
      return value;
    }
  }
  return undefined;
}
