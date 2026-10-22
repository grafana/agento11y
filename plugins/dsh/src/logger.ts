import { appendFileSync, existsSync, mkdirSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";
import { format } from "node:util";

// dsh owns stdout, so plugin diagnostics go to the shared agento11y log.
// Logging is silent unless AGENTO11Y_DEBUG or its SIGIL_DEBUG fallback is true.
const APP_NAME = "agento11y";
// Use the legacy sigil state directory only as a fallback; never migrate it.
const LEGACY_APP_NAME = "sigil";

function stateRootFor(appName: string): string {
  const xdg = (process.env.XDG_STATE_HOME ?? "").trim();
  if (xdg && isAbsolute(xdg)) return join(xdg, appName);
  const home = homedir();
  if (home && isAbsolute(home)) return join(home, ".local", "state", appName);
  return join(tmpdir(), appName);
}

/** Mirrors Go AppStateRoot, including its legacy-directory fallback. */
export function stateRoot(): string {
  const preferred = stateRootFor(APP_NAME);
  if (existsSync(preferred)) return preferred;
  const legacy = stateRootFor(LEGACY_APP_NAME);
  if (existsSync(legacy)) return legacy;
  return preferred;
}

export function logFilePath(): string {
  return join(stateRoot(), "logs", `${APP_NAME}.log`);
}

export interface Agento11yDshLogger {
  debug(message: string, ...args: unknown[]): void;
  warn(message: string, ...args: unknown[]): void;
  error(message: string, ...args: unknown[]): void;
}

function debugEnabled(): boolean {
  // Importing config.ts for this precedence rule would create a module cycle.
  for (const key of ["AGENTO11Y_DEBUG", "SIGIL_DEBUG"]) {
    const v = (process.env[key] ?? "").trim().toLowerCase();
    if (v === "") continue;
    return ["1", "true", "yes", "on"].includes(v);
  }
  return false;
}

// Cache by resolved directory so a changed XDG_STATE_HOME creates the new path.
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
  // loadConfig can set the debug variable after this module loads.
  if (!debugEnabled()) return;
  const path = logFilePath();
  if (!ensureLogDir(path)) return;
  const line = `agento11y[dsh]: ${new Date().toISOString()} ${level} ${format(message, ...args)}\n`;
  try {
    appendFileSync(path, line, { mode: 0o600 });
  } catch {
    // Logging failures must not reach the TUI.
  }
}

export const logger: Agento11yDshLogger = {
  debug: (message, ...args) => emit("debug", message, args),
  warn: (message, ...args) => emit("warn", message, args),
  error: (message, ...args) => emit("error", message, args),
};

export function resetLoggerForTests(): void {
  ensuredDir = undefined;
}
