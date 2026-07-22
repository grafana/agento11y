#!/usr/bin/env node
// Stamps the package.json version into a compiled version.js. src/version.ts
// assigns SDK_VERSION a placeholder; each build rewrites the assignment in the
// tsc output so SDK_VERSION always matches the package that shipped it. Both
// @grafana/agento11y (js/) and @grafana/agento11y-core (js-core/) compile the
// same source, so each package stamps its own dist with its own version.
//
// Usage: node stamp-version.mjs <outDir>  (run from the package root)
import { readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';

const PLACEHOLDER_ASSIGNMENT = /(\bSDK_VERSION\s*=\s*)(['"])0\.0\.0\+unknown\2/;

const outDir = process.argv[2];
if (!outDir) {
  console.error('usage: stamp-version.mjs <outDir>');
  process.exit(1);
}

const pkg = JSON.parse(await readFile(resolve(process.cwd(), 'package.json'), 'utf-8'));
if (!pkg.version) {
  console.error('stamp-version: no "version" in package.json');
  process.exit(1);
}

const target = resolve(process.cwd(), outDir, 'version.js');
const source = await readFile(target, 'utf-8');
if (!PLACEHOLDER_ASSIGNMENT.test(source)) {
  console.error(`stamp-version: placeholder SDK_VERSION assignment not found in ${target}`);
  process.exit(1);
}
await writeFile(target, source.replace(PLACEHOLDER_ASSIGNMENT, `$1$2${pkg.version}$2`));
