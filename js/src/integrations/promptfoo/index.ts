import {
  type Candidate,
  ExperimentsClient,
  type ExperimentsClientOptions,
  ReportRole,
  stableId,
  type TestCase,
  type TestSuite,
  type TestSuitesClient,
  withExperiment,
} from '../../experiments/index.js';

/** The stable subset of Promptfoo's provider response used by this adapter. */
export interface PromptfooProviderResponse {
  output?: unknown;
  metadata?: Record<string, unknown>;
}

/** The stable subset of Promptfoo's completed row used by this adapter. */
export interface PromptfooResult {
  id?: string;
  description?: string;
  promptIdx: number;
  testIdx: number;
  testCase?: {
    description?: string;
    vars?: Record<string, unknown>;
    metadata?: Record<string, unknown>;
    assert?: unknown[];
  };
  prompt?: { raw?: string; label?: string };
  provider?: { id?: string; label?: string; [key: string]: unknown };
  vars?: Record<string, unknown>;
  response?: PromptfooProviderResponse;
  error?: string | null;
  /** Native ResultFailureReason: 0 NONE, 1 ASSERT, 2 ERROR. */
  failureReason?: number;
  success: boolean;
  score: number;
  latencyMs?: number;
  gradingResult?: {
    reason?: string;
    comment?: string;
  } | null;
  namedScores?: Record<string, number>;
  metadata?: Record<string, unknown>;
  tokenUsage?: {
    prompt?: number;
    completion?: number;
  };
  cost?: number;
  traceId?: string;
}

/** Promptfoo's public `afterAll` extension hook context. */
export interface PromptfooAfterAllContext {
  suite: unknown;
  results: PromptfooResult[];
  prompts: unknown[];
  evalId: string;
  config: Record<string, unknown>;
}

export interface PromptfooPublishOptions {
  /** Additional candidate provenance applied to each published run. */
  candidate?: Candidate;
  client?: ExperimentsClient;
  clientOptions?: ExperimentsClientOptions;
  experimentId?: string;
  experimentName?: string;
  suiteId?: string;
  suiteVersion?: string;
  /** Explicitly publish browsable cases and pin all candidates to the returned version. */
  testSuitesClient?: Pick<TestSuitesClient, 'pushSuite'>;
  /** Required with `testSuitesClient`: retain remote-only cases or replace them. */
  suitePublicationPolicy?: 'subset' | 'replace';
  /** Score key for Promptfoo's aggregate row score. Defaults to `promptfoo`. */
  primaryScoreKey?: string;
  /** Records Promptfoo's request and response as an anchor generation. Defaults to true. */
  recordIO?: boolean;
  agentName?: string;
  agentVersion?: string;
  metadata?: Record<string, unknown>;
  resolveConversationId?: (result: PromptfooResult) => string | undefined;
  resolveGenerationId?: (result: PromptfooResult) => string | undefined;
}

export interface PublishedPromptfooRun {
  experimentId: string;
  url: string;
  trialCount: number;
  scoreCount: number;
}

export interface PublishedPromptfooEvaluation {
  runs: PublishedPromptfooRun[];
  trialCount: number;
  scoreCount: number;
}

/**
 * Publishes a completed Promptfoo evaluation as one Agent Observability
 * evaluation. Each prompt/provider candidate becomes one experiment run, and
 * each result row becomes one trial. The aggregate row score is the primary
 * verdict and named scores are diagnostics.
 */
export async function publishPromptfooResults(
  context: PromptfooAfterAllContext,
  options: PromptfooPublishOptions = {},
): Promise<PublishedPromptfooEvaluation> {
  if (context.results.length === 0) {
    throw new Error('Promptfoo produced no results to publish');
  }
  validatePromptfooCaseDefinitions(context.results);
  const client = options.client ?? new ExperimentsClient(options.clientOptions);
  const baseExperimentId = nonBlank(options.experimentId) ?? nonBlank(context.evalId) ?? stableId('exp', 'promptfoo');
  const baseExperimentName = nonBlank(options.experimentName) ?? `Promptfoo ${baseExperimentId}`;
  const primaryScoreKey = nonBlank(options.primaryScoreKey) ?? 'promptfoo';
  const groups = promptfooCandidateGroups(context.results);
  let canonicalCases: Map<string, TestCase> | undefined;
  if (options.testSuitesClient) {
    if (options.suitePublicationPolicy === undefined) {
      throw new Error('suitePublicationPolicy is required when publishing a Promptfoo suite');
    }
    // Candidate prompts belong to runs; suite inputs retain the native test vars.
    const cases = new Map<string, TestCase>();
    for (const { testCase } of promptfooCases(context.results)) {
      const input = testCase.input as { vars: Record<string, unknown> };
      const definition: TestCase = {
        testCaseId: testCase.testCaseId,
        name: testCase.name,
        input: { vars: input.vars },
        expected: testCase.expected,
        metadata: { framework: 'promptfoo' },
      };
      const previous = cases.get(definition.testCaseId);
      if (previous && stableJSON(previous) !== stableJSON(definition)) {
        throw new Error(`Conflicting Promptfoo definitions for case ${definition.testCaseId}`);
      }
      cases.set(definition.testCaseId, definition);
    }
    const pushed = await options.testSuitesClient.pushSuite(
      {
        suiteId: nonBlank(options.suiteId) ?? 'promptfoo',
        name: 'Promptfoo evaluation',
        tags: ['framework:promptfoo'],
        testCases: [...cases.values()],
      },
      { publish: true, prune: options.suitePublicationPolicy === 'replace' },
    );
    canonicalCases = new Map(
      (pushed.suite?.testCases ?? [...cases.values()]).map((testCase) => [testCase.testCaseId, testCase]),
    );
    options = { ...options, suiteVersion: pushed.suiteVersion };
  }
  const configId = stableId('promptfoo-config', stableJSON(context.config));
  const runs: PublishedPromptfooRun[] = [];
  for (const [groupIndex, group] of groups.entries()) {
    const first = group[0] as PromptfooResult;
    const suffix = `${first.promptIdx + 1}-${stableId('candidate', promptfooCandidateIdentity(first)).slice(-16)}`;
    const experimentId = groups.length === 1 ? baseExperimentId : `${baseExperimentId}-${suffix}`;
    const variant = [first.prompt?.label, first.provider?.label ?? first.provider?.id].filter(nonBlank).join(' / ');
    const experimentName =
      groups.length === 1 ? baseExperimentName : `${baseExperimentName} [${variant || groupIndex + 1}]`;
    runs.push(
      await publishPromptfooCandidate(group, client, {
        ...options,
        experimentId,
        experimentName,
        primaryScoreKey,
        evalId: context.evalId,
        configId,
        canonicalCases,
      }),
    );
  }
  return {
    runs,
    trialCount: runs.reduce((total, run) => total + run.trialCount, 0),
    scoreCount: runs.reduce((total, run) => total + run.scoreCount, 0),
  };
}

/** Creates a Promptfoo `afterAll` extension hook. */
export function createPromptfooExtension(
  options: PromptfooPublishOptions = {},
): (context: PromptfooAfterAllContext) => Promise<PromptfooAfterAllContext> {
  return async (context) => {
    const published = await publishPromptfooResults(context, options);
    for (const run of published.runs) {
      process.stdout.write(`Published Promptfoo experiment: ${run.url || run.experimentId}\n`);
    }
    return context;
  };
}

async function publishPromptfooCandidate(
  results: PromptfooResult[],
  client: ExperimentsClient,
  options: PromptfooPublishOptions & {
    experimentId: string;
    experimentName: string;
    primaryScoreKey: string;
    evalId: string;
    configId: string;
    canonicalCases?: Map<string, TestCase>;
  },
): Promise<PublishedPromptfooRun> {
  const cases = promptfooCases(results, options.canonicalCases);
  const suite: TestSuite = {
    suiteId: nonBlank(options.suiteId) ?? 'promptfoo',
    version: nonBlank(options.suiteVersion) ?? '1',
    name: 'Promptfoo evaluation',
    testCases: cases.map(({ testCase }) => testCase),
  };
  const model = promptfooModel(results[0] as PromptfooResult, options.candidate);
  let scoreCount = 0;
  let experimentURL = '';
  await withExperiment(
    client,
    {
      experimentId: options.experimentId,
      name: options.experimentName,
      suite,
      plannedTrialCount: cases.length,
      candidate: {
        promptVersion: results[0]?.prompt?.label ?? String(results[0]?.promptIdx ?? ''),
        modelProvider: model.provider,
        modelName: model.name,
        ...(options.candidate ?? {}),
      },
      metadata: {
        framework: 'promptfoo',
        promptfoo_eval_id: options.evalId,
        promptfoo_config_id: options.configId,
        promptfoo_candidate_id: stableId('candidate', promptfooCandidateIdentity(results[0] as PromptfooResult)),
        suite_publication_policy: options.suitePublicationPolicy ?? 'local',
        executed_case_count: new Set(cases.map(({ testCase }) => testCase.testCaseId)).size,
        ...(options.metadata ?? {}),
      },
    },
    async (experiment) => {
      for (const mapped of cases) {
        await experiment.withTrial(
          mapped.testCase,
          async (trial) => {
            const result = mapped.result;
            const model = promptfooModel(result, options.candidate);
            const conversationId =
              options.resolveConversationId?.(result) ?? metadataString(result, 'agento11y.conversation_id');
            const generationId =
              options.resolveGenerationId?.(result) ?? metadataString(result, 'agento11y.generation_id');
            if (generationId !== undefined) {
              trial.bindGeneration(generationId, { ...(conversationId !== undefined ? { conversationId } : {}) });
            } else if (options.recordIO ?? true) {
              if (conversationId !== undefined) trial.bindConversation(conversationId);
              trial.recordIO({
                input: promptfooIOInput(result),
                output: promptfooIOOutput(result.response?.output),
                modelProvider: model.provider,
                modelName: model.name,
                agentName: options.agentName ?? 'promptfoo-target',
                agentVersion: options.agentVersion ?? '',
                inputTokens: finiteInteger(result.tokenUsage?.prompt),
                outputTokens: finiteInteger(result.tokenUsage?.completion),
              });
            } else if (conversationId !== undefined) {
              trial.bindConversation(conversationId);
            }
            if (result.traceId !== undefined && result.traceId.length > 0) await trial.bindTrace(result.traceId);
            trial.setUsage({
              inputTokens: finiteInteger(result.tokenUsage?.prompt),
              outputTokens: finiteInteger(result.tokenUsage?.completion),
              cost: finiteNumber(result.cost),
            });
            trial.setDuration(nonNegativeNumber(result.latencyMs) ?? null);
            const nativeError = nonBlank(result.error ?? undefined);
            if (result.failureReason === 2 || (nativeError !== undefined && result.failureReason !== 1)) {
              trial.markErrored(new Error(nativeError ?? 'Promptfoo provider error'));
              return;
            }
            trial.score(options.primaryScoreKey, finiteNumber(result.score) ?? result.success, {
              evaluator: { evaluatorId: 'promptfoo', version: options.configId, kind: 'custom' },
              passed: result.success,
              reportRole: ReportRole.PrimaryVerdict,
              explanation: result.gradingResult?.reason ?? result.error ?? '',
              metadata: promptfooMetadata(result),
            });
            scoreCount += 1;
            for (const [name, value] of Object.entries(result.namedScores ?? {})) {
              if (!Number.isFinite(value) || name === options.primaryScoreKey) continue;
              trial.score(name, value, {
                evaluator: { evaluatorId: `promptfoo.${name}`, version: options.configId, kind: 'custom' },
                reportRole: ReportRole.Diagnostic,
                metadata: promptfooMetadata(result),
              });
              scoreCount += 1;
            }
          },
          { attempt: mapped.attempt, metadata: mapped.provenance },
        );
      }
      experimentURL = experiment.url;
    },
  );
  return {
    experimentId: options.experimentId,
    url: experimentURL,
    trialCount: cases.length,
    scoreCount,
  };
}

function promptfooCandidateGroups(results: PromptfooResult[]): PromptfooResult[][] {
  const groups = new Map<string, PromptfooResult[]>();
  for (const result of results) {
    const key = promptfooCandidateIdentity(result);
    const group = groups.get(key) ?? [];
    group.push(result);
    groups.set(key, group);
  }
  return [...groups.values()];
}

function promptfooCases(
  results: PromptfooResult[],
  canonicalCases?: Map<string, TestCase>,
): { result: PromptfooResult; testCase: TestCase; attempt: number; provenance: Record<string, unknown> }[] {
  const attempts = new Map<string, number>();
  return results.map((result) => {
    const explicitId = metadataString(result, 'agento11y.test_case_id');
    const executedCase = {
      // Prompts belong to candidates; vars and assertions define the shared
      // case that must align across prompt variants.
      input: { vars: result.vars ?? result.testCase?.vars ?? {} },
      expected:
        result.testCase?.assert !== undefined
          ? { assertions: result.testCase.assert }
          : result.testCase?.metadata?.expected,
    };
    const testCaseId = explicitId ?? stableId('promptfoo-case', stableJSON(executedCase));
    const attempt = (attempts.get(testCaseId) ?? 0) + 1;
    attempts.set(testCaseId, attempt);
    const executed: TestCase = {
      testCaseId,
      name: result.description ?? result.testCase?.description ?? testCaseId,
      input: executedCase.input,
      expected: executedCase.expected,
      metadata: promptfooMetadata(result),
    };
    const testCase = canonicalCases?.get(testCaseId) ?? executed;
    return {
      result,
      attempt,
      testCase,
      provenance: caseProvenance(executed, testCase),
    };
  });
}

function validatePromptfooCaseDefinitions(results: PromptfooResult[]): void {
  const definitions = new Map<string, string>();
  for (const result of results) {
    const explicitId = metadataString(result, 'agento11y.test_case_id');
    if (explicitId === undefined) continue;
    const digest = stableJSON({
      input: { vars: result.vars ?? result.testCase?.vars ?? {} },
      expected:
        result.testCase?.assert !== undefined
          ? { assertions: result.testCase.assert }
          : result.testCase?.metadata?.expected,
    });
    const previous = definitions.get(explicitId);
    if (previous !== undefined && previous !== digest) {
      throw new Error(`Conflicting Promptfoo definitions for case ${explicitId}`);
    }
    definitions.set(explicitId, digest);
  }
}

function promptfooInput(result: PromptfooResult): unknown {
  return {
    prompt: result.prompt?.raw ?? '',
    vars: result.vars ?? result.testCase?.vars ?? {},
  };
}

function promptfooIOInput(result: PromptfooResult): string {
  return JSON.stringify(promptfooInput(result));
}

function promptfooIOOutput(output: unknown): string | undefined {
  if (output === undefined) return undefined;
  return typeof output === 'string' ? output : JSON.stringify(output);
}

function promptfooMetadata(result: PromptfooResult): Record<string, unknown> {
  const metadata: Record<string, unknown> = {
    framework: 'promptfoo',
    promptfoo_result_id: result.id ?? '',
    promptfoo_test_index: result.testIdx,
    promptfoo_prompt_index: result.promptIdx,
    promptfoo_provider_id: result.provider?.id ?? '',
    promptfoo_provider_label: result.provider?.label ?? '',
    promptfoo_prompt_label: result.prompt?.label ?? '',
    ...(result.testCase?.metadata ?? {}),
    ...(result.metadata ?? {}),
  };
  const latencyMs = nonNegativeNumber(result.latencyMs);
  if (latencyMs !== undefined) metadata.promptfoo_latency_ms = latencyMs;
  return metadata;
}

function promptfooModel(result: PromptfooResult, candidate?: Candidate): { provider: string; name: string } {
  return {
    provider: nonBlank(candidate?.modelProvider) ?? metadataString(result, 'agento11y.model_provider') ?? '',
    name: nonBlank(candidate?.modelName) ?? metadataString(result, 'agento11y.model_name') ?? '',
  };
}

function promptfooCandidateIdentity(result: PromptfooResult): string {
  return stableJSON({
    prompt_idx: result.promptIdx,
    provider: result.provider ?? {},
  });
}

function caseProvenance(executed: TestCase, stored: TestCase): Record<string, unknown> {
  const executedDigest = stableId('case-content', stableJSON({ input: executed.input, expected: executed.expected }));
  const storedDigest = stableId('case-content', stableJSON({ input: stored.input, expected: stored.expected }));
  return {
    agento11y_executed_case_digest: executedDigest,
    agento11y_stored_case_digest: storedDigest,
    agento11y_case_transformed: executedDigest !== storedDigest,
  };
}

function stableJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(',')}]`;
  if (value !== null && typeof value === 'object') {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([key, item]) => `${JSON.stringify(key)}:${stableJSON(item)}`)
      .join(',')}}`;
  }
  return JSON.stringify(value) ?? 'null';
}

function metadataString(result: PromptfooResult, key: string): string | undefined {
  const value = result.metadata?.[key] ?? result.testCase?.metadata?.[key] ?? result.response?.metadata?.[key];
  return nonBlank(typeof value === 'string' ? value : undefined);
}

function nonBlank(value: string | undefined): string | undefined {
  const normalized = value?.trim();
  return normalized !== undefined && normalized.length > 0 ? normalized : undefined;
}

function finiteNumber(value: number | undefined): number | undefined {
  return value !== undefined && Number.isFinite(value) ? value : undefined;
}

function nonNegativeNumber(value: number | undefined): number | undefined {
  const finite = finiteNumber(value);
  return finite !== undefined && finite >= 0 ? finite : undefined;
}

function finiteInteger(value: number | undefined): number | undefined {
  const finite = finiteNumber(value);
  return finite !== undefined ? Math.trunc(finite) : undefined;
}
