import assert from "node:assert/strict";
import {
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  assertSdkBuilt,
  copyTestPackage,
  runTestProcess,
  testEnvironment,
} from "../src/testHarness.mjs";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

const node = realpathSync(process.execPath);
// Resolve pnpm before child HOME changes so its wrapper does not use mise.
assert(
  process.env.npm_execpath,
  "Run pnpm --filter @grafana/agento11y-pi run test:native",
);
const pnpm = realpathSync(process.env.npm_execpath);
assertSdkBuilt(packageDir);
const root = mkdtempSync(join(tmpdir(), "agento11y-pi-native-"));
const quote = (value) => `'${value.replaceAll("'", "'\\''")}'`;
const bin = join(root, "bin");
mkdirSync(bin);
writeFileSync(
  join(bin, "pnpm"),
  `#!/bin/sh\nexec ${quote(node)} ${quote(pnpm)} "$@"\n`,
  { mode: 0o755 },
);
const path = [bin, dirname(node), "/usr/bin", "/bin"].join(delimiter);

function run(name, args, cwd, env, timeout) {
  return runTestProcess({
    name,
    command: node,
    args,
    cwd,
    env,
    timeout,
    stdio: "inherit",
  });
}

try {
  const isolatedPackage = copyTestPackage(packageDir, root);
  await run(
    "plugin build",
    ["scripts/build.mjs"],
    isolatedPackage,
    testEnvironment(
      root,
      "build",
      [realpathSync(join(isolatedPackage, "node_modules/.bin")), path].join(
        delimiter,
      ),
    ),
    120_000,
  );
  const scenarios = [
    "file-loader",
    "reload",
    "lifecycle",
    "lifecycle-request-compaction",
    "lifecycle-summary-failed",
    "lifecycle-summary-cancelled",
    "lifecycle-fork-reload",
    "telemetry-response-model",
    "export-partial-rejection",
    "export-exhausted-retries",
    "export-queue-overflow",
    "export-shutdown-deadline",
    "guard-startup-disabled",
    "guard-startup-open",
    "guard-startup-strict",
    "ordinary-full",
    "ordinary-metadata",
    "ordinary-no-tools",
    "ordinary-metadata-spans",
    ...[
      "sequential",
      "parallel",
      "repeated-ids",
      "nested",
      "transformed",
      "later-changes",
      "finalized-result",
      "result-delay",
      "blocked-invalid",
      "mutated-input",
    ].flatMap((kind) =>
      ["full", "metadata", "no-tools", "metadata-spans"].map(
        (mode) => `tools-${kind}-${mode}`,
      ),
    ),
    "summary-manual",
    "summary-split",
    "summary-virtual",
    "summary-tree",
    "summary-navigation",
    "summary-manual-abort",
    ...["compact", "tree"].flatMap((kind) =>
      ["strict", "open"].map((policy) => `summary-abort-${kind}-${policy}`),
    ),
    "summary-threshold",
    "summary-threshold-cancel",
    "summary-overflow",
    "summary-overflow-cancel",
    ...["compact", "tree"].flatMap((kind) =>
      [
        "deny",
        "invalid",
        "failure",
        ...(kind === "compact" ? ["instructions"] : []),
      ].flatMap((verdict) =>
        ["strict", "open"].map(
          (policy) => `summary-policy-${kind}-${verdict}-${policy}`,
        ),
      ),
    ),
  ];
  const requested = process.argv.slice(2);
  assert(
    requested.every(
      (name) =>
        ["--summary", "--tools", "--lifecycle", "--proposal-controls"].includes(
          name,
        ) || scenarios.includes(name),
    ),
    "Pass native scenario names, --summary, --tools, --lifecycle, or --proposal-controls",
  );
  const failed = [];
  for (const scenario of scenarios.filter(
    (name) =>
      requested.length === 0 ||
      requested.includes(name) ||
      (requested.includes("--summary") && name.startsWith("summary-")) ||
      (requested.includes("--tools") && name.startsWith("tools-")) ||
      (requested.includes("--lifecycle") &&
        (name === "lifecycle" || name.startsWith("lifecycle-"))),
  )) {
    const env = testEnvironment(root, scenario, path);
    const cwd = join(root, scenario, "workspace");
    mkdirSync(cwd);
    try {
      await run(
        scenario,
        [join(isolatedPackage, "src/testdata/native/runner.mjs"), scenario],
        cwd,
        env,
        60_000,
      );
    } catch (error) {
      failed.push(scenario);
      console.error(error.message);
    }
  }
  assert.deepEqual(failed, [], "Native Pi scenarios failed");
  if (requested.length === 0 || requested.includes("--proposal-controls"))
    await run(
      "native acceptance negative controls",
      [
        join(isolatedPackage, "src/testdata/native/negative-controls.mjs"),
        ...(requested.length === 0 ? [] : ["--proposals"]),
      ],
      isolatedPackage,
      testEnvironment(root, "negative-controls", path),
      180_000,
    );
} finally {
  rmSync(root, { recursive: true, force: true });
}
