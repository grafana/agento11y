import { defaultEnv, parseTruthy, resolveHeadersWithAuth } from '../config.js';
import { redactSecretText, redactSecretValue } from '../redaction.js';
import type { Agento11yLogger, TokenUsage } from '../types.js';
import { baseURLFromAPIEndpoint } from '../utils.js';
import { transportError, validationError } from './errors.js';
import { FEATURE_CLOUD_TRIAL_EVALUATION, requireExperimental } from './experimental.js';
import type {
  CreateExperimentRequest,
  Experiment,
  ExperimentReport,
  ExperimentStatus,
  ScoreItem,
  TrialEvaluation,
} from './models.js';
import {
  isRecord,
  normalizeCursor,
  parseExperimentReport,
  parseExperimentRunResponse,
  parseExportScoresResponse,
  parseTrialEvaluation,
  str,
} from './models.js';
import * as routes from './routes.js';
import {
  EXPERIMENT_RUN_SOURCE,
  putNonBlank,
  serializeScore,
  serializeUpsertRequest,
  validateScore,
} from './serialize.js';
import type { ExperimentsConnection, ExperimentsRetryPolicy } from './transport.js';
import { requestExperimentsJSON, sleepWithSignal, throwIfAborted } from './transport.js';

/** Header that attributes an ingest write to the writing SDK. Python sends the same one. */
export const INGEST_ACTOR_HEADER = 'X-Agento11y-Ingest-Actor';
/** Default ingest actor for this SDK. Python sends `ingest:sdk/python`. */
export const DEFAULT_INGEST_ACTOR = 'ingest:sdk/js';

const ENV_ENDPOINT = 'AGENTO11Y_ENDPOINT';
const ENV_AUTH_TOKEN = 'AGENTO11Y_AUTH_TOKEN';
const ENV_AUTH_TENANT_ID = 'AGENTO11Y_AUTH_TENANT_ID';
const ENV_INGEST_ACTOR = 'AGENTO11Y_INGEST_ACTOR';
const ENV_GRAFANA_URL = 'AGENTO11Y_GRAFANA_URL';
const ENV_USE_EXPERIMENTAL_OTEL = 'AGENTO11Y_USE_EXPERIMENTAL_OTEL';

/** A client whose queued generations a trial flushes before starting an evaluation. */
export interface FlushableGenerationClient {
  flush(): Promise<void>;
}

export interface ExperimentsClientOptions {
  /** Agent Observability endpoint. Falls back to `AGENTO11Y_ENDPOINT`. */
  endpoint?: string;
  /** Stack id. Falls back to `AGENTO11Y_AUTH_TENANT_ID`. Basic auth is used when set. */
  tenantId?: string;
  /** Cloud ingestion API key. Falls back to `AGENTO11Y_AUTH_TOKEN`. */
  ingestToken?: string;
  /** Ingest actor header value. Defaults to `ingest:sdk/js`. */
  actor?: string;
  /** Grafana base URL used to build experiment deep links. */
  grafanaUrl?: string;
  /** Per-attempt request timeout. */
  timeoutMs?: number;
  insecure?: boolean;
  retry?: Partial<ExperimentsRetryPolicy>;
  /** Redact known secret formats from scores, generations, and artifacts. Defaults to true. */
  redactSecrets?: boolean;
  /** Emit experimental trial spans and evaluation events. Falls back to `AGENTO11Y_USE_EXPERIMENTAL_OTEL`. */
  useExperimentalOtel?: boolean;
  /** An already-instrumented client whose generations a trial flushes before evaluating. */
  coreClient?: FlushableGenerationClient;
  fetchImpl?: typeof fetch;
  /** Replaces every wait: transport backoff and evaluation poll delays. Test seam. */
  sleep?: (durationMs: number, signal?: AbortSignal) => Promise<void>;
  /** Replaces the monotonic clock the evaluation deadline reads. Test seam. */
  now?: () => number;
  env?: Record<string, string | undefined>;
  logger?: Agento11yLogger;
}

export interface FinalizeExperimentOptions {
  scoreCount?: number;
  error?: string;
}

export interface UpsertTrialRequest {
  trialId: string;
  testCaseId: string;
  attempt?: number;
  status?: string;
  conversationId?: string;
  traceId?: string;
  spanId?: string;
  testCase?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export interface UpdateTrialRequest {
  status?: string;
  error?: string;
  cost?: number;
  inputTokens?: number;
  outputTokens?: number;
  durationMs?: number;
  conversationId?: string;
  traceId?: string;
  spanId?: string;
}

export interface ExportGenerationRequest {
  generationId: string;
  conversationId?: string;
  inputText?: string;
  outputText?: string;
  modelProvider?: string;
  modelName?: string;
  agentName?: string;
  agentVersion?: string;
  operationName?: string;
  inputTokens?: number;
  outputTokens?: number;
  usage?: TokenUsage;
  tags?: Record<string, string>;
  metadata?: Record<string, unknown>;
}

export interface UploadArtifactRequest {
  experimentId: string;
  /** The trial the artifact attaches to. */
  parentId: string;
  name: string;
  kind: string;
  content: Uint8Array;
  mime?: string;
  parentKind?: string;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

/**
 * Connection and auth for experiment writes on a single ingest token.
 *
 * Every write (run upsert, trial upsert, score export, generation export,
 * artifact upload, finalize) shares the tenant ingest credential. Stored test
 * suites are the exception: they live behind the Grafana control plane and use
 * `TestSuitesClient` with its own credential.
 */
export class ExperimentsClient {
  readonly endpoint: string;
  readonly tenantId: string;
  readonly actor: string;
  readonly grafanaUrl: string;
  readonly redactSecrets: boolean;
  readonly useExperimentalOtel: boolean;
  /** The wait used by evaluation polling. Rejects with the signal's reason on abort. */
  readonly sleepFn: (durationMs: number, signal?: AbortSignal) => Promise<void>;
  /** The monotonic clock the evaluation deadline reads. */
  readonly nowMs: () => number;

  private readonly connection: ExperimentsConnection;
  private readonly coreClient: FlushableGenerationClient | undefined;
  private readonly logger: Agento11yLogger | undefined;

  constructor(options: ExperimentsClientOptions = {}) {
    const env = options.env ?? defaultEnv();
    const endpoint = (options.endpoint ?? env[ENV_ENDPOINT] ?? '').trim();
    if (endpoint.length === 0) {
      throw new Error(
        'agento11y experiments: endpoint is required; pass endpoint or set AGENTO11Y_ENDPOINT to your Grafana Cloud Agent Observability URL',
      );
    }
    const ingestToken = (options.ingestToken ?? env[ENV_AUTH_TOKEN] ?? '').trim();
    if (ingestToken.length === 0) {
      throw new Error(
        'agento11y experiments: ingestToken is required; pass ingestToken or set AGENTO11Y_AUTH_TOKEN to your Grafana Cloud ingestion API key',
      );
    }

    this.endpoint = endpoint.replace(/\/+$/, '');
    this.tenantId = (options.tenantId ?? env[ENV_AUTH_TENANT_ID] ?? '').trim();
    // The backend derives the same identity from the run's sdk/js source. Sending
    // it on every request also covers routes such as artifact upload, which
    // cannot carry a JSON source object.
    this.actor = (options.actor ?? env[ENV_INGEST_ACTOR] ?? '').trim() || DEFAULT_INGEST_ACTOR;
    this.grafanaUrl = (options.grafanaUrl ?? env[ENV_GRAFANA_URL] ?? '').trim().replace(/\/+$/, '');
    this.redactSecrets = options.redactSecrets ?? true;
    this.useExperimentalOtel = options.useExperimentalOtel ?? parseTruthy(env[ENV_USE_EXPERIMENTAL_OTEL] ?? '');
    this.coreClient = options.coreClient;
    this.logger = options.logger;
    this.sleepFn = options.sleep ?? sleepWithSignal;
    this.nowMs = options.now ?? defaultMonotonicNow;

    this.connection = {
      endpoint: this.endpoint,
      insecure: options.insecure ?? this.endpoint.startsWith('http://'),
      headers: this.buildHeaders(ingestToken),
      retry: { ...options.retry, ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}) },
      ...(options.fetchImpl !== undefined ? { fetchImpl: options.fetchImpl } : {}),
      ...(options.sleep !== undefined ? { sleep: options.sleep } : {}),
    };
  }

  // --- experiment lifecycle ------------------------------------------------ //

  /** Creates or idempotently claims an external run. */
  async upsertExperiment(request: CreateExperimentRequest, options: RequestOptions = {}): Promise<Experiment> {
    const body = await this.request(
      {
        method: 'POST',
        path: routes.runUpsertPath(),
        body: serializeUpsertRequest(request),
        label: 'experiment create',
      },
      options,
    );
    return parseExperimentRunResponse(body);
  }

  /** Finalizes a run as `completed` or `failed`. */
  async finalize(
    experimentId: string,
    status: ExperimentStatus | string = 'completed',
    finalizeOptions: FinalizeExperimentOptions = {},
    options: RequestOptions = {},
  ): Promise<Experiment> {
    // The backend's terminal success status is `completed`; `succeeded` is a
    // friendly alias mapped onto the wire value the server validates.
    let normalized = status.trim().toLowerCase();
    if (normalized === 'succeeded' || normalized === 'completed') {
      normalized = 'completed';
    } else if (normalized !== 'failed') {
      throw validationError('status must be completed or failed');
    }
    const runId = requireField(experimentId, 'run_id');
    const payload: Record<string, unknown> = { status: normalized, source: { ...EXPERIMENT_RUN_SOURCE } };
    if (finalizeOptions.scoreCount !== undefined) {
      payload.score_count = finalizeOptions.scoreCount;
    }
    if (finalizeOptions.error !== undefined && finalizeOptions.error.length > 0) {
      payload.error = finalizeOptions.error;
    }
    const body = await this.request(
      { method: 'POST', path: routes.runFinalizePath(runId), body: payload, label: 'experiment finalize' },
      options,
    );
    return parseExperimentRunResponse(body);
  }

  // --- trials -------------------------------------------------------------- //

  /** Creates or idempotently upserts a typed trial under a run. */
  async upsertTrial(
    experimentId: string,
    request: UpsertTrialRequest,
    options: RequestOptions = {},
  ): Promise<Record<string, unknown>> {
    const runId = requireField(experimentId, 'experiment_id is required for trial create');
    const payload: Record<string, unknown> = {
      trial_id: requireField(request.trialId, 'trial_id'),
      test_case_id: requireField(request.testCaseId, 'test_case_id'),
      attempt: request.attempt ?? 1,
      status: request.status ?? 'running',
    };
    putNonBlank(payload, 'conversation_id', request.conversationId);
    putNonBlank(payload, 'trace_id', request.traceId);
    putNonBlank(payload, 'span_id', request.spanId);
    if (request.testCase !== undefined) {
      payload.test_case = { ...request.testCase };
    }
    if (request.metadata !== undefined && Object.keys(request.metadata).length > 0) {
      payload.metadata = { ...request.metadata };
    }
    const body = await this.request(
      { method: 'POST', path: routes.trialsPath(runId), body: payload, label: 'test case trial create' },
      options,
    );
    return isRecord(body) ? body : {};
  }

  /** Patches a typed trial's status, bindings, and usage rollups. */
  async updateTrial(
    experimentId: string,
    trialId: string,
    request: UpdateTrialRequest,
    options: RequestOptions = {},
  ): Promise<Record<string, unknown>> {
    const runId = requireField(experimentId, 'experiment_id is required for trial update');
    const normalizedTrialId = requireField(trialId, 'trial_id');
    const payload: Record<string, unknown> = {};
    putNonBlank(payload, 'status', request.status);
    putNonBlank(payload, 'error', request.error);
    if (request.cost !== undefined) {
      payload.cost = request.cost;
    }
    if (request.inputTokens !== undefined) {
      payload.input_tokens = Math.trunc(request.inputTokens);
    }
    if (request.outputTokens !== undefined) {
      payload.output_tokens = Math.trunc(request.outputTokens);
    }
    if (request.durationMs !== undefined) {
      payload.duration_ms = Math.trunc(request.durationMs);
    }
    putNonBlank(payload, 'conversation_id', request.conversationId);
    putNonBlank(payload, 'trace_id', request.traceId);
    putNonBlank(payload, 'span_id', request.spanId);
    const body = await this.request(
      {
        method: 'PATCH',
        path: routes.trialPath(runId, normalizedTrialId),
        body: payload,
        label: 'test case trial update',
      },
      options,
    );
    return isRecord(body) ? body : {};
  }

  /**
   * Queues a stored evaluator for a trial's bound conversation.
   *
   * Experimental: requires `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`,
   * otherwise it rejects with `ExperimentalFeatureDisabledError` without sending
   * a request.
   */
  async triggerTrialEvaluation(
    experimentId: string,
    trialId: string,
    evaluatorId: string,
    evaluatorVersion = '',
    options: RequestOptions = {},
  ): Promise<TrialEvaluation> {
    requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION);
    const runId = requireTrialEvaluationField(experimentId, 'experiment_id');
    const normalizedTrialId = requireTrialEvaluationField(trialId, 'trial_id');
    const normalizedEvaluatorId = requireTrialEvaluationField(evaluatorId, 'evaluator_id');
    const payload: Record<string, unknown> = { evaluator_id: normalizedEvaluatorId };
    const version = evaluatorVersion.trim();
    if (version.length > 0) {
      payload.evaluator_version = version;
    }
    const body = await this.request(
      {
        method: 'POST',
        path: routes.trialEvaluatePath(runId, normalizedTrialId),
        body: payload,
        label: 'trial evaluation trigger',
      },
      options,
    );
    return parseTrialEvaluation(body);
  }

  /**
   * Reads durable status for a triggered trial evaluation.
   *
   * Experimental: requires `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`,
   * otherwise it rejects with `ExperimentalFeatureDisabledError` without sending
   * a request.
   */
  async getTrialEvaluation(
    experimentId: string,
    trialId: string,
    evaluationId: string,
    options: RequestOptions = {},
  ): Promise<TrialEvaluation> {
    requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION);
    const runId = requireTrialEvaluationField(experimentId, 'experiment_id');
    const normalizedTrialId = requireTrialEvaluationField(trialId, 'trial_id');
    const normalizedEvaluationId = requireTrialEvaluationField(evaluationId, 'evaluation_id');
    const body = await this.request(
      {
        method: 'GET',
        path: routes.trialEvaluationPath(runId, normalizedTrialId, normalizedEvaluationId),
        label: 'trial evaluation status',
      },
      options,
    );
    return parseTrialEvaluation(body);
  }

  // --- scores -------------------------------------------------------------- //

  /**
   * Exports scores and returns the count recorded, counting an idempotent
   * duplicate as recorded. An empty list sends no request.
   */
  async exportScores(
    scores: ScoreItem[],
    exportOptions: { raiseOnReject?: boolean } = {},
    options: RequestOptions = {},
  ): Promise<number> {
    if (scores.length === 0) {
      return 0;
    }
    const prepared = scores.map((score) => (this.redactSecrets ? this.redactScore(score) : score));
    for (const score of prepared) {
      validateScore(score);
    }
    const body = await this.request(
      {
        method: 'POST',
        path: routes.scoresExportPath(),
        body: { scores: prepared.map(serializeScore) },
        label: 'score export',
      },
      options,
    );
    const response = parseExportScoresResponse(body);
    if ((exportOptions.raiseOnReject ?? true) && response.rejected.length > 0) {
      const details = response.rejected
        .map((result) => `${result.scoreId}: ${result.error.length > 0 ? result.error : 'rejected'}`)
        .join('; ');
      throw new Error(`agento11y score export rejected ${response.rejected.length} score(s): ${details}`);
    }
    return response.accepted + response.duplicates;
  }

  // --- generations --------------------------------------------------------- //

  /**
   * Exports one generation over the experiments transport.
   *
   * The response is checked, unlike `Agento11yClient.flush()`, which only warns.
   * A trial that silently loses its anchor generation later fails evaluation with
   * a confusing server-side "no conversation" error.
   */
  async exportGeneration(request: ExportGenerationRequest, options: RequestOptions = {}): Promise<string> {
    const generationId = requireField(request.generationId, 'generation_id');
    let generation: Record<string, unknown> = {
      id: generationId,
      operation_name:
        request.operationName !== undefined && request.operationName.length > 0
          ? request.operationName
          : 'invoke_agent',
      mode: 'GENERATION_MODE_SYNC',
      model: {
        provider:
          request.modelProvider !== undefined && request.modelProvider.length > 0 ? request.modelProvider : 'eval',
        name: request.modelName !== undefined && request.modelName.length > 0 ? request.modelName : 'experiment',
      },
    };
    putNonBlank(generation, 'conversation_id', request.conversationId);
    putNonBlank(generation, 'agent_name', request.agentName);
    putNonBlank(generation, 'agent_version', request.agentVersion);
    if (request.inputText !== undefined && request.inputText.length > 0) {
      generation.input = [{ role: 'MESSAGE_ROLE_USER', parts: [{ text: request.inputText }] }];
    }
    if (request.outputText !== undefined && request.outputText.length > 0) {
      generation.output = [{ role: 'MESSAGE_ROLE_ASSISTANT', parts: [{ text: request.outputText }] }];
    }
    if (request.tags !== undefined && Object.keys(request.tags).length > 0) {
      generation.tags = { ...request.tags };
    }
    if (request.metadata !== undefined && Object.keys(request.metadata).length > 0) {
      generation.metadata = { ...request.metadata };
    }
    const usage = normalizeUsage(
      request.usage ?? { inputTokens: request.inputTokens ?? 0, outputTokens: request.outputTokens ?? 0 },
    );
    if (usage !== undefined) {
      generation.usage = usage;
    }
    if (this.redactSecrets) {
      generation = redactSecretValue(generation);
    }

    const body = await this.request(
      {
        method: 'POST',
        path: routes.generationsExportPath(),
        body: { generations: [generation] },
        label: 'generation export',
      },
      options,
    );
    assertGenerationAccepted(body);
    return generationId;
  }

  /** Alias for `exportGeneration`, matching Python's `record_generation`. */
  async recordGeneration(request: ExportGenerationRequest, options: RequestOptions = {}): Promise<string> {
    return this.exportGeneration(request, options);
  }

  /**
   * Flushes an attached instrumented client, if one was supplied.
   *
   * The core client's `flush()` takes no signal, so an abort is honored before the
   * flush starts rather than interrupting it.
   */
  async flushGenerations(options: RequestOptions = {}): Promise<void> {
    throwIfAborted(options.signal);
    await this.coreClient?.flush();
  }

  // --- artifacts ----------------------------------------------------------- //

  /** Uploads artifact bytes and attaches them to a trial. */
  async uploadArtifact(request: UploadArtifactRequest, options: RequestOptions = {}): Promise<Record<string, unknown>> {
    if ((request.parentKind ?? 'test_case_trial') !== 'test_case_trial') {
      throw validationError('only test_case_trial artifacts are supported by the experiments ingest client');
    }
    const runId = requireField(request.experimentId, 'experiment_id');
    const trialId = requireField(request.parentId, 'trial_id');
    const name = requireField(request.name, 'name');
    const kind = requireField(request.kind, 'kind');
    let content = request.content;
    if (content.byteLength === 0) {
      throw validationError('content is required');
    }
    const mime = (request.mime ?? '').trim();
    if (this.redactSecrets && (['json', 'markdown', 'text', 'csv'].includes(kind) || mime.startsWith('text/'))) {
      content = new TextEncoder().encode(redactSecretText(new TextDecoder().decode(content)));
    }
    const body = await this.request(
      {
        method: 'POST',
        path: routes.trialArtifactUploadPath(runId, trialId),
        query: { name, kind, mime },
        bytes: content,
        contentType: mime.length > 0 ? mime : 'application/octet-stream',
        label: 'trial artifact upload',
      },
      options,
    );
    return isRecord(body) ? body : {};
  }

  // --- reads --------------------------------------------------------------- //

  /** Fetches the aggregated report for a run. */
  async getReport(experimentId: string, options: RequestOptions = {}): Promise<ExperimentReport> {
    const runId = requireField(experimentId, 'run_id');
    const body = await this.request(
      { method: 'GET', path: routes.experimentReportPath(runId), label: 'experiment report' },
      options,
    );
    return parseExperimentReport(body);
  }

  /** Lists stored scores for a run. */
  async listScores(
    experimentId: string,
    listOptions: { limit?: number; cursor?: string } = {},
    options: RequestOptions = {},
  ): Promise<{ items: Record<string, unknown>[]; nextCursor?: string }> {
    const runId = requireField(experimentId, 'run_id');
    const limit = listOptions.limit !== undefined && listOptions.limit > 0 ? listOptions.limit : 50;
    const query: Record<string, string> = { limit: String(limit) };
    if (listOptions.cursor !== undefined && listOptions.cursor.trim().length > 0) {
      query.cursor = listOptions.cursor.trim();
    }
    const body = await this.request(
      { method: 'GET', path: routes.experimentScoresPath(runId), query, label: 'experiment scores list' },
      options,
    );
    const items = isRecord(body) && Array.isArray(body.items) ? body.items.filter(isRecord) : [];
    const nextCursor = normalizeCursor(isRecord(body) ? body.next_cursor : undefined);
    return nextCursor !== undefined ? { items, nextCursor } : { items };
  }

  /** Best-effort deep link to the run in the Agent Observability UI. */
  experimentUrl(experimentId: string): string {
    const quoted = encodeURIComponent(experimentId.trim());
    const base =
      this.grafanaUrl.length > 0
        ? this.grafanaUrl
        : baseURLFromAPIEndpoint(this.endpoint, this.connection.insecure, 'agento11y experiments');
    return `${base}/a/grafana-agento11y-app/experiments/runs/${quoted}`;
  }

  /** Flushes an attached instrumented client. Kept for symmetry with the core client. */
  async shutdown(): Promise<void> {
    await this.flushGenerations();
  }

  private async request(
    request: Omit<Parameters<typeof requestExperimentsJSON>[1], 'signal'>,
    options: RequestOptions,
  ): Promise<unknown> {
    return requestExperimentsJSON(this.connection, {
      ...request,
      ...(options.signal !== undefined ? { signal: options.signal } : {}),
    });
  }

  private buildHeaders(ingestToken: string): Record<string, string> {
    const headers =
      this.tenantId.length > 0
        ? resolveHeadersWithAuth(
            undefined,
            {
              mode: 'basic',
              tenantId: this.tenantId,
              basicUser: this.tenantId,
              basicPassword: ingestToken,
            },
            'agento11y experiments',
          )
        : resolveHeadersWithAuth(undefined, { mode: 'bearer', bearerToken: ingestToken }, 'agento11y experiments');
    const out: Record<string, string> = { ...(headers ?? {}) };
    if (this.actor.length > 0) {
      out[INGEST_ACTOR_HEADER] = this.actor;
    }
    return out;
  }

  private redactScore(score: ScoreItem): ScoreItem {
    const out: ScoreItem = { ...score, value: { ...score.value } };
    if (out.value.string !== undefined) {
      out.value.string = redactSecretText(out.value.string);
    }
    if (out.explanation !== undefined) {
      out.explanation = redactSecretText(out.explanation);
    }
    if (out.metadata !== undefined) {
      out.metadata = redactSecretValue(out.metadata);
    }
    return out;
  }

  /** Reports a non-fatal condition through the configured logger. */
  warn(message: string): void {
    this.logger?.warn?.(message);
  }
}

function assertGenerationAccepted(body: unknown): void {
  const results = isRecord(body) && Array.isArray(body.results) ? body.results : [];
  const first = results[0];
  if (!isRecord(first)) {
    throw transportError('generation export: response did not include a result');
  }
  if (first.accepted === true) {
    return;
  }
  const detail = str(first.error).length > 0 ? str(first.error) : 'rejected';
  const normalized = detail.trim().toLowerCase();
  // An idempotent retry of the same anchor generation is a success, not a loss.
  if (normalized.includes('generation already exists') || normalized.includes('duplicate entry')) {
    return;
  }
  throw transportError(`generation export rejected: ${detail}`);
}

function normalizeUsage(usage: TokenUsage): Record<string, number> | undefined {
  const inputTokens = Math.max(0, Math.trunc(usage.inputTokens ?? 0));
  const outputTokens = Math.max(0, Math.trunc(usage.outputTokens ?? 0));
  const cacheReadInputTokens = Math.max(0, Math.trunc(usage.cacheReadInputTokens ?? 0));
  const cacheWriteInputTokens = Math.max(0, Math.trunc(usage.cacheWriteInputTokens ?? 0));
  const reasoningTokens = Math.max(0, Math.trunc(usage.reasoningTokens ?? 0));
  const totalTokens = Math.max(0, Math.trunc(usage.totalTokens ?? 0)) || inputTokens + outputTokens;
  if (totalTokens === 0) {
    return undefined;
  }
  return {
    input_tokens: inputTokens,
    output_tokens: outputTokens,
    total_tokens: totalTokens,
    cache_read_input_tokens: cacheReadInputTokens,
    cache_write_input_tokens: cacheWriteInputTokens,
    reasoning_tokens: reasoningTokens,
  };
}

function requireField(value: string, name: string): string {
  const normalized = value.trim();
  if (normalized.length === 0) {
    throw validationError(name.includes(' ') ? name : `${name} is required`);
  }
  return normalized;
}

function requireTrialEvaluationField(value: string, name: string): string {
  const normalized = value.trim();
  if (normalized.length === 0) {
    throw new Error(`agento11y trial evaluation validation failed: ${name} is required`);
  }
  return normalized;
}

function defaultMonotonicNow(): number {
  return typeof performance !== 'undefined' && typeof performance.now === 'function' ? performance.now() : Date.now();
}
