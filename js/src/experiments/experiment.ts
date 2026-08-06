import {
  isSpanContextValid,
  context as otelContext,
  type Span,
  SpanKind,
  SpanStatusCode,
  trace,
} from '@opentelemetry/api';
import { redactSecretText } from '../redaction.js';
import { asError } from '../utils.js';
import type { ExperimentsClient } from './client.js';
import { TrialEvaluationFailedError, TrialEvaluationTimeoutError, validationError } from './errors.js';
import type { EvaluationResult, OutputEvaluator } from './evaluators.js';
import { FEATURE_CLOUD_TRIAL_EVALUATION, requireExperimental } from './experimental.js';
import { stableId } from './ids.js';
import type {
  Candidate,
  Evaluator,
  ExperimentReport,
  ExperimentStatus,
  ScoreItem,
  ScoreValue,
  TrialEvaluation,
  TrialStatus,
} from './models.js';
import { normalizeEvaluatorKind } from './models.js';
import * as otel from './otel.js';
import { throwIfAborted } from './transport.js';
import type { TestCase, TestSuite, TrialRef } from './types.js';
import { candidateMetadata, suiteCase } from './types.js';

/** Ceiling for `Trial.evaluate`'s poll backoff. Each status read costs an ingest call. */
const MAX_EVALUATION_POLL_INTERVAL_MS = 5_000;
const DEFAULT_EVALUATION_TIMEOUT_MS = 300_000;
const DEFAULT_EVALUATION_POLL_INTERVAL_MS = 500;

const DEFAULT_EVALUATOR: Evaluator = { evaluatorId: 'sdk', version: '0' };

export interface ExperimentOptions {
  experimentId?: string;
  name?: string;
  suite?: TestSuite;
  candidate?: Candidate;
  defaultEvaluator?: Evaluator;
  description?: string;
  tags?: string[];
  metadata?: Record<string, unknown>;
  plannedTrialCount?: number;
  useExperimentalOtel?: boolean;
}

export interface TrialOptions {
  attempt?: number;
  trajectoryId?: string;
  metadata?: Record<string, unknown>;
}

export interface FinalizeOptions {
  error?: string;
  /**
   * Asserts how many scores the run stored. Leave it unset unless a distributed
   * runner knows the total: Agent Observability checks it against every score
   * stored for the run, including ones a stored evaluator wrote through
   * `Trial.evaluate`. `finalize` drops the count once a trial of this experiment
   * queued an evaluation in this process; a runner that evaluates elsewhere has to
   * leave it unset itself.
   */
  scoreCount?: number;
}

export interface EvaluateOptions {
  evaluatorVersion?: string;
  /** Bounds the polling loop, not the whole call. Defaults to 300000. */
  timeoutMs?: number;
  /** First poll delay, doubling to a 5000 ms ceiling. Defaults to 500. */
  pollIntervalMs?: number;
  /** Aborts the wait; the signal's own reason is rethrown unchanged. */
  signal?: AbortSignal;
}

export interface RecordIOOptions {
  input?: unknown;
  output?: unknown;
  modelProvider?: string;
  modelName?: string;
  agentName?: string;
  agentVersion?: string;
  inputTokens?: number;
  outputTokens?: number;
}

export interface ScoreOptions {
  evaluator?: Evaluator;
  passed?: boolean;
  explanation?: string;
  generationId?: string;
  graderConversationId?: string;
  graderGenerationId?: string;
  graderTraceId?: string;
  metadata?: Record<string, unknown>;
  /** Internal: reuses an id already minted by `recordEvaluation`. */
  scoreId?: string;
}

export interface ArtifactOptions {
  data?: unknown;
  text?: string;
  bytes?: Uint8Array;
  kind?: string;
  mime?: string;
}

/**
 * One attempt at one test case: records scores and, when experimental OTel is on,
 * emits a trial span with `gen_ai.evaluation.result` events.
 *
 * Scores buffer locally and export on `flush()` or `close()`. Open a trial with
 * `experiment.trial(...)` then `await trial.start()`, or let
 * `experiment.withTrial(...)` do both and close it on either path.
 */
export class Trial {
  readonly ref: TrialRef;
  readonly trialId: string;

  status: TrialStatus = 'running';
  conversationId = '';
  traceId = '';
  spanId = '';
  error = '';
  readonly artifacts: { name: string; kind: string; artifactId: string }[] = [];

  private readonly client: ExperimentsClient;
  private readonly experiment: Experiment | undefined;
  private readonly testCase: TestCase | undefined;
  private readonly candidate: Candidate | undefined;
  private readonly defaultEvaluator: Evaluator;
  private readonly metadata: Record<string, unknown>;
  private readonly useExperimentalOtel: boolean;

  /** Derived at construction, replaced by `bindGeneration`. Read through `generationId`. */
  private anchorGenerationId: string;
  private buffer: ScoreItem[] = [];
  private accepted = 0;
  private hasFinal = false;
  private finalPassed: boolean | undefined;
  private cloudEvaluated = false;
  private closed = false;
  private trialCreated = false;
  private generationExported = false;
  private hasGeneration = false;
  private io: Record<string, unknown> = {};
  private usage: { inputTokens?: number; outputTokens?: number; cost?: number } = {};
  private startedAtMs: number | undefined;
  private scoreOccurrences = new Map<string, number>();
  private span: Span | undefined;

  constructor(
    client: ExperimentsClient,
    ref: TrialRef,
    options: {
      experiment?: Experiment;
      testCase?: TestCase;
      candidate?: Candidate;
      defaultEvaluator?: Evaluator;
      metadata?: Record<string, unknown>;
      useExperimentalOtel?: boolean;
    } = {},
  ) {
    this.client = client;
    this.ref = ref;
    this.experiment = options.experiment;
    this.testCase = options.testCase;
    this.candidate = options.candidate;
    this.defaultEvaluator = options.defaultEvaluator ?? DEFAULT_EVALUATOR;
    this.metadata = { ...(options.metadata ?? {}) };
    this.useExperimentalOtel = options.useExperimentalOtel ?? client.useExperimentalOtel;
    this.trialId = stableId('trial', ref.experimentId, ref.testCaseId, ref.attempt);
    // Scores attach to the typed trial; a generation is optional, exported only
    // when the harness binds one or supplies I/O through recordIO.
    this.anchorGenerationId = stableId('gen', ref.experimentId, ref.testCaseId, ref.attempt);
  }

  /** The generation this trial's scores attach to: derived, or the bound one. */
  get generationId(): string {
    return this.anchorGenerationId;
  }

  /** Opens a standalone trial bound to `client`, with no parent experiment. */
  static fromRef(
    client: ExperimentsClient,
    ref: TrialRef | undefined,
    options: {
      candidate?: Candidate;
      defaultEvaluator?: Evaluator;
      useExperimentalOtel?: boolean;
    } = {},
  ): Trial {
    if (ref === undefined) {
      throw new Error('trial ref is required; set AGENTO11Y_EXPERIMENT_ID and AGENTO11Y_TEST_CASE_ID');
    }
    return new Trial(client, ref, options);
  }

  /** Creates the typed trial server-side and starts the optional trial span. */
  async start(): Promise<Trial> {
    if (this.closed) {
      throw new Error('cannot start a closed trial');
    }
    this.startedAtMs = this.client.nowMs();
    this.startSpan();
    await this.createTrial();
    return this;
  }

  /**
   * Runs `fn` with this trial's span active, so generations an instrumented agent
   * emits inside it join the trial's trace.
   *
   * `withTrial` wraps the callback in this. A caller driving `start()` and
   * `close()` by hand wraps its own agent call. Without the trial span (the
   * default, since spans are experimental) this only calls `fn`.
   */
  async runInTrialContext<T>(fn: () => Promise<T> | T): Promise<T> {
    const span = this.span;
    if (span === undefined) {
      return fn();
    }
    return otelContext.with(trace.setSpan(otelContext.active(), span), fn);
  }

  // --- binding ------------------------------------------------------------- //

  /** Links this trial's scores to a specific conversation. */
  bindConversation(conversationId: string): Trial {
    this.conversationId = conversationId.trim();
    return this;
  }

  /** Links this trial's scores to a trace produced elsewhere. */
  async bindTrace(traceId: string, spanId = ''): Promise<Trial> {
    this.traceId = traceId.trim();
    this.spanId = spanId.trim();
    if (this.trialCreated) {
      await this.client.updateTrial(this.ref.experimentId, this.trialId, {
        traceId: this.traceId,
        spanId: this.spanId,
      });
    }
    return this;
  }

  /**
   * Attaches this trial's scores to a generation the harness already exported.
   * The trial then records no anchor generation of its own.
   */
  bindGeneration(generationId: string, options: { conversationId?: string } = {}): Trial {
    const id = generationId.trim();
    if (id.length > 0) {
      // The bound id replaces the derived one for every score this trial writes,
      // and the harness already exported it, so this trial exports nothing.
      this.anchorGenerationId = id;
      this.generationExported = true;
      this.hasGeneration = true;
    }
    if (options.conversationId !== undefined && options.conversationId.length > 0) {
      this.conversationId = options.conversationId.trim();
    }
    return this;
  }

  /**
   * Records the attempt's input and output for the anchor generation.
   *
   * Stored now and exported as one generation when scores flush, so the attempt's
   * conversation is visible in Agent Observability and the scores attach to it.
   */
  recordIO(options: RecordIOOptions): Trial {
    this.hasGeneration = true;
    // A real generation will back this trial; mint a conversation id if unbound.
    if (this.conversationId.length === 0) {
      this.conversationId = stableId('conv', this.ref.experimentId, this.ref.testCaseId, this.ref.attempt);
    }
    if (options.input !== undefined) {
      this.io.inputText = typeof options.input === 'string' ? options.input : String(options.input);
    }
    if (options.output !== undefined) {
      this.io.outputText = typeof options.output === 'string' ? options.output : String(options.output);
    }
    for (const [key, value] of [
      ['modelProvider', options.modelProvider],
      ['modelName', options.modelName],
      ['agentName', options.agentName],
      ['agentVersion', options.agentVersion],
    ] as const) {
      if (value !== undefined && value.length > 0) {
        this.io[key] = value;
      }
    }
    if (options.inputTokens !== undefined) {
      this.io.inputTokens = Math.trunc(options.inputTokens);
    }
    if (options.outputTokens !== undefined) {
      this.io.outputTokens = Math.trunc(options.outputTokens);
    }
    return this;
  }

  /**
   * Records token usage and an optional cost override.
   *
   * Tokens go on the span as the standard `gen_ai.usage.*` signal. `cost` is an
   * optional USD override: when omitted, Agent Observability derives cost at
   * ingestion from token usage and model-card pricing. Pass `cost` only when a
   * framework computes it itself; leave it unset rather than passing `0`.
   */
  setUsage(options: { inputTokens?: number; outputTokens?: number; cost?: number }): Trial {
    if (options.inputTokens !== undefined) {
      this.usage.inputTokens = Math.trunc(options.inputTokens);
      this.span?.setAttribute('gen_ai.usage.input_tokens', Math.trunc(options.inputTokens));
    }
    if (options.outputTokens !== undefined) {
      this.usage.outputTokens = Math.trunc(options.outputTokens);
      this.span?.setAttribute('gen_ai.usage.output_tokens', Math.trunc(options.outputTokens));
    }
    if (options.cost !== undefined) {
      this.usage.cost = options.cost;
    }
    return this;
  }

  // --- scoring ------------------------------------------------------------- //

  /** Records a score for this trial. The general primitive. */
  score(scoreKey: string, value: ScoreValue | number | boolean | string, options: ScoreOptions = {}): ScoreItem {
    const evaluator = options.evaluator ?? this.defaultEvaluator;
    const scoreValue = coerceValue(value);
    let passed = options.passed;
    if (scoreKey === 'final' && passed === undefined) {
      passed = inferFinalPassed(scoreValue);
    }
    const scoreId =
      options.scoreId !== undefined && options.scoreId.length > 0
        ? options.scoreId
        : this.nextScoreId(scoreKey, evaluator.evaluatorId);
    const item: ScoreItem = {
      scoreId,
      evaluatorId: evaluator.evaluatorId,
      evaluatorVersion: evaluator.version ?? '0',
      evaluatorKind: normalizeEvaluatorKind(evaluator.kind ?? 'custom'),
      scoreKey,
      value: scoreValue,
      // generation_id only when one exists; trial_id is what attributes the score.
      generationId: options.generationId ?? (this.hasGeneration ? this.generationId : ''),
      trialId: this.trialId,
      conversationId: this.conversationId,
      traceId: this.traceId,
      spanId: this.spanId,
      experimentId: this.ref.experimentId,
      testCaseId: this.ref.testCaseId,
      graderConversationId: options.graderConversationId ?? '',
      graderGenerationId: options.graderGenerationId ?? '',
      graderTraceId: options.graderTraceId ?? '',
      ...(passed !== undefined ? { passed } : {}),
      explanation: options.explanation ?? '',
      metadata: {
        task_id: this.ref.testCaseId,
        trial_id: this.trialId,
        attempt: this.ref.attempt,
        ...this.metadata,
        ...(options.metadata ?? {}),
      },
      source: { kind: 'experiment', id: this.ref.experimentId },
    };
    this.buffer.push(item);
    this.emitScoreEvent(scoreKey, scoreValue, evaluator, passed, options.explanation ?? '', options.generationId ?? '');
    if (scoreKey === 'final') {
      this.hasFinal = true;
      this.finalPassed = passed;
    }
    return item;
  }

  /**
   * The headline score and trial verdict (`scoreKey: "final"`).
   *
   * `passed` is the verdict the report rollup uses. When omitted and `value` is
   * boolean, the boolean is the verdict.
   */
  finalScore(value: ScoreValue | number | boolean | string, options: Omit<ScoreOptions, 'scoreId'> = {}): ScoreItem {
    // `score` infers the verdict for the `final` key, so the rule lives there only.
    return this.score('final', value, options);
  }

  /** A deterministic check, for example `json_valid` or `tool_used`. */
  checkScore(
    name: string,
    options: { passed: boolean; value?: ScoreValue | number | boolean | string } & Omit<
      ScoreOptions,
      'passed' | 'scoreId'
    >,
  ): ScoreItem {
    const evaluator =
      options.evaluator ??
      ({
        evaluatorId: `${this.defaultEvaluator.evaluatorId}.${name}`,
        version: this.defaultEvaluator.version ?? '0',
        kind: 'deterministic',
      } satisfies Evaluator);
    return this.score(name, options.value ?? options.passed, { ...options, evaluator });
  }

  /** An LLM-judge rubric criterion score. */
  rubricScore(
    name: string,
    value: ScoreValue | number | boolean | string,
    options: Omit<ScoreOptions, 'scoreId'> = {},
  ): ScoreItem {
    const evaluator =
      options.evaluator ??
      ({
        evaluatorId: `${this.defaultEvaluator.evaluatorId}.${name}`,
        version: this.defaultEvaluator.version ?? '0',
        kind: 'llm_judge',
      } satisfies Evaluator);
    return this.score(name, value, { ...options, evaluator });
  }

  /**
   * Records an evaluation produced by a runner, framework, or helper.
   *
   * When the result carries a grader generation, it is published and linked to the
   * score. Agent Observability does not fetch or normalize the evaluated
   * conversation; callers bind its existing trace and conversation identifiers.
   */
  async recordEvaluation(
    result: EvaluationResult,
    options: { scoreKey?: string; publishGrader?: boolean; metadata?: Record<string, unknown> } = {},
  ): Promise<ScoreItem> {
    const scoreKey = options.scoreKey ?? result.scoreKey ?? 'final';
    const scoreId = this.nextScoreId(scoreKey, result.evaluator.evaluatorId);
    let graderGenerationId = '';
    let graderConversationId = '';
    if (result.grader !== undefined && (options.publishGrader ?? true)) {
      graderGenerationId = stableId('gen', scoreId, 'grader');
      graderConversationId = stableId('conv', scoreId, 'grader');
      const grader = result.grader;
      await this.client.exportGeneration({
        generationId: graderGenerationId,
        conversationId: graderConversationId,
        inputText: grader.input,
        outputText: grader.output,
        modelProvider: grader.modelProvider,
        modelName: grader.modelName,
        agentName: grader.agentName ?? 'agento11y-llm-judge',
        agentVersion: grader.agentVersion ?? '',
        operationName: grader.operationName ?? 'evaluate',
        ...(grader.usage !== undefined ? { usage: grader.usage } : {}),
        tags: {
          'experiment.run_id': this.ref.experimentId,
          'test.case.id': this.ref.testCaseId,
          'test.case.attempt': String(this.ref.attempt),
          'evaluator.id': result.evaluator.evaluatorId,
        },
        metadata: {
          experiment_run_id: this.ref.experimentId,
          test_case_id: this.ref.testCaseId,
          attempt: this.ref.attempt,
          ...(result.metadata ?? {}),
        },
      });
    }
    return this.score(scoreKey, result.value, {
      evaluator: result.evaluator,
      passed: result.passed,
      explanation: result.explanation ?? '',
      graderConversationId,
      graderGenerationId,
      metadata: { ...(result.metadata ?? {}), ...(options.metadata ?? {}) },
      scoreId,
    });
  }

  /**
   * Grades caller-supplied output with a local judge and records the result.
   *
   * This does not retrieve the trial's bound conversation. Use a framework-native
   * evaluator with `recordEvaluation` for trajectory or transcript evaluation.
   */
  async evaluateOutput(
    judge: OutputEvaluator,
    options: {
      input: unknown;
      output: unknown;
      expected?: unknown;
      scoreKey?: string;
      publishGrader?: boolean;
      metadata?: Record<string, unknown>;
    },
  ): Promise<ScoreItem> {
    const result = await judge.evaluateOutput({
      input: options.input,
      output: options.output,
      expected: options.expected,
    });
    return this.recordEvaluation(result, {
      ...(options.scoreKey !== undefined ? { scoreKey: options.scoreKey } : {}),
      ...(options.publishGrader !== undefined ? { publishGrader: options.publishGrader } : {}),
      ...(options.metadata !== undefined ? { metadata: options.metadata } : {}),
    });
  }

  // --- cloud evaluation ---------------------------------------------------- //

  /**
   * Runs a stored evaluator against this trial's bound conversation.
   *
   * It grades the conversation Agent Observability already stored, using an
   * evaluator defined in your tenant. For an in-process judge, see
   * `evaluateOutput`.
   *
   * The conversation binding is persisted and the anchor generation from
   * `recordIO` is exported before the evaluation is queued, so the evaluator can
   * read the conversation it is asked to grade. A successful evaluation lets the
   * trial close as `completed` without a local `finalScore`. Queuing one also makes
   * `Experiment.finalize` drop a caller-supplied `scoreCount`, since the evaluator
   * writes a score this process never sees.
   *
   * The evaluator grades the conversation, not one generation, so the stored score
   * carries this trial's `conversationId` and `trialId` and no `generationId`. The
   * report's `passRate` stays unset, because that verdict comes from a score under
   * the `final` key and a stored evaluator writes under its own key.
   *
   * `timeoutMs` bounds the polling loop, not the whole call: a status request
   * already in flight spends its own retry budget first. Worker failure rejects
   * with `TrialEvaluationFailedError`, an exceeded deadline with
   * `TrialEvaluationTimeoutError`, and an aborted `signal` with its own reason. A
   * transport error while polling propagates and abandons the wait; the evaluation
   * keeps running server-side.
   *
   * Experimental: requires `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`,
   * otherwise it rejects with `ExperimentalFeatureDisabledError` without sending a
   * request.
   */
  async evaluate(evaluatorId: string, options: EvaluateOptions = {}): Promise<TrialEvaluation> {
    // Checked before the trial is created and the anchor generation is flushed, so
    // a blocked call leaves nothing behind.
    requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION);

    const normalizedEvaluatorId = evaluatorId.trim();
    if (normalizedEvaluatorId.length === 0) {
      throw new Error('agento11y trial evaluation validation failed: evaluator_id is required');
    }
    if (this.conversationId.length === 0) {
      throw new Error('agento11y trial evaluation validation failed: bind a conversation first');
    }
    const timeoutMs = options.timeoutMs ?? DEFAULT_EVALUATION_TIMEOUT_MS;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new Error('agento11y trial evaluation validation failed: timeoutMs must be greater than zero');
    }
    const pollIntervalMs = options.pollIntervalMs ?? DEFAULT_EVALUATION_POLL_INTERVAL_MS;
    if (!Number.isFinite(pollIntervalMs) || pollIntervalMs <= 0) {
      throw new Error('agento11y trial evaluation validation failed: pollIntervalMs must be greater than zero');
    }
    const signal = options.signal;
    throwIfAborted(signal);

    // Every call here takes the caller's signal, so an abort stops the sequence at
    // the current request instead of spending its retry budget first.
    const signalOption = { ...(signal !== undefined ? { signal } : {}) };
    await this.createTrial(signalOption);
    // bindConversation and recordIO are local until now, and the backend rejects
    // an evaluation for a trial with no stored conversation.
    await this.client.updateTrial(
      this.ref.experimentId,
      this.trialId,
      { conversationId: this.conversationId },
      signalOption,
    );
    // The evaluator reads the stored conversation, so the anchor generation has to
    // exist before the wait starts, not when the trial closes.
    await this.ensureGeneration(signalOption);
    await this.client.flushGenerations(signalOption);

    const deadline = this.client.nowMs() + timeoutMs;
    let evaluation = await this.client.triggerTrialEvaluation(
      this.ref.experimentId,
      this.trialId,
      normalizedEvaluatorId,
      options.evaluatorVersion ?? '',
      signalOption,
    );
    // The evaluation row exists from here on, and the score it writes counts toward
    // the experiment's stored total whether or not this wait sees it finish.
    this.experiment?.markCloudEvaluated();

    // Back off so a long wait costs tens of status reads, not hundreds. A caller
    // who asked for a slower cadence than the cap keeps it.
    let interval = pollIntervalMs;
    const maxInterval = Math.max(pollIntervalMs, MAX_EVALUATION_POLL_INTERVAL_MS);
    for (;;) {
      if (evaluation.status === 'success') {
        // The trial's terminal state stays with close(): a cloud score is not a
        // local verdict, and setting status here would swallow an error the trial
        // callback throws afterwards.
        this.cloudEvaluated = true;
        return evaluation;
      }
      if (evaluation.status === 'failed') {
        throw new TrialEvaluationFailedError(evaluation.evaluationId, evaluation.error);
      }
      const remaining = deadline - this.client.nowMs();
      if (remaining <= 0) {
        throw new TrialEvaluationTimeoutError(evaluation.evaluationId, `waited ${timeoutMs}ms`);
      }
      await this.client.sleepFn(Math.min(interval, remaining), signal);
      interval = Math.min(interval * 2, maxInterval);
      // Every sleep is followed by a status read, including the one clamped to the
      // remaining budget, so an evaluation that finishes in the last window is not
      // reported as a timeout.
      evaluation = await this.client.getTrialEvaluation(
        this.ref.experimentId,
        this.trialId,
        evaluation.evaluationId,
        signalOption,
      );
    }
  }

  // --- artifacts ----------------------------------------------------------- //

  /**
   * Attaches an artifact to this trial: a JSON object, text, or raw bytes.
   *
   * Exactly one of `data`, `text`, or `bytes` is required; passing two is rejected.
   * `kind` and `mime` are inferred when omitted. Returns the created artifact
   * record.
   */
  async artifact(name: string, options: ArtifactOptions = {}): Promise<Record<string, unknown>> {
    const resolved = resolveArtifactContent(options);
    const record = await this.client.uploadArtifact({
      experimentId: this.ref.experimentId,
      parentId: this.trialId,
      name,
      kind: resolved.kind,
      content: resolved.content,
      mime: resolved.mime,
    });
    const artifactId = typeof record.artifact_id === 'string' ? record.artifact_id : '';
    this.artifacts.push({ name, kind: resolved.kind, artifactId });
    this.span?.addEvent(otel.ARTIFACT_EVENT, {
      [otel.ARTIFACT_NAME]: name,
      [otel.ARTIFACT_KIND]: resolved.kind,
    });
    return record;
  }

  // --- export and close ---------------------------------------------------- //

  /**
   * Exports buffered scores and returns the number freshly accepted.
   *
   * Anchors the trial's generation first, when one is not externally bound, so the
   * scores' `generationId` resolves under strict score ingest. An empty buffer
   * sends no score request.
   */
  async flush(): Promise<number> {
    await this.ensureGeneration();
    await this.client.flushGenerations();
    await this.createTrial();
    if (this.buffer.length === 0) {
      return 0;
    }
    const pending = this.buffer;
    this.buffer = [];
    let accepted: number;
    try {
      accepted = await this.client.exportScores(pending);
    } catch (error) {
      // Keep the scores buffered so a retry or close can publish them.
      this.buffer = [...pending, ...this.buffer];
      throw error;
    }
    this.accepted += accepted;
    this.experiment?.recordAccepted(accepted);
    return accepted;
  }

  /**
   * Flushes and terminalizes this trial.
   *
   * The close status follows the same state machine as Go and Python: a callback
   * error closes `errored`; no final score and no cloud evaluation closes `failed`
   * with `trial closed without a final score`; a cloud-evaluated trial closes
   * `completed`; a final score closes `passed` or `failed` on its verdict, or
   * `completed` when the verdict is unknown.
   */
  async close(options: { error?: unknown } = {}): Promise<void> {
    if (this.closed) {
      return;
    }
    const callbackError = options.error;
    if (callbackError !== undefined && this.status === 'running') {
      this.status = 'errored';
      this.error = asError(callbackError).message || 'error';
    } else if (this.status === 'running') {
      if (!this.hasFinal) {
        if (this.cloudEvaluated) {
          // A stored evaluator graded this trial; the verdict and score count come
          // from the backend, not from a local final score.
          this.status = 'completed';
        } else {
          this.status = 'failed';
          this.error = 'trial closed without a final score';
        }
      } else if (this.finalPassed === undefined) {
        this.status = 'completed';
      } else {
        this.status = this.finalPassed ? 'passed' : 'failed';
      }
    }
    let flushError: unknown;
    try {
      await this.flush();
    } catch (error) {
      flushError = error;
    }
    try {
      await this.finalizeTrial();
    } catch (error) {
      this.endSpan();
      if (flushError !== undefined) {
        // The finalize error is what the caller sees, but a failed score export is
        // the more actionable half, so it travels along instead of vanishing.
        // Python gets the same pairing from the exception's __context__.
        reportSecondaryError(this.client, 'trial close: score export failed', error, flushError);
      }
      throw error;
    }
    this.endSpan();
    this.closed = true;
    this.experiment?.trialClosed(this);
    if (flushError !== undefined) {
      throw flushError;
    }
  }

  get acceptedScores(): number {
    return this.accepted;
  }

  get isClosed(): boolean {
    return this.closed;
  }

  /** Whether a stored evaluator graded this trial successfully. */
  get isCloudEvaluated(): boolean {
    return this.cloudEvaluated;
  }

  /** The buffered, not yet exported scores. Test and diagnostic use. */
  get pendingScores(): readonly ScoreItem[] {
    return this.buffer;
  }

  // --- internals ----------------------------------------------------------- //

  private async createTrial(options: { signal?: AbortSignal } = {}): Promise<void> {
    if (this.trialCreated) {
      return;
    }
    const snapshot = this.testCaseSnapshot();
    await this.client.upsertTrial(
      this.ref.experimentId,
      {
        trialId: this.trialId,
        testCaseId: this.ref.testCaseId,
        attempt: this.ref.attempt,
        status: 'running',
        conversationId: this.conversationId,
        traceId: this.traceId,
        spanId: this.spanId,
        ...(snapshot !== undefined ? { testCase: snapshot } : {}),
        ...(this.ref.testCaseName !== undefined && this.ref.testCaseName.length > 0
          ? { metadata: { test_case_name: this.ref.testCaseName } }
          : {}),
      },
      { ...(options.signal !== undefined ? { signal: options.signal } : {}) },
    );
    this.trialCreated = true;
  }

  private async finalizeTrial(): Promise<void> {
    if (!this.trialCreated) {
      return;
    }
    // Trial status is the lifecycle (completed/failed); the pass/fail verdict lives
    // in the final score's `passed`, which drives the report pass rate.
    const backendStatus = this.status === 'errored' ? 'failed' : 'completed';
    await this.client.updateTrial(this.ref.experimentId, this.trialId, {
      status: backendStatus,
      error: this.error,
      ...(this.usage.cost !== undefined ? { cost: this.usage.cost } : {}),
      ...(this.usage.inputTokens !== undefined ? { inputTokens: this.usage.inputTokens } : {}),
      ...(this.usage.outputTokens !== undefined ? { outputTokens: this.usage.outputTokens } : {}),
      ...(this.durationMs() !== undefined ? { durationMs: this.durationMs() } : {}),
      conversationId: this.conversationId,
      traceId: this.traceId,
      spanId: this.spanId,
    });
  }

  private durationMs(): number | undefined {
    if (this.startedAtMs === undefined) {
      return undefined;
    }
    return Math.max(0, Math.trunc(this.client.nowMs() - this.startedAtMs));
  }

  /**
   * Exports the anchor generation when `recordIO` supplied content.
   *
   * Generations are optional: the typed trial already attributes scores. One is
   * exported only when the harness gave input or output, so the attempt's
   * conversation is visible in Agent Observability.
   */
  private async ensureGeneration(options: { signal?: AbortSignal } = {}): Promise<void> {
    if (this.generationExported || Object.keys(this.io).length === 0) {
      return;
    }
    let caseInput = '';
    const suiteTestCase = suiteCase(this.experiment?.suite, this.ref.testCaseId);
    if (suiteTestCase?.input !== undefined && suiteTestCase.input !== null) {
      caseInput = typeof suiteTestCase.input === 'string' ? suiteTestCase.input : String(suiteTestCase.input);
    }
    await this.client.exportGeneration(
      {
        generationId: this.generationId,
        conversationId: this.conversationId,
        inputText: stringField(this.io.inputText) ?? caseInput,
        outputText: stringField(this.io.outputText) ?? '',
        modelProvider: stringField(this.io.modelProvider) ?? this.candidate?.modelProvider ?? 'eval',
        modelName: stringField(this.io.modelName) ?? this.candidate?.modelName ?? 'experiment',
        agentName: stringField(this.io.agentName) ?? this.candidate?.agentName ?? '',
        agentVersion: stringField(this.io.agentVersion) ?? this.candidate?.agentVersion ?? '',
        ...(typeof this.io.inputTokens === 'number' ? { inputTokens: this.io.inputTokens } : {}),
        ...(typeof this.io.outputTokens === 'number' ? { outputTokens: this.io.outputTokens } : {}),
        tags: { 'experiment.run_id': this.ref.experimentId, task_id: this.ref.testCaseId },
        metadata: {
          experiment_run_id: this.ref.experimentId,
          task_id: this.ref.testCaseId,
          trial_id: this.trialId,
          attempt: this.ref.attempt,
        },
      },
      { ...(options.signal !== undefined ? { signal: options.signal } : {}) },
    );
    this.generationExported = true;
  }

  private nextScoreId(scoreKey: string, evaluatorId: string): string {
    const identity = `${scoreKey}\u001f${evaluatorId}`;
    const occurrence = this.scoreOccurrences.get(identity) ?? 0;
    this.scoreOccurrences.set(identity, occurrence + 1);
    const parts: unknown[] = [this.ref.experimentId, this.trialId, scoreKey, evaluatorId];
    if (occurrence > 0) {
      parts.push(occurrence + 1);
    }
    return stableId('score', ...parts);
  }

  private startSpan(): void {
    // Experimental OTel is opt-in. Without it the trial is still created and scores
    // still record; only spans and events are skipped.
    if (!this.useExperimentalOtel) {
      return;
    }
    const tracer = trace.getTracer(otel.INSTRUMENTATION_NAME);
    const span = tracer.startSpan(`${otel.TRIAL_SPAN_NAME} ${this.ref.testCaseId}`, {
      kind: SpanKind.INTERNAL,
      attributes: this.identityAttributes(),
    });
    this.span = span;
    const spanContext = span.spanContext();
    // A trace id is 32 hex characters even when it is all zeros, which is what a
    // non-recording span returns when no TracerProvider is registered. Storing it
    // would point every trial and score at a trace that does not exist.
    if (isSpanContextValid(spanContext)) {
      this.traceId = spanContext.traceId;
      this.spanId = spanContext.spanId;
    }
  }

  private endSpan(): void {
    const span = this.span;
    if (span === undefined) {
      return;
    }
    span.setAttributes(this.identityAttributes());
    if (this.error.length > 0) {
      const safeError = this.telemetryText(this.error);
      span.setAttribute(otel.EVAL_EXPLANATION, safeError);
      span.setStatus({ code: SpanStatusCode.ERROR, message: safeError });
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }
    span.end();
    this.span = undefined;
  }

  private identityAttributes(): Record<string, string | number> {
    const attrs = otel.trialIdentityAttributes({
      experimentId: this.ref.experimentId,
      experimentName: this.experiment?.name ?? '',
      suiteId: this.ref.suiteId ?? '',
      suiteVersion: this.ref.suiteVersion ?? '',
      suiteName: this.ref.suiteName ?? '',
      testCaseId: this.ref.testCaseId,
      testCaseName: this.ref.testCaseName ?? '',
      attempt: this.ref.attempt,
      trialId: this.trialId,
      runStatus: this.experiment?.status ?? 'running',
      trialStatus: this.status,
      conversationId: this.conversationId,
      operationName: 'invoke_agent',
    }) as Record<string, string | number>;
    if (this.candidate !== undefined) {
      for (const [key, value] of [
        ['gen_ai.agent.name', this.candidate.agentName],
        ['gen_ai.agent.version', this.candidate.agentVersion],
        ['gen_ai.provider.name', this.candidate.modelProvider],
        ['gen_ai.request.model', this.candidate.modelName],
      ] as const) {
        if (value !== undefined && value.length > 0) {
          attrs[key] = value;
        }
      }
    }
    return attrs;
  }

  private emitScoreEvent(
    scoreKey: string,
    value: ScoreValue,
    evaluator: Evaluator,
    passed: boolean | undefined,
    explanation: string,
    responseId: string,
  ): void {
    if (this.span === undefined) {
      return;
    }
    this.span.addEvent(
      otel.EVENT_EVAL_RESULT,
      otel.scoreEventAttributes({
        name: scoreKey,
        value: eventValue(value),
        ...(passed !== undefined ? { passed } : {}),
        explanation: this.telemetryText(explanation),
        evaluatorId: evaluator.evaluatorId,
        evaluatorVersion: evaluator.version ?? '0',
        evaluatorKind: normalizeEvaluatorKind(evaluator.kind ?? 'custom'),
        referenceSetId: evaluator.referenceSetId ?? '',
        referenceSetVersion: evaluator.referenceSetVersion ?? '',
        responseId,
      }),
    );
  }

  private telemetryText(value: string): string {
    return this.client.redactSecrets ? redactSecretText(value) : value;
  }

  private testCaseSnapshot(): Record<string, unknown> | undefined {
    const testCase = this.testCase;
    if (testCase === undefined) {
      return undefined;
    }
    const artifactRefs = (testCase.artifactRefs ?? []).filter(
      (ref) => ref.artifact_id !== undefined && ref.name !== undefined && ref.kind !== undefined,
    );
    return {
      test_case_id: testCase.testCaseId,
      suite_id: this.ref.suiteId ?? '',
      suite_version: this.ref.suiteVersion ?? '',
      name: testCase.name ?? '',
      description: testCase.description ?? '',
      tags: [...(testCase.tags ?? [])],
      category: testCase.category ?? '',
      input: objectValue(testCase.input),
      expected: objectValue(testCase.expected),
      metadata: { ...(testCase.metadata ?? {}) },
      artifact_refs: artifactRefs.map((ref) => ({ ...ref })),
    };
  }
}

/**
 * An external experiment run: upserted by `start`, finalized by `finalize`.
 *
 * `withExperiment` wraps both so the run is finalized on the success and the
 * failure path, matching Go's `WithExperiment`.
 */
export class Experiment {
  readonly experimentId: string;
  readonly name: string;
  readonly suite: TestSuite | undefined;
  status: string = 'running';

  private readonly client: ExperimentsClient;
  private readonly candidate: Candidate | undefined;
  private readonly defaultEvaluator: Evaluator | undefined;
  private readonly description: string;
  private readonly tags: string[];
  private readonly metadata: Record<string, unknown>;
  private readonly plannedTrialCount: number | undefined;
  private readonly useExperimentalOtel: boolean;

  private accepted = 0;
  private finalized = false;
  private cloudEvaluated = false;
  private readonly openTrials = new Map<string, Trial>();
  private readonly claimedTrialIds = new Set<string>();

  private constructor(client: ExperimentsClient, options: ExperimentOptions) {
    if (options.plannedTrialCount !== undefined && options.plannedTrialCount < 0) {
      throw new Error('plannedTrialCount must be non-negative');
    }
    this.client = client;
    this.experimentId =
      options.experimentId !== undefined && options.experimentId.length > 0
        ? options.experimentId
        : stableId('exp', options.name ?? '', randomSuffix());
    this.name = options.name !== undefined && options.name.length > 0 ? options.name : this.experimentId;
    this.suite = options.suite;
    this.candidate = options.candidate;
    this.defaultEvaluator = options.defaultEvaluator;
    this.description = options.description ?? '';
    this.tags = [...(options.tags ?? [])];
    this.metadata = { ...(options.metadata ?? {}) };
    this.plannedTrialCount = options.plannedTrialCount;
    this.useExperimentalOtel = options.useExperimentalOtel ?? client.useExperimentalOtel;
  }

  /** Upserts the run and returns the open experiment. */
  static async start(client: ExperimentsClient, options: ExperimentOptions = {}): Promise<Experiment> {
    const experiment = new Experiment(client, options);
    const metadata: Record<string, unknown> = { ...experiment.metadata };
    let suiteId = '';
    let suiteVersion = '';
    if (experiment.suite !== undefined) {
      suiteId = experiment.suite.suiteId;
      suiteVersion = experiment.suite.version ?? '';
      metadata.suite_id ??= suiteId;
      metadata.suite_version ??= suiteVersion;
    }
    const candidate = candidateMetadata(experiment.candidate);
    Object.assign(metadata, candidate);
    await client.upsertExperiment({
      runId: experiment.experimentId,
      name: experiment.name,
      description: experiment.description,
      tags: experiment.tags,
      suiteId,
      suiteVersion,
      candidate,
      ...(experiment.plannedTrialCount !== undefined ? { plannedTrialCount: experiment.plannedTrialCount } : {}),
      metadata,
    });
    return experiment;
  }

  /**
   * Claims one unique case attempt.
   *
   * Reusing the same case and attempt in one experiment is rejected: both map to
   * the same durable trial and score identities. Increment `attempt` for a retry.
   * The returned trial is not created server-side until `start()`.
   */
  trial(testCase: TestCase | string, options: TrialOptions = {}): Trial {
    const attempt = options.attempt !== undefined && options.attempt > 0 ? Math.trunc(options.attempt) : 1;
    const resolved = typeof testCase === 'string' ? suiteCase(this.suite, testCase) : testCase;
    const testCaseId = (typeof testCase === 'string' ? testCase : testCase.testCaseId).trim();
    if (testCaseId.length === 0) {
      throw new Error('test case id is required');
    }
    const testCaseName = resolved?.name !== undefined && resolved.name.length > 0 ? resolved.name : testCaseId;
    const ref: TrialRef = {
      experimentId: this.experimentId,
      testCaseId,
      attempt,
      suiteId: this.suite?.suiteId ?? '',
      suiteVersion: this.suite?.version ?? '',
      suiteName: this.suite?.name !== undefined && this.suite.name.length > 0 ? this.suite.name : this.name,
      testCaseName,
      trajectoryId: options.trajectoryId ?? '',
    };
    const trial = new Trial(this.client, ref, {
      experiment: this,
      ...(resolved !== undefined ? { testCase: resolved } : {}),
      ...(this.candidate !== undefined ? { candidate: this.candidate } : {}),
      ...(this.defaultEvaluator !== undefined ? { defaultEvaluator: this.defaultEvaluator } : {}),
      ...(options.metadata !== undefined ? { metadata: options.metadata } : {}),
      useExperimentalOtel: this.useExperimentalOtel,
    });
    // Read the id off the trial: one derivation, so the dedupe set cannot drift
    // from the id the trial actually writes.
    if (this.claimedTrialIds.has(trial.trialId)) {
      throw new Error(
        `trial for test case "${testCaseId}" attempt ${attempt} already exists; increment attempt for a retry`,
      );
    }
    this.claimedTrialIds.add(trial.trialId);
    this.openTrials.set(trial.trialId, trial);
    return trial;
  }

  /**
   * Opens a trial, runs `fn`, and closes the trial on both paths.
   *
   * A callback error terminalizes the trial as `errored` and is rethrown after the
   * close completes.
   */
  async withTrial<T>(
    testCase: TestCase | string,
    fn: (trial: Trial) => Promise<T> | T,
    options: TrialOptions = {},
  ): Promise<T> {
    const trial = this.trial(testCase, options);
    try {
      await trial.start();
    } catch (error) {
      this.trialAbandoned(trial);
      throw error;
    }
    let result: T;
    try {
      result = await trial.runInTrialContext(() => fn(trial));
    } catch (error) {
      // The close still runs, so the trial is terminalized before the error leaves.
      try {
        await trial.close({ error });
      } catch (closeError) {
        reportSecondaryError(this.client, `trial ${trial.trialId} close failed`, error, closeError);
      }
      throw error;
    }
    await trial.close();
    return result;
  }

  /**
   * Finalizes the run. Safe to call twice; the second call is a no-op.
   *
   * Open trials are closed first. A close failure forces `failed`, drops
   * `scoreCount`, and appends the failure to `error`.
   */
  async finalize(status: ExperimentStatus | string = 'completed', options: FinalizeOptions = {}): Promise<void> {
    if (this.finalized) {
      return;
    }
    let statusValue = String(status);
    let scoreCount = options.scoreCount;
    let error = options.error ?? '';
    const closeErrors: Error[] = [];
    for (const trial of [...this.openTrials.values()]) {
      try {
        await trial.close();
      } catch (closeError) {
        closeErrors.push(asError(closeError));
      }
    }
    if (this.cloudEvaluated) {
      // A stored evaluator wrote a score this process never saw, so a locally
      // derived count would fail the server's check with a 409.
      scoreCount = undefined;
    }
    if (closeErrors.length > 0) {
      statusValue = 'failed';
      scoreCount = undefined;
      const summary = closeErrors.map((closeError) => closeError.message || closeError.name).join('; ');
      error = [error, `trial close failed: ${summary}`].filter((part) => part.length > 0).join('; ');
    }
    this.status = statusValue;
    await this.client.finalize(this.experimentId, statusValue, {
      ...(scoreCount !== undefined ? { scoreCount } : {}),
      ...(error.length > 0 ? { error } : {}),
    });
    this.finalized = true;
    if (closeErrors.length > 0) {
      const primary = closeErrors[0] as Error;
      throw primary;
    }
  }

  /** Fetches the aggregated report for this run. */
  async report(): Promise<ExperimentReport> {
    return this.client.getReport(this.experimentId);
  }

  /** Best-effort deep link to the run in the Agent Observability UI. */
  get url(): string {
    return this.client.experimentUrl(this.experimentId);
  }

  get acceptedScores(): number {
    return this.accepted;
  }

  get isFinalized(): boolean {
    return this.finalized;
  }

  /** Whether a stored evaluator was queued for one of this run's trials. */
  get hasCloudEvaluatedTrial(): boolean {
    return this.cloudEvaluated;
  }

  /** @internal Called by `Trial`; not part of the public API. */
  markCloudEvaluated(): void {
    this.cloudEvaluated = true;
  }

  /** @internal Called by `Trial`; not part of the public API. */
  trialClosed(trial: Trial): void {
    this.openTrials.delete(trial.trialId);
  }

  private trialAbandoned(trial: Trial): void {
    this.openTrials.delete(trial.trialId);
    this.claimedTrialIds.delete(trial.trialId);
  }

  /** @internal Called by `Trial`; not part of the public API. */
  recordAccepted(count: number): void {
    this.accepted += count;
  }
}

/**
 * Opens an experiment, runs `fn`, and finalizes the run on both paths.
 *
 * A callback error finalizes the run as `failed` and is rethrown; a finalization
 * failure is attached as the cause so the original error still surfaces.
 */
export async function withExperiment<T>(
  client: ExperimentsClient,
  options: ExperimentOptions,
  fn: (experiment: Experiment) => Promise<T> | T,
): Promise<T> {
  const experiment = await Experiment.start(client, options);
  let result: T;
  try {
    result = await fn(experiment);
  } catch (error) {
    try {
      await experiment.finalize('failed', { error: asError(error).message || 'error' });
    } catch (finalizeError) {
      reportSecondaryError(client, 'experiment finalize failed', error, finalizeError);
    }
    throw error;
  }
  await experiment.finalize('completed');
  return result;
}

function coerceValue(value: ScoreValue | number | boolean | string): ScoreValue {
  if (typeof value === 'boolean') {
    return { boolean: value };
  }
  if (typeof value === 'number') {
    return { number: value };
  }
  if (typeof value === 'string') {
    return { string: value };
  }
  return { ...value };
}

function inferFinalPassed(value: ScoreValue): boolean | undefined {
  return value.boolean;
}

function eventValue(value: ScoreValue): number | boolean | string | undefined {
  if (value.number !== undefined) {
    return value.number;
  }
  if (value.boolean !== undefined) {
    return value.boolean;
  }
  return value.string;
}

/** Maps a MIME type onto an Agent Observability artifact kind. */
export function artifactKindFromMime(mime: string): string {
  const m = mime.toLowerCase();
  if (m.startsWith('image/')) {
    return 'image';
  }
  if (m === 'application/json') {
    return 'json';
  }
  if (m === 'text/markdown' || m === 'text/x-markdown') {
    return 'markdown';
  }
  if (m === 'application/pdf') {
    return 'pdf';
  }
  if (m === 'text/csv') {
    return 'csv';
  }
  if (m.startsWith('text/')) {
    return 'text';
  }
  return 'binary';
}

function resolveArtifactContent(options: ArtifactOptions): { content: Uint8Array; kind: string; mime: string } {
  const encoder = new TextEncoder();
  const supplied = [options.data, options.text, options.bytes].filter((value) => value !== undefined).length;
  if (supplied > 1) {
    // Silently preferring one body would upload content the caller did not mean
    // to send, so two bodies is an error rather than a precedence rule.
    throw validationError('artifact takes exactly one of data, text, or bytes');
  }
  if (options.bytes !== undefined) {
    const mime = options.mime !== undefined && options.mime.length > 0 ? options.mime : 'application/octet-stream';
    return {
      content: options.bytes,
      kind: options.kind !== undefined && options.kind.length > 0 ? options.kind : artifactKindFromMime(mime),
      mime,
    };
  }
  if (options.data !== undefined) {
    return {
      content: encoder.encode(JSON.stringify(options.data)),
      kind: options.kind !== undefined && options.kind.length > 0 ? options.kind : 'json',
      mime: options.mime !== undefined && options.mime.length > 0 ? options.mime : 'application/json',
    };
  }
  if (options.text !== undefined && options.text.length > 0) {
    return {
      content: encoder.encode(options.text),
      kind: options.kind !== undefined && options.kind.length > 0 ? options.kind : 'text',
      mime: options.mime !== undefined && options.mime.length > 0 ? options.mime : 'text/plain',
    };
  }
  throw validationError('artifact requires one of data, text, or bytes');
}

function objectValue(value: unknown): Record<string, unknown> {
  if (value === undefined || value === null) {
    return {};
  }
  if (typeof value === 'object' && !Array.isArray(value)) {
    return { ...(value as Record<string, unknown>) };
  }
  return { value };
}

function stringField(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

/**
 * Random suffix for a generated run id.
 *
 * `crypto.getRandomValues` is assumed present: the next statement in the caller
 * hashes with SHA-1, which throws in any runtime without crypto, and experiments
 * is explicitly outside the edge-runtime guarantee.
 */
function randomSuffix(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(8));
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

/**
 * Reports a cleanup failure that happened while another error was already leaving.
 *
 * The first error stays the thrown value, so a caller's `instanceof` check still
 * works. The cleanup error is logged, and it is attached to the first free `cause`
 * slot on the primary error's chain, so a caller reading `error.cause` finds it.
 * Go joins the pair with `errors.Join`; Python attaches a note. None of the three
 * drops the second error, which is usually a failed score export.
 */
function reportSecondaryError(client: ExperimentsClient, label: string, primary: unknown, secondary: unknown): void {
  const secondaryError = asError(secondary);
  client.warn(`agento11y: ${label}: ${secondaryError.message}`);
  if (!(primary instanceof Error)) {
    return;
  }
  let current = primary as Error & { cause?: unknown };
  const seen = new Set<unknown>([current]);
  while (current.cause instanceof Error && !seen.has(current.cause)) {
    seen.add(current.cause);
    current = current.cause as Error & { cause?: unknown };
  }
  if (current.cause === undefined) {
    current.cause = secondaryError;
  }
}
