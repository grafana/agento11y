import { readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { isMissingFileError } from "./fsErrors.js";

// Mirror plugins/agento11y/internal/dotenv/dotenv.go::AllowedDotenvKey so the
// allow-list stays in sync with the Go launcher. Anything outside the
// AGENTO11Y_* / SIGIL_* prefixes and this small OTEL_* set is ignored,
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

// Alias families the plugin (and the embedded JS SDK) resolve dual-spelled:
// preferred AGENTO11Y_<suffix> with a SIGIL_<suffix> legacy fallback. Dotenv
// resolution materializes exactly these; keys outside this list keep
// exact-key semantics. Mirrors the SDK's env pairs plus the plugin-only
// OTLP and guard variables.
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
  "GUARDS_FAIL_OPEN",
  "GUARDS_TIMEOUT_MS",
];

/**
 * Resolve the path the sigil dotenv loader reads. Mirrors
 * `plugins/agento11y/internal/xdg/xdg.go::ConfigRoot` so every Sigil agent reads
 * the same file:
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
 * - Only keys passing `allowedDotenvKey` (AGENTO11Y_*, SIGIL_*, plus four
 *   OTEL_*) survive.
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
    console.warn(`[sigil-opencode] failed to read ${path}:`, err);
    return { env: {}, reliable: false };
  }
  return { env: parseSigilDotenv(body), reliable: true };
}

/**
 * Read and parse the dotenv file at `path`. Missing files return `{}`
 * silently — the dotenv config is optional and credentials may come from
 * other sources (shell env). Other read failures emit a single
 * `[sigil-opencode]` warning and also return `{}`.
 */
export function loadSigilDotenv(path: string): Record<string, string> {
  return readSigilDotenv(path).env;
}

/**
 * Read the sigil dotenv file and merge it into `process.env`. Mirrors the Go
 * launcher's `dotenv.ApplyEnv`. Alias families resolve source-first,
 * spelling-second:
 *
 *   shell AGENTO11Y_* > shell SIGIL_* > file AGENTO11Y_* > file SIGIL_*
 *
 * so a shell export always beats a config.env entry even across spellings.
 * The winning value is materialized under BOTH names so downstream readers
 * (including the SDK's own dual-read) observe one consistent value. Blank or
 * whitespace-only values count as unset at every step. Keys outside the alias
 * registry keep exact-key semantics: the file value is only written when the
 * OS value is empty or whitespace.
 */
export function applySigilDotenv(): void {
  const loaded = readSigilDotenv(sigilConfigEnvPath());
  if (!loaded.reliable) return;
  const fileEnv = loaded.env;
  const shellEnv: Record<string, string | undefined> = { ...process.env };

  const aliasKeys = new Set<string>();
  for (const suffix of ALIAS_SUFFIXES) {
    const preferred = `AGENTO11Y_${suffix}`;
    const legacy = `SIGIL_${suffix}`;
    aliasKeys.add(preferred);
    aliasKeys.add(legacy);

    const winner = [
      shellEnv[preferred],
      shellEnv[legacy],
      fileEnv[preferred],
      fileEnv[legacy],
    ]
      .map((v) => (v ?? "").trim())
      .find((v) => v !== "");
    if (winner !== undefined) {
      process.env[preferred] = winner;
      process.env[legacy] = winner;
    }
  }

  for (const [key, value] of Object.entries(fileEnv)) {
    if (aliasKeys.has(key)) continue;
    if ((shellEnv[key] ?? "").trim() !== "") continue;
    process.env[key] = value;
  }
}
