import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

test('the package publishes the experiments subpath', async () => {
  const manifest = JSON.parse(await readFile(path.join(__dirname, '..', 'package.json'), 'utf8'));
  assert.deepEqual(manifest.exports['./experiments'], {
    types: './dist/experiments/index.d.ts',
    import: './dist/experiments/index.js',
  });
  assert.equal(typeof manifest.dependencies.yaml, 'string', 'stored suites need a YAML parser at runtime');
});

test('the experiments entrypoint exports the public surface', async () => {
  const module = await import('../.test-dist/experiments/index.js');
  for (const name of [
    'ExperimentsClient',
    'Experiment',
    'Trial',
    'withExperiment',
    'TestSuitesClient',
    'LLMJudge',
    'RegexJudge',
    'stableId',
    'requireExperimental',
    'ExperimentalFeatureDisabledError',
    'TrialEvaluationFailedError',
    'TrialEvaluationTimeoutError',
    'ExperimentConflictError',
    'parseSuiteYAML',
    'stringifySuiteYAML',
    'trialRefFromEnv',
    'otel',
    'routes',
  ]) {
    assert.ok(name in module, `${name} is missing from the experiments export`);
  }
});

test('experiments stays out of the core runtime boundary', async () => {
  // core.ts is imported on edge runtimes without process or Buffer, while the
  // experiments module needs crypto and environment access.
  const coreSource = await readFile(path.join(__dirname, '..', 'src', 'core.ts'), 'utf8');
  assert.ok(!coreSource.includes('experiments'), 'core.ts must not reach into the experiments tree');

  const coreTsconfig = JSON.parse(await readFile(path.join(__dirname, '..', '..', 'js-core', 'tsconfig.json'), 'utf8'));
  assert.ok(
    !coreTsconfig.include.some((entry) => entry.includes('experiments')),
    'the core package must not compile the experiments tree',
  );

  // Walk the compiled core graph: an experiments import anywhere in it would
  // break the edge-runtime promise even when the smoke import happens to pass.
  const testDist = path.join(__dirname, '..', '.test-dist');
  const reached = await reachableModules(path.join(testDist, 'core.js'));
  const experimentsModules = [...reached].filter((file) => file.includes(`${path.sep}experiments${path.sep}`));
  assert.deepEqual(experimentsModules, [], 'the core module graph must not reach the experiments tree');

  const coreEntryUrl = pathToFileURL(path.join(testDist, 'core.js')).href;
  const script = `
    delete globalThis.process;
    delete globalThis.Buffer;
    await import(${JSON.stringify(coreEntryUrl)});
  `;
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script], { encoding: 'utf8' });
  assert.equal(result.status, 0, `core import failed without process/Buffer:\n${result.stderr}`);
});

/** Collects every local module reachable from `entry` through relative imports. */
async function reachableModules(entry, seen = new Set()) {
  if (seen.has(entry)) {
    return seen;
  }
  seen.add(entry);
  const source = await readFile(entry, 'utf8');
  const specifiers = [...source.matchAll(/from\s+'(\.[^']+)'/g)].map((match) => match[1]);
  for (const specifier of specifiers) {
    await reachableModules(path.resolve(path.dirname(entry), specifier), seen);
  }
  return seen;
}
