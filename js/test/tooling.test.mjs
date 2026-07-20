import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

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
      `${dependencyName} should not be a default dependency of @grafana/agento11y-core`,
    );
  }
});

test('core entrypoint loads in runtimes without process or Buffer globals', () => {
  // The core package promises to load on edge-like runtimes where Node-only
  // globals (process, Buffer) and Node built-in modules (node:async_hooks,
  // node:crypto) are not available. The TS build emits the same source under
  // .test-dist, so we point the smoke test at that compiled entry to avoid
  // depending on the published dist layout.
  const coreEntry = path.join(__dirname, '..', '.test-dist', 'core.js');
  const coreEntryUrl = pathToFileURL(coreEntry).href;
  const script = `
    delete globalThis.process;
    delete globalThis.Buffer;
    const mod = await import(${JSON.stringify(coreEntryUrl)});
    if (typeof mod.SigilClient !== 'function') {
      throw new Error('SigilClient missing from core export');
    }
    if (typeof mod.createSigilClient !== 'function') {
      throw new Error('createSigilClient missing from core export');
    }
    // Constructing the default-config client must not touch Buffer or read
    // process.env on its hot path.
    new mod.SigilClient({ generationExport: { protocol: 'none', endpoint: 'http://localhost' } });
  `;

  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script], {
    encoding: 'utf8',
  });

  assert.equal(
    result.status,
    0,
    `core entrypoint failed to load without process/Buffer.\nstdout: ${result.stdout}\nstderr: ${result.stderr}`,
  );
});
