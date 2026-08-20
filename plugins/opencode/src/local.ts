import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { normalizeBaseEndpoint } from "./endpoint.js";

const execFileAsync = promisify(execFile);

/**
 * A local receiver this plugin may export to. `endpoint` is the receiver base
 * URL; `otlpEndpoint` is the OTLP path under it, matching
 * `plugins/agento11y/internal/entry/entry.go::setupLocalLaunch`.
 */
export interface LocalReceiver {
  endpoint: string;
  otlpEndpoint: string;
}

/**
 * Local capture was requested but no receiver could be attached to. The
 * caller reports this and continues without capture; it must never fall back
 * to the configured Grafana Cloud endpoint.
 */
export class LocalReceiverError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "LocalReceiverError";
  }
}

/** Injection points for tests. Production uses the defaults. */
export interface LocalReceiverDeps {
  env?: NodeJS.ProcessEnv;
  /** Run `<bin> local status --json` and return its stdout. */
  runStatus?: (bin: string) => Promise<string>;
  /** Report whether `GET <endpoint>/healthz` answers. */
  probe?: (endpoint: string) => Promise<boolean>;
  exists?: (path: string) => boolean;
}

// `local status` reads the status file and health-probes the receiver with a
// 500ms client timeout (plugins/agento11y/internal/local/manager.go::endpointAlive),
// so this budget only has to cover spawning the binary. Ten times that leaves
// room on a loaded machine and still bounds the worst case, which is every
// candidate hanging while the host waits for us to return.
const STATUS_TIMEOUT_MS = 5_000;

// Our own liveness check, for an endpoint that arrives without one. Only has
// to cover a loopback round trip; a false negative here falls through to the
// binary, which does the same check itself.
const PROBE_TIMEOUT_MS = 2_000;

/**
 * Resolve the local receiver to export this session to.
 *
 * The plugin never starts a receiver: `agento11y opencode` already ensures
 * one and injects its endpoint, and a plain `opencode` attaches to whatever
 * `agento11y local status --json` reports. Throws {@link LocalReceiverError}
 * when there is nothing safe to attach to.
 */
export async function resolveLocalReceiver(
  deps: LocalReceiverDeps = {},
): Promise<LocalReceiver> {
  const env = deps.env ?? process.env;
  const runStatus = deps.runStatus ?? defaultRunStatus;
  const probe = deps.probe ?? defaultProbe;
  const exists = deps.exists ?? existsSync;

  // `agento11y opencode` resolved the receiver before exec and wrote its
  // endpoint into AGENTO11Y_ENDPOINT / SIGIL_ENDPOINT. Reuse it rather than
  // shelling out again: the launched host may not have the binary on its own
  // PATH.
  const injected = brandedEnv(env, "ENDPOINT")?.value;
  const injectedReceiver =
    injected && isLocalEndpoint(injected) ? receiverAt(injected) : undefined;
  // A loopback endpoint in the environment is not proof of a live receiver.
  // The same variable also carries a hand-written config.env value, and a
  // launcher-injected one outlives the receiver that was killed after it. An
  // unanswered probe falls through to the binary, which reports the receiver
  // that is running now.
  if (injectedReceiver && (await probe(injectedReceiver.endpoint))) {
    return injectedReceiver;
  }

  const failed: FailedCandidate[] = [];
  for (const candidate of binaryCandidates(env, exists)) {
    let stdout: string;
    try {
      stdout = await runStatus(candidate.bin);
    } catch (err) {
      // Every spawn failure moves to the next candidate: an unrelated `sigil`
      // earlier on PATH must not hide the real binary in ~/go/bin. Nothing is
      // lost by carrying on, because the failure is named in the final error.
      failed.push({
        ...candidate,
        failure: errorText(err),
        code: errorCode(err),
      });
      continue;
    }
    // The binary answered, so it is the authority on what is running: its
    // status already includes a health probe of the endpoint it reports.
    const endpoint = parseReceiverEndpoint(stdout, candidate.bin);
    if (!endpoint) {
      throw new LocalReceiverError(
        "no local receiver is running; start one with `agento11y local start`",
      );
    }
    if (!isLocalEndpoint(endpoint)) {
      throw new LocalReceiverError(
        `refusing local endpoint ${endpoint}: not an http loopback address`,
      );
    }
    return receiverAt(endpoint);
  }
  // A failing override outranks the generic message: the user named that
  // binary, so the answer is what went wrong with it, not the list of the
  // others that were tried after it.
  const override = failed.find((candidate) => candidate.overrideKey);
  if (override) {
    throw new LocalReceiverError(
      `cannot run the agento11y binary at ${override.bin} ` +
        `(from ${override.overrideKey}): ${override.failure}`,
    );
  }
  throw new LocalReceiverError(
    `no agento11y binary found (tried ${describeFailures(failed)}); ` +
      "install it or point AGENTO11Y_BIN at it",
  );
}

function receiverAt(endpoint: string): LocalReceiver {
  // A pasted endpoint can carry the export path; the receiver serves the API
  // under its root, so both fields are built from the normalized base.
  const base = normalizeBaseEndpoint(endpoint.trim());
  return { endpoint: base, otlpEndpoint: `${base}/otlp` };
}

/**
 * Mirror `plugins/agento11y/internal/envconfig/envconfig.go::IsLocalEndpoint`:
 * plain http on a loopback host. Parsing the URL (rather than matching a
 * prefix) is what keeps `http://localhost.attacker.com` out.
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
  // URL.hostname keeps the brackets around an IPv6 literal; Go's
  // url.Hostname() strips them, and this list is copied from there.
  const host = url.hostname.replace(/^\[|\]$/g, "");
  return host === "127.0.0.1" || host === "::1" || host === "localhost";
}

/**
 * Read the endpoint out of `local status` output. Returns undefined when the
 * receiver is reported as not running.
 *
 * The JSON form is preferred. The prose form is release skew: a binary older
 * than this package ignores the unknown `--json` argument and prints the
 * human line, and the two packages release independently of the binary.
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
      `cannot read a receiver endpoint from \`${bin} local status\` output`,
    );
  }
  return url;
}

interface BinaryCandidate {
  bin: string;
  /** Set when the candidate came from the BIN family, naming the spelling. */
  overrideKey?: string;
}

interface FailedCandidate extends BinaryCandidate {
  failure: string;
  code?: string;
}

// The override first, then per name (preferred before legacy) the bare name
// on PATH and the common install locations plugins/cursor/scripts/run.sh
// knows about. A GUI-launched host inherits launchd's or the desktop
// session's PATH, which holds neither Homebrew's bin nor the `go install`
// target, so a bare name alone is not enough. run.sh probes paths only; the
// bare names are tried too because a host started from a shell usually has
// the binary on PATH already.
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
      // Filtered so the failure message names paths that exist, and so a host
      // with no binary at all spawns two children rather than ten.
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
  // execFile with an argument array and no shell: a binary path from
  // config.env never reaches a shell parser.
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

// A candidate that is simply not installed needs no explanation in the final
// error; anything else (a non-zero exit, a file that is not executable, a
// timeout) is named there so a broken install is not reported as a missing
// one.
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
