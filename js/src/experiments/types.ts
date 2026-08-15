import type { EnvPair } from '../config.js';
import { brandedPair, defaultEnv, envTrimmed } from '../config.js';
import type { Agento11yLogger } from '../types.js';
import type { Candidate, Evaluator, EvaluatorKind } from './models.js';
import { isRecord, normalizeEvaluatorKind } from './models.js';
import { putNonBlank } from './serialize.js';

export type { Candidate, Evaluator, EvaluatorKind };
export { normalizeEvaluatorKind };

/** One test case (scenario) in a suite. */
export interface TestCase {
  testCaseId: string;
  name?: string;
  description?: string;
  tags?: string[];
  category?: string;
  input?: unknown;
  expected?: unknown;
  weight?: number;
  metadata?: Record<string, unknown>;
  artifactRefs?: Record<string, unknown>[];
}

/** A named, versioned collection of test cases. */
export interface TestSuite {
  suiteId: string;
  name?: string;
  version?: string;
  description?: string;
  tags?: string[];
  changelog?: string;
  testCases: TestCase[];
}

/** Returns the case with `testCaseId`, if the suite has one. */
export function suiteCase(suite: TestSuite | undefined, testCaseId: string): TestCase | undefined {
  return suite?.testCases.find((testCase) => testCase.testCaseId === testCaseId);
}

/** Builds a test case from a portable mapping, accepting `id` or `test_case_id`. */
export function testCaseFromObject(data: unknown): TestCase {
  if (!isRecord(data)) {
    throw new Error(`test case must be a mapping, got ${describe(data)}`);
  }
  const testCaseId = String(data.test_case_id ?? data.testCaseId ?? data.id ?? '').trim();
  if (testCaseId.length === 0) {
    throw new Error("test case requires an 'id' (or 'test_case_id')");
  }
  const tags = data.tags ?? [];
  if (!Array.isArray(tags) || !tags.every((tag) => typeof tag === 'string')) {
    throw new Error(`test case "${testCaseId}" tags must be a list of strings`);
  }
  const metadata = data.metadata ?? {};
  if (!isRecord(metadata)) {
    throw new Error(`test case "${testCaseId}" metadata must be a mapping`);
  }
  const rawWeight = data.weight ?? 1;
  const weight = typeof rawWeight === 'number' ? rawWeight : Number(rawWeight);
  if (!Number.isFinite(weight)) {
    throw new Error(`test case "${testCaseId}" weight must be numeric`);
  }
  const artifactRefs = Array.isArray(data.artifact_refs ?? data.artifactRefs)
    ? ((data.artifact_refs ?? data.artifactRefs) as unknown[]).filter(isRecord).map((ref) => ({ ...ref }))
    : [];
  return {
    testCaseId,
    name: optionalString(data.name),
    description: optionalString(data.description),
    tags: [...tags],
    category: optionalString(data.category),
    input: data.input,
    expected: data.expected,
    weight,
    metadata: { ...metadata },
    artifactRefs,
  };
}

/** Returns the portable YAML/JSON representation of one test case. */
export function testCaseToObject(testCase: TestCase): Record<string, unknown> {
  const out: Record<string, unknown> = { id: testCase.testCaseId };
  putNonBlank(out, 'name', testCase.name);
  putNonBlank(out, 'description', testCase.description);
  if (testCase.tags !== undefined && testCase.tags.length > 0) {
    out.tags = [...testCase.tags];
  }
  putNonBlank(out, 'category', testCase.category);
  if (testCase.input !== undefined) {
    out.input = testCase.input;
  }
  if (testCase.expected !== undefined) {
    out.expected = testCase.expected;
  }
  if (testCase.weight !== undefined && testCase.weight !== 1) {
    out.weight = testCase.weight;
  }
  if (testCase.metadata !== undefined && Object.keys(testCase.metadata).length > 0) {
    out.metadata = { ...testCase.metadata };
  }
  if (testCase.artifactRefs !== undefined && testCase.artifactRefs.length > 0) {
    out.artifact_refs = testCase.artifactRefs.map((ref) => ({ ...ref }));
  }
  return out;
}

/** Builds a suite from a portable mapping, accepting `cases` or `test_cases`. */
export function testSuiteFromObject(data: unknown): TestSuite {
  if (!isRecord(data)) {
    throw new Error(`suite must be a mapping, got ${describe(data)}`);
  }
  const suiteId = String(data.suite_id ?? data.suiteId ?? data.id ?? '').trim();
  if (suiteId.length === 0) {
    throw new Error("suite requires a 'suite_id' (or 'id')");
  }
  const rawCases = data.cases ?? data.test_cases ?? data.testCases ?? [];
  if (!Array.isArray(rawCases)) {
    throw new Error('suite cases must be a list of mappings');
  }
  const tags = data.tags ?? [];
  if (!Array.isArray(tags) || !tags.every((tag) => typeof tag === 'string')) {
    throw new Error('suite tags must be a list of strings');
  }
  return {
    suiteId,
    name: optionalString(data.name),
    version: suiteVersionFromObject(data.version),
    description: optionalString(data.description),
    tags: [...tags],
    changelog: optionalString(data.changelog),
    testCases: rawCases.map(testCaseFromObject),
  };
}

/** Returns the portable YAML/JSON representation of a suite. */
export function testSuiteToObject(suite: TestSuite): Record<string, unknown> {
  const out: Record<string, unknown> = { suite_id: suite.suiteId };
  putNonBlank(out, 'name', suite.name);
  putNonBlank(out, 'version', suite.version);
  putNonBlank(out, 'description', suite.description);
  if (suite.tags !== undefined && suite.tags.length > 0) {
    out.tags = [...suite.tags];
  }
  putNonBlank(out, 'changelog', suite.changelog);
  out.cases = suite.testCases.map(testCaseToObject);
  return out;
}

/** Maps a candidate onto the metadata keys a run and its trials carry. */
export function candidateMetadata(candidate: Candidate | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (candidate === undefined) {
    return out;
  }
  for (const [key, value] of [
    ['agent_name', candidate.agentName],
    ['agent_version', candidate.agentVersion],
    ['prompt_version', candidate.promptVersion],
    ['model_provider', candidate.modelProvider],
    ['model_name', candidate.modelName],
    ['git_sha', candidate.gitSha],
  ] as const) {
    if (value !== undefined && value.length > 0) {
      out[key] = value;
    }
  }
  return out;
}

// --------------------------------------------------------------------------- //
// TrialRef: handing one trial across a process or container boundary
// --------------------------------------------------------------------------- //

// One pair per handoff field, so the name `trialRefFromEnv` reads and the name
// `trialRefToEnv` writes cannot drift apart.
const envExperimentId = brandedPair('EXPERIMENT_ID');
const envTestCaseId = brandedPair('TEST_CASE_ID');
const envAttempt = brandedPair('ATTEMPT');
const envSuiteId = brandedPair('SUITE_ID');
const envSuiteVersion = brandedPair('SUITE_VERSION');
const envTrajectoryId = brandedPair('TRAJECTORY_ID');

export const ENV_EXPERIMENT_ID = envExperimentId.preferred;
export const ENV_TEST_CASE_ID = envTestCaseId.preferred;
export const ENV_ATTEMPT = envAttempt.preferred;
export const ENV_SUITE_ID = envSuiteId.preferred;
export const ENV_SUITE_VERSION = envSuiteVersion.preferred;
export const ENV_TRAJECTORY_ID = envTrajectoryId.preferred;

/**
 * Pre-rename spellings, written beside the canonical names by `trialRefToEnv`
 * so a child process on an older SDK build still receives the trial context.
 */
export const LEGACY_ENV_EXPERIMENT_ID = envExperimentId.legacy;
export const LEGACY_ENV_TEST_CASE_ID = envTestCaseId.legacy;
export const LEGACY_ENV_ATTEMPT = envAttempt.legacy;
export const LEGACY_ENV_SUITE_ID = envSuiteId.legacy;
export const LEGACY_ENV_SUITE_VERSION = envSuiteVersion.legacy;
export const LEGACY_ENV_TRAJECTORY_ID = envTrajectoryId.legacy;
// SIGIL_RUN_ID is the pre-rename name of the experiment id, read only after both
// EXPERIMENT_ID spellings. Go resolves it the same way.
const ENV_LEGACY_RUN_ID = 'SIGIL_RUN_ID';

/**
 * A serializable pointer to one trial, openable in any process.
 *
 * `experimentId` is the canonical run identifier. `trajectoryId` is an optional
 * stable per-attempt id used to correlate an out-of-band execution with this
 * trial.
 */
export interface TrialRef {
  experimentId: string;
  testCaseId: string;
  attempt: number;
  suiteId?: string;
  suiteVersion?: string;
  suiteName?: string;
  testCaseName?: string;
  trajectoryId?: string;
}

export function trialRefToJSON(ref: TrialRef): Record<string, unknown> {
  return {
    experiment_id: ref.experimentId,
    test_case_id: ref.testCaseId,
    attempt: ref.attempt,
    suite_id: ref.suiteId ?? '',
    suite_version: ref.suiteVersion ?? '',
    suite_name: ref.suiteName ?? '',
    test_case_name: ref.testCaseName ?? '',
    trajectory_id: ref.trajectoryId ?? '',
  };
}

export function trialRefFromJSON(payload: unknown): TrialRef {
  const data = isRecord(payload) ? payload : {};
  const attempt = Number(data.attempt ?? 1);
  return {
    experimentId: String(data.experiment_id ?? data.run_id ?? '').trim(),
    testCaseId: String(data.test_case_id ?? '').trim(),
    attempt: Number.isFinite(attempt) && attempt > 0 ? Math.trunc(attempt) : 1,
    suiteId: String(data.suite_id ?? '').trim(),
    suiteVersion: String(data.suite_version ?? '').trim(),
    suiteName: String(data.suite_name ?? '').trim(),
    testCaseName: String(data.test_case_name ?? '').trim(),
    trajectoryId: String(data.trajectory_id ?? '').trim(),
  };
}

/**
 * The environment variables that carry this ref to a child process, written
 * under both the canonical and the legacy spelling. Go's core
 * `agento11y.TrialRef.ToEnv` does the same, so a parent can spawn a child
 * running an older SDK build.
 */
export function trialRefToEnv(ref: TrialRef): Record<string, string> {
  const env: Record<string, string> = {
    [ENV_EXPERIMENT_ID]: ref.experimentId,
    [LEGACY_ENV_EXPERIMENT_ID]: ref.experimentId,
    [ENV_TEST_CASE_ID]: ref.testCaseId,
    [LEGACY_ENV_TEST_CASE_ID]: ref.testCaseId,
    [ENV_ATTEMPT]: String(ref.attempt),
    [LEGACY_ENV_ATTEMPT]: String(ref.attempt),
  };
  if (ref.suiteId !== undefined && ref.suiteId.length > 0) {
    env[ENV_SUITE_ID] = ref.suiteId;
    env[LEGACY_ENV_SUITE_ID] = ref.suiteId;
  }
  if (ref.suiteVersion !== undefined && ref.suiteVersion.length > 0) {
    env[ENV_SUITE_VERSION] = ref.suiteVersion;
    env[LEGACY_ENV_SUITE_VERSION] = ref.suiteVersion;
  }
  if (ref.trajectoryId !== undefined && ref.trajectoryId.length > 0) {
    env[ENV_TRAJECTORY_ID] = ref.trajectoryId;
    env[LEGACY_ENV_TRAJECTORY_ID] = ref.trajectoryId;
  }
  return env;
}

/**
 * Reads a ref from the environment, returning undefined when the experiment or
 * test-case id is missing. A nonblank `AGENTO11Y_*` value wins over every
 * `SIGIL_*` spelling; a legacy spelling that supplies a value is read and
 * reported once per process.
 */
export function trialRefFromEnv(
  env: Record<string, string | undefined> = defaultEnv(),
  logger?: Agento11yLogger,
): TrialRef | undefined {
  const experimentId = resolveTrialEnv(env, envExperimentId, logger) ?? resolveLegacyRunId(env, logger) ?? '';
  const testCaseId = resolveTrialEnv(env, envTestCaseId, logger) ?? '';
  if (experimentId.length === 0 || testCaseId.length === 0) {
    return undefined;
  }
  const parsedAttempt = Number.parseInt(resolveTrialEnv(env, envAttempt, logger) ?? '', 10);
  return {
    experimentId,
    testCaseId,
    attempt: Number.isFinite(parsedAttempt) && parsedAttempt > 0 ? parsedAttempt : 1,
    suiteId: resolveTrialEnv(env, envSuiteId, logger) ?? '',
    suiteVersion: resolveTrialEnv(env, envSuiteVersion, logger) ?? '',
    trajectoryId: resolveTrialEnv(env, envTrajectoryId, logger) ?? '',
  };
}

function resolveTrialEnv(
  env: Record<string, string | undefined>,
  pair: EnvPair,
  logger?: Agento11yLogger,
): string | undefined {
  const selected = envTrimmed(env, pair);
  if (selected === undefined) return undefined;
  if (selected.key !== pair.preferred) {
    warnLegacyTrialEnv(selected.key, pair.preferred, logger);
  }
  return selected.value;
}

function resolveLegacyRunId(env: Record<string, string | undefined>, logger?: Agento11yLogger): string | undefined {
  const value = (env[ENV_LEGACY_RUN_ID] ?? '').trim();
  if (value.length === 0) return undefined;
  warnLegacyTrialEnv(ENV_LEGACY_RUN_ID, ENV_EXPERIMENT_ID, logger);
  return value;
}

const warnedLegacyEnv = new Set<string>();

/** Warns once per process that a legacy name supplied a value. */
export function warnLegacyTrialEnv(legacy: string, replacement: string, logger?: Agento11yLogger): void {
  if (warnedLegacyEnv.has(legacy)) return;
  warnedLegacyEnv.add(legacy);
  const message = `agento11y: ${legacy} is deprecated; rename it to ${replacement}`;
  if (logger?.warn !== undefined) {
    logger.warn(message);
  } else {
    console.warn(message);
  }
}

/** Clears the once-per-process warning set. Test-only. */
export function resetLegacyTrialEnvWarnings(): void {
  warnedLegacyEnv.clear();
}

function optionalString(value: unknown): string | undefined {
  if (typeof value !== 'string' || value.length === 0) {
    return undefined;
  }
  return value;
}

function describe(value: unknown): string {
  if (value === null) {
    return 'null';
  }
  return Array.isArray(value) ? 'array' : typeof value;
}

/**
 * Reads the suite version, which has to be a string.
 *
 * An unquoted `version: 1.0` parses to the number 1, and JavaScript cannot tell
 * that from `1`, so it would reach the wire as "1" where Python sends "1.0". The
 * version labels the suite in run upsert, in every trial snapshot, and in the OTel
 * attributes, so rejecting the file beats disagreeing with the other SDKs.
 */
function suiteVersionFromObject(raw: unknown): string {
  if (raw === undefined || raw === null || raw === '') {
    return '1.0.0';
  }
  if (typeof raw !== 'string') {
    throw new Error(`suite version must be a string, got ${describe(raw)}; quote it, for example version: "1.0"`);
  }
  return raw;
}
