import { resetAgento11yDotenvStateForTests } from "./agento11yDotenv.js";

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

// Include home-directory inputs because loadConfig resolves config.env from them.
const REALSDK_PRESERVED_KEYS = [
  "HOME",
  "USERPROFILE",
  "XDG_CONFIG_HOME",
] as const;

function isManagedRealSdkKey(key: string): boolean {
  return (
    key.startsWith("AGENTO11Y_") ||
    key.startsWith("SIGIL_") ||
    key.startsWith("OTEL_")
  );
}

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
