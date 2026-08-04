import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';

import { ExperimentConflictError } from '../.test-dist/experiments/errors.js';
import {
  localCaseToRemote,
  normalizeControlEndpoint,
  remoteCaseToLocal,
  TestSuitesClient,
} from '../.test-dist/experiments/suites.js';
import {
  ENV_ATTEMPT,
  ENV_EXPERIMENT_ID,
  ENV_SUITE_ID,
  ENV_SUITE_VERSION,
  ENV_TEST_CASE_ID,
  ENV_TRAJECTORY_ID,
  resetLegacyTrialEnvWarnings,
  suiteCase,
  testSuiteFromObject,
  testSuiteToObject,
  trialRefFromEnv,
  trialRefFromJSON,
  trialRefToEnv,
  trialRefToJSON,
} from '../.test-dist/experiments/types.js';
import { parseSuiteYAML, stringifySuiteYAML } from '../.test-dist/experiments/yaml.js';

// --- portable YAML -------------------------------------------------------- //

test('the canonical YAML document loads', () => {
  const suite = parseSuiteYAML(`
suite_id: smoke
name: Smoke
version: 2.0.0
description: sanity checks
tags: [nightly]
changelog: first cut
cases:
  - id: add
    name: Addition
    input: 2+2
    expected: "4"
    tags: [math]
    weight: 2
    metadata:
      owner: alice
`);
  assert.equal(suite.suiteId, 'smoke');
  assert.equal(suite.name, 'Smoke');
  assert.equal(suite.version, '2.0.0');
  assert.deepEqual(suite.tags, ['nightly']);
  assert.equal(suite.changelog, 'first cut');
  assert.equal(suite.testCases.length, 1);
  assert.equal(suite.testCases[0].testCaseId, 'add');
  assert.equal(suite.testCases[0].weight, 2);
  assert.deepEqual(suite.testCases[0].metadata, { owner: 'alice' });
});

test('the legacy aliases load and round-trip to the canonical spelling', () => {
  const suite = parseSuiteYAML(`
id: suite-1
test_cases:
  - test_case_id: case-1
    input: hello
`);
  assert.equal(suite.suiteId, 'suite-1');
  assert.equal(suite.testCases[0].testCaseId, 'case-1');
  assert.equal(suite.version, '1.0.0', 'a missing version defaults');

  const reloaded = parseSuiteYAML(stringifySuiteYAML(suite));
  assert.equal(reloaded.suiteId, 'suite-1');
  assert.equal(reloaded.testCases[0].testCaseId, 'case-1');
  assert.equal(reloaded.testCases[0].input, 'hello');
  assert.deepEqual(testSuiteToObject(reloaded), testSuiteToObject(suite));
});

test('a save and reload preserves structured values', () => {
  const suite = {
    suiteId: 'structured',
    name: 'Structured',
    version: '1.2.3',
    testCases: [
      {
        testCaseId: 'nested',
        input: { question: '2+2', context: ['math'] },
        expected: { answer: 4 },
        tags: ['a', 'b'],
        weight: 1,
        metadata: { owner: 'bob' },
        artifactRefs: [{ artifact_id: 'art-1', name: 'notes', kind: 'text' }],
      },
    ],
  };
  const reloaded = parseSuiteYAML(stringifySuiteYAML(suite));
  assert.deepEqual(reloaded.testCases[0].input, { question: '2+2', context: ['math'] });
  assert.deepEqual(reloaded.testCases[0].expected, { answer: 4 });
  assert.deepEqual(reloaded.testCases[0].artifactRefs, [{ artifact_id: 'art-1', name: 'notes', kind: 'text' }]);
  assert.equal(reloaded.testCases[0].weight, 1);
});

test('a default weight is omitted from the saved document', () => {
  const yaml = stringifySuiteYAML({
    suiteId: 's',
    testCases: [{ testCaseId: 'c', input: 'x', weight: 1 }],
  });
  assert.ok(!yaml.includes('weight'));
  assert.ok(
    stringifySuiteYAML({ suiteId: 's', testCases: [{ testCaseId: 'c', input: 'x', weight: 3 }] }).includes('weight: 3'),
  );
});

const invalidYamlCases = [
  { name: 'a missing suite id', text: 'cases: []', message: /suite requires a 'suite_id'/ },
  { name: 'a missing case id', text: 'suite_id: s\ncases:\n  - input: x\n', message: /test case requires an 'id'/ },
  { name: 'a scalar document', text: '"just a string"', message: /suite must be a mapping, got string/ },
  { name: 'non-string tags', text: 'suite_id: s\ntags: [1]\n', message: /suite tags must be a list of strings/ },
  {
    name: 'a non-mapping case list',
    text: 'suite_id: s\ncases: not-a-list\n',
    message: /suite cases must be a list of mappings/,
  },
  {
    name: 'broken YAML',
    text: 'suite_id: [unclosed',
    message: /parse suite YAML:/,
  },
];

test('an invalid suite document is rejected with a specific message', () => {
  for (const { name, text, message } of invalidYamlCases) {
    assert.throws(() => parseSuiteYAML(text), message, name);
  }
});

test('duplicate case ids are rejected', () => {
  assert.throws(
    () => parseSuiteYAML('suite_id: s\ncases:\n  - id: dup\n    input: a\n  - id: dup\n    input: b\n'),
    /agento11y test suite validation failed: duplicate test_case_id "dup"/,
  );
});

test('suiteCase finds a case by id', () => {
  const suite = testSuiteFromObject({ suite_id: 's', cases: [{ id: 'a', input: 1 }] });
  assert.equal(suiteCase(suite, 'a').testCaseId, 'a');
  assert.equal(suiteCase(suite, 'missing'), undefined);
  assert.equal(suiteCase(undefined, 'a'), undefined);
});

// --- TrialRef ------------------------------------------------------------- //

test('a TrialRef round-trips through the environment', () => {
  const ref = {
    experimentId: 'run-1',
    testCaseId: 'case-1',
    attempt: 3,
    suiteId: 'smoke',
    suiteVersion: '2.0.0',
    trajectoryId: 'traj-1',
  };
  const env = trialRefToEnv(ref);
  assert.deepEqual(env, {
    [ENV_EXPERIMENT_ID]: 'run-1',
    [ENV_TEST_CASE_ID]: 'case-1',
    [ENV_ATTEMPT]: '3',
    [ENV_SUITE_ID]: 'smoke',
    [ENV_SUITE_VERSION]: '2.0.0',
    [ENV_TRAJECTORY_ID]: 'traj-1',
  });
  const restored = trialRefFromEnv(env);
  assert.equal(restored.experimentId, 'run-1');
  assert.equal(restored.testCaseId, 'case-1');
  assert.equal(restored.attempt, 3);
  assert.equal(restored.suiteId, 'smoke');
  assert.equal(restored.suiteVersion, '2.0.0');
  assert.equal(restored.trajectoryId, 'traj-1');
});

test('a TrialRef without an experiment or case is undefined', () => {
  assert.equal(trialRefFromEnv({}), undefined);
  assert.equal(trialRefFromEnv({ [ENV_EXPERIMENT_ID]: 'run-1' }), undefined);
  assert.equal(trialRefFromEnv({ [ENV_TEST_CASE_ID]: 'case-1' }), undefined);
});

test('a missing or unparseable attempt falls back to one', () => {
  const base = { [ENV_EXPERIMENT_ID]: 'run-1', [ENV_TEST_CASE_ID]: 'case-1' };
  assert.equal(trialRefFromEnv(base).attempt, 1);
  assert.equal(trialRefFromEnv({ ...base, [ENV_ATTEMPT]: 'nonsense' }).attempt, 1);
  assert.equal(trialRefFromEnv({ ...base, [ENV_ATTEMPT]: '0' }).attempt, 1);
  assert.equal(trialRefFromEnv({ ...base, [ENV_ATTEMPT]: '4' }).attempt, 4);
});

test('a SIGIL_ spelling is reported and ignored', () => {
  resetLegacyTrialEnvWarnings();
  const warnings = [];
  const ref = trialRefFromEnv(
    { SIGIL_EXPERIMENT_ID: 'run-legacy', SIGIL_TEST_CASE_ID: 'case-legacy' },
    { warn: (message) => warnings.push(message) },
  );
  assert.equal(ref, undefined, 'the legacy name is not read as a value');
  assert.deepEqual(warnings, [
    'agento11y: SIGIL_EXPERIMENT_ID is ignored; rename it to AGENTO11Y_EXPERIMENT_ID',
    'agento11y: SIGIL_TEST_CASE_ID is ignored; rename it to AGENTO11Y_TEST_CASE_ID',
  ]);

  // The warning fires once per process, not once per read.
  const again = [];
  trialRefFromEnv({ SIGIL_EXPERIMENT_ID: 'run-legacy' }, { warn: (message) => again.push(message) });
  assert.deepEqual(again, []);
});

test('a TrialRef round-trips through JSON, including the run_id alias', () => {
  const ref = {
    experimentId: 'run-1',
    testCaseId: 'case-1',
    attempt: 2,
    suiteId: 'smoke',
    suiteVersion: '1.0.0',
    suiteName: 'Smoke',
    testCaseName: 'Addition',
    trajectoryId: 'traj-1',
  };
  assert.deepEqual(trialRefFromJSON(trialRefToJSON(ref)), ref);
  assert.equal(trialRefFromJSON({ run_id: 'run-legacy', test_case_id: 'c' }).experimentId, 'run-legacy');
});

// --- control-plane routing ------------------------------------------------ //

const endpointCases = [
  {
    input: 'https://stack.grafana.net',
    want: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
  },
  {
    input: 'https://stack.grafana.net/',
    want: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
  },
  {
    input: 'https://stack.grafana.net/a/grafana-agento11y-app/experiments',
    want: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
  },
  {
    input: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
    want: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
  },
  { input: 'https://host/prefix', want: 'https://host/prefix/api/plugins/grafana-agento11y-app/resources/eval' },
  { input: 'https://host/api/v1/eval', want: 'https://host/api/v1/eval' },
];

test('control endpoints normalize onto the plugin resources path', () => {
  for (const { input, want } of endpointCases) {
    assert.equal(normalizeControlEndpoint(input), want, input);
  }
});

test('a relative or non-http control endpoint is rejected', () => {
  for (const value of ['stack.grafana.net', 'ftp://host/x', '']) {
    assert.throws(() => normalizeControlEndpoint(value), /controlEndpoint must be an absolute URL/, value);
  }
});

test('the client requires an endpoint and a token', () => {
  assert.throws(() => new TestSuitesClient({ env: {} }), /controlEndpoint is required/);
  assert.throws(
    () => new TestSuitesClient({ grafanaUrl: 'https://stack.grafana.net', env: {} }),
    /serviceAccountToken is required/,
  );
  const fromEnv = new TestSuitesClient({
    env: {
      AGENTO11Y_GRAFANA_URL: 'https://stack.grafana.net',
      AGENTO11Y_SERVICE_ACCOUNT_TOKEN: 'glsa_token',
    },
  });
  assert.equal(fromEnv.grafanaUrl, 'https://stack.grafana.net');
  assert.equal(fromEnv.endpoint, 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval');
});

test('the grafana url is derived from the control endpoint when unset', () => {
  const client = new TestSuitesClient({
    controlEndpoint: 'https://stack.grafana.net/api/plugins/grafana-agento11y-app/resources/eval',
    serviceAccountToken: 'glsa_token',
    env: {},
  });
  assert.equal(client.grafanaUrl, 'https://stack.grafana.net');
});

test('suite reads use the control path, the bearer token, and the paging defaults', async () => {
  const { endpoint, seen, close } = await startServer((request) => {
    if (request.url.startsWith('/api/plugins/grafana-agento11y-app/resources/eval/test-suites?')) {
      return { status: 200, body: { items: [{ suite_id: 'smoke' }], next_cursor: '0' } };
    }
    return { status: 200, body: {} };
  });
  try {
    const client = newClient(endpoint);
    const suites = await client.listSuites();
    assert.deepEqual(suites, [{ suite_id: 'smoke' }]);
    assert.equal(seen[0].url, '/api/plugins/grafana-agento11y-app/resources/eval/test-suites?limit=200');
    assert.equal(seen[0].headers.authorization, 'Bearer glsa_token');
  } finally {
    await close();
  }
});

test('paging follows the cursor and stops when it clears', async () => {
  const pages = [
    { items: [{ suite_id: 'a' }], next_cursor: 'p2' },
    { items: [{ suite_id: 'b' }], next_cursor: '' },
  ];
  let index = 0;
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: pages[index++] }));
  try {
    const suites = await newClient(endpoint).listSuites({ limit: 1 });
    assert.deepEqual(suites, [{ suite_id: 'a' }, { suite_id: 'b' }]);
    assert.equal(seen[0].url.endsWith('?limit=1'), true);
    assert.equal(seen[1].url.endsWith('?limit=1&cursor=p2'), true);
  } finally {
    await close();
  }
});

test('a cursor that never clears stops at maxPages', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 200,
    body: { items: [{ suite_id: 'a' }], next_cursor: 'always' },
  }));
  try {
    await assert.rejects(
      newClient(endpoint).listSuites({ maxPages: 3 }),
      /agento11y experiment transport failed: test suite list: pagination did not terminate/,
    );
    assert.equal(seen.length, 3);
  } finally {
    await close();
  }
});

test('version aliases resolve against the stored record', () => {
  const client = newOfflineClient();
  const suite = {
    versions: [
      { version: 'v1', published: true },
      { version: 'v2', published: true },
      { version: 'v3', published: false },
    ],
  };
  assert.equal(client.resolveVersion(suite, 'latest_published'), 'v2');
  assert.equal(client.resolveVersion(suite, 'latest'), 'v3');
  assert.equal(client.resolveVersion(suite, 'draft'), 'v3');
  assert.equal(client.resolveVersion(suite, 'v1'), 'v1');
  assert.throws(() => client.resolveVersion(suite, 'v9'), /agento11y experiment not found: test suite version: v9/);
  assert.throws(() => client.resolveVersion(suite, '  '), /test suite: version is required/);
  assert.throws(
    () => client.resolveVersion({ versions: [{ version: 'v1', published: false }] }, 'latest_published'),
    /test suite version: latest_published/,
  );
  assert.throws(() => client.resolveVersion({ versions: [] }, 'latest'), /test suite version: latest/);
  assert.throws(
    () => client.resolveVersion({ versions: [{ version: 'v1', published: true }] }, 'draft'),
    /test suite version: draft/,
  );
});

test('pullSuite resolves the version and maps remote cases', async () => {
  const { endpoint, seen, close } = await startServer((request) => {
    if (request.url.includes('/test-cases')) {
      return {
        status: 200,
        body: {
          items: [
            {
              test_case_id: 'add',
              name: 'Addition',
              input: { value: '2+2' },
              expected: { value: '4' },
              metadata: {
                owner: 'alice',
                'agento11y.sdk.portability': { version: 1, weight: 2, wrapped_fields: ['input', 'expected'] },
              },
            },
          ],
        },
      };
    }
    return {
      status: 200,
      body: {
        suite_id: 'smoke',
        name: 'Smoke',
        description: 'sanity',
        tags: ['nightly'],
        versions: [
          { version: 'v1', published: true, changelog: 'first' },
          { version: 'v2', published: false },
        ],
      },
    };
  });
  try {
    const suite = await newClient(endpoint).pullSuite('smoke');
    assert.equal(suite.suiteId, 'smoke');
    assert.equal(suite.version, 'v1');
    assert.equal(suite.changelog, 'first');
    assert.equal(suite.testCases[0].input, '2+2', 'a wrapped scalar is unwrapped');
    assert.equal(suite.testCases[0].expected, '4');
    assert.equal(suite.testCases[0].weight, 2);
    assert.deepEqual(suite.testCases[0].metadata, { owner: 'alice' }, 'the portability key is stripped');
    assert.equal(
      seen[1].url,
      '/api/plugins/grafana-agento11y-app/resources/eval/test-suites/smoke/versions/v1/test-cases?limit=200',
    );
  } finally {
    await close();
  }
});

test('a local case wraps scalars and records the wrapping', () => {
  const remote = localCaseToRemote({
    testCaseId: 'add',
    input: '2+2',
    expected: '4',
    weight: 2,
    metadata: { owner: 'alice' },
  });
  assert.deepEqual(remote.input, { value: '2+2' });
  assert.deepEqual(remote.expected, { value: '4' });
  assert.deepEqual(remote.metadata['agento11y.sdk.portability'], {
    version: 1,
    weight: 2,
    wrapped_fields: ['input', 'expected'],
  });
  // The round trip returns the caller's scalars.
  const local = remoteCaseToLocal(remote);
  assert.equal(local.input, '2+2');
  assert.equal(local.expected, '4');
  assert.equal(local.weight, 2);
  assert.deepEqual(local.metadata, { owner: 'alice' });
});

test('an object input is stored as-is with no wrapping metadata', () => {
  const remote = localCaseToRemote({ testCaseId: 'add', input: { question: '2+2' } });
  assert.deepEqual(remote.input, { question: '2+2' });
  assert.equal(remote.metadata, undefined);
  assert.deepEqual(remoteCaseToLocal(remote).input, { question: '2+2' });
});

test('a local case without an id or input is rejected', () => {
  assert.throws(() => localCaseToRemote({ testCaseId: '  ', input: 'x' }), /test suite: test_case_id is required/);
  assert.throws(() => localCaseToRemote({ testCaseId: 'a' }), /test suite: input is required/);
});

test('pushSuite creates the suite, opens a draft, upserts cases, and publishes', async () => {
  let suiteExists = false;
  const { endpoint, seen, close } = await startServer((request) => {
    const path = request.url.replace('/api/plugins/grafana-agento11y-app/resources/eval', '');
    if (request.method === 'GET' && path === '/test-suites/smoke') {
      if (!suiteExists) {
        return { status: 404, body: 'suite not found' };
      }
      return { status: 200, body: { suite_id: 'smoke', name: 'Smoke', versions: [] } };
    }
    if (request.method === 'POST' && path === '/test-suites') {
      suiteExists = true;
      return { status: 200, body: { suite_id: 'smoke', name: 'Smoke', versions: [] } };
    }
    if (request.method === 'POST' && path === '/test-suites/smoke/versions') {
      return { status: 200, body: { version: 'v1', published: false } };
    }
    if (request.method === 'POST' && path.endsWith(':publish')) {
      return { status: 200, body: { version: 'v1', published: true } };
    }
    return { status: 200, body: {} };
  });
  try {
    const pushed = await newClient(endpoint).pushSuite(
      {
        suiteId: 'smoke',
        name: 'Smoke',
        description: 'sanity',
        testCases: [{ testCaseId: 'add', input: '2+2', expected: '4' }],
      },
      { publish: true, changelog: 'first cut' },
    );
    assert.equal(pushed.suiteId, 'smoke');
    assert.equal(pushed.suiteVersion, 'v1');
    assert.equal(pushed.published, true);
    assert.deepEqual(pushed.prunedCaseIds, []);
    const paths = seen.map((entry) => `${entry.method} ${entry.url.split('/resources/eval')[1]}`);
    assert.deepEqual(paths, [
      'GET /test-suites/smoke',
      'POST /test-suites',
      'PATCH /test-suites/smoke',
      'GET /test-suites/smoke',
      'POST /test-suites/smoke/versions',
      'POST /test-suites/smoke/versions/v1/test-cases',
      'POST /test-suites/smoke/versions/v1:publish',
    ]);
    assert.deepEqual(JSON.parse(seen[4].body), { changelog: 'first cut' });
  } finally {
    await close();
  }
});

test('pushSuite reuses an existing draft and refuses a different changelog', async () => {
  const { endpoint, close } = await startServer(() => ({
    status: 200,
    body: { suite_id: 'smoke', name: 'Smoke', versions: [{ version: 'v2', published: false, changelog: 'existing' }] },
  }));
  try {
    const client = newClient(endpoint);
    const pushed = await client.pushSuite({ suiteId: 'smoke', name: 'Smoke', testCases: [] });
    assert.equal(pushed.suiteVersion, 'v2');

    await assert.rejects(
      client.pushSuite({ suiteId: 'smoke', name: 'Smoke', testCases: [] }, { changelog: 'different' }),
      /an existing draft cannot apply a different changelog/,
    );
    await assert.rejects(
      client.pushSuite({ suiteId: 'smoke', name: 'Smoke', testCases: [] }, { emptyDraft: true }),
      /empty_draft only applies when creating a new draft/,
    );
  } finally {
    await close();
  }
});

test('pushSuite prunes remote-only cases when asked', async () => {
  const { endpoint, seen, close } = await startServer((request) => {
    const path = request.url.replace('/api/plugins/grafana-agento11y-app/resources/eval', '');
    if (request.method === 'GET' && path.includes('/test-cases')) {
      return {
        status: 200,
        body: {
          items: [
            { test_case_id: 'add', input: {} },
            { test_case_id: 'stale', input: {} },
          ],
        },
      };
    }
    if (request.method === 'GET' && path.startsWith('/test-suites/smoke')) {
      return {
        status: 200,
        body: { suite_id: 'smoke', name: 'Smoke', versions: [{ version: 'v1', published: false }] },
      };
    }
    return { status: 200, body: {} };
  });
  try {
    const pushed = await newClient(endpoint).pushSuite(
      { suiteId: 'smoke', name: 'Smoke', testCases: [{ testCaseId: 'add', input: '2+2' }] },
      { prune: true },
    );
    assert.deepEqual(pushed.prunedCaseIds, ['stale']);
    const deletes = seen.filter((entry) => entry.method === 'DELETE').map((entry) => entry.url);
    assert.deepEqual(deletes, [
      '/api/plugins/grafana-agento11y-app/resources/eval/test-suites/smoke/versions/v1/test-cases/stale',
    ]);
  } finally {
    await close();
  }
});

test('a draft opened by another writer is adopted after a 409', async () => {
  let versionsField = [];
  const { endpoint, close } = await startServer((request) => {
    const path = request.url.replace('/api/plugins/grafana-agento11y-app/resources/eval', '');
    if (request.method === 'POST' && path === '/test-suites/smoke/versions') {
      // Another writer created the draft first.
      versionsField = [{ version: 'v7', published: false }];
      return { status: 409, body: 'a draft already exists' };
    }
    if (request.method === 'GET' && path === '/test-suites/smoke') {
      return { status: 200, body: { suite_id: 'smoke', name: 'Smoke', versions: versionsField } };
    }
    return { status: 200, body: {} };
  });
  try {
    const pushed = await newClient(endpoint).pushSuite({ suiteId: 'smoke', name: 'Smoke', testCases: [] });
    assert.equal(pushed.suiteVersion, 'v7');
  } finally {
    await close();
  }
});

test('a 409 is classified and sent exactly once', async () => {
  const { endpoint, seen, close } = await startServer(() => ({
    status: 409,
    body: 'suite version is already published and is terminal',
  }));
  try {
    await assert.rejects(newClient(endpoint).getSuite('smoke'), (error) => {
      assert.ok(error instanceof ExperimentConflictError);
      assert.equal(error.kind, 'terminal');
      assert.equal(error.recoverable, false);
      return true;
    });
    assert.equal(seen.length, 1, 'a conflict is a caller error, not a transient failure');
  } finally {
    await close();
  }
});

test('a retryable control-plane failure succeeds on the sixth attempt', async () => {
  let calls = 0;
  const delays = [];
  const { endpoint, seen, close } = await startServer(() => {
    calls++;
    return calls <= 5 ? { status: 503, body: 'control plane churn' } : { status: 200, body: { suite_id: 'smoke' } };
  });
  try {
    const client = new TestSuitesClient({
      controlEndpoint: endpoint,
      serviceAccountToken: 'glsa_token',
      env: {},
      sleep: async (durationMs) => {
        delays.push(durationMs);
      },
    });
    const suite = await client.getSuite('smoke');
    assert.deepEqual(suite, { suite_id: 'smoke' });
    assert.equal(seen.length, 6, 'five retries plus the first attempt');
    assert.deepEqual(delays, [100, 200, 400, 800, 1600]);
  } finally {
    await close();
  }
});

test('a blank suite id is rejected before any request', async () => {
  const { endpoint, seen, close } = await startServer(() => ({ status: 200, body: {} }));
  try {
    const client = newClient(endpoint);
    await assert.rejects(client.getSuite('  '), /test suite: suite_id is required/);
    await assert.rejects(client.listCases('smoke', '  '), /test suite: version is required/);
    assert.equal(seen.length, 0);
  } finally {
    await close();
  }
});

function newClient(endpoint) {
  return new TestSuitesClient({
    controlEndpoint: endpoint,
    serviceAccountToken: 'glsa_token',
    env: {},
    sleep: async () => {},
  });
}

function newOfflineClient() {
  return new TestSuitesClient({
    controlEndpoint: 'https://stack.grafana.net',
    serviceAccountToken: 'glsa_token',
    env: {},
  });
}

async function startServer(respond) {
  const seen = [];
  const server = http.createServer((request, response) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      seen.push({
        method: request.method,
        url: request.url,
        headers: request.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      });
      const { status, body } = respond(request);
      if (typeof body === 'string') {
        response.writeHead(status, { 'content-type': 'text/plain' });
        response.end(body);
        return;
      }
      response.writeHead(status, { 'content-type': 'application/json' });
      response.end(JSON.stringify(body ?? {}));
    });
  });
  await new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', (error) => (error ? reject(error) : resolve(undefined)));
  });
  const { port } = server.address();
  return {
    endpoint: `http://127.0.0.1:${port}`,
    seen,
    close: () => new Promise((resolve) => server.close(() => resolve(undefined))),
  };
}
