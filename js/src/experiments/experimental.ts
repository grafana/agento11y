import { parseTruthy } from '../config.js';

/**
 * Opt-in switch for SDK features that are not stable yet. Set it to `1`, `true`,
 * `yes`, or `on`.
 *
 * An experimental feature can change or be removed in any release, and is not
 * covered by the compatibility the rest of the SDK aims for.
 *
 * This gate is separate from `AGENTO11Y_USE_EXPERIMENTAL_OTEL`, which stays an
 * independent opt-in for experimental trial spans and evaluation-result events.
 */
export const ENV_ENABLE_EXPERIMENTAL_FEATURES = 'AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES';

/** Names the experimental feature that grades a trial with a stored evaluator. */
export const FEATURE_CLOUD_TRIAL_EVALUATION = 'cloud trial evaluation';

/** Thrown when an experimental feature is called without the opt-in env var. */
export class ExperimentalFeatureDisabledError extends Error {
  readonly feature: string;
  readonly envVar: string;

  constructor(feature: string, envVar: string = ENV_ENABLE_EXPERIMENTAL_FEATURES) {
    const normalizedFeature = feature.trim().length > 0 ? feature.trim() : 'this feature';
    const normalizedEnv = envVar.trim().length > 0 ? envVar.trim() : ENV_ENABLE_EXPERIMENTAL_FEATURES;
    super(`agento11y: ${normalizedFeature} is experimental; set ${normalizedEnv}=true to use it`);
    this.name = 'ExperimentalFeatureDisabledError';
    this.feature = normalizedFeature;
    this.envVar = normalizedEnv;
  }
}

/** Reports whether the experimental opt-in is set to a truthy value. */
export function experimentalFeaturesEnabled(env: Record<string, string | undefined> = processEnv()): boolean {
  return parseTruthy(env[ENV_ENABLE_EXPERIMENTAL_FEATURES] ?? '');
}

/** Throws `ExperimentalFeatureDisabledError` unless the gate is set. */
export function requireExperimental(feature: string, env: Record<string, string | undefined> = processEnv()): void {
  if (!experimentalFeaturesEnabled(env)) {
    throw new ExperimentalFeatureDisabledError(feature);
  }
}

function processEnv(): Record<string, string | undefined> {
  if (typeof process !== 'undefined' && process.env !== undefined) {
    return process.env;
  }
  return {};
}
