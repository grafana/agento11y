/**
 * Error shapes for the experiments surface.
 *
 * The SDK convention is a stable message prefix acting as the sentinel, matching
 * `submitConversationRating`. Only caller-actionable control flow gets a
 * subclass: a disabled feature, a failed evaluation worker, a timeout, and a
 * conflict whose kind decides whether the caller can retry.
 */

/** Prefix for a request the SDK rejected before sending it, and for HTTP 400/422. */
export const VALIDATION_PREFIX = 'agento11y experiment validation failed';
/** Prefix for HTTP 404. */
export const NOT_FOUND_PREFIX = 'agento11y experiment not found';
/** Prefix for HTTP 409. */
export const CONFLICT_PREFIX = 'agento11y experiment conflict';
/** Prefix for a transport failure, an unusable response, and an exhausted retry budget. */
export const TRANSPORT_PREFIX = 'agento11y experiment transport failed';

/** Stable categories for experiment and suite HTTP 409 responses. */
export type ConflictKind =
  | 'score_count_mismatch'
  | 'running_trials'
  | 'pending_evaluations'
  | 'terminal'
  | 'immutable_field'
  | 'open_draft'
  | 'unknown';

/**
 * Classifies backend conflict text so callers do not parse strings.
 *
 * Kept in sync with Python's `classify_conflict` and Go's `ClassifyConflict`.
 */
export function classifyConflict(message: string): ConflictKind {
  const value = message.toLowerCase();
  if (
    value.includes('score_count') ||
    value.includes('score count') ||
    (value.includes('expected ') && value.includes(' scores, found '))
  ) {
    return 'score_count_mismatch';
  }
  if (value.includes('pending evaluation')) {
    return 'pending_evaluations';
  }
  if (
    value.includes('running trial') ||
    value.includes('open trial') ||
    (value.includes('cannot complete experiment with ') && value.includes(' trial'))
  ) {
    return 'running_trials';
  }
  if (
    value.includes('terminal') ||
    value.includes('already completed') ||
    value.includes('already finalized') ||
    value.includes('already published')
  ) {
    return 'terminal';
  }
  if (
    value.includes('immutable') ||
    value.includes('cannot change') ||
    value.includes('conflicts with the existing experiment') ||
    value.includes('not a draft')
  ) {
    return 'immutable_field';
  }
  if (value.includes('draft')) {
    return 'open_draft';
  }
  return 'unknown';
}

/** A 409 whose classified kind tells the caller whether the state is recoverable. */
export class ExperimentConflictError extends Error {
  readonly kind: ConflictKind;

  constructor(message: string, kind: ConflictKind) {
    super(message);
    this.name = 'ExperimentConflictError';
    this.kind = kind;
  }

  /** Whether the conflict describes state a caller can wait out or correct. */
  get recoverable(): boolean {
    return (
      this.kind === 'score_count_mismatch' ||
      this.kind === 'running_trials' ||
      this.kind === 'pending_evaluations' ||
      this.kind === 'open_draft'
    );
  }
}

/**
 * A stored evaluator ended a trial evaluation unsuccessfully. The evaluation keeps
 * its row server-side, so triggering the same evaluator again requeues it.
 */
export class TrialEvaluationFailedError extends Error {
  readonly evaluationId: string;
  readonly detail: string;

  constructor(evaluationId: string, detail: string) {
    super(trialEvaluationMessage('failed', evaluationId, detail));
    this.name = 'TrialEvaluationFailedError';
    this.evaluationId = evaluationId.trim();
    this.detail = detail.trim();
  }
}

/**
 * A trial evaluation did not reach a terminal status before the caller's
 * deadline. The evaluation keeps running server-side.
 */
export class TrialEvaluationTimeoutError extends Error {
  readonly evaluationId: string;
  readonly detail: string;

  constructor(evaluationId: string, detail: string) {
    super(trialEvaluationMessage('timed out', evaluationId, detail));
    this.name = 'TrialEvaluationTimeoutError';
    this.evaluationId = evaluationId.trim();
    this.detail = detail.trim();
  }
}

/** Formats one trial evaluation error. A blank detail is dropped, not restated. */
function trialEvaluationMessage(outcome: string, evaluationId: string, detail: string): string {
  const id = evaluationId.trim().length > 0 ? evaluationId.trim() : 'unknown';
  const normalizedDetail = detail.trim();
  if (normalizedDetail.length === 0) {
    return `agento11y trial evaluation ${id} ${outcome}`;
  }
  return `agento11y trial evaluation ${id} ${outcome}: ${normalizedDetail}`;
}

export function validationError(detail: string): Error {
  return new Error(`${VALIDATION_PREFIX}: ${detail}`);
}

export function notFoundError(detail: string): Error {
  return new Error(`${NOT_FOUND_PREFIX}: ${detail}`);
}

export function conflictError(detail: string): ExperimentConflictError {
  return new ExperimentConflictError(`${CONFLICT_PREFIX}: ${detail}`, classifyConflict(detail));
}

export function transportError(detail: string): Error {
  return new Error(`${TRANSPORT_PREFIX}: ${detail}`);
}
