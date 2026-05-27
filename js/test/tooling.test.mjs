import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

test('sdk js package declares local TypeScript compiler tooling', async () => {
  const packageJsonPath = path.join(__dirname, '..', 'package.json');
  const packageJsonRaw = await readFile(packageJsonPath, 'utf8');
  const packageJson = JSON.parse(packageJsonRaw);

  assert.equal(
    typeof packageJson.devDependencies?.typescript,
    'string',
    'sdks/js must declare a local typescript dependency',
  );
});

test('sdk js core package keeps provider and framework dependencies out of default install', async () => {
  const packageJsonPath = path.join(__dirname, '..', '..', 'js-core', 'package.json');
  const packageJsonRaw = await readFile(packageJsonPath, 'utf8');
  const packageJson = JSON.parse(packageJsonRaw);

  assert.deepEqual(Object.keys(packageJson.dependencies ?? {}).sort(), ['@opentelemetry/api']);
  assert.equal(packageJson.peerDependenciesMeta?.['@grpc/grpc-js']?.optional, true);
  assert.equal(packageJson.peerDependenciesMeta?.['@grpc/proto-loader']?.optional, true);

  for (const dependencyName of [
    '@anthropic-ai/sdk',
    '@google/adk',
    '@google/genai',
    '@langchain/core',
    '@langchain/langgraph',
    '@openai/agents',
    '@opentelemetry/sdk-metrics',
    '@opentelemetry/sdk-trace-base',
    'llamaindex',
    'openai',
  ]) {
    assert.equal(
      packageJson.dependencies?.[dependencyName],
      undefined,
      `${dependencyName} should not be a default dependency of @grafana/sigil-sdk-js-core`,
    );
  }
});
