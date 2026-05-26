#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const localProtocols = ["workspace:", "file:", "link:", "portal:"];
const exactVersionPattern =
  /^(?:\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?|npm:(?:@[^/]+\/)?[^@]+@\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$/;

const publishedManifests = [
  "js/package.json",
  "plugins/pi/package.json",
  "plugins/opencode/package.json",
];
const privateManifests = [
  "package.json",
  "examples/getting-started/typescript/package.json",
  "examples/getting-started/typescript-strands/package.json",
];

const errors = [];

for (const manifestPath of publishedManifests) {
  const manifest = readManifest(manifestPath);
  checkExactSection(manifestPath, "devDependencies", manifest.devDependencies ?? {});
  checkScripts(manifestPath, manifest.scripts ?? {});
}

for (const manifestPath of privateManifests) {
  const manifest = readManifest(manifestPath);
  checkExactSection(manifestPath, "dependencies", manifest.dependencies ?? {});
  checkExactSection(manifestPath, "devDependencies", manifest.devDependencies ?? {});
  checkExactSection(
    manifestPath,
    "optionalDependencies",
    manifest.optionalDependencies ?? {},
  );
  checkScripts(manifestPath, manifest.scripts ?? {});
}

if (errors.length > 0) {
  console.error("JavaScript dependency pinning check failed:");
  for (const error of errors) {
    console.error(`- ${error}`);
  }
  process.exit(1);
}

function readManifest(manifestPath) {
  return JSON.parse(fs.readFileSync(path.join(root, manifestPath), "utf8"));
}

function checkExactSection(manifestPath, section, dependencies) {
  for (const [name, specifier] of Object.entries(dependencies)) {
    if (isLocalSpecifier(specifier) || exactVersionPattern.test(specifier)) {
      continue;
    }

    errors.push(
      `${manifestPath}: ${section}.${name} uses non-exact specifier "${specifier}"`,
    );
  }
}

function checkScripts(manifestPath, scripts) {
  for (const [name, command] of Object.entries(scripts)) {
    if (command.includes("@latest")) {
      errors.push(`${manifestPath}: scripts.${name} uses @latest`);
    }
  }
}

function isLocalSpecifier(specifier) {
  return localProtocols.some((protocol) => specifier.startsWith(protocol));
}
