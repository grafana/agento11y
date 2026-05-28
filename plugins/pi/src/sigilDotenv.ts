import { readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { isMissingFileError } from "./fsErrors.js";
import { logger } from "./logger.js";

// Mirror plugins/sigil/internal/dotenv/dotenv.go::AllowedDotenvKey so the
// allow-list stays in sync with the Go launcher. Anything outside the SIGIL_*
// prefix and this small OTEL_* set is ignored, including innocent-looking
// vars like PATH that happen to appear in a shared config.env.
const ALLOWED_OTEL_KEYS = new Set([
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_EXPORTER_OTLP_HEADERS",
  "OTEL_EXPORTER_OTLP_INSECURE",
  "OTEL_SERVICE_NAME",
]);

function allowedDotenvKey(key: string): boolean {
  return key.startsWith("SIGIL_") || ALLOWED_OTEL_KEYS.has(key);
}

/**
 * Resolve the path the sigil dotenv loader reads. Mirrors
 * `plugins/sigil/internal/xdg/xdg.go::ConfigRoot` so plain pi and `sigil pi`
 * read the same file:
 *
 * 1. `$XDG_CONFIG_HOME/sigil/config.env` when XDG_CONFIG_HOME is an absolute path.
 * 2. `$HOME/.config/sigil/config.env` when the user has a resolvable home.
 * 3. `<tmpdir>/sigil/config.env` as a last-resort fallback.
 */
export function sigilConfigEnvPath(): string {
  const xdg = (process.env.XDG_CONFIG_HOME ?? "").trim();
  if (xdg && isAbsolute(xdg)) {
    return join(xdg, "sigil", "config.env");
  }
  const home = homedir();
  if (home && isAbsolute(home)) {
    return join(home, ".config", "sigil", "config.env");
  }
  return join(tmpdir(), "sigil", "config.env");
}

/**
 * Parse a config.env body using the same rules as the Go reference loader
 * (`plugins/sigil/internal/dotenv/dotenv.go::LoadDotenv` +
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
export function parseSigilDotenv(body: string): Record<string, string> {
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

interface SigilDotenvReadResult {
  env: Record<string, string>;
  reliable: boolean;
}

function readSigilDotenv(path: string): SigilDotenvReadResult {
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
  return { env: parseSigilDotenv(body), reliable: true };
}

/**
 * Read and parse the dotenv file at `path`. Missing files return `{}`
 * silently — the dotenv config is optional and credentials may come from
 * other sources (shell env). Other read failures emit a single warning to
 * the sigil debug log and also return `{}`.
 */
export function loadSigilDotenv(path: string): Record<string, string> {
  return readSigilDotenv(path).env;
}

// Maps each key this function has copied from config.env to the exact
// value it wrote into process.env on the last call. We track the value
// (not just the key) so a later writer — a shell-style assignment from
// another loader, an extension, or test setup — can be distinguished from
// our own previously-written value. Once process.env[key] no longer
// matches what we wrote, ownership is released and the key is treated as
// OS-supplied: OS env wins per key, on every call, not just the first.
const ownedValues = new Map<string, string>();

/**
 * Read the sigil dotenv file and synchronize its contents with `process.env`.
 *
 * Per-call, OS env wins per key: if `process.env[key]` is non-empty and
 * was not written by an earlier call of this function, it is left alone —
 * that's the same "shell export beats file" guarantee as the Go launcher's
 * `dotenv.ApplyEnv`, extended to hold across repeated `session_start`
 * events in the same Pi process. Keys this function copied from the file
 * are refreshed (edits propagate) or cleared (removals propagate) on every
 * call, but only while the value in `process.env` is still the one we
 * wrote; the moment another writer replaces it, we relinquish ownership.
 */
export function applySigilDotenv(): void {
  const loaded = readSigilDotenv(sigilConfigEnvPath());
  if (!loaded.reliable) return;
  const fileEnv = loaded.env;

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

/**
 * Forget which keys `applySigilDotenv` has previously written into
 * `process.env`. Intended for tests that mutate `process.env` between cases
 * — production callers don't need to invoke this.
 */
export function resetSigilDotenvStateForTests(): void {
  ownedValues.clear();
}
