import { existsSync, readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { isMissingFileError } from "./fsErrors.js";
import { logger } from "./logger.js";

// Mirror plugins/agento11y/internal/dotenv/dotenv.go::AllowedDotenvKey so the
// allow-list stays in sync with the Go launcher. Anything outside the
// AGENTO11Y_*/SIGIL_* prefixes and this small OTEL_* set is ignored,
// including innocent-looking vars like PATH that happen to appear in a
// shared config.env.
const ALLOWED_OTEL_KEYS = new Set([
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_EXPORTER_OTLP_HEADERS",
  "OTEL_EXPORTER_OTLP_INSECURE",
  "OTEL_SERVICE_NAME",
]);

function allowedDotenvKey(key: string): boolean {
  return (
    key.startsWith("AGENTO11Y_") ||
    key.startsWith("SIGIL_") ||
    ALLOWED_OTEL_KEYS.has(key)
  );
}

// Alias families this plugin resolves source-aware: everything pi itself
// reads (config.ts, logger.ts) plus everything the in-process JS SDK
// dual-reads from env. Each suffix is one logical variable readable under
// the preferred AGENTO11Y_<suffix> spelling with a SIGIL_<suffix> legacy
// fallback. Keys outside this list keep exact-key semantics.
const ALIAS_SUFFIXES = [
  "ENDPOINT",
  "PROTOCOL",
  "INSECURE",
  "HEADERS",
  "AUTH_MODE",
  "AUTH_TENANT_ID",
  "AUTH_TOKEN",
  "AGENT_NAME",
  "AGENT_VERSION",
  "USER_ID",
  "TAGS",
  "CONTENT_CAPTURE_MODE",
  "DEBUG",
  "REDACT_INPUT_MESSAGES",
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_AUTH_TOKEN",
  "GUARDS_ENABLED",
  "GUARDS_TIMEOUT_MS",
  "GUARDS_FAIL_OPEN",
] as const;

function preferredKey(suffix: string): string {
  return `AGENTO11Y_${suffix}`;
}

function legacyKey(suffix: string): string {
  return `SIGIL_${suffix}`;
}

// Preferred config directory name and the pre-rename fallback. The legacy
// directory is still read during the transition so existing installs keep
// working; the file is never moved or copied.
const APP_NAME = "agento11y";
const LEGACY_APP_NAME = "sigil";

// Resolve the config root the same way as
// `plugins/agento11y/internal/xdg/xdg.go::ConfigRoot`:
//
// 1. `$XDG_CONFIG_HOME` when it is an absolute path.
// 2. `$HOME/.config` when the user has a resolvable home.
// 3. `<tmpdir>` as a last-resort fallback.
function configRoot(): string {
  const xdg = (process.env.XDG_CONFIG_HOME ?? "").trim();
  if (xdg && isAbsolute(xdg)) {
    return xdg;
  }
  const home = homedir();
  if (home && isAbsolute(home)) {
    return join(home, ".config");
  }
  return tmpdir();
}

/**
 * Resolve the path the sigil dotenv loader reads. Mirrors
 * `plugins/agento11y/internal/dotenv/dotenv.go::FilePath` so plain pi and
 * `agento11y pi` read the same file: `<config root>/agento11y/config.env`
 * if that file exists, otherwise the legacy `<config root>/sigil/config.env`
 * if that exists, otherwise the new path. Preferring the new path when both
 * exist mirrors the AGENTO11Y_* > SIGIL_* env precedence.
 */
export function agento11yConfigEnvPath(): string {
  const root = configRoot();
  const preferred = join(root, APP_NAME, "config.env");
  if (existsSync(preferred)) {
    return preferred;
  }
  const legacy = join(root, LEGACY_APP_NAME, "config.env");
  if (existsSync(legacy)) {
    return legacy;
  }
  return preferred;
}

/**
 * Parse a config.env body using the same rules as the Go reference loader
 * (`plugins/agento11y/internal/dotenv/dotenv.go::LoadDotenv` +
 * `parseDotenvValue`):
 *
 * - `KEY=value` one pair per line.
 * - `#` line comments and blank lines are skipped.
 * - Optional leading `export ` is stripped.
 * - Optional matching single- or double-quotes around the value; inner
 *   whitespace and `#` are preserved as written.
 * - An unterminated quoted value falls through to the literal value
 *   (including the leading quote), matching Go.
 * - Trailing ` # comment` is stripped from unquoted values only.
 * - Empty values, lines without `=`, and lines with an empty key are dropped.
 * - Only keys passing `allowedDotenvKey` (SIGIL_* plus four OTEL_*) survive.
 */
export function parseAgento11yDotenv(body: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const rawLine of body.split(/\r?\n/)) {
    let line = rawLine.trim();
    if (line === "" || line.startsWith("#")) continue;
    if (line.startsWith("export ")) {
      line = line.slice("export ".length).trim();
    }
    const eq = line.indexOf("=");
    if (eq <= 0) continue;
    const key = line.slice(0, eq).trim();
    if (!key || !allowedDotenvKey(key)) continue;
    const value = parseDotenvValue(line.slice(eq + 1).trim());
    if (value !== "") out[key] = value;
  }
  return out;
}

function parseDotenvValue(v: string): string {
  if (v.length >= 2) {
    const first = v[0];
    if (first === '"' || first === "'") {
      const end = v.indexOf(first, 1);
      if (end >= 0) return v.slice(1, end);
    }
  }
  const hashIdx = v.indexOf(" #");
  if (hashIdx >= 0) {
    return v.slice(0, hashIdx).replace(/[ \t]+$/, "");
  }
  return v;
}

interface Agento11yDotenvReadResult {
  env: Record<string, string>;
  reliable: boolean;
}

function readAgento11yDotenv(path: string): Agento11yDotenvReadResult {
  let body: string;
  try {
    body = readFileSync(path, "utf-8");
  } catch (err) {
    if (isMissingFileError(err)) {
      return { env: {}, reliable: true };
    }
    logger.warn(`failed to read ${path}`, err);
    return { env: {}, reliable: false };
  }
  return { env: parseAgento11yDotenv(body), reliable: true };
}

/**
 * Read and parse the dotenv file at `path`. Missing files return `{}`
 * silently — the dotenv config is optional and credentials may come from
 * other sources (shell env). Other read failures emit a single warning to
 * the sigil debug log and also return `{}`.
 */
export function loadAgento11yDotenv(path: string): Record<string, string> {
  return readAgento11yDotenv(path).env;
}

// Maps each non-family key this function has copied from config.env to the
// exact value it wrote into process.env on the last call. We track the value
// (not just the key) so a later writer — a shell-style assignment from
// another loader, an extension, or test setup — can be distinguished from
// our own previously-written value. Once process.env[key] no longer
// matches what we wrote, ownership is released and the key is treated as
// OS-supplied: OS env wins per key, on every call, not just the first.
const ownedValues = new Map<string, string>();

// Family-level ownership for the alias suffixes. `value` is what we last
// materialized under BOTH spellings; `shell` records the values that were in
// process.env before that write (the true shell/runtime layer), so repeated
// calls can re-run source-aware resolution without mistaking our own writes
// for shell exports.
interface OwnedFamily {
  value: string;
  shell: { preferred: string | undefined; legacy: string | undefined };
}

const ownedFamilies = new Map<string, OwnedFamily>();

/**
 * Read the sigil dotenv file and synchronize its contents with `process.env`.
 *
 * Alias families (see ALIAS_SUFFIXES) resolve source-first, spelling-second:
 *
 *   shell AGENTO11Y_* > shell SIGIL_* > file AGENTO11Y_* > file SIGIL_*
 *
 * so a shell export always beats a config.env entry even across spellings —
 * the same guarantee as the Go launcher's `dotenv.ApplyEnv`, extended to
 * hold across repeated `session_start` events in the same Pi process. Blank
 * or whitespace-only values count as unset at every step, and the winning
 * value is materialized under BOTH spellings so downstream readers (this
 * plugin and the in-process SDK, which itself dual-reads) observe one
 * consistent value.
 *
 * Ownership is family-level: values this function materialized are refreshed
 * (edits propagate) or cleared under both spellings (removals propagate) on
 * every call, but only while process.env still holds what we wrote; the
 * moment another writer replaces either spelling, the writer's value becomes
 * the shell layer for the whole family and the file no longer clobbers it.
 *
 * Keys outside the alias families (the standard OTEL_* passthroughs) keep
 * exact-key semantics: OS env wins per key.
 */
export function applyAgento11yDotenv(): void {
  const loaded = readAgento11yDotenv(agento11yConfigEnvPath());
  if (!loaded.reliable) return;
  const fileEnv = loaded.env;

  // Immutable snapshot of the pre-existing process env, taken before any
  // write below, so resolution never reads back our own materializations.
  const envSnapshot: Record<string, string | undefined> = { ...process.env };

  const familyKeys = new Set<string>();
  for (const suffix of ALIAS_SUFFIXES) {
    familyKeys.add(preferredKey(suffix));
    familyKeys.add(legacyKey(suffix));
    applyFamily(suffix, envSnapshot, fileEnv);
  }

  // Release ownership for any key whose value in process.env no longer
  // matches what we wrote. Some other writer (shell-style assignment from
  // an extension, manual override, etc.) has taken over and the file value
  // must not stomp on it on this or subsequent calls.
  for (const [key, ownedValue] of [...ownedValues]) {
    if (process.env[key] !== ownedValue) {
      ownedValues.delete(key);
    }
  }

  // Drop keys we still own that the file no longer provides, so a deletion
  // in config.env actually takes effect. Keys whose ownership we just
  // released above are left alone — the user-supplied value wins.
  for (const key of [...ownedValues.keys()]) {
    if (!(key in fileEnv)) {
      delete process.env[key];
      ownedValues.delete(key);
    }
  }

  for (const [key, value] of Object.entries(fileEnv)) {
    if (familyKeys.has(key)) continue;
    if (!ownedValues.has(key)) {
      // We don't own this key — defer to a non-empty OS env value (shell
      // export, runtime assignment by any other writer) and leave it
      // untouched. This is the check that enforces "OS env always wins"
      // even for keys observed for the first time after earlier calls.
      const current = process.env[key] ?? "";
      if (current.trim() !== "") continue;
    }
    process.env[key] = value;
    ownedValues.set(key, value);
  }
}

// applyFamily resolves one alias family against the env snapshot and the
// parsed file map, then materializes the winner under both spellings. Env
// values still equal to our last write are ours and are replaced by the
// recorded pre-write shell values for resolution; anything else is a
// shell/runtime-writer value and wins over the file.
function applyFamily(
  suffix: string,
  envSnapshot: Record<string, string | undefined>,
  fileEnv: Record<string, string>,
): void {
  const pKey = preferredKey(suffix);
  const lKey = legacyKey(suffix);
  const curPreferred = envSnapshot[pKey];
  const curLegacy = envSnapshot[lKey];
  const rec = ownedFamilies.get(suffix);
  const shellPreferred =
    rec && curPreferred === rec.value ? rec.shell.preferred : curPreferred;
  const shellLegacy =
    rec && curLegacy === rec.value ? rec.shell.legacy : curLegacy;

  let winner: string | undefined;
  for (const candidate of [
    shellPreferred,
    shellLegacy,
    fileEnv[pKey],
    fileEnv[lKey],
  ]) {
    const trimmed = (candidate ?? "").trim();
    if (trimmed !== "") {
      winner = trimmed;
      break;
    }
  }

  if (winner === undefined) {
    // No source supplies a value anymore. Withdraw only what we wrote —
    // a value some other writer changed is not ours to delete.
    if (rec) {
      if (curPreferred === rec.value) delete process.env[pKey];
      if (curLegacy === rec.value) delete process.env[lKey];
      ownedFamilies.delete(suffix);
    }
    return;
  }

  process.env[pKey] = winner;
  process.env[lKey] = winner;
  ownedFamilies.set(suffix, {
    value: winner,
    shell: { preferred: shellPreferred, legacy: shellLegacy },
  });
}

/**
 * Forget which keys `applyAgento11yDotenv` has previously written into
 * `process.env`. Intended for tests that mutate `process.env` between cases
 * — production callers don't need to invoke this.
 */
export function resetAgento11yDotenvStateForTests(): void {
  ownedValues.clear();
  ownedFamilies.clear();
}
