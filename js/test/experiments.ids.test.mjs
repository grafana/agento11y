import assert from 'node:assert/strict';
import test from 'node:test';

import { stableId } from '../.test-dist/experiments/ids.js';

// Every literal below comes from running the same inputs through Python's
// stable_id (and Go's StableID). A change here means a retry stops being
// idempotent and the SDKs stop agreeing on trial identity.
const vectors = [
  { name: 'a known SHA-1 vector', args: ['trial', 'abc'], want: 'trial-a9993e364706816a' },
  { name: 'an integer attempt', args: ['trial', 1], want: 'trial-356a192b7913b04c' },
  { name: 'the string form of that attempt', args: ['trial', '1'], want: 'trial-356a192b7913b04c' },
  { name: 'a float that is not the integer', args: ['trial', '1.0'], want: 'trial-e8dc057d3346e56a' },
  { name: 'an experiment trial id', args: ['trial', 'run-1', 'case-1', 1], want: 'trial-b4dcde4aa6965898' },
  { name: 'an anchor generation id', args: ['gen', 'run-1', 'case-1', 1], want: 'gen-b4dcde4aa6965898' },
  { name: 'a minted conversation id', args: ['conv', 'run-1', 'case-1', 1], want: 'conv-b4dcde4aa6965898' },
  {
    name: 'a first-occurrence score id',
    args: ['score', 'run-1', 'trial-b4dcde4aa6965898', 'final', 'exact'],
    want: 'score-3dd1bb9385d48c0a',
  },
  {
    name: 'a repeated score id',
    args: ['score', 'run-1', 'trial-b4dcde4aa6965898', 'final', 'exact', 2],
    want: 'score-8070a6fb2282feee',
  },
  { name: 'a grader generation id', args: ['gen', 'score-123', 'grader'], want: 'gen-cfd17a0e2cbaa495' },
  { name: 'a grader conversation id', args: ['conv', 'score-123', 'grader'], want: 'conv-cfd17a0e2cbaa495' },
];

test('stableId matches the cross-SDK vectors', () => {
  for (const { name, args, want } of vectors) {
    assert.equal(stableId(...args), want, name);
  }
});

test('a null part keeps its separator slot', () => {
  // The parts join with \x1f before hashing, so a dropped null would shift every
  // later part into the wrong slot.
  assert.equal(stableId('trial', 'a', null, 'b'), 'trial-e69b7ae7f775b317');
  assert.equal(stableId('trial', 'a', null, 'b'), stableId('trial', 'a', '', 'b'));
  assert.equal(stableId('trial', 'a', undefined, 'b'), stableId('trial', 'a', '', 'b'));
  assert.notEqual(stableId('trial', 'a', null, 'b'), stableId('trial', 'a', 'b'));
});

test('an integer attempt does not gain a decimal suffix', () => {
  assert.equal(stableId('trial', 1), stableId('trial', '1'));
  assert.notEqual(stableId('trial', 1), stableId('trial', '1.0'));
});

test('the prefix and digest length are fixed', () => {
  const id = stableId('trial', 'run-1', 'case-1', 1);
  const [prefix, digest] = id.split('-');
  assert.equal(prefix, 'trial');
  assert.equal(digest.length, 16);
  assert.match(digest, /^[0-9a-f]{16}$/);
});

test('no parts hashes the empty string', () => {
  assert.equal(stableId('exp'), 'exp-da39a3ee5e6b4b0d');
});
