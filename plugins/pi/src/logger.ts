import { appendFileSync, existsSync, mkdirSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";
import { format } from "node:util";

// The pi plugin runs in-process inside pi's TUI. Anything written to the
// terminal via console.* corrupts the live TUI frame (pi does not take over
// stdout in interactive mode — see core/output-guard.ts). So all diagnostics
// go to a file instead, matching the shared agento11y binary, which writes
// its debug log to $XDG_STATE_HOME/agento11y/logs/agento11y.log (see
// plugins/agento11y/internal/xdg and internal/cli InitLogger). Logging is silent
// unless AGENTO11Y_DEBUG (SIGIL_DEBUG fallback) is truthy.
const APP_NAME = "agento11y";
// Pre-rename state directory, still used when it is the only one present so
// existing installs keep their state in one place. Never moved or copied.
const LEGACY_APP_NAME = "sigil";

function stateRootFor(appName: string): string {
  const xdg = (process.env.XDG_STATE_HOME ?? "").trim();
  if (xdg && isAbsolute(xdg)) return join(xdg, appName);
  const home = homedir();
  if (home && isAbsolute(home)) return join(home, ".local", "state", appName);
  return join(tmpdir(), appName);
}

/**
 * Mirrors plugins/agento11y/internal/xdg.AppStateRoot: the agento11y state
 * dir if it exists, otherwise the legacy sigil dir if it exists, otherwise
 * the agento11y dir.
 */
export function stateRoot(): string {
  const preferred = stateRootFor(APP_NAME);
  if (existsSync(preferred)) return preferred;
  const legacy = stateRootFor(LEGACY_APP_NAME);
  if (existsSync(legacy)) return legacy;
  return preferred;
}

/** Mirrors plugins/agento11y/internal/xdg.LogFilePath. */
export function logFilePath(): string {
  return join(stateRoot(), "logs", `${APP_NAME}.log`);
}

export interface Agento11yPiLogger {
  debug(message: string, ...args: unknown[]): void;
  warn(message: string, ...args: unknown[]): void;
  error(message: string, ...args: unknown[]): void;
}

function debugEnabled(): boolean {
  // First nonblank of AGENTO11Y_DEBUG, SIGIL_DEBUG decides; the other
  // spelling is not consulted. (config.ts has the same selection helper,
  // but importing it here would create a module cycle.)
  for (const key of ["AGENTO11Y_DEBUG", "SIGIL_DEBUG"]) {
    const v = (process.env[key] ?? "").trim().toLowerCase();
    if (v === "") continue;
    return ["1", "true", "yes", "on"].includes(v);
  }
  return false;
}

// Cache the last directory we successfully created so we don't mkdir on every
// line. Keyed on the resolved dir so a changed XDG_STATE_HOME (e.g. in tests)
// re-creates it.
let ensuredDir: string | undefined;

function ensureLogDir(path: string): boolean {
  const dir = dirname(path);
  if (ensuredDir === dir) return true;
  try {
    mkdirSync(dir, { recursive: true, mode: 0o755 });
    ensuredDir = dir;
    return true;
  } catch {
    return false;
  }
}

function emit(level: string, message: string, args: unknown[]): void {
  // Re-read the env per write: loadConfig() applies the launcher dotenv file,
  // which may set the debug variable after the module first loads.
  if (!debugEnabled()) return;
  const path = logFilePath();
  if (!ensureLogDir(path)) return;
  const line = `agento11y[pi]: ${new Date().toISOString()} ${level} ${format(message, ...args)}\n`;
  try {
    appendFileSync(path, line, { mode: 0o600 });
  } catch {
    // Best effort. A logging failure must never surface into the TUI.
  }
}

export const logger: Agento11yPiLogger = {
  debug: (message, ...args) => emit("debug", message, args),
  warn: (message, ...args) => emit("warn", message, args),
  error: (message, ...args) => emit("error", message, args),
};

/** @internal Reset cached state so tests can re-evaluate the log directory. */
export function resetLoggerForTests(): void {
  ensuredDir = undefined;
}
