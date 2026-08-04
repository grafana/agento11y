/**
 * The experiment ingest and query routes, copied from
 * `python/agento11y/_experiments_transport.py`.
 *
 * External writes go through the ingest lifecycle on the same tenant token used
 * for generation export:
 *
 * ```
 * POST   /api/v1/experiment-runs:upsert              create or claim an external run
 * POST   /api/v1/experiment-runs/{runId}:finalize    finalize an external run
 * POST   /api/v1/scores:export                       publish scores (attribute via experiment_id)
 * POST   /api/v1/generations:export                  publish the trial's anchor generation
 * POST   /api/v1/experiment-runs/{runId}/trials      create or claim a typed trial
 * PATCH  /api/v1/experiment-runs/{runId}/trials/{trialId}
 * POST   /api/v1/experiment-runs/{runId}/trials/{trialId}/artifacts:upload
 * POST   /api/v1/experiment-runs/{runId}/trials/{trialId}:evaluate
 * GET    /api/v1/experiment-runs/{runId}/trials/{trialId}/evaluations/{evaluationId}
 * ```
 *
 * Reads use the Agent Observability query routes on the same endpoint and auth:
 *
 * ```
 * GET    /api/v1/eval/experiments/{runId}            fetch a run
 * GET    /api/v1/eval/experiments/{runId}/scores     list run scores (paginated)
 * GET    /api/v1/eval/experiments/{runId}/report     aggregated run report
 * ```
 */

export const DEFAULT_PATH_PREFIX = '/api/v1';
export const EVAL_EXPERIMENTS_SUFFIX = '/eval/experiments';
export const EXPERIMENT_RUNS_PREFIX = '/api/v1/experiment-runs';
export const EXPERIMENT_RUNS_UPSERT_PATH = '/api/v1/experiment-runs:upsert';
export const SCORES_EXPORT_PATH = '/api/v1/scores:export';
export const GENERATIONS_EXPORT_PATH = '/api/v1/generations:export';

/** Percent-encodes one dynamic path segment.
 *
 * `encodeURIComponent` is required, not `encodeURI`: a `:` inside a trial ID
 * would otherwise shadow the trailing `:evaluate` and `:finalize` verbs, which
 * the ingest router reads off the raw path segment before decoding the ID.
 */
export function segment(value: string): string {
  return encodeURIComponent(value);
}

export function runUpsertPath(): string {
  return EXPERIMENT_RUNS_UPSERT_PATH;
}

export function runFinalizePath(runId: string): string {
  return `${EXPERIMENT_RUNS_PREFIX}/${segment(runId)}:finalize`;
}

export function scoresExportPath(): string {
  return SCORES_EXPORT_PATH;
}

export function generationsExportPath(): string {
  return GENERATIONS_EXPORT_PATH;
}

export function trialsPath(runId: string): string {
  return `${EXPERIMENT_RUNS_PREFIX}/${segment(runId)}/trials`;
}

export function trialPath(runId: string, trialId: string): string {
  return `${EXPERIMENT_RUNS_PREFIX}/${segment(runId)}/trials/${segment(trialId)}`;
}

export function trialArtifactUploadPath(runId: string, trialId: string): string {
  return `${trialPath(runId, trialId)}/artifacts:upload`;
}

export function trialEvaluatePath(runId: string, trialId: string): string {
  return `${trialPath(runId, trialId)}:evaluate`;
}

export function trialEvaluationPath(runId: string, trialId: string, evaluationId: string): string {
  return `${trialPath(runId, trialId)}/evaluations/${segment(evaluationId)}`;
}

export function experimentReadPath(runId: string, pathPrefix: string = DEFAULT_PATH_PREFIX): string {
  const prefix = `/${pathPrefix.trim().replace(/^\/+|\/+$/g, '')}`;
  return `${prefix}${EVAL_EXPERIMENTS_SUFFIX}/${segment(runId)}`;
}

export function experimentScoresPath(runId: string, pathPrefix: string = DEFAULT_PATH_PREFIX): string {
  return `${experimentReadPath(runId, pathPrefix)}/scores`;
}

export function experimentReportPath(runId: string, pathPrefix: string = DEFAULT_PATH_PREFIX): string {
  return `${experimentReadPath(runId, pathPrefix)}/report`;
}
