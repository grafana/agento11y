// Guard behavior of the JS SDK against a real local HTTP server.
//
// The other hook tests assert on fields of the captured body they already expect,
// so an encoding the server cannot read looks identical to a correct one. These
// cases run the full transport and compare each captured body with
// `conformance/hooks/request-preflight.json`.

import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { Agento11yClient, defaultConfig } from '../.test-dist/index.js';
import { diffJson, loadPreflightRequest, loadResponses, preflightRequest } from './hooksFixtures.mjs';

const hooksEvaluatePath = '/api/v1/hooks:evaluate';
const responses = loadResponses();

/**
 * Starts a local server whose status, body, and delay come from a per-request
 * responder. Same programmable model as `plugins/pi/src/testHttp.ts`, kept local
 * so the SDK suite does not depend on the pi plugin.
 */
async function startHookServer(respond) {
  const state = { requests: [], inFlight: 0, maxInFlight: 0, errors: [] };
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) {
      chunks.push(chunk);
    }
    const raw = Buffer.concat(chunks).toString('utf8');
    const call = { path: request.url ?? '', headers: { ...request.headers }, raw, payload: undefined };
    try {
      call.payload = JSON.parse(raw);
    } catch {
      call.payload = undefined;
    }
    state.requests.push(call);
    state.inFlight += 1;
    state.maxInFlight = Math.max(state.maxInFlight, state.inFlight);

    const out = respond(call) ?? {};
    const send = () => {
      state.inFlight -= 1;
      try {
        response.writeHead(out.status ?? 200, { 'content-type': 'application/json' });
        response.end(out.body ?? JSON.stringify({ action: 'allow', evaluations: [] }));
      } catch (error) {
        // A client that aborted on timeout leaves a torn-down response here.
        // Record it: the timeout cases expect it, and every other case asserts
        // this list is empty.
        state.errors.push(`could not write the response: ${String(error)}`);
      }
    };
    response.on('error', (error) => {
      state.errors.push(`response stream error: ${String(error)}`);
    });
    if (out.delayMs !== undefined && out.delayMs > 0) {
      setTimeout(send, out.delayMs).unref();
      return;
    }
    send();
  });

  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return {
    ...state,
    requests: state.requests,
    errors: state.errors,
    baseUrl: `http://127.0.0.1:${port}`,
    get maxInFlight() {
      return state.maxInFlight;
    },
    async close() {
      server.closeAllConnections?.();
      await new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
    },
  };
}

function respondWith(status, body) {
  return () => ({ status, body });
}

function newClient(options) {
  const warnings = [];
  const client = new Agento11yClient({
    ...defaultConfig(),
    api: { endpoint: options.baseUrl },
    generationExport: {
      ...defaultConfig().generationExport,
      protocol: 'http',
      endpoint: `${options.baseUrl}/api/v1/generations:export`,
      insecure: true,
      auth: options.auth ?? { mode: 'none' },
    },
    hooks: {
      enabled: options.enabled ?? true,
      phases: options.phases ?? ['preflight'],
      timeoutMs: options.timeoutMs ?? 5_000,
      failOpen: options.failOpen ?? true,
    },
    logger: {
      warn: (message) => warnings.push(String(message)),
      error: () => {},
      debug: () => {},
    },
    generationExporter: { exportGenerations: async () => ({ results: [] }), shutdown: async () => {} },
  });
  return { client, warnings };
}

function assertPreflightRequest(server, { expectResponseWritten = true } = {}) {
  assert.equal(server.requests.length, 1, `expected exactly one hook request, got ${server.requests.length}`);
  if (expectResponseWritten) {
    // A server that failed to answer makes the client fail open, and a fail-open
    // assertion passes whether or not the response was ever sent.
    assert.deepEqual(server.errors, [], `the test server could not answer: ${server.errors.join('; ')}`);
  }
  const [call] = server.requests;
  assert.equal(call.path, hooksEvaluatePath);
  assert.equal(call.headers['content-type'], 'application/json');
  const diffs = diffJson(call.payload, loadPreflightRequest());
  assert.deepEqual(diffs, [], `captured request body does not match the shared fixture:\n${diffs.join('\n')}`);
}

function assertFailOpenWarning(warnings, wantSubstring) {
  const matched = warnings.filter((line) => line.includes('allowing request (failOpen)'));
  assert.ok(matched.length > 0, `a swallowed hook failure must be logged, got ${JSON.stringify(warnings)}`);
  if (wantSubstring !== undefined) {
    assert.ok(
      matched.some((line) => line.includes(wantSubstring)),
      `warning does not mention ${wantSubstring}: ${JSON.stringify(matched)}`,
    );
  }
}

for (const failOpen of [true, false]) {
  const policy = failOpen ? 'fail-open' : 'fail-closed';

  test(`allow proceeds under ${policy}`, async () => {
    const server = await startHookServer(respondWith(200, JSON.stringify(responses.allow)));
    const { client } = newClient({ baseUrl: server.baseUrl, failOpen });
    try {
      const response = await client.evaluateHook(preflightRequest());
      assert.equal(response.action, 'allow');
      assert.equal(response.evaluations[0].ruleId, 'pii-detect');
      assertPreflightRequest(server);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });

  test(`deny is enforced under ${policy}`, async () => {
    const server = await startHookServer(respondWith(200, JSON.stringify(responses.deny)));
    const { client } = newClient({ baseUrl: server.baseUrl, failOpen });
    try {
      const response = await client.evaluateHook(preflightRequest());
      assert.equal(response.action, 'deny');
      assert.equal(response.ruleId, 'block-destructive-bash');
      assert.equal(response.reason, 'Bash(*rm*) is not allowed in this environment');
      assertPreflightRequest(server);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });

  test(`an unconfigured phase sends no request under ${policy}`, async () => {
    const server = await startHookServer(respondWith(500, 'should not be called'));
    const { client } = newClient({ baseUrl: server.baseUrl, failOpen, phases: ['postflight'] });
    try {
      const response = await client.evaluateHook(preflightRequest());
      assert.equal(response.action, 'allow');
      assert.equal(server.requests.length, 0);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });

  test(`disabled hooks send no request under ${policy}`, async () => {
    const server = await startHookServer(respondWith(500, 'should not be called'));
    const { client } = newClient({ baseUrl: server.baseUrl, failOpen, enabled: false });
    try {
      const response = await client.evaluateHook(preflightRequest());
      assert.equal(response.action, 'allow');
      assert.equal(server.requests.length, 0);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });
}

for (const status of [429, 503]) {
  test(`status ${status} fails open with a warning`, async () => {
    const server = await startHookServer(respondWith(status, 'upstream unavailable'));
    const { client, warnings } = newClient({ baseUrl: server.baseUrl, failOpen: true });
    try {
      const response = await client.evaluateHook(preflightRequest());
      assert.equal(response.action, 'allow');
      assertFailOpenWarning(warnings, `status ${status}`);
      assertPreflightRequest(server);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });

  test(`status ${status} fails closed`, async () => {
    const server = await startHookServer(respondWith(status, 'upstream unavailable'));
    const { client } = newClient({ baseUrl: server.baseUrl, failOpen: false });
    try {
      await assert.rejects(() => client.evaluateHook(preflightRequest()), /hook evaluation failed: status/);
      assertPreflightRequest(server);
    } finally {
      await client.shutdown();
      await server.close();
    }
  });
}

test('a malformed response body fails open with a warning', async () => {
  const server = await startHookServer(respondWith(200, '{"action": "allow"'));
  const { client, warnings } = newClient({ baseUrl: server.baseUrl, failOpen: true });
  try {
    const response = await client.evaluateHook(preflightRequest());
    assert.equal(response.action, 'allow');
    assertFailOpenWarning(warnings, 'invalid JSON response');
    assertPreflightRequest(server);
  } finally {
    await client.shutdown();
    await server.close();
  }
});

test('a malformed response body fails closed', async () => {
  const server = await startHookServer(respondWith(200, '{"action": "allow"'));
  const { client } = newClient({ baseUrl: server.baseUrl, failOpen: false });
  try {
    await assert.rejects(() => client.evaluateHook(preflightRequest()), /invalid JSON response/);
    assertPreflightRequest(server);
  } finally {
    await client.shutdown();
    await server.close();
  }
});

const slowResponseDelayMs = 2_000;
const slowResponse = () => ({
  status: 200,
  body: JSON.stringify({ action: 'deny', rule_id: 'late' }),
  delayMs: slowResponseDelayMs,
});

test('a response slower than the client timeout fails open with a warning', async () => {
  const server = await startHookServer(slowResponse);
  const { client, warnings } = newClient({ baseUrl: server.baseUrl, failOpen: true, timeoutMs: 250 });
  try {
    const started = Date.now();
    const response = await client.evaluateHook(preflightRequest());
    const elapsed = Date.now() - started;
    assert.equal(response.action, 'allow');
    assert.ok(elapsed < slowResponseDelayMs, `client waited ${elapsed}ms, so it did not enforce its own timeout`);
    assertFailOpenWarning(warnings);
    // The client is gone by the time the delayed write runs, so a write error is
    // the case under test rather than a broken server.
    assertPreflightRequest(server, { expectResponseWritten: false });
  } finally {
    await client.shutdown();
    await server.close();
  }
});

test('a response slower than the client timeout fails closed', async () => {
  const server = await startHookServer(slowResponse);
  const { client } = newClient({ baseUrl: server.baseUrl, failOpen: false, timeoutMs: 250 });
  try {
    const started = Date.now();
    await assert.rejects(
      () => client.evaluateHook(preflightRequest()),
      /hook evaluation failed/,
      'a timeout must surface as a hook transport failure, not as any thrown error',
    );
    const elapsed = Date.now() - started;
    assert.ok(elapsed < slowResponseDelayMs, `client waited ${elapsed}ms, so it did not enforce its own timeout`);
    assertPreflightRequest(server, { expectResponseWritten: false });
  } finally {
    await client.shutdown();
    await server.close();
  }
});

test('configured auth reaches the server', async () => {
  const server = await startHookServer(respondWith(200, JSON.stringify(responses.allow)));
  const { client } = newClient({
    baseUrl: server.baseUrl,
    auth: { mode: 'basic', basicUser: '12345', basicPassword: 'glc-token', tenantId: '12345' },
  });
  try {
    await client.evaluateHook(preflightRequest());
    const [call] = server.requests;
    assert.equal(call.headers.authorization, `Basic ${Buffer.from('12345:glc-token', 'utf8').toString('base64')}`);
    assert.equal(call.headers['x-scope-orgid'], '12345');
    assert.equal(call.headers['x-agento11y-hook-timeout-ms'], '5000');
    assertPreflightRequest(server);
  } finally {
    await client.shutdown();
    await server.close();
  }
});

test('concurrent evaluations are not serialized', async () => {
  // A guard on the request path must not funnel every caller through one
  // connection.
  const server = await startHookServer(() => ({
    status: 200,
    body: JSON.stringify(responses.allow),
    delayMs: 200,
  }));
  const { client } = newClient({ baseUrl: server.baseUrl });
  try {
    const results = await Promise.all([0, 1, 2, 3].map(() => client.evaluateHook(preflightRequest())));
    assert.deepEqual(
      results.map((r) => r.action),
      ['allow', 'allow', 'allow', 'allow'],
    );
    assert.ok(server.maxInFlight > 1, `evaluations were serialized: peak in-flight ${server.maxInFlight}`);
    assert.deepEqual(server.errors, []);
    const fixture = loadPreflightRequest();
    for (const call of server.requests) {
      assert.deepEqual(diffJson(call.payload, fixture), []);
    }
  } finally {
    await client.shutdown();
    await server.close();
  }
});
