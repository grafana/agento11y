import assert from 'node:assert/strict';
import test from 'node:test';
import { Agento11yClient } from '../.test-dist/index.js';

// Auth configs that produce specific headers. resolveHeadersWithAuth tolerates
// mode-irrelevant fields silently (env layering can populate any field
// regardless of mode), so these cases never throw.
const headerCases = [
  {
    name: 'basic mode derives basic_user from tenantId',
    auth: { mode: 'basic', tenantId: '42', basicPassword: 'secret' },
    want: {
      Authorization: `Basic ${btoa('42:secret')}`,
      'X-Scope-OrgID': '42',
    },
  },
  {
    name: 'basic mode uses basicUser over tenantId for credential',
    auth: { mode: 'basic', tenantId: '42', basicUser: 'probe-user', basicPassword: 'secret' },
    want: {
      Authorization: `Basic ${btoa('probe-user:secret')}`,
      'X-Scope-OrgID': '42',
    },
  },
  {
    name: 'basic mode handles non-ASCII via UTF-8',
    auth: { mode: 'basic', basicUser: 'ユーザー', basicPassword: 'パスワード' },
    want: {
      Authorization: `Basic ${Buffer.from('ユーザー:パスワード', 'utf-8').toString('base64')}`,
    },
  },
  {
    name: 'none mode silently ignores extra credentials',
    auth: { mode: 'none', basicPassword: 'secret', basicUser: 'user' },
    want: {},
  },
  {
    name: 'bearer mode silently ignores tenantId',
    auth: { mode: 'bearer', bearerToken: 'tok', tenantId: 'ignored' },
    want: { Authorization: 'Bearer tok' },
  },
];

for (const tc of headerCases) {
  test(`auth config: ${tc.name}`, () => {
    const client = new Agento11yClient({
      generationExport: { auth: tc.auth },
    });
    try {
      const headers = client.config.generationExport.headers ?? {};
      for (const [k, v] of Object.entries(tc.want)) {
        assert.equal(headers[k], v, `header ${k}`);
      }
    } finally {
      client.shutdown();
    }
  });
}

// Auth configs that throw at client init: missing required fields for the
// declared mode. These are caller programming errors that surface loudly.
const throwCases = [
  {
    name: 'tenant mode requires tenantId',
    auth: { mode: 'tenant' },
    pattern: /requires tenantId/,
  },
  {
    name: 'basic mode requires basicPassword',
    auth: { mode: 'basic', tenantId: '42' },
    pattern: /requires basicPassword/,
  },
  {
    name: 'basic mode requires basicUser or tenantId',
    auth: { mode: 'basic', basicPassword: 'secret' },
    pattern: /requires basicUser or tenantId/,
  },
];

for (const tc of throwCases) {
  test(`auth config throws: ${tc.name}`, () => {
    assert.throws(() => new Agento11yClient({ generationExport: { auth: tc.auth } }), tc.pattern);
  });
}

test('explicit headers win over auth-derived headers', () => {
  const client = new Agento11yClient({
    generationExport: {
      headers: {
        Authorization: 'Basic override',
        'X-Scope-OrgID': 'override-tenant',
      },
      auth: { mode: 'basic', tenantId: '42', basicPassword: 'secret' },
    },
  });
  try {
    assert.equal(client.config.generationExport.headers?.Authorization, 'Basic override');
    assert.equal(client.config.generationExport.headers?.['X-Scope-OrgID'], 'override-tenant');
  } finally {
    client.shutdown();
  }
});
