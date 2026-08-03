import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';

import { ExperimentConflictError } from '../.test-dist/experiments/errors.js';
import {
  experimentReportPath,
  experimentScoresPath,
  runFinalizePath,
  runUpsertPath,
  scoresExportPath,
  trialEvaluatePath,
  trialEvaluationPath,
  trialPath,
  trialsPath,
} from '../.test-dist/experiments/routes.js';
import { requestExperimentsJSON } from '../.test-dist/experiments/transport.js';

test('routes encode every dynamic segment', () => {
  assert.equal(runUpsertPath(), '/api/v1/experiment-runs:upsert');
  assert.equal(runFinalizePath('run-1'), '/api/v1/experiment-runs/run-1:finalize');
  assert.equal(scoresExportPath(), '/api/v1/scores:export');
  assert.equal(trialsPath('run-1'), '/api/v1/experiment-runs/run-1/trials');
  assert.equal(trialPath('run-1', 'trial-1'), '/api/v1/experiment-runs/run-1/trials/trial-1');
  assert.equal(trialEvaluatePath('run-1', 'trial-1'), '/api/v1/experiment-runs/run-1/trials/trial-1:evaluate');
  assert.equal(
    trialEvaluationPath('run-1', 'trial-1', 'eval-1'),
    '/api/v1/experiment-runs/run-1/trials/trial-1/evaluations/eval-1',
  );
  assert.equal(experimentReportPath('run-1'), '/api/v1/eval/experiments/run-1/report');
  assert.equal(experimentScoresPath('run-1'), '/api/v1/eval/experiments/run-1/scores');
});

test('a trial id containing reserved characters cannot shadow the route verb', () => {
  const path = trialEvaluatePath('run/one', 'trial:one/blue');
  assert.equal(path, '/api/v1/experiment-runs/run%2Fone/trials/trial%3Aone%2Fblue:evaluate');
  assert.ok(path.endsWith(':evaluate'));
  assert.equal(path.split(':').length, 2, 'the only literal colon left is the route verb');
  assert.ok(!path.slice(0, -':evaluate'.length).includes(':'));
});

test('the transport sends the encoded path and the JSON body', async () => {
  const seen = [];
  const server = jsonServer(seen, () => ({ status: 200, body: { evaluation_id: 'eval-1' } }));
  const endpoint = await listen(server);
  try {
    const body = await requestExperimentsJSON(
      { endpoint, insecure: true, headers: { Authorization: 'Bearer tok' } },
      {
        method: 'POST',
        path: trialEvaluatePath('run-1', 'trial:one/blue'),
        body: { evaluator_id: 'helpfulness' },
        label: 'trial evaluation trigger',
      },
    );
    assert.deepEqual(body, { evaluation_id: 'eval-1' });
    assert.equal(seen.length, 1);
    assert.equal(seen[0].method, 'POST');
    assert.equal(seen[0].url, '/api/v1/experiment-runs/run-1/trials/trial%3Aone%2Fblue:evaluate');
    assert.equal(seen[0].headers.authorization, 'Bearer tok');
    assert.equal(seen[0].headers['content-type'], 'application/json');
    assert.deepEqual(JSON.parse(seen[0].body), { evaluator_id: 'helpfulness' });
  } finally {
    await close(server);
  }
});

test('a GET request carries the query string and no body', async () => {
  const seen = [];
  const server = jsonServer(seen, () => ({ status: 200, body: { items: [] } }));
  const endpoint = await listen(server);
  try {
    await requestExperimentsJSON(
      { endpoint, insecure: true },
      {
        method: 'GET',
        path: experimentScoresPath('run-1'),
        query: { limit: '50', cursor: 'a b' },
        label: 'experiment scores list',
      },
    );
    assert.equal(seen[0].url, '/api/v1/eval/experiments/run-1/scores?limit=50&cursor=a+b');
    assert.equal(seen[0].body, '');
  } finally {
    await close(server);
  }
});

test('a 503 followed by success resolves after exactly two requests', async () => {
  const seen = [];
  let calls = 0;
  const server = jsonServer(seen, () => {
    calls++;
    return calls === 1 ? { status: 503, body: 'backend churn' } : { status: 200, body: { ok: true } };
  });
  const endpoint = await listen(server);
  try {
    const body = await requestExperimentsJSON(
      { endpoint, insecure: true, retry: { initialBackoffMs: 1, maxBackoffMs: 2 } },
      { method: 'POST', path: runUpsertPath(), body: { name: 'run' }, label: 'experiment create' },
    );
    assert.deepEqual(body, { ok: true });
    assert.equal(seen.length, 2);
  } finally {
    await close(server);
  }
});

test('retryable failures exhaust the budget after four total requests', async () => {
  const seen = [];
  const delays = [];
  const server = jsonServer(seen, () => ({ status: 503, body: 'still down' }));
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        {
          endpoint,
          insecure: true,
          sleep: async (durationMs) => {
            delays.push(durationMs);
          },
        },
        { method: 'POST', path: runUpsertPath(), body: { name: 'run' }, label: 'experiment create' },
      ),
      (error) => {
        assert.match(error.message, /^agento11y experiment transport failed: experiment create: status 503/);
        return true;
      },
    );
    assert.equal(seen.length, 4, 'one attempt plus three retries');
    assert.deepEqual(delays, [100, 200, 400]);
    assert.ok(delays.every((delay) => delay <= 5000));
  } finally {
    await close(server);
  }
});

test('the backoff never exceeds the ceiling', async () => {
  const delays = [];
  const server = jsonServer([], () => ({ status: 429, body: 'slow down' }));
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        {
          endpoint,
          insecure: true,
          retry: { maxRetries: 5, initialBackoffMs: 2_000, maxBackoffMs: 5_000 },
          sleep: async (durationMs) => {
            delays.push(durationMs);
          },
        },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
      ),
      /transport failed/,
    );
    assert.deepEqual(delays, [2000, 4000, 5000, 5000, 5000]);
  } finally {
    await close(server);
  }
});

const statusCases = [
  { status: 400, message: 'agento11y experiment validation failed: experiment create: server said no' },
  { status: 422, message: 'agento11y experiment validation failed: experiment create: server said no' },
  { status: 404, message: 'agento11y experiment not found: experiment create: server said no' },
  {
    status: 418,
    message: 'agento11y experiment transport failed: experiment create: status 418: server said no',
  },
];

test('status codes map onto the stable error prefixes without retrying', async () => {
  for (const { status, message } of statusCases) {
    const seen = [];
    const server = jsonServer(seen, () => ({ status, body: 'server said no' }));
    const endpoint = await listen(server);
    try {
      await assert.rejects(
        requestExperimentsJSON(
          { endpoint, insecure: true, retry: { initialBackoffMs: 1 } },
          { method: 'POST', path: runUpsertPath(), body: {}, label: 'experiment create' },
        ),
        (error) => {
          assert.equal(error.message, message);
          return true;
        },
      );
      assert.equal(seen.length, 1, `status ${status} must not retry`);
    } finally {
      await close(server);
    }
  }
});

test('a 409 is classified and not retried', async () => {
  const seen = [];
  const server = jsonServer(seen, () => ({ status: 409, body: 'score_count mismatch: expected 4 scores, found 5' }));
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true },
        { method: 'POST', path: runFinalizePath('run-1'), body: {}, label: 'experiment finalize' },
      ),
      (error) => {
        assert.ok(error instanceof ExperimentConflictError);
        assert.equal(error.kind, 'score_count_mismatch');
        assert.equal(error.recoverable, true);
        assert.match(error.message, /^agento11y experiment conflict: experiment finalize:/);
        return true;
      },
    );
    assert.equal(seen.length, 1);
  } finally {
    await close(server);
  }
});

test('a 503 naming a capability gap is not retried', async () => {
  const seen = [];
  const server = http.createServer((request, response) => {
    seen.push(request.url);
    response.writeHead(503, { 'content-type': 'text/plain; charset=utf-8' });
    response.end('trial evaluation service is unavailable');
  });
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true, retry: { initialBackoffMs: 1 } },
        { method: 'POST', path: trialEvaluatePath('run-1', 'trial-1'), body: {}, label: 'trial evaluation trigger' },
      ),
      /transport failed: trial evaluation trigger: status 503/,
    );
    assert.equal(seen.length, 1, 'a capability gap answers the same way on every retry');
  } finally {
    await close(server);
  }
});

test('an empty success body decodes to an empty object', async () => {
  const server = http.createServer((_request, response) => {
    response.writeHead(204);
    response.end();
  });
  const endpoint = await listen(server);
  try {
    const body = await requestExperimentsJSON(
      { endpoint, insecure: true },
      { method: 'DELETE', path: trialPath('run-1', 'trial-1'), label: 'test case trial update' },
    );
    assert.deepEqual(body, {});
  } finally {
    await close(server);
  }
});

test('a malformed success body fails with the transport prefix', async () => {
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end('{not json');
  });
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
      ),
      /^Error: agento11y experiment transport failed: experiment report: invalid JSON response/,
    );
  } finally {
    await close(server);
  }
});

test('a response above the body cap is abandoned', async () => {
  const oversized = 'x'.repeat((8 << 20) + 16);
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(`"${oversized}"`);
  });
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
      ),
      /transport failed: .*response too large/,
    );
  } finally {
    await close(server);
  }
});

test('a caller abort surfaces its own reason, not a transport error', async () => {
  const server = http.createServer(() => {
    // Never answers: the caller's abort is the only thing that ends the request.
  });
  const endpoint = await listen(server);
  const controller = new AbortController();
  const stop = new Error('stop');
  try {
    const pending = requestExperimentsJSON(
      { endpoint, insecure: true },
      { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report', signal: controller.signal },
    );
    setTimeout(() => controller.abort(stop), 20);
    await assert.rejects(pending, (error) => {
      assert.equal(error, stop);
      return true;
    });
  } finally {
    server.closeAllConnections?.();
    await close(server);
  }
});

test('an already aborted signal sends no request at all', async () => {
  const seen = [];
  const server = jsonServer(seen, () => ({ status: 200, body: {} }));
  const endpoint = await listen(server);
  const controller = new AbortController();
  const stop = new Error('too late');
  controller.abort(stop);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report', signal: controller.signal },
      ),
      (error) => {
        assert.equal(error, stop);
        return true;
      },
    );
    assert.equal(seen.length, 0);
  } finally {
    await close(server);
  }
});

test('a per-attempt timeout is retried and reported as a transport failure', async () => {
  const seen = [];
  const server = http.createServer((request) => {
    seen.push(request.url);
    // Hangs so the per-attempt timeout is the only thing that ends the attempt.
  });
  const endpoint = await listen(server);
  try {
    await assert.rejects(
      requestExperimentsJSON(
        {
          endpoint,
          insecure: true,
          retry: { maxRetries: 1, timeoutMs: 30 },
          sleep: async () => {},
        },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
      ),
      /^Error: agento11y experiment transport failed: experiment report:/,
    );
    assert.equal(seen.length, 2);
  } finally {
    server.closeAllConnections?.();
    await close(server);
  }
});

test('a raw byte body sets the caller content type', async () => {
  const seen = [];
  const server = jsonServer(seen, () => ({ status: 200, body: { artifact_id: 'art-1' } }));
  const endpoint = await listen(server);
  try {
    await requestExperimentsJSON(
      { endpoint, insecure: true },
      {
        method: 'POST',
        path: `${trialPath('run-1', 'trial-1')}/artifacts:upload`,
        query: { name: 'transcript.txt', kind: 'text', mime: 'text/plain' },
        bytes: new TextEncoder().encode('hello'),
        contentType: 'text/plain',
        label: 'trial artifact upload',
      },
    );
    assert.equal(seen[0].headers['content-type'], 'text/plain');
    assert.equal(seen[0].body, 'hello');
    assert.equal(
      seen[0].url,
      '/api/v1/experiment-runs/run-1/trials/trial-1/artifacts:upload?name=transcript.txt&kind=text&mime=text%2Fplain',
    );
  } finally {
    await close(server);
  }
});

test('a stalled response body still hits the per-attempt timeout', async () => {
  // The other timeout test never answers, so it only exercises the header phase.
  // Here the headers arrive and the body stalls, which is the case that used to
  // hang: the timeout has to stay armed while the body is read.
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.write('{"evaluation_id":');
  });
  const endpoint = await listen(server);
  const started = Date.now();
  try {
    await assert.rejects(
      requestExperimentsJSON(
        { endpoint, insecure: true, retry: { maxRetries: 0, timeoutMs: 50 } },
        { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
      ),
      /^Error: agento11y experiment transport failed: experiment report:/,
    );
    assert.ok(Date.now() - started < 2_000, 'the call must not wait for the stalled body');
  } finally {
    server.closeAllConnections?.();
    await close(server);
  }
});

test('a caller abort during a stalled response body surfaces its own reason', async () => {
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.write('{"evaluation_id":');
  });
  const endpoint = await listen(server);
  const controller = new AbortController();
  const stop = new Error('stop');
  try {
    const pending = requestExperimentsJSON(
      { endpoint, insecure: true, retry: { maxRetries: 0, timeoutMs: 30_000 } },
      { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report', signal: controller.signal },
    );
    setTimeout(() => controller.abort(stop), 20);
    await assert.rejects(pending, (error) => {
      assert.equal(error, stop);
      return true;
    });
  } finally {
    server.closeAllConnections?.();
    await close(server);
  }
});

test('a blank endpoint fails before any request', async () => {
  await assert.rejects(
    requestExperimentsJSON(
      { endpoint: '  ', insecure: true },
      { method: 'GET', path: experimentReportPath('run-1'), label: 'experiment report' },
    ),
    /agento11y experiment transport failed: api endpoint is required/,
  );
});

function jsonServer(seen, respond) {
  return http.createServer((request, response) => {
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
      response.end(JSON.stringify(body));
    });
  });
}

async function listen(server) {
  await new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', (error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(undefined);
    });
  });
  const address = server.address();
  return `http://127.0.0.1:${address.port}`;
}

async function close(server) {
  await new Promise((resolve) => {
    server.close(() => resolve(undefined));
  });
}
