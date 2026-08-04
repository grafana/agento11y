import { parse as parseYAML, stringify as stringifyYAML } from 'yaml';
import type { TestSuite } from './types.js';
import { testSuiteFromObject, testSuiteToObject } from './types.js';

/**
 * Portable suite YAML: the same document Go's `ParseSuite` and Python's
 * `TestSuite.from_yaml` read.
 *
 * Three legacy aliases are accepted on load: a suite id under `suite_id` or `id`,
 * a case collection under `cases` or `test_cases`, and a case id under `id` or
 * `test_case_id`. Saving always writes the canonical spelling (`suite_id`,
 * `cases`, `id`), so a load-save round trip normalizes an older document.
 */
export function parseSuiteYAML(text: string): TestSuite {
  let document: unknown;
  try {
    document = parseYAML(text);
  } catch (error) {
    throw new Error(`agento11y test suite validation failed: parse suite YAML: ${(error as Error).message}`);
  }
  const suite = testSuiteFromObject(document);
  validateSuite(suite);
  return suite;
}

/** Serializes a suite to portable YAML. */
export function stringifySuiteYAML(suite: TestSuite): string {
  validateSuite(suite);
  return stringifyYAML(testSuiteToObject(suite));
}

/**
 * Rejects a suite the control plane and the report rollup cannot use: a blank
 * suite id, a blank case id, or two cases claiming the same id.
 */
export function validateSuite(suite: TestSuite): void {
  if (suite.suiteId.trim().length === 0) {
    throw new Error('agento11y test suite validation failed: suite_id is required');
  }
  const seen = new Set<string>();
  for (const testCase of suite.testCases) {
    const id = testCase.testCaseId.trim();
    if (id.length === 0) {
      throw new Error('agento11y test suite validation failed: test_case_id is required');
    }
    if (seen.has(id)) {
      throw new Error(`agento11y test suite validation failed: duplicate test_case_id "${id}"`);
    }
    seen.add(id);
  }
}
