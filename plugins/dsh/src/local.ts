import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { normalizeBaseEndpoint } from "./endpoint.js";

const execFileAsync = promisify(execFile);

export interface LocalReceiver {
  endpoint: string;
  otlpEndpoint: string;
}

/** Local capture failures disable capture instead of falling back to Cloud. */
export class LocalReceiverError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "LocalReceiverError";
  }
}

export interface LocalReceiverDeps {
  env?: NodeJS.ProcessEnv;
  runStatus?: (bin: string) => Promise<string>;
  probe?: (endpoint: string) => Promise<boolean>;
  exists?: (path: string) => boolean;
}

// `local status` includes a 500 ms health probe. Allow 5 seconds for process
// startup while still limiting delays from hung candidates.
const STATUS_TIMEOUT_MS = 5_000;

// Limit the extra loopback probe; failures fall back to `local status`.
const PROBE_TIMEOUT_MS = 2_000;

/**
 * Resolves an existing local receiver. It never starts one or falls back to a
 * remote endpoint. Throws `LocalReceiverError` when no live HTTP loopback
 * receiver is available.
 */
export async function resolveLocalReceiver(
  deps: LocalReceiverDeps = {},
): Promise<LocalReceiver> {
  const env = deps.env ?? process.env;
  const runStatus = deps.runStatus ?? defaultRunStatus;
  const probe = deps.probe ?? defaultProbe;
  const exists = deps.exists ?? existsSync;

  // Check the environment endpoint first; dsh may not inherit a PATH that
  // contains the binary.
  const injected = brandedEnv(env, "ENDPOINT")?.value;
  const injectedReceiver =
    injected && isLocalEndpoint(injected) ? receiverAt(injected) : undefined;
  // Probe injected loopback URLs because config can be stale or hand-written.
  if (injectedReceiver && (await probe(injectedReceiver.endpoint))) {
    return injectedReceiver;
  }

  const failed: FailedCandidate[] = [];
  for (const candidate of binaryCandidates(env, exists)) {
    let stdout: string;
    try {
      stdout = await runStatus(candidate.bin);
    } catch (err) {
      // One broken candidate must not mask a later valid installation. Keep
      // failures for the final diagnostic.
      failed.push({
        ...candidate,
        failure: errorText(err),
        code: errorCode(err),
      });
      continue;
    }
    // The binary health-probes an endpoint before reporting it as running.
    const endpoint = parseReceiverEndpoint(stdout, candidate.bin);
    if (!endpoint) {
      throw new LocalReceiverError(
        "no local receiver is running; start one with `agento11y local start`",
      );
    }
    if (!isLocalEndpoint(endpoint)) {
      throw new LocalReceiverError(
        `refusing local endpoint ${endpoint}: expected an HTTP URL with host 127.0.0.1, ::1, or localhost`,
      );
    }
    return receiverAt(endpoint);
  }
  // Report a failing override directly because the user selected that binary.
  const override = failed.find((candidate) => candidate.overrideKey);
  if (override) {
    throw new LocalReceiverError(
      `cannot run the agento11y binary at ${override.bin} ` +
        `(from ${override.overrideKey}): ${override.failure}`,
    );
  }
  throw new LocalReceiverError(
    `no usable agento11y binary found (tried ${describeFailures(failed)}). ` +
      "Install agento11y or set AGENTO11Y_BIN to a working binary path. " +
      "If a listed binary failed, fix the reported error.",
  );
}

function receiverAt(endpoint: string): LocalReceiver {
  // A pasted endpoint can include the export path; both receiver fields need
  // the normalized API base.
  const base = normalizeBaseEndpoint(endpoint.trim());
  return { endpoint: base, otlpEndpoint: `${base}/otlp` };
}

/**
 * Mirrors Go `IsLocalEndpoint`: accept only plain HTTP on a loopback host.
 * Parsing the hostname rejects attacker-controlled lookalike hosts.
 */
export function isLocalEndpoint(endpoint: string): boolean {
  const raw = endpoint.trim();
  if (!raw) return false;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return false;
  }
  if (url.protocol !== "http:") return false;
  const host = url.hostname.replace(/^\[|\]$/g, "");
  return host === "127.0.0.1" || host === "::1" || host === "localhost";
}

/**
 * Accept prose output for release skew: older binaries ignore `--json`, and
 * the package and binary release independently.
 */
function parseReceiverEndpoint(
  stdout: string,
  bin: string,
): string | undefined {
  const text = stdout.trim();
  if (!text) {
    throw new LocalReceiverError(
      `\`${bin} local status --json\` printed nothing`,
    );
  }
  if (text.startsWith("{")) {
    let payload: unknown;
    try {
      payload = JSON.parse(text);
    } catch (err) {
      throw new LocalReceiverError(
        `cannot parse \`${bin} local status --json\` output: ${errorText(err)}`,
        { cause: err },
      );
    }
    const record = payload as { running?: unknown; endpoint?: unknown };
    if (record?.running !== true) return undefined;
    if (typeof record.endpoint !== "string" || record.endpoint.trim() === "") {
      throw new LocalReceiverError(
        `\`${bin} local status --json\` reported a running receiver with no endpoint`,
      );
    }
    return record.endpoint.trim();
  }
  if (/not running/i.test(text)) return undefined;
  const url = text.match(/https?:\/\/[^\s)]+/)?.[0];
  if (!url) {
    throw new LocalReceiverError(
      `cannot read a receiver endpoint from \`${bin} local status --json\` output`,
    );
  }
  return url;
}

interface BinaryCandidate {
  bin: string;
  overrideKey?: string;
}

interface FailedCandidate extends BinaryCandidate {
  failure: string;
  code?: string;
}

// Match plugins/cursor/scripts/run.sh's known install directories, and try
// bare names for shell-launched hosts. GUI hosts often lack Homebrew and Go
// binary directories in PATH.
function binaryCandidates(
  env: NodeJS.ProcessEnv,
  exists: (path: string) => boolean,
): BinaryCandidate[] {
  const override = brandedEnv(env, "BIN");
  const paths: string[] = [];
  const home = (env.HOME ?? "").trim() || homedir();
  for (const name of ["agento11y", "sigil"]) {
    paths.push(name);
    for (const dir of [
      join(home, "go", "bin"),
      "/opt/homebrew/bin",
      "/usr/local/bin",
      join(home, ".local", "bin"),
    ]) {
      const path = join(dir, name);
      if (exists(path)) paths.push(path);
    }
  }
  const candidates = [...new Set(paths)].map((bin) => ({ bin }));
  if (!override) return candidates;
  return [
    { bin: override.value, overrideKey: override.key },
    ...candidates.filter((candidate) => candidate.bin !== override.value),
  ];
}

async function defaultRunStatus(bin: string): Promise<string> {
  // Keep configured binary paths out of a shell parser.
  const { stdout } = await execFileAsync(bin, ["local", "status", "--json"], {
    timeout: STATUS_TIMEOUT_MS,
    encoding: "utf-8",
  });
  return stdout;
}

async function defaultProbe(endpoint: string): Promise<boolean> {
  // Same check as the Go manager's endpointAlive: /healthz is the JSON
  // liveness probe, while / serves the viewer HTML.
  try {
    const res = await fetch(`${endpoint.replace(/\/+$/, "")}/healthz`, {
      signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
    });
    return res.ok;
  } catch {
    return false;
  }
}

// Omit ENOENT details. Report other failures so broken installs are not
// mislabeled as missing.
function describeFailures(failed: FailedCandidate[]): string {
  if (failed.length === 0) return "nothing";
  return failed
    .map(({ bin, failure, code }) =>
      code === "ENOENT" ? bin : `${bin} (${failure})`,
    )
    .join(", ");
}

function errorCode(err: unknown): string | undefined {
  const code = (err as { code?: unknown } | null)?.code;
  return typeof code === "string" ? code : undefined;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

interface BrandedValue {
  value: string;
  key: string;
}

function brandedEnv(
  env: NodeJS.ProcessEnv,
  suffix: string,
): BrandedValue | undefined {
  for (const key of [`AGENTO11Y_${suffix}`, `SIGIL_${suffix}`]) {
    const value = (env[key] ?? "").trim();
    if (value !== "") return { value, key };
  }
  return undefined;
}
