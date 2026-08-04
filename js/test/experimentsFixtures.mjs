// Loaders for the cross-language experiment wire fixtures in
// `conformance/experiments/`. See that directory's README.md for the encodings.

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
// js/test -> repository root
const fixtureDir = join(__dirname, '..', '..', 'conformance', 'experiments');

/** The `source.id` this SDK substitutes for the fixture placeholder. */
export const sdkId = 'js';

/** The literal placeholder the fixtures carry inside every `source` object. */
// biome-ignore lint/suspicious/noTemplateCurlyInString: the fixtures store this text verbatim
const sdkIdPlaceholder = '${SDK_ID}';

export function loadInputs() {
  return readFixture('inputs.json');
}

export function loadIds() {
  return readFixture('ids.json');
}

export function loadRequests() {
  return withSdkId(readFixture('requests.json'));
}

export function loadResponses() {
  return readFixture('responses.json');
}

function readFixture(name) {
  return JSON.parse(readFileSync(join(fixtureDir, name), 'utf8'));
}

/** Replaces the SDK-id placeholder every `source` object carries. */
function withSdkId(value) {
  if (typeof value === 'string') {
    return value === sdkIdPlaceholder ? sdkId : value;
  }
  if (Array.isArray(value)) {
    return value.map(withSdkId);
  }
  if (value !== null && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, withSdkId(entry)]));
  }
  return value;
}

/**
 * Reports structural differences as dotted JSON paths plus both values, so a
 * failure names the offending field instead of dumping two payloads. Keys the
 * fixture uses for documentation (`comment`) are ignored.
 */
export function diffJson(got, want, path = '') {
  const label = path === '' ? '<root>' : path;
  if (Array.isArray(want)) {
    if (!Array.isArray(got)) {
      return [`${label}: got ${JSON.stringify(got)}, want an array`];
    }
    if (got.length !== want.length) {
      return [`${label}: got ${got.length} items, want ${want.length}`];
    }
    return want.flatMap((item, index) => diffJson(got[index], item, `${path}[${index}]`));
  }
  if (want !== null && typeof want === 'object') {
    if (got === null || typeof got !== 'object' || Array.isArray(got)) {
      return [`${label}: got ${JSON.stringify(got)}, want an object`];
    }
    const wantKeys = Object.keys(want).filter((key) => key !== 'comment');
    const gotKeys = Object.keys(got);
    const differences = [];
    for (const key of gotKeys) {
      if (!wantKeys.includes(key)) {
        differences.push(`${path}.${key}: unexpected field ${JSON.stringify(got[key])}`);
      }
    }
    for (const key of wantKeys) {
      if (!gotKeys.includes(key)) {
        differences.push(`${path}.${key}: missing, want ${JSON.stringify(want[key])}`);
        continue;
      }
      differences.push(...diffJson(got[key], want[key], `${path}.${key}`));
    }
    return differences;
  }
  if (got !== want) {
    return [`${label}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`];
  }
  return [];
}
