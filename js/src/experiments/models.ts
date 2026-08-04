import { transportError } from './errors.js';

/** Terminal status an external run can be finalized to (plus `running`). */
export type ExperimentStatus = 'running' | 'completed' | 'failed';

/** Lifecycle of a single trial (one test case attempt). */
export type TrialStatus = 'running' | 'completed' | 'passed' | 'failed' | 'errored' | 'skipped';

/** The OTel-aligned evaluator type vocabulary. */
export type EvaluatorKind = 'llm_judge' | 'deterministic' | 'human' | 'custom';

/** Durable state of a stored evaluator run for an experiment trial. */
export type TrialEvaluationStatus = 'queued' | 'claimed' | 'success' | 'failed';

const trialEvaluationStatuses: readonly TrialEvaluationStatus[] = ['queued', 'claimed', 'success', 'failed'];

/** Whether the worker has finished the evaluation. */
export function isTerminalEvaluationStatus(status: TrialEvaluationStatus): boolean {
  return status === 'success' || status === 'failed';
}

/** Maps a free-form evaluator kind onto the OTel-aligned set. */
export function normalizeEvaluatorKind(kind: string): EvaluatorKind {
  const k = kind.trim().toLowerCase();
  if (k === 'llm_judge' || k === 'llm-judge' || k === 'llm' || k === 'judge' || k === 'rubric') {
    return 'llm_judge';
  }
  if (k === 'deterministic' || k === 'check' || k === 'rule' || k === 'exact' || k === 'code') {
    return 'deterministic';
  }
  if (k === 'human' || k === 'manual' || k === 'annotator') {
    return 'human';
  }
  return 'custom';
}

/** One score's value. Exactly one field is set. */
export interface ScoreValue {
  number?: number;
  boolean?: boolean;
  string?: string;
}

export interface ScoreSource {
  kind?: string;
  id?: string;
}

/** One score ready to publish through `POST /api/v1/scores:export`. */
export interface ScoreItem {
  scoreId: string;
  evaluatorId: string;
  evaluatorVersion: string;
  evaluatorKind?: string;
  scoreKey: string;
  value: ScoreValue;
  generationId?: string;
  conversationId?: string;
  experimentId?: string;
  trialId?: string;
  testCaseId?: string;
  traceId?: string;
  spanId?: string;
  graderConversationId?: string;
  graderGenerationId?: string;
  graderTraceId?: string;
  ruleId?: string;
  passed?: boolean;
  explanation?: string;
  metadata?: Record<string, unknown>;
  createdAt?: Date;
  source?: ScoreSource;
}

export interface ExportScoreResult {
  scoreId: string;
  accepted: boolean;
  status: string;
  error: string;
}

export interface ExportScoresResponse {
  results: ExportScoreResult[];
  accepted: number;
  duplicates: number;
  rejectedCount: number;
  /** Scores the backend refused outright, excluding idempotent duplicates. */
  rejected: ExportScoreResult[];
}

/** The candidate under test, as recorded on a run and its trials. */
export interface Candidate {
  agentName?: string;
  agentVersion?: string;
  promptVersion?: string;
  modelProvider?: string;
  modelName?: string;
  gitSha?: string;
}

/** Provenance for whatever produced a score. */
export interface Evaluator {
  evaluatorId: string;
  version?: string;
  kind?: string;
  referenceSetId?: string;
  referenceSetVersion?: string;
}

export interface CreateExperimentRequest {
  runId?: string;
  name: string;
  description?: string;
  tags?: string[];
  suiteId?: string;
  suiteVersion?: string;
  candidate?: Record<string, unknown>;
  plannedTrialCount?: number;
  metadata?: Record<string, unknown>;
}

export interface ExperimentReportSummary {
  testCaseCount: number;
  trialCount: number;
  completedCount: number;
  failedCount: number;
  canceledCount: number;
  /** Unset when the backend omitted it, which is the case for a cloud-evaluated run. */
  passRate?: number;
  passAtK: Record<string, number>;
  passPowerK: Record<string, number>;
  finalScoreAvg?: number;
  totalCost?: number;
  totalTokens?: number;
  passCount: number;
  passDenominator: number;
  finalScoreSum: number;
  finalScoreCount: number;
  tokenCoverage: string;
  costCoverage: string;
}

/** An experiment run as the backend returns it. */
export interface Experiment {
  runId: string;
  name: string;
  status: string;
  tenantId: string;
  description: string;
  tags: string[];
  suiteId: string;
  suiteVersion: string;
  candidate: Record<string, unknown>;
  metadata: Record<string, unknown>;
  error: string;
  plannedTrialCount?: number;
  resultStatus: string;
  resultError: string;
  result?: ExperimentReportSummary;
  createdBy: string;
  createdAt?: Date;
  updatedAt?: Date;
  startedAt?: Date;
  completedAt?: Date;
}

export interface ExperimentReport {
  run: Experiment;
  summary: ExperimentReportSummary;
  rows: Record<string, unknown>[];
}

/** The work row a stored evaluator run is tracked by. */
export interface TrialEvaluation {
  evaluationId: string;
  experimentId: string;
  trialId: string;
  testCaseId: string;
  conversationId: string;
  evaluatorId: string;
  evaluatorVersion: string;
  status: TrialEvaluationStatus;
  attempts: number;
  scheduledAt?: Date;
  createdAt?: Date;
  updatedAt?: Date;
  error: string;
}

// --------------------------------------------------------------------------- //
// Parsers
// --------------------------------------------------------------------------- //

export function parseExperiment(payload: unknown): Experiment {
  if (!isRecord(payload)) {
    throw transportError('invalid response payload');
  }
  return {
    runId: str(payload.experiment_id) || str(payload.run_id),
    name: str(payload.name),
    status: str(payload.status),
    tenantId: str(payload.tenant_id),
    description: str(payload.description),
    tags: strList(payload.tags),
    suiteId: str(payload.suite_id),
    suiteVersion: str(payload.suite_version),
    candidate: record(payload.candidate),
    metadata: record(payload.metadata),
    error: str(payload.error),
    plannedTrialCount: optionalInt(payload.planned_trial_count),
    resultStatus: str(payload.result_status),
    resultError: str(payload.result_error),
    result: isRecord(payload.result) ? parseReportSummary(payload.result) : undefined,
    createdBy: str(payload.created_by),
    createdAt: parseTimestamp(payload.created_at),
    updatedAt: parseTimestamp(payload.updated_at),
    startedAt: parseTimestamp(payload.started_at),
    completedAt: parseTimestamp(payload.completed_at),
  };
}

/** Accepts both the `{run: {...}}` envelope and a bare run object. */
export function parseExperimentRunResponse(payload: unknown): Experiment {
  if (isRecord(payload) && isRecord(payload.run)) {
    return parseExperiment(payload.run);
  }
  return parseExperiment(payload);
}

export function parseReportSummary(payload: unknown): ExperimentReportSummary {
  const summary = isRecord(payload) ? payload : {};
  return {
    testCaseCount: int(summary.test_case_count),
    trialCount: int(summary.trial_count),
    completedCount: int(summary.completed_count),
    failedCount: int(summary.failed_count),
    canceledCount: int(summary.canceled_count),
    passRate: optionalNumber(summary.pass_rate),
    passAtK: numberMap(summary.pass_at_k),
    passPowerK: numberMap(summary.pass_power_k),
    finalScoreAvg: optionalNumber(summary.final_score_avg),
    totalCost: optionalNumber(summary.total_cost),
    totalTokens: optionalInt(summary.total_tokens),
    passCount: int(summary.pass_count),
    passDenominator: int(summary.pass_denominator),
    finalScoreSum: num(summary.final_score_sum),
    finalScoreCount: int(summary.final_score_count),
    tokenCoverage: str(summary.token_coverage),
    costCoverage: str(summary.cost_coverage),
  };
}

/** Accepts both report envelopes: the backend keys the run under `experiment`, older drafts under `run`. */
export function parseExperimentReport(payload: unknown): ExperimentReport {
  if (!isRecord(payload)) {
    throw transportError('invalid response payload');
  }
  const runPayload = isRecord(payload.experiment) ? payload.experiment : isRecord(payload.run) ? payload.run : {};
  return {
    run: parseExperiment(runPayload),
    summary: parseReportSummary(payload.summary),
    rows: Array.isArray(payload.rows) ? payload.rows.filter(isRecord) : [],
  };
}

/**
 * Parses one evaluation row.
 *
 * An unknown status is a hard error, not "keep polling": a newer terminal state
 * would otherwise read as non-terminal and poll until the caller's deadline. A
 * blank `evaluation_id` would turn the next status read into a validation error
 * that blames the caller's arguments.
 */
export function parseTrialEvaluation(payload: unknown): TrialEvaluation {
  if (!isRecord(payload)) {
    throw transportError('invalid response payload');
  }
  const rawStatus = str(payload.status);
  if (!trialEvaluationStatuses.includes(rawStatus as TrialEvaluationStatus)) {
    throw transportError(`unsupported evaluation status "${rawStatus}"`);
  }
  const evaluationId = str(payload.evaluation_id).trim();
  if (evaluationId.length === 0) {
    throw transportError('evaluation response carries no evaluation_id');
  }
  return {
    evaluationId,
    experimentId: str(payload.experiment_id),
    trialId: str(payload.trial_id),
    testCaseId: str(payload.test_case_id),
    conversationId: str(payload.conversation_id),
    evaluatorId: str(payload.evaluator_id),
    evaluatorVersion: str(payload.evaluator_version),
    status: rawStatus as TrialEvaluationStatus,
    attempts: int(payload.attempts),
    scheduledAt: parseTimestamp(payload.scheduled_at),
    createdAt: parseTimestamp(payload.created_at),
    updatedAt: parseTimestamp(payload.updated_at),
    error: str(payload.error),
  };
}

export function parseExportScoresResponse(payload: unknown): ExportScoresResponse {
  if (!isRecord(payload)) {
    throw transportError('invalid response payload');
  }
  const results: ExportScoreResult[] = [];
  for (const entry of Array.isArray(payload.results) ? payload.results : []) {
    if (!isRecord(entry)) {
      continue;
    }
    results.push({
      scoreId: str(entry.score_id),
      accepted: entry.accepted === true,
      status: str(entry.status),
      error: str(entry.error),
    });
  }
  const accepted = int(payload.accepted) || results.filter((result) => result.accepted).length;
  const duplicates = int(payload.duplicates) || results.filter((result) => result.status === 'duplicate').length;
  const rejected = results.filter((result) => !result.accepted && result.status !== 'duplicate');
  return {
    results,
    accepted,
    duplicates,
    rejectedCount: int(payload.rejected) || rejected.length,
    rejected,
  };
}

// --------------------------------------------------------------------------- //
// Field decoding
// --------------------------------------------------------------------------- //

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

export function num(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

export function int(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? Math.trunc(value) : 0;
}

export function optionalNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

export function optionalInt(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? Math.trunc(value) : undefined;
}

function strList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((entry): entry is string => typeof entry === 'string') : [];
}

function record(value: unknown): Record<string, unknown> {
  return isRecord(value) ? { ...value } : {};
}

function numberMap(value: unknown): Record<string, number> {
  if (!isRecord(value)) {
    return {};
  }
  const out: Record<string, number> = {};
  for (const [key, raw] of Object.entries(value)) {
    out[key] = num(raw);
  }
  return out;
}

/** Parses an RFC 3339 timestamp, returning undefined for a blank or unparseable value. */
export function parseTimestamp(value: unknown): Date | undefined {
  if (typeof value !== 'string' || value.trim().length === 0) {
    return undefined;
  }
  const parsed = new Date(value.trim());
  return Number.isNaN(parsed.getTime()) ? undefined : parsed;
}

/**
 * Formats a timestamp the way the backend expects it: RFC 3339 in UTC.
 *
 * Milliseconds survive, because two scores created 300 ms apart must not carry
 * the same `created_at`. Only an all-zero fraction is dropped, which is what Go's
 * `time.RFC3339Nano` does and what keeps the whole-second fixture in
 * `conformance/experiments/requests.json` identical across the three SDKs.
 */
export function formatTimestamp(value: Date): string {
  return value.toISOString().replace(/\.000Z$/, 'Z');
}

/** Reads a paging cursor, treating a blank value and `"0"` as "no more pages". */
export function normalizeCursor(value: unknown): string | undefined {
  if (typeof value !== 'string') {
    return undefined;
  }
  const text = value.trim();
  if (text.length === 0 || text === '0') {
    return undefined;
  }
  return text;
}
