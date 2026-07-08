import assert from 'node:assert/strict';
import test from 'node:test';

import { normalizeHTTPGenerationEndpoint, normalizeHTTPWorkflowStepEndpoint } from '../.test-dist/exporters/http.js';

const cases = [
  {
    name: 'missing path appends default ingest path',
    input: 'http://localhost:8080',
    want: 'http://localhost:8080/api/v1/generations:export',
  },
  {
    name: 'trailing slash treated as missing path',
    input: 'http://localhost:8080/',
    want: 'http://localhost:8080/api/v1/generations:export',
  },
  {
    name: 'explicit ingest path is preserved',
    input: 'http://localhost:8080/api/v1/generations:export',
    want: 'http://localhost:8080/api/v1/generations:export',
  },
  {
    name: 'custom path is preserved',
    input: 'http://localhost:8080/custom/ingest',
    want: 'http://localhost:8080/custom/ingest',
  },
  {
    name: 'no scheme defaults to http and appends path',
    input: 'localhost:8080',
    want: 'http://localhost:8080/api/v1/generations:export',
  },
  {
    name: 'https with no path appends default ingest path',
    input: 'https://stack.grafana.net',
    want: 'https://stack.grafana.net/api/v1/generations:export',
  },
  {
    name: 'uppercase scheme normalized to lowercase',
    input: 'HTTPS://stack.grafana.net',
    want: 'https://stack.grafana.net/api/v1/generations:export',
  },
  {
    name: 'query string preserved when path appended',
    input: 'http://localhost:8080?token=abc',
    want: 'http://localhost:8080/api/v1/generations:export?token=abc',
  },
];

for (const tc of cases) {
  test(`normalizeHTTPGenerationEndpoint: ${tc.name}`, () => {
    assert.equal(normalizeHTTPGenerationEndpoint(tc.input), tc.want);
  });
}

test('normalizeHTTPGenerationEndpoint: empty input throws', () => {
  assert.throws(() => normalizeHTTPGenerationEndpoint(''), /endpoint is required/);
  assert.throws(() => normalizeHTTPGenerationEndpoint('   '), /endpoint is required/);
});

test('normalizeHTTPWorkflowStepEndpoint: appends and rewrites workflow-step path', () => {
  assert.equal(
    normalizeHTTPWorkflowStepEndpoint('http://localhost:8080'),
    'http://localhost:8080/api/v1/workflow-steps:export',
  );
  assert.equal(
    normalizeHTTPWorkflowStepEndpoint('http://localhost:8080/api/v1/generations:export'),
    'http://localhost:8080/api/v1/workflow-steps:export',
  );
  assert.equal(
    normalizeHTTPWorkflowStepEndpoint('http://localhost:8080/api/v1/generations:export/'),
    'http://localhost:8080/api/v1/workflow-steps:export',
  );
  assert.equal(
    normalizeHTTPWorkflowStepEndpoint('http://localhost:8080/custom/ingest'),
    'http://localhost:8080/custom/ingest',
  );
});
