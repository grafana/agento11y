import assert from 'node:assert/strict';
import test from 'node:test';

import { Agento11yClient, configFromEnv, mergeConfig } from '../.test-dist/index.js';

// configFromEnv table: env-only resolution with no caller-supplied config.
const configFromEnvCases = [
  {
    name: 'no env returns defaults',
    env: {},
    check: (cfg) => {
      assert.equal(cfg.generationExport.protocol, 'http');
      assert.equal(cfg.contentCapture, 'default');
      assert.equal(cfg.agentName, undefined);
      assert.equal(cfg.userId, undefined);
      assert.equal(cfg.debug, undefined);
    },
  },
  {
    name: 'transport from env',
    env: { SIGIL_ENDPOINT: 'https://env:4318', SIGIL_PROTOCOL: 'grpc' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'https://env:4318');
      assert.equal(cfg.generationExport.protocol, 'grpc');
    },
  },
  {
    name: 'basic auth from env',
    env: {
      SIGIL_AUTH_MODE: 'basic',
      SIGIL_AUTH_TENANT_ID: '42',
      SIGIL_AUTH_TOKEN: 'glc_xxx',
    },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'basic');
      assert.equal(cfg.generationExport.auth.tenantId, '42');
      assert.equal(cfg.generationExport.auth.basicUser, '42');
      assert.equal(cfg.generationExport.auth.basicPassword, 'glc_xxx');
    },
  },
  {
    name: 'bearer auth from env',
    env: { SIGIL_AUTH_MODE: 'bearer', SIGIL_AUTH_TOKEN: 'tok' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'bearer');
      assert.equal(cfg.generationExport.auth.bearerToken, 'tok');
    },
  },
  {
    name: 'agent / user / tags / debug from env',
    env: {
      SIGIL_AGENT_NAME: 'planner',
      SIGIL_AGENT_VERSION: '1.2.3',
      SIGIL_USER_ID: 'alice',
      SIGIL_TAGS: 'service=orchestrator,env=prod',
      SIGIL_DEBUG: 'true',
    },
    check: (cfg) => {
      assert.equal(cfg.agentName, 'planner');
      assert.equal(cfg.agentVersion, '1.2.3');
      assert.equal(cfg.userId, 'alice');
      assert.deepEqual(cfg.tags, { service: 'orchestrator', env: 'prod' });
      assert.equal(cfg.debug, true);
    },
  },
  {
    name: 'insecure default is false',
    env: {},
    check: (cfg) => {
      assert.equal(cfg.generationExport.insecure, false);
    },
  },
  {
    name: 'invalid content_capture_mode falls back to default',
    env: { SIGIL_CONTENT_CAPTURE_MODE: 'bogus' },
    check: (cfg) => {
      assert.equal(cfg.contentCapture, 'default');
    },
  },
  {
    name: 'legacy "default" content_capture_mode falls back to default',
    env: { SIGIL_CONTENT_CAPTURE_MODE: 'default' },
    check: (cfg) => {
      assert.equal(cfg.contentCapture, 'default');
    },
  },
  {
    name: 'full_with_metadata_spans content_capture_mode from env',
    env: { SIGIL_CONTENT_CAPTURE_MODE: 'full_with_metadata_spans' },
    check: (cfg) => {
      assert.equal(cfg.contentCapture, 'full_with_metadata_spans');
    },
  },
  {
    name: 'invalid auth mode preserves valid env siblings',
    env: {
      SIGIL_AUTH_MODE: 'Bearrer',
      SIGIL_ENDPOINT: 'valid.example:4318',
      SIGIL_AGENT_NAME: 'valid-agent',
    },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'none');
      assert.equal(cfg.generationExport.endpoint, 'valid.example:4318');
      assert.equal(cfg.agentName, 'valid-agent');
    },
  },
  {
    // configFromEnv must use the supplied env, not process.env. Poisoning
    // process.env here would corrupt the result if the supplied env were ignored.
    name: 'uses supplied env, ignores process.env',
    env: { SIGIL_ENDPOINT: 'supplied.example:4318' },
    setupAmbient: { SIGIL_ENDPOINT: 'ambient.example:4318', SIGIL_AGENT_NAME: 'ambient-agent' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'supplied.example:4318');
      // Ambient values must not leak through.
      assert.equal(cfg.agentName, undefined);
    },
  },
  {
    name: 'preferred AGENTO11Y_* spelling matches legacy',
    env: {
      AGENTO11Y_ENDPOINT: 'https://env:4318',
      AGENTO11Y_PROTOCOL: 'grpc',
      AGENTO11Y_INSECURE: 'true',
      AGENTO11Y_HEADERS: 'X-A=1',
      AGENTO11Y_AUTH_MODE: 'basic',
      AGENTO11Y_AUTH_TENANT_ID: '42',
      AGENTO11Y_AUTH_TOKEN: 'glc_xxx',
      AGENTO11Y_AGENT_NAME: 'planner',
      AGENTO11Y_AGENT_VERSION: '1.2.3',
      AGENTO11Y_USER_ID: 'alice',
      AGENTO11Y_TAGS: 'service=orchestrator,env=prod',
      AGENTO11Y_CONTENT_CAPTURE_MODE: 'metadata_only',
      AGENTO11Y_DEBUG: 'true',
    },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'https://env:4318');
      assert.equal(cfg.generationExport.protocol, 'grpc');
      assert.equal(cfg.generationExport.insecure, true);
      assert.equal(cfg.generationExport.headers['X-A'], '1');
      assert.equal(cfg.generationExport.auth.mode, 'basic');
      assert.equal(cfg.generationExport.auth.tenantId, '42');
      assert.equal(cfg.generationExport.auth.basicUser, '42');
      assert.equal(cfg.generationExport.auth.basicPassword, 'glc_xxx');
      assert.equal(cfg.agentName, 'planner');
      assert.equal(cfg.agentVersion, '1.2.3');
      assert.equal(cfg.userId, 'alice');
      assert.deepEqual(cfg.tags, { service: 'orchestrator', env: 'prod' });
      assert.equal(cfg.contentCapture, 'metadata_only');
      assert.equal(cfg.debug, true);
    },
  },
  {
    name: 'preferred wins over legacy',
    env: { AGENTO11Y_ENDPOINT: 'preferred.example:4318', SIGIL_ENDPOINT: 'legacy.example:4318' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'preferred.example:4318');
    },
  },
  {
    name: 'blank preferred falls through to legacy',
    env: { AGENTO11Y_ENDPOINT: '   ', SIGIL_ENDPOINT: 'legacy.example:4318' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'legacy.example:4318');
    },
  },
  {
    name: 'invalid preferred content_capture_mode does not fall back to valid legacy',
    env: { AGENTO11Y_CONTENT_CAPTURE_MODE: 'bogus', SIGIL_CONTENT_CAPTURE_MODE: 'full' },
    check: (cfg) => {
      assert.equal(cfg.contentCapture, 'default');
    },
  },
  {
    name: 'mixed-prefix auth resolves per field',
    env: {
      AGENTO11Y_AUTH_MODE: 'basic',
      SIGIL_AUTH_TENANT_ID: '42',
      AGENTO11Y_AUTH_TOKEN: 'glc_xxx',
    },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'basic');
      assert.equal(cfg.generationExport.auth.tenantId, '42');
      assert.equal(cfg.generationExport.auth.basicUser, '42');
      assert.equal(cfg.generationExport.auth.basicPassword, 'glc_xxx');
    },
  },
  {
    name: 'preferred TAGS replaces legacy TAGS entirely',
    env: { AGENTO11Y_TAGS: 'team=ai', SIGIL_TAGS: 'service=orch,env=prod' },
    check: (cfg) => {
      assert.deepEqual(cfg.tags, { team: 'ai' });
    },
  },
];

for (const tc of configFromEnvCases) {
  test(`configFromEnv: ${tc.name}`, () => {
    const original = process.env;
    if (tc.setupAmbient) {
      process.env = { ...original, ...tc.setupAmbient };
    }
    try {
      const cfg = configFromEnv(tc.env);
      tc.check(cfg);
    } finally {
      if (tc.setupAmbient) {
        process.env = original;
      }
    }
  });
}

// mergeConfig table: caller config layered over env, with explicit precedence.
const mergeConfigCases = [
  {
    name: 'explicit caller wins over env',
    config: { generationExport: { endpoint: 'https://explicit:4318' } },
    env: { SIGIL_ENDPOINT: 'https://env:4318', SIGIL_AGENT_NAME: 'planner' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'https://explicit:4318');
      // Unset fields still come from env.
      assert.equal(cfg.agentName, 'planner');
    },
  },
  {
    name: 'env SIGIL_AUTH_TOKEN fills caller-supplied bearer mode',
    config: { generationExport: { auth: { mode: 'bearer' } } },
    env: { SIGIL_AUTH_TOKEN: 'env-tok' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'bearer');
      assert.equal(cfg.generationExport.auth.bearerToken, 'env-tok');
    },
  },
  {
    // Caller mode wins; env's mode-incompatible credentials are silently
    // ignored by resolveHeadersWithAuth.
    name: 'caller bearer mode wins over env basic mode',
    config: {
      generationExport: { auth: { mode: 'bearer', bearerToken: 'callertok' } },
    },
    env: {
      SIGIL_AUTH_MODE: 'basic',
      SIGIL_AUTH_TENANT_ID: '42',
      SIGIL_AUTH_TOKEN: 'envpass',
    },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'bearer');
      assert.equal(cfg.generationExport.headers?.Authorization, 'Bearer callertok');
    },
  },
  {
    name: 'stray SIGIL_AUTH_TENANT_ID does not throw',
    config: {},
    env: { SIGIL_AUTH_TENANT_ID: '42' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.auth.mode, 'none');
    },
  },
  {
    // Caller tags merge with env tags as a base layer; caller wins on key
    // collision. Matches Go and Python SDK behavior.
    name: 'caller tags merge with env tags',
    config: { tags: { team: 'ai', env: 'staging' } },
    env: { SIGIL_TAGS: 'service=orch,env=prod' },
    check: (cfg) => {
      assert.deepEqual(cfg.tags, { service: 'orch', team: 'ai', env: 'staging' });
    },
  },
  {
    name: 'explicit caller wins over both spellings',
    config: { generationExport: { endpoint: 'https://explicit:4318' } },
    env: { AGENTO11Y_ENDPOINT: 'preferred.example:4318', SIGIL_ENDPOINT: 'legacy.example:4318' },
    check: (cfg) => {
      assert.equal(cfg.generationExport.endpoint, 'https://explicit:4318');
    },
  },
  {
    name: 'caller tags merge with preferred env tags',
    config: { tags: { team: 'ai', env: 'staging' } },
    env: { AGENTO11Y_TAGS: 'service=orch,env=prod' },
    check: (cfg) => {
      assert.deepEqual(cfg.tags, { service: 'orch', team: 'ai', env: 'staging' });
    },
  },
];

for (const tc of mergeConfigCases) {
  test(`mergeConfig: ${tc.name}`, () => {
    const cfg = mergeConfig(tc.config, tc.env);
    tc.check(cfg);
  });
}

test('mergeConfig: invalid preferred value warns with AGENTO11Y key', () => {
  const warnings = [];
  const cfg = mergeConfig(
    { logger: { warn: (message) => warnings.push(message) } },
    { AGENTO11Y_CONTENT_CAPTURE_MODE: 'bogus', SIGIL_CONTENT_CAPTURE_MODE: 'full' },
  );
  assert.equal(cfg.contentCapture, 'default');
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /AGENTO11Y_CONTENT_CAPTURE_MODE.*bogus/);
});

// Agento11yClient integration: env-driven defaults applied to generation seeds.
test('Agento11yClient applies default agent / user / tags from config', async () => {
  const original = process.env;
  try {
    process.env = {
      ...original,
      SIGIL_PROTOCOL: 'none',
      SIGIL_AGENT_NAME: 'planner',
      SIGIL_AGENT_VERSION: '1.0',
      SIGIL_USER_ID: 'alice',
      SIGIL_TAGS: 'service=orch',
    };
    const client = new Agento11yClient();
    try {
      const rec = client.startGeneration({
        model: { provider: 'openai', name: 'gpt-5' },
        conversationId: 'conv-1',
      });
      assert.equal(rec.seed.agentName, 'planner');
      assert.equal(rec.seed.agentVersion, '1.0');
      assert.equal(rec.seed.userId, 'alice');
      assert.deepEqual(rec.seed.tags, { service: 'orch' });
    } finally {
      await client.shutdown();
    }
  } finally {
    process.env = original;
  }
});

test('Agento11yClient: per-call args override env defaults', async () => {
  const original = process.env;
  try {
    process.env = {
      ...original,
      SIGIL_PROTOCOL: 'none',
      SIGIL_AGENT_NAME: 'planner',
      SIGIL_TAGS: 'env=prod',
    };
    const client = new Agento11yClient();
    try {
      const rec = client.startGeneration({
        model: { provider: 'openai', name: 'gpt-5' },
        agentName: 'reviewer',
        tags: { env: 'staging', task: 'summarize' },
      });
      assert.equal(rec.seed.agentName, 'reviewer');
      assert.deepEqual(rec.seed.tags, { env: 'staging', task: 'summarize' });
    } finally {
      await client.shutdown();
    }
  } finally {
    process.env = original;
  }
});

test('Agento11yClient applies default agent identity to tool execution recorders', async () => {
  const original = process.env;
  try {
    process.env = {
      ...original,
      SIGIL_PROTOCOL: 'none',
      SIGIL_AGENT_NAME: 'planner',
      SIGIL_AGENT_VERSION: '1.0',
    };
    const client = new Agento11yClient();
    try {
      const rec = client.startToolExecution({ toolName: 'lookup' });
      assert.equal(rec.seed.agentName, 'planner');
      assert.equal(rec.seed.agentVersion, '1.0');
    } finally {
      await client.shutdown();
    }
  } finally {
    process.env = original;
  }
});
