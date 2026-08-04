import { validationError } from './errors.js';
import type { CreateExperimentRequest, ScoreItem, ScoreValue } from './models.js';
import { formatTimestamp } from './models.js';

/** The `source` object every experiment-run and score write carries. */
export const EXPERIMENT_RUN_SOURCE = { kind: 'sdk', id: 'js' } as const;

/**
 * Serializes a run upsert body.
 *
 * The backend ingest route keys on `experiment_id`, not `run_id`, and rejects
 * unknown fields, so `runId` is renamed here rather than at the call site.
 */
export function serializeUpsertRequest(request: CreateExperimentRequest): Record<string, unknown> {
  const name = request.name.trim();
  if (name.length === 0) {
    throw validationError('name is required');
  }
  const out: Record<string, unknown> = { name, source: { ...EXPERIMENT_RUN_SOURCE } };
  if (request.runId !== undefined && request.runId.length > 0) {
    out.experiment_id = request.runId;
  }
  if (request.description !== undefined && request.description.length > 0) {
    out.description = request.description;
  }
  if (request.tags !== undefined && request.tags.length > 0) {
    out.tags = [...request.tags];
  }
  if (request.suiteId !== undefined && request.suiteId.length > 0) {
    out.suite_id = request.suiteId;
  }
  if (request.suiteVersion !== undefined && request.suiteVersion.length > 0) {
    out.suite_version = request.suiteVersion;
  }
  if (request.candidate !== undefined && Object.keys(request.candidate).length > 0) {
    out.candidate = { ...request.candidate };
  }
  if (request.plannedTrialCount !== undefined) {
    if (request.plannedTrialCount < 0) {
      throw validationError('planned_trial_count must be non-negative');
    }
    out.planned_trial_count = request.plannedTrialCount;
  }
  if (request.metadata !== undefined && Object.keys(request.metadata).length > 0) {
    out.metadata = { ...request.metadata };
  }
  return out;
}

export function serializeScoreValue(value: ScoreValue): Record<string, unknown> {
  if (value.number !== undefined) {
    return { number: value.number };
  }
  if (value.boolean !== undefined) {
    return { bool: value.boolean };
  }
  if (value.string !== undefined) {
    return { string: value.string };
  }
  throw new Error('agento11y score validation failed: value must set one of number/boolean/string');
}

/**
 * Builds the score body.
 *
 * `evaluatorKind` is missing on purpose: it names the evaluator type on the OTel
 * evaluation event, and neither Go's nor Python's score body carries it.
 */
export function serializeScore(score: ScoreItem): Record<string, unknown> {
  const out: Record<string, unknown> = {
    score_id: score.scoreId,
    evaluator_id: score.evaluatorId,
    evaluator_version: score.evaluatorVersion,
    score_key: score.scoreKey,
    value: serializeScoreValue(score.value),
  };
  putNonBlank(out, 'generation_id', score.generationId);
  putNonBlank(out, 'conversation_id', score.conversationId);
  putNonBlank(out, 'experiment_id', score.experimentId);
  putNonBlank(out, 'trial_id', score.trialId);
  putNonBlank(out, 'test_case_id', score.testCaseId);
  putNonBlank(out, 'trace_id', score.traceId);
  putNonBlank(out, 'span_id', score.spanId);
  putNonBlank(out, 'grader_conversation_id', score.graderConversationId);
  putNonBlank(out, 'grader_generation_id', score.graderGenerationId);
  putNonBlank(out, 'grader_trace_id', score.graderTraceId);
  putNonBlank(out, 'rule_id', score.ruleId);
  if (score.passed !== undefined) {
    out.passed = score.passed;
  }
  putNonBlank(out, 'explanation', score.explanation);
  if (score.metadata !== undefined && Object.keys(score.metadata).length > 0) {
    out.metadata = { ...score.metadata };
  }
  if (score.createdAt !== undefined) {
    out.created_at = formatTimestamp(score.createdAt);
  }
  if (score.source !== undefined && ((score.source.kind ?? '').length > 0 || (score.source.id ?? '').length > 0)) {
    out.source = { kind: score.source.kind ?? '', id: score.source.id ?? '' };
  }
  return out;
}

/**
 * Rejects a score the backend would reject, so the error names the missing field
 * instead of arriving as a 400 body.
 */
export function validateScore(score: ScoreItem): void {
  const missing: string[] = [];
  for (const [name, raw] of [
    ['score_id', score.scoreId],
    ['evaluator_id', score.evaluatorId],
    ['evaluator_version', score.evaluatorVersion],
    ['score_key', score.scoreKey],
  ] as const) {
    if (raw.trim().length === 0) {
      missing.push(name);
    }
  }
  if (missing.length > 0) {
    throw new Error(`agento11y score validation failed: missing required field(s): ${missing.join(', ')}`);
  }
  // The backend requires a generation_id OR a trial_id.
  if ((score.generationId ?? '').trim().length === 0 && (score.trialId ?? '').trim().length === 0) {
    throw new Error('agento11y score validation failed: generation_id or trial_id is required');
  }
  serializeScoreValue(score.value);
}

/** Sets `key` only when `value` has content, so blank optional fields are omitted. */
export function putNonBlank(out: Record<string, unknown>, key: string, value: string | undefined): void {
  if (value !== undefined && value.length > 0) {
    out[key] = value;
  }
}
