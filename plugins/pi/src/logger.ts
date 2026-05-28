import { appendFileSync, mkdirSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";
import { format } from "node:util";

// The pi plugin runs in-process inside pi's TUI. Anything written to the
// terminal via console.* corrupts the live TUI frame (pi does not take over
// stdout in interactive mode — see core/output-guard.ts). So all diagnostics
// go to a file instead, matching the shared sigil binary, which writes its
// SIGIL_DEBUG log to $XDG_STATE_HOME/sigil/logs/sigil.log (see
// plugins/sigil/internal/xdg and internal/cli InitLogger). Logging is silent
// unless SIGIL_DEBUG is truthy.
const APP_NAME = "sigil";

/** Mirrors plugins/sigil/internal/xdg.StateRoot for app "sigil". */
export function stateRoot(appName = APP_NAME): string {
  const xdg = (process.env.XDG_STATE_HOME ?? "").trim();
  if (xdg && isAbsolute(xdg)) return join(xdg, appName);
  const home = homedir();
  if (home && isAbsolute(home)) return join(home, ".local", "state", appName);
  return join(tmpdir(), appName);
}

/** Mirrors plugins/sigil/internal/xdg.LogFilePath for app "sigil". */
export function logFilePath(appName = APP_NAME): string {
  return join(stateRoot(appName), "logs", `${appName}.log`);
}

export interface SigilPiLogger {
  debug(message: string, ...args: unknown[]): void;
  warn(message: string, ...args: unknown[]): void;
  error(message: string, ...args: unknown[]): void;
}

function debugEnabled(): boolean {
  const v = (process.env.SIGIL_DEBUG ?? "").trim().toLowerCase();
  return ["1", "true", "yes", "on"].includes(v);
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
  // Re-read the env per write: loadConfig() applies the sigil dotenv file,
  // which may set SIGIL_DEBUG after the module first loads.
  if (!debugEnabled()) return;
  const path = logFilePath();
  if (!ensureLogDir(path)) return;
  const line = `sigil[pi]: ${new Date().toISOString()} ${level} ${format(message, ...args)}\n`;
  try {
    appendFileSync(path, line, { mode: 0o600 });
  } catch {
    // Best effort. A logging failure must never surface into the TUI.
  }
}

export const logger: SigilPiLogger = {
  debug: (message, ...args) => emit("debug", message, args),
  warn: (message, ...args) => emit("warn", message, args),
  error: (message, ...args) => emit("error", message, args),
};

/** @internal Reset cached state so tests can re-evaluate the log directory. */
export function resetLoggerForTests(): void {
  ensuredDir = undefined;
}
