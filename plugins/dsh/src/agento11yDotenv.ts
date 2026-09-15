import { existsSync, readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { isMissingFileError } from "./fsErrors.js";
import { logger } from "./logger.js";

// Keep this allowlist in sync with
// plugins/agento11y/internal/dotenv/dotenv.go::AllowedDotenvKey.
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

// Alias families cover variables that dsh or the in-process SDK reads under
// both names. Other allowed keys retain exact-key semantics.
const ALIAS_SUFFIXES = [
  "ENDPOINT",
  "PROTOCOL",
  "INSECURE",
  "HEADERS",
  "EXPORT_TIMEOUT_MS",
  "AUTH_MODE",
  "AUTH_TENANT_ID",
  "AUTH_TOKEN",
  "AGENT_NAME",
  "AGENT_VERSION",
  "USER_ID",
  "TAGS",
  "AUTO_CODING_AGENT_TAGS",
  "AUTO_CODING_AGENT_TAGS_NAMES",
  "CONTENT_CAPTURE_MODE",
  "DEBUG",
  "REDACT_INPUT_MESSAGES",
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_AUTH_TOKEN",
  // `LOCAL` and `BIN` are not SDK inputs, but must use the launcher's
  // shell-before-file, preferred-before-legacy precedence.
  "LOCAL",
  "BIN",
] as const;

function preferredKey(suffix: string): string {
  return `AGENTO11Y_${suffix}`;
}

function legacyKey(suffix: string): string {
  return `SIGIL_${suffix}`;
}

// Use the legacy sigil directory only as a fallback; never migrate it.
const APP_NAME = "agento11y";
const LEGACY_APP_NAME = "sigil";

// Keep config-root resolution aligned with
// plugins/agento11y/internal/xdg/xdg.go::ConfigRoot.
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
 * Must match `plugins/agento11y/internal/dotenv/dotenv.go::FilePath`.
 * The agento11y file wins when both it and the legacy sigil file exist.
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
 * Must match `LoadDotenv` and `parseDotenvValue` in
 * `plugins/agento11y/internal/dotenv/dotenv.go`, including literal
 * unterminated quotes and trailing comments recognized only after ` #`.
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

/** Missing files are silent; other read failures are logged. Both return `{}`. */
export function loadAgento11yDotenv(path: string): Record<string, string> {
  return readAgento11yDotenv(path).env;
}

// Track each non-family value written here. A mismatch releases ownership so
// a later process.env writer is not overwritten.
const ownedValues = new Map<string, string>();

// Preserve each family's pre-write environment so repeated loads do not treat
// values written here as shell input.
interface OwnedFamily {
  value: string;
  shell: { preferred: string | undefined; legacy: string | undefined };
}

const ownedFamilies = new Map<string, OwnedFamily>();

/**
 * Alias families resolve shell AGENTO11Y_* > shell SIGIL_* > file
 * AGENTO11Y_* > file SIGIL_*. Blank values are unset, and the winner is
 * written under both names. Repeated calls update or remove only values still
 * owned by this loader; later writers take ownership. Non-family keys retain
 * exact-key operating-system environment precedence.
 */
export function applyAgento11yDotenv(): void {
  const loaded = readAgento11yDotenv(agento11yConfigEnvPath());
  if (!loaded.reliable) return;
  const fileEnv = loaded.env;

  const envSnapshot: Record<string, string | undefined> = { ...process.env };

  const familyKeys = new Set<string>();
  for (const suffix of ALIAS_SUFFIXES) {
    familyKeys.add(preferredKey(suffix));
    familyKeys.add(legacyKey(suffix));
    applyFamily(suffix, envSnapshot, fileEnv);
  }

  for (const [key, ownedValue] of [...ownedValues]) {
    if (process.env[key] !== ownedValue) {
      ownedValues.delete(key);
    }
  }

  for (const key of [...ownedValues.keys()]) {
    if (!(key in fileEnv)) {
      delete process.env[key];
      ownedValues.delete(key);
    }
  }

  for (const [key, value] of Object.entries(fileEnv)) {
    if (familyKeys.has(key)) continue;
    if (!ownedValues.has(key)) {
      const current = process.env[key] ?? "";
      if (current.trim() !== "") continue;
    }
    process.env[key] = value;
    ownedValues.set(key, value);
  }
}

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

export function resetAgento11yDotenvStateForTests(): void {
  ownedValues.clear();
  ownedFamilies.clear();
}
