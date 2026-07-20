import { resetAgento11yDotenvStateForTests } from "./agento11yDotenv.js";

/**
 * Reset every AGENTO11Y_*, SIGIL_*, and OTEL_* env var and XDG_CONFIG_HOME
 * between test cases so on-disk fixtures (config.env) and shell exports
 * cannot leak across cases. Used by both config.test.ts and
 * agento11yDotenv.test.ts.
 *
 * Also clears the dotenv loader's per-process "owned keys" tracking so a
 * test that calls applyAgento11yDotenv doesn't poison the next case's view of
 * which env values came from the shell vs. config.env.
 */
export function clearAgento11yEnv(): void {
  for (const key of Object.keys(process.env)) {
    if (
      key.startsWith("AGENTO11Y_") ||
      key.startsWith("SIGIL_") ||
      key.startsWith("OTEL_")
    ) {
      delete process.env[key];
    }
  }
  delete process.env.XDG_CONFIG_HOME;
  resetAgento11yDotenvStateForTests();
}

// Keys that the real-SDK suites (golden, guards e2e) snapshot and clear so
// neither on-disk config.env nor shell exports leak across cases. HOME and the
// XDG/USERPROFILE pointers are included because loadConfig resolves the dotenv
// path from them.
const REALSDK_PRESERVED_KEYS = [
  "HOME",
  "USERPROFILE",
  "XDG_CONFIG_HOME",
] as const;

function isManagedRealSdkKey(key: string): boolean {
  return (
    key.startsWith("AGENTO11Y_") ||
    key.startsWith("SIGIL_") ||
    key.startsWith("SIGIL_PI_") ||
    key.startsWith("OTEL_")
  );
}

/**
 * Snapshot then delete the env vars a real-SDK test must control: HOME and the
 * XDG/USERPROFILE pointers plus every key matched by {@link isManagedRealSdkKey}
 * (the AGENTO11Y, SIGIL, SIGIL_PI, and OTEL prefixes). Returns the prior
 * values so {@link restoreEnv} can put them back in afterEach. Also resets the
 * dotenv loader's owned-keys tracking.
 */
export function snapshotAndClearTestEnv(): Record<string, string | undefined> {
  const keys = new Set<string>(REALSDK_PRESERVED_KEYS);
  for (const key of Object.keys(process.env)) {
    if (isManagedRealSdkKey(key)) {
      keys.add(key);
    }
  }

  const saved: Record<string, string | undefined> = {};
  for (const key of keys) {
    saved[key] = process.env[key];
    delete process.env[key];
  }
  resetAgento11yDotenvStateForTests();
  return saved;
}

/** Restore the env captured by {@link snapshotAndClearTestEnv}. */
export function restoreEnv(saved: Record<string, string | undefined>): void {
  for (const key of Object.keys(process.env)) {
    if (
      (REALSDK_PRESERVED_KEYS as readonly string[]).includes(key) ||
      isManagedRealSdkKey(key)
    ) {
      delete process.env[key];
    }
  }
  for (const [key, value] of Object.entries(saved)) {
    if (value === undefined) {
      delete process.env[key];
    } else {
      process.env[key] = value;
    }
  }
  resetAgento11yDotenvStateForTests();
}
