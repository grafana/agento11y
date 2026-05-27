/**
 * Reset every SIGIL_* and OTEL_* env var and XDG_CONFIG_HOME between test
 * cases so on-disk fixtures (config.env) and shell exports cannot leak across
 * cases. Used by both config.test.ts and sigilDotenv.test.ts.
 */
export function clearSigilEnv(): void {
  for (const key of Object.keys(process.env)) {
    if (key.startsWith("SIGIL_") || key.startsWith("OTEL_")) {
      delete process.env[key];
    }
  }
  delete process.env.XDG_CONFIG_HOME;
}
