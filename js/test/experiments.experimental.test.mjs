import assert from 'node:assert/strict';
import test from 'node:test';

import {
  ENV_ENABLE_EXPERIMENTAL_FEATURES,
  ExperimentalFeatureDisabledError,
  experimentalFeaturesEnabled,
  FEATURE_CLOUD_TRIAL_EVALUATION,
  requireExperimental,
} from '../.test-dist/experiments/experimental.js';

// Mirrors test_experimental_gate_reads_truthy_values in python/tests/test_experiments.py
// and go/agento11y/experimental_test.go.
const truthyTable = [
  { value: '1', enabled: true },
  { value: 'true', enabled: true },
  { value: 'TRUE', enabled: true },
  { value: 'True', enabled: true },
  { value: 'yes', enabled: true },
  { value: 'on', enabled: true },
  { value: '  true  ', enabled: true },
  { value: '0', enabled: false },
  { value: 'false', enabled: false },
  { value: 'no', enabled: false },
  { value: 'off', enabled: false },
  { value: 'maybe', enabled: false },
];

test('experimentalFeaturesEnabled accepts only the documented truthy spellings', () => {
  for (const { value, enabled } of truthyTable) {
    assert.equal(
      experimentalFeaturesEnabled({ [ENV_ENABLE_EXPERIMENTAL_FEATURES]: value }),
      enabled,
      `value ${JSON.stringify(value)} should be ${enabled ? 'enabled' : 'disabled'}`,
    );
  }
});

test('an unset variable and an empty variable are both disabled', () => {
  // Unset and empty are distinct inputs: a gate test that assigns a falsy value
  // never exercises the absent case.
  assert.equal(experimentalFeaturesEnabled({}), false);
  assert.equal(experimentalFeaturesEnabled({ [ENV_ENABLE_EXPERIMENTAL_FEATURES]: '' }), false);
  assert.equal(experimentalFeaturesEnabled({ [ENV_ENABLE_EXPERIMENTAL_FEATURES]: '   ' }), false);
});

test('the gate has no legacy SIGIL_ spelling', () => {
  // The name postdates the rename, so no release ever read SIGIL_ for it.
  assert.equal(experimentalFeaturesEnabled({ SIGIL_ENABLE_EXPERIMENTAL_FEATURES: 'true' }), false);
});

test('requireExperimental throws a named error when the gate is closed', () => {
  assert.throws(
    () => requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION, {}),
    (error) => {
      assert.ok(error instanceof ExperimentalFeatureDisabledError);
      assert.equal(error.name, 'ExperimentalFeatureDisabledError');
      assert.equal(error.feature, 'cloud trial evaluation');
      assert.equal(error.envVar, 'AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES');
      assert.equal(
        error.message,
        'agento11y: cloud trial evaluation is experimental; set AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true to use it',
      );
      return true;
    },
  );
});

test('requireExperimental passes when the gate is open', () => {
  requireExperimental(FEATURE_CLOUD_TRIAL_EVALUATION, { [ENV_ENABLE_EXPERIMENTAL_FEATURES]: 'yes' });
});

test('a blank feature name still names something', () => {
  const error = new ExperimentalFeatureDisabledError('   ');
  assert.equal(error.feature, 'this feature');
  assert.match(error.message, /^agento11y: this feature is experimental/);
});

test('the gate reads process.env by default', () => {
  assert.equal(experimentalFeaturesEnabled(), false);
  process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES] = 'on';
  try {
    assert.equal(experimentalFeaturesEnabled(), true);
  } finally {
    delete process.env[ENV_ENABLE_EXPERIMENTAL_FEATURES];
  }
  assert.equal(experimentalFeaturesEnabled(), false);
});
