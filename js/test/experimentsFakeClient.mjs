/**
 * Recording stand-in for `ExperimentsClient`, mirroring `FakeClient` in
 * python/tests/test_experiments.py so both suites assert the same call order.
 *
 * `evaluationOrder` records only the calls `trial.evaluate` makes, so a test can
 * pin the ordering without filtering the full call log.
 */
export class FakeExperimentsClient {
  constructor(options = {}) {
    this.useExperimentalOtel = options.useExperimentalOtel ?? false;
    this.redactSecrets = options.redactSecrets ?? false;
    this.upserts = [];
    this.scores = [];
    this.finalized = [];
    this.generations = [];
    this.generationCalls = [];
    this.trials = [];
    this.trialUpdates = [];
    this.artifacts = [];
    this.calls = [];
    this.warnings = [];
    this.evaluationOrder = [];
    this.triggeredEvaluations = [];
    this.evaluationResult = evaluation('success');
    this.evaluationStatuses = [];
    this.exportScoresError = options.exportScoresError;
    this.exportGenerationError = options.exportGenerationError;
    this.updateTrialError = options.updateTrialError;
    this.triggerError = options.triggerError;

    // Fake clock: every sleep advances it, so a deadline is reached without
    // spending wall-clock time.
    this.clockMs = 0;
    this.sleeps = [];
    this.sleepFn = async (durationMs, signal) => {
      if (signal?.aborted === true) {
        throw signal.reason;
      }
      this.sleeps.push(durationMs);
      this.clockMs += durationMs;
      if (options.onSleep !== undefined) {
        await options.onSleep(durationMs, this);
      }
      if (signal?.aborted === true) {
        throw signal.reason;
      }
    };
    this.nowMs = () => this.clockMs;
  }

  async upsertExperiment(request) {
    this.calls.push('upsert_experiment');
    this.upserts.push(request);
    return { runId: request.runId ?? '', name: request.name, status: 'running' };
  }

  async exportScores(scores) {
    this.calls.push('export_scores');
    if (this.exportScoresError !== undefined) {
      throw this.exportScoresError;
    }
    this.scores.push(...scores);
    return scores.length;
  }

  async exportGeneration(request) {
    this.calls.push('export_generation');
    this.evaluationOrder.push('generation');
    if (this.exportGenerationError !== undefined) {
      throw this.exportGenerationError;
    }
    this.generations.push(request.generationId);
    this.generationCalls.push(request);
    return request.generationId;
  }

  async recordGeneration(request) {
    return this.exportGeneration(request);
  }

  async flushGenerations() {
    this.calls.push('flush_generations');
    this.evaluationOrder.push('flush');
  }

  async upsertTrial(experimentId, request) {
    this.calls.push('upsert_trial');
    this.trials.push({ experimentId, ...request });
    return { trial_id: request.trialId };
  }

  async updateTrial(experimentId, trialId, request) {
    this.calls.push('update_trial');
    this.evaluationOrder.push('update');
    if (this.updateTrialError !== undefined) {
      throw this.updateTrialError;
    }
    this.trialUpdates.push({ experimentId, trialId, ...request });
    return { trial_id: trialId };
  }

  async triggerTrialEvaluation(experimentId, trialId, evaluatorId, evaluatorVersion = '') {
    this.calls.push('trigger_trial_evaluation');
    this.evaluationOrder.push('trigger');
    this.triggeredEvaluations.push({ experimentId, trialId, evaluatorId, evaluatorVersion });
    if (this.triggerError !== undefined) {
      throw this.triggerError;
    }
    return this.evaluationResult;
  }

  async getTrialEvaluation() {
    this.calls.push('get_trial_evaluation');
    this.evaluationOrder.push('status');
    if (this.evaluationStatuses.length > 0) {
      return this.evaluationStatuses.shift();
    }
    return this.evaluationResult;
  }

  async uploadArtifact(request) {
    this.calls.push('upload_artifact');
    this.artifacts.push(request);
    return { artifact_id: `art_${request.name}`, name: request.name, kind: request.kind };
  }

  async finalize(experimentId, status = 'completed', options = {}) {
    this.calls.push('finalize');
    this.finalized.push({ experimentId, status, ...options });
    return { runId: experimentId, status };
  }

  async getReport(experimentId) {
    this.calls.push('get_report');
    return { run: { runId: experimentId }, summary: {}, rows: [] };
  }

  experimentUrl(experimentId) {
    return `http://ui/${experimentId}`;
  }

  warn(message) {
    this.warnings.push(message);
  }
}

export function evaluation(status, overrides = {}) {
  return {
    evaluationId: 'eval-123',
    experimentId: 'run-1',
    trialId: 'trial-1',
    testCaseId: 'case-1',
    conversationId: 'conv-1',
    evaluatorId: 'helpfulness',
    evaluatorVersion: '1',
    status,
    attempts: 1,
    error: '',
    ...overrides,
  };
}
