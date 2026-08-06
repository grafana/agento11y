import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const generator = path.join(scriptDir, 'generate.mjs');
const repoRoot = path.resolve(scriptDir, '..', '..');
const realPatterns = JSON.parse(fs.readFileSync(path.join(repoRoot, 'redaction', 'patterns.json'), 'utf8'));

function withTempDir(fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'redaction-generate-'));
  try {
    return fn(dir);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

/** Runs the generator CLI over a table and returns {status, stderr, written}. */
function runGenerator(patterns, { format = false, version = 1 } = {}) {
  return withTempDir((dir) => {
    const patternsFile = path.join(dir, 'patterns.json');
    const outRoot = path.join(dir, 'out');
    fs.mkdirSync(outRoot);
    fs.writeFileSync(patternsFile, JSON.stringify({ version, patterns }));

    const args = [generator, outRoot, '--patterns', patternsFile];
    if (!format) args.push('--no-format');

    let status = 0;
    let stderr = '';
    try {
      execFileSync(process.execPath, args, { encoding: 'utf8', stdio: 'pipe' });
    } catch (err) {
      status = err.status ?? 1;
      stderr = String(err.stderr ?? '');
    }

    return { status, stderr, written: listFiles(outRoot) };
  });
}

function listFiles(root) {
  const found = [];
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) walk(full);
      else found.push(path.relative(root, full));
    }
  };
  walk(root);
  return found;
}

const validTier1 = { id: 'probe-token', tier: 1, regex: '\\bprobe_[A-Za-z0-9]{10,}' };
const validEmail = {
  id: 'email',
  tier: 'email',
  regex: '\\b[A-Za-z0-9._%+\\-]+@[A-Za-z0-9.\\-]+\\.[A-Za-z]{2,}\\b',
};
const validTier2 = {
  id: 'env-secret-value',
  tier: 2,
  flags: ['i'],
  regex: '(^|[^A-Za-z0-9])(TOKEN[ \\t]*[=:][ \\t]*)([^ \\t"]+)',
  keepGroups: [1, 2],
};

test('accepts a minimal valid table and writes every target', () => {
  const result = runGenerator([validTier1, validEmail, validTier2]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.written.sort(), [
    'dotnet/src/Grafana.Agento11y/RedactionPatterns.g.cs',
    'go/agento11y/redaction_patterns_gen.go',
    'js/src/redaction-patterns.generated.ts',
    'plugins/agento11y/internal/redact/patterns_gen.go',
    'python/agento11y/_redaction_patterns.py',
  ]);
});

const rejections = [
  {
    name: 'lookbehind',
    patterns: [{ id: 'bad-lookbehind', tier: 2, regex: '(?<=TOKEN=)([^ ]+)', keepGroups: [] }],
    expect: /bad-lookbehind: lookbehind or named group/,
  },
  {
    name: 'lookahead',
    patterns: [{ id: 'bad-lookahead', tier: 1, regex: 'TOKEN(?=abc)' }],
    expect: /bad-lookahead: lookahead/,
  },
  {
    name: 'negative lookahead',
    patterns: [{ id: 'bad-neg-lookahead', tier: 1, regex: 'TOKEN(?!abc)' }],
    expect: /bad-neg-lookahead: lookahead/,
  },
  {
    name: 'backreference',
    patterns: [{ id: 'bad-backref', tier: 2, regex: '(a)\\1(b)', keepGroups: [1] }],
    expect: /bad-backref: backreference/,
  },
  {
    name: 'tier 1 capturing group',
    patterns: [{ id: 'bad-tier1-group', tier: 1, regex: '\\b(glc_[A-Za-z0-9]{10,})' }],
    expect: /bad-tier1-group: tier 1 patterns must not contain capturing groups/,
  },
  {
    name: 'email capturing group',
    patterns: [{ id: 'email', tier: 'email', regex: '(a)@(b)' }],
    expect: /email: tier email patterns must not contain capturing groups/,
  },
  {
    name: 'keepGroups out of range',
    patterns: [{ ...validTier2, id: 'bad-range', keepGroups: [1, 9] }],
    expect: /bad-range: keepGroups entry 9 is not a capturing group \(regex has 3\)/,
  },
  {
    name: 'keepGroups out of order',
    patterns: [{ ...validTier2, id: 'bad-order', keepGroups: [2, 1] }],
    expect: /bad-order: keepGroups must be strictly increasing/,
  },
  {
    name: 'keepGroups keeps everything',
    patterns: [{ ...validTier2, id: 'bad-keep-all', keepGroups: [1, 2, 3] }],
    expect: /bad-keep-all: keepGroups keeps every group/,
  },
  {
    name: 'keepGroups leaves a non-contiguous gap',
    patterns: [
      {
        id: 'bad-gap',
        tier: 2,
        regex: '(a)(b)(c)(d)',
        keepGroups: [1, 3],
      },
    ],
    expect: /bad-gap: groups left out of keepGroups must be contiguous, got 2, 4/,
  },
  {
    name: 'missing keepGroups on tier 2',
    patterns: [{ id: 'bad-missing', tier: 2, regex: '(TOKEN=)([^ ]+)' }],
    expect: /bad-missing: tier 2 patterns need a non-empty keepGroups array/,
  },
  {
    name: 'keepGroups on tier 1',
    patterns: [{ id: 'bad-tier1-keep', tier: 1, regex: 'abc', keepGroups: [1] }],
    expect: /bad-tier1-keep: keepGroups is only valid for tier 2 patterns/,
  },
  {
    name: 'flags on tier 1',
    patterns: [{ id: 'bad-tier1-flag', tier: 1, regex: 'abc', flags: ['i'] }],
    expect: /bad-tier1-flag: tier 1 patterns must not set flags, got \["i"\]/,
  },
  {
    name: 'a capturing group nested in another',
    patterns: [
      {
        id: 'bad-nested',
        tier: 2,
        regex: '(SECRET=)(([a-z]+)-([a-z]+))',
        keepGroups: [1, 2],
      },
    ],
    expect: /bad-nested: tier 2 patterns must not nest one capturing group inside another/,
  },
  {
    name: 'a Unicode-dependent shorthand class',
    patterns: [{ id: 'bad-shorthand', tier: 1, regex: '\\bTOKEN\\s+[a-z]{10,}' }],
    expect: /bad-shorthand: \\s means ASCII in Go and JavaScript and Unicode in Python and \.NET/,
  },
  {
    name: 'an unterminated character class',
    patterns: [{ id: 'bad-class', tier: 1, regex: '\\bTOKEN[a-z0-9' }],
    expect: /bad-class: regex has an unterminated character class/,
  },
  {
    name: 'a trailing backslash',
    patterns: [{ id: 'bad-backslash', tier: 1, regex: '\\bTOKEN\\' }],
    expect: /bad-backslash: regex ends with a trailing backslash/,
  },
  {
    name: 'an unknown table version',
    patterns: [validTier1, validEmail],
    version: 2,
    expect: /patterns\.json version must be 1, got 2/,
  },
  {
    name: 'backtick in regex',
    patterns: [{ id: 'bad-backtick', tier: 1, regex: 'a`b' }],
    expect: /bad-backtick: regex must not contain a backtick/,
  },
  {
    name: 'unsupported flag',
    patterns: [{ id: 'bad-flag', tier: 1, regex: 'abc', flags: ['m'] }],
    expect: /bad-flag: unsupported flag "m"/,
  },
  {
    name: 'duplicate id',
    patterns: [validTier1, validTier1],
    expect: /probe-token: duplicate pattern id/,
  },
  {
    name: 'non-kebab id',
    patterns: [{ id: 'Bad_Id', tier: 1, regex: 'abc' }],
    expect: /invalid pattern id "Bad_Id"/,
  },
  {
    name: 'missing email pattern',
    patterns: [validTier1],
    expect: /must declare exactly one email pattern, found 0/,
  },
  {
    name: 'missing tier 1 patterns',
    patterns: [validEmail],
    expect: /must declare at least one tier 1 pattern/,
  },
];

for (const rejection of rejections) {
  test(`rejects ${rejection.name} and writes nothing`, () => {
    const result = runGenerator(rejection.patterns, { version: rejection.version });
    assert.notEqual(result.status, 0, `expected a non-zero exit for ${rejection.name}`);
    assert.match(result.stderr, rejection.expect);
    assert.deepEqual(result.written, [], 'no output file may be written when validation fails');
  });
}

test('every rendered file is a declared target', async () => {
  const { render, targets, validateTable } = await import('./generate.mjs');
  // check:redaction diffs the paths the generator prints. A renderer that is not
  // in `targets` would be written and never checked.
  assert.deepEqual(Object.keys(render(validateTable(realPatterns))), targets);
});

test('the generated tables pin word boundaries to ASCII', async () => {
  const { render, validateTable } = await import('./generate.mjs');
  const files = render(validateTable(realPatterns));

  // Python and .NET read \b as Unicode by default, so their tables carry the
  // flag that makes them agree with Go and JavaScript.
  assert.match(files['python/agento11y/_redaction_patterns.py'], /BASE_FLAGS: int = re\.ASCII/);
  assert.match(
    files['dotnet/src/Grafana.Agento11y/RedactionPatterns.g.cs'],
    /BaseOptions =\s+RegexOptions\.Compiled \| RegexOptions\.ECMAScript \| RegexOptions\.CultureInvariant;/,
  );
});

test('the committed table passes validation', async () => {
  const { validateTable } = await import('./generate.mjs');
  const table = validateTable(realPatterns);
  assert.ok(table.tier1.length > 0);
  assert.equal(table.email.id, 'email');
  assert.ok(table.tier2.length > 0);
});

test('every tier 2 replacement keeps the key and drops the value', async () => {
  const { validateTable, render } = await import('./generate.mjs');
  const table = validateTable(realPatterns);
  const files = render(table);

  for (const pattern of table.tier2) {
    assert.match(
      files['js/src/redaction-patterns.generated.ts'],
      new RegExp(`\\$1[^"]*\\[REDACTED:${pattern.id}\\]`),
      `${pattern.id} must preserve group 1 in the JS replacement`,
    );
  }
});

test('generated output is byte-identical across two runs', () => {
  const first = withTempDir((dir) => {
    execFileSync(process.execPath, [generator, dir, '--no-format'], { stdio: 'pipe' });
    return readAll(dir);
  });
  const second = withTempDir((dir) => {
    execFileSync(process.execPath, [generator, dir, '--no-format'], { stdio: 'pipe' });
    return readAll(dir);
  });
  assert.deepEqual(first, second);
});

function readAll(root) {
  const out = {};
  for (const relative of listFiles(root)) {
    out[relative] = fs.readFileSync(path.join(root, relative), 'utf8');
  }
  return out;
}
