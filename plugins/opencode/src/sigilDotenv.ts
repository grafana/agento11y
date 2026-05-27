import { readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { isMissingFileError } from "./fsErrors.js";

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
 * `plugins/sigil/internal/xdg/xdg.go::ConfigRoot` so every Sigil agent reads
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
 * Read the sigil dotenv file and fill any empty entries in `process.env`.
 * Mirrors the Go launcher's `dotenv.ApplyEnv`: OS env wins per key, the
 * file value is only written when the OS value is empty or whitespace.
 */
export function applySigilDotenv(): void {
  const loaded = readSigilDotenv(sigilConfigEnvPath());
  if (!loaded.reliable) return;
  for (const [key, value] of Object.entries(loaded.env)) {
    const current = (process.env[key] ?? "").trim();
    if (current !== "") continue;
    process.env[key] = value;
  }
}
